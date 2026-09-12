package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/21strive/redifu"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresSettlementNotificationRepository struct {
	db DBTX
}

func NewPostgresSettlementNotificationRepository(db DBTX) *PostgresSettlementNotificationRepository {
	return &PostgresSettlementNotificationRepository{db: db}
}

// settlementNotificationColumns is the read projection. The INSERT spells its own columns
// out; see Save.
const settlementNotificationColumns = `
	uuid, randid, settlement_id, settlement_reference, event,
	settlement_method, settlement_type, start_date, end_date,
	total_transactions, amount, total_fee, currency,
	raw_payload, status, failure_reason,
	received_at, processed_at, created_at, updated_at`

// Save stores a verified delivery.
//
// ON CONFLICT DO NOTHING against the (settlement_id, event) identity, and the reported
// `stored` is how the caller learns which happened. A redelivery is ordinary traffic —
// Singapay retries, and the handler must still answer 200 either way, because answering
// anything else teaches it to retry a delivery that was already accepted.
func (r *PostgresSettlementNotificationRepository) Save(ctx context.Context, n *domain.SettlementNotification) (bool, error) {
	// The column list is spelled out here rather than reusing settlementNotificationColumns
	// because TestInsertStatementsHaveMatchingArity reads the literal: a concatenated
	// constant parses as one column and the guard silently stops guarding.
	query := `
		INSERT INTO settlement_notifications (
			uuid, randid, settlement_id, settlement_reference, event,
			settlement_method, settlement_type, start_date, end_date,
			total_transactions, amount, total_fee, currency,
			raw_payload, status, failure_reason,
			received_at, processed_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		ON CONFLICT (settlement_id, event) DO NOTHING
	`

	result, err := r.db.ExecContext(
		ctx,
		query,
		n.UUID,
		n.RandId,
		n.SettlementID,
		n.SettlementReference,
		n.Event,
		toNullString(n.Method),
		toNullString(n.Type),
		toNullTime(n.StartDate),
		toNullTime(n.EndDate),
		n.TotalTransactions,
		n.Amount,
		n.TotalFee,
		toNullString(n.Currency),
		n.RawPayload,
		n.Status,
		toNullString(n.FailureReason),
		n.ReceivedAt,
		toNullTime(n.ProcessedAt),
		n.CreatedAt,
		n.UpdatedAt,
	)
	if err != nil {
		return false, ErrFailedInsertSQL.WithError(err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, ErrFailedQuerySQL.WithError(err)
	}

	return rowsAffected > 0, nil
}

func (r *PostgresSettlementNotificationRepository) GetByID(ctx context.Context, id string) (*domain.SettlementNotification, error) {
	query := `SELECT ` + settlementNotificationColumns + ` FROM settlement_notifications WHERE uuid = $1`
	return r.scanOne(ctx, query, id)
}

func (r *PostgresSettlementNotificationRepository) GetByIdentity(ctx context.Context, settlementID, event string) (*domain.SettlementNotification, error) {
	query := `SELECT ` + settlementNotificationColumns + `
		FROM settlement_notifications WHERE settlement_id = $1 AND event = $2`
	return r.scanOne(ctx, query, settlementID, event)
}

// GetActionable returns deliveries still waiting for a settling pass, oldest first.
//
// FAILED is included deliberately: a pass that errored has not been acted on, and leaving
// it out would mean one transport error permanently strands a settlement. NEEDS_REVIEW is
// excluded just as deliberately — those need a person, and re-picking them on every tick
// would turn a decision into a loop.
func (r *PostgresSettlementNotificationRepository) GetActionable(ctx context.Context, limit int) ([]*domain.SettlementNotification, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT ` + settlementNotificationColumns + `
		FROM settlement_notifications
		WHERE status IN ('PENDING', 'FAILED')
		ORDER BY received_at ASC
		LIMIT $1`

	return r.scanMany(ctx, query, limit)
}

// CountActionable answers "is there anything to do?" without loading rows. The settlement
// worker asks this first on every tick, and on a quiet tick it is the only query it runs.
func (r *PostgresSettlementNotificationRepository) CountActionable(ctx context.Context) (int, error) {
	query := `SELECT COUNT(*) FROM settlement_notifications WHERE status IN ('PENDING', 'FAILED')`

	var count int
	if err := r.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, ErrFailedQuerySQL.WithError(err)
	}

	return count, nil
}

// ClaimIfActionable moves a row to PROCESSING only if it is still actionable.
//
// Conditional for the same reason UpdateStatusIf is: two workers on one row is a race, not
// a rarity — a slow pass overlapping the next tick is enough. The loser gets false and
// leaves the row alone.
func (r *PostgresSettlementNotificationRepository) ClaimIfActionable(ctx context.Context, id string) (bool, error) {
	query := `
		UPDATE settlement_notifications
		SET status = 'PROCESSING', updated_at = $1
		WHERE uuid = $2 AND status IN ('PENDING', 'FAILED')
	`

	result, err := r.db.ExecContext(ctx, query, time.Now(), id)
	if err != nil {
		return false, ErrFailedUpdateSQL.WithError(err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, ErrFailedQuerySQL.WithError(err)
	}

	return rowsAffected > 0, nil
}

func (r *PostgresSettlementNotificationRepository) MarkProcessed(ctx context.Context, id string) error {
	query := `
		UPDATE settlement_notifications
		SET status = 'PROCESSED', processed_at = $1, failure_reason = NULL, updated_at = $1
		WHERE uuid = $2
	`
	return r.exec(ctx, query, time.Now(), id)
}

func (r *PostgresSettlementNotificationRepository) MarkFailed(ctx context.Context, id string, reason string) error {
	query := `
		UPDATE settlement_notifications
		SET status = 'FAILED', failure_reason = $1, updated_at = $2
		WHERE uuid = $3
	`
	return r.exec(ctx, query, reason, time.Now(), id)
}

func (r *PostgresSettlementNotificationRepository) MarkNeedsReview(ctx context.Context, id string, reason string) error {
	query := `
		UPDATE settlement_notifications
		SET status = 'NEEDS_REVIEW', failure_reason = $1, updated_at = $2
		WHERE uuid = $3
	`
	return r.exec(ctx, query, reason, time.Now(), id)
}

func (r *PostgresSettlementNotificationRepository) exec(ctx context.Context, query string, args ...any) error {
	result, err := r.db.ExecContext(ctx, query, args...)
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

func (r *PostgresSettlementNotificationRepository) scanOne(ctx context.Context, query string, args ...any) (*domain.SettlementNotification, error) {
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

func (r *PostgresSettlementNotificationRepository) scanMany(ctx context.Context, query string, args ...any) ([]*domain.SettlementNotification, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	var notifications []*domain.SettlementNotification
	for rows.Next() {
		n, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, n)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return notifications, nil
}

func (r *PostgresSettlementNotificationRepository) scanRow(rows *sql.Rows) (*domain.SettlementNotification, error) {
	var row struct {
		UUID                string
		RandId              string
		SettlementID        string
		SettlementReference string
		Event               string
		Method              sql.NullString
		Type                sql.NullString
		StartDate           sql.NullTime
		EndDate             sql.NullTime
		TotalTransactions   sql.NullInt64
		Amount              sql.NullInt64
		TotalFee            sql.NullInt64
		Currency            sql.NullString
		RawPayload          []byte
		Status              string
		FailureReason       sql.NullString
		ReceivedAt          time.Time
		ProcessedAt         sql.NullTime
		CreatedAt           time.Time
		UpdatedAt           time.Time
	}

	err := rows.Scan(
		&row.UUID,
		&row.RandId,
		&row.SettlementID,
		&row.SettlementReference,
		&row.Event,
		&row.Method,
		&row.Type,
		&row.StartDate,
		&row.EndDate,
		&row.TotalTransactions,
		&row.Amount,
		&row.TotalFee,
		&row.Currency,
		&row.RawPayload,
		&row.Status,
		&row.FailureReason,
		&row.ReceivedAt,
		&row.ProcessedAt,
		&row.CreatedAt,
		&row.UpdatedAt,
	)
	if err != nil {
		return nil, ErrFailedScanSQL.WithError(err)
	}

	var startDate *time.Time
	if row.StartDate.Valid {
		startDate = &row.StartDate.Time
	}

	var endDate *time.Time
	if row.EndDate.Valid {
		endDate = &row.EndDate.Time
	}

	var processedAt *time.Time
	if row.ProcessedAt.Valid {
		processedAt = &row.ProcessedAt.Time
	}

	n := &domain.SettlementNotification{
		SettlementID:        row.SettlementID,
		SettlementReference: row.SettlementReference,
		Event:               row.Event,
		Method:              row.Method.String,
		Type:                row.Type.String,
		StartDate:           startDate,
		EndDate:             endDate,
		TotalTransactions:   int(row.TotalTransactions.Int64),
		Amount:              row.Amount.Int64,
		TotalFee:            row.TotalFee.Int64,
		Currency:            row.Currency.String,
		RawPayload:          row.RawPayload,
		Status:              domain.SettlementNotificationStatus(row.Status),
		FailureReason:       row.FailureReason.String,
		ReceivedAt:          row.ReceivedAt,
		ProcessedAt:         processedAt,
	}
	redifu.InitRecord(n)
	// Override auto-generated values with database values
	n.UUID = row.UUID
	n.RandId = row.RandId
	n.CreatedAt = row.CreatedAt
	n.UpdatedAt = row.UpdatedAt

	return n, nil
}
