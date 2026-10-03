package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// qrisGateway answers the QRIS detail lookup an on-demand check makes, and counts it.
type qrisGateway struct {
	*fakeGateway
	qr    *singapay.QRISTransaction
	err   error
	calls int
}

func (g *qrisGateway) GetQRISTransaction(context.Context, string, int64) (*singapay.QRISTransaction, error) {
	g.calls++
	if g.err != nil {
		return nil, g.err
	}
	return g.qr, nil
}

// unpaidVAGateway is a virtual account nobody has paid into: the VA-number lookup
// returns no rows.
type unpaidVAGateway struct {
	*fakeGateway
}

func (g *unpaidVAGateway) GetVATransactionsByVANumber(context.Context, string, string) ([]singapay.VATransaction, singapay.Pagination, error) {
	return nil, singapay.Pagination{}, nil
}

// settledQRIS is Singapay's record of a QRIS payment of Rp24.169 whose funds have reached
// the merchant: Rp169,18 kept, Rp23.999,82 settled.
func settledQRIS(hasSettle bool) *singapay.QRISTransaction {
	qr := &singapay.QRISTransaction{
		ID:             524493,
		ReffNo:         "647179076721430151",
		MerchantReffNo: "INV-CHECK-1",
		Status:         "success",
		TotalAmount:    singapay.NewAmount(24169, "IDR"),
		MDRCost:        singapay.NewAmountFromMinor(16918, "IDR"),
		VendorFee:      singapay.NewAmountFromMinor(16918, "IDR"),
		HasSettle:      hasSettle,
	}
	if hasSettle {
		qr.SettledToMerchant = singapay.NewAmountFromMinor(2399982, "IDR")
		qr.SettleAt = singapay.ISOTime{Time: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC), Set: true}
	}
	return qr
}

type checkFixture struct {
	client   *LedgerClient
	fakes    *FakeRepositoryProvider
	tx       *domain.ProductTransaction
	seller   *domain.Account
	platform *domain.Account
}

