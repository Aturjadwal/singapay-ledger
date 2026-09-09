package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresDisbursementRepository struct {
	db DBTX
}

func NewPostgresDisbursementRepository(db DBTX) *PostgresDisbursementRepository {
	return &PostgresDisbursementRepository{db: db}
}

func (r *PostgresDisbursementRepository) GetByID(ctx context.Context, id string) (*domain.Disbursement, error) {
	query := `
		SELECT uuid, randid, account_uuid, amount, currency, status,
		       bank_code, account_number, account_name,
		       description, external_transaction_id, failure_reason,
		       payout_request_id, gateway_fee, created_at, updated_at, processed_at
		FROM disbursements
		WHERE uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, id)

	var d domain.Disbursement
	redifu.InitRecord(&d)
	var externalTxID sql.NullString
	var failureReason sql.NullString
	var payoutRequestID sql.NullString
	var description sql.NullString
	var processedAt sql.NullTime

	err := row.Scan(
		&d.UUID,
		&d.RandId,
		&d.LedgerUUID,
		&d.Amount,
		&d.Currency,
		&d.Status,
		&d.BankAccount.BankCode,
		&d.BankAccount.AccountNumber,
		&d.BankAccount.AccountName,
		&description,
		&externalTxID,
		&failureReason,
		&payoutRequestID,
		&d.GatewayFee,
		&d.CreatedAt,
		&d.UpdatedAt,
		&processedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	if description.Valid {
		d.Description = description.String
	}
	if externalTxID.Valid {
		d.ExternalTransactionID = externalTxID.String
	}
	if failureReason.Valid {
		d.FailureReason = failureReason.String
	}
	// Read on its own, not nested under failure_reason. The two columns are unrelated,
	// and the row that most needs its request id — PENDING after a timeout, which
	// recordPayoutFailure deliberately leaves untouched — carries no failure reason at
	// all. While these were nested, RetryDisbursement saw an empty id on exactly those
	// rows and refused them as predating idempotent retries.
	if payoutRequestID.Valid {
		d.PayoutRequestID = payoutRequestID.String
	}
	if processedAt.Valid {
		d.ProcessedAt = &processedAt.Time
	}

	return &d, nil
}

// GetByAccountIDWithCursor returns disbursements with cursor-based pagination using RandId.
// This mimics redifu's infinite scrolling pattern where RandId is used as the cursor.
// Since RandId is a random string, we use it to identify the starting position,
// but actual sorting is done on created_at field.
// sortOrder: "ASC" or "DESC" (defaults to DESC if invalid)
func (r *PostgresDisbursementRepository) GetByAccountIDWithCursor(ctx context.Context, accountID string, cursor string, pageSize int, sortOrder string) ([]*domain.Disbursement, error) {
	var query string
	var args []any

	// Normalize sort order
	if sortOrder != "ASC" && sortOrder != "DESC" {
		sortOrder = "DESC"
	}

	if cursor == "" {
		// First page: no cursor, start from beginning
		query = `
			SELECT uuid, randid, account_uuid, amount, currency, status,
			       bank_code, account_number, account_name,
			       description, external_transaction_id, failure_reason,
			       payout_request_id, gateway_fee, created_at, updated_at, processed_at
			FROM disbursements
			WHERE account_uuid = $1
			ORDER BY created_at ` + sortOrder + `
			LIMIT $2
		`
		args = []any{accountID, pageSize}
	} else {
		// Subsequent pages: find the cursor item first, then get items after it
		// Use a subquery to get the created_at of the cursor item
		if sortOrder == "DESC" {
			query = `
				SELECT uuid, randid, account_uuid, amount, currency, status,
				       bank_code, account_number, account_name,
				       description, external_transaction_id, failure_reason,
				       payout_request_id, gateway_fee, created_at, updated_at, processed_at
				FROM disbursements
				WHERE account_uuid = $1 
				  AND (created_at < (SELECT created_at FROM disbursements WHERE randid = $2)
				       OR (created_at = (SELECT created_at FROM disbursements WHERE randid = $2) AND randid < $2))
				ORDER BY created_at DESC, randid DESC
				LIMIT $3
			`
		} else {
			query = `
				SELECT uuid, randid, account_uuid, amount, currency, status,
				       bank_code, account_number, account_name,
				       description, external_transaction_id, failure_reason,
				       payout_request_id, gateway_fee, created_at, updated_at, processed_at
				FROM disbursements
				WHERE account_uuid = $1 
				  AND (created_at > (SELECT created_at FROM disbursements WHERE randid = $2)
				       OR (created_at = (SELECT created_at FROM disbursements WHERE randid = $2) AND randid > $2))
				ORDER BY created_at ASC, randid ASC
				LIMIT $3
			`
		}
		args = []any{accountID, cursor, pageSize}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanDisbursements(rows)
}

func (r *PostgresDisbursementRepository) GetByLedgerID(ctx context.Context, ledgerID string, page, pageSize int) ([]*domain.Disbursement, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	query := `
		SELECT uuid, randid, account_uuid, amount, currency, status,
		       bank_code, account_number, account_name,
		       description, external_transaction_id, failure_reason,
		       payout_request_id, gateway_fee, created_at, updated_at, processed_at
		FROM disbursements
		WHERE account_uuid = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, ledgerID, pageSize, offset)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanDisbursements(rows)
}

