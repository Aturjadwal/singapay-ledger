package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// LedgerAnalyticsClient provides the interface for running analytics ETL jobs.
// It manages dimension and fact table populations with idempotency and watermarking.
//
// It takes no payment gateway client. It used to, for exactly one purpose — reading the
// gateway's supported-bank catalogue to build dim_bank — and Singapay publishes no such
// catalogue. The dimension is now derived from the payouts this ledger has actually made,
// which needs no network at all. See ensureDimBank.
type LedgerAnalyticsClient struct {
	ledgerDB          *sql.DB
	ledgerAnalyticsDB *sql.DB
	redis             redis.UniversalClient
	logger            *slog.Logger
}

// NewLedgerAnalyticsClient creates a new analytics client.
func NewLedgerAnalyticsClient(ledgerDB *sql.DB, ledgerAnalyticsDB *sql.DB, redisClient redis.UniversalClient, logger *slog.Logger) *LedgerAnalyticsClient {
	return &LedgerAnalyticsClient{
		ledgerDB:          ledgerDB,
		ledgerAnalyticsDB: ledgerAnalyticsDB,
		redis:             redisClient,
		logger:            logger,
	}
}

// RunAllDimensions runs all dimension ETL jobs sequentially.
func (c *LedgerAnalyticsClient) RunAllDimensions(opts ETLOptions) error {
	start := time.Now()
	ctx := context.Background()
	completedJobs := 0
	totalJobs := 4

	c.logger.Info("Starting dimensions pipeline", "run_id", opts.RunID, "total_jobs", totalJobs)

	// 1. Static Dimensions
	if err := c.RunStaticDimensionsETL(ctx, opts); err != nil {
		c.logger.Error("Dimensions pipeline failed", "run_id", opts.RunID, "failed_job", "static_dimensions", "completed_jobs", completedJobs, "total_jobs", totalJobs, "duration", time.Since(start), "error", err)
		return err
	}
	completedJobs++

	// 2. Dim Account (SCD Type 2)
	if err := c.RunDimAccountETL(ctx, opts); err != nil {
		c.logger.Error("Dimensions pipeline failed", "run_id", opts.RunID, "failed_job", "dim_account", "completed_jobs", completedJobs, "total_jobs", totalJobs, "duration", time.Since(start), "error", err)
		return err
	}
	completedJobs++

	// 3. Dim Bank Account (Transactional)
	if err := c.RunDimBankAccountETL(ctx, opts); err != nil {
		c.logger.Error("Dimensions pipeline failed", "run_id", opts.RunID, "failed_job", "dim_bank_account", "completed_jobs", completedJobs, "total_jobs", totalJobs, "duration", time.Since(start), "error", err)
		return err
	}
	completedJobs++

	// 4. Dim Payment Channel (Config)
	if err := c.RunDimPaymentChannelETL(ctx, opts); err != nil {
		c.logger.Error("Dimensions pipeline failed", "run_id", opts.RunID, "failed_job", "dim_payment_channel", "completed_jobs", completedJobs, "total_jobs", totalJobs, "duration", time.Since(start), "error", err)
		return err
	}
	completedJobs++

	c.logger.Info("Dimensions pipeline completed", "run_id", opts.RunID, "completed_jobs", completedJobs, "total_jobs", totalJobs, "duration", time.Since(start))

	return nil
}

// RunAllFacts runs all fact table ETL jobs sequentially.
func (c *LedgerAnalyticsClient) RunAllFacts(ctx context.Context, opts ETLOptions) error {
	start := time.Now()
	jobs := []struct {
		name string
		run  func(context.Context, ETLOptions) error
	}{
		{name: "fact_revenue_timeseries", run: c.RunFactRevenueTimeseriesETL},
		{name: "fact_platform_balance", run: c.RunFactPlatformBalanceETL},
		{name: "fact_user_accumulation", run: c.RunFactUserAccumulationETL},
		{name: "fact_withdrawal_timeseries", run: c.RunFactWithdrawalTimeseriesETL},
	}
	completedJobs := 0
	totalJobs := len(jobs)
	failedJobs := 0
	var groupedErrors []error

	c.logger.Info("Starting facts pipeline", "run_id", opts.RunID, "total_jobs", totalJobs)

	for _, job := range jobs {
		if err := job.run(ctx, opts); err != nil {
			failedJobs++
			groupedErrors = append(groupedErrors, fmt.Errorf("%s: %w", job.name, err))
			c.logger.Error("Facts job failed", "run_id", opts.RunID, "failed_job", job.name, "completed_jobs", completedJobs, "failed_jobs", failedJobs, "total_jobs", totalJobs, "duration", time.Since(start), "error", err)
			continue
		}

		completedJobs++
	}

	if failedJobs > 0 {
		err := fmt.Errorf("facts pipeline completed with errors (%d/%d failed): %w", failedJobs, totalJobs, errors.Join(groupedErrors...))
		c.logger.Error("Facts pipeline completed with errors", "run_id", opts.RunID, "completed_jobs", completedJobs, "failed_jobs", failedJobs, "total_jobs", totalJobs, "duration", time.Since(start), "error", err)
		return err
	}

	c.logger.Info("Facts pipeline completed", "run_id", opts.RunID, "completed_jobs", completedJobs, "total_jobs", totalJobs, "duration", time.Since(start))

	return nil
}

// RunFullETL runs the complete analytics pipeline (Dimensions -> Facts).
func (c *LedgerAnalyticsClient) RunFullETL(ctx context.Context, opts ETLOptions) error {
	start := time.Now()
	c.logger.Info("Starting full analytics ETL", "run_id", opts.RunID, "steps", 2)

	// 1. Dimensions (Must run first to ensure FKs exist)
	if err := c.RunAllDimensions(opts); err != nil {
		c.logger.Error("Full analytics ETL failed", "run_id", opts.RunID, "failed_step", "dimensions", "duration", time.Since(start), "error", err)
		return err
	}

	// 2. Facts
	if err := c.RunAllFacts(ctx, opts); err != nil {
		c.logger.Error("Full analytics ETL failed", "run_id", opts.RunID, "failed_step", "facts", "duration", time.Since(start), "error", err)
		return err
	}

	c.logger.Info("Full analytics ETL completed", "run_id", opts.RunID, "duration", time.Since(start), "status", "success")
	return nil
}

// RunFactRevenueETL wrapper for scheduler (revenue_timeseries)
func (c *LedgerAnalyticsClient) RunFactRevenueETL() error {
	return c.RunFactRevenueTimeseriesETL(context.Background(), ETLOptions{})
}

// RunFactPlatformBalanceETLScheduler wrapper for scheduler (platform_balance)
func (c *LedgerAnalyticsClient) RunFactPlatformBalanceETLScheduler() error {
	return c.RunFactPlatformBalanceETL(context.Background(), ETLOptions{})
}

// RunFactUserAccumulationETLScheduler wrapper for scheduler (user_accumulation)
func (c *LedgerAnalyticsClient) RunFactUserAccumulationETLScheduler() error {
	return c.RunFactUserAccumulationETL(context.Background(), ETLOptions{})
}

// RunFactWithdrawalTimeseriesETLScheduler wrapper for scheduler (withdrawal_timeseries)
func (c *LedgerAnalyticsClient) RunFactWithdrawalTimeseriesETLScheduler() error {
	return c.RunFactWithdrawalTimeseriesETL(context.Background(), ETLOptions{})
}
