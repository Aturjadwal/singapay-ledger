package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ETLOptions configures the ETL execution.
type ETLOptions struct {
	// EndTime specifies the upper bound for the data processing window.
	// If nil, defaults to time.Now().
	EndTime *time.Time
	// RecalculateDate forces date-specific recalculation where supported (UTC midnight).
	RecalculateDate *time.Time
	// RecalculateEndDate sets optional end date for recalculation range (UTC midnight).
	RecalculateEndDate *time.Time
	// RunID is the correlation ID for one scheduler cycle across all jobs/log lines.
	RunID string
}

// RunDimAccountETL executes the ETL job for dim_account (SCD Type 2).
// It syncs changes from ledger_accounts to dim_account.
func (c *LedgerAnalyticsClient) RunDimAccountETL(ctx context.Context, opts ETLOptions) error {
	jobName := "dim_account_loader"
	jobStart := time.Now()

	batchEnd := c.GetRunBatchEnd(jobName, opts)
	recalculateMode := opts.RecalculateDate != nil
	c.logger.Info("Starting ETL job", "job", jobName, "run_id", opts.RunID, "batch_end", batchEnd, "recalculate_mode", recalculateMode)

	err := c.RunWithIdempotency(ctx, jobName, func(ctx context.Context) error {
		// 1. Get last watermark
		lastWatermark, err := c.GetRunWatermark(ctx, jobName, opts)
		if err != nil {
			c.logger.Error("Failed to get watermark", "job", jobName, "run_id", opts.RunID, "error", err)
			return err
		}
		c.logger.Info("Loaded watermark", "job", jobName, "run_id", opts.RunID, "last_watermark", lastWatermark)

		// 2. Log start
		batchStart := time.Now()
		logID, err := c.LogMicrobatchStart(ctx, jobName, batchStart, batchEnd)
		if err != nil {
			return err
		}

		// 3. Execute ETL Logic (SCD Type 2)
		// We'll wrap this in a transaction
		tx, err := c.ledgerAnalyticsDB.BeginTx(ctx, nil)
		if err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return err
		}
		defer tx.Rollback()

		type changedAccount struct {
			accountUUID       string
			ownerType         string
			ownerID           string
			currency          string
			singapayAccountID string
		}

		// Step 3a: Identify changed accounts from ledgerDB
		// We fetch all accounts updated in the window (watermark, batchEnd]
		queryChanged := `
			SELECT uuid, owner_type, owner_id, currency, singapay_account_id
			FROM ledger_accounts
			WHERE (
				(NOT $3 AND updated_at > $1 AND updated_at <= $2)
				OR
				($3 AND updated_at >= DATE_TRUNC('day', $1) AND updated_at <= $2)
			)
		`

		rows, err := c.ledgerDB.QueryContext(ctx, queryChanged, lastWatermark, batchEnd, recalculateMode)
		if err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return fmt.Errorf("failed to query changed accounts: %w", err)
		}

		changedAccounts := make([]changedAccount, 0)
		for rows.Next() {
			var accountUUID, ownerType, currency string
			var ownerID sql.NullString
			var singapayID sql.NullString

			if err := rows.Scan(&accountUUID, &ownerType, &ownerID, &currency, &singapayID); err != nil {
				_ = rows.Close()
				c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
				return fmt.Errorf("failed to scan account row: %w", err)
			}

			ownerIDValue := ""
			if ownerID.Valid {
				ownerIDValue = ownerID.String
			}

			singapayAccountID := ""
			if singapayID.Valid {
				singapayAccountID = singapayID.String
			}

			changedAccounts = append(changedAccounts, changedAccount{
				accountUUID:       accountUUID,
				ownerType:         ownerType,
				ownerID:           ownerIDValue,
				currency:          currency,
				singapayAccountID: singapayAccountID,
			})
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return fmt.Errorf("failed while iterating changed accounts: %w", err)
		}

		if err := rows.Close(); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, 0, err.Error())
			return fmt.Errorf("failed to close changed accounts rows: %w", err)
		}

		processedCount := 0
		for _, acct := range changedAccounts {
			accountUUID := acct.accountUUID
			ownerType := acct.ownerType
			ownerID := acct.ownerID
			currency := acct.currency
			singapayAccountID := acct.singapayAccountID

			// Check if we need to create a new version
			var currentUUID, curOwnerType, curCurrency string
			var curOwnerID sql.NullString
			var curSingapayID sql.NullString

			checkQuery := `
				SELECT uuid, owner_type, owner_id, currency, singapay_account_id
				FROM dim_account
				WHERE account_id = $1 AND is_current = true
			`
			err := tx.QueryRowContext(ctx, checkQuery, accountUUID).Scan(&currentUUID, &curOwnerType, &curOwnerID, &curCurrency, &curSingapayID)

			needsUpdate := false
			if err == sql.ErrNoRows {
				needsUpdate = true // New account
			} else if err != nil {
				c.LogMicrobatchEnd(ctx, logID, StatusFailed, processedCount, err.Error())
				return fmt.Errorf("failed to check existing dimension: %w", err)
			} else {
				// Compare values to prevent unnecessary SCD2 versions if only balance changed
				curOwnerIDValue := ""
				if curOwnerID.Valid {
					curOwnerIDValue = curOwnerID.String
				}
				curSingapayAccountID := ""
				if curSingapayID.Valid {
					curSingapayAccountID = curSingapayID.String
				}

				if curOwnerType != ownerType || curOwnerIDValue != ownerID || curCurrency != currency || curSingapayAccountID != singapayAccountID {
					needsUpdate = true
				}
			}

			if !needsUpdate {
				continue
			}

			// Step 3b: Expire existing current record for this account (if exists)
			if currentUUID != "" {
				expireQuery := `
					UPDATE dim_account
					SET is_current = false, end_date = $1, updated_at = $2
					WHERE uuid = $3
				`
				if _, err := tx.ExecContext(ctx, expireQuery, batchEnd, time.Now(), currentUUID); err != nil {
					c.LogMicrobatchEnd(ctx, logID, StatusFailed, processedCount, err.Error())
					return fmt.Errorf("failed to expire old dimension record: %w", err)
				}
			}

			// Step 3c: Insert new current record
			insertQuery := `
				INSERT INTO dim_account (
					uuid, randid, created_at, updated_at,
					account_id, owner_type, owner_id, currency, singapay_account_id,
					effective_date, end_date, is_current
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, true)
			`
			newUUID := uuid.New().String()
			newRandID := uuid.New().String()
			if _, err := tx.ExecContext(ctx, insertQuery,
				newUUID, newRandID, time.Now(), time.Now(),
				accountUUID, ownerType, ownerID, currency, singapayAccountID,
				batchEnd, // Effective from this batch timestamp
			); err != nil {
				c.LogMicrobatchEnd(ctx, logID, StatusFailed, processedCount, err.Error())
				return fmt.Errorf("failed to insert new dimension record: %w", err)
			}
			processedCount++
		}

		if err := tx.Commit(); err != nil {
			c.LogMicrobatchEnd(ctx, logID, StatusFailed, processedCount, err.Error())
			c.logger.Error("Failed to commit ETL transaction", "job", jobName, "run_id", opts.RunID, "processed_count", processedCount, "error", err)
			return err
		}
		c.logger.Info("ETL job completed", "job", jobName, "run_id", opts.RunID, "processed_count", processedCount)

		// 4. Log completion
		return c.LogMicrobatchEnd(ctx, logID, StatusCompleted, processedCount, "Success")
	})

	if err != nil {
		c.logger.Error("ETL job summary", "job", jobName, "run_id", opts.RunID, "status", "failed", "duration", time.Since(jobStart), "error", err)
		return err
	}
	c.logger.Info("ETL job summary", "job", jobName, "run_id", opts.RunID, "status", "success", "duration", time.Since(jobStart))
	return nil
}
