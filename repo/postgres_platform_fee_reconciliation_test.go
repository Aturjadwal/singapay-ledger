package repo

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

func newMockReconciliationRepo(t *testing.T) (*PostgresPlatformFeeReconciliationRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return NewPostgresPlatformFeeReconciliationRepository(db), mock
}

func int64Ptr(v int64) *int64 { return &v }

// ─────────────────────────────────────────────────────────────────────────────
// product_transactions: the settled fee source
// ─────────────────────────────────────────────────────────────────────────────

// Settlement records where the figures came from, in the same statement as the figures.
func TestSaveSettledFees_WritesTheSource(t *testing.T) {
	r, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	mock.ExpectExec(`(?s)UPDATE product_transactions\s+SET settled_platform_fee = \$1,\s+settled_gateway_fee = \$2,\s+platform_residual = \$3,\s+settled_fee_source = \$4,\s+updated_at = \$5\s+WHERE uuid = \$6`).
		WithArgs(int64(400000), int64(404100), int64(0), "ESTIMATE", sqlmock.AnyArg(), "pt-swimlq").
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, r.SaveSettledFees(context.Background(), "pt-swimlq", 400000, 404100, 0, domain.SettledFeeSourceEstimate))
	require.NoError(t, mock.ExpectationsWereMet())
}

