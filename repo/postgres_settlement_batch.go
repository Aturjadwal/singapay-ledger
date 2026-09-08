package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/redifu"
	"github.com/lib/pq"
)

type PostgresSettlementBatchRepository struct {
	db DBTX
}

func NewPostgresSettlementBatchRepository(db DBTX) *PostgresSettlementBatchRepository {
	return &PostgresSettlementBatchRepository{db: db}
}

func (r *PostgresSettlementBatchRepository) GetByID(ctx context.Context, id string) (*domain.SettlementBatch, error) {
	query := `
		SELECT uuid, randid, account_uuid, settlement_reference, settlement_date,
		       settle_from, settle_to,
		       batch_id, gross_amount, net_amount, gateway_fee, currency,
		       initiated_by, initiated_at, processed_at, processing_status,
		       matched_count, unmatched_count, failure_reason, metadata, created_at, updated_at
		FROM settlement_batches
		WHERE uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, id)
	return r.scanSettlementBatch(row)
}

func (r *PostgresSettlementBatchRepository) GetByLedgerID(ctx context.Context, ledgerID string, page, pageSize int) ([]*domain.SettlementBatch, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	query := `
		SELECT uuid, randid, account_uuid, settlement_reference, settlement_date,
		       settle_from, settle_to,
		       batch_id, gross_amount, net_amount, gateway_fee, currency,
		       initiated_by, initiated_at, processed_at, processing_status,
		       matched_count, unmatched_count, failure_reason, metadata, created_at, updated_at
		FROM settlement_batches
		WHERE account_uuid = $1
		ORDER BY settlement_date DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, ledgerID, pageSize, offset)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanSettlementBatches(rows)
}

func (r *PostgresSettlementBatchRepository) GetByLedgerIDAndDate(ctx context.Context, ledgerID string, settlementDate time.Time) (*domain.SettlementBatch, error) {
	query := `
		SELECT uuid, randid, account_uuid, settlement_reference, settlement_date,
		       settle_from, settle_to,
		       batch_id, gross_amount, net_amount, gateway_fee, currency,
		       initiated_by, initiated_at, processed_at, processing_status,
		       matched_count, unmatched_count, failure_reason, metadata, created_at, updated_at
		FROM settlement_batches
		WHERE account_uuid = $1 AND DATE(settlement_date) = DATE($2)
	`

	row := r.db.QueryRowContext(ctx, query, ledgerID, settlementDate)
	return r.scanSettlementBatch(row)
}

func (r *PostgresSettlementBatchRepository) GetByBatchID(ctx context.Context, batchID string) (*domain.SettlementBatch, error) {
	// An empty batch_id never matches. Rows predating migration 005 carry NULL, and
	// domain.NewSettlementBatch refuses to build a batch without one, so nothing this
	// package writes is empty either. Answering ErrNotFound keeps the caller on the
	// "not yet ingested" branch rather than letting an empty string wander into the
	// query and match on some future schema where the column is NOT NULL.
	if batchID == "" {
		return nil, ErrNotFound
	}

	// Oldest first: duplicates are impossible once the unique index from migration
	// 013 exists, but on a database that predates it the original ingest is the
	// meaningful one to report back.
	query := `
		SELECT uuid, randid, account_uuid, settlement_reference, settlement_date,
		       settle_from, settle_to,
		       batch_id, gross_amount, net_amount, gateway_fee, currency,
		       initiated_by, initiated_at, processed_at, processing_status,
		       matched_count, unmatched_count, failure_reason, metadata, created_at, updated_at
		FROM settlement_batches
		WHERE batch_id = $1
		ORDER BY created_at ASC
		LIMIT 1
	`

	row := r.db.QueryRowContext(ctx, query, batchID)
	return r.scanSettlementBatch(row)
}

func (r *PostgresSettlementBatchRepository) FilterIngestedBatchIDs(ctx context.Context, batchIDs []string) (map[string]struct{}, error) {
	ingested := make(map[string]struct{}, len(batchIDs))

	if len(batchIDs) == 0 {
		return ingested, nil
	}

	// Backed by the unique index on batch_id (migration 013). Without it this
	// sequentially scans the whole table on every reconciler tick.
	query := `SELECT batch_id FROM settlement_batches WHERE batch_id = ANY($1)`

	rows, err := r.db.QueryContext(ctx, query, pq.Array(batchIDs))
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
		ingested[name] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return ingested, nil
}

