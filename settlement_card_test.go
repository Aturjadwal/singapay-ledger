package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// paymentLinkHistories is one sub-account's payment-link history, answered the way
// Singapay answers it.
//
// The fake it replaces returned every row whatever was asked, which is how the v0.7.0
// fallback passed its tests and found nothing in production. Singapay's reff_no filter
// never looks at payment_link_reff_no, which is where the invoice number lives; by every
// sign it matches a row's OWN reff_no — the reference of one payment attempt — partially.
// So this fake filters that way, orders newest first, cuts pages, and records every call,
// so a test can count what a lookup cost. Anything it does not model panics rather than
// answering something plausible.
type paymentLinkHistories struct {
	*fakeGateway
	rows []singapay.PaymentLinkHistory

	// perPageCap is the most rows one page holds whatever per_page asks for, the way an
	// endpoint may cap it. Zero is no cap.
	perPageCap int
	// omitTotalPages answers without total_pages, so only the rows can end a scan.
	omitTotalPages bool
	// beforeList runs at the start of every listing with its 1-based call number.
	beforeList func(call int)

	// lists is every listing asked for and gets every history id read directly, in order.
	lists []singapay.SettlementWindow
	gets  []int64
}

func (g *paymentLinkHistories) ListPaymentLinkHistories(ctx context.Context, _ string, w singapay.SettlementWindow) ([]singapay.PaymentLinkHistory, singapay.Pagination, error) {
	g.lists = append(g.lists, w)
	if g.beforeList != nil {
		g.beforeList(len(g.lists))
	}
	if err := ctx.Err(); err != nil {
		return nil, singapay.Pagination{}, &singapay.Error{Err: err}
	}
	if w.SettleFrom != "" || w.SettleTo != "" || w.Settled != nil || w.Status != "" {
		panic("paymentLinkHistories: only reff_no, page and per_page are modelled")
	}
	if w.PerPage <= 0 {
		panic("paymentLinkHistories: a listing without per_page relies on an undocumented default")
	}

	var matched []singapay.PaymentLinkHistory
	for _, row := range g.rows {
		if w.ReffNo != "" && !strings.Contains(row.ReffNo, w.ReffNo) {
			continue
		}
		matched = append(matched, row)
	}
	// Newest first. Rows without a created_at sort after the dated ones and keep the order
	// they were given in.
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].CreatedAt.Time.After(matched[j].CreatedAt.Time)
	})

	perPage := w.PerPage
	if g.perPageCap > 0 && perPage > g.perPageCap {
		perPage = g.perPageCap
	}
	page := max(w.Page, 1)

	var out []singapay.PaymentLinkHistory
	if start := (page - 1) * perPage; start < len(matched) {
		out = matched[start:min(start+perPage, len(matched))]
	}

	pagination := singapay.Pagination{Count: len(out), PerPage: perPage, CurrentPage: page}
	if !g.omitTotalPages {
		pagination.Total = len(matched)
		pagination.TotalPages = (len(matched) + perPage - 1) / perPage
	}
	return out, pagination, nil
}

