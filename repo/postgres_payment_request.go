package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresPaymentRequestRepository struct {
	db DBTX
}

func NewPostgresPaymentRequestRepository(db DBTX) *PostgresPaymentRequestRepository {
	return &PostgresPaymentRequestRepository{db: db}
}

func (r *PostgresPaymentRequestRepository) GetByID(ctx context.Context, id string) (*domain.PaymentRequest, error) {
	query := `
		SELECT uuid, randid, product_transaction_uuid, request_id, payment_code, payment_channel, payment_url,
		       amount, currency, status, failure_reason, created_at, updated_at, completed_at, expires_at
		FROM payment_requests
		WHERE uuid = $1
	`

	return r.scanOne(ctx, query, id)
}

func (r *PostgresPaymentRequestRepository) GetByRequestID(ctx context.Context, requestID string) (*domain.PaymentRequest, error) {
	query := `
		SELECT uuid, randid, product_transaction_uuid, request_id, payment_code, payment_channel, payment_url,
		       amount, currency, status, failure_reason, created_at, updated_at, completed_at, expires_at
		FROM payment_requests
		WHERE request_id = $1
	`

	return r.scanOne(ctx, query, requestID)
}

func (r *PostgresPaymentRequestRepository) GetByPaymentCode(ctx context.Context, paymentCode string) (*domain.PaymentRequest, error) {
	query := `
		SELECT uuid, randid, product_transaction_uuid, request_id, payment_code, payment_channel, payment_url,
		       amount, currency, status, failure_reason, created_at, updated_at, completed_at, expires_at
		FROM payment_requests
		WHERE payment_code = $1
	`

	return r.scanOne(ctx, query, paymentCode)
}

func (r *PostgresPaymentRequestRepository) GetByProductTransactionID(ctx context.Context, productTransactionID string) (*domain.PaymentRequest, error) {
	query := `
		SELECT uuid, randid, product_transaction_uuid, request_id, payment_code, payment_channel, payment_url,
		       amount, currency, status, failure_reason, created_at, updated_at, completed_at, expires_at
		FROM payment_requests
		WHERE product_transaction_uuid = $1
	`

	return r.scanOne(ctx, query, productTransactionID)
}

func (r *PostgresPaymentRequestRepository) GetPendingExpired(ctx context.Context, before time.Time) ([]*domain.PaymentRequest, error) {
	query := `
		SELECT uuid, randid, product_transaction_uuid, request_id, payment_code, payment_channel, payment_url,
		       amount, currency, status, failure_reason, created_at, updated_at, completed_at, expires_at
		FROM payment_requests
		WHERE status = 'PENDING' AND expires_at < $1
		ORDER BY expires_at ASC
	`

	return r.scanMany(ctx, query, before)
}

func (r *PostgresPaymentRequestRepository) Save(ctx context.Context, pr *domain.PaymentRequest) error {
	query := `
		INSERT INTO payment_requests (
			uuid, randid, product_transaction_uuid, request_id, payment_code, payment_channel, payment_url,
			amount, currency, status, failure_reason, created_at, updated_at, completed_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`

	_, err := r.db.ExecContext(
		ctx,
		query,
		pr.UUID,
		pr.RandId,
		pr.ProductTransactionUUID,
		pr.RequestID,
		toNullString(pr.PaymentCode),
		pr.PaymentChannel,
		toNullString(pr.PaymentURL),
		pr.Amount,
		pr.Currency,
		pr.Status,
		toNullString(pr.FailureReason),
		pr.CreatedAt,
		pr.UpdatedAt,
		toNullTime(pr.CompletedAt),
		pr.ExpiresAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresPaymentRequestRepository) Update(ctx context.Context, pr *domain.PaymentRequest) error {
	query := `
		UPDATE payment_requests SET
			payment_code = $1,
			payment_url = $2,
			status = $3,
			failure_reason = $4,
			updated_at = $5,
			completed_at = $6
		WHERE uuid = $7
	`

	result, err := r.db.ExecContext(
		ctx,
		query,
		toNullString(pr.PaymentCode),
		toNullString(pr.PaymentURL),
		pr.Status,
		toNullString(pr.FailureReason),
		pr.UpdatedAt,
		toNullTime(pr.CompletedAt),
		pr.UUID,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return ErrFailedQuerySQL.WithError(err)
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// scanOne scans a single row into a PaymentRequest
func (r *PostgresPaymentRequestRepository) scanOne(ctx context.Context, query string, args ...any) (*domain.PaymentRequest, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, ErrNotFound
	}

	return r.scanRow(rows)
}

// scanMany scans multiple rows into PaymentRequests
func (r *PostgresPaymentRequestRepository) scanMany(ctx context.Context, query string, args ...any) ([]*domain.PaymentRequest, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	var requests []*domain.PaymentRequest
	for rows.Next() {
		pr, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, pr)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return requests, nil
}

// scanRow scans a single row into a PaymentRequest
func (r *PostgresPaymentRequestRepository) scanRow(rows *sql.Rows) (*domain.PaymentRequest, error) {
	var row struct {
		UUID                   string
		RandId                 string
		ProductTransactionUUID string
		RequestID              string
		PaymentCode            sql.NullString
		PaymentChannel         string
		PaymentURL             sql.NullString
		Amount                 int64
		Currency               string
		Status                 string
		FailureReason          sql.NullString
		CreatedAt              time.Time
		UpdatedAt              time.Time
		CompletedAt            sql.NullTime
		ExpiresAt              time.Time
	}

	err := rows.Scan(
		&row.UUID,
		&row.RandId,
		&row.ProductTransactionUUID,
		&row.RequestID,
		&row.PaymentCode,
		&row.PaymentChannel,
		&row.PaymentURL,
		&row.Amount,
		&row.Currency,
		&row.Status,
		&row.FailureReason,
		&row.CreatedAt,
		&row.UpdatedAt,
		&row.CompletedAt,
		&row.ExpiresAt,
	)
	if err != nil {
		return nil, ErrFailedScanSQL.WithError(err)
	}

	var completedAt *time.Time
	if row.CompletedAt.Valid {
		completedAt = &row.CompletedAt.Time
	}

	pr := &domain.PaymentRequest{
		ProductTransactionUUID: row.ProductTransactionUUID,
		RequestID:              row.RequestID,
		PaymentCode:            row.PaymentCode.String,
		PaymentChannel:         row.PaymentChannel,
		PaymentURL:             row.PaymentURL.String,
		Amount:                 row.Amount,
		Currency:               domain.Currency(row.Currency),
		Status:                 domain.PaymentStatus(row.Status),
		FailureReason:          row.FailureReason.String,
		CompletedAt:            completedAt,
		ExpiresAt:              row.ExpiresAt,
	}
	redifu.InitRecord(pr)
	// Override auto-generated values with database values
	pr.UUID = row.UUID
	pr.RandId = row.RandId
	pr.CreatedAt = row.CreatedAt
	pr.UpdatedAt = row.UpdatedAt
	return pr, nil
}
