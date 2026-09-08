package repo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/21strive/ledger/domain"
)

// disbursementColumns is the SELECT list every disbursement query in this file uses, in
// order. The scan depends on that order, so the tests state it once.
var disbursementColumns = []string{
	"uuid", "randid", "account_uuid", "amount", "currency", "status",
	"bank_code", "account_number", "account_name",
	"description", "external_transaction_id", "failure_reason",
	"payout_request_id", "gateway_fee", "created_at", "updated_at", "processed_at",
}

// disbursementRow builds one row. failureReason and payoutRequestID are untyped so a
// test can pass nil for a SQL NULL.
func disbursementRow(uuid, status string, failureReason, payoutRequestID any, createdAt time.Time) []driver.Value {
	return []driver.Value{
		uuid, "randid-" + uuid, "acc-001", int64(250_000), "IDR", status,
		"014", "1234567890", "Seller Name",
		"Disbursement request", nil, failureReason,
		payoutRequestID, int64(0), createdAt, createdAt, nil,
	}
}

func newMockRepo(t *testing.T) (*PostgresDisbursementRepository, sqlmock.Sqlmock, func()) {
	t.Helper()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	return NewPostgresDisbursementRepository(db), mock, func() { db.Close() }
}

func rowsFrom(values ...[]driver.Value) *sqlmock.Rows {
	rows := sqlmock.NewRows(disbursementColumns)
	for _, v := range values {
		rows.AddRow(v...)
	}
	return rows
}

// A payout stuck at PENDING after a timeout carries no failure reason: recordPayoutFailure
// leaves the row untouched on an unknown outcome, on purpose. Its request id must still be
// read, because that id is the only thing that makes the retry safe.
//
// While the two columns were read together, RetryDisbursement saw an empty id on exactly
// these rows and refused every one of them as predating idempotent retries — which made
// the whole retry path unusable for the case it was built for.
func TestGetByID_ReadsPayoutRequestIDWithoutFailureReason(t *testing.T) {
	repo, mock, closeDB := newMockRepo(t)
	defer closeDB()

	createdAt := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery("(?s)SELECT.*FROM disbursements.*WHERE uuid").
		WithArgs("d-001").
		WillReturnRows(rowsFrom(disbursementRow("d-001", "PENDING", nil, "req-001", createdAt)))

	disbursement, err := repo.GetByID(context.Background(), "d-001")

	require.NoError(t, err)
	assert.Equal(t, "req-001", disbursement.PayoutRequestID)
	assert.Empty(t, disbursement.FailureReason)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByID_LeavesPayoutRequestIDEmptyWhenNull(t *testing.T) {
	repo, mock, closeDB := newMockRepo(t)
	defer closeDB()

	createdAt := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery("(?s)SELECT.*FROM disbursements.*WHERE uuid").
		WithArgs("d-old").
		WillReturnRows(rowsFrom(disbursementRow("d-old", "PENDING", nil, nil, createdAt)))

	disbursement, err := repo.GetByID(context.Background(), "d-old")

	// A row from before migration 014. It must stay empty so RetryDisbursement refuses
	// it rather than inventing a reference Singapay has never seen.
	require.NoError(t, err)
	assert.Empty(t, disbursement.PayoutRequestID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByID_ReadsBothColumnsWhenBothPresent(t *testing.T) {
	repo, mock, closeDB := newMockRepo(t)
	defer closeDB()

	createdAt := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery("(?s)SELECT.*FROM disbursements.*WHERE uuid").
		WithArgs("d-failed").
		WillReturnRows(rowsFrom(disbursementRow("d-failed", "FAILED", "Singapay refused the payout", "req-002", createdAt)))

	disbursement, err := repo.GetByID(context.Background(), "d-failed")

	require.NoError(t, err)
	assert.Equal(t, "req-002", disbursement.PayoutRequestID)
	assert.Equal(t, "Singapay refused the payout", disbursement.FailureReason)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByID_NotFound(t *testing.T) {
	repo, mock, closeDB := newMockRepo(t)
	defer closeDB()

	mock.ExpectQuery("(?s)SELECT.*FROM disbursements.*WHERE uuid").
		WithArgs("d-missing").
		WillReturnError(sql.ErrNoRows)

	disbursement, err := repo.GetByID(context.Background(), "d-missing")

	assert.Nil(t, disbursement)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetPendingOlderThan(t *testing.T) {
	repo, mock, closeDB := newMockRepo(t)
	defer closeDB()

	cutoff := time.Date(2026, 8, 30, 11, 45, 0, 0, time.UTC)
	older := cutoff.Add(-3 * time.Hour)
	old := cutoff.Add(-2 * time.Hour)

	mock.ExpectQuery("(?s)SELECT.*FROM disbursements.*WHERE status = .* AND created_at < .*ORDER BY created_at ASC.*LIMIT").
		WithArgs(domain.DisbursementStatusPending, cutoff, 10).
		WillReturnRows(rowsFrom(
			disbursementRow("d-001", "PENDING", nil, "req-001", older),
			// No failure reason here either — the sweep's whole population looks like this.
			disbursementRow("d-002", "PENDING", nil, "req-002", old),
		))

	disbursements, err := repo.GetPendingOlderThan(context.Background(), cutoff, 10)

	require.NoError(t, err)
	require.Len(t, disbursements, 2)
	assert.Equal(t, "d-001", disbursements[0].UUID)
	assert.Equal(t, "req-001", disbursements[0].PayoutRequestID)
	assert.Equal(t, "req-002", disbursements[1].PayoutRequestID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetPendingOlderThan_SurfacesRowsWithoutARequestID(t *testing.T) {
	repo, mock, closeDB := newMockRepo(t)
	defer closeDB()

	cutoff := time.Date(2026, 8, 30, 11, 45, 0, 0, time.UTC)

	mock.ExpectQuery("(?s)SELECT.*FROM disbursements.*WHERE status = .* AND created_at <").
		WithArgs(domain.DisbursementStatusPending, cutoff, 10).
		WillReturnRows(rowsFrom(disbursementRow("d-old", "PENDING", nil, nil, cutoff.Add(-time.Hour))))

	disbursements, err := repo.GetPendingOlderThan(context.Background(), cutoff, 10)

	// Returned, not filtered in SQL: a row that cannot be replayed still needs to be
	// visible to whoever is deciding what to settle by hand.
	require.NoError(t, err)
	require.Len(t, disbursements, 1)
	assert.Empty(t, disbursements[0].PayoutRequestID)
	assert.NoError(t, mock.ExpectationsWereMet())
}
