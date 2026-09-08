package repo

import (
	"context"
	"database/sql"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/redifu"
)

type PostgresAccountRepository struct {
	db DBTX
}

func NewPostgresAccountRepository(db DBTX) *PostgresAccountRepository {
	return &PostgresAccountRepository{db: db}
}

const accountSelectColumns = `
	uuid, randid, doku_subaccount_id, singapay_account_id, singapay_account_number,
	owner_type, owner_id, currency,
	pending_balance, available_balance, total_withdrawal_amount, total_deposit_amount,
	created_at, updated_at
`

// scanAccount scans a single row into a domain.Account.
//
// Every gateway identifier is nullable: an account is backed by one gateway, so the
// other's columns are empty, and singapay_account_number can be empty even on a Singapay
// account because Singapay may not assign one at creation.
func scanAccount(row interface {
	Scan(dest ...any) error
}) (*domain.Account, error) {
	var a domain.Account
	redifu.InitRecord(&a)
	var (
		dokuSubAccountID      sql.NullString
		singapayAccountID     sql.NullString
		singapayAccountNumber sql.NullString
	)

	err := row.Scan(
		&a.UUID,
		&a.RandId,
		&dokuSubAccountID,
		&singapayAccountID,
		&singapayAccountNumber,
		&a.OwnerType,
		&a.OwnerID,
		&a.Currency,
		&a.PendingBalance,
		&a.AvailableBalance,
		&a.TotalWithdrawalAmount,
		&a.TotalDepositAmount,
		&a.CreatedAt,
		&a.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	a.DokuSubAccountID = dokuSubAccountID.String
	a.SingapayAccountID = singapayAccountID.String
	a.SingapayAccountNumber = singapayAccountNumber.String

	return &a, nil
}

func (r *PostgresAccountRepository) GetByID(ctx context.Context, id string) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, id)
	return scanAccount(row)
}

// GetByIDForUpdate takes a row lock on the account. Only meaningful inside a transaction:
// outside one the lock is released the moment the statement finishes, which protects
// nothing. See the interface doc for why the withdrawal path needs it.
func (r *PostgresAccountRepository) GetByIDForUpdate(ctx context.Context, id string) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE uuid = $1
		FOR UPDATE
	`

	row := r.db.QueryRowContext(ctx, query, id)
	return scanAccount(row)
}

func (r *PostgresAccountRepository) GetByOwner(ctx context.Context, ownerType domain.OwnerType, ownerID string) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE owner_type = $1 AND owner_id = $2
	`

	row := r.db.QueryRowContext(ctx, query, ownerType, ownerID)
	return scanAccount(row)
}

func (r *PostgresAccountRepository) GetByDokuSubAccountID(ctx context.Context, dokuSubAccountID string) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE doku_subaccount_id = $1
	`

	row := r.db.QueryRowContext(ctx, query, dokuSubAccountID)
	return scanAccount(row)
}

// GetBySingapayAccountID looks an account up by its Singapay ULID.
//
// The ULID, not the account number: the number identifies the same account but is only
// ever used as a transfer beneficiary, and is not present on every row.
func (r *PostgresAccountRepository) GetBySingapayAccountID(ctx context.Context, singapayAccountID string) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE singapay_account_id = $1
	`

	row := r.db.QueryRowContext(ctx, query, singapayAccountID)
	return scanAccount(row)
}

func (r *PostgresAccountRepository) GetBySellerID(ctx context.Context, sellerID string) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE owner_type = $1 AND owner_id = $2
	`

	row := r.db.QueryRowContext(ctx, query, domain.OwnerTypeSeller, sellerID)
	return scanAccount(row)
}

func (r *PostgresAccountRepository) GetPlatformAccount(ctx context.Context) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE owner_type = $1
		LIMIT 1
	`

	row := r.db.QueryRowContext(ctx, query, domain.OwnerTypePlatform)
	return scanAccount(row)
}

