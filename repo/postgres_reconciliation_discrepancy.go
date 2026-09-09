package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresReconciliationDiscrepancyRepository struct {
	db DBTX
}

func NewPostgresReconciliationDiscrepancyRepository(db DBTX) *PostgresReconciliationDiscrepancyRepository {
	return &PostgresReconciliationDiscrepancyRepository{db: db}
}

func (r *PostgresReconciliationDiscrepancyRepository) Save(ctx context.Context, discrepancy *domain.ReconciliationDiscrepancy) error {
	status := discrepancy.Status
	if status == "" {
		status = domain.DiscrepancyStatusPending
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO reconciliation_discrepancies (
			uuid, randid, account_uuid, settlement_batch_uuid, discrepancy_type,
			expected_pending, actual_pending, expected_available, actual_available,
			pending_diff, available_diff,
			item_discrepancy_count, total_item_discrepancy,
			status, detected_at, resolved_at, resolution_notes,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (account_uuid, settlement_batch_uuid) DO UPDATE SET
			status = EXCLUDED.status,
			resolved_at = EXCLUDED.resolved_at,
			resolution_notes = EXCLUDED.resolution_notes,
			updated_at = EXCLUDED.updated_at
	`,
		discrepancy.UUID, discrepancy.RandId, discrepancy.LedgerUUID,
		discrepancy.SettlementBatchUUID,
		string(discrepancy.DiscrepancyType),
		discrepancy.ExpectedPending,
		discrepancy.ActualPending,
		discrepancy.ExpectedAvailable,
		discrepancy.ActualAvailable,
		discrepancy.PendingDiff,
		discrepancy.AvailableDiff,
		discrepancy.ItemDiscrepancyCount,
		discrepancy.TotalItemDiscrepancy,
		string(status),
		discrepancy.DetectedAt,
		toNullTime(discrepancy.ResolvedAt),
		discrepancy.ResolutionNotes,
		discrepancy.CreatedAt,
		discrepancy.UpdatedAt,
	)

	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}
	return nil
}

func (r *PostgresReconciliationDiscrepancyRepository) GetByID(ctx context.Context, id string) (*domain.ReconciliationDiscrepancy, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT uuid, randid, account_uuid, settlement_batch_uuid, discrepancy_type,
		       expected_pending, actual_pending, expected_available, actual_available,
		       pending_diff, available_diff,
		       item_discrepancy_count, total_item_discrepancy,
		       status, detected_at, resolved_at, resolution_notes, created_at, updated_at
		FROM reconciliation_discrepancies
		WHERE uuid = $1
	`, id)

	return r.scanRow(row)
}

func (r *PostgresReconciliationDiscrepancyRepository) GetBySettlementBatchID(ctx context.Context, batchID string) (*domain.ReconciliationDiscrepancy, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT uuid, randid, account_uuid, settlement_batch_uuid, discrepancy_type,
		       expected_pending, actual_pending, expected_available, actual_available,
		       pending_diff, available_diff,
		       item_discrepancy_count, total_item_discrepancy,
		       status, detected_at, resolved_at, resolution_notes, created_at, updated_at
		FROM reconciliation_discrepancies
		WHERE settlement_batch_uuid = $1
	`, batchID)

	return r.scanRow(row)
}

func (r *PostgresReconciliationDiscrepancyRepository) GetByLedgerID(ctx context.Context, ledgerID string, limit, offset int) ([]domain.ReconciliationDiscrepancy, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT uuid, randid, account_uuid, settlement_batch_uuid, discrepancy_type,
		       expected_pending, actual_pending, expected_available, actual_available,
		       pending_diff, available_diff,
		       item_discrepancy_count, total_item_discrepancy,
		       status, detected_at, resolved_at, resolution_notes, created_at, updated_at
		FROM reconciliation_discrepancies
		WHERE account_uuid = $1
		ORDER BY detected_at DESC
		LIMIT $2 OFFSET $3
	`, ledgerID, limit, offset)

	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanRows(rows)
}

