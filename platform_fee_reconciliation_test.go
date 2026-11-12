package ledger

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixture: one seller, the platform, and a Singapay that keeps the sub-account's balance
// ─────────────────────────────────────────────────────────────────────────────
//
// The fixture books sales the way the money cycle does — the payment entries, then the real
// bookSettlement with what Singapay's read reports — and keeps, beside the ledger, what the
// seller's sub-account really holds at Singapay: each sale's charge less the fee Singapay
// really took, less every transfer and payout. For a card the read reports no fee, so
// settlement books the estimate whatever Singapay took; the reconciliation is what has to
// recover the difference, and the sweep is what proves it did.

// cardFeeSellerULID is SWIMLQ's seller sub-account.
const cardFeeSellerULID = "01M2MGFRBT4G9WPP3TGTQEWP54"

type cardFeeFixture struct {
	t      *testing.T
	ctx    context.Context
	fakes  *FakeRepositoryProvider
	txs    *FakeTransactionProvider
	gw     *fakeGateway
	client *LedgerClient
	fees   *domain.FeeCalculator

	platform *domain.Account
	gateway  *domain.Account
	seller   *domain.Account

	// singapayAvailable and singapayPending are the seller sub-account at Singapay, in sen.
	singapayAvailable int64
	singapayPending   int64
	// balanceErr fails the balance inquiry; transferErr fails every account transfer.
	balanceErr  error
	transferErr error

	sales int
}

func newCardFeeFixture(t *testing.T) *cardFeeFixture {
	t.Helper()
	ctx := context.Background()
	fakes := NewFakeRepositoryProvider()

	platform := domain.NewPlatformAccount("01PLATFORMSUBACCOUNT0000000", "platform", domain.CurrencyIDR)
	platform.SetSingapayAccount("01PLATFORMSUBACCOUNT0000000", "000000000001")
	require.NoError(t, fakes.Account().Save(ctx, &platform))

	gatewayAccount := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
	require.NoError(t, fakes.Account().Save(ctx, &gatewayAccount))

	seller := domain.NewSellerAccount(cardFeeSellerULID, "ninoproton", domain.CurrencyIDR)
	seller.SetSingapayAccount(cardFeeSellerULID, "000000000777")
	require.NoError(t, fakes.Account().Save(ctx, &seller))

	f := &cardFeeFixture{
		t:        t,
		ctx:      ctx,
		fakes:    fakes,
		txs:      NewFakeTransactionProvider(fakes),
		platform: &platform,
		gateway:  &gatewayAccount,
		seller:   &seller,
		fees: domain.NewFeeCalculator([]*domain.FeeConfig{
			{ConfigType: domain.FeeConfigTypePlatform, PaymentChannel: "PLATFORM", FeeType: domain.FeeTypeFixed, FixedAmount: 4000, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: ChannelCreditCard, FeeType: domain.FeeTypeHybrid, FixedAmount: 2000, Percentage: 3, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "QRIS", FeeType: domain.FeeTypePercentage, Percentage: 0.7, IsActive: true},
		}),
	}

	f.gw = &fakeGateway{
		balance: func(accountID string) (*singapay.Balance, error) {
			if f.balanceErr != nil {
				return nil, f.balanceErr
			}
			if accountID != cardFeeSellerULID {
				return nil, fmt.Errorf("fixture knows no sub-account %s", accountID)
			}
			return &singapay.Balance{
				Available: singapay.NewAmountFromMinor(f.singapayAvailable, "IDR"),
				Pending:   singapay.NewAmountFromMinor(f.singapayPending, "IDR"),
				Held:      singapay.NewAmountFromMinor(0, "IDR"),
				Total:     singapay.NewAmountFromMinor(f.singapayAvailable+f.singapayPending, "IDR"),
				AccountID: accountID,
			}, nil
		},
		transfer: func(from string, req singapay.TransferRequest) (*singapay.AccountTransfer, error) {
			if f.transferErr != nil {
				return nil, f.transferErr
			}
			if from != cardFeeSellerULID {
				return nil, fmt.Errorf("fixture knows no sub-account %s", from)
			}
			if req.Amount.Minor() > f.singapayAvailable {
				return nil, &singapay.Error{StatusCode: http.StatusUnprocessableEntity, Message: "Insufficient remitter account balance"}
			}
			f.singapayAvailable -= req.Amount.Minor()
			return &singapay.AccountTransfer{Status: "success", MerchantRefNo: req.MerchantRefNo, Amount: req.Amount}, nil
		},
	}

	f.client = &LedgerClient{
		txProvider:   f.txs,
		repoProvider: fakes,
		logger:       testLogger(),
		gateway:      f.gw,
	}
	return f
}

// cardFee is what Singapay takes on a card charge, in sen: 3% + Rp2.000, plus noise sen —
// SWIMLQ's was one sen above the formula.
func cardFee(totalCharged, noise int64) int64 { return totalCharged*3 + 200000 + noise }