func (g *paymentLinkHistories) GetPaymentLinkHistory(_ context.Context, accountID string, historyID int64) (*singapay.PaymentLinkHistory, error) {
	g.gets = append(g.gets, historyID)
	for _, row := range g.rows {
		if row.ID == historyID {
			found := row
			return &found, nil
		}
	}
	return nil, &singapay.Error{
		StatusCode: http.StatusNotFound,
		Message:    fmt.Sprintf("No result for account_id: %s with history_id: %d", accountID, historyID),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reading a card or payment-link payment back
// ─────────────────────────────────────────────────────────────────────────────
//
// The fixtures replay the production incident (INV-20260925161538-SWIMLQ): a card payment —
// a payment link pinned to the card methods — booked by a money-in webhook that carried no
// numeric id, so its payment request holds "0" and no reference, while Singapay's history
// holds the paid attempt, 80937, among the seller's other payments.

const (
	// cardInvoice is the invoice newCheckFixture books, and so the link's reference.
	cardInvoice = "INV-CHECK-1"
	// cardAttemptRef is the paid attempt's own reff_no: Singapay's reference for one
	// attempt, which a payment-link webhook reports as transaction.reff_no.
	cardAttemptRef = "18917720251110094037705"
	// cardSellerAccount is the seller's Singapay sub-account in newCheckFixture.
	cardSellerAccount = "01SELLERACCOUNTULID"
)

// cardCreatedAt is when the card transaction was created. A scan's cutoff is an hour before.
var cardCreatedAt = time.Date(2026, 9, 25, 16, 15, 38, 0, time.UTC)

// cardAttempt is the attempt that paid the card transaction: Rp24.169, newCheckFixture's
// total, against the link whose reference is the invoice.
func cardAttempt(hasSettle bool, createdAt time.Time) singapay.PaymentLinkHistory {
	return singapay.PaymentLinkHistory{
		ID:                80937,
		ReffNo:            cardAttemptRef,
		PaymentLinkReffNo: cardInvoice,
		Amount:            singapay.NewAmount(24169, "IDR"),
		Status:            singapay.PaymentPaid,
		HasSettle:         hasSettle,
		CreatedAt:         singapay.ISOTime{Time: createdAt, Set: true},
	}
}

// otherAttempts is n paid attempts at other links on the same sub-account: the first at
// newest, each one a minute older than the one before.
func otherAttempts(n int, newest time.Time) []singapay.PaymentLinkHistory {
	rows := make([]singapay.PaymentLinkHistory, n)
	for i := range rows {
		rows[i] = singapay.PaymentLinkHistory{
			ID:                int64(100000 + i),
			ReffNo:            fmt.Sprintf("2891772025111%010d", i),
			PaymentLinkReffNo: fmt.Sprintf("INV-OTHER-%04d", i),
			Amount:            singapay.NewAmount(50000, "IDR"),
			Status:            singapay.PaymentPaid,
			HasSettle:         true,
			CreatedAt:         singapay.ISOTime{Time: newest.Add(-time.Duration(i) * time.Minute), Set: true},
		}
	}
	return rows
}

// newCardFixture is newCheckFixture's sale paid by card and created at createdAt, with its
// payment request as the money-in webhook left it before this release: the link's id as
// request_id, "0" as the payment's id, and no reference.
func newCardFixture(t *testing.T, gw *paymentLinkHistories, createdAt time.Time) (*checkFixture, *domain.PaymentRequest) {
	t.Helper()
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	f.tx.CreatedAt = createdAt

	pr := f.paymentRequest(t)
	pr.PaymentChannel = ChannelCreditCard
	pr.RequestID = "98465"
	pr.GatewayTransactionID = "0"
	pr.GatewayTransactionRef = ""
	require.NoError(t, f.fakes.PaymentRequest().Update(context.Background(), pr))
	return f, pr
}

// paymentRequest reads the fixture's payment request as the repository holds it.
func (f *checkFixture) paymentRequest(t *testing.T) *domain.PaymentRequest {
	t.Helper()
	pr, err := f.fakes.PaymentRequest().GetByProductTransactionID(context.Background(), f.tx.UUID)
	require.NoError(t, err)
	return pr
}

// ageForThePass backdates the payment past SettlementFloorAge, so a pass runs on age alone.
func (f *checkFixture) ageForThePass() {
	paidAt := time.Now().Add(-2 * SettlementFloorAge)
	f.fakes.productTransactionRepo.transactions[f.tx.UUID].CompletedAt = &paidAt
}

// The fake has to reproduce the trap, or the tests below prove nothing. Filtered on the
// invoice number the history answers nothing — the invoice is the link's reference, and the
// filter reads only the attempt's own — while part of the attempt's reference finds it.
func TestPaymentLinkHistoriesFake_TheReffNoFilterNeverSeesTheInvoice(t *testing.T) {
	gw := &paymentLinkHistories{fakeGateway: &fakeGateway{}, rows: []singapay.PaymentLinkHistory{cardAttempt(true, cardCreatedAt)}}
	ctx := context.Background()

	byInvoice, _, err := gw.ListPaymentLinkHistories(ctx, cardSellerAccount, singapay.SettlementWindow{ReffNo: cardInvoice, PerPage: 25})
	require.NoError(t, err)
	assert.Empty(t, byInvoice)

	byAttempt, _, err := gw.ListPaymentLinkHistories(ctx, cardSellerAccount, singapay.SettlementWindow{ReffNo: "094037705", PerPage: 25})
	require.NoError(t, err)
	require.Len(t, byAttempt, 1, "a partial match on the attempt's own reff_no")
	assert.Equal(t, int64(80937), byAttempt[0].ID)
}

// The production case: the card payment's money-in webhook carried no numeric id, so "0"
// was stored, and every settling pass asked Singapay for history 0 and got a 404 — the
// transaction never settled. A "0" is no id: the attempt has to be found without one, and
// history 0 never asked for.
//
// v0.7.0 fails this against the fake above, as it failed in production: its fallback
// filtered the listing on the invoice number, and the filter cannot see an invoice.
func TestReadSettledTransaction_AStoredZeroIdFallsBackToTheInvoice(t *testing.T) {
	gw := &paymentLinkHistories{fakeGateway: &fakeGateway{}, rows: []singapay.PaymentLinkHistory{cardAttempt(true, cardCreatedAt.Add(2*time.Minute))}}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	settled, err := f.client.readSettledTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "80937", settled.GatewayTransactionID)
	assert.Equal(t, ChannelCreditCard, settled.PaymentChannel)
	assert.Empty(t, gw.gets, "history 0 is never asked for")
}

// The incident against a Singapay that filters like the real one: the attempt is on page 2
// of the account's history, behind a hundred newer payments to other links. The scan finds
// it, the settlement is read from it — booked under the card channel, at the priced fee,
// since a payment link reports none — and its id and reference are stored.
func TestReadSettledTransaction_ACardIsFoundByScanningTheHistory(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        append(otherAttempts(150, cardCreatedAt.Add(72*time.Hour)), cardAttempt(true, cardCreatedAt.Add(2*time.Minute))),
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	settled, err := f.client.readSettledTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "80937", settled.GatewayTransactionID)
	assert.Equal(t, ChannelCreditCard, settled.PaymentChannel)
	assert.Equal(t, cardInvoice, settled.MerchantReference)
	assert.Equal(t, int64(2416900), settled.GrossMinor)
	assert.Equal(t, int64(16900), settled.FeeMinor, "the priced fee, in sen")
	assert.Equal(t, int64(2400000), settled.NetMinor)
	assert.False(t, settled.FeeReported)

	require.Len(t, gw.lists, 2, "found on page 2")
	for i, w := range gw.lists {
		assert.Empty(t, w.ReffNo, "the scan is not filtered")
		assert.Equal(t, i+1, w.Page)
		assert.Equal(t, paymentLinkScanPerPage, w.PerPage)
	}
	assert.Empty(t, gw.gets)

	stored := f.paymentRequest(t)
	assert.Equal(t, "80937", stored.GatewayTransactionID)
	assert.Equal(t, cardAttemptRef, stored.GatewayTransactionRef)
}