// newCheckFixture books one paid QRIS sale of Rp20.000 (platform fee Rp4.000, gateway
// fee Rp169, paid by the customer) exactly as the money-in webhook books it, and leaves
// the transaction in status.
func newCheckFixture(t *testing.T, gateway PaymentGateway, status domain.TransactionStatus) *checkFixture {
	t.Helper()
	ctx := context.Background()
	fakes := NewFakeRepositoryProvider()

	platform := createTestAccount(domain.OwnerTypePlatform, "platform", "01PLATFORMACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, platform))
	gatewayAcc := createTestAccount(domain.OwnerTypePaymentGateway, "SINGAPAY", "")
	require.NoError(t, fakes.Account().Save(ctx, gatewayAcc))
	seller := createTestAccount(domain.OwnerTypeSeller, "seller-1", "01SELLERACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, seller))

	fee, err := domain.NewFeeBreakdown(20000, 4000, 169, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
	require.NoError(t, err)
	tx := createTestProductTransaction("INV-CHECK-1", seller.UUID, fee)
	tx.Status = status
	if status == domain.TransactionStatusPending {
		tx.CompletedAt = nil
	}
	if status == domain.TransactionStatusSettled {
		settledAt := time.Date(2026, 10, 1, 6, 0, 4, 0, time.UTC)
		tx.SettledAt = &settledAt
	}
	require.NoError(t, fakes.ProductTransaction().Save(ctx, tx))

	paymentReq := domain.NewPaymentRequest(tx.UUID, "524493", "QRIS", fee.TotalCharged, domain.CurrencyIDR, time.Now().Add(time.Hour))
	paymentReq.SetGatewayTransaction("524493", "")
	require.NoError(t, fakes.PaymentRequest().Save(ctx, paymentReq))

	if status != domain.TransactionStatusPending {
		journal := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
		require.NoError(t, fakes.Journal().Save(ctx, journal))
		require.NoError(t, fakes.LedgerEntry().SaveBatch(ctx, domain.NewPaymentEntries(
			journal.UUID, tx.UUID,
			seller.UUID, fee.SellerNetAmount,
			platform.UUID, fee.PlatformFee,
			gatewayAcc.UUID, fee.GatewayFee,
		)))
	}

	client := &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		gateway:      gateway,
		logger:       testLogger(),
	}
	return &checkFixture{client: client, fakes: fakes, tx: tx, seller: seller, platform: platform}
}

func (f *checkFixture) entryCount(t *testing.T) int {
	t.Helper()
	entries, err := f.fakes.LedgerEntry().GetBySourceID(context.Background(), f.tx.UUID)
	require.NoError(t, err)
	return len(entries)
}

func (f *checkFixture) status(t *testing.T) domain.TransactionStatus {
	t.Helper()
	tx, err := f.fakes.ProductTransaction().GetByID(context.Background(), f.tx.UUID)
	require.NoError(t, err)
	return tx.Status
}

// The case the button exists for: the funds settled at Singapay before the daily pass got
// to them. The check books the settlement the pass would have booked.
func TestCheckTransactionSettlement_SettlesWhatSingapaySettled(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	ctx := context.Background()

	result, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckSettled, result.Outcome)
	assert.Equal(t, domain.TransactionStatusCompleted, result.PreviousStatus)
	assert.Equal(t, domain.TransactionStatusSettled, result.Status, "read back from the row")
	require.NotNil(t, result.SettledAt)
	assert.Equal(t, "INV-CHECK-1", result.InvoiceNumber)
	assert.Equal(t, "QRIS", result.PaymentChannel)

	assert.True(t, result.Gateway.Found)
	assert.True(t, result.Gateway.Settled)
	assert.Equal(t, "success", result.Gateway.Status)
	assert.Equal(t, "524493", result.Gateway.GatewayTransactionID)
	assert.Equal(t, "01SELLERACCOUNTULID", result.Gateway.GatewayAccountID)
	assert.Equal(t, int64(2416900), result.Gateway.GrossMinor)
	assert.Equal(t, int64(2399982), result.Gateway.NetMinor)
	assert.Equal(t, int64(16918), result.Gateway.FeeMinor)
	require.NotNil(t, result.Gateway.SettledAt)
	assert.Equal(t, 1, gw.calls)

	assert.Equal(t, domain.TransactionStatusSettled, f.status(t))

	// The same entries the pass writes: the seller's share leaves PENDING for AVAILABLE.
	pending, available, err := f.fakes.LedgerEntry().GetAllBalances(ctx, f.seller.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pending)
	assert.Equal(t, int64(20000), available)

	settled, err := f.fakes.ProductTransaction().GetByID(ctx, f.tx.UUID)
	require.NoError(t, err)
	require.NotNil(t, settled.SettledGatewayFeeMinor)
	assert.Equal(t, int64(16918), *settled.SettledGatewayFeeMinor)

	journals, err := f.fakes.Journal().GetBySourceID(ctx, domain.SourceTypeProductTransaction, f.tx.UUID)
	require.NoError(t, err)
	assert.Len(t, journals, 2, "payment + settlement")
}

func TestCheckTransactionSettlement_NotSettledYetWritesNothing(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(false)}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	before := f.entryCount(t)

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckNotSettled, result.Outcome)
	assert.Equal(t, domain.TransactionStatusCompleted, result.Status)
	assert.Nil(t, result.SettledAt)
	assert.Nil(t, recordedTime(&time.Time{}), "the zero time a NULL scans into is no settlement date")
	assert.True(t, result.Gateway.Found)
	assert.False(t, result.Gateway.Settled)
	assert.Equal(t, int64(2416900), result.Gateway.GrossMinor)
	assert.Zero(t, result.Gateway.NetMinor, "no settled-to-merchant amount yet, so no net to report")
	assert.Zero(t, result.Gateway.FeeMinor, "and no fee derived from a net that is not there")

	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t))
	assert.Equal(t, before, f.entryCount(t))
}

// A transaction the ledger already settled is reported as such, and Singapay is still
// asked so its answer can be compared. Nothing is written a second time.
func TestCheckTransactionSettlement_AlreadySettledIsReadOnly(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusSettled)
	before := f.entryCount(t)

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckAlreadySettled, result.Outcome)
	assert.Equal(t, domain.TransactionStatusSettled, result.PreviousStatus)
	assert.Equal(t, domain.TransactionStatusSettled, result.Status)
	require.NotNil(t, result.SettledAt)
	assert.True(t, result.Gateway.Settled)
	assert.Equal(t, 1, gw.calls)
	assert.Equal(t, before, f.entryCount(t))
}

// A settlement is only ever booked on top of a booked payment. A PENDING transaction
// Singapay calls settled — a money-in webhook that never arrived — is shown, not settled.
func TestCheckTransactionSettlement_UnpaidTransactionIsNeverSettled(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusPending)

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckNotApplicable, result.Outcome)
	assert.Equal(t, domain.TransactionStatusPending, result.Status)
	assert.True(t, result.Gateway.Settled, "Singapay's answer is still reported")
	assert.Equal(t, domain.TransactionStatusPending, f.status(t))
	assert.Equal(t, 0, f.entryCount(t))
}

