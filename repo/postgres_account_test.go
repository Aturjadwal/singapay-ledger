package repo

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/21strive/ledger/domain"
)

// accountColumns is the SELECT list accountSelectColumns produces, in order. The scan
// depends on that order, so the tests state it once — a column inserted in the middle of
// the const without updating the scan shifts every field after it, and the compiler
// cannot see that.
var accountColumns = []string{
	"uuid", "randid", "singapay_account_id", "singapay_account_number",
	"owner_type", "owner_id", "currency",
	"pending_balance", "available_balance", "total_withdrawal_amount", "total_deposit_amount",
	"created_at", "updated_at",
}

// accountRow builds one row. Both identifiers are untyped so a test can pass nil for a SQL
// NULL — which is what the PAYMENT_GATEWAY expense account looks like, and what a
// sub-account Singapay issued without a number looks like.
func accountRow(uuid string, singapayID, singapayNumber any, createdAt time.Time) []driver.Value {
	return []driver.Value{
		uuid, "randid-" + uuid, singapayID, singapayNumber,
		"SELLER", "seller-001", "IDR",
		int64(0), int64(0), int64(0), int64(0),
		createdAt, createdAt,
	}
}

func newMockAccountRepo(t *testing.T) (*PostgresAccountRepository, sqlmock.Sqlmock, func()) {
	t.Helper()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	return NewPostgresAccountRepository(db), mock, func() { db.Close() }
}

func TestScanAccountReadsBothSingapayIdentifiers(t *testing.T) {
	repo, mock, closeDB := newMockAccountRepo(t)
	defer closeDB()

	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery("SELECT").
		WithArgs("acc-001").
		WillReturnRows(sqlmock.NewRows(accountColumns).
			AddRow(accountRow("acc-001", "01K946KF851RK7FX075GJHBVKF", "000000000123", now)...))

	got, err := repo.GetByID(context.Background(), "acc-001")
	require.NoError(t, err)

	assert.Equal(t, "01K946KF851RK7FX075GJHBVKF", got.SingapayAccountID)
	assert.Equal(t, "000000000123", got.SingapayAccountNumber)
	assert.True(t, got.CanReceiveTransfer())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScanAccountHandlesMissingAccountNumber(t *testing.T) {
	repo, mock, closeDB := newMockAccountRepo(t)
	defer closeDB()

	now := time.Now().UTC().Truncate(time.Second)
	// Singapay declares account_number nullable, so this row is legitimate — and the
	// account it describes cannot be the beneficiary of a transfer.
	mock.ExpectQuery("SELECT").
		WithArgs("acc-002").
		WillReturnRows(sqlmock.NewRows(accountColumns).
			AddRow(accountRow("acc-002", "01K946KF851RK7FX075GJHBVKG", nil, now)...))

	got, err := repo.GetByID(context.Background(), "acc-002")
	require.NoError(t, err)

	assert.Equal(t, "01K946KF851RK7FX075GJHBVKG", got.SingapayAccountID)
	assert.Empty(t, got.SingapayAccountNumber)
	assert.False(t, got.CanReceiveTransfer(),
		"an account with no number cannot receive a platform-fee transfer")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetBySingapayAccountID(t *testing.T) {
	repo, mock, closeDB := newMockAccountRepo(t)
	defer closeDB()

	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery("WHERE singapay_account_id = \\$1").
		WithArgs("01K946KF851RK7FX075GJHBVKF").
		WillReturnRows(sqlmock.NewRows(accountColumns).
			AddRow(accountRow("acc-001", "01K946KF851RK7FX075GJHBVKF", "000000000123", now)...))

	got, err := repo.GetBySingapayAccountID(context.Background(), "01K946KF851RK7FX075GJHBVKF")
	require.NoError(t, err)
	assert.Equal(t, "acc-001", got.UUID)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetBySingapayAccountIDNotFound(t *testing.T) {
	repo, mock, closeDB := newMockAccountRepo(t)
	defer closeDB()

	mock.ExpectQuery("WHERE singapay_account_id = \\$1").
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows(accountColumns))

	_, err := repo.GetBySingapayAccountID(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSaveWritesEmptyIdentifiersAsNull pins the detail the partial unique indexes depend
// on. The indexes are declared WHERE ... IS NOT NULL, so NULL rows never collide — but a
// literal empty string is a value, and the second account saved without a Singapay id
// would violate uniqueness against the first.
func TestSaveWritesEmptyIdentifiersAsNull(t *testing.T) {
	repo, mock, closeDB := newMockAccountRepo(t)
	defer closeDB()

	// The PAYMENT_GATEWAY expense account is the real case for this: it is bookkeeping
	// only and has no Singapay sub-account behind it.
	account := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)

	mock.ExpectExec("INSERT INTO ledger_accounts").
		WithArgs(
			account.UUID,
			account.RandId,
			nil, // singapay_account_id
			nil, // singapay_account_number
			domain.OwnerTypePaymentGateway,
			"SINGAPAY",
			domain.CurrencyIDR,
			int64(0), int64(0), int64(0), int64(0),
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	require.NoError(t, repo.Save(context.Background(), &account))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSaveWritesSingapayIdentifiers(t *testing.T) {
	repo, mock, closeDB := newMockAccountRepo(t)
	defer closeDB()

	account := domain.NewSellerAccount("01K946KF851RK7FX075GJHBVKF", "seller-002", domain.CurrencyIDR)
	account.SetSingapayAccount("01K946KF851RK7FX075GJHBVKF", "000000000123")

	mock.ExpectExec("INSERT INTO ledger_accounts").
		WithArgs(
			account.UUID,
			account.RandId,
			"01K946KF851RK7FX075GJHBVKF",
			"000000000123",
			domain.OwnerTypeSeller,
			"seller-002",
			domain.CurrencyIDR,
			int64(0), int64(0), int64(0), int64(0),
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	require.NoError(t, repo.Save(context.Background(), &account))
	require.NoError(t, mock.ExpectationsWereMet())
}
