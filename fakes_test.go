package ledger

// In-memory repository fakes shared by every test in this package.
//
// They are fakes rather than mocks on purpose: the paths under test here decide what
// happens to money by reading balances back after writing entries, so a stub that records
// calls without storing anything would prove nothing. GetAllBalances sums the entries it
// was actually given, which is what makes a double-payout or a lost reservation visible in
// a test at all.

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/repo"
)

// ═══════════════════════════════════════════════════════════════════════════
// FAKE IMPLEMENTATIONS - In-memory repositories for testing
// ═══════════════════════════════════════════════════════════════════════════

// FakeAccountRepository provides in-memory account storage
type FakeAccountRepository struct {
	accounts map[string]*domain.Account
	byOwner  map[string]*domain.Account // key: "ownerType:ownerID"
	bySeller map[string]*domain.Account // key: sellerID

	// lockedForUpdate records every account the code took a row lock on, so a test can
	// prove the withdrawal path locks before it reads the balance.
	lockedForUpdate []string
}

func NewFakeAccountRepository() *FakeAccountRepository {
	return &FakeAccountRepository{
		accounts: make(map[string]*domain.Account),
		byOwner:  make(map[string]*domain.Account),
		bySeller: make(map[string]*domain.Account),
	}
}

func (f *FakeAccountRepository) GetByID(ctx context.Context, id string) (*domain.Account, error) {
	if acc, ok := f.accounts[id]; ok {
		return acc, nil
	}
	return nil, repo.ErrNotFound
}

// GetByIDForUpdate has no locking to do in memory; the fakes run one goroutine at a time.
// It exists so the fake still satisfies the interface, and so tests can assert the
// withdrawal path takes the lock at all.
func (f *FakeAccountRepository) GetByIDForUpdate(ctx context.Context, id string) (*domain.Account, error) {
	f.lockedForUpdate = append(f.lockedForUpdate, id)
	return f.GetByID(ctx, id)
}