func (r *PostgresAccountRepository) GetPaymentGatewayAccount(ctx context.Context) (*domain.Account, error) {
	query := `
		SELECT` + accountSelectColumns + `
		FROM ledger_accounts
		WHERE owner_type = $1
		LIMIT 1
	`

	row := r.db.QueryRowContext(ctx, query, domain.OwnerTypePaymentGateway)
	return scanAccount(row)
}

func (r *PostgresAccountRepository) Save(ctx context.Context, account *domain.Account) error {
	query := `
		INSERT INTO ledger_accounts (
			uuid, randid, doku_subaccount_id, singapay_account_id, singapay_account_number,
			owner_type, owner_id, currency,
			pending_balance, available_balance, total_withdrawal_amount, total_deposit_amount,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (uuid) DO UPDATE SET
			doku_subaccount_id      = EXCLUDED.doku_subaccount_id,
			singapay_account_id     = EXCLUDED.singapay_account_id,
			singapay_account_number = EXCLUDED.singapay_account_number,
			owner_type              = EXCLUDED.owner_type,
			owner_id                = EXCLUDED.owner_id,
			currency                = EXCLUDED.currency,
			pending_balance         = EXCLUDED.pending_balance,
			available_balance       = EXCLUDED.available_balance,
			total_withdrawal_amount = EXCLUDED.total_withdrawal_amount,
			total_deposit_amount    = EXCLUDED.total_deposit_amount,
			updated_at              = EXCLUDED.updated_at
	`

	// Empty is written as NULL, never as "". The unique indexes on these columns are
	// partial (WHERE ... IS NOT NULL); a literal empty string would collide across every
	// account that has no identifier for that gateway.
	dokuSubAccountID := nullIfEmpty(account.DokuSubAccountID)
	singapayAccountID := nullIfEmpty(account.SingapayAccountID)
	singapayAccountNumber := nullIfEmpty(account.SingapayAccountNumber)

	_, err := r.db.ExecContext(
		ctx,
		query,
		account.UUID,
		account.RandId,
		dokuSubAccountID,
		singapayAccountID,
		singapayAccountNumber,
		account.OwnerType,
		account.OwnerID,
		account.Currency,
		account.PendingBalance,
		account.AvailableBalance,
		account.TotalWithdrawalAmount,
		account.TotalDepositAmount,
		account.CreatedAt,
		account.UpdatedAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

// UpdateBalances atomically updates pending and available balances by delta amounts.
// Use positive values to increase, negative to decrease.
func (r *PostgresAccountRepository) UpdateBalances(ctx context.Context, accountID string, pendingDelta, availableDelta int64) error {
	query := `
		UPDATE ledger_accounts
		SET pending_balance = pending_balance + $1,
		    available_balance = available_balance + $2,
		    updated_at = NOW()
		WHERE uuid = $3
	`

	result, err := r.db.ExecContext(ctx, query, pendingDelta, availableDelta, accountID)
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

// IncrementDeposit atomically increments the total deposit amount.
func (r *PostgresAccountRepository) IncrementDeposit(ctx context.Context, accountID string, amount int64) error {
	query := `
		UPDATE ledger_accounts
		SET total_deposit_amount = total_deposit_amount + $1,
		    updated_at = NOW()
		WHERE uuid = $2
	`

	result, err := r.db.ExecContext(ctx, query, amount, accountID)
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

// IncrementWithdrawal atomically increments the total withdrawal amount.
func (r *PostgresAccountRepository) IncrementWithdrawal(ctx context.Context, accountID string, amount int64) error {
	query := `
		UPDATE ledger_accounts
		SET total_withdrawal_amount = total_withdrawal_amount + $1,
		    updated_at = NOW()
		WHERE uuid = $2
	`

	result, err := r.db.ExecContext(ctx, query, amount, accountID)
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

func (r *PostgresAccountRepository) Delete(ctx context.Context, id string) error {
	query := `DELETE FROM ledger_accounts WHERE uuid = $1`

	res, err := r.db.ExecContext(ctx, query, id)
	if err != nil {
		return ErrFailedDeleteSQL.WithError(err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return ErrFailedDeleteSQL.WithError(err)
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// nullIfEmpty maps "" to SQL NULL, keeping the partial unique indexes on the gateway
// identifier columns meaningful.
func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
