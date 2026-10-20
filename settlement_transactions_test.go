package ledger

import (
	"context"
	"errors"
	"net/http"
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

func runListingScenario(t *testing.T, newClient func(t *testing.T, gateway PaymentGateway) *LedgerClient) {
	gw := &listingGateway{
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
	require.Len(t, page.Transactions, 5)

	settled := page.Transactions[0]
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
	open := page.Transactions[1]
	assert.Equal(t, SettlementMatchInvoice, open.Match)
	require.NotNil(t, open.Transaction)
	assert.Equal(t, f.qrisOpen.UUID, open.Transaction.UUID)
	assert.Equal(t, domain.TransactionStatusCompleted, open.Transaction.Status)

	card := page.Transactions[2]
	assert.Equal(t, SettlementMatchGatewayID, card.Match)
	require.NotNil(t, card.Transaction)
	assert.Equal(t, f.card.UUID, card.Transaction.UUID)
	require.NotNil(t, card.Account)
	assert.Equal(t, f.sellerB.UUID, card.Account.UUID)
	assert.Nil(t, card.Gateway.FeeMinor, "a fee Singapay did not report is not a zero fee")

	stranger := page.Transactions[3]
	assert.Equal(t, SettlementMatchNone, stranger.Match)
	assert.Nil(t, stranger.Transaction)
	require.NotNil(t, stranger.Account, "the sub-account Singapay names still says whose it is")
	assert.Equal(t, f.sellerB.UUID, stranger.Account.UUID)

	bare := page.Transactions[4]
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
