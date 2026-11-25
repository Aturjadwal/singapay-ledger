package repo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/21strive/redifu"
	"github.com/lib/pq"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

// PostgresPlatformReconciliationRepository implements domain.PlatformReconciliationRepository:
// the platform balance reconciliation's reads, and its two tables (migration 034).
type PostgresPlatformReconciliationRepository struct {
	db DBTX
}

func NewPostgresPlatformReconciliationRepository(db DBTX) *PostgresPlatformReconciliationRepository {
	return &PostgresPlatformReconciliationRepository{db: db}
}

// The unique indexes of platform_adjustments.
const (
	// ConstraintUniqueReconciliationBooking refuses a second booking of one check.
	ConstraintUniqueReconciliationBooking = "uq_platform_adjustments_reconciliation"
	// ConstraintUniqueReserveArchive refuses a second DOKU-era archive.
	ConstraintUniqueReserveArchive = "uq_platform_adjustments_reserve_archive"
)

// ReadBalanceSnapshot reads the ledger's side of the equation in a single statement, so every
// figure is from the same snapshot. $1 is the platform account.
//
//   - ledger_available: L, the platform's AVAILABLE entries summed, whole rupiah — what a
//     payout derives (GetAllBalances), not the cached column.
//   - swept_residual: Σ platform_residual over swept rows of sellers' sales. The platform's
//     own sales are never swept (paid straight into its own sub-account), and a row settled
//     before the residual was recorded has NULL, read as 0.
//   - adjustment_residual and rounded: the pool's other two terms, from platform_adjustments.
//   - unswept and unswept_count: B. Over sellers' SETTLED rows whose fee is not transferred,
//     the platform's AVAILABLE entries sourced on them (SETTLEMENT_NET and FEE_ADJUSTMENT),
//     whole rupiah.
//   - in_flight: the platform's payouts in PENDING or PROCESSING.
func (r *PostgresPlatformReconciliationRepository) ReadBalanceSnapshot(ctx context.Context, platformAccountUUID string) (*domain.PlatformBalanceSnapshot, error) {
	query := `
		SELECT
			(SELECT COALESCE(SUM(le.amount), 0)::bigint
			   FROM ledger_entries le
			  WHERE le.account_uuid = $1
			    AND le.balance_bucket = 'AVAILABLE') AS ledger_available,
			(SELECT COALESCE(SUM(COALESCE(pt.platform_residual, 0)), 0)::bigint
			   FROM product_transactions pt
			  WHERE pt.platform_fee_transferred = true
			    AND pt.seller_account_id <> $1) AS swept_residual,
			(SELECT COALESCE(SUM(pa.residual_minor), 0)::bigint
			   FROM platform_adjustments pa
			  WHERE pa.platform_account_uuid = $1) AS adjustment_residual,
			(SELECT COALESCE(SUM(pa.amount * 100), 0)::bigint
			   FROM platform_adjustments pa
			  WHERE pa.platform_account_uuid = $1
			    AND pa.kind = 'ROUNDING') AS rounded,
			(SELECT COALESCE(SUM(u.platform_amount), 0)::bigint
			   FROM (SELECT (SELECT COALESCE(SUM(le.amount), 0)
			                   FROM ledger_entries le
			                  WHERE le.account_uuid = $1
			                    AND le.balance_bucket = 'AVAILABLE'
			                    AND le.source_type = 'PRODUCT_TRANSACTION'
			                    AND le.source_id = pt.uuid) AS platform_amount
			           FROM product_transactions pt
			          WHERE pt.status = 'SETTLED'
			            AND pt.platform_fee_transferred = false
			            AND pt.seller_account_id <> $1) u) AS unswept,
			(SELECT COUNT(*)
			   FROM product_transactions pt
			  WHERE pt.status = 'SETTLED'
			    AND pt.platform_fee_transferred = false
			    AND pt.seller_account_id <> $1
			    AND EXISTS (SELECT 1 FROM ledger_entries le
			                 WHERE le.account_uuid = $1
			                   AND le.balance_bucket = 'AVAILABLE'
			                   AND le.source_type = 'PRODUCT_TRANSACTION'
			                   AND le.source_id = pt.uuid
			                   AND le.amount <> 0)) AS unswept_count,
			(SELECT COUNT(*)
			   FROM disbursements d
			  WHERE d.account_uuid = $1
			    AND d.status IN ('PENDING', 'PROCESSING')) AS in_flight
	`

	snapshot := &domain.PlatformBalanceSnapshot{PlatformAccountUUID: platformAccountUUID}
	var unsweptCount, inFlight int64
	err := r.db.QueryRowContext(ctx, query, platformAccountUUID).Scan(
		&snapshot.LedgerAvailable,
		&snapshot.SweptResidualMinor,
		&snapshot.AdjustmentResidualMinor,
		&snapshot.RoundedMinor,
		&snapshot.UnsweptPlatformFee,
		&unsweptCount,
		&inFlight,
	)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	snapshot.UnsweptCount = int(unsweptCount)
	snapshot.PayoutsInFlight = int(inFlight)
	return snapshot, nil
}