// A card settles days after it is paid. Until then the answer is "found, not settled" — and
// the attempt is stored all the same, so the next check, and every pass until it settles,
// reads it directly instead of scanning for it again.
func TestCheckTransactionSettlement_AnUnsettledCardIsFoundAndRemembered(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        append(otherAttempts(150, cardCreatedAt.Add(72*time.Hour)), cardAttempt(false, cardCreatedAt.Add(2*time.Minute))),
	}
	f, _ := newCardFixture(t, gw, cardCreatedAt)
	before := f.entryCount(t)
	ctx := context.Background()

	result, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckNotSettled, result.Outcome)
	assert.True(t, result.Gateway.Found)
	assert.False(t, result.Gateway.Settled)
	assert.Equal(t, "paid", result.Gateway.Status)
	assert.Equal(t, "80937", result.Gateway.GatewayTransactionID)
	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t))
	assert.Equal(t, before, f.entryCount(t))

	stored := f.paymentRequest(t)
	assert.Equal(t, "80937", stored.GatewayTransactionID)
	assert.Equal(t, cardAttemptRef, stored.GatewayTransactionRef)
	require.Len(t, gw.lists, 2)

	again, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckNotSettled, again.Outcome)
	assert.True(t, again.Gateway.Found)
	assert.Equal(t, []int64{80937}, gw.gets, "the second check is a point lookup")
	assert.Len(t, gw.lists, 2, "and lists nothing")
}

