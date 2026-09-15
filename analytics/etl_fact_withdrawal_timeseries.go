package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RunFactWithdrawalTimeseriesETL aggregates disbursement metrics into daily/monthly time series
// Strategy: UPSERT by (date_key, interval_type) for idempotency
// Full recalculation of affected intervals prevents duplicate sums
func (c *LedgerAnalyticsClient) RunFactWithdrawalTimeseriesETL(ctx context.Context, opts ETLOptions) error {
	jobName := "fact_withdrawal_timeseries_loader"
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

		logID, err := c.LogMicrobatchStart(ctx, jobName, lastWatermark, batchEnd)
		if err != nil {
			return fmt.Errorf("failed to log microbatch start: %w", err)
		}

		// Phase 1: Query affected disbursements from ledgerDB.
		//
		// Both money columns sum disbursements.amount, which is what the seller REQUESTED
		// and what their balance was debited — the transfer fee is inside it, not beside
		// it (migration 028). So total_disbursed_amount is what sellers were charged, and
		// it runs above the sum that actually reached their banks by the fees paid.
		queryAffected := `
SELECT DISTINCT
  TO_CHAR(DATE_TRUNC(i.trunc_unit::TEXT, d.created_at), 'YYYYMMDD')::INT AS date_key,
  i.interval_type,
  COUNT(*) AS attempt_count,
  COUNT(*) FILTER (WHERE d.status = 'COMPLETED') AS success_count,
  COUNT(*) FILTER (WHERE d.status = 'FAILED') AS failed_count,
  COALESCE(SUM(d.amount), 0) AS total_requested_amount,
  COALESCE(SUM(d.amount) FILTER (WHERE d.status = 'COMPLETED'), 0) AS total_disbursed_amount,
  COALESCE(AVG(EXTRACT(EPOCH FROM (d.processed_at - d.created_at))) FILTER (WHERE d.status = 'COMPLETED'), 0)::INT AS avg_processing_time_sec
FROM disbursements d
CROSS JOIN (VALUES ('day'::TEXT, 'DAILY'::TEXT), ('month'::TEXT, 'MONTHLY'::TEXT)) AS i(trunc_unit, interval_type)
WHERE (NOT $3 AND d.updated_at > $1 AND d.updated_at <= $2)
   OR ($3 AND d.created_at >= DATE_TRUNC('day', $1) AND d.created_at <= $2)
GROUP BY date_key, interval_type
		`

		// Execute SELECT on ledgerDB to fetch withdrawal aggregates
		rows, err := c.ledgerDB.QueryContext(ctx, queryAffected, lastWatermark, batchEnd, recalculateMode)
		if err != nil {
			return fmt.Errorf("failed to query source disbursements: %w", err)
		}
		defer rows.Close()

		type withdrawalRecord struct {
			dateKey                                    int64
			intervalType                               string
			attemptCount, successCount, failedCount    int64
			totalRequestedAmount, totalDisbursedAmount int64
			avgProcessingTimeSec                       int64
		}

		records := make([]withdrawalRecord, 0)
		for rows.Next() {
			var r withdrawalRecord
			if err := rows.Scan(&r.dateKey, &r.intervalType, &r.attemptCount, &r.successCount, &r.failedCount, &r.totalRequestedAmount, &r.totalDisbursedAmount, &r.avgProcessingTimeSec); err != nil {
				return fmt.Errorf("failed to scan withdrawal record: %w", err)
			}
			records = append(records, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("failed iterating withdrawal records: %w", err)
		}

		// Phase 2: Upsert into ledgerAnalyticsDB
		insertQuery := `
INSERT INTO fact_withdrawal_timeseries (
  uuid, randid, created_at, updated_at,
  date_key, interval_type,
  attempt_count, success_count, failed_count,
  total_requested_amount, total_disbursed_amount, avg_processing_time_sec
) VALUES ($1, $2, NOW(), NOW(), $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (date_key, interval_type) DO UPDATE SET
  attempt_count = EXCLUDED.attempt_count,
  success_count = EXCLUDED.success_count,
  failed_count = EXCLUDED.failed_count,
  total_requested_amount = EXCLUDED.total_requested_amount,
  total_disbursed_amount = EXCLUDED.total_disbursed_amount,
  avg_processing_time_sec = EXCLUDED.avg_processing_time_sec,
  updated_at = NOW()
		`

		processedCount := 0
		for _, rec := range records {
			_, err := c.ledgerAnalyticsDB.ExecContext(ctx, insertQuery,
				uuid.New().String(),
				uuid.New().String(),
				rec.dateKey,
				rec.intervalType,
				rec.attemptCount,
				rec.successCount,
				rec.failedCount,
				rec.totalRequestedAmount,
				rec.totalDisbursedAmount,
				rec.avgProcessingTimeSec,
			)
			if err != nil {
				return fmt.Errorf("failed to upsert fact_withdrawal_timeseries: %w", err)
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
