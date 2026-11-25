package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixture: the platform, one seller, and a Singapay that keeps both sub-accounts' balances
// ─────────────────────────────────────────────────────────────────────────────
//
// Sales are booked the way the money cycle books them — payment entries, then the real
// bookSettlement — and swept by the real ProcessPlatformFeeTransfer. Beside the ledger the
// fixture keeps what each sub-account holds at Singapay, in sen, so S in the equation is the
// money the sweep really moved.

const (
	reconPlatformULID   = "01M2DCNBH0Y3CS41H0MM2SQGND"
	reconPlatformNumber = "537010748513"
	reconSellerULID     = "01M2RECONSELLER00000000000"
)

type platformReconFixture struct {
	t      *testing.T
	ctx    context.Context
	fakes  *FakeRepositoryProvider
	gw     *fakeGateway
	client *LedgerClient
	fees   *domain.FeeCalculator

	platform *domain.Account
	gateway  *domain.Account
	seller   *domain.Account

	// singapay is each sub-account's available balance at Singapay, in sen.
	singapay map[string]int64
	// balanceErr fails the balance inquiry; failTransfers fails the transfer of these refs.
	balanceErr    error
	failTransfers map[string]bool
	balanceCalls  int

	sales int
	now   time.Time
}

func newPlatformReconFixture(t *testing.T) *platformReconFixture {
	t.Helper()
	ctx := context.Background()
	fakes := NewFakeRepositoryProvider()

	platform := domain.NewPlatformAccount(reconPlatformULID, "platform", domain.CurrencyIDR)
	platform.SetSingapayAccount(reconPlatformULID, reconPlatformNumber)
	require.NoError(t, fakes.Account().Save(ctx, &platform))

	gatewayAccount := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
	require.NoError(t, fakes.Account().Save(ctx, &gatewayAccount))

	seller := domain.NewSellerAccount(reconSellerULID, "seller-1", domain.CurrencyIDR)
	seller.SetSingapayAccount(reconSellerULID, "000000000777")
	require.NoError(t, fakes.Account().Save(ctx, &seller))

	f := &platformReconFixture{
		t:             t,
		ctx:           ctx,
		fakes:         fakes,
		platform:      &platform,
		gateway:       &gatewayAccount,
		seller:        &seller,
		singapay:      map[string]int64{},
		failTransfers: map[string]bool{},
		now:           time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC),
		fees: domain.NewFeeCalculator([]*domain.FeeConfig{
			{ConfigType: domain.FeeConfigTypePlatform, PaymentChannel: "PLATFORM", FeeType: domain.FeeTypeFixed, FixedAmount: 4000, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: ChannelCreditCard, FeeType: domain.FeeTypeHybrid, FixedAmount: 2000, Percentage: 3, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "QRIS", FeeType: domain.FeeTypePercentage, Percentage: 0.7, IsActive: true},
		}),
	}

	f.gw = &fakeGateway{
		balance: func(accountID string) (*singapay.Balance, error) {
			f.balanceCalls++
			if f.balanceErr != nil {
				return nil, f.balanceErr
			}
			available, ok := f.singapay[accountID]
			if !ok && accountID != reconPlatformULID {
				return nil, fmt.Errorf("fixture knows no sub-account %s", accountID)
			}
			return &singapay.Balance{
				Available: singapay.NewAmountFromMinor(available, "IDR"),
				Pending:   singapay.NewAmountFromMinor(0, "IDR"),
				Held:      singapay.NewAmountFromMinor(0, "IDR"),
				Total:     singapay.NewAmountFromMinor(available, "IDR"),
				AccountID: accountID,
			}, nil
		},
		transfer: func(from string, req singapay.TransferRequest) (*singapay.AccountTransfer, error) {
			if f.failTransfers[req.MerchantRefNo] {
				return nil, &singapay.Error{StatusCode: http.StatusInternalServerError, Message: "fixture: transfer refused"}
			}
			if req.BeneficiaryAccountNumber != reconPlatformNumber {
				return nil, fmt.Errorf("fixture knows no beneficiary %s", req.BeneficiaryAccountNumber)
			}
			if req.Amount.Minor() > f.singapay[from] {
				return nil, &singapay.Error{StatusCode: http.StatusUnprocessableEntity, Message: "Insufficient remitter account balance"}
			}
			f.singapay[from] -= req.Amount.Minor()
			f.singapay[reconPlatformULID] += req.Amount.Minor()
			return &singapay.AccountTransfer{Status: "success", MerchantRefNo: req.MerchantRefNo, Amount: req.Amount}, nil
		},
	}

	f.client = &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		logger:       testLogger(),
		gateway:      f.gw,
	}

	previous := platformReconciliationClock
	platformReconciliationClock = func() time.Time { return f.now }
	t.Cleanup(func() { platformReconciliationClock = previous })
	return f
}

