package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// The card platform-fee reconciliation against PostgreSQL: the snapshot statement, the
// conditional update, the CHECK constraints and indexes of migration 032, the platform's
// running balance, and a rollback that really rolls back. It runs only when
// LEDGER_TEST_DATABASE_URL names a database (README_TESTING.md), each test in a schema of
// its own built from database/schemas/schema.sql.

type pgCardFeeFixture struct {
	t      *testing.T
	ctx    context.Context
	db     *sql.DB
	client *LedgerClient
	gw     *fakeGateway
	repos  repo.RepositoryProvider
	fees   *domain.FeeCalculator

	platform, gateway, seller *domain.Account

	// singapayAvailable is the seller sub-account's available balance at Singapay, in sen.
	singapayAvailable int64
	sales             int
}

func newPgCardFeeFixture(t *testing.T) *pgCardFeeFixture {
	t.Helper()
	db := ledgerTestDB(t)
	ctx := context.Background()
	repos := repo.NewRepositoryProvider(db)

	platform := domain.NewPlatformAccount("01PGPLATFORM000000000000000", "platform", domain.CurrencyIDR)
	platform.SetSingapayAccount("01PGPLATFORM000000000000000", "000000000001")
	require.NoError(t, repos.Account().Save(ctx, &platform))
	gatewayAccount := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
	require.NoError(t, repos.Account().Save(ctx, &gatewayAccount))
	seller := domain.NewSellerAccount(cardFeeSellerULID, "ninoproton", domain.CurrencyIDR)
	seller.SetSingapayAccount(cardFeeSellerULID, "000000000777")
	require.NoError(t, repos.Account().Save(ctx, &seller))

	f := &pgCardFeeFixture{
		t: t, ctx: ctx, db: db, repos: repos,
		platform: &platform, gateway: &gatewayAccount, seller: &seller,
		fees: domain.NewFeeCalculator([]*domain.FeeConfig{
			{ConfigType: domain.FeeConfigTypePlatform, PaymentChannel: "PLATFORM", FeeType: domain.FeeTypeFixed, FixedAmount: 4000, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: ChannelCreditCard, FeeType: domain.FeeTypeHybrid, FixedAmount: 2000, Percentage: 3, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "QRIS", FeeType: domain.FeeTypePercentage, Percentage: 0.7, IsActive: true},
		}),
	}
	f.gw = &fakeGateway{balance: func(accountID string) (*singapay.Balance, error) {
		if accountID != cardFeeSellerULID {
			return nil, fmt.Errorf("unknown sub-account %s", accountID)
		}
		return &singapay.Balance{
			Available: singapay.NewAmountFromMinor(f.singapayAvailable, "IDR"),
			Total:     singapay.NewAmountFromMinor(f.singapayAvailable, "IDR"),
			AccountID: accountID,
		}, nil
	}}
	f.client = NewLedgerClient(db, f.gw, testLogger())
	return f
}