// ListUnroundedResidualSources returns the swept rows of sellers' sales whose residual is
// above zero and that no ROUNDING names yet, oldest sweep first.
func (r *PostgresPlatformReconciliationRepository) ListUnroundedResidualSources(ctx context.Context, platformAccountUUID string) ([]string, error) {
	query := `
		SELECT pt.uuid
		FROM product_transactions pt
		WHERE pt.platform_fee_transferred = true
		  AND pt.seller_account_id <> $1
		  AND COALESCE(pt.platform_residual, 0) > 0
		  AND NOT EXISTS (SELECT 1 FROM platform_adjustments pa
		                   WHERE pa.platform_account_uuid = $1
		                     AND pa.kind = 'ROUNDING'
		                     AND pt.uuid = ANY (pa.source_transaction_uuids))
		ORDER BY pt.platform_fee_transferred_at ASC NULLS FIRST, pt.uuid COLLATE "C" ASC
	`

	rows, err := r.db.QueryContext(ctx, query, platformAccountUUID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	sources := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
		sources = append(sources, id)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	return sources, nil
}

// ListReserveArchiveSources returns the DOKU-era transactions with what the platform's
// AVAILABLE entries hold for each: the transactions settled before the ledger recorded settled
// fees (settled_platform_fee NULL — every settlement booked since migration 024 records one),
// whose platform AVAILABLE entries do not net to zero.
func (r *PostgresPlatformReconciliationRepository) ListReserveArchiveSources(ctx context.Context, platformAccountUUID string) ([]domain.ReserveArchiveSource, error) {
	query := `
		SELECT pt.uuid, pt.invoice_number, pt.product_type, pt.seller_account_id = $1 AS platform_sale,
		       pt.settled_at, SUM(le.amount)::bigint AS amount
		FROM ledger_entries le
		JOIN product_transactions pt ON pt.uuid = le.source_id
		WHERE le.account_uuid = $1
		  AND le.balance_bucket = 'AVAILABLE'
		  AND le.source_type = 'PRODUCT_TRANSACTION'
		  AND pt.settled_platform_fee IS NULL
		GROUP BY pt.uuid, pt.invoice_number, pt.product_type, pt.seller_account_id, pt.settled_at
		HAVING SUM(le.amount) <> 0
		ORDER BY pt.settled_at ASC NULLS FIRST, pt.uuid COLLATE "C" ASC
	`

	rows, err := r.db.QueryContext(ctx, query, platformAccountUUID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	sources := []domain.ReserveArchiveSource{}
	for rows.Next() {
		var (
			source    domain.ReserveArchiveSource
			settledAt sql.NullTime
		)
		if err := rows.Scan(&source.ProductTransactionUUID, &source.InvoiceNumber, &source.ProductType,
			&source.PlatformSale, &settledAt, &source.Amount); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
		if settledAt.Valid {
			t := settledAt.Time
			source.SettledAt = &t
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	return sources, nil
}

const platformAdjustmentColumns = `uuid, randid, platform_account_uuid, kind, amount, residual_minor, pool_before_minor,
	reason, note, admin_id, admin_name, journal_uuid, reconciliation_uuid, reserve_account_uuid,
	source_transaction_uuids, created_at`

// SaveAdjustment inserts one adjustment. A second booking of one check, or a second archive,
// is refused by the unique indexes (IsUniqueViolation with the constraint names above).
func (r *PostgresPlatformReconciliationRepository) SaveAdjustment(ctx context.Context, a *domain.PlatformAdjustment) error {
	query := `
		INSERT INTO platform_adjustments (
			uuid, randid, platform_account_uuid, kind, amount, residual_minor, pool_before_minor,
			reason, note, admin_id, admin_name, journal_uuid, reconciliation_uuid, reserve_account_uuid,
			source_transaction_uuids, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`

	sources := a.SourceTransactionUUIDs
	if sources == nil {
		sources = []string{}
	}

	_, err := r.db.ExecContext(ctx, query,
		a.UUID,
		a.RandId,
		a.PlatformAccountUUID,
		string(a.Kind),
		a.Amount,
		a.ResidualMinor,
		toNullInt64(a.PoolBeforeMinor),
		toNullString(string(a.Reason)),
		toNullString(a.Note),
		toNullString(a.AdminID),
		toNullString(a.AdminName),
		toNullString(a.JournalUUID),
		toNullString(a.ReconciliationUUID),
		toNullString(a.ReserveAccountUUID),
		pq.Array(sources),
		createdAtOrNow(a.CreatedAt).UTC(),
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}
	return nil
}

func scanPlatformAdjustment(row interface{ Scan(dest ...any) error }) (*domain.PlatformAdjustment, error) {
	a := &domain.PlatformAdjustment{}
	redifu.InitRecord(a)
	var (
		kind, reason, note, adminID, adminName sql.NullString
		journalUUID, reconciliationUUID        sql.NullString
		reserveAccountUUID                     sql.NullString
		poolBefore                             sql.NullInt64
		sources                                pq.StringArray
	)
	if err := row.Scan(&a.UUID, &a.RandId, &a.PlatformAccountUUID, &kind, &a.Amount, &a.ResidualMinor, &poolBefore,
		&reason, &note, &adminID, &adminName, &journalUUID, &reconciliationUUID, &reserveAccountUUID,
		&sources, &a.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}
	a.Kind = domain.PlatformAdjustmentKind(kind.String)
	a.PoolBeforeMinor = nullableInt64(poolBefore)
	a.Reason = domain.PlatformAdjustmentReason(reason.String)
	a.Note = note.String
	a.AdminID = adminID.String
	a.AdminName = adminName.String
	a.JournalUUID = journalUUID.String
	a.ReconciliationUUID = reconciliationUUID.String
	a.ReserveAccountUUID = reserveAccountUUID.String
	a.SourceTransactionUUIDs = []string(sources)
	if a.SourceTransactionUUIDs == nil {
		a.SourceTransactionUUIDs = []string{}
	}
	a.UpdatedAt = a.CreatedAt
	return a, nil
}

// GetAdjustmentByReconciliation returns the DIFFERENCE booked against a check.
func (r *PostgresPlatformReconciliationRepository) GetAdjustmentByReconciliation(ctx context.Context, reconciliationUUID string) (*domain.PlatformAdjustment, error) {
	query := `SELECT ` + platformAdjustmentColumns + ` FROM platform_adjustments WHERE reconciliation_uuid = $1`
	return scanPlatformAdjustment(r.db.QueryRowContext(ctx, query, reconciliationUUID))
}

// GetReserveArchive returns the RESERVE_ARCHIVE adjustment.
func (r *PostgresPlatformReconciliationRepository) GetReserveArchive(ctx context.Context) (*domain.PlatformAdjustment, error) {
	query := `SELECT ` + platformAdjustmentColumns + ` FROM platform_adjustments WHERE kind = 'RESERVE_ARCHIVE' ORDER BY created_at LIMIT 1`
	return scanPlatformAdjustment(r.db.QueryRowContext(ctx, query))
}

// ListAdjustments pages the platform's adjustments in (created_at, uuid) order, one past
// after, for the statement's merge. The tiebreak compares uuid under COLLATE "C", as the
// statement's other two sources do, because the merge compares ids byte-wise in Go.
func (r *PostgresPlatformReconciliationRepository) ListAdjustments(ctx context.Context, platformAccountUUID string, after *domain.KeysetCursor, limit int, ascending bool) ([]*domain.PlatformAdjustment, error) {
	comparison, direction := "<", "DESC"
	if ascending {
		comparison, direction = ">", "ASC"
	}

	args := []any{platformAccountUUID}
	cursorClause := ""
	if after != nil {
		args = append(args, after.At, after.ID)
		cursorClause = fmt.Sprintf(`AND (created_at, uuid COLLATE "C") %s ($2::timestamp, $3::varchar)`, comparison)
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT %s
		FROM platform_adjustments
		WHERE platform_account_uuid = $1
		  %s
		ORDER BY created_at %s, uuid COLLATE "C" %s
		LIMIT $%d
	`, platformAdjustmentColumns, cursorClause, direction, direction, len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	adjustments := []*domain.PlatformAdjustment{}
	for rows.Next() {
		a, err := scanPlatformAdjustment(rows)
		if err != nil {
			return nil, err
		}
		adjustments = append(adjustments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	return adjustments, nil
}

const platformReconciliationColumns = `uuid, randid, platform_account_uuid, singapay_account_id,
	singapay_available, singapay_pending, singapay_held,
	ledger_available, residual_pool, unswept_platform_fee, unswept_count, payouts_in_flight,
	unexplained, status, skip_reason, skip_detail, raw_balance, checked_at, emailed_at, created_at`

// SaveReconciliation inserts one check.
func (r *PostgresPlatformReconciliationRepository) SaveReconciliation(ctx context.Context, rec *domain.PlatformReconciliation) error {
	query := `
		INSERT INTO platform_reconciliations (
			uuid, randid, platform_account_uuid, singapay_account_id,
			singapay_available, singapay_pending, singapay_held,
			ledger_available, residual_pool, unswept_platform_fee, unswept_count, payouts_in_flight,
			unexplained, status, skip_reason, skip_detail, raw_balance, checked_at, emailed_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
	`

	var rawBalance sql.NullString
	if len(rec.RawBalance) > 0 {
		rawBalance = sql.NullString{String: string(rec.RawBalance), Valid: true}
	}
	var emailedAt sql.NullTime
	if rec.EmailedAt != nil {
		emailedAt = sql.NullTime{Time: rec.EmailedAt.UTC(), Valid: true}
	}

	_, err := r.db.ExecContext(ctx, query,
		rec.UUID,
		rec.RandId,
		rec.PlatformAccountUUID,
		rec.SingapayAccountID,
		toNullInt64(rec.SingapayAvailableMinor),
		toNullInt64(rec.SingapayPendingMinor),
		toNullInt64(rec.SingapayHeldMinor),
		rec.LedgerAvailableMinor,
		rec.ResidualPoolMinor,
		rec.UnsweptPlatformFeeMinor,
		rec.UnsweptCount,
		rec.PayoutsInFlight,
		toNullInt64(rec.UnexplainedMinor),
		string(rec.Status),
		toNullString(string(rec.SkipReason)),
		toNullString(rec.SkipDetail),
		rawBalance,
		rec.CheckedAt.UTC(),
		emailedAt,
		createdAtOrNow(rec.CreatedAt).UTC(),
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}
	return nil
}

func scanPlatformReconciliation(row interface{ Scan(dest ...any) error }) (*domain.PlatformReconciliation, error) {
	rec := &domain.PlatformReconciliation{}
	redifu.InitRecord(rec)
	var (
		available, pending, held, unexplained sql.NullInt64
		unsweptCount, payoutsInFlight         int64
		status, skipReason, skipDetail        sql.NullString
		rawBalance                            sql.NullString
		emailedAt                             sql.NullTime
	)
	if err := row.Scan(&rec.UUID, &rec.RandId, &rec.PlatformAccountUUID, &rec.SingapayAccountID,
		&available, &pending, &held,
		&rec.LedgerAvailableMinor, &rec.ResidualPoolMinor, &rec.UnsweptPlatformFeeMinor, &unsweptCount, &payoutsInFlight,
		&unexplained, &status, &skipReason, &skipDetail, &rawBalance, &rec.CheckedAt, &emailedAt, &rec.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}
	rec.SingapayAvailableMinor = nullableInt64(available)
	rec.SingapayPendingMinor = nullableInt64(pending)
	rec.SingapayHeldMinor = nullableInt64(held)
	rec.UnexplainedMinor = nullableInt64(unexplained)
	rec.UnsweptCount = int(unsweptCount)
	rec.PayoutsInFlight = int(payoutsInFlight)
	rec.Status = domain.PlatformReconciliationStatus(status.String)
	rec.SkipReason = domain.PlatformReconciliationSkipReason(skipReason.String)
	rec.SkipDetail = skipDetail.String
	if rawBalance.Valid {
		rec.RawBalance = []byte(rawBalance.String)
	} else {
		rec.RawBalance = nil
	}
	rec.EmailedAt = nil
	if emailedAt.Valid {
		t := emailedAt.Time
		rec.EmailedAt = &t
	}
	rec.UpdatedAt = rec.CreatedAt
	return rec, nil
}

// LatestReconciliation returns the platform's most recent check.
func (r *PostgresPlatformReconciliationRepository) LatestReconciliation(ctx context.Context, platformAccountUUID string) (*domain.PlatformReconciliation, error) {
	query := `SELECT ` + platformReconciliationColumns + `
		FROM platform_reconciliations
		WHERE platform_account_uuid = $1
		ORDER BY checked_at DESC, created_at DESC, uuid COLLATE "C" DESC
		LIMIT 1`
	return scanPlatformReconciliation(r.db.QueryRowContext(ctx, query, platformAccountUUID))
}

// LastEmailedAt returns when the admins were last emailed about one of the platform's checks.
func (r *PostgresPlatformReconciliationRepository) LastEmailedAt(ctx context.Context, platformAccountUUID string) (*time.Time, error) {
	query := `SELECT MAX(emailed_at) FROM platform_reconciliations WHERE platform_account_uuid = $1 AND emailed_at IS NOT NULL`
	var at sql.NullTime
	if err := r.db.QueryRowContext(ctx, query, platformAccountUUID).Scan(&at); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time
	return &t, nil
}

// MarkEmailed records when the admins were emailed about a check. ErrNotFound when no check
// has that uuid.
func (r *PostgresPlatformReconciliationRepository) MarkEmailed(ctx context.Context, reconciliationUUID string, at time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE platform_reconciliations SET emailed_at = $2 WHERE uuid = $1`, reconciliationUUID, at.UTC())
	if err != nil {
		return ErrFailedUpdateSQL.WithError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ErrFailedUpdateSQL.WithError(err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