// sale books one paid, settled QRIS sale whose platform fee settles at 4000 rupiah plus
// residualMinor sen (negative: below 4000), and credits the seller's sub-account with what
// Singapay leaves there.
func (f *platformReconFixture) sale(invoice string, residualMinor int64) *domain.ProductTransaction {
	f.t.Helper()
	f.sales++

	fee := f.fees.GetFeeBreakdown(100000, "QRIS", domain.CurrencyIDR)
	tx := domain.NewProductTransaction("buyer-"+invoice, f.seller.UUID, "product-"+invoice, "BOOKING", invoice, fee, nil)
	tx.Status = domain.TransactionStatusCompleted
	completedAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.sales) * time.Minute)
	tx.CompletedAt = &completedAt
	require.NoError(f.t, f.fakes.ProductTransaction().Save(f.ctx, tx))

	payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(f.t, f.fakes.Journal().Save(f.ctx, payment))
	require.NoError(f.t, f.fakes.LedgerEntry().SaveBatch(f.ctx, domain.NewPaymentEntries(
		payment.UUID, tx.UUID, f.seller.UUID, fee.SellerNetAmount, f.platform.UUID, fee.PlatformFee, f.gateway.UUID, fee.GatewayFee)))

	// The platform fee settles at PlatformFee×100 − (actual − estimated): Singapay taking
	// residualMinor sen less than estimated leaves them to the platform.
	gross := domain.RupiahToMinor(fee.TotalCharged)
	actualFee := domain.RupiahToMinor(fee.GatewayFee) - residualMinor
	settled := domain.SettledTransaction{
		MerchantReference:    invoice,
		GatewayTransactionID: "gw-" + invoice,
		GatewayAccountID:     f.seller.SingapayAccountID,
		PaymentChannel:       "QRIS",
		GrossMinor:           gross,
		FeeMinor:             actualFee,
		NetMinor:             gross - actualFee,
		FeeReported:          true,
	}
	outcome, err := f.client.bookSettlement(f.ctx, tx, settled)
	require.NoError(f.t, err)
	require.Equal(f.t, settleOutcomeSettled, outcome)

	stored := f.fakes.productTransactionRepo.transactions[tx.UUID]
	settledAt := completedAt.Add(24 * time.Hour)
	stored.SettledAt = &settledAt

	f.singapay[reconSellerULID] += gross - actualFee
	return stored
}

// card books a card sale, settled on the checkout estimate (ESTIMATE): the sweep waits for the
// card fee recomputation, which this fixture never runs — a held round.
func (f *platformReconFixture) card(invoice string) *domain.ProductTransaction {
	f.t.Helper()
	f.sales++

	fee := f.fees.GetFeeBreakdown(150000, ChannelCreditCard, domain.CurrencyIDR)
	tx := domain.NewProductTransaction("buyer-"+invoice, f.seller.UUID, "product-"+invoice, "BOOKING", invoice, fee, nil)
	tx.Status = domain.TransactionStatusCompleted
	completedAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.sales) * time.Minute)
	tx.CompletedAt = &completedAt
	require.NoError(f.t, f.fakes.ProductTransaction().Save(f.ctx, tx))

	payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(f.t, f.fakes.Journal().Save(f.ctx, payment))
	require.NoError(f.t, f.fakes.LedgerEntry().SaveBatch(f.ctx, domain.NewPaymentEntries(
		payment.UUID, tx.UUID, f.seller.UUID, fee.SellerNetAmount, f.platform.UUID, fee.PlatformFee, f.gateway.UUID, fee.GatewayFee)))

	gross := domain.RupiahToMinor(fee.TotalCharged)
	estimate := domain.RupiahToMinor(fee.GatewayFee)
	outcome, err := f.client.bookSettlement(f.ctx, tx, domain.SettledTransaction{
		MerchantReference: invoice, GatewayTransactionID: "gw-" + invoice, GatewayAccountID: f.seller.SingapayAccountID,
		PaymentChannel: ChannelCreditCard, GrossMinor: gross, FeeMinor: estimate, NetMinor: gross - estimate, FeeReported: false,
	})
	require.NoError(f.t, err)
	require.Equal(f.t, settleOutcomeSettled, outcome)
	require.Equal(f.t, domain.SettledFeeSourceEstimate, f.fakes.productTransactionRepo.feeSources[tx.UUID])

	// Singapay really took 37 sen more than the estimate: the card's platform share is
	// 3999.63, not 4000. Nothing moves until a recomputation says so.
	f.singapay[reconSellerULID] += gross - estimate - 37
	return f.fakes.productTransactionRepo.transactions[tx.UUID]
}

func (f *platformReconFixture) sweep() *PlatformFeeTransferResult {
	f.t.Helper()
	result, err := f.client.ProcessPlatformFeeTransfer(f.ctx, 50)
	require.NoError(f.t, err)
	return result
}

func (f *platformReconFixture) check() *domain.PlatformReconciliation {
	f.t.Helper()
	rec, err := f.client.CheckPlatformBalance(f.ctx)
	require.NoError(f.t, err)
	return rec
}

func (f *platformReconFixture) round() *PlatformResidualBooking {
	f.t.Helper()
	booking, err := f.client.BookPlatformResidual(f.ctx)
	require.NoError(f.t, err)
	return booking
}

// ledgerAvailable is the platform's AVAILABLE balance, summed from its entries.
func (f *platformReconFixture) ledgerAvailable(account *domain.Account) int64 {
	f.t.Helper()
	_, available, err := f.fakes.LedgerEntry().GetAllBalances(f.ctx, account.UUID)
	require.NoError(f.t, err)
	return available
}

func (f *platformReconFixture) pool() int64 {
	f.t.Helper()
	snapshot, err := f.fakes.PlatformReconciliation().ReadBalanceSnapshot(f.ctx, f.platform.UUID)
	require.NoError(f.t, err)
	return snapshot.PoolMinor()
}