// sale books one paid, settled sale, and credits the sub-account with what Singapay leaves
// there: the charge less realFeeMinor.
func (f *cardFeeFixture) sale(invoice, channel string, sellerPrice, realFeeMinor int64) *domain.ProductTransaction {
	f.t.Helper()
	f.sales++

	fee := f.fees.GetFeeBreakdown(sellerPrice, channel, domain.CurrencyIDR)
	tx := domain.NewProductTransaction("buyer-"+invoice, f.seller.UUID, "product-"+invoice, "BOOKING", invoice, fee, nil)
	tx.Status = domain.TransactionStatusCompleted
	completedAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.sales) * time.Minute)
	tx.CompletedAt = &completedAt
	require.NoError(f.t, f.fakes.ProductTransaction().Save(f.ctx, tx))

	pr := domain.NewPaymentRequest(tx.UUID, "req-"+invoice, channel, fee.TotalCharged, domain.CurrencyIDR, completedAt.Add(time.Hour))
	require.NoError(f.t, f.fakes.PaymentRequest().Save(f.ctx, pr))

	payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(f.t, f.fakes.Journal().Save(f.ctx, payment))
	require.NoError(f.t, f.fakes.LedgerEntry().SaveBatch(f.ctx, domain.NewPaymentEntries(
		payment.UUID, tx.UUID, f.seller.UUID, fee.SellerNetAmount, f.platform.UUID, fee.PlatformFee, f.gateway.UUID, fee.GatewayFee)))

	gross := domain.RupiahToMinor(fee.TotalCharged)
	settled := domain.SettledTransaction{
		MerchantReference:    invoice,
		GatewayTransactionID: "gw-" + invoice,
		GatewayAccountID:     f.seller.SingapayAccountID,
		PaymentChannel:       channel,
		GrossMinor:           gross,
		FeeMinor:             realFeeMinor,
		FeeReported:          true,
	}
	if channel == ChannelCreditCard {
		// What readGatewayTransaction does for a card: no fee is reported, the estimate is copied.
		settled.FeeMinor = domain.RupiahToMinor(fee.GatewayFee)
		settled.FeeReported = false
	}
	settled.NetMinor = gross - settled.FeeMinor

	outcome, err := f.client.bookSettlement(f.ctx, tx, settled)
	require.NoError(f.t, err)
	require.Equal(f.t, settleOutcomeSettled, outcome)

	settledAt := completedAt.Add(24 * time.Hour)
	stored := f.tx(tx.UUID)
	stored.SettledAt = &settledAt

	f.singapayAvailable += gross - realFeeMinor
	return stored
}

// card books a card sale of sellerPrice, Singapay taking the formula fee plus noise sen.
func (f *cardFeeFixture) card(invoice string, sellerPrice, noise int64) *domain.ProductTransaction {
	fee := f.fees.GetFeeBreakdown(sellerPrice, ChannelCreditCard, domain.CurrencyIDR)
	return f.sale(invoice, ChannelCreditCard, sellerPrice, cardFee(fee.TotalCharged, noise))
}

// withdrawn books a payout Singapay has completed: debited in the ledger and at Singapay.
func (f *cardFeeFixture) withdrawn(amount int64) {
	f.t.Helper()
	d := f.payout(amount)
	d.Status = domain.DisbursementStatusCompleted
	require.NoError(f.t, f.fakes.Disbursement().Save(f.ctx, d))
	f.singapayAvailable -= domain.RupiahToMinor(amount)
}

// payout books a reserved payout: the ledger debit is written, Singapay has not debited yet.
func (f *cardFeeFixture) payout(amount int64) *domain.Disbursement {
	f.t.Helper()
	d, err := domain.NewDisbursement(f.seller.UUID, amount, domain.CurrencyIDR,
		domain.BankAccount{BankCode: "CENAIDJA", AccountNumber: "1234567890", AccountName: "Nino Proton"}, "withdrawal")
	require.NoError(f.t, err)
	require.NoError(f.t, f.fakes.Disbursement().Save(f.ctx, d))
	journal := domain.NewJournal(domain.EventTypeDisbursement, domain.SourceTypeDisbursement, d.UUID, nil)
	require.NoError(f.t, f.fakes.Journal().Save(f.ctx, journal))
	require.NoError(f.t, f.fakes.LedgerEntry().Save(f.ctx, domain.NewDisbursementEntry(journal.UUID, d.UUID, f.seller.UUID, amount)))
	return d
}

func (f *cardFeeFixture) tx(id string) *domain.ProductTransaction {
	f.t.Helper()
	tx, ok := f.fakes.productTransactionRepo.transactions[id]
	require.True(f.t, ok, "transaction %s", id)
	return tx
}

func (f *cardFeeFixture) source(tx *domain.ProductTransaction) domain.SettledFeeSource {
	return f.fakes.productTransactionRepo.feeSources[tx.UUID]
}