// The history is newest first, and an attempt cannot predate its link. Once a page holds
// nothing at or after the transaction's creation, less the hour allowed for clock drift,
// nothing further down can be its payment: "not found" is proven, and the scan stops.
func TestReadPaymentLinkHistory_StopsOnceAPageIsOlderThanTheTransaction(t *testing.T) {
	// One attempt a minute, from 150 minutes after the transaction backwards, and no
	// total_pages, so only the rows can end the scan. The cutoff is 60 minutes before the
	// transaction: page 1 (+150..+51) and page 2 (+50..-49) are after it, page 3
	// (-50..-149) straddles it, and page 4 (-150..-249) is entirely before it.
	gw := &paymentLinkHistories{
		fakeGateway:    &fakeGateway{},
		rows:           otherAttempts(500, cardCreatedAt.Add(150*time.Minute)),
		omitTotalPages: true,
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	history, route, err := f.client.readPaymentLinkHistory(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	assert.Nil(t, history)
	assert.Equal(t, paymentLinkByScan, route)
	assert.Len(t, gw.lists, 4, "page 4 is the first with nothing at or after the cutoff")
}

// The history runs out: the page after the last answers no rows. A page shorter than
// per_page is not taken for the last — Singapay may cap per_page — so the scan asks once
// more before concluding.
func TestReadPaymentLinkHistory_StopsWhenTheHistoryRunsOut(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway:    &fakeGateway{},
		rows:           otherAttempts(150, cardCreatedAt.Add(72*time.Hour)),
		omitTotalPages: true,
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	history, _, err := f.client.readPaymentLinkHistory(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	assert.Nil(t, history)
	assert.Len(t, gw.lists, 3, "100 rows, 50 rows, then an empty page")
}

// When Singapay reports total_pages, its last page ends the scan without one more call.
func TestReadPaymentLinkHistory_StopsAtTheLastPageSingapayReports(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        otherAttempts(150, cardCreatedAt.Add(72*time.Hour)),
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	history, _, err := f.client.readPaymentLinkHistory(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	assert.Nil(t, history)
	assert.Len(t, gw.lists, 2)
}

// Singapay may hand back fewer rows than per_page asks for. The scan walks page numbers, so a
// capped page costs calls and never skips a row.
func TestReadPaymentLinkHistory_ACappedPageSizeSkipsNothing(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway:    &fakeGateway{},
		rows:           append(otherAttempts(120, cardCreatedAt.Add(72*time.Hour)), cardAttempt(true, cardCreatedAt.Add(2*time.Minute))),
		perPageCap:     50,
		omitTotalPages: true,
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	history, _, err := f.client.readPaymentLinkHistory(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, history)
	assert.Equal(t, int64(80937), history.ID)
	assert.Len(t, gw.lists, 3, "the attempt is the 121st row: page 3 of 50")
}

// Twenty full pages, every row after the transaction and none of them its payment: the
// history was not read back far enough to say either way. That is an error — the check
// answers that Singapay could not be read and changes nothing, and the pass counts a
// failure to retry — never "not found".
func TestCheckTransactionSettlement_AnInconclusiveScanIsAGatewayError(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway:    &fakeGateway{},
		rows:           otherAttempts(2100, cardCreatedAt.Add(40*time.Hour)),
		omitTotalPages: true,
	}
	f, _ := newCardFixture(t, gw, cardCreatedAt)
	before := f.entryCount(t)
	ctx := context.Background()

	result, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err))
	assert.Contains(t, err.Error(), "inconclusive")
	assert.Contains(t, err.Error(), cardInvoice)
	assert.Contains(t, err.Error(), "2026-09-25T16:15:38Z", "the transaction's creation, which the scan did not reach")
	assert.Len(t, gw.lists, paymentLinkScanMaxPages)

	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t))
	assert.Equal(t, before, f.entryCount(t))
	assert.Equal(t, "0", f.paymentRequest(t).GatewayTransactionID, "nothing is stored either")

	f.ageForThePass()
	pass, err := f.client.ProcessSettlementNotifications(ctx, 10)

	require.NoError(t, err)
	assert.Equal(t, 1, pass.Failed)
	require.Len(t, pass.Errors, 1)
	assert.Contains(t, pass.Errors[0].Reason, "inconclusive")
	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t), "left for the next pass")
}