func (f *platformReconFixture) book(rec *domain.PlatformReconciliation, reason domain.PlatformAdjustmentReason, note string) (*PlatformDifferenceBooking, error) {
	return f.client.BookPlatformDifference(f.ctx, PlatformDifferenceRequest{
		ReconciliationUUID: rec.UUID, Reason: reason, Note: note, AdminID: "admin-1", AdminName: "Admin Satu",
	})
}

// platformPayout reserves a platform payout: the ledger debit is written, Singapay has not
// debited yet.
func (f *platformReconFixture) platformPayout(amount int64) *domain.Disbursement {
	f.t.Helper()
	d, err := domain.NewDisbursement(f.platform.UUID, amount, domain.CurrencyIDR,
		domain.BankAccount{BankCode: "CENAIDJA", AccountNumber: "1234567890", AccountName: "PT Atur Jadwal Digital"}, "platform payout")
	require.NoError(f.t, err)
	require.NoError(f.t, f.fakes.Disbursement().Save(f.ctx, d))
	journal := domain.NewJournal(domain.EventTypeDisbursement, domain.SourceTypeDisbursement, d.UUID, nil)
	require.NoError(f.t, f.fakes.Journal().Save(f.ctx, journal))
	require.NoError(f.t, f.fakes.LedgerEntry().Save(f.ctx, domain.NewDisbursementEntry(journal.UUID, d.UUID, f.platform.UUID, amount)))
	return d
}

// ─────────────────────────────────────────────────────────────────────────────
// The equation
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckPlatformBalance_SweptResidualIsInThePool(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 16)  // 4000.16
	f.sale("INV-B", -44) // 3999.56
	f.sale("INV-C", 6)   // 4000.06
	sweep := f.sweep()
	require.Equal(t, 3, sweep.Succeeded)

	// Singapay holds the three fees to the sen, the ledger their whole rupiah.
	assert.Equal(t, int64(400016+399956+400006), f.singapay[reconPlatformULID])
	assert.Equal(t, int64(4000+3999+4000), f.ledgerAvailable(f.platform))

	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status)
	assert.Equal(t, int64(16+56+6), rec.ResidualPoolMinor, "K = Σ platform_residual of the swept rows")
	assert.Equal(t, int64(0), rec.UnsweptPlatformFeeMinor)
	assert.Equal(t, int64(0), rec.Unexplained())
	assert.Equal(t, f.singapay[reconPlatformULID], *rec.SingapayAvailableMinor)
	assert.Equal(t, domain.RupiahToMinor(4000+3999+4000), rec.LedgerAvailableMinor)
	assert.Equal(t, f.now, rec.CheckedAt)
	assert.Nil(t, rec.EmailedAt)
}

func TestCheckPlatformBalance_UnsweptFeeWithSenIsNoAlert(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 16)
	failing := f.sale("INV-B", 72) // 4000.72, its sweep fails
	f.failTransfers[platformFeeTransferReference(failing.InvoiceNumber)] = true

	sweep := f.sweep()
	require.Equal(t, 1, sweep.Succeeded)
	require.Equal(t, 1, sweep.Failed)

	// The ledger holds 4000 for the unswept fee, Singapay nothing yet, and its 72 sen are in
	// neither S nor K. B is the ledger's whole rupiah, so the check is exactly 0.
	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status, "unexplained %d", rec.Unexplained())
	assert.Equal(t, domain.RupiahToMinor(4000), rec.UnsweptPlatformFeeMinor, "B is the whole rupiah the ledger holds, not 4000.72")
	assert.Equal(t, 1, rec.UnsweptCount)
	assert.Equal(t, int64(16), rec.ResidualPoolMinor, "the unswept row's sen are not in the pool yet")

	// The next sweep moves it; still 0, the 72 sen now in the pool.
	delete(f.failTransfers, platformFeeTransferReference(failing.InvoiceNumber))
	require.Equal(t, 1, f.sweep().Succeeded)
	rec = f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status, "unexplained %d", rec.Unexplained())
	assert.Equal(t, int64(0), rec.UnsweptPlatformFeeMinor)
	assert.Equal(t, int64(16+72), rec.ResidualPoolMinor)
}

func TestCheckPlatformBalance_HeldCardFeeIsNoAlert(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 16)
	f.card("INV-CARD") // on the estimate, its recomputation held: the sweep leaves it
	require.Equal(t, 1, f.sweep().Succeeded)

	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status, "unexplained %d", rec.Unexplained())
	assert.Equal(t, domain.RupiahToMinor(4000), rec.UnsweptPlatformFeeMinor)
}

func TestCheckPlatformBalance_UnexplainedMoneyIsAMismatch(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 16)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] += 400016 // CSV7MW: a staging sweep into the same sub-account

	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationMismatch, rec.Status)
	assert.Equal(t, int64(400016), rec.Unexplained())
}

func TestCheckPlatformBalance_SkipsWhileAPlatformPayoutIsInFlight(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 16)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.platformPayout(2000)

	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationSkipped, rec.Status)
	assert.Equal(t, domain.SkipPayoutInFlight, rec.SkipReason)
	assert.Nil(t, rec.UnexplainedMinor)
	assert.Equal(t, 1, rec.PayoutsInFlight)
	assert.Equal(t, 0, f.balanceCalls, "no balance is read while a payout is in flight")
}

