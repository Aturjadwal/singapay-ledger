package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// listingGateway answers the settlement listing with a scripted page and records what it
// was asked for.
type listingGateway struct {
	*fakeGateway
	rows       []singapay.SettlementTransaction
	pagination singapay.Pagination
	err        error

	identifiers []string
	options     []singapay.ListSettlementTransactionsOptions
}

func (g *listingGateway) ListSettlementTransactions(_ context.Context, identifier string, opts singapay.ListSettlementTransactionsOptions) ([]singapay.SettlementTransaction, singapay.Pagination, error) {
	g.identifiers = append(g.identifiers, identifier)
	g.options = append(g.options, opts)
	if g.err != nil {
		return nil, singapay.Pagination{}, g.err
	}
	return g.rows, g.pagination, nil
}

// listedRow builds a row the way Singapay lists one; an empty accountID means account: null.
func listedRow(transactionID, merchantReffNo, accountID string, amountRupiah int64, feeMinor *int64) singapay.SettlementTransaction {
	row := singapay.SettlementTransaction{
		TransactionID:  transactionID,
		MerchantReffNo: merchantReffNo,
		Amount:         singapay.NewAmount(amountRupiah, "IDR"),
		HasSettle:      true,
		PostedAt:       singapay.MillisTime{Time: time.UnixMilli(1759395328000), Set: true},
		SettleAt:       singapay.MillisTime{Time: time.UnixMilli(1759795200000), Set: true},
	}
	if accountID != "" {
		row.Account = &singapay.SettlementTransactionAccount{ID: accountID, Name: "Sub-account " + accountID}
	}
	row.Fees.Name = "QRIS"
	row.Fees.Currency = "IDR"
	if feeMinor != nil {
		row.Fees.Amount = singapay.NewAmountFromMinor(*feeMinor, "IDR")
	}
	return row
}

// listingStores are the two stores the listing's scenario runs against: the in-memory fakes,
// which prove the method's own logic, and PostgreSQL when LEDGER_TEST_DATABASE_URL names a
// database (README_TESTING.md), which proves the batch lookups' SQL finds the same rows.
var listingStores = []struct {
	name   string
	client func(t *testing.T, gateway PaymentGateway) *LedgerClient
}{
	{"fakes", fakeListingClient},
	{"postgres", func(t *testing.T, gateway PaymentGateway) *LedgerClient {
		return NewLedgerClient(ledgerTestDB(t), gateway, testLogger())
	}},
}

func fakeListingClient(t *testing.T, gateway PaymentGateway) *LedgerClient {
	t.Helper()
	fakes := NewFakeRepositoryProvider()
	return &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		gateway:      gateway,
		logger:       testLogger(),
	}
}

type listingFixture struct {
	client       *LedgerClient
	notification *domain.SettlementNotification
	sellerA      *domain.Account
	sellerB      *domain.Account
	qrisSettled  *domain.ProductTransaction
	qrisOpen     *domain.ProductTransaction
	card         *domain.ProductTransaction
}

