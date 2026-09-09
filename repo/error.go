package repo

import "github.com/Aturjadwal/singapay-ledger/ledgererr"

var (
	ErrNotFound        = ledgererr.NewError(ledgererr.CodeNotFound, "record not found", nil)
	ErrFailedScanSQL   = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to scan record", nil)
	ErrFailedInsertSQL = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to insert record", nil)
	ErrFailedUpdateSQL = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to update record", nil)
	ErrFailedDeleteSQL = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to delete record", nil)
	ErrFailedQuerySQL  = ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to query records", nil)
)