// A stored id whose attempt belongs to another invoice is refused, not read: its amount is
// another payment's, and settling this transaction on it would move the wrong money. The
// pass counts a failure and books nothing, and the id is not second-guessed by a search.
func TestProcessSettlementNotifications_AStoredIdOfAnotherInvoiceIsRefused(t *testing.T) {
	someoneElses := cardAttempt(true, cardCreatedAt.Add(2*time.Minute))
	someoneElses.PaymentLinkReffNo = "INV-SOMEONE-ELSE"
	gw := &paymentLinkHistories{fakeGateway: &fakeGateway{}, rows: []singapay.PaymentLinkHistory{someoneElses}}
	f, pr := newCardFixture(t, gw, cardCreatedAt)
	pr.GatewayTransactionID = "80937"
	require.NoError(t, f.fakes.PaymentRequest().Update(context.Background(), pr))
	f.ageForThePass()
	before := f.entryCount(t)

	result, err := f.client.ProcessSettlementNotifications(context.Background(), 10)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Examined)
	assert.Equal(t, 1, result.Failed)
	assert.Zero(t, result.Settled)
	require.Len(t, result.Errors, 1)
	assert.Contains(t, result.Errors[0].Reason, `payment link history 80937 belongs to "INV-SOMEONE-ELSE", not "INV-CHECK-1"`)

	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t))
	assert.Equal(t, before, f.entryCount(t), "bookSettlement is never reached")
	assert.Equal(t, []int64{80937}, gw.gets)
	assert.Empty(t, gw.lists)
}

// The reference a payment-link webhook now stores is the attempt's own reff_no, so one
// filtered listing finds the attempt — no scan — and its id is stored beside it.
func TestReadSettledTransaction_AStoredReferenceFindsTheAttemptInOneCall(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        append(otherAttempts(300, cardCreatedAt.Add(72*time.Hour)), cardAttempt(true, cardCreatedAt.Add(2*time.Minute))),
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)
	pr.GatewayTransactionID = ""
	pr.GatewayTransactionRef = cardAttemptRef
	require.NoError(t, f.fakes.PaymentRequest().Update(context.Background(), pr))

	settled, err := f.client.readSettledTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "80937", settled.GatewayTransactionID)
	require.Len(t, gw.lists, 1)
	assert.Equal(t, cardAttemptRef, gw.lists[0].ReffNo)
	assert.Empty(t, gw.gets)
	assert.Equal(t, "80937", f.paymentRequest(t).GatewayTransactionID)
}

// Whether the webhook's reference is the history row's reff_no is not confirmed against
// Singapay. If it is not, the filtered listing comes back empty, which proves nothing: the
// scan runs and finds the attempt by its link reference. The stored reference is kept — a
// stored value is never replaced — and the id is filled in.
func TestReadSettledTransaction_AReferenceThatMatchesNothingFallsThroughToTheScan(t *testing.T) {
	const webhookRef = "77777720251110094099999"
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        append(otherAttempts(150, cardCreatedAt.Add(72*time.Hour)), cardAttempt(true, cardCreatedAt.Add(2*time.Minute))),
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)
	pr.GatewayTransactionRef = webhookRef
	require.NoError(t, f.fakes.PaymentRequest().Update(context.Background(), pr))

	settled, err := f.client.readSettledTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "80937", settled.GatewayTransactionID)
	require.Len(t, gw.lists, 3, "the filtered listing, then two pages of scan")
	assert.Equal(t, webhookRef, gw.lists[0].ReffNo)
	assert.Empty(t, gw.lists[1].ReffNo)

	stored := f.paymentRequest(t)
	assert.Equal(t, "80937", stored.GatewayTransactionID)
	assert.Equal(t, webhookRef, stored.GatewayTransactionRef, "a stored reference is never replaced")
}

// A valid stored id is a point lookup, verified against the invoice: nothing is listed, and
// nothing new is stored.
func TestReadSettledTransaction_AStoredIdIsAPointLookup(t *testing.T) {
	gw := &paymentLinkHistories{fakeGateway: &fakeGateway{}, rows: []singapay.PaymentLinkHistory{cardAttempt(true, cardCreatedAt.Add(2*time.Minute))}}
	f, pr := newCardFixture(t, gw, cardCreatedAt)
	pr.GatewayTransactionID = "80937"
	require.NoError(t, f.fakes.PaymentRequest().Update(context.Background(), pr))

	settled, err := f.client.readSettledTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "80937", settled.GatewayTransactionID)
	assert.Equal(t, []int64{80937}, gw.gets)
	assert.Empty(t, gw.lists)
	assert.Zero(t, f.fakes.paymentRequestRepo.recorded)
}