// newListingFixture stores one settlement notification and three paid sales: two QRIS
// sales of seller A — one the ledger has settled, one it still holds as COMPLETED — and a
// card sale of seller B whose payment-link attempt the ledger stored the identifiers of.
func newListingFixture(t *testing.T, client *LedgerClient) *listingFixture {
	t.Helper()
	ctx := context.Background()
	fakes := client.repoProvider

	sellerA := createTestAccount(domain.OwnerTypeSeller, "user-a", "01SELLERAULID")
	require.NoError(t, fakes.Account().Save(ctx, sellerA))
	sellerB := createTestAccount(domain.OwnerTypeSeller, "user-b", "01SELLERBULID")
	require.NoError(t, fakes.Account().Save(ctx, sellerB))

	fee, err := domain.NewFeeBreakdown(36000, 3000, 275, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
	require.NoError(t, err)

	qrisSettled := createTestProductTransaction("INV-20261002183927-YF2GYY", sellerA.UUID, fee)
	qrisSettled.Status = domain.TransactionStatusSettled
	require.NoError(t, fakes.ProductTransaction().Save(ctx, qrisSettled))
	qrisOpen := createTestProductTransaction("INV-20261002182028-R7LK6V", sellerA.UUID, fee)
	require.NoError(t, fakes.ProductTransaction().Save(ctx, qrisOpen))
	card := createTestProductTransaction("INV-20260925161538-SWIMLQ", sellerB.UUID, fee)
	card.Status = domain.TransactionStatusSettled
	require.NoError(t, fakes.ProductTransaction().Save(ctx, card))

	for requestID, tx := range map[string]*domain.ProductTransaction{"578437": qrisSettled, "578344": qrisOpen} {
		pr := domain.NewPaymentRequest(tx.UUID, requestID, "QRIS", fee.TotalCharged, domain.CurrencyIDR, time.Now().Add(time.Hour))
		pr.SetGatewayTransaction(requestID, "")
		require.NoError(t, fakes.PaymentRequest().Save(ctx, pr))
	}
	cardRequest := domain.NewPaymentRequest(card.UUID, "98465", ChannelCreditCard, fee.TotalCharged, domain.CurrencyIDR, time.Now().Add(time.Hour))
	require.NoError(t, fakes.PaymentRequest().Save(ctx, cardRequest))
	// Stored the way settlement stores an attempt it found: after the request exists.
	cardRequest.SetGatewayTransaction("80937", "984656477720260925231717415YVichz6Y")
	require.NoError(t, fakes.PaymentRequest().Update(ctx, cardRequest))

	notification, err := domain.NewSettlementNotification("3937", "SETTLEMENT-647-6AC4EDE197FB9",
		string(singapay.EventSettlementCompleted), []byte(`{"event":"settlement.completed"}`))
	require.NoError(t, err)
	stored, err := fakes.SettlementNotification().Save(ctx, notification)
	require.NoError(t, err)
	require.True(t, stored)

	return &listingFixture{
		client: client, notification: notification,
		sellerA: sellerA, sellerB: sellerB,
		qrisSettled: qrisSettled, qrisOpen: qrisOpen, card: card,
	}
}

func feeMinor(v int64) *int64 { return &v }

// The page a person opens: every row Singapay listed, in its order, each with the ledger's
// record of the payment — found by invoice number, by stored gateway id, or not at all.
func TestListSettlementTransactions_PutsEachRowBesideTheLedgersRecord(t *testing.T) {
	for _, store := range listingStores {
		t.Run(store.name, func(t *testing.T) {
			runListingScenario(t, store.client)
		})
	}
}

// scenarioGateway lists the scenario's settlement: two QRIS sales of seller A, a card sale
// of seller B found by its attempt's reference, a payment the ledger does not know and a
// row with nothing to go on.
func scenarioGateway() *listingGateway {
	return &listingGateway{
		fakeGateway: &fakeGateway{},
		rows: []singapay.SettlementTransaction{
			listedRow("647179096636882876", "INV-20261002183927-YF2GYY", "01SELLERAULID", 39275, feeMinor(27493)),
			listedRow("647179096522977462", "INV-20261002182028-R7LK6V", "01SELLERAULID", 22155, feeMinor(15509)),
			// A payment link is created with the invoice as reff_no, so its row may carry no
			// merchant reference the ledger knows; the attempt's own reference finds it.
			listedRow("984656477720260925231717415YVichz6Y", "", "01SELLERBULID", 68041, nil),
			// Settled at Singapay, unknown to the ledger: kept, on the seller its
			// sub-account names.
			listedRow("647170000000000001", "INV-NOT-OURS", "01SELLERBULID", 10000, feeMinor(70)),
			// Nothing to go on at all.
			listedRow("", "", "", 5000, nil),
		},
		pagination: singapay.Pagination{Count: 5, Total: 5, PerPage: 50, CurrentPage: 1, TotalPages: 1},
	}
}

func runListingScenario(t *testing.T, newClient func(t *testing.T, gateway PaymentGateway) *LedgerClient) {
	gw := scenarioGateway()
	f := newListingFixture(t, newClient(t, gw))

	page, err := f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 1, 0)
	require.NoError(t, err)

	// The settlement is read by Singapay's own id, at the default page size.
	assert.Equal(t, []string{"3937"}, gw.identifiers)
	assert.Equal(t, []singapay.ListSettlementTransactionsOptions{{Page: 1, PerPage: DefaultSettlementTransactionsPerPage}}, gw.options)

	assert.Equal(t, f.notification.UUID, page.Notification.UUID)
	assert.Equal(t, 1, page.Page)
	assert.Equal(t, 50, page.PerPage)
	assert.Equal(t, 5, page.Total)
	assert.False(t, page.HasMore)
	assertScenarioTransactions(t, f, page.Transactions)
}

