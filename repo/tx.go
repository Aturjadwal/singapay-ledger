package repo

import (
	"database/sql"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

type Tx interface {
	Account() domain.AccountRepository
	LedgerEntry() domain.LedgerEntryRepository
	Journal() domain.JournalRepository
	ProductTransaction() domain.ProductTransactionRepository
	PaymentRequest() domain.PaymentRequestRepository
	FeeConfig() domain.FeeConfigRepository
	Disbursement() domain.DisbursementRepository
	SettlementNotification() domain.SettlementNotificationRepository
}

type postgresTx struct {
	tx *sql.Tx
}

func (p *postgresTx) Account() domain.AccountRepository {
	return NewPostgresAccountRepository(p.tx)
}

func (p *postgresTx) LedgerEntry() domain.LedgerEntryRepository {
	return NewPostgresLedgerEntryRepository(p.tx)
}

func (p *postgresTx) Journal() domain.JournalRepository {
	return NewPostgresJournalRepository(p.tx)
}

func (p *postgresTx) ProductTransaction() domain.ProductTransactionRepository {
	return NewPostgresProductTransactionRepository(p.tx)
}

func (p *postgresTx) PaymentRequest() domain.PaymentRequestRepository {
	return NewPostgresPaymentRequestRepository(p.tx)
}

func (p *postgresTx) FeeConfig() domain.FeeConfigRepository {
	return NewPostgresFeeConfigRepository(p.tx)
}

func (p *postgresTx) Disbursement() domain.DisbursementRepository {
	return NewPostgresDisbursementRepository(p.tx)
}

func (p *postgresTx) SettlementNotification() domain.SettlementNotificationRepository {
	return NewPostgresSettlementNotificationRepository(p.tx)
}