// sale books a paid sale through the real repositories and the real bookSettlement.
func (f *pgCardFeeFixture) sale(invoice, channel string, sellerPrice, realFeeMinor int64) *domain.ProductTransaction {
	f.t.Helper()
	f.sales++
	fee := f.fees.GetFeeBreakdown(sellerPrice, channel, domain.CurrencyIDR)
	tx := domain.NewProductTransaction("buyer", f.seller.UUID, "product-"+invoice, "BOOKING", invoice, fee, nil)
	require.NoError(f.t, f.repos.ProductTransaction().Save(f.ctx, tx))
	require.NoError(f.t, f.repos.PaymentRequest().Save(f.ctx, domain.NewPaymentRequest(
		tx.UUID, "req-"+invoice, channel, fee.TotalCharged, domain.CurrencyIDR, time.Now().Add(time.Hour))))

	payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(f.t, f.repos.Journal().Save(f.ctx, payment))
	require.NoError(f.t, f.repos.LedgerEntry().SaveBatch(f.ctx, domain.NewPaymentEntries(
		payment.UUID, tx.UUID, f.seller.UUID, fee.SellerNetAmount, f.platform.UUID, fee.PlatformFee, f.gateway.UUID, fee.GatewayFee)))
	moved, err := f.repos.ProductTransaction().UpdateStatusIf(f.ctx, tx.UUID,
		domain.TransactionStatusPending, domain.TransactionStatusCompleted, time.Now().Add(time.Duration(f.sales)*time.Second))
	require.NoError(f.t, err)
	require.True(f.t, moved)
	tx.Status = domain.TransactionStatusCompleted

	gross := domain.RupiahToMinor(fee.TotalCharged)
	settled := domain.SettledTransaction{PaymentChannel: channel, GrossMinor: gross, FeeMinor: realFeeMinor, FeeReported: true}
	if channel == ChannelCreditCard {
		settled.FeeMinor = domain.RupiahToMinor(fee.GatewayFee)
		settled.FeeReported = false
	}
	settled.NetMinor = gross - settled.FeeMinor
	outcome, err := f.client.bookSettlement(f.ctx, tx, settled)
	require.NoError(f.t, err)
	require.Equal(f.t, settleOutcomeSettled, outcome)

	f.singapayAvailable += gross - realFeeMinor
	return tx
}

func (f *pgCardFeeFixture) card(invoice string, sellerPrice, noise int64) *domain.ProductTransaction {
	fee := f.fees.GetFeeBreakdown(sellerPrice, ChannelCreditCard, domain.CurrencyIDR)
	return f.sale(invoice, ChannelCreditCard, sellerPrice, cardFee(fee.TotalCharged, noise))
}

// payout reserves a payout the way withdrawFrom does; completed also debits Singapay.
func (f *pgCardFeeFixture) payout(amount int64, completed bool) {
	f.t.Helper()
	d, err := domain.NewDisbursement(f.seller.UUID, amount, domain.CurrencyIDR,
		domain.BankAccount{BankCode: "CENAIDJA", AccountNumber: "1234567890", AccountName: "Nino Proton"}, "withdrawal")
	require.NoError(f.t, err)
	if completed {
		d.Status = domain.DisbursementStatusCompleted
		f.singapayAvailable -= domain.RupiahToMinor(amount)
	}
	require.NoError(f.t, f.repos.Disbursement().Save(f.ctx, d))
	journal := domain.NewJournal(domain.EventTypeDisbursement, domain.SourceTypeDisbursement, d.UUID, nil)
	require.NoError(f.t, f.repos.Journal().Save(f.ctx, journal))
	// SaveBatch, not Save: Save sends empty metadata as '' rather than NULL, which PostgreSQL
	// refuses as JSON unless the connection uses binary_parameters, as the service's does.
	require.NoError(f.t, f.repos.LedgerEntry().SaveBatch(f.ctx, []*domain.LedgerEntry{
		domain.NewDisbursementEntry(journal.UUID, d.UUID, f.seller.UUID, amount)}))
}

type pgFeeRow struct {
	source                         sql.NullString
	platformFee, gatewayFee, resid sql.NullInt64
}

func (f *pgCardFeeFixture) feeRow(tx *domain.ProductTransaction) pgFeeRow {
	f.t.Helper()
	var row pgFeeRow
	require.NoError(f.t, f.db.QueryRow(
		`SELECT settled_fee_source, settled_platform_fee, settled_gateway_fee, platform_residual FROM product_transactions WHERE uuid = $1`,
		tx.UUID).Scan(&row.source, &row.platformFee, &row.gatewayFee, &row.resid))
	return row
}

func (f *pgCardFeeFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(query, args...).Scan(&n))
	return n
}

func (f *pgCardFeeFixture) reconcile() *CardPlatformFeeReconciliationResult {
	f.t.Helper()
	result, err := f.client.ReconcileCardPlatformFees(f.ctx, CardPlatformFeeReconciliationOptions{})
	require.NoError(f.t, err)
	return result
}