func (f *cardFeeFixture) reconcile() *CardPlatformFeeReconciliationResult {
	f.t.Helper()
	result, err := f.client.ReconcileCardPlatformFees(f.ctx, CardPlatformFeeReconciliationOptions{})
	require.NoError(f.t, err)
	return result
}

func (f *cardFeeFixture) sweep() *PlatformFeeTransferResult {
	f.t.Helper()
	result, err := f.client.ProcessPlatformFeeTransfer(f.ctx, 50)
	require.NoError(f.t, err)
	return result
}

// ledgerAvailable is an account's AVAILABLE balance, summed from its entries.
func (f *cardFeeFixture) ledgerAvailable(account *domain.Account) int64 {
	f.t.Helper()
	_, available, err := f.fakes.LedgerEntry().GetAllBalances(f.ctx, account.UUID)
	require.NoError(f.t, err)
	return available
}

// platformEntriesFor sums the platform's AVAILABLE entries for one transaction: its line on
// the platform statement once settled.
func (f *cardFeeFixture) platformEntriesFor(tx *domain.ProductTransaction) int64 {
	var sum int64
	for _, e := range f.fakes.ledgerEntryRepo.entries {
		if e.AccountUUID == f.platform.UUID && e.SourceID == tx.UUID && e.BalanceBucket == domain.BalanceBucketAvailable {
			sum += e.Amount
		}
	}
	return sum
}

func (f *cardFeeFixture) feeAdjustments() []*domain.LedgerEntry {
	var out []*domain.LedgerEntry
	for _, e := range f.fakes.ledgerEntryRepo.entries {
		if e.EntryType == domain.EntryTypeFeeAdjustment {
			out = append(out, e)
		}
	}
	return out
}

func (f *cardFeeFixture) reconciliationJournals() []*domain.Journal {
	var out []*domain.Journal
	for _, j := range f.fakes.journalRepo.journals {
		if j.EventType == domain.EventTypeReconciliation {
			out = append(out, j)
		}
	}
	return out
}

// requireSubAccountMatchesLedger is the invariant the whole mechanism exists for, checked
// once every fee is swept: the seller's sub-account holds exactly their ledger balance.
func (f *cardFeeFixture) requireSubAccountMatchesLedger() {
	f.t.Helper()
	require.Equal(f.t, domain.RupiahToMinor(f.ledgerAvailable(f.seller)), f.singapayAvailable,
		"the seller's Singapay balance equals their ledger balance once the platform fees are swept")
}

func minorPtr(v int64) *int64 { return &v }

// ─────────────────────────────────────────────────────────────────────────────
// The SWIMLQ incident
// ─────────────────────────────────────────────────────────────────────────────