func TestCheckPlatformBalance_SkipsWhenTheBalanceCannotBeRead(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.balanceErr = errors.New("singapay: 503")

	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationSkipped, rec.Status)
	assert.Equal(t, domain.SkipBalanceReadFailed, rec.SkipReason)
	assert.Contains(t, rec.SkipDetail, "503")
	assert.Nil(t, rec.SingapayAvailableMinor)
}

func TestCheckPlatformBalance_ReadsAgainWhenTheLedgerMoves(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 16)
	require.Equal(t, 1, f.sweep().Succeeded)

	// The ledger moves once, between the first two reads: the balance is read again.
	repo := f.fakes.platformReconciliationRepo
	repo.beforeSnapshot = func(call int) {
		if call == 2 {
			f.sale("INV-B", 3) // settled, not swept: L and B move by the same rupiah
		}
	}
	rec := f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status, "unexplained %d", rec.Unexplained())
	assert.Equal(t, 2, f.balanceCalls)

	// It moves on every read: skipped after three.
	f.balanceCalls = 0
	repo.snapshots = 0
	n := 0
	repo.beforeSnapshot = func(call int) {
		if call > 1 {
			n++
			f.sale(fmt.Sprintf("INV-M%d", n), 1)
		}
	}
	rec = f.check()
	assert.Equal(t, domain.PlatformReconciliationSkipped, rec.Status)
	assert.Equal(t, domain.SkipLedgerMoved, rec.SkipReason)
	assert.Equal(t, platformBalanceReadAttempts, f.balanceCalls)
	assert.NotNil(t, rec.SingapayAvailableMinor, "the last balance read is kept")
	assert.Nil(t, rec.UnexplainedMinor)
}

// ─────────────────────────────────────────────────────────────────────────────
// The cent rounding
// ─────────────────────────────────────────────────────────────────────────────

func TestBookPlatformResidual_BooksWholeRupiahAndCarriesTheSen(t *testing.T) {
	f := newPlatformReconFixture(t)
	a := f.sale("INV-A", 72)
	b := f.sale("INV-B", 90)
	f.sale("INV-Z", 0) // a whole-rupiah fee: no sen, not a source
	c := f.sale("INV-C", 16)
	require.Equal(t, 4, f.sweep().Succeeded)
	require.Equal(t, int64(72+90+16), f.pool())
	before := f.ledgerAvailable(f.platform)

	booking := f.round()
	require.True(t, booking.Booked)
	assert.Equal(t, int64(178), booking.PoolBeforeMinor)
	assert.Equal(t, int64(78), booking.PoolAfterMinor, "under a rupiah is carried")
	assert.Equal(t, int64(1), booking.Adjustment.Amount)
	assert.Equal(t, domain.PlatformAdjustmentRounding, booking.Adjustment.Kind)
	assert.ElementsMatch(t, []string{a.UUID, b.UUID, c.UUID}, booking.Adjustment.SourceTransactionUUIDs)
	assert.Equal(t, 3, booking.Sources)
	assert.Equal(t, before+1, f.ledgerAvailable(f.platform))
	assert.Equal(t, int64(78), f.pool())
	assert.Contains(t, f.fakes.accountRepo.lockedForUpdate, f.platform.UUID, "the rounding takes the platform's row lock")

	journal := f.fakes.journalRepo.journals[booking.Adjustment.JournalUUID]
	require.NotNil(t, journal)
	assert.Equal(t, domain.EventTypeReconciliation, journal.EventType)
	assert.Equal(t, domain.SourceTypeManualAdjustment, journal.SourceType)
	assert.Equal(t, booking.Adjustment.UUID, journal.SourceID)

	// The check agrees after the rounding: L and K moved by the same rupiah.
	assert.Equal(t, domain.PlatformReconciliationMatched, f.check().Status)

	// Nothing more to book: the pool is under a rupiah. A second run — another process after
	// the first — writes nothing.
	again := f.round()
	assert.False(t, again.Booked)
	assert.Equal(t, int64(78), again.PoolBeforeMinor)

	// The next sweep's 30 sen make a rupiah again; that rounding names only the new row.
	d := f.sale("INV-D", 30)
	require.Equal(t, 1, f.sweep().Succeeded)
	next := f.round()
	require.True(t, next.Booked)
	assert.Equal(t, int64(1), next.Adjustment.Amount)
	assert.Equal(t, int64(8), next.PoolAfterMinor)
	assert.Equal(t, []string{d.UUID}, next.Adjustment.SourceTransactionUUIDs)
	assert.Equal(t, domain.PlatformReconciliationMatched, f.check().Status)
}

func TestBookPlatformResidual_UnderARupiahBooksNothing(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 99)
	require.Equal(t, 1, f.sweep().Succeeded)

	booking := f.round()
	assert.False(t, booking.Booked)
	assert.Nil(t, booking.Adjustment)
	assert.Equal(t, int64(99), booking.PoolAfterMinor)
	assert.Empty(t, f.fakes.platformReconciliationRepo.adjustments)
}

func TestBookPlatformResidual_UnsweptSenWait(t *testing.T) {
	f := newPlatformReconFixture(t)
	failing := f.sale("INV-A", 95)
	f.failTransfers[platformFeeTransferReference(failing.InvoiceNumber)] = true
	f.sale("INV-B", 10)
	f.sweep()

	// Only the swept row's 10 sen are in the pool: 95 sen still in the seller's sub-account
	// are not the platform's money at Singapay yet.
	booking := f.round()
	assert.False(t, booking.Booked)
	assert.Equal(t, int64(10), booking.PoolBeforeMinor)
}

