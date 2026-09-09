package repo

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresJournalRepository struct {
	db DBTX
}

func NewPostgresJournalRepository(db DBTX) *PostgresJournalRepository {
	return &PostgresJournalRepository{db: db}
}

func (r *PostgresJournalRepository) Save(ctx context.Context, journal *domain.Journal) error {
	metadataJSON, err := json.Marshal(journal.Metadata)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	query := `
		INSERT INTO journals (
			uuid, randid, event_type, source_type, source_id, metadata, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`

	_, err = r.db.ExecContext(
		ctx,
		query,
		journal.UUID,
		journal.RandId,
		journal.EventType,
		journal.SourceType,
		journal.SourceID,
		metadataJSON,
		journal.CreatedAt,
		journal.UpdatedAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresJournalRepository) GetByID(ctx context.Context, id string) (*domain.Journal, error) {
	query := `
		SELECT uuid, randid, event_type, source_type, source_id, metadata, created_at, updated_at
		FROM journals
		WHERE uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, id)
	return r.scanJournal(row)
}

func (r *PostgresJournalRepository) GetBySourceID(ctx context.Context, sourceType domain.SourceType, sourceID string) ([]*domain.Journal, error) {
	query := `
		SELECT uuid, randid, event_type, source_type, source_id, metadata, created_at, updated_at
		FROM journals
		WHERE source_type = $1 AND source_id = $2
		ORDER BY created_at DESC
	`

	rows, err := r.db.QueryContext(ctx, query, sourceType, sourceID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanJournals(rows)
}

func (r *PostgresJournalRepository) GetByEventType(ctx context.Context, eventType domain.EventType, limit, offset int) ([]*domain.Journal, error) {
	if limit < 1 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT uuid, randid, event_type, source_type, source_id, metadata, created_at, updated_at
		FROM journals
		WHERE event_type = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, eventType, limit, offset)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanJournals(rows)
}

func (r *PostgresJournalRepository) scanJournal(row *sql.Row) (*domain.Journal, error) {
	var j domain.Journal
	redifu.InitRecord(&j)

	var metadataJSON []byte

	err := row.Scan(
		&j.UUID,
		&j.RandId,
		&j.EventType,
		&j.SourceType,
		&j.SourceID,
		&metadataJSON,
		&j.CreatedAt,
		&j.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &j.Metadata); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
	}

	return &j, nil
}

func (r *PostgresJournalRepository) scanJournals(rows *sql.Rows) ([]*domain.Journal, error) {
	var journals []*domain.Journal

	for rows.Next() {
		var j domain.Journal
		redifu.InitRecord(&j)

		var metadataJSON []byte

		err := rows.Scan(
			&j.UUID,
			&j.RandId,
			&j.EventType,
			&j.SourceType,
			&j.SourceID,
			&metadataJSON,
			&j.CreatedAt,
			&j.UpdatedAt,
		)
		if err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}

		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &j.Metadata); err != nil {
				return nil, ErrFailedScanSQL.WithError(err)
			}
		}

		journals = append(journals, &j)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return journals, nil
}
