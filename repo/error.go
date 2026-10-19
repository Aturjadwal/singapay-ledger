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

// IsUniqueViolation reports whether err is a write refused by a unique index or
// constraint (SQLSTATE 23505), wherever it sits in the chain: the repositories wrap the
// driver's error in their own.
func IsUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}