// ─────────────────────────────────────────────────────────────────────────────
// Booking a difference
// ─────────────────────────────────────────────────────────────────────────────

func TestBookPlatformDifference_BooksRupiahAndPoolsTheSen(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 28)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] += 400016 // CSV7MW
	before := f.ledgerAvailable(f.platform)

	rec := f.check()
	require.Equal(t, int64(400016), rec.Unexplained())

	booking, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "  Dana uji coba staging, PF-INV-20260911084626-CSV7MW  ")
	require.NoError(t, err)
	adj := booking.Adjustment
	assert.Equal(t, domain.PlatformAdjustmentDifference, adj.Kind)
	assert.Equal(t, int64(4000), adj.Amount)
	assert.Equal(t, int64(16), adj.ResidualMinor)
	assert.Equal(t, "Dana uji coba staging, PF-INV-20260911084626-CSV7MW", adj.Note, "the note is trimmed")
	assert.Equal(t, "admin-1", adj.AdminID)
	assert.Equal(t, "Admin Satu", adj.AdminName)
	assert.Equal(t, rec.UUID, adj.ReconciliationUUID)
	assert.Equal(t, before+4000, f.ledgerAvailable(f.platform))
	assert.Equal(t, int64(28+16), f.pool())

	journal := f.fakes.journalRepo.journals[adj.JournalUUID]
	require.NotNil(t, journal)
	assert.Equal(t, domain.EventTypeManualAdjustment, journal.EventType)

	// Checked again: 0, the 44 sen in the pool explaining the rest.
	after := f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, after.Status, "unexplained %d", after.Unexplained())
	assert.Equal(t, int64(44), after.ResidualPoolMinor)
	assert.Equal(t, f.singapay[reconPlatformULID], domain.RupiahToMinor(f.ledgerAvailable(f.platform))+44)
}

func TestBookPlatformDifference_NegativeIsRoundedDown(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 0)
	f.sale("INV-B", 0)
	require.Equal(t, 2, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] -= 4130 // Rp41,30 left the sub-account outside the system
	before := f.ledgerAvailable(f.platform)

	rec := f.check()
	require.Equal(t, domain.PlatformReconciliationMismatch, rec.Status)
	require.Equal(t, int64(-4130), rec.Unexplained())

	booking, err := f.book(rec, domain.PlatformAdjustmentReasonOffSystemWithdrawal, "")
	require.NoError(t, err)
	assert.Equal(t, int64(-42), booking.Adjustment.Amount, "rounded down: the ledger never claims more than Singapay holds")
	assert.Equal(t, int64(70), booking.Adjustment.ResidualMinor)
	assert.Equal(t, before-42, f.ledgerAvailable(f.platform))
	assert.LessOrEqual(t, domain.RupiahToMinor(f.ledgerAvailable(f.platform)), f.singapay[reconPlatformULID])

	after := f.check()
	assert.Equal(t, domain.PlatformReconciliationMatched, after.Status, "unexplained %d", after.Unexplained())
}

func TestBookPlatformDifference_UnderARupiahOnlyPoolsTheSen(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.sale("INV-A", 0)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] += 44
	before := f.ledgerAvailable(f.platform)
	journals := len(f.fakes.journalRepo.journals)

	rec := f.check()
	booking, err := f.book(rec, domain.PlatformAdjustmentReasonSingapayCorrection, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), booking.Adjustment.Amount)
	assert.Equal(t, int64(44), booking.Adjustment.ResidualMinor)
	assert.Empty(t, booking.Adjustment.JournalUUID, "no rupiah moved, so no journal and no entry")
	assert.Equal(t, journals, len(f.fakes.journalRepo.journals))
	assert.Equal(t, before, f.ledgerAvailable(f.platform))
	assert.Equal(t, domain.PlatformReconciliationMatched, f.check().Status)
}