// INV-20260925161538-SWIMLQ, before the manual correction: settled on the estimate (fee
// 4041.00, platform fee 4000.00), Singapay took 4041.24, the seller withdrew everything
// else, and 3999.76 is left. The round recovers 3999.76 from the balance alone and writes
// what the manual correction wrote by hand: 404124 / 399976 / 76 and a −1 FEE_ADJUSTMENT.
func TestReconcileCardPlatformFees_SWIMLQBeforeTheManualCorrection(t *testing.T) {
	f := newCardFeeFixture(t)
	swimlq := f.card("INV-20260925161538-SWIMLQ", 60000, 1)
	require.Equal(t, int64(68041), swimlq.Fee.TotalCharged)
	require.Equal(t, int64(4041), swimlq.Fee.GatewayFee)
	require.Equal(t, domain.SettledFeeSourceEstimate, f.source(swimlq), "settlement marks a card's figures as the estimate")
	require.Equal(t, int64(400000), *swimlq.SettledPlatformFeeMinor)
	require.Equal(t, int64(404100), *swimlq.SettledGatewayFeeMinor)

	f.withdrawn(60000)
	require.Equal(t, int64(399976), f.singapayAvailable)
	require.Zero(t, f.ledgerAvailable(f.seller))

	// Before the round, the sweep does not touch it: the estimate is what failed with 422.
	before := f.sweep()
	assert.Zero(t, before.Succeeded+before.Failed)
	assert.Empty(t, f.gw.transfers)

	result := f.reconcile()
	require.Equal(t, 1, result.Applied)
	require.Len(t, result.Sellers, 1)
	seller := result.Sellers[0]
	assert.Equal(t, CardFeeOutcomeApplied, seller.Outcome)
	assert.Equal(t, int64(399976), *seller.ComputedPlatformFeeMinor)
	assert.Equal(t, int64(400000), seller.NominalPlatformFeeMinor)
	require.Len(t, seller.Invoices, 1)
	assert.Equal(t, int64(-1), seller.Invoices[0].PlatformEntryAdjustment)

	swimlq = f.tx(swimlq.UUID)
	assert.Equal(t, domain.SettledFeeSourceBalance, f.source(swimlq))
	assert.Equal(t, int64(399976), *swimlq.SettledPlatformFeeMinor)
	assert.Equal(t, int64(404124), *swimlq.SettledGatewayFeeMinor)
	assert.Equal(t, int64(76), *swimlq.PlatformResidualMinor)
	assert.Equal(t, int64(3999), f.platformEntriesFor(swimlq), "the statement line shows the recomputed fee")

	adjustments := f.feeAdjustments()
	require.Len(t, adjustments, 1)
	assert.Equal(t, int64(-1), adjustments[0].Amount)
	assert.Equal(t, f.platform.UUID, adjustments[0].AccountUUID)
	assert.Equal(t, domain.BalanceBucketAvailable, adjustments[0].BalanceBucket)
	assert.Equal(t, domain.SourceTypeProductTransaction, adjustments[0].SourceType)
	assert.Equal(t, swimlq.UUID, adjustments[0].SourceID)

	journals := f.reconciliationJournals()
	require.Len(t, journals, 1)
	assert.Equal(t, domain.SourceTypeProductTransaction, journals[0].SourceType)
	assert.Equal(t, swimlq.UUID, journals[0].SourceID)
	assert.Equal(t, adjustments[0].JournalUUID, journals[0].UUID)

	recs := f.fakes.reconciliationRepo.reconciliations
	require.Len(t, recs, 1)
	assert.Equal(t, domain.PlatformFeeReconciliationApplied, recs[0].Outcome)
	assert.Empty(t, recs[0].HoldReason)
	assert.Equal(t, int64(399976), *recs[0].SingapayAvailableMinor)
	assert.Equal(t, int64(0), *recs[0].SingapayHeldMinor)
	assert.Zero(t, recs[0].SellerLedgerAvailableMinor)
	assert.Zero(t, recs[0].CertainUnsweptPlatformFeeMinor)
	assert.Equal(t, int64(399976), *recs[0].ComputedPlatformFeeMinor)
	assert.Equal(t, int64(10000), recs[0].ToleranceMinor)
	assert.Equal(t, 1, recs[0].CardCount)
	assert.NotEmpty(t, recs[0].RawBalance)

	items := f.fakes.reconciliationRepo.items
	require.Len(t, items, 1)
	assert.Equal(t, recs[0].UUID, items[0].ReconciliationUUID)
	assert.Equal(t, int64(400000), items[0].PreviousSettledPlatformFeeMinor)
	assert.Equal(t, int64(404100), items[0].PreviousSettledGatewayFeeMinor)
	assert.Equal(t, int64(399976), *items[0].AllocatedPlatformFeeMinor)
	assert.Equal(t, journals[0].UUID, items[0].JournalUUID)
	assert.Equal(t, adjustments[0].UUID, items[0].LedgerEntryUUID)

	// And now the sweep moves exactly what is there.
	swept := f.sweep()
	require.Equal(t, 1, swept.Succeeded)
	require.Len(t, f.gw.transfers, 1)
	assert.Equal(t, int64(399976), f.gw.transfers[0].Amount.Minor())
	assert.Zero(t, f.singapayAvailable)
	f.requireSubAccountMatchesLedger()
}

// Production today: the manual correction made SWIMLQ MANUAL (399976 / 404124 / 76), and the
// 13:00 sweep has not moved it yet. It is not a candidate — nothing about it changes — and
// its 3999.76 is subtracted as a final fee when the seller's next card is reconciled.
func TestReconcileCardPlatformFees_SWIMLQAfterTheManualCorrection(t *testing.T) {
	f := newCardFeeFixture(t)
	swimlq := f.card("INV-20260925161538-SWIMLQ", 60000, 1)
	f.withdrawn(60000)

	// The correction script, as migration 032 then marks it.
	*swimlq.SettledPlatformFeeMinor = 399976
	*swimlq.SettledGatewayFeeMinor = 404124
	*swimlq.PlatformResidualMinor = 76
	f.fakes.productTransactionRepo.feeSources[swimlq.UUID] = domain.SettledFeeSourceManual

	// Nothing else is on the estimate: no round at all, and no balance read.
	empty := f.reconcile()
	assert.Empty(t, empty.Sellers)
	assert.Empty(t, f.gw.balanceCalls)

	// A new card for the same seller.
	next := f.card("INV-NEXT", 100000, 0)

	result := f.reconcile()
	require.Equal(t, 1, result.Applied)
	seller := result.Sellers[0]
	assert.Equal(t, int64(399976), seller.CertainUnsweptPlatformFeeMinor, "SWIMLQ's share is final and still in the sub-account")
	require.Len(t, seller.Invoices, 1)
	assert.Equal(t, next.UUID, seller.Invoices[0].TransactionID)

	swimlq = f.tx(swimlq.UUID)
	assert.Equal(t, domain.SettledFeeSourceManual, f.source(swimlq))
	assert.Equal(t, int64(399976), *swimlq.SettledPlatformFeeMinor)
	assert.Equal(t, int64(404124), *swimlq.SettledGatewayFeeMinor)

	next = f.tx(next.UUID)
	expected := domain.RupiahToMinor(next.Fee.TotalCharged) - domain.RupiahToMinor(next.Fee.SellerNetAmount) - cardFee(next.Fee.TotalCharged, 0)
	assert.Equal(t, expected, *next.SettledPlatformFeeMinor)
	for _, item := range f.fakes.reconciliationRepo.items {
		assert.NotEqual(t, swimlq.UUID, item.ProductTransactionUUID, "a MANUAL row is never recorded as a candidate")
	}

	swept := f.sweep()
	assert.Equal(t, 2, swept.Succeeded)
	f.requireSubAccountMatchesLedger()
}