func (r *PostgresSettlementBatchRepository) Save(ctx context.Context, batch *domain.SettlementBatch) error {
	metadataJSON, err := json.Marshal(batch.Metadata)
	if err != nil {
		metadataJSON = []byte("{}")
	}

	query := `
		INSERT INTO settlement_batches (
			uuid, randid, account_uuid, settlement_reference, settlement_date,
			settle_from, settle_to,
			batch_id, gross_amount, net_amount, gateway_fee, currency,
			initiated_by, initiated_at, processed_at, processing_status,
			matched_count, unmatched_count, failure_reason, metadata,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		ON CONFLICT (uuid) DO UPDATE SET
			batch_id = EXCLUDED.batch_id,
			gross_amount = EXCLUDED.gross_amount,
			net_amount = EXCLUDED.net_amount,
			gateway_fee = EXCLUDED.gateway_fee,
			processed_at = EXCLUDED.processed_at,
			processing_status = EXCLUDED.processing_status,
			matched_count = EXCLUDED.matched_count,
			unmatched_count = EXCLUDED.unmatched_count,
			failure_reason = EXCLUDED.failure_reason,
			metadata = EXCLUDED.metadata,
			updated_at = EXCLUDED.updated_at
	`

	_, err = r.db.ExecContext(ctx, query,
		batch.UUID, batch.RandId, batch.LedgerUUID,
		batch.SettlementReference,
		batch.SettlementDate,
		batch.SettleFrom,
		batch.SettleTo,
		batch.BatchID,
		batch.GrossAmount,
		batch.NetAmount,
		batch.GatewayFee,
		batch.Currency,
		batch.InitiatedBy,
		batch.InitiatedAt,
		toNullTime(batch.ProcessedAt),
		batch.ProcessingStatus,
		batch.MatchedCount,
		batch.UnmatchedCount,
		batch.FailureReason,
		metadataJSON,
		batch.CreatedAt,
		batch.UpdatedAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresSettlementBatchRepository) UpdateStatus(ctx context.Context, id string, status domain.SettlementBatchStatus, processedAt *time.Time, failureReason string) error {
	query := `
		UPDATE settlement_batches
		SET processing_status = $2, processed_at = $3, failure_reason = $4
		WHERE uuid = $1
	`

	result, err := r.db.ExecContext(ctx, query, id, status, toNullTime(processedAt), failureReason)
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

func (r *PostgresSettlementBatchRepository) scanSettlementBatch(row *sql.Row) (*domain.SettlementBatch, error) {
	var batch domain.SettlementBatch
	redifu.InitRecord(&batch)
	var processedAt sql.NullTime
	var failureReason sql.NullString
	var batchID sql.NullString
	var settlementReference sql.NullString
	var metadataJSON []byte

	err := row.Scan(
		&batch.UUID,
		&batch.RandId,
		&batch.LedgerUUID,
		&settlementReference,
		&batch.SettlementDate,
		&batch.SettleFrom,
		&batch.SettleTo,
		&batchID,
		&batch.GrossAmount,
		&batch.NetAmount,
		&batch.GatewayFee,
		&batch.Currency,
		&batch.InitiatedBy,
		&batch.InitiatedAt,
		&processedAt,
		&batch.ProcessingStatus,
		&batch.MatchedCount,
		&batch.UnmatchedCount,
		&failureReason,
		&metadataJSON,
		&batch.CreatedAt,
		&batch.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	if processedAt.Valid {
		batch.ProcessedAt = &processedAt.Time
	}
	if failureReason.Valid {
		batch.FailureReason = failureReason.String
	}
	if batchID.Valid {
		batch.BatchID = batchID.String
	}
	if settlementReference.Valid {
		batch.SettlementReference = settlementReference.String
	}

	batch.Metadata = make(map[string]any)
	if len(metadataJSON) > 0 {
		_ = json.Unmarshal(metadataJSON, &batch.Metadata)
	}

	return &batch, nil
}

func (r *PostgresSettlementBatchRepository) scanSettlementBatches(rows *sql.Rows) ([]*domain.SettlementBatch, error) {
	var batches []*domain.SettlementBatch

	for rows.Next() {
		var batch domain.SettlementBatch
		redifu.InitRecord(&batch)
		var processedAt sql.NullTime
		var failureReason sql.NullString
		var batchID sql.NullString
		var settlementReference sql.NullString
		var metadataJSON []byte

		err := rows.Scan(
			&batch.UUID,
			&batch.RandId,
			&batch.LedgerUUID,
			&settlementReference,
			&batch.SettlementDate,
			&batch.SettleFrom,
			&batch.SettleTo,
			&batchID,
			&batch.GrossAmount,
			&batch.NetAmount,
			&batch.GatewayFee,
			&batch.Currency,
			&batch.InitiatedBy,
			&batch.InitiatedAt,
			&processedAt,
			&batch.ProcessingStatus,
			&batch.MatchedCount,
			&batch.UnmatchedCount,
			&failureReason,
			&metadataJSON,
			&batch.CreatedAt,
			&batch.UpdatedAt,
		)
		if err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}

		if processedAt.Valid {
			batch.ProcessedAt = &processedAt.Time
		}
		if failureReason.Valid {
			batch.FailureReason = failureReason.String
		}
		if batchID.Valid {
			batch.BatchID = batchID.String
		}
		if settlementReference.Valid {
			batch.SettlementReference = settlementReference.String
		}

		batch.Metadata = make(map[string]any)
		if len(metadataJSON) > 0 {
			_ = json.Unmarshal(metadataJSON, &batch.Metadata)
		}

		batches = append(batches, &batch)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return batches, nil
}