func TestBookPlatformDifference_Guards(t *testing.T) {
	mismatch := func(t *testing.T) (*platformReconFixture, *domain.PlatformReconciliation) {
		f := newPlatformReconFixture(t)
		f.sale("INV-A", 0)
		require.Equal(t, 1, f.sweep().Succeeded)
		f.singapay[reconPlatformULID] += 400016
		rec := f.check()
		require.Equal(t, domain.PlatformReconciliationMismatch, rec.Status)
		return f, rec
	}
	assertNothingWritten := func(t *testing.T, f *platformReconFixture, ledgerBefore int64) {
		t.Helper()
		assert.Empty(t, f.fakes.platformReconciliationRepo.adjustments)
		assert.Equal(t, ledgerBefore, f.ledgerAvailable(f.platform))
	}

	t.Run("no check yet", func(t *testing.T) {
		f := newPlatformReconFixture(t)
		_, err := f.client.BookPlatformDifference(f.ctx, PlatformDifferenceRequest{
			ReconciliationUUID: "nope", Reason: domain.PlatformAdjustmentReasonTestFunds, AdminID: "admin-1"})
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationNotFound), "%v", err)
	})

	t.Run("a check older than ten minutes", func(t *testing.T) {
		f, rec := mismatch(t)
		before := f.ledgerAvailable(f.platform)
		f.now = rec.CheckedAt.Add(PlatformDifferenceMaxCheckAge + time.Second)
		_, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationStale), "%v", err)
		assertNothingWritten(t, f, before)

		// Exactly ten minutes is still in time.
		f.now = rec.CheckedAt.Add(PlatformDifferenceMaxCheckAge)
		_, err = f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.NoError(t, err)
	})

	t.Run("a check that found nothing", func(t *testing.T) {
		f := newPlatformReconFixture(t)
		f.sale("INV-A", 0)
		require.Equal(t, 1, f.sweep().Succeeded)
		rec := f.check()
		require.Equal(t, domain.PlatformReconciliationMatched, rec.Status)
		before := f.ledgerAvailable(f.platform)
		_, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationNothingToBook), "%v", err)
		assertNothingWritten(t, f, before)
	})

	t.Run("a skipped check", func(t *testing.T) {
		f := newPlatformReconFixture(t)
		f.balanceErr = errors.New("down")
		rec := f.check()
		_, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationNothingToBook), "%v", err)
	})

	t.Run("not the latest check", func(t *testing.T) {
		f, first := mismatch(t)
		f.now = f.now.Add(time.Minute)
		f.check()
		before := f.ledgerAvailable(f.platform)
		_, err := f.book(first, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationNotLatest), "%v", err)
		assertNothingWritten(t, f, before)
	})

	t.Run("twice: the second is refused", func(t *testing.T) {
		f, rec := mismatch(t)
		before := f.ledgerAvailable(f.platform)
		_, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		require.NoError(t, err)
		_, err = f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationAlreadyBooked), "%v", err)
		assert.Len(t, f.fakes.platformReconciliationRepo.adjustments, 1)
		assert.Equal(t, before+4000, f.ledgerAvailable(f.platform), "booked once")
	})

	t.Run("a payout started since the check", func(t *testing.T) {
		f, rec := mismatch(t)
		f.platformPayout(1000)
		before := f.ledgerAvailable(f.platform)
		_, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationPayoutInFlight), "%v", err)
		assertNothingWritten(t, f, before)
	})

	t.Run("the request", func(t *testing.T) {
		f, rec := mismatch(t)
		before := f.ledgerAvailable(f.platform)
		for name, req := range map[string]PlatformDifferenceRequest{
			"no reason":          {ReconciliationUUID: rec.UUID, AdminID: "admin-1"},
			"an unknown reason":  {ReconciliationUUID: rec.UUID, Reason: "BECAUSE", AdminID: "admin-1"},
			"other with no note": {ReconciliationUUID: rec.UUID, Reason: domain.PlatformAdjustmentReasonOther, Note: "   ", AdminID: "admin-1"},
			"a note too long":    {ReconciliationUUID: rec.UUID, Reason: domain.PlatformAdjustmentReasonTestFunds, Note: strings.Repeat("x", 501), AdminID: "admin-1"},
			"no admin":           {ReconciliationUUID: rec.UUID, Reason: domain.PlatformAdjustmentReasonTestFunds},
		} {
			_, err := f.client.BookPlatformDifference(f.ctx, req)
			assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeInvalidPlatformAdjustment, err), "%s: %v", name, err)
		}
		assertNothingWritten(t, f, before)

		// OTHER with a note is fine.
		_, err := f.client.BookPlatformDifference(f.ctx, PlatformDifferenceRequest{
			ReconciliationUUID: rec.UUID, Reason: domain.PlatformAdjustmentReasonOther, Note: "Koreksi manual", AdminID: "admin-1"})
		assert.NoError(t, err)
	})
}

func TestGetPlatformReconciliation_State(t *testing.T) {
	f := newPlatformReconFixture(t)
	state, err := f.client.GetPlatformReconciliation(f.ctx)
	require.NoError(t, err)
	assert.Nil(t, state.Latest)
	assert.False(t, state.Bookable)

	f.sale("INV-A", 0)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] += 400016
	rec := f.check()

	state, err = f.client.GetPlatformReconciliation(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, state.Latest)
	assert.Equal(t, rec.UUID, state.Latest.UUID)
	assert.True(t, state.Bookable)
	assert.Equal(t, rec.CheckedAt.Add(PlatformDifferenceMaxCheckAge), *state.BookableUntil)
	assert.Nil(t, state.LastEmailedAt)

	emailedAt := f.now.Add(time.Second)
	require.NoError(t, f.client.MarkPlatformReconciliationEmailed(f.ctx, rec.UUID, emailedAt))
	err = f.client.MarkPlatformReconciliationEmailed(f.ctx, "nope", emailedAt)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationNotFound), "%v", err)

	f.now = rec.CheckedAt.Add(PlatformDifferenceMaxCheckAge + time.Minute)
	state, err = f.client.GetPlatformReconciliation(f.ctx)
	require.NoError(t, err)
	assert.False(t, state.Bookable, "too old to book")
	require.NotNil(t, state.LastEmailedAt)
	assert.Equal(t, emailedAt, *state.LastEmailedAt)

	f.now = rec.CheckedAt.Add(time.Minute)
	_, err = f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "")
	require.NoError(t, err)
	state, err = f.client.GetPlatformReconciliation(f.ctx)
	require.NoError(t, err)
	assert.False(t, state.Bookable, "already booked")
	require.NotNil(t, state.Booking)
	assert.Equal(t, int64(4000), state.Booking.Amount)
}

