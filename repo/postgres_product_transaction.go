package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/lib/pq"
)

type PostgresProductTransactionRepository struct {
	db DBTX
}

func NewPostgresProductTransactionRepository(db DBTX) *PostgresProductTransactionRepository {
	return &PostgresProductTransactionRepository{db: db}
}

func (r *PostgresProductTransactionRepository) GetByID(ctx context.Context, id string) (*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE uuid = $1
	`

	return r.scanOne(ctx, query, id)
}

func (r *PostgresProductTransactionRepository) GetByInvoiceNumber(ctx context.Context, invoiceNumber string) (*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE invoice_number = $1
	`

	return r.scanOne(ctx, query, invoiceNumber)
}

func (r *PostgresProductTransactionRepository) GetBySellerAccountID(ctx context.Context, sellerAccountID string, page, pageSize int) ([]*domain.ProductTransaction, error) {
	offset := (page - 1) * pageSize
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE seller_account_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	return r.scanMany(ctx, query, sellerAccountID, pageSize, offset)
}

func (r *PostgresProductTransactionRepository) GetByBuyerAccountID(ctx context.Context, buyerAccountID string, page, pageSize int) ([]*domain.ProductTransaction, error) {
	offset := (page - 1) * pageSize
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE buyer_account_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	return r.scanMany(ctx, query, buyerAccountID, pageSize, offset)
}

// GetBySellerAccountIDWithCursor returns transactions with cursor-based pagination using RandId.
// This mimics redifu's infinite scrolling pattern where RandId is used as the cursor.
// Since RandId is a random string, we use it to identify the starting position,
// but actual sorting is done on created_at field.
// sortOrder: "ASC" or "DESC" (defaults to DESC if invalid)
func (r *PostgresProductTransactionRepository) GetBySellerAccountIDWithCursor(ctx context.Context, sellerAccountID string, cursor string, pageSize int, sortOrder string) ([]*domain.ProductTransaction, error) {
	var query string
	var args []any

	// Normalize sort order
	if sortOrder != "ASC" && sortOrder != "DESC" {
		sortOrder = "DESC"
	}

	if cursor == "" {
		// First page: no cursor, start from beginning
		query = fmt.Sprintf(`
			SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
			       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
			       status, created_at, updated_at, completed_at, settled_at,
			       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
			FROM product_transactions
			WHERE seller_account_id = $1
			ORDER BY created_at %s
			LIMIT $2
		`, sortOrder)
		args = []any{sellerAccountID, pageSize}
	} else {
		// Subsequent pages: find the cursor item first, then get items after it
		// Use a subquery to get the created_at of the cursor item
		if sortOrder == "DESC" {
			query = `
				SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
				       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
				       status, created_at, updated_at, completed_at, settled_at,
				       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
				FROM product_transactions
				WHERE seller_account_id = $1 
				  AND (created_at < (SELECT created_at FROM product_transactions WHERE randid = $2)
				       OR (created_at = (SELECT created_at FROM product_transactions WHERE randid = $2) AND randid < $2))
				ORDER BY created_at DESC, randid DESC
				LIMIT $3
			`
		} else {
			query = `
				SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
				       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
				       status, created_at, updated_at, completed_at, settled_at,
				       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
				FROM product_transactions
				WHERE seller_account_id = $1 
				  AND (created_at > (SELECT created_at FROM product_transactions WHERE randid = $2)
				       OR (created_at = (SELECT created_at FROM product_transactions WHERE randid = $2) AND randid > $2))
				ORDER BY created_at ASC, randid ASC
				LIMIT $3
			`
		}
		args = []any{sellerAccountID, cursor, pageSize}
	}

	return r.scanMany(ctx, query, args...)
}