func (r *PostgresDisbursementRepository) GetPendingByLedgerID(ctx context.Context, ledgerID string) ([]*domain.Disbursement, error) {
	query := `
		SELECT uuid, randid, account_uuid, amount, currency, status,
		       bank_code, account_number, account_name,
		       description, external_transaction_id, failure_reason,
		       payout_request_id, gateway_fee, created_at, updated_at, processed_at
		FROM disbursements
		WHERE account_uuid = $1 AND status = $2
		ORDER BY created_at ASC
	`

	rows, err := r.db.QueryContext(ctx, query, ledgerID, domain.DisbursementStatusPending)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanDisbursements(rows)
}

// GetPendingOlderThan returns PENDING disbursements created before the cutoff, oldest
// first. It spans every account, which is what separates it from GetPendingByLedgerID:
// the caller is looking for payouts whose outcome was never learned, and those are not
// concentrated in any one seller.
//
// The cutoff exists because a disbursement is written PENDING before the gateway is called, so
// a row created moments ago is not stuck — its first attempt is simply still in flight.
func (r *PostgresDisbursementRepository) GetPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*domain.Disbursement, error) {
	query := `
		SELECT uuid, randid, account_uuid, amount, currency, status,
		       bank_code, account_number, account_name,
		       description, external_transaction_id, failure_reason,
		       payout_request_id, gateway_fee, created_at, updated_at, processed_at
		FROM disbursements
		WHERE status = $1 AND created_at < $2
		ORDER BY created_at ASC
		LIMIT $3
	`

	rows, err := r.db.QueryContext(ctx, query, domain.DisbursementStatusPending, cutoff, limit)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanDisbursements(rows)
}

