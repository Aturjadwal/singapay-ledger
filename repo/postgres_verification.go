package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresVerificationRepository struct {
	db *sql.DB
}

func NewPostgresVerificationRepository(db *sql.DB) *PostgresVerificationRepository {
	return &PostgresVerificationRepository{db: db}
}

func (r *PostgresVerificationRepository) Save(ctx context.Context, verification *domain.Verification) error {
	metadataJSON, err := json.Marshal(verification.Metadata)
	if err != nil {
		metadataJSON = []byte("{}")
	}

	query := `
		INSERT INTO ledger_verifications (
			uuid, randid, account_uuid, identity_id, fullname, birth_date,
			province, city, district, postal_code,
			ktp_photo_url, selfie_photo_url,
			status, approved_by, approved_at, rejection_reason,
			metadata, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (uuid) DO UPDATE SET
			identity_id = EXCLUDED.identity_id,
			fullname = EXCLUDED.fullname,
			birth_date = EXCLUDED.birth_date,
			province = EXCLUDED.province,
			city = EXCLUDED.city,
			district = EXCLUDED.district,
			postal_code = EXCLUDED.postal_code,
			ktp_photo_url = EXCLUDED.ktp_photo_url,
			selfie_photo_url = EXCLUDED.selfie_photo_url,
			status = EXCLUDED.status,
			approved_by = EXCLUDED.approved_by,
			approved_at = EXCLUDED.approved_at,
			rejection_reason = EXCLUDED.rejection_reason,
			metadata = EXCLUDED.metadata,
			updated_at = EXCLUDED.updated_at
	`

	_, err = r.db.ExecContext(
		ctx,
		query,
		verification.UUID,
		verification.RandId,
		verification.AccountUUID,
		verification.IdentityID,
		verification.Fullname,
		verification.BirthDate,
		verification.Province,
		verification.City,
		verification.District,
		verification.PostalCode,
		verification.KTPPhotoURL,
		verification.SelfiePhotoURL,
		verification.Status,
		toNullString(verification.ApprovedBy),
		toNullTime(verification.ApprovedAt),
		toNullString(verification.RejectionReason),
		metadataJSON,
		verification.CreatedAt,
		verification.UpdatedAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresVerificationRepository) GetByID(ctx context.Context, id string) (*domain.Verification, error) {
	query := `
		SELECT uuid, randid, account_uuid, identity_id, fullname, birth_date,
		       province, city, district, postal_code,
		       ktp_photo_url, selfie_photo_url,
		       status, approved_by, approved_at, rejection_reason,
		       metadata, created_at, updated_at
		FROM ledger_verifications
		WHERE uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, id)
	return r.scanRow(row)
}

func (r *PostgresVerificationRepository) GetByAccountID(ctx context.Context, accountID string) (*domain.Verification, error) {
	query := `
		SELECT uuid, randid, account_uuid, identity_id, fullname, birth_date,
		       province, city, district, postal_code,
		       ktp_photo_url, selfie_photo_url,
		       status, approved_by, approved_at, rejection_reason,
		       metadata, created_at, updated_at
		FROM ledger_verifications
		WHERE account_uuid = $1
		ORDER BY created_at DESC
		LIMIT 1
	`

	row := r.db.QueryRowContext(ctx, query, accountID)
	return r.scanRow(row)
}

func (r *PostgresVerificationRepository) GetByIdentityID(ctx context.Context, identityID string) (*domain.Verification, error) {
	query := `
		SELECT uuid, randid, account_uuid, identity_id, fullname, birth_date,
		       province, city, district, postal_code,
		       ktp_photo_url, selfie_photo_url,
		       status, approved_by, approved_at, rejection_reason,
		       metadata, created_at, updated_at
		FROM ledger_verifications
		WHERE identity_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`

	row := r.db.QueryRowContext(ctx, query, identityID)
	return r.scanRow(row)
}

func (r *PostgresVerificationRepository) GetPendingVerifications(ctx context.Context, limit, offset int) ([]*domain.Verification, error) {
	if limit < 1 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT uuid, randid, account_uuid, identity_id, fullname, birth_date,
		       province, city, district, postal_code,
		       ktp_photo_url, selfie_photo_url,
		       status, approved_by, approved_at, rejection_reason,
		       metadata, created_at, updated_at
		FROM ledger_verifications
		WHERE status = 'PENDING'
		ORDER BY created_at ASC
		LIMIT $1 OFFSET $2
	`

	rows, err := r.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanRows(rows)
}

func (r *PostgresVerificationRepository) UpdateStatus(ctx context.Context, id string, status domain.VerificationStatus, approvedBy string, rejectionReason string) error {
	now := time.Now()

	query := `
		UPDATE ledger_verifications
		SET status = $1,
		    approved_by = $2,
		    approved_at = $3,
		    rejection_reason = $4,
		    updated_at = $5
		WHERE uuid = $6
	`

	_, err := r.db.ExecContext(
		ctx,
		query,
		status,
		toNullString(approvedBy),
		toNullTime(&now),
		toNullString(rejectionReason),
		now,
		id,
	)
	if err != nil {
		return ErrFailedUpdateSQL.WithError(err)
	}

	return nil
}

func (r *PostgresVerificationRepository) scanRow(row *sql.Row) (*domain.Verification, error) {
	var v domain.Verification
	redifu.InitRecord(&v)

	var approvedBy sql.NullString
	var approvedAt sql.NullTime
	var rejectionReason sql.NullString
	var metadataJSON []byte

	err := row.Scan(
		&v.UUID,
		&v.RandId,
		&v.AccountUUID,
		&v.IdentityID,
		&v.Fullname,
		&v.BirthDate,
		&v.Province,
		&v.City,
		&v.District,
		&v.PostalCode,
		&v.KTPPhotoURL,
		&v.SelfiePhotoURL,
		&v.Status,
		&approvedBy,
		&approvedAt,
		&rejectionReason,
		&metadataJSON,
		&v.CreatedAt,
		&v.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	if approvedBy.Valid {
		v.ApprovedBy = approvedBy.String
	}
	if approvedAt.Valid {
		v.ApprovedAt = &approvedAt.Time
	}
	if rejectionReason.Valid {
		v.RejectionReason = rejectionReason.String
	}

	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &v.Metadata); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
	}

	return &v, nil
}

func (r *PostgresVerificationRepository) scanRows(rows *sql.Rows) ([]*domain.Verification, error) {
	verifications := []*domain.Verification{}

	for rows.Next() {
		var v domain.Verification
		redifu.InitRecord(&v)

		var approvedBy sql.NullString
		var approvedAt sql.NullTime
		var rejectionReason sql.NullString
		var metadataJSON []byte

		err := rows.Scan(
			&v.UUID,
			&v.RandId,
			&v.AccountUUID,
			&v.IdentityID,
			&v.Fullname,
			&v.BirthDate,
			&v.Province,
			&v.City,
			&v.District,
			&v.PostalCode,
			&v.KTPPhotoURL,
			&v.SelfiePhotoURL,
			&v.Status,
			&approvedBy,
			&approvedAt,
			&rejectionReason,
			&metadataJSON,
			&v.CreatedAt,
			&v.UpdatedAt,
		)
		if err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}

		if approvedBy.Valid {
			v.ApprovedBy = approvedBy.String
		}
		if approvedAt.Valid {
			v.ApprovedAt = &approvedAt.Time
		}
		if rejectionReason.Valid {
			v.RejectionReason = rejectionReason.String
		}

		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &v.Metadata); err != nil {
				return nil, ErrFailedScanSQL.WithError(err)
			}
		}

		verifications = append(verifications, &v)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return verifications, nil
}
