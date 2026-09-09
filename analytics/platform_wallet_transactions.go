package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// PlatformWalletTransactionRow represents one ledger row shown in platform wallet transaction list.
type PlatformWalletTransactionRow struct {
	LedgerEntryUUID string    `json:"ledger_entry_uuid"`
	CreatedAt       time.Time `json:"created_at"`
	Amount          int64     `json:"amount"`
	BalanceBucket   string    `json:"balance_bucket"`
	EntryType       string    `json:"entry_type"`
	SourceType      string    `json:"source_type"`
	SourceID        string    `json:"source_id"`
	BalanceAfter    int64     `json:"balance_after"`
	InvoiceNumber   *string   `json:"invoice_number,omitempty"`
}

// GetPlatformWalletTransactions returns ledger transactions for PLATFORM account,
// and references invoice_number when source_type is PRODUCT_TRANSACTION.
func (c *LedgerAnalyticsClient) GetPlatformWalletTransactions(
	ctx context.Context,
	limit int,
	offset int,
) ([]PlatformWalletTransactionRow, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		return nil, ledgererr.ErrInvalidLimit.WithError(fmt.Errorf("max 200, got %d", limit))
	}
	if offset < 0 {
		return nil, ledgererr.ErrInvalidOffset.WithError(fmt.Errorf("must be >= 0, got %d", offset))
	}

	query := `
SELECT
  le.uuid,
  le.created_at,
  le.amount,
  le.balance_bucket,
  le.entry_type,
  le.source_type,
  le.source_id,
  le.balance_after,
  pt.invoice_number
FROM ledger_entries le
JOIN ledger_accounts la
  ON la.uuid = le.account_uuid
 AND la.owner_type = 'PLATFORM'
LEFT JOIN product_transactions pt
  ON le.source_type = 'PRODUCT_TRANSACTION'
 AND le.source_id = pt.uuid
ORDER BY le.created_at DESC
LIMIT $1 OFFSET $2;`

	rows, err := c.ledgerDB.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}
	defer rows.Close()

	result := make([]PlatformWalletTransactionRow, 0)
	for rows.Next() {
		var row PlatformWalletTransactionRow
		if err := rows.Scan(
			&row.LedgerEntryUUID,
			&row.CreatedAt,
			&row.Amount,
			&row.BalanceBucket,
			&row.EntryType,
			&row.SourceType,
			&row.SourceID,
			&row.BalanceAfter,
			&row.InvoiceNumber,
		); err != nil {
			return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
		}
		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}

	return result, nil
}