// ─────────────────────────────────────────────────────────────────────────────
// Many cards
// ─────────────────────────────────────────────────────────────────────────────

// sellerPrices are fifteen prices, so the fifteen transactions weigh differently.
var sellerPrices = []int64{60000, 15000, 250000, 75000, 125000, 30000, 99000, 500000, 42000, 18000, 64000, 1000000, 37500, 88000, 150000}

// Fourteen cards and one QRIS. The QRIS fee is Singapay's own and final at settlement; the
// cards' total comes from the balance, and each card's share lands within a sen or two of
// what Singapay really left it.
func TestReconcileCardPlatformFees_FourteenOfFifteenAreCards(t *testing.T) {
	f := newCardFeeFixture(t)
	var cards []*domain.ProductTransaction
	real := map[string]int64{}
	var qris *domain.ProductTransaction
	for i, price := range sellerPrices {
		invoice := fmt.Sprintf("INV-MIX-%02d", i)
		if i == 7 {
			qrisFee := f.fees.GetFeeBreakdown(price, "QRIS", domain.CurrencyIDR)
			qris = f.sale(invoice, "QRIS", price, domain.RupiahToMinor(qrisFee.GatewayFee)-16)
			continue
		}
		noise := int64(i % 3) // Singapay a sen or two above the formula, or exact
		tx := f.card(invoice, price, noise)
		cards = append(cards, tx)
		real[tx.UUID] = domain.RupiahToMinor(tx.Fee.TotalCharged) - domain.RupiahToMinor(tx.Fee.SellerNetAmount) - cardFee(tx.Fee.TotalCharged, noise)
	}
	require.Len(t, cards, 14)
	require.Equal(t, domain.SettledFeeSourceGateway, f.source(qris))
	qrisFee := *qris.SettledPlatformFeeMinor
	require.Equal(t, int64(400016), qrisFee, "Singapay charged 0.16 less on the QRIS")

	result := f.reconcile()
	require.Equal(t, 1, result.Applied)
	seller := result.Sellers[0]
	assert.Equal(t, 14, seller.CardCount)
	assert.Equal(t, qrisFee, seller.CertainUnsweptPlatformFeeMinor)

	var total, realTotal int64
	for _, card := range cards {
		got := f.tx(card.UUID)
		assert.Equal(t, domain.SettledFeeSourceBalance, f.source(got))
		share := *got.SettledPlatformFeeMinor
		total += share
		realTotal += real[card.UUID]
		assert.InDelta(t, real[card.UUID], share, 2, "%s: within two sen of what Singapay left", card.InvoiceNumber)
		// The payer's fees are unchanged; only sen moved between the two parts.
		assert.Equal(t, domain.RupiahToMinor(got.Fee.TotalCharged)-domain.RupiahToMinor(got.Fee.SellerNetAmount),
			share+*got.SettledGatewayFeeMinor)
		rupiah, residual := domain.SplitPlatformFeeMinor(share)
		assert.Equal(t, residual, *got.PlatformResidualMinor)
		assert.Equal(t, rupiah, f.platformEntriesFor(got), "%s: statement line = whole-rupiah part", card.InvoiceNumber)
	}
	assert.Equal(t, realTotal, total, "the shares add up to the computed total exactly")
	assert.Equal(t, *seller.ComputedPlatformFeeMinor, total)

	// The QRIS is untouched.
	qris = f.tx(qris.UUID)
	assert.Equal(t, domain.SettledFeeSourceGateway, f.source(qris))
	assert.Equal(t, qrisFee, *qris.SettledPlatformFeeMinor)
	assert.Len(t, f.reconciliationJournals(), 14)
	assert.Len(t, f.fakes.reconciliationRepo.items, 14)

	swept := f.sweep()
	assert.Equal(t, 15, swept.Succeeded)
	f.requireSubAccountMatchesLedger()
}

func TestReconcileCardPlatformFees_AllFifteenAreCards(t *testing.T) {
	f := newCardFeeFixture(t)
	var cards []*domain.ProductTransaction
	real := map[string]int64{}
	for i, price := range sellerPrices {
		noise := int64((i * 7) % 3)
		tx := f.card(fmt.Sprintf("INV-CARD-%02d", i), price, noise)
		cards = append(cards, tx)
		real[tx.UUID] = domain.RupiahToMinor(tx.Fee.TotalCharged) - domain.RupiahToMinor(tx.Fee.SellerNetAmount) - cardFee(tx.Fee.TotalCharged, noise)
	}

	result := f.reconcile()
	require.Equal(t, 1, result.Applied)
	assert.Zero(t, result.Sellers[0].CertainUnsweptPlatformFeeMinor)

	var total, realTotal int64
	for _, card := range cards {
		share := *f.tx(card.UUID).SettledPlatformFeeMinor
		total += share
		realTotal += real[card.UUID]
		assert.InDelta(t, real[card.UUID], share, 2, card.InvoiceNumber)
	}
	assert.Equal(t, realTotal, total)
	assert.Len(t, f.fakes.reconciliationRepo.items, 15)

	swept := f.sweep()
	assert.Equal(t, 15, swept.Succeeded)
	f.requireSubAccountMatchesLedger()
}

