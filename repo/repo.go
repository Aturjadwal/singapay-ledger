package repo

import (
	"context"
	"database/sql"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

type DBTX interface {
	Exec(string, ...any) (sql.Result, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// RepositoryProvider is an interface that provides access to all repositories.
// This allows for dependency injection of fake/mock implementations in tests.
type RepositoryProvider interface {
	Account() domain.AccountRepository
	LedgerEntry() domain.LedgerEntryRepository
	Journal() domain.JournalRepository
	ProductTransaction() domain.ProductTransactionRepository
	PaymentRequest() domain.PaymentRequestRepository
	FeeConfig() domain.FeeConfigRepository
	Disbursement() domain.DisbursementRepository
	SettlementBatch() domain.SettlementBatchRepository
	SettlementItem() domain.SettlementItemRepository
	SettlementNotification() domain.SettlementNotificationRepository
	ReconciliationDiscrepancy() domain.ReconciliationDiscrepancyRepository
	Verification() domain.VerificationRepository
}

// PostgresRepositoryProvider implements RepositoryProvider interface with PostgreSQL.
type PostgresRepositoryProvider struct {
	db *sql.DB
}

func NewRepositoryProvider(db *sql.DB) RepositoryProvider {
	return &PostgresRepositoryProvider{db: db}
}

func (p *PostgresRepositoryProvider) Account() domain.AccountRepository {
	return NewPostgresAccountRepository(p.db)
}

func (p *PostgresRepositoryProvider) LedgerEntry() domain.LedgerEntryRepository {
	return NewPostgresLedgerEntryRepository(p.db)
}

func (p *PostgresRepositoryProvider) Journal() domain.JournalRepository {
	return NewPostgresJournalRepository(p.db)
}

func (p *PostgresRepositoryProvider) ProductTransaction() domain.ProductTransactionRepository {
	return NewPostgresProductTransactionRepository(p.db)
}

func (p *PostgresRepositoryProvider) PaymentRequest() domain.PaymentRequestRepository {
	return NewPostgresPaymentRequestRepository(p.db)
}

func (p *PostgresRepositoryProvider) FeeConfig() domain.FeeConfigRepository {
	return NewPostgresFeeConfigRepository(p.db)
}

func (p *PostgresRepositoryProvider) Disbursement() domain.DisbursementRepository {
	return NewPostgresDisbursementRepository(p.db)
}

func (p *PostgresRepositoryProvider) SettlementBatch() domain.SettlementBatchRepository {
	return NewPostgresSettlementBatchRepository(p.db)
}

func (p *PostgresRepositoryProvider) SettlementItem() domain.SettlementItemRepository {
	return NewPostgresSettlementItemRepository(p.db)
}

func (p *PostgresRepositoryProvider) SettlementNotification() domain.SettlementNotificationRepository {
	return NewPostgresSettlementNotificationRepository(p.db)
}

func (p *PostgresRepositoryProvider) ReconciliationDiscrepancy() domain.ReconciliationDiscrepancyRepository {
	return NewPostgresReconciliationDiscrepancyRepository(p.db)
}

func (p *PostgresRepositoryProvider) Verification() domain.VerificationRepository {
	return NewPostgresVerificationRepository(p.db)
}
