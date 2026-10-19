package repo

import (
	"errors"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/lib/pq"
)

var (
	ErrNotFound        = ledgererr.NewError(ledgererr.CodeNotFound, "record not found", nil)
	ErrFailedScanSQL   = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to scan record", nil)
	ErrFailedInsertSQL = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to insert record", nil)
	ErrFailedUpdateSQL = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to update record", nil)
	ErrFailedDeleteSQL = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to delete record", nil)
	ErrFailedQuerySQL  = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to query records", nil)
)

// IsUniqueViolation reports whether err is a write refused by the named unique index or
// constraint (SQLSTATE 23505), wherever it sits in the chain: the repositories wrap the
// driver's error in their own. Named, because a table has several: a row refused for a
// duplicate Singapay identifier means something else than one refused for its owner.
func IsUniqueViolation(err error, constraint string) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == constraint
}

// The unique indexes on ledger_accounts that decide "this account already exists".
const (
	ConstraintUniqueSeller         = "idx_accounts_unique_seller"
	ConstraintUniquePaymentGateway = "idx_accounts_unique_payment_gateway"
)