func (r *PostgresProductTransactionRepository) GetPendingBySellerAccountID(ctx context.Context, sellerAccountID string) ([]*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE seller_account_id = $1 AND status = 'PENDING'
		ORDER BY created_at DESC
	`

	return r.scanMany(ctx, query, sellerAccountID)
}

func (r *PostgresProductTransactionRepository) GetCompletedNotSettled(ctx context.Context, sellerAccountID string) ([]*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE seller_account_id = $1 AND status = 'COMPLETED'
		ORDER BY created_at ASC
	`

	return r.scanMany(ctx, query, sellerAccountID)
}

func (r *PostgresProductTransactionRepository) GetAllBySellerID(ctx context.Context, sellerAccountID string) ([]*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE seller_account_id = $1
		ORDER BY created_at DESC
	`

	return r.scanMany(ctx, query, sellerAccountID)
}

func (r *PostgresProductTransactionRepository) Save(ctx context.Context, tx *domain.ProductTransaction) error {
	metadataJSON, err := json.Marshal(tx.Metadata)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	// Convert *time.Time to sql.NullTime to properly handle NULL values
	completedAt := toNullTime(tx.CompletedAt)
	settledAt := toNullTime(tx.SettledAt)
	platformFeeTransferredAt := toNullTime(tx.PlatformFeeTransferredAt)

	query := `
		INSERT INTO product_transactions (
			uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
			seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
			status, created_at, updated_at, completed_at, settled_at,
			platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		ON CONFLICT (uuid) DO UPDATE SET
			status = EXCLUDED.status,
			completed_at = EXCLUDED.completed_at,
			settled_at = EXCLUDED.settled_at,
			platform_fee_transferred = EXCLUDED.platform_fee_transferred,
			platform_fee_transferred_at = EXCLUDED.platform_fee_transferred_at,
			transfer_request_id = EXCLUDED.transfer_request_id,
			metadata = EXCLUDED.metadata
	`

	slog.InfoContext(ctx, "Saving ProductTransaction", "product_transaction", tx)
	slog.InfoContext(ctx, "metadataJSON", "metadata_json", string(metadataJSON))

	_, err = r.db.ExecContext(
		ctx,
		query,
		tx.UUID,
		tx.RandId,
		tx.BuyerAccountID,
		tx.SellerAccountID,
		tx.ProductID,
		tx.ProductType,
		tx.InvoiceNumber,
		tx.Fee.SellerPrice,
		tx.Fee.PlatformFee,
		tx.Fee.GatewayFee,
		tx.Fee.TotalCharged,
		tx.Fee.SellerNetAmount,
		tx.Fee.FeeModel,
		tx.Fee.Currency,
		tx.Status,
		tx.CreatedAt,
		tx.UpdatedAt,
		completedAt,
		settledAt,
		tx.PlatformFeeTransferred,
		platformFeeTransferredAt,
		toNullString(tx.TransferRequestID),
		string(metadataJSON),
	)
	if err != nil {
		pqErr, ok := err.(*pq.Error)
		if ok {
			slog.ErrorContext(ctx, "PostgreSQL error", "error", pqErr.Message, "code", pqErr.Code, "detail", pqErr.Detail, "position", pqErr.Position)
		}
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresProductTransactionRepository) UpdateStatus(ctx context.Context, id string, status domain.TransactionStatus, timestamp time.Time) error {
	var query string
	var args []any

	switch status {
	case domain.TransactionStatusCompleted:
		query = `UPDATE product_transactions SET status = $1, completed_at = $2 WHERE uuid = $3`
		args = []any{status, timestamp, id}
	case domain.TransactionStatusSettled:
		query = `UPDATE product_transactions SET status = $1, settled_at = $2 WHERE uuid = $3`
		args = []any{status, timestamp, id}
	default:
		query = `UPDATE product_transactions SET status = $1 WHERE uuid = $2`
		args = []any{status, id}
	}

	result, err := r.db.ExecContext(ctx, query, args...)
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

// UpdateStatusIf performs the compare-and-set described on
// domain.ProductTransactionRepository. A false return is not an error: it is the answer
// "somebody else already moved this row".
func (r *PostgresProductTransactionRepository) UpdateStatusIf(ctx context.Context, id string, from, to domain.TransactionStatus, timestamp time.Time) (bool, error) {
	var query string
	var args []any

	switch to {
	case domain.TransactionStatusCompleted:
		query = `UPDATE product_transactions SET status = $1, completed_at = $2 WHERE uuid = $3 AND status = $4`
		args = []any{to, timestamp, id, from}
	case domain.TransactionStatusSettled:
		query = `UPDATE product_transactions SET status = $1, settled_at = $2 WHERE uuid = $3 AND status = $4`
		args = []any{to, timestamp, id, from}
	default:
		query = `UPDATE product_transactions SET status = $1 WHERE uuid = $2 AND status = $3`
		args = []any{to, id, from}
	}

	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, ErrFailedInsertSQL.WithError(err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, ErrFailedQuerySQL.WithError(err)
	}

	return rowsAffected > 0, nil
}

// scanOne scans a single row into a ProductTransaction
func (r *PostgresProductTransactionRepository) scanOne(ctx context.Context, query string, args ...any) (*domain.ProductTransaction, error) {
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

// scanMany scans multiple rows into ProductTransactions
func (r *PostgresProductTransactionRepository) scanMany(ctx context.Context, query string, args ...any) ([]*domain.ProductTransaction, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	var transactions []*domain.ProductTransaction
	for rows.Next() {
		tx, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		transactions = append(transactions, tx)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return transactions, nil
}

// scanRow scans a single row into a ProductTransaction
func (r *PostgresProductTransactionRepository) scanRow(rows *sql.Rows) (*domain.ProductTransaction, error) {
	var row struct {
		UUID                     string
		RandId                   string
		BuyerAccountID           string
		SellerAccountID          string
		ProductID                string
		ProductType              string
		InvoiceNumber            string
		SellerPrice              int64
		PlatformFee              int64
		GatewayFee               int64
		TotalCharged             int64
		SellerNetAmount          int64
		FeeModel                 string
		Currency                 string
		Status                   string
		CreatedAt                time.Time
		UpdatedAt                time.Time
		CompletedAt              sql.NullTime
		SettledAt                sql.NullTime
		PlatformFeeTransferred   bool
		PlatformFeeTransferredAt sql.NullTime
		TransferRequestID        sql.NullString
		Metadata                 []byte
		SettledPlatformFeeMinor  sql.NullInt64
		SettledGatewayFeeMinor   sql.NullInt64
		PlatformResidualMinor    sql.NullInt64
	}

	err := rows.Scan(
		&row.UUID,
		&row.RandId,
		&row.BuyerAccountID,
		&row.SellerAccountID,
		&row.ProductID,
		&row.ProductType,
		&row.InvoiceNumber,
		&row.SellerPrice,
		&row.PlatformFee,
		&row.GatewayFee,
		&row.TotalCharged,
		&row.SellerNetAmount,
		&row.FeeModel,
		&row.Currency,
		&row.Status,
		&row.CreatedAt,
		&row.UpdatedAt,
		&row.CompletedAt,
		&row.SettledAt,
		&row.PlatformFeeTransferred,
		&row.PlatformFeeTransferredAt,
		&row.TransferRequestID,
		&row.Metadata,
		&row.SettledPlatformFeeMinor,
		&row.SettledGatewayFeeMinor,
		&row.PlatformResidualMinor,
	)
	if err != nil {
		return nil, ErrFailedScanSQL.WithError(err)
	}

	var completedAt *time.Time
	if row.CompletedAt.Valid {
		completedAt = &row.CompletedAt.Time
	}

	var settledAt *time.Time
	if row.SettledAt.Valid {
		settledAt = &row.SettledAt.Time
	}

	var platformFeeTransferredAt *time.Time
	if row.PlatformFeeTransferredAt.Valid {
		platformFeeTransferredAt = &row.PlatformFeeTransferredAt.Time
	}

	// NULL here means "not recorded" — a transaction that settled before these columns
	// existed, or one that has not settled. It is not zero, and must not collapse to it:
	// the platform fee transfer reads this and falls back to the priced figure.
	var settledPlatformFeeMinor *int64
	if row.SettledPlatformFeeMinor.Valid {
		settledPlatformFeeMinor = &row.SettledPlatformFeeMinor.Int64
	}

	var settledGatewayFeeMinor *int64
	if row.SettledGatewayFeeMinor.Valid {
		settledGatewayFeeMinor = &row.SettledGatewayFeeMinor.Int64
	}

	var platformResidualMinor *int64
	if row.PlatformResidualMinor.Valid {
		platformResidualMinor = &row.PlatformResidualMinor.Int64
	}

	var metadata map[string]any
	if len(row.Metadata) > 0 {
		if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
	}

	tx := &domain.ProductTransaction{
		BuyerAccountID:  row.BuyerAccountID,
		SellerAccountID: row.SellerAccountID,
		ProductID:       row.ProductID,
		ProductType:     row.ProductType,
		InvoiceNumber:   row.InvoiceNumber,
		Fee: domain.FeeBreakdown{
			SellerPrice:     row.SellerPrice,
			PlatformFee:     row.PlatformFee,
			GatewayFee:      row.GatewayFee,
			TotalCharged:    row.TotalCharged,
			SellerNetAmount: row.SellerNetAmount,
			FeeModel:        domain.FeeModel(row.FeeModel),
			Currency:        domain.Currency(row.Currency),
		},
		Status:                   domain.TransactionStatus(row.Status),
		Metadata:                 metadata,
		CompletedAt:              completedAt,
		SettledAt:                settledAt,
		PlatformFeeTransferred:   row.PlatformFeeTransferred,
		PlatformFeeTransferredAt: platformFeeTransferredAt,
		TransferRequestID:        row.TransferRequestID.String,
		SettledPlatformFeeMinor:  settledPlatformFeeMinor,
		SettledGatewayFeeMinor:   settledGatewayFeeMinor,
		PlatformResidualMinor:    platformResidualMinor,
	}
	redifu.InitRecord(tx)
	// Override auto-generated values with database values
	tx.UUID = row.UUID
	tx.RandId = row.RandId
	tx.CreatedAt = row.CreatedAt
	tx.UpdatedAt = row.UpdatedAt
	return tx, nil
}

// SaveTransferRequestID persists the merchant_ref_no that will be used for the platform fee
// transfer. Must be written before the account transfer is sent so the reference is
// available for idempotent retries.
func (r *PostgresProductTransactionRepository) SaveTransferRequestID(ctx context.Context, id string, requestID string) error {
	query := `
		UPDATE product_transactions
		SET transfer_request_id = $1,
		    updated_at = NOW()
		WHERE uuid = $2
	`

	result, err := r.db.ExecContext(ctx, query, requestID, id)
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

// MarkPlatformFeeTransferred marks a transaction as having its platform fee successfully transferred
func (r *PostgresProductTransactionRepository) MarkPlatformFeeTransferred(ctx context.Context, id string) error {
	now := time.Now()
	query := `
		UPDATE product_transactions 
		SET platform_fee_transferred = true, 
		    platform_fee_transferred_at = $1,
		    updated_at = $1
		WHERE uuid = $2
	`

	result, err := r.db.ExecContext(ctx, query, now, id)
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

// GetSettledWithoutPlatformFeeTransfer returns SETTLED transactions that haven't had platform fees transferred yet
// Used by background job to retry failed platform fee transfers
func (r *PostgresProductTransactionRepository) GetSettledWithoutPlatformFeeTransfer(ctx context.Context, limit int) ([]*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE status = 'SETTLED' 
		  AND platform_fee_transferred = false 
		  AND platform_fee > 0
		ORDER BY settled_at ASC
		LIMIT $1
	`

	return r.scanMany(ctx, query, limit)
}

// GetAwaitingSettlement returns COMPLETED transactions whose funds have not settled yet,
// oldest first.
//
// This is what makes reconciliation possible without a settlement file. Singapay announces
// a batch and a date window and nothing else, so the reconciler has to know which invoices
// it is waiting on before it can go looking for them — and that set is exactly this. It
// also bounds the work: only accounts holding unsettled money are queried at the gateway,
// rather than every sub-account the merchant owns.
func (r *PostgresProductTransactionRepository) GetAwaitingSettlement(ctx context.Context, limit int) ([]*domain.ProductTransaction, error) {
	query := `
		SELECT uuid, randid, buyer_account_id, seller_account_id, product_id, product_type, invoice_number,
		       seller_price, platform_fee, gateway_fee, total_charged, seller_net_amount, fee_model, currency,
		       status, created_at, updated_at, completed_at, settled_at,
		       platform_fee_transferred, platform_fee_transferred_at, transfer_request_id, metadata,
		       settled_platform_fee, settled_gateway_fee, platform_residual
		FROM product_transactions
		WHERE status = 'COMPLETED'
		ORDER BY completed_at ASC
		LIMIT $1
	`

	return r.scanMany(ctx, query, limit)
}

// SaveSettledFees records what the fees turned out to be once Singapay reported what it
// actually took.
//
// Written inside the same transaction as the COMPLETED -> SETTLED move, so a transaction
// can never reach SETTLED with these unset — ProcessPlatformFeeTransfer reads them the
// moment the status changes, and an unset value there means it silently moves the priced
// figure instead of the booked one.
//
// The Go names carry a Minor suffix and the columns do not. All of them are sen: the
// columns were left named as migration 024 created them rather than renamed by 027, since
// the tables were near-empty when the unit changed and a rename would have churned a dozen
// queries. The suffix is kept in Go because that is where it earns its place — the ×100 bug
// this replaced was a rupiah figure assigned to a sen field, and a compiler cannot catch
// that when neither name says which is which.
func (r *PostgresProductTransactionRepository) SaveSettledFees(ctx context.Context, id string, platformFeeMinor, gatewayFeeMinor, residualMinor int64) error {
	query := `
		UPDATE product_transactions
		SET settled_platform_fee = $1,
		    settled_gateway_fee = $2,
		    platform_residual = $3,
		    updated_at = $4
		WHERE uuid = $5
	`

	result, err := r.db.ExecContext(ctx, query, platformFeeMinor, gatewayFeeMinor, residualMinor, time.Now(), id)
	if err != nil {
		return ErrFailedUpdateSQL.WithError(err)
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

// OldestAwaitingSettlement returns when the oldest unsettled COMPLETED transaction was
// completed, and false when there are none.
//
// This is the settlement worker's health signal, and the one alarm a reconciler cannot
// fool by running cleanly and booking nothing: if settlement stops for any reason — a
// webhook that never arrived, a pass that matched nothing, a worker that is not running —
// this age climbs and keeps climbing. Served as a one-row index scan by
// idx_product_transactions_awaiting_settlement.
func (r *PostgresProductTransactionRepository) OldestAwaitingSettlement(ctx context.Context) (time.Time, bool, error) {
	query := `
		SELECT MIN(completed_at)
		FROM product_transactions
		WHERE status = 'COMPLETED'
	`

	var oldest sql.NullTime
	if err := r.db.QueryRowContext(ctx, query).Scan(&oldest); err != nil {
		return time.Time{}, false, ErrFailedQuerySQL.WithError(err)
	}
	if !oldest.Valid {
		return time.Time{}, false, nil
	}

	return oldest.Time, true, nil
}
