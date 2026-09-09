package analytics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

const (
	RevenueIntervalDaily   = "DAILY"
	RevenueIntervalWeekly  = "WEEKLY"
	RevenueIntervalMonthly = "MONTHLY"
	RevenueIntervalYearly  = "YEARLY"
)

// OverviewRevenueBreakdownRow represents one chart point in the revenue breakdown response.
type OverviewRevenueBreakdownRow struct {
	DateKey                    int64  `json:"date_key"`
	IntervalLabel              string `json:"interval_label"`
	IntervalType               string `json:"interval_type"`
	AdminFee                   int64  `json:"admin_fee"`
	Subscription               int64  `json:"subscription"`
	TotalRevenue               int64  `json:"total_revenue"`
	GatewayFeePaidTotal        int64  `json:"gateway_fee_paid_total"`
	SettlementTransactionCount int64  `json:"settlement_transaction_count"`
}

// GetOverviewRevenueBreakdown returns revenue breakdown rows based on interval and date range.
func (c *LedgerAnalyticsClient) GetOverviewRevenueBreakdown(
	ctx context.Context,
	intervalType string,
	startDate time.Time,
	endDate time.Time,
) ([]OverviewRevenueBreakdownRow, error) {
	normalizedInterval, err := validateOverviewRevenueBreakdownParams(intervalType, startDate, endDate)
	if err != nil {
		return nil, err
	}

	query := `
SELECT
  frt.date_key,
  frt.interval_label,
  frt.interval_type,
  frt.convenience_fee_total AS admin_fee,
  frt.subscription_fee_total AS subscription,
  frt.total_revenue,
  frt.gateway_fee_paid_total,
  frt.settlement_transaction_count
FROM fact_revenue_timeseries frt
WHERE frt.interval_type = $1
  AND frt.date_key BETWEEN
    CASE
      WHEN $1 = 'DAILY' THEN TO_CHAR($2::date, 'YYYYMMDD')::INT
      WHEN $1 = 'WEEKLY' THEN TO_CHAR(DATE_TRUNC('week', $2::date), 'YYYYMMDD')::INT
      WHEN $1 = 'MONTHLY' THEN TO_CHAR(DATE_TRUNC('month', $2::date), 'YYYYMMDD')::INT
      WHEN $1 = 'YEARLY' THEN TO_CHAR(DATE_TRUNC('year', $2::date), 'YYYYMMDD')::INT
    END
    AND
    CASE
      WHEN $1 = 'DAILY' THEN TO_CHAR($3::date, 'YYYYMMDD')::INT
      WHEN $1 = 'WEEKLY' THEN TO_CHAR(DATE_TRUNC('week', $3::date), 'YYYYMMDD')::INT
      WHEN $1 = 'MONTHLY' THEN TO_CHAR(DATE_TRUNC('month', $3::date), 'YYYYMMDD')::INT
      WHEN $1 = 'YEARLY' THEN TO_CHAR(DATE_TRUNC('year', $3::date), 'YYYYMMDD')::INT
    END
ORDER BY frt.date_key ASC;`

	rows, err := c.ledgerAnalyticsDB.QueryContext(ctx, query, normalizedInterval, startDate, endDate)
	if err != nil {
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}
	defer rows.Close()

	result := make([]OverviewRevenueBreakdownRow, 0)
	for rows.Next() {
		var row OverviewRevenueBreakdownRow
		if err := rows.Scan(
			&row.DateKey,
			&row.IntervalLabel,
			&row.IntervalType,
			&row.AdminFee,
			&row.Subscription,
			&row.TotalRevenue,
			&row.GatewayFeePaidTotal,
			&row.SettlementTransactionCount,
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

func validateOverviewRevenueBreakdownParams(intervalType string, startDate time.Time, endDate time.Time) (string, error) {
	normalizedInterval := strings.ToUpper(strings.TrimSpace(intervalType))
	switch normalizedInterval {
	case RevenueIntervalDaily, RevenueIntervalWeekly, RevenueIntervalMonthly, RevenueIntervalYearly:
		// valid
	default:
		return "", ledgererr.ErrInvalidIntervalType.WithError(fmt.Errorf("allowed: DAILY, WEEKLY, MONTHLY, YEARLY, got %q", intervalType))
	}

	if startDate.IsZero() {
		return "", ledgererr.ErrStartDateRequired
	}
	if endDate.IsZero() {
		return "", ledgererr.ErrEndDateRequired
	}
	if startDate.After(endDate) {
		return "", ledgererr.ErrInvalidDateRange
	}

	return normalizedInterval, nil
}