// assertScenarioTransactions checks the scenario's five rows, however they were read: each
// beside the ledger's record — by invoice number, by stored gateway id, or not at all — and
// nothing booked.
func assertScenarioTransactions(t *testing.T, f *listingFixture, transactions []SettlementTransaction) {
	t.Helper()
	require.Len(t, transactions, 5)

	settled := transactions[0]
	assert.Equal(t, SettlementMatchInvoice, settled.Match)
	require.NotNil(t, settled.Transaction)
	assert.Equal(t, f.qrisSettled.UUID, settled.Transaction.UUID)
	assert.Equal(t, domain.TransactionStatusSettled, settled.Transaction.Status)
	require.NotNil(t, settled.Account)
	assert.Equal(t, f.sellerA.UUID, settled.Account.UUID)
	assert.Equal(t, "647179096636882876", settled.Gateway.TransactionID)
	assert.Equal(t, "01SELLERAULID", settled.Gateway.AccountID)
	assert.Equal(t, int64(3927500), settled.Gateway.AmountMinor)
	require.NotNil(t, settled.Gateway.FeeMinor)
	assert.Equal(t, int64(27493), *settled.Gateway.FeeMinor)
	assert.Equal(t, "IDR", settled.Gateway.Currency)
	assert.True(t, settled.Gateway.Settled)
	require.NotNil(t, settled.Gateway.SettledAt)
	assert.True(t, settled.Gateway.SettledAt.Equal(time.UnixMilli(1759795200000)))
	assert.Nil(t, settled.Gateway.PaidAt, "Singapay reported no processed time")

	// Settled at Singapay and still COMPLETED in the ledger: reported, not settled from here.
	open := transactions[1]
	assert.Equal(t, SettlementMatchInvoice, open.Match)
	require.NotNil(t, open.Transaction)
	assert.Equal(t, f.qrisOpen.UUID, open.Transaction.UUID)
	assert.Equal(t, domain.TransactionStatusCompleted, open.Transaction.Status)

	card := transactions[2]
	assert.Equal(t, SettlementMatchGatewayID, card.Match)
	require.NotNil(t, card.Transaction)
	assert.Equal(t, f.card.UUID, card.Transaction.UUID)
	require.NotNil(t, card.Account)
	assert.Equal(t, f.sellerB.UUID, card.Account.UUID)
	assert.Nil(t, card.Gateway.FeeMinor, "a fee Singapay did not report is not a zero fee")

	stranger := transactions[3]
	assert.Equal(t, SettlementMatchNone, stranger.Match)
	assert.Nil(t, stranger.Transaction)
	require.NotNil(t, stranger.Account, "the sub-account Singapay names still says whose it is")
	assert.Equal(t, f.sellerB.UUID, stranger.Account.UUID)

	bare := transactions[4]
	assert.Equal(t, SettlementMatchNone, bare.Match)
	assert.Nil(t, bare.Transaction)
	assert.Nil(t, bare.Account)

	// Read only: the open transaction is still COMPLETED and nothing was booked.
	stillOpen, err := f.client.repoProvider.ProductTransaction().GetByID(context.Background(), f.qrisOpen.UUID)
	require.NoError(t, err)
	assert.Equal(t, domain.TransactionStatusCompleted, stillOpen.Status)
	entries, err := f.client.repoProvider.LedgerEntry().GetBySourceID(context.Background(), f.qrisOpen.UUID)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestListSettlementTransactions_PageBounds(t *testing.T) {
	gw := &listingGateway{fakeGateway: &fakeGateway{}}
	f := newListingFixture(t, fakeListingClient(t, gw))

	_, err := f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 0, 1000)
	require.NoError(t, err)
	_, err = f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 3, 20)
	require.NoError(t, err)

	assert.Equal(t, []singapay.ListSettlementTransactionsOptions{
		{Page: 1, PerPage: MaxSettlementTransactionsPerPage},
		{Page: 3, PerPage: 20},
	}, gw.options)
}

