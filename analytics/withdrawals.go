package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// WithdrawalsSummary represents summary cards for withdrawals page.
type WithdrawalsSummary struct {
	AttemptCount          int64   `json:"attempt_count"`
	SuccessCount          int64   `json:"success_count"`
	FailedCount           int64   `json:"failed_count"`
	TotalRequestedAmount  int64   `json:"total_requested_amount"`
	TotalDisbursedAmount  int64   `json:"total_disbursed_amount"`
	AvgProcessingTimeSec  int64   `json:"avg_processing_time_sec"`
	SuccessRatePercentage float64 `json:"success_rate_percentage"`
}

// WithdrawalTransactionRow represents one withdrawal transaction row.
type WithdrawalTransactionRow struct {
	DisbursementID        string     `json:"disbursement_id"`
	AccountUUID           string     `json:"account_uuid"`
	Amount                int64      `json:"amount"`
	Currency              string     `json:"currency"`
	Status                string     `json:"status"`
	BankCode              string     `json:"bank_code"`
	AccountNumber         string     `json:"account_number"`
	AccountName           string     `json:"account_name"`
	Description           *string    `json:"description,omitempty"`
	ExternalTransactionID *string    `json:"external_transaction_id,omitempty"`
	FailureReason         *string    `json:"failure_reason,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	ProcessedAt           *time.Time `json:"processed_at,omitempty"`
}

// GetWithdrawalsSummary returns summary values by summing MONTHLY rows from fact_withdrawal_timeseries.
func (c *LedgerAnalyticsClient) GetWithdrawalsSummary(ctx context.Context) (*WithdrawalsSummary, error) {
	query := `
SELECT
  COALESCE(SUM(attempt_count), 0) AS attempt_count,
  COALESCE(SUM(success_count), 0) AS success_count,
  COALESCE(SUM(failed_count), 0) AS failed_count,
  COALESCE(SUM(total_requested_amount), 0) AS total_requested_amount,
  COALESCE(SUM(total_disbursed_amount), 0) AS total_disbursed_amount,
  COALESCE(ROUND(AVG(avg_processing_time_sec)), 0) AS avg_processing_time_sec,
  CASE
    WHEN COALESCE(SUM(attempt_count), 0) = 0 THEN 0
    ELSE ROUND((COALESCE(SUM(success_count), 0)::numeric / COALESCE(SUM(attempt_count), 0)::numeric) * 100, 2)
  END AS success_rate_percentage
FROM fact_withdrawal_timeseries
WHERE interval_type = 'MONTHLY';`

	row := c.ledgerAnalyticsDB.QueryRowContext(ctx, query)
	result := &WithdrawalsSummary{}
	if err := row.Scan(
		&result.AttemptCount,
		&result.SuccessCount,
		&result.FailedCount,
		&result.TotalRequestedAmount,
		&result.TotalDisbursedAmount,
		&result.AvgProcessingTimeSec,
		&result.SuccessRatePercentage,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ledgererr.ErrAnalyticsDataNotFound.WithError(err)
		}
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}

	return result, nil
}

// GetWithdrawalTransactions returns paginated withdrawal transactions from disbursements table.
func (c *LedgerAnalyticsClient) GetWithdrawalTransactions(
	ctx context.Context,
	limit int,
	offset int,
) ([]WithdrawalTransactionRow, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		return nil, ledgererr.ErrInvalidLimit.WithError(fmt.Errorf("max 200, got %d", limit))
	}
	if offset < 0 {
		return nil, ledgererr.ErrInvalidOffset.WithError(fmt.Errorf("must be >= 0, got %d", offset))
	}

	query := `
SELECT
  uuid,
  account_uuid,
  amount,
  currency,
  status,
  bank_code,
  account_number,
  account_name,
  description,
  external_transaction_id,
  failure_reason,
  created_at,
  updated_at,
  processed_at
FROM disbursements
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;`

	rows, err := c.ledgerDB.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, ledgererr.ErrAnalyticsQueryError.WithError(err)
	}
	defer rows.Close()

	result := make([]WithdrawalTransactionRow, 0)
	for rows.Next() {
		var row WithdrawalTransactionRow
		if err := rows.Scan(
			&row.DisbursementID,
			&row.AccountUUID,
			&row.Amount,
			&row.Currency,
			&row.Status,
			&row.BankCode,
			&row.AccountNumber,
			&row.AccountName,
			&row.Description,
			&row.ExternalTransactionID,
			&row.FailureReason,
			&row.CreatedAt,
			&row.UpdatedAt,
			&row.ProcessedAt,
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
