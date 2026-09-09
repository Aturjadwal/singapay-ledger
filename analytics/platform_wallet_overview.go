package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// PlatformWalletOverviewCards represents the top summary cards on platform wallet page.
type PlatformWalletOverviewCards struct {
	PlatformAvailableBalance int64 `json:"platform_available_balance"`
	PlatformPendingBalance   int64 `json:"platform_pending_balance"`
	PlatformTotalBalance     int64 `json:"platform_total_balance"`
	SettlementPendingCount   int64 `json:"settlement_pending_count"`
	TotalRevenueYTD          int64 `json:"total_revenue_ytd"`
	GatewayFeeYTD            int64 `json:"gateway_fee_ytd"`
}

// PlatformWalletPerformanceRow represents one monthly chart point.
type PlatformWalletPerformanceRow struct {
	DateKey       int64  `json:"date_key"`
	IntervalLabel string `json:"interval_label"`
	RevenueIn     int64  `json:"revenue_in"`
	ExpensesOut   int64  `json:"expenses_out"`
}

// PlatformWalletMonthlyComparison represents the monthly comparison card values.
type PlatformWalletMonthlyComparison struct {
	CurrentMonthLabel     string  `json:"current_month_label"`
	PreviousMonthLabel    string  `json:"previous_month_label"`
	TotalRevenue          int64   `json:"total_revenue"`
	SubscriptionFee       int64   `json:"subscription_fee"`
	ConvenienceFee        int64   `json:"convenience_fee"`
	PreviousMonthRevenue  int64   `json:"previous_month_revenue"`
	TotalRevenueChangePct float64 `json:"total_revenue_change_pct"`
	SubscriptionChangePct float64 `json:"subscription_change_pct"`
	ConvenienceChangePct  float64 `json:"convenience_change_pct"`
}

// GetPlatformWalletOverviewCards returns platform wallet summary cards.
// It aggregates values from all rows in fact_platform_balance.
func (c *LedgerAnalyticsClient) GetPlatformWalletOverviewCards(ctx context.Context, year int) (*PlatformWalletOverviewCards, error) {
	_ = year

	query := `
SELECT
	COALESCE(SUM(platform_available_balance), 0) AS platform_available_balance,
	COALESCE(SUM(platform_pending_balance), 0) AS platform_pending_balance,
	COALESCE(SUM(platform_total_balance), 0) AS platform_total_balance,
	COALESCE(SUM(settlement_pending_count), 0) AS settlement_pending_count,
	COALESCE(SUM(total_revenue_ytd), 0) AS total_revenue_ytd,
	COALESCE(SUM(gateway_fee_ytd), 0) AS gateway_fee_ytd
FROM fact_platform_balance;`

	row := c.ledgerAnalyticsDB.QueryRowContext(ctx, query)
	result := &PlatformWalletOverviewCards{}
	if err := row.Scan(
		&result.PlatformAvailableBalance,
		&result.PlatformPendingBalance,
		&result.PlatformTotalBalance,
		&result.SettlementPendingCount,
		&result.TotalRevenueYTD,
		&result.GatewayFeeYTD,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ledgererr.ErrAnalyticsDataNotFound.WithError(err)
		}
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}

	return result, nil
}