// A pass settles the transaction between the check's read and its write. The check's
// compare-and-set loses, writes nothing, and says the transaction was already settled.
func TestCheckTransactionSettlement_LosingTheRaceToThePassWritesNothing(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	before := f.entryCount(t)

	f.fakes.productTransactionRepo.beforeCAS = func() {
		moved, err := f.fakes.productTransactionRepo.updateStatusIf(f.tx.UUID,
			domain.TransactionStatusCompleted, domain.TransactionStatusSettled, time.Now())
		require.NoError(t, err)
		require.True(t, moved)
	}

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckAlreadySettled, result.Outcome)
	assert.Equal(t, domain.TransactionStatusCompleted, result.PreviousStatus)
	assert.Equal(t, domain.TransactionStatusSettled, result.Status)
	assert.Equal(t, before, f.entryCount(t), "the loser of the race must not book a second settlement")
}

// Singapay kept more than the whole platform fee: no split makes that right, so the
// check leaves the transaction COMPLETED for a person — as the pass does — and says why.
func TestCheckTransactionSettlement_BlockedFeeIsLeftForAPerson(t *testing.T) {
	qr := settledQRIS(true)
	qr.SettledToMerchant = singapay.NewAmount(19000, "IDR") // Rp5.169 kept against Rp169 priced
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: qr}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	before := f.entryCount(t)

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckBlocked, result.Outcome)
	assert.Contains(t, result.BlockedReason, "exceeds the entire platform fee")
	assert.Equal(t, domain.TransactionStatusCompleted, result.Status)
	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t))
	assert.Equal(t, before, f.entryCount(t))
}

func TestCheckTransactionSettlement_SingapayErrorIsAGatewayError(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, err: errors.New("connection reset by peer")}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)

	result, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err))
	assert.Equal(t, domain.TransactionStatusCompleted, f.status(t))
}

// A virtual account nobody has paid into is not an error: Singapay simply holds no
// payment against it yet.
func TestCheckTransactionSettlement_VirtualAccountWithNoPaymentIsNotSettled(t *testing.T) {
	f := newCheckFixture(t, &unpaidVAGateway{fakeGateway: &fakeGateway{}}, domain.TransactionStatusCompleted)
	ctx := context.Background()

	paymentReq, err := f.fakes.PaymentRequest().GetByProductTransactionID(ctx, f.tx.UUID)
	require.NoError(t, err)
	paymentReq.PaymentChannel = "VA_BCA"
	paymentReq.PaymentCode = "8888800012345678"
	paymentReq.GatewayTransactionID = ""
	require.NoError(t, f.fakes.PaymentRequest().Update(ctx, paymentReq))

	result, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckNotSettled, result.Outcome)
	assert.False(t, result.Gateway.Found)
	assert.Equal(t, "VA_BCA", result.Gateway.PaymentChannel)
	assert.Equal(t, "VA_BCA", result.PaymentChannel)
}

func TestCheckTransactionSettlement_UnknownTransaction(t *testing.T) {
	f := newCheckFixture(t, &qrisGateway{fakeGateway: &fakeGateway{}}, domain.TransactionStatusCompleted)

	_, err := f.client.CheckTransactionSettlement(context.Background(), "no-such-transaction")

	require.Error(t, err)
	assert.Equal(t, ledgererr.CodeProductTransactionNotFound, outerCode(t, err),
		"the code a consumer switches on is the outer one")
}

func TestCheckTransactionSettlement_SellerWithoutSubAccountIsRefusedBeforeSingapay(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	f.seller.SingapayAccountID = ""

	_, err := f.client.CheckTransactionSettlement(context.Background(), f.tx.UUID)

	require.Error(t, err)
	assert.Equal(t, ledgererr.CodeInvalidRequest, outerCode(t, err))
	assert.Equal(t, 0, gw.calls)
}

// The pass's count must not change now that bookSettlement tells a lost race apart: a
// transaction another caller settled first is still one the pass counts as settled.
func TestProcessSettlementNotifications_CountsALostRaceAsSettled(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	ctx := context.Background()

	// Old enough that the pass runs on age alone.
	stored, err := f.fakes.ProductTransaction().GetByID(ctx, f.tx.UUID)
	require.NoError(t, err)
	old := time.Now().Add(-48 * time.Hour)
	f.fakes.productTransactionRepo.transactions[stored.UUID].CompletedAt = &old

	f.fakes.productTransactionRepo.beforeCAS = func() {
		_, err := f.fakes.productTransactionRepo.updateStatusIf(f.tx.UUID,
			domain.TransactionStatusCompleted, domain.TransactionStatusSettled, time.Now())
		require.NoError(t, err)
	}

	result, err := f.client.ProcessSettlementNotifications(ctx, 10)

	require.NoError(t, err)
	assert.True(t, result.Triggered)
	assert.Equal(t, 1, result.Examined)
	assert.Equal(t, 1, result.Settled)
	assert.Equal(t, 0, result.Failed)
}