// ─────────────────────────────────────────────────────────────────────────────
// The DOKU era
// ─────────────────────────────────────────────────────────────────────────────

// dokuEra books what the production DOKU era left in the ledger: seven platform fees of 1000
// on sellers' sales and the platform's own subscription YTWGBR at 38.727, all settled before
// the ledger recorded settled fees (settled_platform_fee NULL), the fees swept.
func (f *platformReconFixture) dokuEra() []*domain.ProductTransaction {
	f.t.Helper()
	var txs []*domain.ProductTransaction
	book := func(invoice string, sellerAccountID string, productType string, available int64, swept bool) {
		f.sales++
		fee := domain.FeeBreakdown{SellerPrice: 50000, PlatformFee: 1000, GatewayFee: 350, TotalCharged: 51350,
			SellerNetAmount: 50000, FeeModel: domain.FeeModelGatewayOnCustomer, Currency: domain.CurrencyIDR}
		if sellerAccountID == f.platform.UUID {
			fee = domain.FeeBreakdown{SellerPrice: 39000, TotalCharged: 39000, SellerNetAmount: 39000,
				FeeModel: domain.FeeModelGatewayOnSeller, Currency: domain.CurrencyIDR}
		}
		tx := domain.NewProductTransaction("buyer-"+invoice, sellerAccountID, "product-"+invoice, productType, invoice, fee, nil)
		tx.Status = domain.TransactionStatusSettled
		paid := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.sales) * 24 * time.Hour)
		settled := paid.Add(24 * time.Hour)
		tx.CompletedAt = &paid
		tx.SettledAt = &settled
		tx.PlatformFeeTransferred = swept
		tx.SettledPlatformFeeMinor, tx.SettledGatewayFeeMinor, tx.PlatformResidualMinor = nil, nil, nil
		require.NoError(f.t, f.fakes.ProductTransaction().Save(f.ctx, tx))
		// The platform's credit at payment time, then the settlement's two legs.
		payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
		require.NoError(f.t, f.fakes.Journal().Save(f.ctx, payment))
		credit := &domain.LedgerEntry{JournalUUID: payment.UUID, AccountUUID: f.platform.UUID, Amount: available,
			BalanceBucket: domain.BalanceBucketPending, EntryType: domain.EntryTypePlatformCommission,
			SourceType: domain.SourceTypeProductTransaction, SourceID: tx.UUID}
		require.NoError(f.t, f.fakes.LedgerEntry().Save(f.ctx, credit))
		journal := domain.NewJournal(domain.EventTypeSettlement, domain.SourceTypeProductTransaction, tx.UUID, nil)
		require.NoError(f.t, f.fakes.Journal().Save(f.ctx, journal))
		require.NoError(f.t, f.fakes.LedgerEntry().SaveBatch(f.ctx, domain.NewSettlementEntriesForAccount(journal.UUID, tx.UUID, f.platform.UUID, available)))
		txs = append(txs, f.fakes.productTransactionRepo.transactions[tx.UUID])
	}
	for i := 0; i < 7; i++ {
		book(fmt.Sprintf("INV-DOKU-%d", i), f.seller.UUID, "BOOKING", 1000, true)
	}
	book("INV-20260904130258-YTWGBR", f.platform.UUID, "SUBSCRIPTION", 38727, false)
	return txs
}