// Whether a next page exists comes from Singapay's counts when it sends them, and from a
// full page when it does not.
func TestListSettlementTransactions_HasMore(t *testing.T) {
	two := []singapay.SettlementTransaction{
		listedRow("a", "INV-A", "", 1000, nil),
		listedRow("b", "INV-B", "", 1000, nil),
	}
	tests := []struct {
		name       string
		pagination singapay.Pagination
		rows       []singapay.SettlementTransaction
		page       int
		want       bool
	}{
		{"pages left", singapay.Pagination{CurrentPage: 2, PerPage: 2, Total: 6, TotalPages: 3}, two, 2, true},
		{"last page", singapay.Pagination{CurrentPage: 3, PerPage: 2, Total: 6, TotalPages: 3}, two, 3, false},
		{"total only", singapay.Pagination{Total: 5}, two, 2, true},
		{"total reached", singapay.Pagination{Total: 4}, two, 2, false},
		{"no counts, full page", singapay.Pagination{}, two, 1, true},
		{"no counts, short page", singapay.Pagination{}, two[:1], 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := &listingGateway{fakeGateway: &fakeGateway{}, rows: tc.rows, pagination: tc.pagination}
			f := newListingFixture(t, fakeListingClient(t, gw))

			page, err := f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, tc.page, 2)
			require.NoError(t, err)
			assert.Equal(t, tc.want, page.HasMore)
		})
	}
}

func TestListSettlementTransactions_UnknownNotification(t *testing.T) {
	gw := &listingGateway{fakeGateway: &fakeGateway{}}
	f := newListingFixture(t, fakeListingClient(t, gw))

	_, err := f.client.ListSettlementTransactions(context.Background(), "no-such-notification", 1, 0)

	assert.Equal(t, ledgererr.CodeSettlementNotificationNotFound, outerCode(t, err), "got %v", err)
	assert.Empty(t, gw.identifiers, "Singapay is not asked about a settlement nobody stored")

	_, err = f.client.ListSettlementTransactions(context.Background(), "", 1, 0)
	assert.Equal(t, ledgererr.CodeInvalidRequest, outerCode(t, err), "got %v", err)
}

// Singapay's 404 is an answer — the batch is not completed, or not this merchant's — and
// it is told apart from Singapay failing to answer.
func TestListSettlementTransactions_GatewayErrors(t *testing.T) {
	notListed := &listingGateway{fakeGateway: &fakeGateway{},
		err: &singapay.Error{StatusCode: http.StatusNotFound, Message: "Account not found."}}
	f := newListingFixture(t, fakeListingClient(t, notListed))
	_, err := f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 1, 0)
	assert.Equal(t, ledgererr.CodeSettlementNotListed, outerCode(t, err), "got %v", err)

	down := &listingGateway{fakeGateway: &fakeGateway{},
		err: &singapay.Error{StatusCode: http.StatusBadGateway, Message: "Bad Gateway"}}
	f = newListingFixture(t, fakeListingClient(t, down))
	_, err = f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 1, 0)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err), "got %v", err)
	gatewayErr, ok := singapay.AsError(err)
	require.True(t, ok, "Singapay's own error stays readable through the ledger's")
	assert.Equal(t, http.StatusBadGateway, gatewayErr.StatusCode)

	timeout := &listingGateway{fakeGateway: &fakeGateway{}, err: context.DeadlineExceeded}
	f = newListingFixture(t, fakeListingClient(t, timeout))
	_, err = f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 1, 0)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err), "got %v", err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

// Two transactions answering to one reference: the paid one is the payment Singapay
// settled, and between equals the newer one is.
func TestPreferredTransaction(t *testing.T) {
	at := func(status domain.TransactionStatus, created time.Time) *domain.ProductTransaction {
		fee, err := domain.NewFeeBreakdown(10000, 0, 0, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
		require.NoError(t, err)
		tx := createTestProductTransaction("INV-DUP", "seller", fee)
		tx.Status = status
		tx.CreatedAt = created
		return tx
	}
	early := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)

	pending := at(domain.TransactionStatusPending, late)
	completed := at(domain.TransactionStatusCompleted, early)
	settled := at(domain.TransactionStatusSettled, early)
	newerSettled := at(domain.TransactionStatusSettled, late)

	assert.Same(t, completed, preferredTransaction(nil, completed))
	assert.Same(t, completed, preferredTransaction(pending, completed))
	assert.Same(t, completed, preferredTransaction(completed, pending))
	assert.Same(t, settled, preferredTransaction(completed, settled))
	assert.Same(t, newerSettled, preferredTransaction(settled, newerSettled))
	assert.Same(t, newerSettled, preferredTransaction(newerSettled, settled))
}