// A row without a created_at says nothing about how far back the history has gone, so a page
// of them cannot end the scan — although the zero time it would otherwise be read as is
// older than any transaction.
func TestReadPaymentLinkHistory_UndatedRowsDoNotEndTheScan(t *testing.T) {
	undated := otherAttempts(100, cardCreatedAt)
	for i := range undated {
		undated[i].CreatedAt = singapay.ISOTime{}
	}
	attempt := cardAttempt(true, cardCreatedAt)
	attempt.CreatedAt = singapay.ISOTime{}
	gw := &paymentLinkHistories{
		fakeGateway:    &fakeGateway{},
		rows:           append(undated, attempt),
		omitTotalPages: true,
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	history, _, err := f.client.readPaymentLinkHistory(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, history)
	assert.Equal(t, int64(80937), history.ID)
	assert.Len(t, gw.lists, 2)
}

// The on-demand check runs under a deadline — 45 seconds in the monoservice. A scan that
// reaches it fails with the deadline; it does not answer "not found".
func TestReadPaymentLinkHistory_ADeadlineMidScanIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	gw := &paymentLinkHistories{
		fakeGateway:    &fakeGateway{},
		rows:           otherAttempts(1000, cardCreatedAt.Add(72*time.Hour)),
		omitTotalPages: true,
		// The deadline passes while page 3 is being fetched.
		beforeList: func(call int) {
			if call == 3 {
				<-ctx.Done()
			}
		},
	}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	history, _, err := f.client.readPaymentLinkHistory(ctx, cardSellerAccount, f.tx, pr)

	require.Error(t, err)
	assert.Nil(t, history)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Len(t, gw.lists, 3)
}

// The incident end to end, through the daily pass. A card payment stored with id "0" has
// been COMPLETED for days. The pass finds its attempt by scanning, settles it — COMPLETED to
// SETTLED, the seller's share from pending to available — and stores the attempt's id. The
// next pass has nothing left to do, and a check afterwards reads the stored id and writes
// nothing: the transaction is settled exactly once.
func TestProcessSettlementNotifications_SettlesAStuckCardExactlyOnce(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        append(otherAttempts(150, cardCreatedAt.Add(72*time.Hour)), cardAttempt(true, cardCreatedAt.Add(2*time.Minute))),
	}
	f, _ := newCardFixture(t, gw, cardCreatedAt)
	f.ageForThePass()
	ctx := context.Background()

	result, err := f.client.ProcessSettlementNotifications(ctx, 10)

	require.NoError(t, err)
	assert.Equal(t, "floor_age", result.TriggerReason)
	assert.Equal(t, 1, result.Examined)
	assert.Equal(t, 1, result.Settled)
	assert.Zero(t, result.Failed)
	assert.Equal(t, domain.TransactionStatusSettled, f.status(t))

	pending, available, err := f.fakes.LedgerEntry().GetAllBalances(ctx, f.seller.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pending)
	assert.Equal(t, int64(20000), available)
	assert.Equal(t, "80937", f.paymentRequest(t).GatewayTransactionID)
	entries := f.entryCount(t)

	second, err := f.client.ProcessSettlementNotifications(ctx, 10)

	require.NoError(t, err)
	assert.Zero(t, second.Examined, "nothing is left COMPLETED")

	check, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckAlreadySettled, check.Outcome)
	assert.Equal(t, []int64{80937}, gw.gets, "the check reads the stored id")
	assert.Len(t, gw.lists, 2, "only the first pass scanned")

	assert.Equal(t, entries, f.entryCount(t))
	journals, err := f.fakes.Journal().GetBySourceID(ctx, domain.SourceTypeProductTransaction, f.tx.UUID)
	require.NoError(t, err)
	assert.Len(t, journals, 2, "the payment and one settlement")
}

