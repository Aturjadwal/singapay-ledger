package analytics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// UserWalletRow represents one seller row in the user wallets table.
type UserWalletRow struct {
	AccountID               string     `json:"account_id"`
	OwnerID                 string     `json:"owner_id"`
	OwnerType               string     `json:"owner_type"`
	CurrentAvailableBalance int64      `json:"current_available_balance"`
	CurrentPendingBalance   int64      `json:"current_pending_balance"`
	TotalEarnings           int64      `json:"total_earnings"`
	TotalWithdrawn          int64      `json:"total_withdrawn"`
	AccountStatus           *string    `json:"account_status,omitempty"`
	UpdatedAt               *time.Time `json:"updated_at,omitempty"`
	HasPendingBalance       bool       `json:"has_pending_balance"`
	HasAvailableBalance     bool       `json:"has_available_balance"`
}

// GetUserWallets returns the seller wallet list for the user wallets page.
// It follows the analytics spec: join fact_user_accumulation with dim_account,
// filter current seller rows, optional search by owner_id, and sort by available balance.
func (c *LedgerAnalyticsClient) GetUserWallets(
	ctx context.Context,
	search string,
	limit int,
	offset int,
) ([]UserWalletRow, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		return nil, ledgererr.ErrInvalidLimit.WithError(fmt.Errorf("max 200, got %d", limit))
	}
	if offset < 0 {
		return nil, ledgererr.ErrInvalidOffset.WithError(fmt.Errorf("must be >= 0, got %d", offset))
	}

	search = strings.TrimSpace(search)
	if search != "" && !strings.HasPrefix(search, "%") {
		search = "%" + search + "%"
	}

	query := `
SELECT
  da.account_id,
  da.owner_id,
  da.owner_type,
  fua.current_available_balance,
  fua.current_pending_balance,
  fua.total_earnings,
  fua.total_withdrawn,
  fua.account_status,
  fua.updated_at,
  fua.has_pending_balance,
  fua.has_available_balance
FROM fact_user_accumulation fua
JOIN dim_account da
  ON fua.dim_account_uuid = da.uuid
WHERE da.is_current = TRUE
  AND da.owner_type = 'SELLER'
  AND ($1 = '' OR da.owner_id ILIKE $1)
ORDER BY fua.current_available_balance DESC, fua.updated_at DESC
LIMIT $2 OFFSET $3;`

	rows, err := c.ledgerAnalyticsDB.QueryContext(ctx, query, search, limit, offset)
	if err != nil {
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}
	defer rows.Close()

	result := make([]UserWalletRow, 0)
	for rows.Next() {
		var row UserWalletRow
		if err := rows.Scan(
			&row.AccountID,
			&row.OwnerID,
			&row.OwnerType,
			&row.CurrentAvailableBalance,
			&row.CurrentPendingBalance,
			&row.TotalEarnings,
			&row.TotalWithdrawn,
			&row.AccountStatus,
			&row.UpdatedAt,
			&row.HasPendingBalance,
			&row.HasAvailableBalance,
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
