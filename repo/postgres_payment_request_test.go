package repo

import (
	"context"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMockPaymentRequestRepo(t *testing.T) (*PostgresPaymentRequestRepository, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	return NewPostgresPaymentRequestRepository(db), mock
}

// The identifiers settlement found go through the one conditional statement, trimmed, and
// the answer is whether a row changed.
func TestRecordGatewayTransaction_WritesThroughTheConditionalUpdate(t *testing.T) {
	repo, mock := newMockPaymentRequestRepo(t)

	mock.ExpectExec("UPDATE payment_requests SET").
		WithArgs("pr-001", "80937", "18917720251110094037705", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	changed, err := repo.RecordGatewayTransaction(context.Background(), "pr-001", " 80937 ", "18917720251110094037705")

	require.NoError(t, err)
	assert.True(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// A row that already holds valid identifiers is not matched by the statement's WHERE, so
// nothing changes — which is an answer, not an error.
func TestRecordGatewayTransaction_ARowWithNothingToFillIsNoChange(t *testing.T) {
	repo, mock := newMockPaymentRequestRepo(t)

	mock.ExpectExec("UPDATE payment_requests SET").
		WithArgs("pr-001", "80937", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	changed, err := repo.RecordGatewayTransaction(context.Background(), "pr-001", "80937", "")

	require.NoError(t, err)
	assert.False(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// An id that is not a positive integer is no id. With no reference beside it there is
// nothing to write, and the database is not asked.
func TestRecordGatewayTransaction_NoUsableIdentifierWritesNothing(t *testing.T) {
	repo, mock := newMockPaymentRequestRepo(t)

	for _, id := range []string{"", "0", " 0 ", "-5", "007", "VA-ABC-1"} {
		changed, err := repo.RecordGatewayTransaction(context.Background(), "pr-001", id, "  ")

		require.NoError(t, err, "id %q", id)
		assert.False(t, changed, "id %q", id)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

// go-sqlmock never parses SQL, so the conditions the statement exists for would pass every
// test above if they were deleted. They are checked here as text; their behaviour was
// checked against PostgreSQL when they were written.
func TestRecordGatewayTransactionSQL_KeepsItsConditions(t *testing.T) {
	statement := strings.Join(strings.Fields(recordGatewayTransactionSQL), " ")

	assert.Contains(t, statement, "WHERE uuid = $1 AND (")
	assert.Contains(t, statement, "($2::text <> '' AND COALESCE(gateway_transaction_id, '') !~ '^[1-9][0-9]*$')")
	assert.Contains(t, statement, "($3::text <> '' AND btrim(COALESCE(gateway_transaction_ref, '')) = '')")
	assert.Equal(t, 2, strings.Count(statement, "ELSE gateway_transaction_"), "each column keeps its value unless its condition holds")
}
