package analytics

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// UserWalletOverviewCards represents the summary cards for the user wallets page.
type UserWalletOverviewCards struct {
	TotalSellerAccounts   int64 `json:"total_seller_accounts"`
	TotalAvailableBalance int64 `json:"total_available_balance"`
	TotalPendingBalance   int64 `json:"total_pending_balance"`
	TotalEarnings         int64 `json:"total_earnings"`
	TotalWithdrawn        int64 `json:"total_withdrawn"`
	ActiveWallets         int64 `json:"active_wallets"`
}

// GetUserWalletsOverview returns the summary cards for the user wallets page.
// Data is aggregated from fact_user_accumulation for current seller accounts.
func (c *LedgerAnalyticsClient) GetUserWalletsOverview(ctx context.Context) (*UserWalletOverviewCards, error) {
	query := `
SELECT
  COUNT(*) AS total_seller_accounts,
  COALESCE(SUM(fua.current_available_balance), 0) AS total_available_balance,
  COALESCE(SUM(fua.current_pending_balance), 0) AS total_pending_balance,
  COALESCE(SUM(fua.total_earnings), 0) AS total_earnings,
  COALESCE(SUM(fua.total_withdrawn), 0) AS total_withdrawn,
  COUNT(*) FILTER (WHERE fua.current_available_balance > 0 OR fua.current_pending_balance > 0) AS active_wallets
FROM fact_user_accumulation fua
JOIN dim_account da
  ON fua.dim_account_uuid = da.uuid
WHERE da.is_current = TRUE
  AND da.owner_type = 'SELLER';`

	row := c.ledgerAnalyticsDB.QueryRowContext(ctx, query)
	result := &UserWalletOverviewCards{}
	if err := row.Scan(
		&result.TotalSellerAccounts,
		&result.TotalAvailableBalance,
		&result.TotalPendingBalance,
		&result.TotalEarnings,
		&result.TotalWithdrawn,
		&result.ActiveWallets,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ledgererr.ErrAnalyticsDataNotFound.WithError(err)
		}
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}

	return result, nil
}
