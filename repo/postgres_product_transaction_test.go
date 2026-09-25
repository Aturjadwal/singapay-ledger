package repo

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

// productTransactionColumns is the SELECT list every product transaction query reads, in
// the order scanRow scans it.
var productTransactionColumns = []string{
	"uuid", "randid", "buyer_account_id", "seller_account_id", "product_id", "product_type", "invoice_number",
	"seller_price", "platform_fee", "gateway_fee", "total_charged", "seller_net_amount", "fee_model", "currency",
	"status", "created_at", "updated_at", "completed_at", "settled_at",
	"platform_fee_transferred", "platform_fee_transferred_at", "transfer_request_id", "metadata",
	"settled_platform_fee", "settled_gateway_fee", "platform_residual",
}

// platformIncomeColumns is that list followed by the one column GetPlatformIncomes adds.
var platformIncomeColumns = append(append([]string{}, productTransactionColumns...), "platform_amount")

// scanRow reads every product transaction the money path touches — the payment webhook
// finds its transaction with GetByInvoiceNumber — and it now delegates to scanRowWith. With
// no extra columns it must read exactly what it read before, field for field. Every column
// here holds a distinct, non-null value, so a destination shifted by one position fails.
func TestGetByInvoiceNumber_ScansEveryColumnIntoItsField(t *testing.T) {
	repo, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	created := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	updated := created.Add(1 * time.Minute)
	completed := created.Add(2 * time.Minute)
	settled := created.Add(3 * time.Minute)
	transferred := created.Add(4 * time.Minute)

	mock.ExpectQuery(`(?s)SELECT uuid, randid.*FROM product_transactions\s+WHERE invoice_number = \$1`).
		WithArgs("INV-42").
		WillReturnRows(sqlmock.NewRows(productTransactionColumns).AddRow(
			"pt-42", "rand-42", "buyer-42", "seller-42", "product-42", "SERVICE", "INV-42",
			int64(100_000), int64(1_000), int64(2_000), int64(103_000), int64(98_000), "GATEWAY_ON_SELLER", "IDR",
			"SETTLED", created, updated, completed, settled,
			true, transferred, "ref-42", []byte(`{"booking_uuid":"b-42"}`),
			int64(99_850), int64(2_150), int64(50),
		))

	tx, err := repo.GetByInvoiceNumber(context.Background(), "INV-42")

	require.NoError(t, err)
	assert.Equal(t, "pt-42", tx.UUID)
	assert.Equal(t, "rand-42", tx.RandId)
	assert.Equal(t, "buyer-42", tx.BuyerAccountID)
	assert.Equal(t, "seller-42", tx.SellerAccountID)
	assert.Equal(t, "product-42", tx.ProductID)
	assert.Equal(t, "SERVICE", tx.ProductType)
	assert.Equal(t, "INV-42", tx.InvoiceNumber)
	assert.Equal(t, domain.FeeBreakdown{
		SellerPrice:     100_000,
		PlatformFee:     1_000,
		GatewayFee:      2_000,
		TotalCharged:    103_000,
		SellerNetAmount: 98_000,
		FeeModel:        domain.FeeModelGatewayOnSeller,
		Currency:        domain.CurrencyIDR,
	}, tx.Fee)
	assert.Equal(t, domain.TransactionStatusSettled, tx.Status)
	assert.True(t, created.Equal(tx.CreatedAt))
	assert.True(t, updated.Equal(tx.UpdatedAt))
	require.NotNil(t, tx.CompletedAt)
	assert.True(t, completed.Equal(*tx.CompletedAt))
	require.NotNil(t, tx.SettledAt)
	assert.True(t, settled.Equal(*tx.SettledAt))
	assert.True(t, tx.PlatformFeeTransferred)
	require.NotNil(t, tx.PlatformFeeTransferredAt)
	assert.True(t, transferred.Equal(*tx.PlatformFeeTransferredAt))
	assert.Equal(t, "ref-42", tx.TransferRequestID)
	assert.Equal(t, "b-42", tx.Metadata["booking_uuid"])
	require.NotNil(t, tx.SettledPlatformFeeMinor)
	assert.Equal(t, int64(99_850), *tx.SettledPlatformFeeMinor)
	require.NotNil(t, tx.SettledGatewayFeeMinor)
	assert.Equal(t, int64(2_150), *tx.SettledGatewayFeeMinor)
	require.NotNil(t, tx.PlatformResidualMinor)
	assert.Equal(t, int64(50), *tx.PlatformResidualMinor)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func platformIncomeRow(uuid, sellerAccountID string, platformFee int64, status string, completedAt time.Time, platformAmount int64) []driver.Value {
	return []driver.Value{
		uuid, "randid-" + uuid, "buyer-1", sellerAccountID, "product-1", "SERVICE", "INV-" + uuid,
		int64(100_000), platformFee, int64(2_000), int64(102_000 + platformFee), int64(100_000), "GATEWAY_ON_CUSTOMER", "IDR",
		status, completedAt.Add(-time.Minute), completedAt, completedAt, nil,
		false, nil, nil, []byte(`{"booking_uuid":"b-1"}`),
		nil, nil, nil,
		platformAmount,
	}
}

func newMockProductTransactionRepo(t *testing.T) (*PostgresProductTransactionRepository, sqlmock.Sqlmock, func()) {
	t.Helper()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	return NewPostgresProductTransactionRepository(db), mock, func() { db.Close() }
}

// The statement's income side: paid rows only, the platform's own sales and anything with a
// platform fee, each carrying the sum of the platform's entries for it — which is the
// number a line shows, so it has to come out of the scan intact.
func TestGetPlatformIncomes_ReadsEachSaleWithThePlatformsCredit(t *testing.T) {
	repo, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	paidAt := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT pt\.uuid.*`+
		regexp.QuoteMeta(`(SELECT COALESCE(SUM(le.amount), 0)`)+`\s+FROM ledger_entries le\s+WHERE le\.account_uuid = \$1\s+`+
		`AND le\.source_type = 'PRODUCT_TRANSACTION'\s+AND le\.source_id = pt\.uuid\) AS platform_amount\s+`+
		`FROM product_transactions pt\s+WHERE pt\.status IN \('COMPLETED', 'SETTLED'\)\s+`+
		regexp.QuoteMeta(`AND (pt.seller_account_id = $1`)+`.*`+
		regexp.QuoteMeta(`ORDER BY COALESCE(pt.completed_at, pt.created_at) DESC, pt.uuid COLLATE "C" DESC`)+`\s+LIMIT \$2`).
		WithArgs("platform-acc", 3).
		WillReturnRows(sqlmock.NewRows(platformIncomeColumns).
			AddRow(platformIncomeRow("pt-2", "seller-acc", 1_000, "SETTLED", paidAt, 998)...).
			AddRow(platformIncomeRow("pt-1", "platform-acc", 0, "COMPLETED", paidAt.Add(-time.Hour), 50_000)...))

	incomes, err := repo.GetPlatformIncomes(context.Background(), "platform-acc", nil, 3, false)

	require.NoError(t, err)
	require.Len(t, incomes, 2)

	assert.Equal(t, "pt-2", incomes[0].Transaction.UUID)
	assert.Equal(t, int64(998), incomes[0].PlatformAmount)
	assert.Equal(t, int64(1_000), incomes[0].Transaction.Fee.PlatformFee)
	assert.Equal(t, domain.TransactionStatusSettled, incomes[0].Transaction.Status)
	require.NotNil(t, incomes[0].Transaction.CompletedAt)
	assert.True(t, paidAt.Equal(*incomes[0].Transaction.CompletedAt))
	assert.Equal(t, "b-1", incomes[0].Transaction.Metadata["booking_uuid"])

	assert.Equal(t, "platform-acc", incomes[1].Transaction.SellerAccountID)
	assert.Equal(t, int64(50_000), incomes[1].PlatformAmount)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// A later page continues strictly past the cursor on the same key the rows are ordered by —
// the payment time with created_at as its fallback, then uuid byte-wise.
func TestGetPlatformIncomes_ContinuesPastTheCursorInSortDirection(t *testing.T) {
	cursor := &domain.KeysetCursor{At: time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), ID: "pt-9"}

	cases := []struct {
		ascending bool
		predicate string
		order     string
	}{
		{false,
			`(COALESCE(pt.completed_at, pt.created_at), pt.uuid COLLATE "C") < ($2::timestamp, $3::varchar)`,
			`ORDER BY COALESCE(pt.completed_at, pt.created_at) DESC, pt.uuid COLLATE "C" DESC`},
		{true,
			`(COALESCE(pt.completed_at, pt.created_at), pt.uuid COLLATE "C") > ($2::timestamp, $3::varchar)`,
			`ORDER BY COALESCE(pt.completed_at, pt.created_at) ASC, pt.uuid COLLATE "C" ASC`},
	}

	for _, tc := range cases {
		repo, mock, closeDB := newMockProductTransactionRepo(t)

		mock.ExpectQuery(`(?s)`+regexp.QuoteMeta(tc.predicate)+`\s+`+regexp.QuoteMeta(tc.order)+`\s+LIMIT \$4`).
			WithArgs("platform-acc", cursor.At, cursor.ID, 5).
			WillReturnRows(sqlmock.NewRows(platformIncomeColumns))

		incomes, err := repo.GetPlatformIncomes(context.Background(), "platform-acc", cursor, 5, tc.ascending)

		require.NoError(t, err, "ascending=%v", tc.ascending)
		assert.Empty(t, incomes)
		assert.NoError(t, mock.ExpectationsWereMet(), "ascending=%v", tc.ascending)
		closeDB()
	}
}
