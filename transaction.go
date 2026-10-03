package ledger

import (
	"context"
	"sort"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
)

// TransactionDetail is one product transaction as the ledger holds it: its priced
// breakdown and state, the instrument it was issued on, and every journal and entry booked
// against it.
//
// It exists so a consumer can explain a payment end to end — to an operator, say — without
// reading this package's tables with SQL of its own (AGENTS.md §1). Nothing in it is
// computed: every field is what a table says.
type TransactionDetail struct {
	Transaction *domain.ProductTransaction
	// PaymentRequest is the instrument the transaction was issued on; nil for a
	// transaction that has none.
	PaymentRequest *domain.PaymentRequest
	// Journals are the journals booked against the transaction, oldest first: the payment
	// (PAYMENT_SUCCESS) and, once it has settled, the settlement — including a settlement
	// booked under a batch journal by the old DOKU reconciler, found through its entries.
	Journals []*domain.Journal
	// Entries are the ledger entries booked against the transaction, oldest first. Each
	// names its journal (JournalUUID) and its account (AccountUUID).
	Entries []*domain.LedgerEntry
	// Accounts holds the seller's account and every account the entries name, by UUID.
	Accounts map[string]*domain.Account
}

// GetTransactionDetail reads one product transaction with everything the ledger booked for
// it. Read only: nothing is asked of Singapay and nothing is written.
func (c *LedgerClient) GetTransactionDetail(ctx context.Context, productTransactionID string) (*TransactionDetail, error) {
	if productTransactionID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "product transaction id is required", nil)
	}

	tx, err := c.repoProvider.ProductTransaction().GetByID(ctx, productTransactionID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrProductTransactionNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the product transaction", err)
	}

	detail := &TransactionDetail{
		Transaction: tx,
		Accounts:    map[string]*domain.Account{},
	}

	paymentReq, err := c.repoProvider.PaymentRequest().GetByProductTransactionID(ctx, tx.UUID)
	switch {
	case err == nil:
		detail.PaymentRequest = paymentReq
	case !ledgererr.IsAppError(err, repo.ErrNotFound):
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the payment request", err)
	}

	journals, err := c.repoProvider.Journal().GetBySourceID(ctx, domain.SourceTypeProductTransaction, tx.UUID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the transaction's journals", err)
	}

	entries, err := c.repoProvider.LedgerEntry().GetBySourceID(ctx, tx.UUID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the transaction's ledger entries", err)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt.Before(entries[j].CreatedAt) })
	detail.Entries = entries

	// An entry can belong to a journal whose source is not the transaction: the old DOKU
	// batch reconciler booked settlements under a SETTLEMENT_BATCH journal. Those are read
	// by id, so every entry shows under the journal that booked it.
	known := make(map[string]bool, len(journals))
	for _, j := range journals {
		known[j.UUID] = true
	}
	for _, entry := range entries {
		if entry.JournalUUID == "" || known[entry.JournalUUID] {
			continue
		}
		known[entry.JournalUUID] = true
		journal, err := c.repoProvider.Journal().GetByID(ctx, entry.JournalUUID)
		if err != nil {
			if ledgererr.IsAppError(err, repo.ErrNotFound) {
				continue
			}
			return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read a journal", err)
		}
		journals = append(journals, journal)
	}
	sort.SliceStable(journals, func(i, j int) bool { return journals[i].CreatedAt.Before(journals[j].CreatedAt) })
	detail.Journals = journals

	accountIDs := []string{tx.SellerAccountID}
	for _, entry := range entries {
		accountIDs = append(accountIDs, entry.AccountUUID)
	}
	for _, id := range accountIDs {
		if _, seen := detail.Accounts[id]; seen || id == "" {
			continue
		}
		account, err := c.repoProvider.Account().GetByID(ctx, id)
		if err != nil {
			// An entry naming an account that is gone is history worth showing, not a
			// reason to show nothing.
			if ledgererr.IsAppError(err, repo.ErrNotFound) {
				continue
			}
			return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read a ledger account", err)
		}
		detail.Accounts[id] = account
	}

	return detail, nil
}
