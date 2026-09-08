package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RunFactPlatformBalanceETL updates the singleton platform balance snapshot.
// Strategy:
//   - Normal run: increment revenue counters using daily deltas in (watermark, batchEnd].
//   - Recalculate run: overwrite revenue counters with full YTD recomputation.
func (c *LedgerAnalyticsClient) RunFactPlatformBalanceETL(ctx context.Context, opts ETLOptions) error {
	jobName := "fact_platform_balance_loader"
	jobStart := time.Now()

	err := c.RunWithIdempotency(ctx, jobName, func(ctx context.Context) error {
		lastWatermark, err := c.GetRunWatermark(ctx, jobName, opts)
		if err != nil {
			return fmt.Errorf("failed to get watermark: %w", err)
		}
		recalculateMode := opts.RecalculateDate != nil

		batchEnd := time.Now()
		if opts.EndTime != nil {
			batchEnd = *opts.EndTime
		}
		if recalculateMode && opts.RecalculateEndDate != nil {
			c.logger.Info("Ignoring recalculate-end-date for full platform snapshot recomputation", "job", jobName, "run_id", opts.RunID, "recalculate_end_date", *opts.RecalculateEndDate)
		}
		c.logger.Info("Starting ETL job",
			"job", jobName,
			"run_id", opts.RunID,
			"watermark", lastWatermark,
			"batch_end", batchEnd,
			"recalculate_mode", recalculateMode,
		)

		logID, err := c.LogMicrobatchStart(ctx, jobName, lastWatermark, batchEnd)
		if err != nil {
			return fmt.Errorf("failed to log microbatch start: %w", err)
		}

		// Phase 1: Read aggregated revenue deltas from ledgerDB
		queryDeltas := `
WITH daily_revenue_deltas AS (
  SELECT
    DATE_TRUNC('day', settled_at)::DATE AS settled_day,
    COALESCE(SUM(platform_fee) FILTER (WHERE product_type != 'SUBSCRIPTION'), 0) AS day_convenience,
    COALESCE(SUM(seller_price) FILTER (WHERE product_type = 'SUBSCRIPTION'), 0) AS day_subscription,
    COALESCE(SUM(gateway_fee), 0) AS day_gateway
  FROM product_transactions
  WHERE status = 'SETTLED'
    AND updated_at > $1
    AND updated_at <= $2
  GROUP BY DATE_TRUNC('day', settled_at)::DATE
)
SELECT
  COALESCE(SUM(day_convenience), 0) AS delta_convenience,
  COALESCE(SUM(day_subscription), 0) AS delta_subscription,
  COALESCE(SUM(day_gateway), 0) AS delta_gateway
FROM daily_revenue_deltas
		`

		// Phase 1b: Read full YTD revenue from ledgerDB
		queryYTD := `
SELECT
  COALESCE(SUM(platform_fee) FILTER (WHERE product_type != 'SUBSCRIPTION'), 0) AS total_convenience,
  COALESCE(SUM(seller_price) FILTER (WHERE product_type = 'SUBSCRIPTION'), 0) AS total_subscription,
  COALESCE(SUM(gateway_fee), 0) AS total_gateway
FROM product_transactions
WHERE status = 'SETTLED'
	AND settled_at >= DATE_TRUNC('year', $1::timestamptz)
	AND settled_at <= $1::timestamptz
		`

		// Read deltas from ledgerDB
		var deltaConvenience, deltaSubscription, deltaGateway int64
		err = c.ledgerDB.QueryRowContext(ctx, queryDeltas, lastWatermark, batchEnd).Scan(&deltaConvenience, &deltaSubscription, &deltaGateway)
		if err != nil {
			return fmt.Errorf("failed to query deltas from ledgerDB: %w", err)
		}

		// Read YTD from ledgerDB
		var ytdConvenience, ytdSubscription, ytdGateway int64
		err = c.ledgerDB.QueryRowContext(ctx, queryYTD, batchEnd).Scan(&ytdConvenience, &ytdSubscription, &ytdGateway)
		if err != nil {
			return fmt.Errorf("failed to query YTD from ledgerDB: %w", err)
		}

		// Phase 1c: Read platform and seller aggregates from ledgerDB
		queryPlatform := `
SELECT pending_balance, available_balance FROM ledger_accounts WHERE owner_type = 'PLATFORM' LIMIT 1
		`

		querySellers := `
SELECT
  COUNT(*) AS total_accounts,
  COALESCE(SUM(available_balance), 0) AS total_available,
  COALESCE(SUM(pending_balance), 0) AS total_pending,
  COALESCE(SUM(total_deposit_amount), 0) AS total_user_earnings,
  COALESCE(SUM(total_withdrawal_amount), 0) AS total_user_withdrawn
FROM ledger_accounts WHERE owner_type = 'SELLER'
		`

		queryActiveTransactions := `
SELECT
	COUNT(*) AS active_transactions_count
FROM product_transactions
WHERE COALESCE(completed_at, settled_at, created_at) >= DATE_TRUNC('year', $1::timestamptz)
	AND COALESCE(completed_at, settled_at, created_at) <= $1::timestamptz
		`

		var platformPending, platformAvailable int64
		err = c.ledgerDB.QueryRowContext(ctx, queryPlatform).Scan(&platformPending, &platformAvailable)
		if err != nil && err.Error() != "sql: no rows in result set" {
			return fmt.Errorf("failed to query platform from ledgerDB: %w", err)
		}

		var totalAccounts, totalAvailable, totalPending, totalEarnings, totalWithdrawn int64
		err = c.ledgerDB.QueryRowContext(ctx, querySellers).Scan(&totalAccounts, &totalAvailable, &totalPending, &totalEarnings, &totalWithdrawn)
		if err != nil && err.Error() != "sql: no rows in result set" {
			return fmt.Errorf("failed to query sellers from ledgerDB: %w", err)
		}

		var activeTransactionsCount int64
		err = c.ledgerDB.QueryRowContext(ctx, queryActiveTransactions, batchEnd).Scan(&activeTransactionsCount)
		if err != nil && err.Error() != "sql: no rows in result set" {
			return fmt.Errorf("failed to query active transactions from ledgerDB: %w", err)
		}

		// Phase 2: Build platform balance data
		var finalConvenience, finalSubscription, finalGateway int64
		if recalculateMode {
			finalConvenience = ytdConvenience
			finalSubscription = ytdSubscription
			finalGateway = ytdGateway
		} else {
			finalConvenience = deltaConvenience
			finalSubscription = deltaSubscription
			finalGateway = deltaGateway
		}

		totalRevenue := finalConvenience + finalSubscription
		platformTotal := platformPending + platformAvailable

		// Phase 3: Upsert into ledgerAnalyticsDB
		insertQuery := `
INSERT INTO fact_platform_balance (
  uuid, randid, created_at, updated_at,
  date_key,
  convenience_fee_ytd, subscription_fee_ytd, gateway_fee_ytd, total_revenue_ytd,
  platform_pending_balance, platform_available_balance, platform_total_balance,
  total_seller_accounts, total_user_available_balance, total_user_pending_balance,
  total_user_earnings, total_user_withdrawn,
  settlement_completed_count, active_transactions_count
) VALUES ($1, $2, NOW(), NOW(), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
ON CONFLICT (date_key) DO UPDATE SET
  convenience_fee_ytd = CASE WHEN $18 THEN $4 ELSE fact_platform_balance.convenience_fee_ytd + $4 END,
  subscription_fee_ytd = CASE WHEN $18 THEN $5 ELSE fact_platform_balance.subscription_fee_ytd + $5 END,
  gateway_fee_ytd = CASE WHEN $18 THEN $6 ELSE fact_platform_balance.gateway_fee_ytd + $6 END,
  total_revenue_ytd = CASE WHEN $18 THEN $7 ELSE fact_platform_balance.total_revenue_ytd + $7 END,
  platform_pending_balance = $8,
  platform_available_balance = $9,
  platform_total_balance = $10,
  total_seller_accounts = $11,
  total_user_available_balance = $12,
  total_user_pending_balance = $13,
  total_user_earnings = $14,
  total_user_withdrawn = $15,
  active_transactions_count = $17,
  updated_at = NOW()
		`

		dateKey, _ := time.Parse("2006-01-02", fmt.Sprintf("%d-%02d-%02d", batchEnd.Year(), batchEnd.Month(), 1))
		dateKeyInt := dateKey.Year()*10000 + int(dateKey.Month())*100 + 1

		_, err = c.ledgerAnalyticsDB.ExecContext(ctx, insertQuery,
			uuid.New().String(),     // $1 - uuid
			uuid.New().String(),     // $2 - randid
			dateKeyInt,              // $3 - date_key
			finalConvenience,        // $4
			finalSubscription,       // $5
			finalGateway,            // $6
			totalRevenue,            // $7
			platformPending,         // $8
			platformAvailable,       // $9
			platformTotal,           // $10
			totalAccounts,           // $11
			totalAvailable,          // $12
			totalPending,            // $13
			totalEarnings,           // $14
			totalWithdrawn,          // $15
			int64(0),                // $16 - settlement_completed_count
			activeTransactionsCount, // $17 - active_transactions_count
			recalculateMode,         // $18 - for conditional update logic
		)
		if err != nil {
			return fmt.Errorf("failed to upsert fact_platform_balance: %w", err)
		}

		c.logger.Info("ETL job completed", "job", jobName, "run_id", opts.RunID, "rows_affected", 1)

		if err := c.LogMicrobatchEnd(ctx, logID, "COMPLETED", 1, "Processed 1 row (singleton)"); err != nil {
			return fmt.Errorf("failed to log microbatch end: %w", err)
		}

		return nil
	})

	if err != nil {
		c.logger.Error("ETL job summary", "job", jobName, "run_id", opts.RunID, "status", "failed", "duration", time.Since(jobStart), "error", err)
		return err
	}
	c.logger.Info("ETL job summary", "job", jobName, "run_id", opts.RunID, "status", "success", "duration", time.Since(jobStart))
	return nil
}