func (r *PostgresReconciliationDiscrepancyRepository) GetPendingDiscrepancies(ctx context.Context, limit int) ([]domain.ReconciliationDiscrepancy, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT uuid, randid, account_uuid, settlement_batch_uuid, discrepancy_type,
		       expected_pending, actual_pending, expected_available, actual_available,
		       pending_diff, available_diff,
		       item_discrepancy_count, total_item_discrepancy,
		       status, detected_at, resolved_at, resolution_notes, created_at, updated_at
		FROM reconciliation_discrepancies
		WHERE status = 'PENDING'
		ORDER BY detected_at DESC
		LIMIT $1
	`, limit)

	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanRows(rows)
}

func (r *PostgresReconciliationDiscrepancyRepository) MarkResolved(ctx context.Context, id string, notes string) error {
	now := time.Now()
	result, err := r.db.ExecContext(ctx, `
		UPDATE reconciliation_discrepancies
		SET status = 'RESOLVED', resolved_at = $1, resolution_notes = $2
		WHERE uuid = $3 AND status = 'PENDING'
	`, now, notes, id)

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

func (r *PostgresReconciliationDiscrepancyRepository) scanRow(row *sql.Row) (*domain.ReconciliationDiscrepancy, error) {
	var d domain.ReconciliationDiscrepancy
	redifu.InitRecord(&d)
	var discrepancyType string
	var status string
	var resolvedAt sql.NullTime
	var resolutionNotes sql.NullString

	err := row.Scan(
		&d.UUID,
		&d.RandId,
		&d.LedgerUUID,
		&d.SettlementBatchUUID,
		&discrepancyType,
		&d.ExpectedPending,
		&d.ActualPending,
		&d.ExpectedAvailable,
		&d.ActualAvailable,
		&d.PendingDiff,
		&d.AvailableDiff,
		&d.ItemDiscrepancyCount,
		&d.TotalItemDiscrepancy,
		&status,
		&d.DetectedAt,
		&resolvedAt,
		&resolutionNotes,
		&d.CreatedAt,
		&d.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	d.DiscrepancyType = domain.DiscrepancyType(discrepancyType)
	d.Status = domain.DiscrepancyStatus(status)

	if resolvedAt.Valid {
		d.ResolvedAt = &resolvedAt.Time
	}
	if resolutionNotes.Valid {
		d.ResolutionNotes = resolutionNotes.String
	}

	return &d, nil
}

func (r *PostgresReconciliationDiscrepancyRepository) scanRows(rows *sql.Rows) ([]domain.ReconciliationDiscrepancy, error) {
	discrepancies := []domain.ReconciliationDiscrepancy{}

	for rows.Next() {
		var d domain.ReconciliationDiscrepancy
		redifu.InitRecord(&d)
		var discrepancyType string
		var status string
		var resolvedAt sql.NullTime
		var resolutionNotes sql.NullString

		err := rows.Scan(
			&d.UUID,
			&d.RandId,
			&d.LedgerUUID,
			&d.SettlementBatchUUID,
			&discrepancyType,
			&d.ExpectedPending,
			&d.ActualPending,
			&d.ExpectedAvailable,
			&d.ActualAvailable,
			&d.PendingDiff,
			&d.AvailableDiff,
			&d.ItemDiscrepancyCount,
			&d.TotalItemDiscrepancy,
			&status,
			&d.DetectedAt,
			&resolvedAt,
			&resolutionNotes,
			&d.CreatedAt,
			&d.UpdatedAt,
		)
		if err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}

		d.DiscrepancyType = domain.DiscrepancyType(discrepancyType)
		d.Status = domain.DiscrepancyStatus(status)

		if resolvedAt.Valid {
			d.ResolvedAt = &resolvedAt.Time
		}
		if resolutionNotes.Valid {
			d.ResolutionNotes = resolutionNotes.String
		}

		discrepancies = append(discrepancies, d)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return discrepancies, nil
}