// GetPlatformWalletPerformance returns monthly revenue-vs-expense rows for the given date range.
func (c *LedgerAnalyticsClient) GetPlatformWalletPerformance(
	ctx context.Context,
	startDate time.Time,
	endDate time.Time,
) ([]PlatformWalletPerformanceRow, error) {
	if startDate.IsZero() {
		return nil, ledgererr.ErrStartDateRequired
	}
	if endDate.IsZero() {
		return nil, ledgererr.ErrEndDateRequired
	}
	if startDate.After(endDate) {
		return nil, ledgererr.ErrInvalidDateRange
	}

	query := `
SELECT
  frt.date_key,
  frt.interval_label,
  (frt.convenience_fee_total + frt.subscription_fee_total) AS revenue_in,
  frt.gateway_fee_paid_total AS expenses_out
FROM fact_revenue_timeseries frt
WHERE frt.interval_type = 'MONTHLY'
  AND frt.date_key BETWEEN
    TO_CHAR(DATE_TRUNC('month', $1::date), 'YYYYMMDD')::INT
    AND TO_CHAR(DATE_TRUNC('month', $2::date), 'YYYYMMDD')::INT
ORDER BY frt.date_key ASC;`

	rows, err := c.ledgerAnalyticsDB.QueryContext(ctx, query, startDate, endDate)
	if err != nil {
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}
	defer rows.Close()

	result := make([]PlatformWalletPerformanceRow, 0)
	for rows.Next() {
		var row PlatformWalletPerformanceRow
		if err := rows.Scan(
			&row.DateKey,
			&row.IntervalLabel,
			&row.RevenueIn,
			&row.ExpensesOut,
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

// GetPlatformWalletMonthlyComparison returns values for the monthly comparison card.
func (c *LedgerAnalyticsClient) GetPlatformWalletMonthlyComparison(
	ctx context.Context,
	year int,
	month int,
) (*PlatformWalletMonthlyComparison, error) {
	if year < 2000 || year > 3000 {
		return nil, ledgererr.ErrInvalidYear.WithError(fmt.Errorf("got %d", year))
	}
	if month < 1 || month > 12 {
		return nil, ledgererr.ErrInvalidMonth.WithError(fmt.Errorf("got %d", month))
	}

	query := `
WITH base AS (
	SELECT MAKE_DATE($1, $2, 1) AS current_month_start
),
current_month AS (
	SELECT
		COALESCE(frt.total_revenue, 0) AS total_revenue,
		COALESCE(frt.subscription_fee_total, 0) AS subscription_fee,
		COALESCE(frt.convenience_fee_total, 0) AS convenience_fee
	FROM base b
	LEFT JOIN fact_revenue_timeseries frt
		ON frt.interval_type = 'MONTHLY'
	 AND frt.date_key = TO_CHAR(b.current_month_start, 'YYYYMMDD')::INT
),
previous_month AS (
	SELECT
		COALESCE(frt.total_revenue, 0) AS total_revenue,
		COALESCE(frt.subscription_fee_total, 0) AS subscription_fee,
		COALESCE(frt.convenience_fee_total, 0) AS convenience_fee
	FROM base b
	LEFT JOIN fact_revenue_timeseries frt
		ON frt.interval_type = 'MONTHLY'
	 AND frt.date_key = TO_CHAR((b.current_month_start - INTERVAL '1 month')::date, 'YYYYMMDD')::INT
)
SELECT
	TO_CHAR((SELECT current_month_start FROM base), 'FMMonth YYYY') AS current_month_label,
	TO_CHAR(((SELECT current_month_start FROM base) - INTERVAL '1 month')::date, 'FMMonth YYYY') AS previous_month_label,
	(SELECT total_revenue FROM current_month) AS total_revenue,
	(SELECT subscription_fee FROM current_month) AS subscription_fee,
	(SELECT convenience_fee FROM current_month) AS convenience_fee,
	(SELECT total_revenue FROM previous_month) AS previous_month_revenue,
	CASE
		WHEN (SELECT total_revenue FROM previous_month) = 0 THEN 0
		ELSE ROUND((((SELECT total_revenue FROM current_month) - (SELECT total_revenue FROM previous_month))::numeric
			/ (SELECT total_revenue FROM previous_month)::numeric) * 100, 2)
	END AS total_revenue_change_pct,
	CASE
		WHEN (SELECT subscription_fee FROM previous_month) = 0 THEN 0
		ELSE ROUND((((SELECT subscription_fee FROM current_month) - (SELECT subscription_fee FROM previous_month))::numeric
			/ (SELECT subscription_fee FROM previous_month)::numeric) * 100, 2)
	END AS subscription_change_pct,
	CASE
		WHEN (SELECT convenience_fee FROM previous_month) = 0 THEN 0
		ELSE ROUND((((SELECT convenience_fee FROM current_month) - (SELECT convenience_fee FROM previous_month))::numeric
			/ (SELECT convenience_fee FROM previous_month)::numeric) * 100, 2)
	END AS convenience_change_pct;`

	row := c.ledgerAnalyticsDB.QueryRowContext(ctx, query, year, month)
	result := &PlatformWalletMonthlyComparison{}
	if err := row.Scan(
		&result.CurrentMonthLabel,
		&result.PreviousMonthLabel,
		&result.TotalRevenue,
		&result.SubscriptionFee,
		&result.ConvenienceFee,
		&result.PreviousMonthRevenue,
		&result.TotalRevenueChangePct,
		&result.SubscriptionChangePct,
		&result.ConvenienceChangePct,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ledgererr.ErrAnalyticsDataNotFound.WithError(err)
		}
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}

	return result, nil
}