func TestGetTransactionDetail_ReturnsEverythingBookedAgainstTheTransaction(t *testing.T) {
	gw := &qrisGateway{fakeGateway: &fakeGateway{}, qr: settledQRIS(true)}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	ctx := context.Background()

	_, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)
	require.NoError(t, err)
	assert.Equal(t, 1, gw.calls)

	detail, err := f.client.GetTransactionDetail(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, 1, gw.calls, "reading the detail asks Singapay nothing")
	assert.Equal(t, f.tx.UUID, detail.Transaction.UUID)
	assert.Equal(t, domain.TransactionStatusSettled, detail.Transaction.Status)
	require.NotNil(t, detail.PaymentRequest)
	assert.Equal(t, "524493", detail.PaymentRequest.GatewayTransactionID)

	require.Len(t, detail.Journals, 2)
	assert.Equal(t, domain.EventTypePaymentSuccess, detail.Journals[0].EventType, "oldest first")
	assert.Equal(t, domain.EventTypeSettlement, detail.Journals[1].EventType)
	assert.Len(t, detail.Entries, 3+5, "three payment entries, five settlement entries")

	for _, entry := range detail.Entries {
		_, ok := detail.Accounts[entry.AccountUUID]
		assert.True(t, ok, "every account an entry names is resolved")
	}
	assert.Equal(t, domain.OwnerTypeSeller, detail.Accounts[f.seller.UUID].OwnerType)
}

func TestGetTransactionDetail_UnknownTransaction(t *testing.T) {
	f := newCheckFixture(t, &qrisGateway{fakeGateway: &fakeGateway{}}, domain.TransactionStatusCompleted)

	_, err := f.client.GetTransactionDetail(context.Background(), "no-such-transaction")

	require.Error(t, err)
	assert.Equal(t, ledgererr.CodeProductTransactionNotFound, outerCode(t, err),
		"the code a consumer switches on is the outer one")
}

// The stuck card payment, end to end: a stored "0" no longer sends the check to a 404,
// and the settlement is booked from the history found by invoice.
func TestCheckTransactionSettlement_CardWithAStoredZeroIdSettles(t *testing.T) {
	gw := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows: []singapay.PaymentLinkHistory{{
			ID: 77, PaymentLinkReffNo: "INV-CHECK-1", Amount: singapay.NewAmount(24169, "IDR"), HasSettle: true, Status: "paid",
		}},
	}
	f := newCheckFixture(t, gw, domain.TransactionStatusCompleted)
	ctx := context.Background()
	paymentReq, err := f.fakes.PaymentRequest().GetByProductTransactionID(ctx, f.tx.UUID)
	require.NoError(t, err)
	paymentReq.PaymentChannel = ChannelCreditCard
	paymentReq.GatewayTransactionID = "0"
	require.NoError(t, f.fakes.PaymentRequest().Update(ctx, paymentReq))

	result, err := f.client.CheckTransactionSettlement(ctx, f.tx.UUID)

	require.NoError(t, err)
	assert.Equal(t, SettlementCheckSettled, result.Outcome)
	assert.Equal(t, domain.TransactionStatusSettled, result.Status)
	assert.Equal(t, "77", result.Gateway.GatewayTransactionID)
	assert.False(t, result.Gateway.FeeReported, "a card reports no fee; the priced one stands")
	assert.Equal(t, int64(16900), result.Gateway.FeeMinor)

	_, available, err := f.fakes.LedgerEntry().GetAllBalances(ctx, f.seller.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(20000), available)
}

// A settlement the old DOKU batch reconciler booked sits under a SETTLEMENT_BATCH journal,
// whose source is the batch, not the transaction. Its entries still name the transaction,
// and the detail shows them under the journal that booked them.
func TestGetTransactionDetail_FindsABatchJournalThroughItsEntries(t *testing.T) {
	f := newCheckFixture(t, &qrisGateway{fakeGateway: &fakeGateway{}}, domain.TransactionStatusCompleted)
	ctx := context.Background()

	batch := domain.NewJournal(domain.EventTypeSettlement, domain.SourceTypeSettlementBatch, "batch-uuid", nil)
	require.NoError(t, f.fakes.Journal().Save(ctx, batch))
	require.NoError(t, f.fakes.LedgerEntry().SaveBatch(ctx,
		domain.NewSettlementEntriesForAccount(batch.UUID, f.tx.UUID, f.seller.UUID, 20000)))

	detail, err := f.client.GetTransactionDetail(ctx, f.tx.UUID)

	require.NoError(t, err)
	require.Len(t, detail.Journals, 2)
	assert.Equal(t, domain.EventTypePaymentSuccess, detail.Journals[0].EventType)
	assert.Equal(t, batch.UUID, detail.Journals[1].UUID)
	assert.Equal(t, domain.SourceTypeSettlementBatch, detail.Journals[1].SourceType)
}