// Save inserts a disbursement, or updates the mutable parts of one that already exists.
//
// payout_request_id and gateway_fee are deliberately absent from the DO UPDATE SET list.
// Both are set once, when the row is first written ahead of the gateway call, and must
// survive every later save. Overwriting the reference would hand a retry a fresh
// idempotency key, which is precisely the hole that column was added to close; overwriting
// the fee would make the reversal disagree with the reservation it is meant to release.
func (r *PostgresDisbursementRepository) Save(ctx context.Context, d *domain.Disbursement) error {
	query := `
		INSERT INTO disbursements (
			uuid, randid, account_uuid, amount, currency, status,
			bank_code, account_number, account_name,
			description, external_transaction_id, failure_reason,
			payout_request_id, gateway_fee, created_at, updated_at, processed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (uuid) DO UPDATE SET
			status = EXCLUDED.status,
			external_transaction_id = EXCLUDED.external_transaction_id,
			failure_reason = EXCLUDED.failure_reason,
			updated_at = EXCLUDED.updated_at,
			processed_at = EXCLUDED.processed_at
	`

	_, err := r.db.ExecContext(
		ctx,
		query,
		d.UUID,
		d.RandId,
		d.LedgerUUID,
		d.Amount,
		d.Currency,
		d.Status,
		d.BankAccount.BankCode,
		d.BankAccount.AccountNumber,
		d.BankAccount.AccountName,
		toNullString(d.Description),
		toNullString(d.ExternalTransactionID),
		toNullString(d.FailureReason),
		toNullString(d.PayoutRequestID),
		d.GatewayFee,
		d.CreatedAt,
		d.UpdatedAt,
		toNullTime(d.ProcessedAt),
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresDisbursementRepository) UpdateStatus(
	ctx context.Context,
	id string,
	status domain.DisbursementStatus,
	processedAt *time.Time,
	failureReason string,
) error {
	query := `
		UPDATE disbursements
		SET status = $2, processed_at = $3, failure_reason = $4
		WHERE uuid = $1
	`

	result, err := r.db.ExecContext(ctx, query, id, status, toNullTime(processedAt), toNullString(failureReason))
	if err != nil {
		return ErrFailedUpdateSQL.WithError(err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return ErrFailedUpdateSQL.WithError(err)
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

func (r *PostgresDisbursementRepository) scanDisbursements(rows *sql.Rows) ([]*domain.Disbursement, error) {
	var disbursements []*domain.Disbursement

	for rows.Next() {
		var d domain.Disbursement
		redifu.InitRecord(&d)
		var externalTxID sql.NullString
		var failureReason sql.NullString
		var payoutRequestID sql.NullString
		var description sql.NullString
		var processedAt sql.NullTime

		err := rows.Scan(
			&d.UUID,
			&d.RandId,
			&d.LedgerUUID,
			&d.Amount,
			&d.Currency,
			&d.Status,
			&d.BankAccount.BankCode,
			&d.BankAccount.AccountNumber,
			&d.BankAccount.AccountName,
			&description,
			&externalTxID,
			&failureReason,
			&payoutRequestID,
			&d.GatewayFee,
			&d.CreatedAt,
			&d.UpdatedAt,
			&processedAt,
		)
		if err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}

		if description.Valid {
			d.Description = description.String
		}
		if externalTxID.Valid {
			d.ExternalTransactionID = externalTxID.String
		}
		if failureReason.Valid {
			d.FailureReason = failureReason.String
		}
		// See GetByID: payout_request_id is independent of failure_reason.
		if payoutRequestID.Valid {
			d.PayoutRequestID = payoutRequestID.String
		}
		if processedAt.Valid {
			d.ProcessedAt = &processedAt.Time
		}

		disbursements = append(disbursements, &d)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return disbursements, nil
}

// GetByPayoutRequestID resolves a disbursement from the reference_number a payout went out
// under.
//
// This is how a money-out webhook finds its row. Singapay's callback names the payout by
// its reference_number and by its own transaction_id, and only the first is something this
// ledger chose and stored before the call — the transaction_id does not exist until
// Singapay accepts the instruction, which is exactly the moment that can be lost.
func (r *PostgresDisbursementRepository) GetByPayoutRequestID(ctx context.Context, payoutRequestID string) (*domain.Disbursement, error) {
	// An empty reference never matches. Rows predating migration 014 carry NULL, and
	// letting "" through would match one of them on a schema where the column is NOT
	// NULL and book a stranger's payout outcome onto it.
	if payoutRequestID == "" {
		return nil, ErrNotFound
	}

	query := `
		SELECT uuid, randid, account_uuid, amount, currency, status,
		       bank_code, account_number, account_name,
		       description, external_transaction_id, failure_reason,
		       payout_request_id, gateway_fee, created_at, updated_at, processed_at
		FROM disbursements
		WHERE payout_request_id = $1
		LIMIT 1
	`

	rows, err := r.db.QueryContext(ctx, query, payoutRequestID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	disbursements, err := r.scanDisbursements(rows)
	if err != nil {
		return nil, err
	}
	if len(disbursements) == 0 {
		return nil, ErrNotFound
	}
	return disbursements[0], nil
}