func (f *FakeAccountRepository) GetByOwner(ctx context.Context, ownerType domain.OwnerType, ownerID string) (*domain.Account, error) {
	key := string(ownerType) + ":" + ownerID
	if acc, ok := f.byOwner[key]; ok {
		return acc, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeAccountRepository) GetBySingapayAccountID(ctx context.Context, singapayAccountID string) (*domain.Account, error) {
	for _, acc := range f.accounts {
		if acc.SingapayAccountID != "" && acc.SingapayAccountID == singapayAccountID {
			return acc, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakeAccountRepository) GetBySellerID(ctx context.Context, sellerID string) (*domain.Account, error) {
	if acc, ok := f.bySeller[sellerID]; ok {
		return acc, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeAccountRepository) GetPlatformAccount(ctx context.Context) (*domain.Account, error) {
	return f.GetByOwner(ctx, domain.OwnerTypePlatform, "platform")
}

func (f *FakeAccountRepository) GetPaymentGatewayAccount(ctx context.Context) (*domain.Account, error) {
	return f.GetByOwner(ctx, domain.OwnerTypePaymentGateway, "SINGAPAY")
}

func (f *FakeAccountRepository) Save(ctx context.Context, account *domain.Account) error {
	f.accounts[account.UUID] = account
	key := string(account.OwnerType) + ":" + account.OwnerID
	f.byOwner[key] = account
	if account.OwnerType == domain.OwnerTypeSeller {
		f.bySeller[account.OwnerID] = account
	}
	return nil
}

func (f *FakeAccountRepository) Delete(ctx context.Context, id string) error {
	delete(f.accounts, id)
	return nil
}

func (f *FakeAccountRepository) UpdateBalances(ctx context.Context, accountID string, pendingDelta, availableDelta int64) error {
	acc, ok := f.accounts[accountID]
	if !ok {
		return repo.ErrNotFound
	}
	acc.PendingBalance += pendingDelta
	acc.AvailableBalance += availableDelta
	return nil
}

func (f *FakeAccountRepository) IncrementDeposit(ctx context.Context, accountID string, amount int64) error {
	acc, ok := f.accounts[accountID]
	if !ok {
		return repo.ErrNotFound
	}
	acc.TotalDepositAmount += amount
	return nil
}

func (f *FakeAccountRepository) IncrementWithdrawal(ctx context.Context, accountID string, amount int64) error {
	acc, ok := f.accounts[accountID]
	if !ok {
		return repo.ErrNotFound
	}
	acc.TotalWithdrawalAmount += amount
	return nil
}

// FakeLedgerEntryRepository provides in-memory ledger entry storage
type FakeLedgerEntryRepository struct {
	entries []*domain.LedgerEntry
}

func NewFakeLedgerEntryRepository() *FakeLedgerEntryRepository {
	return &FakeLedgerEntryRepository{
		entries: make([]*domain.LedgerEntry, 0),
	}
}

func (f *FakeLedgerEntryRepository) Save(ctx context.Context, entry *domain.LedgerEntry) error {
	f.entries = append(f.entries, entry)
	return nil
}

func (f *FakeLedgerEntryRepository) SaveBatch(ctx context.Context, entries []*domain.LedgerEntry) error {
	f.entries = append(f.entries, entries...)
	return nil
}

func (f *FakeLedgerEntryRepository) GetBalance(ctx context.Context, accountID string, bucket domain.BalanceBucket) (int64, error) {
	var balance int64
	for _, entry := range f.entries {
		if entry.AccountUUID == accountID && entry.BalanceBucket == bucket {
			balance += entry.Amount
		}
	}
	return balance, nil
}

func (f *FakeLedgerEntryRepository) GetAllBalances(ctx context.Context, accountID string) (pending, available int64, err error) {
	for _, entry := range f.entries {
		if entry.AccountUUID == accountID {
			switch entry.BalanceBucket {
			case domain.BalanceBucketPending:
				pending += entry.Amount
			case domain.BalanceBucketAvailable:
				available += entry.Amount
			}
		}
	}
	return pending, available, nil
}

func (f *FakeLedgerEntryRepository) SumPendingBalanceBySellerID(ctx context.Context, sellerID string) (int64, error) {
	return 0, nil
}

func (f *FakeLedgerEntryRepository) SumAvailableBalanceBySellerID(ctx context.Context, sellerID string) (int64, error) {
	return 0, nil
}

func (f *FakeLedgerEntryRepository) GetAllBalancesBySellerID(ctx context.Context, sellerID string) (pending, available int64, err error) {
	return 0, 0, nil
}

func (f *FakeLedgerEntryRepository) GetByJournalID(ctx context.Context, journalID string) ([]*domain.LedgerEntry, error) {
	return nil, nil
}

func (f *FakeLedgerEntryRepository) GetBySourceID(ctx context.Context, sourceID string) ([]*domain.LedgerEntry, error) {
	var out []*domain.LedgerEntry
	for _, e := range f.entries {
		if e.SourceID == sourceID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *FakeLedgerEntryRepository) GetByAccountID(ctx context.Context, accountID string, limit, offset int) ([]*domain.LedgerEntry, error) {
	return nil, nil
}

func (f *FakeLedgerEntryRepository) GetLastBalanceAfter(ctx context.Context, accountID string, bucket domain.BalanceBucket) (int64, error) {
	return 0, nil
}

// FakeProductTransactionRepository provides in-memory product transaction storage
type FakeProductTransactionRepository struct {
	transactions map[string]*domain.ProductTransaction
	byInvoice    map[string]*domain.ProductTransaction

	// beforeCAS, when set, runs once immediately before UpdateStatusIf compares. It is
	// how a test lands a concurrent delivery inside the read-then-write window.
	beforeCAS func()
}

func NewFakeProductTransactionRepository() *FakeProductTransactionRepository {
	return &FakeProductTransactionRepository{
		transactions: make(map[string]*domain.ProductTransaction),
		byInvoice:    make(map[string]*domain.ProductTransaction),
	}
}

// detach copies a stored transaction on the way out, the way a real read does.
//
// Handing back the stored pointer makes a test that mutates what it read also mutate the
// "row", which quietly turns a read-then-conditional-write into a single aliased object —
// and that is exactly the shape the compare-and-set exists to defend against. The fake has
// to be able to disagree with the caller's copy or it cannot model the race at all.
func detach(tx *domain.ProductTransaction) *domain.ProductTransaction {
	copied := *tx
	return &copied
}

func (f *FakeProductTransactionRepository) GetByID(ctx context.Context, id string) (*domain.ProductTransaction, error) {
	if tx, ok := f.transactions[id]; ok {
		return detach(tx), nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeProductTransactionRepository) GetByInvoiceNumber(ctx context.Context, invoiceNumber string) (*domain.ProductTransaction, error) {
	if tx, ok := f.byInvoice[invoiceNumber]; ok {
		return detach(tx), nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeProductTransactionRepository) GetBySellerAccountID(ctx context.Context, sellerAccountID string, page, pageSize int) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) GetByBuyerAccountID(ctx context.Context, buyerAccountID string, page, pageSize int) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) GetPendingBySellerAccountID(ctx context.Context, sellerAccountID string) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) GetCompletedNotSettled(ctx context.Context, sellerAccountID string) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) GetAllBySellerID(ctx context.Context, sellerAccountID string) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) GetBySellerAccountIDWithCursor(ctx context.Context, sellerAccountID string, cursor string, pageSize int, sortOrder string) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) Save(ctx context.Context, tx *domain.ProductTransaction) error {
	f.transactions[tx.UUID] = tx
	if tx.InvoiceNumber != "" {
		f.byInvoice[tx.InvoiceNumber] = tx
	}
	return nil
}

func (f *FakeProductTransactionRepository) UpdateStatus(ctx context.Context, id string, status domain.TransactionStatus, timestamp time.Time) error {
	tx, ok := f.transactions[id]
	if !ok {
		return repo.ErrNotFound
	}
	tx.Status = status
	if status == domain.TransactionStatusSettled {
		tx.SettledAt = &timestamp
	}
	return nil
}

// UpdateStatusIf is the fake's compare-and-set. It is not concurrency-safe and does not
// need to be — the map behind it is not either — but it reproduces the property the real
// one is there for: the second caller to ask for the same transition is told no.
//
// beforeCAS runs just before the comparison, which is where a test injects the concurrent
// delivery that commits first. In Postgres that window is closed by the row lock; here it
// is opened deliberately, because the point is to prove the loser writes nothing.
func (f *FakeProductTransactionRepository) UpdateStatusIf(ctx context.Context, id string, from, to domain.TransactionStatus, timestamp time.Time) (bool, error) {
	if f.beforeCAS != nil {
		hook := f.beforeCAS
		f.beforeCAS = nil // once: the injected delivery must not recurse into itself
		hook()
	}
	return f.updateStatusIf(id, from, to, timestamp)
}

func (f *FakeProductTransactionRepository) updateStatusIf(id string, from, to domain.TransactionStatus, timestamp time.Time) (bool, error) {
	tx, ok := f.transactions[id]
	if !ok {
		return false, repo.ErrNotFound
	}
	if tx.Status != from {
		return false, nil
	}
	tx.Status = to
	switch to {
	case domain.TransactionStatusCompleted:
		tx.CompletedAt = &timestamp
	case domain.TransactionStatusSettled:
		tx.SettledAt = &timestamp
	}
	return true, nil
}

func (f *FakeProductTransactionRepository) SaveTransferRequestID(ctx context.Context, id string, requestID string) error {
	return nil
}

func (f *FakeProductTransactionRepository) MarkPlatformFeeTransferred(ctx context.Context, id string) error {
	return nil
}

// GetAwaitingSettlement returns COMPLETED transactions, oldest first, the way the real
// repository does. Nothing consumes it while reconciliation is unimplemented, but the fake
// has to satisfy the interface, and a stub that returned nil would hide the day it starts
// mattering.
func (f *FakeProductTransactionRepository) GetAwaitingSettlement(ctx context.Context, limit int) ([]*domain.ProductTransaction, error) {
	var result []*domain.ProductTransaction
	for _, tx := range f.transactions {
		if tx.Status == domain.TransactionStatusCompleted {
			result = append(result, tx)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (f *FakeProductTransactionRepository) GetSettledWithoutPlatformFeeTransfer(ctx context.Context, limit int) ([]*domain.ProductTransaction, error) {
	return nil, nil
}

func (f *FakeProductTransactionRepository) SaveSettledFees(ctx context.Context, id string, platformFee, gatewayFee int64) error {
	tx, ok := f.transactions[id]
	if !ok {
		return repo.ErrNotFound
	}
	tx.SettledPlatformFee = &platformFee
	tx.SettledGatewayFee = &gatewayFee
	return nil
}

func (f *FakeProductTransactionRepository) OldestAwaitingSettlement(ctx context.Context) (time.Time, bool, error) {
	var oldest time.Time
	found := false
	for _, tx := range f.transactions {
		if tx.Status != domain.TransactionStatusCompleted || tx.CompletedAt == nil {
			continue
		}
		if !found || tx.CompletedAt.Before(oldest) {
			oldest = *tx.CompletedAt
			found = true
		}
	}
	return oldest, found, nil
}

// FakeSettlementNotificationRepository provides in-memory settlement inbox storage.
type FakeSettlementNotificationRepository struct {
	notifications map[string]*domain.SettlementNotification
}

func NewFakeSettlementNotificationRepository() *FakeSettlementNotificationRepository {
	return &FakeSettlementNotificationRepository{
		notifications: make(map[string]*domain.SettlementNotification),
	}
}

// Save mirrors the real ON CONFLICT DO NOTHING: a repeat delivery of the same
// (settlement_id, event) stores nothing and is reported as not stored, never as an error.
func (f *FakeSettlementNotificationRepository) Save(ctx context.Context, n *domain.SettlementNotification) (bool, error) {
	for _, existing := range f.notifications {
		if existing.SettlementID == n.SettlementID && existing.Event == n.Event {
			return false, nil
		}
	}
	f.notifications[n.UUID] = n
	return true, nil
}

func (f *FakeSettlementNotificationRepository) GetByID(ctx context.Context, id string) (*domain.SettlementNotification, error) {
	n, ok := f.notifications[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return n, nil
}

func (f *FakeSettlementNotificationRepository) GetByIdentity(ctx context.Context, settlementID, event string) (*domain.SettlementNotification, error) {
	for _, n := range f.notifications {
		if n.SettlementID == settlementID && n.Event == event {
			return n, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakeSettlementNotificationRepository) GetActionable(ctx context.Context, limit int) ([]*domain.SettlementNotification, error) {
	var result []*domain.SettlementNotification
	for _, n := range f.notifications {
		if n.IsActionable() {
			result = append(result, n)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (f *FakeSettlementNotificationRepository) CountActionable(ctx context.Context) (int, error) {
	count := 0
	for _, n := range f.notifications {
		if n.IsActionable() {
			count++
		}
	}
	return count, nil
}

func (f *FakeSettlementNotificationRepository) ClaimIfActionable(ctx context.Context, id string) (bool, error) {
	n, ok := f.notifications[id]
	if !ok {
		return false, repo.ErrNotFound
	}
	if !n.IsActionable() {
		return false, nil
	}
	n.Status = domain.SettlementNotificationProcessing
	return true, nil
}

func (f *FakeSettlementNotificationRepository) MarkProcessed(ctx context.Context, id string) error {
	n, ok := f.notifications[id]
	if !ok {
		return repo.ErrNotFound
	}
	now := time.Now()
	n.Status = domain.SettlementNotificationProcessed
	n.ProcessedAt = &now
	n.FailureReason = ""
	return nil
}

func (f *FakeSettlementNotificationRepository) MarkFailed(ctx context.Context, id string, reason string) error {
	n, ok := f.notifications[id]
	if !ok {
		return repo.ErrNotFound
	}
	n.Status = domain.SettlementNotificationFailed
	n.FailureReason = reason
	return nil
}

func (f *FakeSettlementNotificationRepository) MarkNeedsReview(ctx context.Context, id string, reason string) error {
	n, ok := f.notifications[id]
	if !ok {
		return repo.ErrNotFound
	}
	n.Status = domain.SettlementNotificationNeedsReview
	n.FailureReason = reason
	return nil
}

// FakeSettlementBatchRepository provides in-memory settlement batch storage
type FakeSettlementBatchRepository struct {
	batches map[string]*domain.SettlementBatch
}

func NewFakeSettlementBatchRepository() *FakeSettlementBatchRepository {
	return &FakeSettlementBatchRepository{
		batches: make(map[string]*domain.SettlementBatch),
	}
}

func (f *FakeSettlementBatchRepository) Save(ctx context.Context, batch *domain.SettlementBatch) error {
	f.batches[batch.UUID] = batch
	return nil
}

func (f *FakeSettlementBatchRepository) GetByID(ctx context.Context, id string) (*domain.SettlementBatch, error) {
	if batch, ok := f.batches[id]; ok {
		return batch, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeSettlementBatchRepository) GetByAccountID(ctx context.Context, accountID string, page, pageSize int) ([]*domain.SettlementBatch, error) {
	return nil, nil
}

func (f *FakeSettlementBatchRepository) GetBySettlementDate(ctx context.Context, accountID string, settlementDate time.Time) (*domain.SettlementBatch, error) {
	return nil, nil
}

func (f *FakeSettlementBatchRepository) GetByLedgerID(ctx context.Context, ledgerID string, page, pageSize int) ([]*domain.SettlementBatch, error) {
	return nil, nil
}

func (f *FakeSettlementBatchRepository) GetByLedgerIDAndDate(ctx context.Context, ledgerID string, settlementDate time.Time) (*domain.SettlementBatch, error) {
	return nil, nil
}

func (f *FakeSettlementBatchRepository) GetByBatchID(ctx context.Context, batchID string) (*domain.SettlementBatch, error) {
	if batchID == "" {
		return nil, repo.ErrNotFound
	}
	for _, batch := range f.batches {
		if batch.BatchID == batchID {
			return batch, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakeSettlementBatchRepository) FilterIngestedBatchIDs(ctx context.Context, batchIDs []string) (map[string]struct{}, error) {
	ingested := make(map[string]struct{}, len(batchIDs))
	for _, id := range batchIDs {
		for _, batch := range f.batches {
			if batch.BatchID == id {
				ingested[id] = struct{}{}
				break
			}
		}
	}
	return ingested, nil
}

func (f *FakeSettlementBatchRepository) UpdateStatus(ctx context.Context, id string, status domain.SettlementBatchStatus, processedAt *time.Time, failureReason string) error {
	if batch, ok := f.batches[id]; ok {
		batch.ProcessingStatus = status
		batch.ProcessedAt = processedAt
		return nil
	}
	return repo.ErrNotFound
}

// FakeSettlementItemRepository provides in-memory settlement item storage
type FakeSettlementItemRepository struct {
	items map[string]*domain.SettlementItem
}

func NewFakeSettlementItemRepository() *FakeSettlementItemRepository {
	return &FakeSettlementItemRepository{
		items: make(map[string]*domain.SettlementItem),
	}
}

func (f *FakeSettlementItemRepository) Save(ctx context.Context, item *domain.SettlementItem) error {
	f.items[item.UUID] = item
	return nil
}

func (f *FakeSettlementItemRepository) SaveBatch(ctx context.Context, items []*domain.SettlementItem) error {
	for _, item := range items {
		f.items[item.UUID] = item
	}
	return nil
}

func (f *FakeSettlementItemRepository) GetByID(ctx context.Context, id string) (*domain.SettlementItem, error) {
	if item, ok := f.items[id]; ok {
		return item, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeSettlementItemRepository) GetBySettlementBatchID(ctx context.Context, batchID string) ([]*domain.SettlementItem, error) {
	var result []*domain.SettlementItem
	for _, item := range f.items {
		if item.SettlementBatchUUID == batchID {
			result = append(result, item)
		}
	}
	return result, nil
}

func (f *FakeSettlementItemRepository) GetByProductTransactionID(ctx context.Context, txID string) ([]*domain.SettlementItem, error) {
	var result []*domain.SettlementItem
	for _, item := range f.items {
		if item.ProductTransactionUUID == txID {
			result = append(result, item)
		}
	}
	return result, nil
}

func (f *FakeSettlementItemRepository) GetUnmatchedByBatchID(ctx context.Context, batchID string) ([]*domain.SettlementItem, error) {
	var result []*domain.SettlementItem
	for _, item := range f.items {
		if item.SettlementBatchUUID == batchID && !item.IsMatched {
			result = append(result, item)
		}
	}
	return result, nil
}

// FakeJournalRepository provides in-memory journal storage
type FakeJournalRepository struct {
	journals map[string]*domain.Journal
}

func NewFakeJournalRepository() *FakeJournalRepository {
	return &FakeJournalRepository{
		journals: make(map[string]*domain.Journal),
	}
}

func (f *FakeJournalRepository) Save(ctx context.Context, journal *domain.Journal) error {
	f.journals[journal.UUID] = journal
	return nil
}

func (f *FakeJournalRepository) GetByID(ctx context.Context, id string) (*domain.Journal, error) {
	if j, ok := f.journals[id]; ok {
		return j, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeJournalRepository) GetBySourceID(ctx context.Context, sourceType domain.SourceType, sourceID string) ([]*domain.Journal, error) {
	return nil, nil
}

func (f *FakeJournalRepository) GetByEventType(ctx context.Context, eventType domain.EventType, page, pageSize int) ([]*domain.Journal, error) {
	return nil, nil
}

// FakeReconciliationDiscrepancyRepository provides in-memory discrepancy storage
type FakeReconciliationDiscrepancyRepository struct {
	discrepancies map[string]*domain.ReconciliationDiscrepancy
}

func NewFakeReconciliationDiscrepancyRepository() *FakeReconciliationDiscrepancyRepository {
	return &FakeReconciliationDiscrepancyRepository{
		discrepancies: make(map[string]*domain.ReconciliationDiscrepancy),
	}
}

func (f *FakeReconciliationDiscrepancyRepository) Save(ctx context.Context, discrepancy *domain.ReconciliationDiscrepancy) error {
	f.discrepancies[discrepancy.UUID] = discrepancy
	return nil
}

func (f *FakeReconciliationDiscrepancyRepository) GetByID(ctx context.Context, id string) (*domain.ReconciliationDiscrepancy, error) {
	if d, ok := f.discrepancies[id]; ok {
		return d, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeReconciliationDiscrepancyRepository) GetByAccountIDAndBatchID(ctx context.Context, accountID, batchID string) (*domain.ReconciliationDiscrepancy, error) {
	return nil, nil
}

func (f *FakeReconciliationDiscrepancyRepository) GetByStatus(ctx context.Context, status domain.DiscrepancyStatus, page, pageSize int) ([]*domain.ReconciliationDiscrepancy, error) {
	return nil, nil
}

func (f *FakeReconciliationDiscrepancyRepository) GetByLedgerID(ctx context.Context, ledgerID string, limit, offset int) ([]domain.ReconciliationDiscrepancy, error) {
	return nil, nil
}

func (f *FakeReconciliationDiscrepancyRepository) GetBySettlementBatchID(ctx context.Context, batchID string) (*domain.ReconciliationDiscrepancy, error) {
	for _, disc := range f.discrepancies {
		if disc.SettlementBatchUUID == batchID {
			return disc, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakeReconciliationDiscrepancyRepository) GetPendingDiscrepancies(ctx context.Context, limit int) ([]domain.ReconciliationDiscrepancy, error) {
	return nil, nil // Stub - implement if needed
}

func (f *FakeReconciliationDiscrepancyRepository) MarkResolved(ctx context.Context, id string, notes string) error {
	return nil // Stub - implement if needed
}

// FakePaymentRequestRepository provides in-memory payment-request storage, keyed the two
// ways the money-in webhook looks one up.
type FakePaymentRequestRepository struct {
	byID      map[string]*domain.PaymentRequest
	byProduct map[string]*domain.PaymentRequest

	// updates counts Update calls, so a duplicate delivery can be shown to have written
	// nothing rather than merely to have returned nil.
	updates int
}

func NewFakePaymentRequestRepository() *FakePaymentRequestRepository {
	return &FakePaymentRequestRepository{
		byID:      make(map[string]*domain.PaymentRequest),
		byProduct: make(map[string]*domain.PaymentRequest),
	}
}

func (f *FakePaymentRequestRepository) GetByID(ctx context.Context, id string) (*domain.PaymentRequest, error) {
	if pr, ok := f.byID[id]; ok {
		return pr, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakePaymentRequestRepository) GetByRequestID(ctx context.Context, requestID string) (*domain.PaymentRequest, error) {
	for _, pr := range f.byID {
		if pr.RequestID == requestID {
			return pr, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakePaymentRequestRepository) GetByPaymentCode(ctx context.Context, paymentCode string) (*domain.PaymentRequest, error) {
	for _, pr := range f.byID {
		if pr.PaymentCode == paymentCode {
			return pr, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakePaymentRequestRepository) GetByProductTransactionID(ctx context.Context, productTransactionID string) (*domain.PaymentRequest, error) {
	if pr, ok := f.byProduct[productTransactionID]; ok {
		return pr, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakePaymentRequestRepository) Save(ctx context.Context, pr *domain.PaymentRequest) error {
	f.byID[pr.UUID] = pr
	f.byProduct[pr.ProductTransactionUUID] = pr
	return nil
}

func (f *FakePaymentRequestRepository) Update(ctx context.Context, pr *domain.PaymentRequest) error {
	f.updates++
	f.byID[pr.UUID] = pr
	f.byProduct[pr.ProductTransactionUUID] = pr
	return nil
}

// FakeRepositoryProvider implements repo.RepositoryProvider interface
// This allows us to inject fakes into LedgerClient
type FakeRepositoryProvider struct {
	accountRepo                   *FakeAccountRepository
	ledgerEntryRepo               *FakeLedgerEntryRepository
	productTransactionRepo        *FakeProductTransactionRepository
	settlementBatchRepo           *FakeSettlementBatchRepository
	settlementItemRepo            *FakeSettlementItemRepository
	settlementNotificationRepo    *FakeSettlementNotificationRepository
	journalRepo                   *FakeJournalRepository
	reconciliationDiscrepancyRepo *FakeReconciliationDiscrepancyRepository
	disbursementRepo              *FakeDisbursementRepository
	paymentRequestRepo            *FakePaymentRequestRepository
}

// Ensure FakeRepositoryProvider implements repo.RepositoryProvider at compile time
var _ repo.RepositoryProvider = (*FakeRepositoryProvider)(nil)

// Ensure FakeRepositoryProvider also implements repo.Tx (same interface)
var _ repo.Tx = (*FakeRepositoryProvider)(nil)

func NewFakeRepositoryProvider() *FakeRepositoryProvider {
	return &FakeRepositoryProvider{
		accountRepo:                   NewFakeAccountRepository(),
		ledgerEntryRepo:               NewFakeLedgerEntryRepository(),
		productTransactionRepo:        NewFakeProductTransactionRepository(),
		settlementBatchRepo:           NewFakeSettlementBatchRepository(),
		settlementItemRepo:            NewFakeSettlementItemRepository(),
		settlementNotificationRepo:    NewFakeSettlementNotificationRepository(),
		journalRepo:                   NewFakeJournalRepository(),
		reconciliationDiscrepancyRepo: NewFakeReconciliationDiscrepancyRepository(),
		disbursementRepo:              NewFakeDisbursementRepository(),
		paymentRequestRepo:            NewFakePaymentRequestRepository(),
	}
}

func (f *FakeRepositoryProvider) Account() domain.AccountRepository {
	return f.accountRepo
}

func (f *FakeRepositoryProvider) LedgerEntry() domain.LedgerEntryRepository {
	return f.ledgerEntryRepo
}

func (f *FakeRepositoryProvider) ProductTransaction() domain.ProductTransactionRepository {
	return f.productTransactionRepo
}

func (f *FakeRepositoryProvider) SettlementBatch() domain.SettlementBatchRepository {
	return f.settlementBatchRepo
}

func (f *FakeRepositoryProvider) SettlementItem() domain.SettlementItemRepository {
	return f.settlementItemRepo
}

func (f *FakeRepositoryProvider) SettlementNotification() domain.SettlementNotificationRepository {
	return f.settlementNotificationRepo
}

func (f *FakeRepositoryProvider) Journal() domain.JournalRepository {
	return f.journalRepo
}

func (f *FakeRepositoryProvider) ReconciliationDiscrepancy() domain.ReconciliationDiscrepancyRepository {
	return f.reconciliationDiscrepancyRepo
}

func (f *FakeRepositoryProvider) PaymentRequest() domain.PaymentRequestRepository {
	return f.paymentRequestRepo
}

func (f *FakeRepositoryProvider) FeeConfig() domain.FeeConfigRepository {
	return nil // Not needed for reconciliation tests
}

func (f *FakeRepositoryProvider) Disbursement() domain.DisbursementRepository {
	return f.disbursementRepo
}

func (f *FakeRepositoryProvider) Verification() domain.VerificationRepository {
	return nil // Not needed for reconciliation tests
}

// FakeTransactionProvider implements repo.TransactionProvider for testing
// It doesn't actually provide transaction semantics - just returns the same fakes
type FakeTransactionProvider struct {
	fakes *FakeRepositoryProvider
}

// Ensure FakeTransactionProvider implements repo.TransactionProvider
var _ repo.TransactionProvider = (*FakeTransactionProvider)(nil)

func NewFakeTransactionProvider(fakes *FakeRepositoryProvider) *FakeTransactionProvider {
	return &FakeTransactionProvider{fakes: fakes}
}

// Transact executes the function with fake repositories (no actual transaction)
func (f *FakeTransactionProvider) Transact(ctx context.Context, fn func(tx repo.Tx) error) error {
	// Just call the function with our fakes (which implement repo.Tx)
	return fn(f.fakes)
}

// testLogger keeps test output readable: only errors are printed, so a passing run says
// nothing and a failing one says why.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
}
