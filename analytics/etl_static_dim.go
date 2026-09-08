package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RunStaticDimensionsETL ensures static dimensions are populated.
func (c *LedgerAnalyticsClient) RunStaticDimensionsETL(ctx context.Context, opts ETLOptions) error {
	jobName := "static_dimensions_loader"
	jobStart := time.Now()
	c.logger.Info("Starting ETL job", "job", jobName, "run_id", opts.RunID, "has_recalculate_date", opts.RecalculateDate != nil)

	err := c.RunWithIdempotency(ctx, jobName, func(ctx context.Context) error {
		// Keep dim_date maintained on every cycle (idempotent top-up),
		// while other static dimensions are initialized only once.
		if err := c.ensureDimDate(ctx, opts); err != nil {
			c.logger.Error("Failed to ensure dim_date", "job", jobName, "run_id", opts.RunID, "error", err)
			return err
		}

		alreadyCompleted, err := c.HasCompletedRun(ctx, jobName)
		if err != nil {
			return err
		}
		if alreadyCompleted {
			c.logger.Info("Static dimensions already initialized, skipping", "job", jobName, "run_id", opts.RunID)
			return nil
		}

		logID, err := c.LogMicrobatchStart(ctx, jobName, time.Now(), time.Now())
		if err != nil {
			return err
		}

		if err := c.ensureDimLedgerBucket(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimLedgerEntryType(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimAccountStatus(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimTransactionStatus(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimProductType(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimAccountOwnerType(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimBank(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimTransactionType(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		if err := c.ensureDimSubscription(ctx); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}

		c.logger.Info("ETL job completed", "job", jobName, "run_id", opts.RunID)
		return c.LogMicrobatchEnd(ctx, logID, StatusCompleted, 0, "Static dimensions verified")
	})

	if err != nil {
		c.logger.Error("ETL job summary", "job", jobName, "run_id", opts.RunID, "status", "failed", "duration", time.Since(jobStart), "error", err)
		return err
	}
	c.logger.Info("ETL job summary", "job", jobName, "run_id", opts.RunID, "status", "success", "duration", time.Since(jobStart))
	return nil
}

// ensureDimDate ensures dim_date table is populated for a reasonable range.
// If opts.RecalculateDate is set, it force-upserts the range from
// recalculate-date to recalculate-end-date (or single day if end is not set).
func (c *LedgerAnalyticsClient) ensureDimDate(ctx context.Context, opts ETLOptions) error {
	if opts.RecalculateDate != nil {
		start := time.Date(opts.RecalculateDate.Year(), opts.RecalculateDate.Month(), opts.RecalculateDate.Day(), 0, 0, 0, 0, time.UTC)
		end := start
		if opts.RecalculateEndDate != nil {
			end = time.Date(opts.RecalculateEndDate.Year(), opts.RecalculateEndDate.Month(), opts.RecalculateEndDate.Day(), 0, 0, 0, 0, time.UTC)
		}
		if end.Before(start) {
			return fmt.Errorf("recalculate-end-date is before recalculate-date")
		}

		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			if err := c.upsertDimDate(ctx, d); err != nil {
				return err
			}
		}
		return nil
	}

	today := time.Now().Truncate(24 * time.Hour)
	todayKey := int(today.Year()*10000 + int(today.Month())*100 + today.Day())

	var exists int
	err := c.ledgerAnalyticsDB.QueryRowContext(ctx, "SELECT count(*) FROM dim_date WHERE date_key = $1", todayKey).Scan(&exists)
	if err != nil {
		return err
	}

	if exists > 0 {
		return nil
	}

	start := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(2, 0, 0)

	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		if err := c.upsertDimDate(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) upsertDimDate(ctx context.Context, d time.Time) error {
	normalized := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	key := int(normalized.Year()*10000 + int(normalized.Month())*100 + normalized.Day())
	query := `
		INSERT INTO dim_date (uuid, randid, created_at, updated_at, date_key, date)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (date_key) DO UPDATE SET
			updated_at = EXCLUDED.updated_at,
			date = EXCLUDED.date
	`
	if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), key, normalized); err != nil {
		return fmt.Errorf("failed to upsert date %d: %w", key, err)
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimLedgerBucket(ctx context.Context) error {
	buckets := []string{"PENDING", "AVAILABLE"}
	for _, b := range buckets {
		query := `
			INSERT INTO dim_ledger_bucket (uuid, randid, created_at, updated_at, bucket_key)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (bucket_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), b); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimLedgerEntryType(ctx context.Context) error {
	types := []string{"CREDIT", "DEBIT"}
	for _, t := range types {
		query := `
			INSERT INTO dim_ledger_entry_type (uuid, randid, created_at, updated_at, entry_type_key)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (entry_type_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), t); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimAccountStatus(ctx context.Context) error {
	statuses := []string{"ACTIVE", "SUSPENDED", "CLOSED"}
	for _, s := range statuses {
		query := `
			INSERT INTO dim_account_status (uuid, randid, created_at, updated_at, status_key)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (status_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), s); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimTransactionStatus(ctx context.Context) error {
	statuses := map[string]bool{
		"PENDING":   false,
		"COMPLETED": true,
		"SETTLED":   true,
		"FAILED":    true,
	}
	for s, isTerminal := range statuses {
		query := `
			INSERT INTO dim_transaction_status (uuid, randid, created_at, updated_at, status_key, is_terminal)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (status_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), s, isTerminal); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimProductType(ctx context.Context) error {
	types := []string{"PHOTO", "FOLDER", "SUBSCRIPTION"}
	for _, t := range types {
		query := `
			INSERT INTO dim_product_type (uuid, randid, created_at, updated_at, product_type_key)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (product_type_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), t); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimAccountOwnerType(ctx context.Context) error {
	types := []string{"SELLER", "BUYER", "PLATFORM", "PAYMENT_GATEWAY", "RESERVE"}
	for _, t := range types {
		query := `
			INSERT INTO dim_account_owner_type (uuid, randid, created_at, updated_at, owner_type_key)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (owner_type_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), t); err != nil {
			return err
		}
	}
	return nil
}

// ensureDimBank populates the bank dimension from the payouts this ledger has made.
//
// There is no gateway catalogue to read. Singapay exposes no bank list through its API —
// the destination bank of a payout is validated one account at a time through
// check-beneficiary, and there is no endpoint that enumerates what it will accept. So the
// dimension is built from observed fact: every distinct bank code that has appeared on a
// disbursement.
//
// That makes it lag rather than lead — a bank nobody has been paid at yet is absent — which
// is the correct behaviour for a dimension whose only consumers are fact tables keyed on
// disbursements. Nothing can reference a bank that has never been used.
//
// bank_name is set to the code. Singapay returns a human name on check-beneficiary, but
// this ledger does not store it, and inventing a lookup table here would be a second copy
// of a list that belongs at the gateway.
func (c *LedgerAnalyticsClient) ensureDimBank(ctx context.Context) error {
	rows, err := c.ledgerDB.QueryContext(ctx, `
		SELECT DISTINCT bank_code
		FROM disbursements
		WHERE bank_code IS NOT NULL AND bank_code <> ''
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var bankCodes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return err
		}
		bankCodes = append(bankCodes, code)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, code := range bankCodes {
		query := `
			INSERT INTO dim_bank (uuid, randid, created_at, updated_at, bank_code, bank_name, swift_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (bank_code) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), code, code, ""); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimTransactionType(ctx context.Context) error {
	types := []struct {
		Key      int
		Source   string
		Channel  string
		Category string
	}{
		{1, "PAYMENT", "QRIS", "SALE"},
		{2, "PAYMENT", "VA", "SALE"},
		{3, "DISBURSEMENT", "BANK_TRANSFER", "WITHDRAWAL"},
	}

	for _, t := range types {
		query := `
			INSERT INTO dim_transaction_type (uuid, randid, created_at, updated_at, transaction_type_key, source_type, payment_channel, transaction_category)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (transaction_type_key) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), t.Key, t.Source, t.Channel, t.Category); err != nil {
			return err
		}
	}
	return nil
}

func (c *LedgerAnalyticsClient) ensureDimSubscription(ctx context.Context) error {
	statuses := []string{"NONE", "ACTIVE", "EXPIRED", "CANCELLED"}
	for _, s := range statuses {
		query := `
			INSERT INTO dim_subscription (uuid, randid, created_at, updated_at, subscription_status)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (subscription_status) DO NOTHING
		`
		if _, err := c.ledgerAnalyticsDB.ExecContext(ctx, query, uuid.New().String(), uuid.New().String(), time.Now(), time.Now(), s); err != nil {
			return err
		}
	}
	return nil
}