// ─────────────────────────────────────────────────────────────────────────────
// Holds
// ─────────────────────────────────────────────────────────────────────────────

// requireHeld checks a held round wrote its row and one empty item per card, and nothing
// else: the cards are still ESTIMATE on their old figures, and the sweep still leaves them.
func (f *cardFeeFixture) requireHeld(result *CardPlatformFeeReconciliationResult, reason domain.PlatformFeeReconciliationHoldReason, cards ...*domain.ProductTransaction) {
	f.t.Helper()
	require.Equal(f.t, 1, result.Held)
	require.Len(f.t, result.Sellers, 1)
	assert.Equal(f.t, CardFeeOutcomeHeld, result.Sellers[0].Outcome)
	assert.Equal(f.t, string(reason), result.Sellers[0].HoldReason)

	recs := f.fakes.reconciliationRepo.reconciliations
	require.Len(f.t, recs, 1)
	assert.Equal(f.t, domain.PlatformFeeReconciliationHeld, recs[0].Outcome)
	assert.Equal(f.t, reason, recs[0].HoldReason)
	assert.Equal(f.t, len(cards), recs[0].CardCount)

	items := f.fakes.reconciliationRepo.items
	require.Len(f.t, items, len(cards))
	for _, item := range items {
		assert.Nil(f.t, item.AllocatedPlatformFeeMinor)
		assert.Nil(f.t, item.SettledGatewayFeeMinor)
		assert.Nil(f.t, item.PlatformResidualMinor)
		assert.Empty(f.t, item.JournalUUID)
		assert.Empty(f.t, item.LedgerEntryUUID)
	}

	for _, card := range cards {
		got := f.tx(card.UUID)
		assert.Equal(f.t, domain.SettledFeeSourceEstimate, f.source(got))
		assert.Equal(f.t, domain.RupiahToMinor(got.Fee.PlatformFee), *got.SettledPlatformFeeMinor)
	}
	assert.Empty(f.t, f.feeAdjustments())
	assert.Empty(f.t, f.reconciliationJournals())

	swept := f.sweep()
	assert.Zero(f.t, swept.Succeeded+swept.Failed, "a held card is not swept")
}

// The ledger settled the card, but at Singapay its money is still pending: the available
// balance is short by the whole sale, and booking that as a fee would be absurd.
func TestReconcileCardPlatformFees_HoldsACardSettledInTheLedgerButNotAvailableAtSingapay(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-NOT-YET", 60000, 1)
	notYet := domain.RupiahToMinor(card.Fee.TotalCharged) - cardFee(card.Fee.TotalCharged, 1)
	f.singapayAvailable -= notYet
	f.singapayPending += notYet

	result := f.reconcile()
	f.requireHeld(result, domain.HoldDeviationAboveTolerance, card)
	rec := f.fakes.reconciliationRepo.reconciliations[0]
	require.NotNil(t, rec.ComputedPlatformFeeMinor)
	assert.Equal(t, int64(-6000000), *rec.ComputedPlatformFeeMinor, "the seller's Rp60.000 is booked and not there")
	assert.Equal(t, notYet, *rec.SingapayPendingMinor)
}

// Money the ledger has not booked: a VA payment Singapay has settled into the sub-account
// that the settlement pass has not reached yet.
func TestReconcileCardPlatformFees_HoldsMoneyTheLedgerHasNotBooked(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.singapayAvailable += domain.RupiahToMinor(50000)

	result := f.reconcile()
	f.requireHeld(result, domain.HoldDeviationAboveTolerance, card)
	assert.Greater(t, *f.fakes.reconciliationRepo.reconciliations[0].ComputedPlatformFeeMinor, int64(5000000))
}

// A payout in flight: the ledger has debited it and Singapay may not have. The balance is not
// even asked for.
func TestReconcileCardPlatformFees_HoldsWhileAPayoutIsInFlight(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.payout(30000) // PENDING: debited in the ledger only

	result := f.reconcile()
	f.requireHeld(result, domain.HoldDisbursementInFlight, card)
	assert.Empty(t, f.gw.balanceCalls)
	assert.Nil(t, f.fakes.reconciliationRepo.reconciliations[0].SingapayAvailableMinor)
}

