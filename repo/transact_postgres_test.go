package repo

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TransactReadCommitted runs READ COMMITTED even where the connection's default is REPEATABLE
// READ, and Transact keeps the default. Against PostgreSQL, when LEDGER_TEST_DATABASE_URL names
// a database (README_TESTING.md); nothing is written.
func TestTransactReadCommittedOverridesTheDefaultIsolation(t *testing.T) {
	dsn := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set LEDGER_TEST_DATABASE_URL to run against PostgreSQL (README_TESTING.md)")
	}
	db, err := sql.Open("postgres", dsn+" default_transaction_isolation='repeatable read'")
	require.NoError(t, err)
	defer db.Close()
	var isolation string
	require.NoError(t, db.QueryRow(`SHOW default_transaction_isolation`).Scan(&isolation))
	if isolation != "repeatable read" {
		t.Skipf("default_transaction_isolation is %q: this pooler does not pass run-time parameters", isolation)
	}

	provider := NewTransactionProvider(db)
	isolationIn := func(run func(context.Context, func(Tx) error) error) string {
		var got string
		require.NoError(t, run(context.Background(), func(tx Tx) error {
			return tx.(*postgresTx).tx.QueryRow(`SHOW transaction_isolation`).Scan(&got)
		}))
		return got
	}
	assert.Equal(t, "read committed", isolationIn(provider.TransactReadCommitted))
	assert.Equal(t, "repeatable read", isolationIn(provider.Transact), "Transact keeps the connection's default")
}