// Singapay's documentation lists no `page` parameter. If it answers page 1 to a request for
// page 2, those rows are not page 2's: the page comes back empty and the listing ends, rather
// than showing the first rows again.
func TestListSettlementTransactions_APageSingapayDidNotHonour(t *testing.T) {
	gw := &listingGateway{
		fakeGateway: &fakeGateway{},
		rows:        []singapay.SettlementTransaction{listedRow("a", "INV-A", "", 1000, nil), listedRow("b", "INV-B", "", 1000, nil)},
		pagination:  singapay.Pagination{CurrentPage: 1, PerPage: 2, Total: 6, TotalPages: 3},
	}
	f := newListingFixture(t, fakeListingClient(t, gw))

	page, err := f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 2, 2)
	require.NoError(t, err)

	assert.Empty(t, page.Transactions)
	assert.False(t, page.HasMore)
	assert.Equal(t, 2, page.Page)
}

// A row whose merchant_reff_no names nothing the ledger knows is still found when its
// transaction_id is an invoice number — a payment link's reference may land there.
func TestListSettlementTransactions_TransactionIDAsInvoice(t *testing.T) {
	gw := &listingGateway{fakeGateway: &fakeGateway{}}
	f := newListingFixture(t, fakeListingClient(t, gw))
	gw.rows = []singapay.SettlementTransaction{listedRow(f.qrisOpen.InvoiceNumber, "", "01SELLERAULID", 22155, nil)}

	page, err := f.client.ListSettlementTransactions(context.Background(), f.notification.UUID, 1, 0)
	require.NoError(t, err)

	require.Len(t, page.Transactions, 1)
	assert.Equal(t, SettlementMatchInvoice, page.Transactions[0].Match)
	require.NotNil(t, page.Transactions[0].Transaction)
	assert.Equal(t, f.qrisOpen.UUID, page.Transactions[0].Transaction.UUID)
}

// pagedListingGateway answers the settlement listing a page at a time out of one scripted
// listing, as Singapay pages it, and records how many reads were in flight at once.
type pagedListingGateway struct {
	*fakeGateway
	rows []singapay.SettlementTransaction

	// counts sends total and total_pages; bare sends no pagination at all.
	counts bool
	bare   bool
	// singapayPerPage is Singapay's own page size cap; 0 honours the asked per_page.
	singapayPerPage int
	// ignorePageFrom answers page 1 for this page and every later one: Singapay not honouring
	// `page`.
	ignorePageFrom int
	// failPage fails with failErr.
	failPage int
	failErr  error

	mu          sync.Mutex
	asked       []singapay.ListSettlementTransactionsOptions
	inFlight    int
	maxInFlight int
}

func (g *pagedListingGateway) ListSettlementTransactions(ctx context.Context, _ string, opts singapay.ListSettlementTransactionsOptions) ([]singapay.SettlementTransaction, singapay.Pagination, error) {
	g.mu.Lock()
	g.asked = append(g.asked, opts)
	g.inFlight++
	g.maxInFlight = max(g.maxInFlight, g.inFlight)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()

	// Long enough for the reads in flight to overlap.
	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
		return nil, singapay.Pagination{}, ctx.Err()
	}
	if opts.Page == g.failPage {
		return nil, singapay.Pagination{}, g.failErr
	}

	perPage := opts.PerPage
	if g.singapayPerPage > 0 {
		perPage = min(perPage, g.singapayPerPage)
	}
	page := opts.Page
	if g.ignorePageFrom > 0 && page >= g.ignorePageFrom {
		page = 1
	}
	from := min((page-1)*perPage, len(g.rows))
	to := min(from+perPage, len(g.rows))
	if g.bare {
		return g.rows[from:to], singapay.Pagination{}, nil
	}
	pagination := singapay.Pagination{Count: to - from, PerPage: perPage, CurrentPage: page}
	if g.counts {
		pagination.Total = len(g.rows)
		pagination.TotalPages = (len(g.rows) + perPage - 1) / perPage
	}
	return g.rows[from:to], pagination, nil
}