func TestReconcileCardPlatformFees_HoldsWhenTheBalanceCannotBeRead(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.balanceErr = &singapay.Error{StatusCode: http.StatusForbidden, Message: "Your IP address is not registered"}

	result := f.reconcile()
	f.requireHeld(result, domain.HoldBalanceReadFailed, card)
	assert.Contains(t, result.Sellers[0].Detail, "not registered")
	rec := f.fakes.reconciliationRepo.reconciliations[0]
	assert.Nil(t, rec.SingapayAvailableMinor)
	assert.Nil(t, rec.ComputedPlatformFeeMinor)
	assert.Nil(t, rec.RawBalance)
}

// A held round is tried again: once the money is available, the next round applies.
func TestReconcileCardPlatformFees_AHeldRoundIsAppliedOnceTheBalanceAgrees(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-NOT-YET", 60000, 1)
	notYet := domain.RupiahToMinor(card.Fee.TotalCharged) - cardFee(card.Fee.TotalCharged, 1)
	f.singapayAvailable -= notYet

	held := f.reconcile()
	require.Equal(t, 1, held.Held)

	f.singapayAvailable += notYet
	applied := f.reconcile()
	require.Equal(t, 1, applied.Applied)
	assert.Equal(t, domain.SettledFeeSourceBalance, f.source(f.tx(card.UUID)))
	assert.Len(t, f.fakes.reconciliationRepo.reconciliations, 2)
	assert.Len(t, f.fakes.reconciliationRepo.items, 2, "one empty item from the hold, one applied")
}

// ─────────────────────────────────────────────────────────────────────────────
// Across rounds
// ─────────────────────────────────────────────────────────────────────────────

// Yesterday's card was made BALANCE but its transfer failed; today another card settles.
// Yesterday's share is a final fee now: it is subtracted, not recomputed, and only today's
// card is a candidate.
func TestReconcileCardPlatformFees_YesterdaysBalanceCardIsCertainToday(t *testing.T) {
	f := newCardFeeFixture(t)
	yesterday := f.card("INV-YESTERDAY", 60000, 1)
	require.Equal(t, 1, f.reconcile().Applied)
	yesterdayShare := *f.tx(yesterday.UUID).SettledPlatformFeeMinor
	require.Equal(t, int64(399976), yesterdayShare)

	f.transferErr = &singapay.Error{StatusCode: http.StatusBadGateway, Message: "upstream timeout"}
	failed := f.sweep()
	require.Equal(t, 1, failed.Failed)
	f.transferErr = nil

	today := f.card("INV-TODAY", 100000, 2)
	result := f.reconcile()
	require.Equal(t, 1, result.Applied)
	seller := result.Sellers[0]
	assert.Equal(t, yesterdayShare, seller.CertainUnsweptPlatformFeeMinor)
	require.Len(t, seller.Invoices, 1)
	assert.Equal(t, today.UUID, seller.Invoices[0].TransactionID)

	assert.Equal(t, yesterdayShare, *f.tx(yesterday.UUID).SettledPlatformFeeMinor, "not recomputed")
	todayReal := domain.RupiahToMinor(today.Fee.TotalCharged) - domain.RupiahToMinor(today.Fee.SellerNetAmount) - cardFee(today.Fee.TotalCharged, 2)
	assert.Equal(t, todayReal, *f.tx(today.UUID).SettledPlatformFeeMinor)

	applied := 0
	for _, item := range f.fakes.reconciliationRepo.items {
		if item.ProductTransactionUUID == yesterday.UUID {
			applied++
		}
	}
	assert.Equal(t, 1, applied, "yesterday's card was recomputed once")

	swept := f.sweep()
	assert.Equal(t, 2, swept.Succeeded)
	f.requireSubAccountMatchesLedger()
}

func TestReconcileCardPlatformFees_ASecondRoundChangesNothing(t *testing.T) {
	f := newCardFeeFixture(t)
	f.card("INV-A", 60000, 1)
	f.card("INV-B", 75000, 0)
	require.Equal(t, 1, f.reconcile().Applied)

	entries := len(f.fakes.ledgerEntryRepo.entries)
	journals := len(f.fakes.journalRepo.journals)
	recs := len(f.fakes.reconciliationRepo.reconciliations)
	items := len(f.fakes.reconciliationRepo.items)
	calls := len(f.gw.balanceCalls)
	transactions := f.txs.transactions

	again := f.reconcile()
	assert.Empty(t, again.Sellers)
	assert.Zero(t, again.Applied+again.Held+again.Skipped+again.Failed)
	assert.Len(t, f.fakes.ledgerEntryRepo.entries, entries)
	assert.Len(t, f.fakes.journalRepo.journals, journals)
	assert.Len(t, f.fakes.reconciliationRepo.reconciliations, recs)
	assert.Len(t, f.fakes.reconciliationRepo.items, items)
	assert.Len(t, f.gw.balanceCalls, calls, "no balance read when nothing is on the estimate")
	assert.Equal(t, transactions, f.txs.transactions)
}

// ─────────────────────────────────────────────────────────────────────────────
// Races
// ─────────────────────────────────────────────────────────────────────────────