func TestArchiveToReserve(t *testing.T) {
	f := newPlatformReconFixture(t)
	doku := f.dokuEra()
	f.sale("INV-A", 28)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] += 400016 // CSV7MW
	platformBefore := f.ledgerAvailable(f.platform)
	require.Equal(t, int64(45727+4000), platformBefore)

	statementBefore := f.statement(PlatformTransactionIncome)

	// Before the archive the check reports the DOKU era and CSV7MW together.
	rec := f.check()
	assert.Equal(t, int64(-4572700+400016), rec.Unexplained(), "−45.727 + 4.000,16")

	// Dry run: computed, nothing written.
	plan, err := f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 45727})
	require.NoError(t, err)
	assert.False(t, plan.Applied)
	assert.False(t, plan.AlreadyArchived)
	assert.True(t, plan.AmountMatches)
	assert.Equal(t, int64(45727), plan.Amount)
	assert.Len(t, plan.Sources, 8)
	assert.Equal(t, platformBefore, plan.PlatformAvailableBefore)
	assert.Equal(t, platformBefore-45727, plan.PlatformAvailableAfter)
	assert.Equal(t, int64(0), plan.ReserveAvailableBefore)
	assert.Equal(t, int64(45727), plan.ReserveAvailableAfter)
	assert.Nil(t, plan.ReserveAccount)
	assert.Equal(t, platformBefore, f.ledgerAvailable(f.platform))
	assert.Empty(t, f.fakes.platformReconciliationRepo.adjustments)
	sources := map[string]bool{}
	for _, s := range plan.Sources {
		sources[s.ProductTransactionUUID] = true
	}
	for _, tx := range doku {
		assert.True(t, sources[tx.UUID], tx.InvoiceNumber)
	}

	// A different expected amount is refused, nothing written.
	result, err := f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 45000, Apply: true})
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrReserveArchiveAmountMismatch), "%v", err)
	require.NotNil(t, result)
	assert.Equal(t, int64(45727), result.Amount)
	assert.Equal(t, platformBefore, f.ledgerAvailable(f.platform))
	assert.Empty(t, f.fakes.platformReconciliationRepo.adjustments)

	// Applied.
	result, err = f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 45727, Apply: true, AdminName: "operator", Note: "Arsip era DOKU"})
	require.NoError(t, err)
	assert.True(t, result.Applied)
	require.NotNil(t, result.ReserveAccount)
	assert.Equal(t, domain.OwnerTypeReserve, result.ReserveAccount.OwnerType)
	assert.Equal(t, domain.ReserveArchiveOwnerID, result.ReserveAccount.OwnerID)
	assert.Equal(t, platformBefore-45727, f.ledgerAvailable(f.platform))
	assert.Equal(t, int64(45727), f.ledgerAvailable(result.ReserveAccount))
	require.NotNil(t, result.Adjustment)
	assert.Equal(t, int64(-45727), result.Adjustment.Amount)
	assert.Len(t, result.Adjustment.SourceTransactionUUIDs, 8)
	journal := f.fakes.journalRepo.journals[result.Adjustment.JournalUUID]
	require.NotNil(t, journal)
	assert.Equal(t, domain.EventTypeManualAdjustment, journal.EventType)

	// The DOKU-era statement lines did not move.
	assert.Equal(t, statementBefore, f.statement(PlatformTransactionIncome))

	// Now the check reports CSV7MW alone.
	assert.Equal(t, int64(400016), f.check().Unexplained())

	// Idempotent: a second run is refused, and a dry run says it ran.
	_, err = f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 45727, Apply: true})
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrReserveArchiveAlreadyDone), "%v", err)
	plan, err = f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 45727})
	require.NoError(t, err)
	assert.True(t, plan.AlreadyArchived)
	assert.Equal(t, plan.PlatformAvailableBefore, plan.PlatformAvailableAfter)
	assert.Len(t, f.fakes.platformReconciliationRepo.adjustments, 1)
	assert.Equal(t, platformBefore-45727, f.ledgerAvailable(f.platform))
}

func TestArchiveToReserve_RefusesAnExpectationThatIsNotPositive(t *testing.T) {
	f := newPlatformReconFixture(t)
	_, err := f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{})
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeInvalidRequest, err), "%v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// The statement
// ─────────────────────────────────────────────────────────────────────────────

// statement reads the whole statement of one type, newest first, as (type, id, amount) lines.
func (f *platformReconFixture) statement(kind PlatformTransactionType) []string {
	f.t.Helper()
	var lines []string
	cursor := ""
	for {
		page, err := f.client.GetPlatformTransactions(f.ctx, PlatformTransactionsRequest{Type: kind, Cursor: cursor, PageSize: 2})
		require.NoError(f.t, err)
		for _, line := range page.Transactions {
			lines = append(lines, fmt.Sprintf("%s %s %d", line.Type, line.ID, line.Amount))
		}
		if !page.HasMore {
			return lines
		}
		cursor = page.NextCursor
	}
}

func TestGetPlatformTransactions_AdjustmentLines(t *testing.T) {
	f := newPlatformReconFixture(t)
	f.dokuEra()
	f.sale("INV-A", 72)
	f.sale("INV-B", 90)
	require.Equal(t, 2, f.sweep().Succeeded)
	f.singapay[reconPlatformULID] += 400016

	f.now = f.now.Add(time.Minute)
	archive, err := f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 45727, Apply: true})
	require.NoError(t, err)
	rounding := f.round()
	require.True(t, rounding.Booked)
	rec := f.check()
	difference, err := f.book(rec, domain.PlatformAdjustmentReasonTestFunds, "CSV7MW")
	require.NoError(t, err)

	adjustments := f.statement(PlatformTransactionAdjustment)
	require.Len(t, adjustments, 3)
	assert.ElementsMatch(t, []string{
		fmt.Sprintf("ADJUSTMENT %s -45727", archive.Adjustment.UUID),
		fmt.Sprintf("ADJUSTMENT %s 1", rounding.Adjustment.UUID),
		fmt.Sprintf("ADJUSTMENT %s 4000", difference.Adjustment.UUID),
	}, adjustments)

	// All three kinds interleave, and the lines add up to the balance.
	page, err := f.client.GetPlatformTransactions(f.ctx, PlatformTransactionsRequest{PageSize: 100})
	require.NoError(t, err)
	var sum int64
	kinds := map[PlatformTransactionType]int{}
	for _, line := range page.Transactions {
		kinds[line.Type]++
		sum += line.Amount
		if line.Type == PlatformTransactionAdjustment {
			require.NotNil(t, line.Adjustment)
			assert.Nil(t, line.Income)
			assert.Nil(t, line.Payout)
		}
	}
	assert.Equal(t, 3, kinds[PlatformTransactionAdjustment])
	assert.Equal(t, 10, kinds[PlatformTransactionIncome])
	assert.Equal(t, f.ledgerAvailable(f.platform), sum, "income + adjustments = the AVAILABLE balance")

	all := f.statement("")
	assert.Len(t, all, 13, "paged two at a time across three sources, nothing skipped or repeated")

	_, err = f.client.GetPlatformTransactions(f.ctx, PlatformTransactionsRequest{Type: "REFUND"})
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeInvalidRequest, err), "%v", err)
}