func (g *pagedListingGateway) askedPages() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	pages := make([]int, 0, len(g.asked))
	for _, opts := range g.asked {
		pages = append(pages, opts.Page)
	}
	sort.Ints(pages)
	return pages
}

// numberedRows lists n payments T0001 …, each of Rp1.000.
func numberedRows(n int) []singapay.SettlementTransaction {
	rows := make([]singapay.SettlementTransaction, n)
	for i := range rows {
		id := fmt.Sprintf("T%04d", i+1)
		rows[i] = listedRow(id, "INV-"+id, "01SELLERAULID", 1000, feeMinor(700))
	}
	return rows
}

func listedIDs(rows []SettlementGatewayTransaction) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.TransactionID
	}
	return ids
}

func gatewayIDs(rows []singapay.SettlementTransaction) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.TransactionID
	}
	return ids
}

// Every page is read, a hundred rows a page, the pages after the first a few at a time, and
// the rows come back in Singapay's order whatever order the pages answered in.
func TestReadSettlementListing_ReadsEveryPage(t *testing.T) {
	gw := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(1_050), counts: true}
	f := newListingFixture(t, fakeListingClient(t, gw))

	listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
	require.NoError(t, err)

	assert.Equal(t, f.notification.UUID, listing.Notification.UUID)
	assert.True(t, listing.Complete)
	assert.Equal(t, 1_050, listing.Total)
	assert.Equal(t, gatewayIDs(gw.rows), listedIDs(listing.Rows))
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, gw.askedPages())
	for _, opts := range gw.asked {
		assert.Equal(t, MaxSettlementTransactionsPerPage, opts.PerPage)
	}
	assert.LessOrEqual(t, gw.maxInFlight, settlementListingConcurrency)
	assert.Greater(t, gw.maxInFlight, 1, "the pages after the first are read side by side")

	row := listing.Rows[0]
	assert.Equal(t, "INV-T0001", row.MerchantReference)
	assert.Equal(t, int64(100000), row.AmountMinor)
	require.NotNil(t, row.FeeMinor)
	assert.Equal(t, int64(700), *row.FeeMinor)
}

// Without counts, pages are read one after another until one comes back short — an empty
// page after a full one included.
func TestReadSettlementListing_NoCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  int
		pages []int
	}{
		{"short last page", 250, []int{1, 2, 3}},
		{"empty page after full ones", 200, []int{1, 2, 3}},
		{"one short page", 7, []int{1}},
		{"nothing listed", 0, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(tc.rows), bare: true}
			f := newListingFixture(t, fakeListingClient(t, gw))

			listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
			require.NoError(t, err)

			assert.True(t, listing.Complete)
			assert.Equal(t, 0, listing.Total)
			assert.Len(t, listing.Rows, tc.rows)
			assert.Equal(t, tc.pages, gw.askedPages())
			assert.Equal(t, 1, gw.maxInFlight)
		})
	}
}

// The row cap ends the read: more pages are not asked for, and the listing says it is not
// complete.
func TestReadSettlementListing_RowCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		gw   *pagedListingGateway
	}{
		{"counted", &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(450), counts: true}},
		{"not counted", &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(450), bare: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListingFixture(t, fakeListingClient(t, tc.gw))

			listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 150)
			require.NoError(t, err)

			assert.False(t, listing.Complete)
			assert.Equal(t, gatewayIDs(tc.gw.rows[:150]), listedIDs(listing.Rows))
			assert.Equal(t, []int{1, 2}, tc.gw.askedPages())
		})
	}

	// Exactly at the cap is complete.
	gw := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(200), counts: true}
	f := newListingFixture(t, fakeListingClient(t, gw))
	listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 200)
	require.NoError(t, err)
	assert.True(t, listing.Complete)
	assert.Len(t, listing.Rows, 200)
}

// Singapay answering smaller pages than asked for still pages through everything: its page
// numbers count its own pages.
func TestReadSettlementListing_SingapaysOwnPageSize(t *testing.T) {
	gw := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(120), counts: true, singapayPerPage: 50}
	f := newListingFixture(t, fakeListingClient(t, gw))

	listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
	require.NoError(t, err)

	assert.True(t, listing.Complete)
	assert.Equal(t, gatewayIDs(gw.rows), listedIDs(listing.Rows))
	assert.Equal(t, []int{1, 2, 3}, gw.askedPages())
}