// The ledger moves while the balance is read — here a VA settling for the seller. The balance
// may be from before it or after; the round records nothing and the next one reads again.
func TestReconcileCardPlatformFees_SkipsWhenTheLedgerMovesDuringTheBalanceRead(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.fakes.reconciliationRepo.beforeSnapshot = func(call int) {
		if call == 2 {
			f.sale("INV-VA", "QRIS", 20000, 15000)
		}
	}

	result := f.reconcile()
	require.Equal(t, 1, result.Skipped)
	assert.Equal(t, CardFeeOutcomeSkipped, result.Sellers[0].Outcome)
	assert.Empty(t, f.fakes.reconciliationRepo.reconciliations)
	assert.Empty(t, f.fakes.reconciliationRepo.items)
	assert.Empty(t, f.reconciliationJournals())
	assert.Equal(t, domain.SettledFeeSourceEstimate, f.source(f.tx(card.UUID)))
}

// A payout that starts during the balance read holds the round rather than skipping it.
func TestReconcileCardPlatformFees_HoldsWhenAPayoutStartsDuringTheBalanceRead(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.fakes.reconciliationRepo.beforeSnapshot = func(call int) {
		if call == 2 {
			f.payout(10000)
		}
	}

	result := f.reconcile()
	f.requireHeld(result, domain.HoldDisbursementInFlight, card)
	assert.NotNil(t, f.fakes.reconciliationRepo.reconciliations[0].SingapayAvailableMinor, "the balance was read, and is recorded")
}

// Between the second read and the write, another round (or a person) changes the card. The
// conditional update finds nothing to move and nothing is written.
func TestReconcileCardPlatformFees_LosesTheRaceWithoutWriting(t *testing.T) {
	f := newCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.fakes.productTransactionRepo.beforeBalancedCAS = func() {
		f.fakes.productTransactionRepo.feeSources[card.UUID] = domain.SettledFeeSourceManual
	}

	result := f.reconcile()
	require.Equal(t, 1, result.Skipped)
	assert.Contains(t, result.Sellers[0].Detail, card.InvoiceNumber)
	assert.Empty(t, f.fakes.reconciliationRepo.reconciliations)
	assert.Empty(t, f.fakes.reconciliationRepo.items)
	assert.Empty(t, f.reconciliationJournals())
	assert.Empty(t, f.feeAdjustments())
	assert.Equal(t, int64(400000), *f.tx(card.UUID).SettledPlatformFeeMinor)
}

// ─────────────────────────────────────────────────────────────────────────────
// What is not a candidate
// ─────────────────────────────────────────────────────────────────────────────

// VA, QRIS and e-wallet report their own fee: settled GATEWAY, swept as before, never asked
// about. A subscription is a payment link on GATEWAY_ON_SELLER with no platform fee: never a
// candidate either.
func TestReconcileCardPlatformFees_LeavesEveryOtherChannelAlone(t *testing.T) {
	f := newCardFeeFixture(t)
	fee := f.fees.GetFeeBreakdown(50000, "QRIS", domain.CurrencyIDR)
	qris := f.sale("INV-QRIS", "QRIS", 50000, domain.RupiahToMinor(fee.GatewayFee)+30)
	assert.Equal(t, domain.SettledFeeSourceGateway, f.source(qris))

	subscription := domain.NewProductTransaction("buyer-sub", f.seller.UUID, "plan", "SUBSCRIPTION", "INV-SUB",
		domain.FeeBreakdown{SellerPrice: 99000, TotalCharged: 99000, SellerNetAmount: 99000 - 2970, GatewayFee: 2970,
			FeeModel: domain.FeeModelGatewayOnSeller, Currency: domain.CurrencyIDR}, nil)
	subscription.Status = domain.TransactionStatusCompleted
	require.NoError(t, f.fakes.ProductTransaction().Save(f.ctx, subscription))
	require.NoError(t, f.fakes.PaymentRequest().Save(f.ctx,
		domain.NewPaymentRequest(subscription.UUID, "req-sub", ChannelPaymentLink, 99000, domain.CurrencyIDR, time.Now())))
	outcome, err := f.client.bookSettlement(f.ctx, subscription, domain.SettledTransaction{
		PaymentChannel: ChannelPaymentLink, GrossMinor: 9900000, FeeMinor: 297000, NetMinor: 9603000,
	})
	require.NoError(t, err)
	require.Equal(t, settleOutcomeSettled, outcome)
	assert.Equal(t, domain.SettledFeeSourceEstimate, f.source(subscription), "a payment link's figures are the estimate")

	result := f.reconcile()
	assert.Empty(t, result.Sellers)
	assert.Empty(t, f.gw.balanceCalls)

	swept := f.sweep()
	assert.Equal(t, 1, swept.Succeeded, "the QRIS is swept as before; the subscription has no platform fee")
	assert.Equal(t, *qris.SettledPlatformFeeMinor, f.gw.transfers[0].Amount.Minor())
}
