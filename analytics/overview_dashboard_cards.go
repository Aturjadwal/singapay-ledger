package analytics

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// OverviewDashboardCards contains the year-to-date dashboard card metrics.
type OverviewDashboardCards struct {
	PlatformTotalBalance    int64 `json:"platform_total_balance"`
	PlatformPendingBalance  int64 `json:"platform_pending_balance"`
	TotalRevenue            int64 `json:"total_revenue"`
	ConvenienceFee          int64 `json:"convenience_fee"`
	SubscriptionFee         int64 `json:"subscription_fee"`
	TotalUserEarnings       int64 `json:"total_user_earnings"`
	TotalUserWithdrawn      int64 `json:"total_user_withdrawn"`
	TotalActiveTransactions int64 `json:"total_active_transactions"`
}

// GetOverviewDashboardCards returns the dashboard cards for the requested year.
func (c *LedgerAnalyticsClient) GetOverviewDashboardCards(ctx context.Context, year int) (*OverviewDashboardCards, error) {
	query := `
WITH platform_row AS (
  -- Current-year snapshot; fallback to latest available snapshot row
  SELECT
    *
  FROM
    fact_platform_balance
  WHERE
    date_key = TO_CHAR(MAKE_DATE($1, 1, 1), 'YYYYMMDD') :: INT
  UNION
  ALL
  SELECT
    *
  FROM
    fact_platform_balance
  WHERE
    NOT EXISTS (
      SELECT
        1
      FROM
        fact_platform_balance
      WHERE
        date_key = TO_CHAR(MAKE_DATE($1, 1, 1), 'YYYYMMDD') :: INT
    )
  ORDER BY
    date_key DESC
  LIMIT
    1
), range_revenue AS (
  SELECT
    COALESCE(SUM(convenience_fee_total), 0) AS convenience_fee,
    COALESCE(SUM(subscription_fee_total), 0) AS subscription_fee,
    COALESCE(SUM(total_revenue), 0) AS total_revenue
  FROM
    fact_revenue_timeseries
  WHERE
    interval_type = 'DAILY'
    AND date_key BETWEEN TO_CHAR(MAKE_DATE($1, 1, 1), 'YYYYMMDD') :: INT
    AND TO_CHAR(MAKE_DATE($1, 12, 31), 'YYYYMMDD') :: INT
)
SELECT
  p.platform_total_balance,
  p.platform_pending_balance,
  r.total_revenue,
  r.convenience_fee,
  r.subscription_fee,
  p.total_user_earnings,
  p.total_user_withdrawn,
  p.active_transactions_count AS total_active_transactions
FROM
  platform_row p
  CROSS JOIN range_revenue r;`

	row := c.ledgerAnalyticsDB.QueryRowContext(ctx, query, year)

	result := &OverviewDashboardCards{}
	if err := row.Scan(
		&result.PlatformTotalBalance,
		&result.PlatformPendingBalance,
		&result.TotalRevenue,
		&result.ConvenienceFee,
		&result.SubscriptionFee,
		&result.TotalUserEarnings,
		&result.TotalUserWithdrawn,
		&result.TotalActiveTransactions,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ledgererr.ErrAnalyticsDataNotFound.WithError(err)
		}
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}

	return result, nil
}