// A page Singapay answers as another page ends the listing there: the rows before it are
// kept and nothing is shown twice.
func TestReadSettlementListing_APageSingapayDidNotHonour(t *testing.T) {
	gw := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(250), counts: true, ignorePageFrom: 2}
	f := newListingFixture(t, fakeListingClient(t, gw))

	listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
	require.NoError(t, err)

	assert.False(t, listing.Complete)
	assert.Equal(t, gatewayIDs(gw.rows[:100]), listedIDs(listing.Rows))
}

// One page failing fails the read with that page's error, never a listing with a hole in
// it, and never the cancellation it caused in the pages beside it.
func TestReadSettlementListing_Errors(t *testing.T) {
	down := &singapay.Error{StatusCode: http.StatusBadGateway, Message: "Bad Gateway"}
	gw := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(1_000), counts: true, failPage: 3, failErr: down}
	f := newListingFixture(t, fakeListingClient(t, gw))
	_, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err), "got %v", err)
	gatewayErr, ok := singapay.AsError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusBadGateway, gatewayErr.StatusCode)

	notListed := &pagedListingGateway{fakeGateway: &fakeGateway{}, failPage: 1,
		failErr: &singapay.Error{StatusCode: http.StatusNotFound, Message: "Account not found."}}
	f = newListingFixture(t, fakeListingClient(t, notListed))
	_, err = f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
	assert.Equal(t, ledgererr.CodeSettlementNotListed, outerCode(t, err), "got %v", err)

	idle := &pagedListingGateway{fakeGateway: &fakeGateway{}}
	f = newListingFixture(t, fakeListingClient(t, idle))
	_, err = f.client.ReadSettlementListing(context.Background(), "no-such-notification", 0)
	assert.Equal(t, ledgererr.CodeSettlementNotificationNotFound, outerCode(t, err), "got %v", err)
	assert.Empty(t, idle.asked, "Singapay is not asked about a settlement nobody stored")

	// The caller's deadline ending mid-read is an error too, not a short listing.
	slow := &pagedListingGateway{fakeGateway: &fakeGateway{}, rows: numberedRows(1_000), counts: true}
	f = newListingFixture(t, fakeListingClient(t, slow))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Millisecond)
	defer cancel()
	_, err = f.client.ReadSettlementListing(ctx, f.notification.UUID, 0)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err), "got %v", err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
}

// A listing read whole and matched afterwards puts every row beside the ledger's record
// exactly as the page-at-a-time read does.
func TestMatchSettlementTransactions_PutsEachRowBesideTheLedgersRecord(t *testing.T) {
	for _, store := range listingStores {
		t.Run(store.name, func(t *testing.T) {
			gw := scenarioGateway()
			f := newListingFixture(t, store.client(t, gw))

			listing, err := f.client.ReadSettlementListing(context.Background(), f.notification.UUID, 0)
			require.NoError(t, err)
			assert.True(t, listing.Complete)
			assert.Equal(t, 5, listing.Total)
			assert.Equal(t, []singapay.ListSettlementTransactionsOptions{{Page: 1, PerPage: MaxSettlementTransactionsPerPage}}, gw.options)

			matched, err := f.client.MatchSettlementTransactions(context.Background(), listing.Rows)
			require.NoError(t, err)
			assertScenarioTransactions(t, f, matched)

			// The ledger's side is read again on every match: a payment settled since is
			// shown settled.
			require.NoError(t, f.client.repoProvider.ProductTransaction().UpdateStatus(context.Background(),
				f.qrisOpen.UUID, domain.TransactionStatusSettled, time.Now()))
			again, err := f.client.MatchSettlementTransactions(context.Background(), listing.Rows)
			require.NoError(t, err)
			require.NotNil(t, again[1].Transaction)
			assert.Equal(t, domain.TransactionStatusSettled, again[1].Transaction.Status)

			none, err := f.client.MatchSettlementTransactions(context.Background(), nil)
			require.NoError(t, err)
			assert.Empty(t, none)
		})
	}
}
