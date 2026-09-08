package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RunFactRevenueTimeseriesETL implements the incremental load for fact_revenue_timeseries.
// Trigger: Incremental Delta Load (Micro-batch every 5m)
// Source: product_transactions (status = 'SETTLED' updated since last watermark)
func (c *LedgerAnalyticsClient) RunFactRevenueTimeseriesETL(ctx context.Context, opts ETLOptions) error {
	jobName := "fact_revenue_timeseries_loader"
	jobStart := time.Now()

	err := c.RunWithIdempotency(ctx, jobName, func(ctx context.Context) error {
		// 1. Get Watermark
		lastWatermark, err := c.GetRunWatermark(ctx, jobName, opts)
		if err != nil {
			return fmt.Errorf("failed to get watermark: %w", err)
		}
		recalculateMode := opts.RecalculateDate != nil

		batchEnd := time.Now()
		if opts.EndTime != nil {
			batchEnd = *opts.EndTime
		}
		if opts.RecalculateEndDate != nil {
			endOfDay := time.Date(opts.RecalculateEndDate.Year(), opts.RecalculateEndDate.Month(), opts.RecalculateEndDate.Day(), 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
			batchEnd = endOfDay
		}
		c.logger.Info("Starting ETL job",
			"job", jobName,
			"run_id", opts.RunID,
			"watermark", lastWatermark,
			"batch_end", batchEnd,
			"recalculate_mode", recalculateMode,
		)

		// Create microbatch log entry
		logID, err := c.LogMicrobatchStart(ctx, jobName, lastWatermark, batchEnd)
		if err != nil {
			return fmt.Errorf("failed to log microbatch start: %w", err)
		}

		// forceTarget := true
		// if forceTarget {
		// 	return fmt.Errorf("simulated forced error in %s via ETL_TEST_FORCE_FACT_ERROR=%s", jobName, forceTarget)
		// }

		// 2. Prepare the SQL Query
		// Logic:
		// 1. Identify settlements in current batch window (from ledgerDB)
		// 2. Map each settlement to its Daily, Weekly, Monthly, and Yearly bucket
		// 3. Recalculate full metrics for affected buckets (UPSERT on ledgerAnalyticsDB)

		// Phase 1: Read aggregated revenue data from ledgerDB
		queryRead := `
		WITH source_rows AS (
		  SELECT pt.*
		  FROM product_transactions pt
		  WHERE pt.status = 'SETTLED'
		    AND (
		      (NOT $3 AND pt.updated_at > $1 AND pt.updated_at <= $2)
		      OR
		      ($3 AND pt.settled_at >= DATE_TRUNC('day', $1) AND pt.settled_at <= $2)
		    )
		),
		affected_intervals AS (
		  SELECT DISTINCT
		    TO_CHAR(DATE_TRUNC(i.trunc_unit, sr.settled_at), 'YYYYMMDD')::INT AS date_key,
		    i.interval_type,
		    CASE i.interval_type
		      WHEN 'DAILY' THEN TO_CHAR(sr.settled_at, 'YYYY-MM-DD')
		      WHEN 'WEEKLY' THEN TO_CHAR(sr.settled_at, '"W"IW-YYYY')
		      WHEN 'MONTHLY' THEN TO_CHAR(sr.settled_at, 'YYYY-MM')
		      WHEN 'YEARLY' THEN TO_CHAR(sr.settled_at, 'YYYY')
		    END as interval_label
		  FROM source_rows sr
		  CROSS JOIN ( VALUES ('day', 'DAILY'), ('week', 'WEEKLY'), ('month', 'MONTHLY'), ('year', 'YEARLY') ) AS i(trunc_unit, interval_type)
		),
		recalculated AS (
		  SELECT
		    ai.date_key,
		    ai.interval_type,
		    ai.interval_label,
		    COALESCE(SUM(pt.platform_fee) FILTER (WHERE pt.product_type != 'SUBSCRIPTION'), 0) AS convenience_fee_total,
		    COALESCE(SUM(pt.seller_price) FILTER (WHERE pt.product_type = 'SUBSCRIPTION'), 0)  AS subscription_fee_total,
		    COALESCE(SUM(pt.gateway_fee), 0)                                                        AS gateway_fee_paid_total,
		    COUNT(*)                                                                             AS settlement_transaction_count
		  FROM affected_intervals ai
		  JOIN product_transactions pt ON pt.status = 'SETTLED'
		    AND TO_CHAR(DATE_TRUNC(
		          CASE ai.interval_type 
		            WHEN 'DAILY' THEN 'day' 
		            WHEN 'WEEKLY' THEN 'week' 
		            WHEN 'MONTHLY' THEN 'month' 
		            WHEN 'YEARLY' THEN 'year' 
		          END,
		          pt.settled_at
		        ), 'YYYYMMDD')::INT = ai.date_key
		  GROUP BY ai.date_key, ai.interval_type, ai.interval_label
		)
		SELECT
		  date_key, interval_type, interval_label,
		  convenience_fee_total, subscription_fee_total, gateway_fee_paid_total,
		  convenience_fee_total + subscription_fee_total,
		  settlement_transaction_count
		FROM recalculated
		`

		// Execute SELECT on ledgerDB to fetch aggregated data
		rows, err := c.ledgerDB.QueryContext(ctx, queryRead, lastWatermark, batchEnd, recalculateMode)
		if err != nil {
			return fmt.Errorf("failed to query source revenue aggregates: %w", err)
		}
		defer rows.Close()

		type revenueRecord struct {
			dateKey, convenience, subscription, gateway, total int64
			intervalType, intervalLabel                        string
			transactionCount                                   int64
		}

		records := make([]revenueRecord, 0)
		for rows.Next() {
			var r revenueRecord
			if err := rows.Scan(&r.dateKey, &r.intervalType, &r.intervalLabel, &r.convenience, &r.subscription, &r.gateway, &r.total, &r.transactionCount); err != nil {
				return fmt.Errorf("failed to scan revenue record: %w", err)
			}
			records = append(records, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("failed iterating revenue records: %w", err)
		}

		// Phase 2: Upsert into ledgerAnalyticsDB
		insertQuery := `
		INSERT INTO fact_revenue_timeseries (
		  uuid, randid, created_at, updated_at,
		  date_key, interval_type, interval_label,
		  convenience_fee_total, subscription_fee_total, gateway_fee_paid_total,
		  total_revenue, settlement_transaction_count
		) VALUES ($1, $2, NOW(), NOW(), $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (date_key, interval_type) DO UPDATE SET
		  convenience_fee_total = EXCLUDED.convenience_fee_total,
		  subscription_fee_total = EXCLUDED.subscription_fee_total,
		  gateway_fee_paid_total = EXCLUDED.gateway_fee_paid_total,
		  total_revenue = EXCLUDED.total_revenue,
		  settlement_transaction_count = EXCLUDED.settlement_transaction_count,
		  updated_at = NOW()
		`

		processedCount := 0
		for _, rec := range records {
			_, err := c.ledgerAnalyticsDB.ExecContext(ctx, insertQuery,
				uuid.New().String(),
				uuid.New().String(),
				rec.dateKey,
				rec.intervalType,
				rec.intervalLabel,
				rec.convenience,
				rec.subscription,
				rec.gateway,
				rec.total,
				rec.transactionCount,
			)
			if err != nil {
				return fmt.Errorf("failed to upsert fact_revenue_timeseries: %w", err)
			}
			processedCount++
		}

		c.logger.Info("ETL job completed", "job", jobName, "run_id", opts.RunID, "rows_affected", processedCount)

		if err := c.LogMicrobatchEnd(ctx, logID, "COMPLETED", processedCount, fmt.Sprintf("Processed %d rows", processedCount)); err != nil {
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