// The compare-and-set carries every condition the reconciliation relied on, and a row that
// no longer matches is an answer (false), not an error.
func TestSaveBalancedFeesIf_IsAConditionalUpdate(t *testing.T) {
	r, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	conditional := `(?s)UPDATE product_transactions\s+SET settled_platform_fee = \$1,\s+settled_gateway_fee = \$2,\s+platform_residual = \$3,\s+settled_fee_source = 'BALANCE',\s+updated_at = \$4\s+` +
		`WHERE uuid = \$5\s+AND status = 'SETTLED'\s+AND platform_fee_transferred = false\s+AND settled_fee_source = 'ESTIMATE'\s+` +
		`AND settled_platform_fee IS NOT DISTINCT FROM \$6\s+AND settled_gateway_fee IS NOT DISTINCT FROM \$7`

	mock.ExpectExec(conditional).
		WithArgs(int64(399976), int64(404124), int64(76), sqlmock.AnyArg(), "pt-swimlq",
			sql.NullInt64{Int64: 400000, Valid: true}, sql.NullInt64{Int64: 404100, Valid: true}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	moved, err := r.SaveBalancedFeesIf(context.Background(), "pt-swimlq", int64Ptr(400000), int64Ptr(404100), 399976, 404124, 76)
	require.NoError(t, err)
	assert.True(t, moved)

	// NULL figures compare as NULL, and a row someone else moved first reports false.
	mock.ExpectExec(conditional).
		WithArgs(int64(399976), int64(404124), int64(76), sqlmock.AnyArg(), "pt-swimlq",
			sql.NullInt64{}, sql.NullInt64{}).
		WillReturnResult(sqlmock.NewResult(0, 0))
	moved, err = r.SaveBalancedFeesIf(context.Background(), "pt-swimlq", nil, nil, 399976, 404124, 76)
	require.NoError(t, err)
	assert.False(t, moved)

	require.NoError(t, mock.ExpectationsWereMet())
}

// The sweep leaves the estimate alone, and reads NULL as GATEWAY.
func TestGetSettledWithoutPlatformFeeTransfer_LeavesEstimatedFeesAlone(t *testing.T) {
	r, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	mock.ExpectQuery(`(?s)FROM product_transactions\s+WHERE status = 'SETTLED'\s+AND platform_fee_transferred = false\s+AND platform_fee > 0\s+` +
		`AND COALESCE\(settled_fee_source, 'GATEWAY'\) <> 'ESTIMATE'\s+ORDER BY settled_at ASC\s+LIMIT \$1`).
		WithArgs(25).
		WillReturnRows(sqlmock.NewRows(productTransactionColumns))

	_, err := r.GetSettledWithoutPlatformFeeTransfer(context.Background(), 25)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ─────────────────────────────────────────────────────────────────────────────
// The reconciliation's reads
// ─────────────────────────────────────────────────────────────────────────────

// The candidate rule, written once and used by both reads.
var candidateRule = regexp.QuoteMeta(`pt.status = 'SETTLED'`) + `\s+AND ` +
	regexp.QuoteMeta(`pt.platform_fee_transferred = false`) + `\s+AND ` +
	regexp.QuoteMeta(`pt.settled_fee_source = 'ESTIMATE'`) + `\s+AND ` +
	regexp.QuoteMeta(`pt.fee_model = 'GATEWAY_ON_CUSTOMER'`) + `\s+AND ` +
	regexp.QuoteMeta(`pt.platform_fee > 0`) + `\s+AND EXISTS \(SELECT 1 FROM payment_requests pr\s+WHERE pr.product_transaction_uuid = pt.uuid\s+AND pr.payment_channel = 'CREDIT_CARD'\)`

func TestSellersWithEstimatedCardFees(t *testing.T) {
	r, mock := newMockReconciliationRepo(t)

	mock.ExpectQuery(`(?s)SELECT pt.seller_account_id\s+FROM product_transactions pt\s+JOIN ledger_accounts a ON a.uuid = pt.seller_account_id\s+` +
		`WHERE a.owner_type = 'SELLER'\s+AND ` + candidateRule + `\s+GROUP BY pt.seller_account_id\s+` +
		`ORDER BY MIN\(pt.settled_at\) ASC, pt.seller_account_id ASC\s+LIMIT \$1`).
		WithArgs(100).
		WillReturnRows(sqlmock.NewRows([]string{"seller_account_id"}).AddRow("seller-old").AddRow("seller-new"))

	sellers, err := r.SellersWithEstimatedCardFees(context.Background(), 100)
	require.NoError(t, err)
	assert.Equal(t, []string{"seller-old", "seller-new"}, sellers)
	require.NoError(t, mock.ExpectationsWereMet())
}

var snapshotColumns = []string{
	"seller_available", "certain_unswept", "in_flight",
	"uuid", "invoice_number", "platform_fee", "gateway_fee", "total_charged", "seller_net_amount",
	"settled_platform_fee", "settled_gateway_fee", "platform_residual", "settled_at",
}

// One statement: the three figures, then one row per card. Every field lands where it
// belongs, and the sums are cast to bigint because SUM(bigint) is numeric.
func TestReadCardFeeSnapshot_ReadsTheFiguresAndTheCards(t *testing.T) {
	r, mock := newMockReconciliationRepo(t)
	settled := time.Date(2026, 10, 8, 14, 46, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT s.seller_available, s.certain_unswept, s.in_flight,.*` +
		regexp.QuoteMeta(`(SELECT COALESCE(SUM(le.amount), 0)::bigint`) + `\s+FROM ledger_entries le\s+WHERE le.account_uuid = \$1\s+AND le.balance_bucket = 'AVAILABLE'\) AS seller_available,.*` +
		regexp.QuoteMeta(`(SELECT COALESCE(SUM(COALESCE(u.settled_platform_fee, u.platform_fee * 100)), 0)::bigint`) + `\s+FROM product_transactions u\s+WHERE u.seller_account_id = \$1\s+` +
		`AND u.status = 'SETTLED'\s+AND u.platform_fee_transferred = false\s+AND ` + regexp.QuoteMeta(`COALESCE(u.settled_fee_source, 'GATEWAY') <> 'ESTIMATE') AS certain_unswept,`) + `.*` +
		`FROM disbursements d\s+WHERE d.account_uuid = \$1\s+AND d.status IN \('PENDING', 'PROCESSING'\)\) AS in_flight.*` +
		`LEFT JOIN LATERAL \(.*WHERE pt.seller_account_id = \$1\s+AND ` + candidateRule + `.*a.owner_type = 'SELLER'\)\s+\) c ON true\s+ORDER BY c.settled_at ASC, c.uuid ASC`).
		WithArgs("seller-1").
		WillReturnRows(sqlmock.NewRows(snapshotColumns).
			AddRow(int64(100000), int64(399976), int64(0),
				"pt-a", "INV-A", int64(4000), int64(4041), int64(68041), int64(60000),
				int64(400000), int64(404100), int64(0), settled).
			AddRow(int64(100000), int64(399976), int64(0),
				"pt-b", "INV-B", int64(4000), int64(5278), int64(109278), int64(100000),
				nil, nil, nil, nil))

	snapshot, err := r.ReadCardFeeSnapshot(context.Background(), "seller-1")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Equal(t, "seller-1", snapshot.SellerAccountID)
	assert.Equal(t, int64(100000), snapshot.SellerLedgerAvailable)
	assert.Equal(t, int64(399976), snapshot.CertainUnsweptPlatformFeeMinor)
	assert.Zero(t, snapshot.DisbursementsInFlight)
	require.Len(t, snapshot.Cards, 2)

	a := snapshot.Cards[0]
	assert.Equal(t, "pt-a", a.ProductTransactionUUID)
	assert.Equal(t, "INV-A", a.InvoiceNumber)
	assert.Equal(t, int64(4000), a.PlatformFee)
	assert.Equal(t, int64(4041), a.GatewayFee)
	assert.Equal(t, int64(68041), a.TotalCharged)
	assert.Equal(t, int64(60000), a.SellerNetAmount)
	assert.Equal(t, int64(400000), *a.SettledPlatformFeeMinor)
	assert.Equal(t, int64(404100), *a.SettledGatewayFeeMinor)
	assert.Equal(t, int64(0), *a.PlatformResidualMinor)
	require.NotNil(t, a.SettledAt)
	assert.True(t, settled.Equal(*a.SettledAt))

	b := snapshot.Cards[1]
	assert.Nil(t, b.SettledPlatformFeeMinor, "NULL stays NULL")
	assert.Nil(t, b.SettledGatewayFeeMinor)
	assert.Nil(t, b.SettledAt)
	assert.Equal(t, int64(400000), b.PreviousPlatformFeeMinor(), "and is read as the priced fee")
}

// No card: one row of NULLs, which still carries the figures.
func TestReadCardFeeSnapshot_NoCard(t *testing.T) {
	r, mock := newMockReconciliationRepo(t)

	mock.ExpectQuery(`(?s)SELECT s.seller_available`).
		WithArgs("seller-1").
		WillReturnRows(sqlmock.NewRows(snapshotColumns).
			AddRow(int64(5000), int64(0), int64(2), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	snapshot, err := r.ReadCardFeeSnapshot(context.Background(), "seller-1")
	require.NoError(t, err)
	assert.Equal(t, int64(5000), snapshot.SellerLedgerAvailable)
	assert.Equal(t, 2, snapshot.DisbursementsInFlight)
	assert.Empty(t, snapshot.Cards)
}

// ─────────────────────────────────────────────────────────────────────────────
// The reconciliation's rows
// ─────────────────────────────────────────────────────────────────────────────

func TestSaveReconciliation_WritesNullWhereNothingWasRead(t *testing.T) {
	r, mock := newMockReconciliationRepo(t)

	snapshot := &domain.CardFeeSnapshot{SellerAccountID: "seller-1", Cards: []domain.CardFeeCandidate{{PlatformFee: 4000}}}
	held := domain.NewPlatformFeeReconciliation(snapshot, "01M2MGFRBT4G9WPP3TGTQEWP54", 0)
	held.Hold(domain.HoldBalanceReadFailed)

	mock.ExpectExec(`(?s)INSERT INTO platform_fee_reconciliations`).
		WithArgs(held.UUID, held.RandId, "seller-1", "01M2MGFRBT4G9WPP3TGTQEWP54",
			sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{},
			int64(0), int64(0), sql.NullInt64{},
			int64(400000), int64(10000), 1, "HELD",
			sql.NullString{String: "BALANCE_READ_FAILED", Valid: true}, sql.NullString{}, held.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.Save(context.Background(), held))

	applied := domain.NewPlatformFeeReconciliation(snapshot, "01M2MGFRBT4G9WPP3TGTQEWP54", 0)
	applied.SingapayAvailableMinor = int64Ptr(399976)
	applied.SingapayPendingMinor = int64Ptr(0)
	applied.SingapayHeldMinor = int64Ptr(0)
	applied.ComputedPlatformFeeMinor = int64Ptr(399976)
	applied.RawBalance = []byte(`{"available_balance":3999.76}`)
	applied.Apply()

	mock.ExpectExec(`(?s)INSERT INTO platform_fee_reconciliations`).
		WithArgs(applied.UUID, applied.RandId, "seller-1", "01M2MGFRBT4G9WPP3TGTQEWP54",
			sql.NullInt64{Int64: 399976, Valid: true}, sql.NullInt64{Int64: 0, Valid: true}, sql.NullInt64{Int64: 0, Valid: true},
			int64(0), int64(0), sql.NullInt64{Int64: 399976, Valid: true},
			int64(400000), int64(10000), 1, "APPLIED",
			sql.NullString{}, sql.NullString{String: `{"available_balance":3999.76}`, Valid: true}, applied.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.Save(context.Background(), applied))

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSaveItems_OneStatementPerCard(t *testing.T) {
	r, mock := newMockReconciliationRepo(t)

	card := domain.CardFeeCandidate{
		ProductTransactionUUID:  "pt-swimlq",
		PlatformFee:             4000,
		GatewayFee:              4041,
		SettledPlatformFeeMinor: int64Ptr(400000),
		SettledGatewayFeeMinor:  int64Ptr(404100),
	}
	held := domain.NewPlatformFeeReconciliationItem("rec-held", card)
	applied := domain.NewPlatformFeeReconciliationItem("rec-applied", card)
	applied.AllocatedPlatformFeeMinor = int64Ptr(399976)
	applied.SettledGatewayFeeMinor = int64Ptr(404124)
	applied.PlatformResidualMinor = int64Ptr(76)
	applied.JournalUUID = "journal-1"
	applied.LedgerEntryUUID = "entry-1"

	insert := `(?s)INSERT INTO platform_fee_reconciliation_items`
	mock.ExpectExec(insert).
		WithArgs(held.UUID, held.RandId, "rec-held", "pt-swimlq", int64(400000), int64(404100), int64(400000),
			sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullString{}, sql.NullString{}, held.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(insert).
		WithArgs(applied.UUID, applied.RandId, "rec-applied", "pt-swimlq", int64(400000), int64(404100), int64(400000),
			sql.NullInt64{Int64: 399976, Valid: true}, sql.NullInt64{Int64: 404124, Valid: true}, sql.NullInt64{Int64: 76, Valid: true},
			sql.NullString{String: "journal-1", Valid: true}, sql.NullString{String: "entry-1", Valid: true}, applied.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, r.SaveItems(context.Background(), []*domain.PlatformFeeReconciliationItem{held, applied}))
	require.NoError(t, mock.ExpectationsWereMet())
}