func (f *pgCardFeeFixture) owed() []string {
	f.t.Helper()
	rows, err := f.repos.ProductTransaction().GetSettledWithoutPlatformFeeTransfer(f.ctx, 50)
	require.NoError(f.t, err)
	var invoices []string
	for _, tx := range rows {
		invoices = append(invoices, tx.InvoiceNumber)
	}
	return invoices
}

func TestReconcileCardPlatformFees_Postgres(t *testing.T) {
	f := newPgCardFeeFixture(t)

	swimlq := f.card("INV-20260925161538-SWIMLQ", 60000, 1)
	qrisFee := f.fees.GetFeeBreakdown(40000, "QRIS", domain.CurrencyIDR)
	qris := f.sale("INV-QRIS", "QRIS", 40000, domain.RupiahToMinor(qrisFee.GatewayFee)-16)
	f.payout(100000, true)

	require.Equal(t, "ESTIMATE", f.feeRow(swimlq).source.String)
	require.Equal(t, "GATEWAY", f.feeRow(qris).source.String)
	assert.Equal(t, []string{"INV-QRIS"}, f.owed(), "the sweep leaves the estimate alone")

	// Singapay holds the seller's nothing, the QRIS fee and SWIMLQ's 3999.76.
	require.Equal(t, int64(400016+399976), f.singapayAvailable)

	result := f.reconcile()
	require.Equal(t, 1, result.Applied, "%+v", result.Sellers)
	assert.Equal(t, int64(400016), result.Sellers[0].CertainUnsweptPlatformFeeMinor)

	row := f.feeRow(swimlq)
	assert.Equal(t, "BALANCE", row.source.String)
	assert.Equal(t, int64(399976), row.platformFee.Int64)
	assert.Equal(t, int64(404124), row.gatewayFee.Int64)
	assert.Equal(t, int64(76), row.resid.Int64)
	assert.ElementsMatch(t, []string{"INV-QRIS", "INV-20260925161538-SWIMLQ"}, f.owed())

	// The platform's FEE_ADJUSTMENT went through SaveBatch: running balance and cached column.
	var amount, balanceAfter, cached, summed int64
	require.NoError(t, f.db.QueryRow(`
		SELECT le.amount, le.balance_after, a.available_balance,
		       (SELECT SUM(x.amount) FROM ledger_entries x WHERE x.account_uuid = a.uuid AND x.balance_bucket = 'AVAILABLE')
		FROM ledger_entries le JOIN ledger_accounts a ON a.uuid = le.account_uuid
		WHERE le.entry_type = 'FEE_ADJUSTMENT' AND le.source_type = 'PRODUCT_TRANSACTION' AND le.source_id = $1`,
		swimlq.UUID).Scan(&amount, &balanceAfter, &cached, &summed))
	assert.Equal(t, int64(-1), amount)
	assert.Equal(t, summed, balanceAfter)
	assert.Equal(t, summed, cached)
	assert.Equal(t, int64(4000+3999), summed, "QRIS 4000 (+16 sen residual) and SWIMLQ now 3999 (+76)")

	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journals WHERE event_type = 'RECONCILIATION' AND source_type = 'PRODUCT_TRANSACTION' AND source_id = $1`, swimlq.UUID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_fee_reconciliations WHERE outcome = 'APPLIED' AND computed_platform_fee = 399976 AND seller_ledger_available = 0 AND certain_unswept_platform_fee = 400016 AND raw_balance IS NOT NULL`))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_fee_reconciliation_items WHERE product_transaction_uuid = $1 AND allocated_platform_fee = 399976 AND settled_gateway_fee = 404124 AND platform_residual = 76 AND journal_uuid IS NOT NULL AND ledger_entry_uuid IS NOT NULL`, swimlq.UUID))

	// A second round reads nothing to do.
	again := f.reconcile()
	assert.Empty(t, again.Sellers)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_fee_reconciliations`))

	// A recomputed card is recomputed once: the database refuses a second applied item.
	var reconciliationUUID string
	require.NoError(t, f.db.QueryRow(`SELECT uuid FROM platform_fee_reconciliations`).Scan(&reconciliationUUID))
	item := domain.NewPlatformFeeReconciliationItem(reconciliationUUID, domain.CardFeeCandidate{ProductTransactionUUID: swimlq.UUID, PlatformFee: 4000, GatewayFee: 4041})
	share, gw, residual := int64(399976), int64(404124), int64(76)
	item.AllocatedPlatformFeeMinor, item.SettledGatewayFeeMinor, item.PlatformResidualMinor = &share, &gw, &residual
	var journalUUID string
	require.NoError(t, f.db.QueryRow(`SELECT uuid FROM journals WHERE event_type = 'RECONCILIATION'`).Scan(&journalUUID))
	item.JournalUUID = journalUUID
	err := f.repos.PlatformFeeReconciliation().SaveItems(f.ctx, []*domain.PlatformFeeReconciliationItem{item})
	require.Error(t, err)
	assert.True(t, repo.IsUniqueViolation(err, repo.ConstraintUniqueAppliedReconciliationItem), "%v", err)
}

