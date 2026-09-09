package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

// PostgresLedgerEntryRepository implements domain.LedgerEntryRepository.
// Entries are insert-only — no Update or Delete methods exist by design.
type PostgresLedgerEntryRepository struct {
	db DBTX
}

func NewPostgresLedgerEntryRepository(db DBTX) *PostgresLedgerEntryRepository {
	return &PostgresLedgerEntryRepository{db: db}
}

// ─────────────────────────────────────────────────────────────────────────────
// Write operations
// ─────────────────────────────────────────────────────────────────────────────

// Save inserts a single immutable ledger entry.
func (r *PostgresLedgerEntryRepository) Save(ctx context.Context, entry *domain.LedgerEntry) error {
	// Calculate balance_after if not already set
	if entry.BalanceAfter == 0 {
		lastBalance, err := r.GetLastBalanceAfter(ctx, entry.AccountUUID, entry.BalanceBucket)
		if err != nil {
			return err
		}
		entry.BalanceAfter = lastBalance + entry.Amount
	}

	query := `
		INSERT INTO ledger_entries (
			uuid, randid, journal_uuid, account_uuid, amount, balance_bucket,
			entry_type, source_type, source_id, balance_after, metadata, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`

	metadataJSON, err := marshalMetadata(entry.Metadata)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	_, err = r.db.ExecContext(
		ctx,
		query,
		entry.UUID,
		entry.RandId,
		entry.JournalUUID,
		entry.AccountUUID,
		entry.Amount,
		entry.BalanceBucket,
		entry.EntryType,
		entry.SourceType,
		entry.SourceID,
		entry.BalanceAfter,
		metadataJSON,
		entry.CreatedAt,
		entry.UpdatedAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	// Update account balances after successfully inserting entry
	var pendingDelta, availableDelta int64
	switch entry.BalanceBucket {
	case domain.BalanceBucketPending:
		pendingDelta = entry.Amount
	case domain.BalanceBucketAvailable:
		availableDelta = entry.Amount
	}

	// Update balance buckets
	if pendingDelta != 0 || availableDelta != 0 {
		query := `
			UPDATE ledger_accounts
			SET pending_balance = pending_balance + $1,
				available_balance = available_balance + $2,
				updated_at = NOW()
			WHERE uuid = $3
		`
		_, err := r.db.ExecContext(ctx, query, pendingDelta, availableDelta, entry.AccountUUID)
		if err != nil {
			return ErrFailedUpdateSQL.WithError(err)
		}
	}

	// NOTE: total_deposit_amount and total_withdrawal_amount are NOT updated here
	// They should only be updated when:
	// - Product transaction is marked SETTLED (in ProcessReconciliation)
	// - Disbursement is marked COMPLETED (in ProcessDisbursement)
	// This ensures these totals reflect actual settled money, not intermediate ledger movements

	return nil
}

// SaveBatch inserts multiple entries in a single round-trip using a loop.
// Should be called inside a transaction (via repo.Tx) to ensure atomicity.
// Calculates balance_after for each entry based on account+bucket.
func (r *PostgresLedgerEntryRepository) SaveBatch(ctx context.Context, entries []*domain.LedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}

	// Build a map to track running balances per account+bucket during insertion
	balanceTracker := make(map[string]int64)

	query := `
		INSERT INTO ledger_entries (
			uuid, randid, journal_uuid, account_uuid, amount, balance_bucket,
			entry_type, source_type, source_id, balance_after, metadata, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`

	for _, entry := range entries {
		// Calculate balance_after if not already set
		if entry.BalanceAfter == 0 {
			key := entry.AccountUUID + ":" + string(entry.BalanceBucket)

			// Get last balance for this account+bucket if not cached
			if _, exists := balanceTracker[key]; !exists {
				lastBalance, err := r.GetLastBalanceAfter(ctx, entry.AccountUUID, entry.BalanceBucket)
				if err != nil {
					return err
				}
				balanceTracker[key] = lastBalance
			}

			// Calculate new balance
			balanceTracker[key] += entry.Amount
			entry.BalanceAfter = balanceTracker[key]
		}

		var metadataArg any
		if len(entry.Metadata) > 0 {
			metadataJSON, err := marshalMetadata(entry.Metadata)
			if err != nil {
				return ErrFailedInsertSQL.WithError(err)
			}
			metadataArg = metadataJSON
		}

		_, err := r.db.ExecContext(
			ctx,
			query,
			entry.UUID,
			entry.RandId,
			entry.JournalUUID,
			entry.AccountUUID,
			entry.Amount,
			entry.BalanceBucket,
			entry.EntryType,
			entry.SourceType,
			entry.SourceID,
			entry.BalanceAfter,
			metadataArg,
			entry.CreatedAt,
			entry.UpdatedAt,
		)
		if err != nil {
			return ErrFailedInsertSQL.WithError(err)
		}
	}

	// After successfully inserting all entries, update account balances
	// Aggregate deltas per account+bucket
	accountBalanceUpdates := make(map[string]struct {
		accountID      string
		pendingDelta   int64
		availableDelta int64
	})

	for _, entry := range entries {
		key := entry.AccountUUID
		update := accountBalanceUpdates[key]
		update.accountID = entry.AccountUUID

		// Update the appropriate balance bucket
		if entry.BalanceBucket == domain.BalanceBucketPending {
			update.pendingDelta += entry.Amount
		} else if entry.BalanceBucket == domain.BalanceBucketAvailable {
			update.availableDelta += entry.Amount
		}

		accountBalanceUpdates[key] = update
	}

	// Apply balance updates to each affected account
	for _, update := range accountBalanceUpdates {
		// Update balances
		if update.pendingDelta != 0 || update.availableDelta != 0 {
			query := `
				UPDATE ledger_accounts
				SET pending_balance = pending_balance + $1,
					available_balance = available_balance + $2,
					updated_at = NOW()
				WHERE uuid = $3
			`
			_, err := r.db.ExecContext(ctx, query, update.pendingDelta, update.availableDelta, update.accountID)
			if err != nil {
				return ErrFailedUpdateSQL.WithError(err)
			}
		}
	}

	// NOTE: total_deposit_amount and total_withdrawal_amount are NOT updated here
	// They should only be updated when:
	// - Product transaction is marked SETTLED (in ProcessReconciliation)
	// - Disbursement is marked COMPLETED (in ProcessDisbursement)
	// This ensures these totals reflect actual settled money, not intermediate ledger movements

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Balance queries (derived — never stored)
// ─────────────────────────────────────────────────────────────────────────────

// GetBalance returns the derived balance for a specific account + bucket by
// summing all entries. A zero balance is returned when no entries exist.
func (r *PostgresLedgerEntryRepository) GetBalance(ctx context.Context, accountID string, bucket domain.BalanceBucket) (int64, error) {
	query := `
		SELECT COALESCE(SUM(amount), 0)
		FROM ledger_entries
		WHERE account_uuid = $1 AND balance_bucket = $2
	`

	var balance int64
	row := r.db.QueryRowContext(ctx, query, accountID, bucket)
	if err := row.Scan(&balance); err != nil {
		return 0, ErrFailedQuerySQL.WithError(err)
	}

	return balance, nil
}

// GetAllBalances returns both PENDING and AVAILABLE derived balances for an
// account in a single query, avoiding two round-trips.
func (r *PostgresLedgerEntryRepository) GetAllBalances(ctx context.Context, accountID string) (pending, available int64, err error) {
	query := `
		SELECT
			COALESCE(SUM(amount) FILTER (WHERE balance_bucket = 'PENDING'),   0) AS pending,
			COALESCE(SUM(amount) FILTER (WHERE balance_bucket = 'AVAILABLE'), 0) AS available
		FROM ledger_entries
		WHERE account_uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, accountID)
	if err = row.Scan(&pending, &available); err != nil {
		return 0, 0, ErrFailedQuerySQL.WithError(err)
	}

	return pending, available, nil
}

// SumPendingBalanceBySellerID returns the total PENDING balance for a seller
// by looking up their account via owner_type='SELLER' AND owner_id=sellerID.
// Returns 0 when there are no entries (seller exists but has never been paid).
func (r *PostgresLedgerEntryRepository) SumPendingBalanceBySellerID(ctx context.Context, sellerID string) (int64, error) {
	return r.sumSellerBalance(ctx, sellerID, domain.BalanceBucketPending)
}

// SumAvailableBalanceBySellerID returns the total AVAILABLE balance for a seller
// by looking up their account via owner_type='SELLER' AND owner_id=sellerID.
// Returns 0 when there are no entries (seller has not yet had any settlement).
func (r *PostgresLedgerEntryRepository) SumAvailableBalanceBySellerID(ctx context.Context, sellerID string) (int64, error) {
	return r.sumSellerBalance(ctx, sellerID, domain.BalanceBucketAvailable)
}

// sumSellerBalance is the shared implementation for both seller balance queries.
// It joins ledger_entries → ledger_accounts to filter by the seller's business-level owner_id.
func (r *PostgresLedgerEntryRepository) sumSellerBalance(ctx context.Context, sellerID string, bucket domain.BalanceBucket) (int64, error) {
	query := `
		SELECT COALESCE(SUM(le.amount), 0)
		FROM ledger_entries le
		JOIN ledger_accounts a ON a.uuid = le.account_uuid
		WHERE a.owner_type = 'SELLER'
		  AND a.owner_id  = $1
		  AND le.balance_bucket = $2
	`

	var balance int64
	row := r.db.QueryRowContext(ctx, query, sellerID, bucket)
	if err := row.Scan(&balance); err != nil {
		return 0, ErrFailedQuerySQL.WithError(err)
	}

	return balance, nil
}

// GetAllBalancesBySellerID returns both PENDING and AVAILABLE derived balances
// for a seller in a single query — avoids two separate round-trips.
func (r *PostgresLedgerEntryRepository) GetAllBalancesBySellerID(ctx context.Context, sellerID string) (pending, available int64, err error) {
	query := `
		SELECT
			COALESCE(SUM(le.amount) FILTER (WHERE le.balance_bucket = 'PENDING'),   0) AS pending,
			COALESCE(SUM(le.amount) FILTER (WHERE le.balance_bucket = 'AVAILABLE'), 0) AS available
		FROM ledger_entries le
		JOIN ledger_accounts a ON a.uuid = le.account_uuid
		WHERE a.owner_type = 'SELLER'
		  AND a.owner_id  = $1
	`

	row := r.db.QueryRowContext(ctx, query, sellerID)
	if err = row.Scan(&pending, &available); err != nil {
		return 0, 0, ErrFailedQuerySQL.WithError(err)
	}

	return pending, available, nil
}

// GetLastBalanceAfter returns the most recent balance_after for an account+bucket.
// Returns 0 if no entries exist yet (starting balance).
func (r *PostgresLedgerEntryRepository) GetLastBalanceAfter(ctx context.Context, accountID string, bucket domain.BalanceBucket) (int64, error) {
	query := `
		SELECT balance_after
		FROM ledger_entries
		WHERE account_uuid = $1 AND balance_bucket = $2
		ORDER BY created_at DESC
		LIMIT 1
	`

	var balance int64
	row := r.db.QueryRowContext(ctx, query, accountID, bucket)
	err := row.Scan(&balance)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil // No entries yet, starting balance is 0
		}
		return 0, ErrFailedQuerySQL.WithError(err)
	}

	return balance, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Read queries
// ─────────────────────────────────────────────────────────────────────────────

// GetByJournalID returns all entries grouped in a specific journal.
func (r *PostgresLedgerEntryRepository) GetByJournalID(ctx context.Context, journalID string) ([]*domain.LedgerEntry, error) {
	query := `
		SELECT uuid, randid, journal_uuid, account_uuid, amount, balance_bucket,
		       entry_type, source_type, source_id, balance_after, metadata, created_at, updated_at
		FROM ledger_entries
		WHERE journal_uuid = $1
		ORDER BY created_at ASC
	`

	rows, err := r.db.QueryContext(ctx, query, journalID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return scanLedgerEntries(rows)
}

// GetBySourceID returns all entries originating from a given source.
func (r *PostgresLedgerEntryRepository) GetBySourceID(ctx context.Context, sourceID string) ([]*domain.LedgerEntry, error) {
	query := `
		SELECT uuid, randid, journal_uuid, account_uuid, amount, balance_bucket,
		       entry_type, source_type, source_id, balance_after, metadata, created_at, updated_at
		FROM ledger_entries
		WHERE source_id = $1
		ORDER BY created_at ASC
	`

	rows, err := r.db.QueryContext(ctx, query, sourceID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return scanLedgerEntries(rows)
}

// GetByAccountID returns paginated ledger entries for an account, newest first.
func (r *PostgresLedgerEntryRepository) GetByAccountID(ctx context.Context, accountID string, limit, offset int) ([]*domain.LedgerEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT uuid, randid, journal_uuid, account_uuid, amount, balance_bucket,
		       entry_type, source_type, source_id, balance_after, metadata, created_at, updated_at
		FROM ledger_entries
		WHERE account_uuid = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.QueryContext(ctx, query, accountID, limit, offset)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return scanLedgerEntries(rows)
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal scan helpers
// ─────────────────────────────────────────────────────────────────────────────

func scanLedgerEntries(rows *sql.Rows) ([]*domain.LedgerEntry, error) {
	var entries []*domain.LedgerEntry

	for rows.Next() {
		entry, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return entries, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanLedgerEntry(s scannable) (*domain.LedgerEntry, error) {
	var entry domain.LedgerEntry
	redifu.InitRecord(&entry)
	var metadataRaw []byte
	var createdAt, updatedAt time.Time

	err := s.Scan(
		&entry.UUID,
		&entry.RandId,
		&entry.JournalUUID,
		&entry.AccountUUID,
		&entry.Amount,
		&entry.BalanceBucket,
		&entry.EntryType,
		&entry.SourceType,
		&entry.SourceID,
		&entry.BalanceAfter,
		&metadataRaw,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		return nil, ErrFailedScanSQL.WithError(err)
	}

	if len(metadataRaw) > 0 {
		if err := json.Unmarshal(metadataRaw, &entry.Metadata); err != nil {
			// Non-fatal: metadata parse failure shouldn't break reads
			entry.Metadata = nil
		}
	}

	entry.CreatedAt = createdAt
	entry.UpdatedAt = updatedAt
	return &entry, nil
}

// marshalMetadata converts a metadata map to a JSON byte slice.
// Returns nil (not error) for nil/empty maps so the DB column receives NULL.
func marshalMetadata(metadata map[string]any) ([]byte, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	return json.Marshal(metadata)
}