// A link can carry several attempts, and only one took the money. Here the payer generated a
// VA, then a QRIS code they abandoned, and then paid the VA — so the newest attempt at the
// link is not the payment. The scan passes over it to the paid one, and only the paid one is
// stored: an abandoned attempt's id, once stored, would be read back as the payment from
// then on.
func TestReadSettledTransaction_TheAttemptThatPaidWinsOverANewerAbandonedOne(t *testing.T) {
	paidVA := cardAttempt(true, cardCreatedAt.Add(2*time.Minute))
	paidVA.ID, paidVA.ReffNo = 80936, "18917720251110094037704"
	abandonedQRIS := cardAttempt(false, cardCreatedAt.Add(5*time.Minute))
	abandonedQRIS.Status = singapay.PaymentExpired
	gw := &paymentLinkHistories{fakeGateway: &fakeGateway{}, rows: []singapay.PaymentLinkHistory{abandonedQRIS, paidVA}}
	f, pr := newCardFixture(t, gw, cardCreatedAt)
	pr.PaymentChannel = ChannelPaymentLink
	require.NoError(t, f.fakes.PaymentRequest().Update(context.Background(), pr))

	settled, err := f.client.readSettledTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "80936", settled.GatewayTransactionID)
	assert.Equal(t, ChannelPaymentLink, settled.PaymentChannel)

	stored := f.paymentRequest(t)
	assert.Equal(t, "80936", stored.GatewayTransactionID)
	assert.Equal(t, "18917720251110094037704", stored.GatewayTransactionRef)
}

// When no attempt at the link took the money, the newest one is still the answer — a check
// shows what Singapay holds — but nothing is stored.
func TestReadGatewayTransaction_AnAttemptThatDidNotPayIsShownButNotStored(t *testing.T) {
	abandoned := cardAttempt(false, cardCreatedAt.Add(5*time.Minute))
	abandoned.Status = singapay.PaymentExpired
	gw := &paymentLinkHistories{fakeGateway: &fakeGateway{}, rows: []singapay.PaymentLinkHistory{abandoned}}
	f, pr := newCardFixture(t, gw, cardCreatedAt)

	reading, err := f.client.readGatewayTransaction(context.Background(), cardSellerAccount, f.tx, pr)

	require.NoError(t, err)
	assert.True(t, reading.Found)
	assert.False(t, reading.HasSettle)
	assert.Equal(t, "expired", reading.Status)
	assert.Equal(t, "0", f.paymentRequest(t).GatewayTransactionID)
	assert.Zero(t, f.fakes.paymentRequestRepo.recorded)
}

// Storing what a search found is a shortcut for next time, not part of settling: when the
// write fails, the settlement is booked all the same, and the next read searches again.
func TestCheckTransactionSettlement_SettlesEvenWhenTheFoundAttemptCannotBeStored(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows:        append(otherAttempts(150, cardCreatedAt.Add(72*time.Hour)), cardAttempt(true, cardCreatedAt.Add(2*time.Minute))),
	}
	f, _ := newCardFixture(t, gw, cardCreatedAt)
	f.fakes.paymentRequestRepo.recordErr = errors.New("connection reset by peer")

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckSettled, result.Outcome)
	assert.Equal(t, domain.TransactionStatusSettled, f.status(t))
	assert.Equal(t, "0", f.paymentRequest(t).GatewayTransactionID)
}

func TestStoredGatewayTransactionID(t *testing.T) {
	for stored, want := range map[string]string{
		"":         "",
		"0":        "",
		" 0 ":      "",
		"-1":       "",
		"524493":   "524493",
		"VA-ABC-1": "VA-ABC-1", // not numeric: a business id, kept as given
	} {
		got := storedGatewayTransactionID(&domain.PaymentRequest{GatewayTransactionID: stored})
		assert.Equal(t, want, got, "stored %q", stored)
	}
}

// QRIS reads by the numeric id; a stored "0" must fall back to the instrument id, which
// for QRIS is the same entity.
func TestGatewayNumericID_AStoredZeroFallsBackToTheRequestID(t *testing.T) {
	id, err := gatewayNumericID(&domain.PaymentRequest{GatewayTransactionID: "0", RequestID: "361677"})

	require.NoError(t, err)
	assert.Equal(t, int64(361677), id)
}