// A held round satisfies the tables' CHECKs: a reason, NULL results, card_count > 0.
func TestReconcileCardPlatformFees_PostgresHold(t *testing.T) {
	f := newPgCardFeeFixture(t)
	card := f.card("INV-CARD", 60000, 1)
	f.payout(10000, false) // PENDING

	result := f.reconcile()
	require.Equal(t, 1, result.Held, "%+v", result.Sellers)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_fee_reconciliations WHERE outcome = 'HELD' AND hold_reason = 'DISBURSEMENT_IN_FLIGHT' AND singapay_available IS NULL AND computed_platform_fee IS NULL AND card_count = 1`))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_fee_reconciliation_items WHERE product_transaction_uuid = $1 AND allocated_platform_fee IS NULL AND journal_uuid IS NULL`, card.UUID))
	assert.Equal(t, "ESTIMATE", f.feeRow(card).source.String)
	assert.Empty(t, f.owed())
}

// hookedTxProvider runs before() once, just before the first transaction begins.
type hookedTxProvider struct {
	repo.TransactionProvider
	before func()
}

func (h *hookedTxProvider) Transact(ctx context.Context, fn func(tx repo.Tx) error) error {
	if h.before != nil {
		before := h.before
		h.before = nil
		before()
	}
	return h.TransactionProvider.Transact(ctx, fn)
}

// The second card changes between the round's last read and its write. The first card's
// conditional update has already succeeded inside the transaction; the rollback has to undo
// it, and nothing of the round may remain.
func TestReconcileCardPlatformFees_PostgresRollsBackTheWholeSeller(t *testing.T) {
	f := newPgCardFeeFixture(t)
	first := f.card("INV-FIRST", 60000, 1)
	second := f.card("INV-SECOND", 75000, 0)

	f.client.txProvider = &hookedTxProvider{
		TransactionProvider: f.client.txProvider,
		before: func() {
			_, err := f.db.Exec(`UPDATE product_transactions SET settled_platform_fee = settled_platform_fee - 1 WHERE uuid = $1`, second.UUID)
			require.NoError(t, err)
		},
	}

	result := f.reconcile()
	require.Equal(t, 1, result.Skipped, "%+v", result.Sellers)
	assert.Contains(t, result.Sellers[0].Detail, "INV-SECOND")

	assert.Equal(t, "ESTIMATE", f.feeRow(first).source.String, "the first card's update was rolled back")
	assert.Equal(t, int64(400000), f.feeRow(first).platformFee.Int64)
	assert.Zero(t, f.count(`SELECT count(*) FROM platform_fee_reconciliations`))
	assert.Zero(t, f.count(`SELECT count(*) FROM platform_fee_reconciliation_items`))
	assert.Zero(t, f.count(`SELECT count(*) FROM journals WHERE event_type = 'RECONCILIATION'`))
	assert.Zero(t, f.count(`SELECT count(*) FROM ledger_entries WHERE entry_type = 'FEE_ADJUSTMENT'`))
}
