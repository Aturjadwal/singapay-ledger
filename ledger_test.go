package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/repo"
	"github.com/21strive/redifu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// FAKE IMPLEMENTATIONS - In-memory repositories for testing
// ═══════════════════════════════════════════════════════════════════════════

// FakeAccountRepository provides in-memory account storage
type FakeAccountRepository struct {
	accounts map[string]*domain.Account
	byOwner  map[string]*domain.Account // key: "ownerType:ownerID"
	byDoku   map[string]*domain.Account // key: dokuSubAccountID
	bySeller map[string]*domain.Account // key: sellerID

	// lockedForUpdate records every account the code took a row lock on, so a test can
	// prove the withdrawal path locks before it reads the balance.
	lockedForUpdate []string
}

func NewFakeAccountRepository() *FakeAccountRepository {
	return &FakeAccountRepository{
		accounts: make(map[string]*domain.Account),
		byOwner:  make(map[string]*domain.Account),
		byDoku:   make(map[string]*domain.Account),
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

func (f *FakeAccountRepository) GetByDokuSubAccountID(ctx context.Context, dokuSubAccountID string) (*domain.Account, error) {
	if acc, ok := f.byDoku[dokuSubAccountID]; ok {
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
	return f.GetByOwner(ctx, domain.OwnerTypePaymentGateway, "doku")
}

func (f *FakeAccountRepository) Save(ctx context.Context, account *domain.Account) error {
	f.accounts[account.UUID] = account
	key := string(account.OwnerType) + ":" + account.OwnerID
	f.byOwner[key] = account
	if account.DokuSubAccountID != "" {
		f.byDoku[account.DokuSubAccountID] = account
	}
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
}

func NewFakeProductTransactionRepository() *FakeProductTransactionRepository {
	return &FakeProductTransactionRepository{
		transactions: make(map[string]*domain.ProductTransaction),
		byInvoice:    make(map[string]*domain.ProductTransaction),
	}
}

func (f *FakeProductTransactionRepository) GetByID(ctx context.Context, id string) (*domain.ProductTransaction, error) {
	if tx, ok := f.transactions[id]; ok {
		return tx, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeProductTransactionRepository) GetByInvoiceNumber(ctx context.Context, invoiceNumber string) (*domain.ProductTransaction, error) {
	if tx, ok := f.byInvoice[invoiceNumber]; ok {
		return tx, nil
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

func (f *FakeProductTransactionRepository) SaveTransferRequestID(ctx context.Context, id string, requestID string) error {
	return nil
}

func (f *FakeProductTransactionRepository) MarkPlatformFeeTransferred(ctx context.Context, id string) error {
	return nil
}

func (f *FakeProductTransactionRepository) GetSettledWithoutPlatformFeeTransfer(ctx context.Context, limit int) ([]*domain.ProductTransaction, error) {
	return nil, nil
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

func (f *FakeSettlementBatchRepository) FilterIngestedReportFiles(ctx context.Context, reportFileNames []string) (map[string]struct{}, error) {
	ingested := make(map[string]struct{}, len(reportFileNames))
	for _, name := range reportFileNames {
		for _, batch := range f.batches {
			if batch.ReportFileName == name {
				ingested[name] = struct{}{}
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

// FakeRepositoryProvider implements repo.RepositoryProvider interface
// This allows us to inject fakes into LedgerClient
type FakeRepositoryProvider struct {
	accountRepo                   *FakeAccountRepository
	ledgerEntryRepo               *FakeLedgerEntryRepository
	productTransactionRepo        *FakeProductTransactionRepository
	settlementBatchRepo           *FakeSettlementBatchRepository
	settlementItemRepo            *FakeSettlementItemRepository
	journalRepo                   *FakeJournalRepository
	reconciliationDiscrepancyRepo *FakeReconciliationDiscrepancyRepository
	disbursementRepo              *FakeDisbursementRepository
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
		journalRepo:                   NewFakeJournalRepository(),
		reconciliationDiscrepancyRepo: NewFakeReconciliationDiscrepancyRepository(),
		disbursementRepo:              NewFakeDisbursementRepository(),
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

func (f *FakeRepositoryProvider) Journal() domain.JournalRepository {
	return f.journalRepo
}

func (f *FakeRepositoryProvider) ReconciliationDiscrepancy() domain.ReconciliationDiscrepancyRepository {
	return f.reconciliationDiscrepancyRepo
}

func (f *FakeRepositoryProvider) PaymentRequest() domain.PaymentRequestRepository {
	return nil // Not needed for reconciliation tests
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

// ═══════════════════════════════════════════════════════════════════════════
// UNIT TESTS
// ═══════════════════════════════════════════════════════════════════════════

// TestProcessReconciliation_CSVParsing tests CSV parsing in isolation
func TestProcessReconciliation_CSVParsing(t *testing.T) {
	tests := []struct {
		name        string
		csv         string
		expectError bool
		expectedMsg string
	}{
		{
			name: "valid csv with metadata and data",
			csv: `Report Type,Transaction Report
Merchant Name,Test Merchant
Date Report,03-13-2026
Batch ID,BATCH-20260313-001
Total Amount (Purchase),51000
Total Fee,4995
Total Pay to Merchant (NET),46005
Total Transaction,1
Download Date,03-13-2026

NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SAC
1,Test Seller,QRIS,03-12-2026,INV-001,John Doe,ACCEPTED,51000,SUCCESS,4995,0,46005,03-13-2026,Purchase,,SAC-001`,
			expectError: false,
		},
		{
			name: "csv with multiple blank lines",
			csv: `Report Type,Transaction Report
Merchant Name,Test Merchant
Date Report,03-13-2026
Batch ID,BATCH-20260313-002
Total Amount (Purchase),100000
Total Fee,10000
Total Pay to Merchant (NET),90000
Total Transaction,2
Download Date,03-13-2026


NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SAC
1,Test Seller 1,QRIS,03-12-2026,INV-001,John Doe,ACCEPTED,51000,SUCCESS,4995,0,46005,03-13-2026,Purchase,,SAC-001
2,Test Seller 2,QRIS,03-12-2026,INV-002,Jane Doe,ACCEPTED,49000,SUCCESS,5005,0,43995,03-13-2026,Purchase,,SAC-002`,
			expectError: false,
		},
		{
			name: "csv missing batch id",
			csv: `Report Type,Transaction Report
Merchant Name,Test Merchant
Date Report,03-13-2026

NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE
1,Test Seller,QRIS,03-12-2026,INV-001,John Doe,ACCEPTED,51000,SUCCESS,4995,0,46005,03-13-2026,Purchase,`,
			expectError: true,
			expectedMsg: "CSV metadata missing Batch ID",
		},
		{
			name:        "empty csv",
			csv:         ``,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup fake client
			fakes := NewFakeRepositoryProvider()

			// Create platform and payment gateway accounts
			platformAcc := createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
			_ = fakes.Account().Save(context.Background(), platformAcc)

			paymentGatewayAcc := createTestAccount(domain.OwnerTypePaymentGateway, "doku", "DOKU-SAC")
			_ = fakes.Account().Save(context.Background(), paymentGatewayAcc)

			// Create test client with fake repos
			client := &LedgerClient{
				repoProvider: fakes,
				txProvider:   NewFakeTransactionProvider(fakes),
				logger:       testLogger(),
			}

			// Create reconciliation request
			req := &ReconciliationRequest{
				CSVReader:      strings.NewReader(tt.csv),
				ReportFileName: "test.csv",
				UploadedBy:     "admin@test.com",
				SettlementDate: time.Now(),
			}

			// Execute
			resp, err := client.ProcessReconciliation(context.Background(), req)

			// Assert
			if tt.expectError {
				assert.Error(t, err)
				if tt.expectedMsg != "" {
					assert.Contains(t, err.Error(), tt.expectedMsg)
				}
			} else {
				require.NoError(t, err)
				assert.NotNil(t, resp)
			}
		})
	}
}

// settlementCSVWithBatchID builds a minimal but structurally valid DOKU settlement
// CSV carrying the given Batch ID. The metadata block is nine "Label_,Value" rows in
// the order parseMetadata expects, and dates are DD-MM-YYYY to match the parser's
// default layout — ProcessReconciliation constructs its parser with an empty format
// string, which falls back to exactly that.
func settlementCSVWithBatchID(batchID string) string {
	return `Total Amount Purchase_,51000
Total Fee_,4995
Total Purchase_,1
Total Amount Refund_,0
Total Refund_,0
Total Settlement Amount_,46005
Total Discount_,0
Total Transactions_,1
Batch ID_,` + batchID + `

NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SUB ACCOUNT
1,Test Seller,QRIS,05-03-2026,INV-001,John Doe,ACCEPTED,51000,SUCCESS,4995,0,46005,06-03-2026,Purchase,,SAC-001
`
}

// newReconciliationTestClient wires a LedgerClient over in-memory fakes with the
// platform and payment-gateway accounts already created, which is the minimum
// ProcessReconciliation needs before it looks at the CSV at all.
func newReconciliationTestClient() (*LedgerClient, *FakeRepositoryProvider) {
	fakes := NewFakeRepositoryProvider()

	platformAcc := createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
	_ = fakes.Account().Save(context.Background(), platformAcc)

	paymentGatewayAcc := createTestAccount(domain.OwnerTypePaymentGateway, "doku", "DOKU-SAC")
	_ = fakes.Account().Save(context.Background(), paymentGatewayAcc)

	client := &LedgerClient{
		repoProvider: fakes,
		txProvider:   NewFakeTransactionProvider(fakes),
		logger:       testLogger(),
	}

	return client, fakes
}

// TestProcessReconciliation_Idempotency covers the guarantee the settlement pipeline
// rests on: a batch_id is booked at most once, no matter how many times, or under how
// many names, its CSV is presented.
func TestProcessReconciliation_Idempotency(t *testing.T) {
	t.Run("same batch_id returns already ingested and posts nothing", func(t *testing.T) {
		client, fakes := newReconciliationTestClient()

		// A batch already booked under one name...
		existing, err := domain.NewSettlementBatch(
			"platform-account", "2026/03/settlement-original.csv", time.Now(), "System", domain.CurrencyIDR,
		)
		require.NoError(t, err)
		existing.BatchID = "B-DOKU-ALREADY-BOOKED"
		require.NoError(t, fakes.SettlementBatch().Save(context.Background(), existing))

		// ...is presented a second time under a DIFFERENT object key, which is what a
		// re-download or a re-ship looks like. report_file_name cannot catch that;
		// batch_id is the whole reason this check keys on the CSV's own identity.
		resp, err := client.ProcessReconciliation(context.Background(), &ReconciliationRequest{
			CSVReader:      strings.NewReader(settlementCSVWithBatchID("B-DOKU-ALREADY-BOOKED")),
			ReportFileName: "2026/03/settlement-reshipped.csv",
			UploadedBy:     "System",
		})

		// Not an error: for a caller that discovers files by listing a bucket, meeting
		// a file it has already booked is the ordinary case, not a failure.
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.AlreadyIngested)
		assert.Equal(t, "2026/03/settlement-original.csv", resp.IngestedAs,
			"caller needs the original name to tell a benign re-ship from a possible DOKU correction")
		assert.Equal(t, existing.UUID, resp.ReconciliationID)

		// Nothing was posted. A second batch row would mean a second set of immutable
		// ledger entries, which is the failure this whole mechanism exists to prevent.
		assert.Len(t, fakes.settlementBatchRepo.batches, 1)
		assert.Empty(t, resp.BalanceUpdates.Pending.Diff)
		assert.Empty(t, resp.BalanceUpdates.Available.Diff)
	})

	t.Run("unknown batch_id proceeds past the guard", func(t *testing.T) {
		client, fakes := newReconciliationTestClient()

		existing, err := domain.NewSettlementBatch(
			"platform-account", "2026/03/settlement-other.csv", time.Now(), "System", domain.CurrencyIDR,
		)
		require.NoError(t, err)
		existing.BatchID = "B-DOKU-SOMETHING-ELSE"
		require.NoError(t, fakes.SettlementBatch().Save(context.Background(), existing))

		resp, _ := client.ProcessReconciliation(context.Background(), &ReconciliationRequest{
			CSVReader:      strings.NewReader(settlementCSVWithBatchID("B-DOKU-BRAND-NEW")),
			ReportFileName: "2026/03/settlement-new.csv",
			UploadedBy:     "System",
		})

		// The unmatched invoice means this run has no transactions to settle, so it may
		// still end in an error further down. What matters here is only that the guard
		// did not claim it: an unrelated batch_id must never short-circuit a new file.
		if resp != nil {
			assert.False(t, resp.AlreadyIngested)
			assert.Empty(t, resp.IngestedAs)
		}
	})

	t.Run("empty batch_id is rejected before the guard", func(t *testing.T) {
		client, _ := newReconciliationTestClient()

		// A CSV with no Batch ID has no identity, so it can be neither recognised as a
		// repeat nor safely booked. It is refused outright rather than treated as new —
		// otherwise every such file would post again on every tick.
		_, err := client.ProcessReconciliation(context.Background(), &ReconciliationRequest{
			CSVReader:      strings.NewReader(settlementCSVWithBatchID("")),
			ReportFileName: "2026/03/settlement-no-batch-id.csv",
			UploadedBy:     "System",
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "Batch ID")
	})
}

// TestFilterIngestedReportFiles covers the tick's diff: the caller lists object storage
// and needs to know which of those keys the ledger has already booked.
func TestFilterIngestedReportFiles(t *testing.T) {
	client, fakes := newReconciliationTestClient()

	booked, err := domain.NewSettlementBatch(
		"platform-account", "2026/03/settlement-done.csv", time.Now(), "System", domain.CurrencyIDR,
	)
	require.NoError(t, err)
	booked.BatchID = "B-DOKU-DONE"
	require.NoError(t, fakes.SettlementBatch().Save(context.Background(), booked))

	ingested, err := client.FilterIngestedReportFiles(context.Background(), []string{
		"2026/03/settlement-done.csv",
		"2026/03/settlement-pending.csv",
	})

	require.NoError(t, err)
	assert.Len(t, ingested, 1)
	_, done := ingested["2026/03/settlement-done.csv"]
	assert.True(t, done)
	_, pending := ingested["2026/03/settlement-pending.csv"]
	assert.False(t, pending, "an unlisted key is unprocessed work, and must not be filtered out")

	// An empty listing is an ordinary tick on a quiet bucket, not an edge case.
	empty, err := client.FilterIngestedReportFiles(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestProcessReconciliation_TransactionMatching tests invoice matching logic
func TestProcessReconciliation_TransactionMatching(t *testing.T) {
	t.Run("exact invoice match", func(t *testing.T) {
		// Setup
		fakes := NewFakeRepositoryProvider()
		ctx := context.Background()

		// Create accounts
		platformAcc := createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
		_ = fakes.Account().Save(ctx, platformAcc)

		sellerAcc := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
		_ = fakes.Account().Save(ctx, sellerAcc)

		// Create completed product transaction
		fee, _ := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		tx := createTestProductTransaction("INV-001", sellerAcc.UUID, fee)
		_ = fakes.ProductTransaction().Save(ctx, tx)

		// CSV with matching invoice
		csv := `Report Type,Transaction Report
Merchant Name,Test Merchant
Date Report,03-13-2026
Batch ID,BATCH-001
Total Amount (Purchase),51000
Total Fee,4995
Total Pay to Merchant (NET),46005
Total Transaction,1
Download Date,03-13-2026

NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SAC
1,Test Seller,QRIS,03-12-2026,INV-001,John Doe,ACCEPTED,51000,SUCCESS,4995,0,46005,03-13-2026,Purchase,,SAC-001`

		// Create client (this is simplified - in real test you'd need full setup)
		// The key is: fakes.ProductTransaction().GetByInvoiceNumber() will return our tx

		// Parse CSV to verify matching would work
		parser := domain.NewDokuSettlementCSVParser("test.csv", 1)
		err := parser.Parse(strings.NewReader(csv))
		require.NoError(t, err)

		rows := parser.GetRows()
		require.Len(t, rows, 1)
		assert.Equal(t, "INV-001", rows[0].InvoiceNumber)

		// Verify we can find the transaction by invoice
		matchedTx, err := fakes.ProductTransaction().GetByInvoiceNumber(ctx, "INV-001")
		require.NoError(t, err)
		assert.Equal(t, tx.UUID, matchedTx.UUID)
	})

	t.Run("invoice not found", func(t *testing.T) {
		// Setup
		fakes := NewFakeProductTransactionRepository()
		ctx := context.Background()

		// Try to get non-existent invoice
		_, err := fakes.GetByInvoiceNumber(ctx, "INV-NOTFOUND")
		assert.Error(t, err)
		assert.Equal(t, repo.ErrNotFound, err)
	})
}

// TestProcessReconciliation_AmountDiscrepancy tests amount mismatch detection
func TestProcessReconciliation_AmountDiscrepancy(t *testing.T) {
	t.Run("csv amount matches expected", func(t *testing.T) {
		// GATEWAY_ON_SELLER: sellerNetAmount = sellerPrice - dokuFee (seller bears DOKU fee)
		fee, err := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		require.NoError(t, err)

		// Expected: 50000 - 4995 = 45005
		assert.Equal(t, int64(45005), fee.SellerNetAmount)

		// Create settlement item
		item, err := domain.NewSettlementItem(
			"batch-123",
			"INV-001",
			"SAC-001",
			51000,
			46005,
			4995,
			1,
			map[string]string{},
		)
		require.NoError(t, err)

		// Match to transaction
		tx := createTestProductTransaction("INV-001", "seller-123", fee)
		err = item.MatchToTransaction(tx)
		require.NoError(t, err)

		// Should NOT have discrepancy
		assert.False(t, item.HasAmountDiscrepancy())
		assert.Equal(t, int64(0), item.AmountDiscrepancy)
	})

	t.Run("csv amount mismatches expected", func(t *testing.T) {
		// GATEWAY_ON_SELLER: seller gets 46005
		fee, err := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		require.NoError(t, err)

		// Create settlement item with WRONG PayToMerchant amount
		item, err := domain.NewSettlementItem(
			"batch-123",
			"INV-001",
			"SAC-001",
			51000,
			45000, // WRONG! Should be 46005
			4995,
			1,
			map[string]string{},
		)
		require.NoError(t, err)

		tx := createTestProductTransaction("INV-001", "seller-123", fee)
		err = item.MatchToTransaction(tx)
		require.NoError(t, err)

		// Should have discrepancy
		assert.True(t, item.HasAmountDiscrepancy())
		assert.Equal(t, int64(-1005), item.AmountDiscrepancy) // 45000 - 46005
	})
}

// TestProcessReconciliation_SACMismatch tests SubAccount verification
func TestProcessReconciliation_SACMismatch(t *testing.T) {
	t.Run("sac matches", func(t *testing.T) {
		fakes := NewFakeAccountRepository()
		ctx := context.Background()

		// Seller account with DOKU SAC
		sellerAcc := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
		_ = fakes.Save(ctx, sellerAcc)

		// CSV row with matching SAC
		csvSAC := "SAC-001"

		// Verify
		dbAcc, err := fakes.GetBySellerID(ctx, "seller-1")
		require.NoError(t, err)
		assert.Equal(t, csvSAC, dbAcc.DokuSubAccountID)
	})

	t.Run("sac mismatch", func(t *testing.T) {
		fakes := NewFakeAccountRepository()
		ctx := context.Background()

		// Seller account with different SAC
		sellerAcc := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
		_ = fakes.Save(ctx, sellerAcc)

		// CSV row with different SAC (MISMATCH!)
		csvSAC := "SAC-999"

		// Verify mismatch would be detected
		dbAcc, err := fakes.GetBySellerID(ctx, "seller-1")
		require.NoError(t, err)
		assert.NotEqual(t, csvSAC, dbAcc.DokuSubAccountID)
	})
}

// TestProcessReconciliation_BalanceCalculation tests balance updates
func TestProcessReconciliation_BalanceCalculation(t *testing.T) {
	t.Run("balance moves from pending to available", func(t *testing.T) {
		fakes := NewFakeLedgerEntryRepository()
		ctx := context.Background()

		sellerAccountID := "seller-acc-123"

		// Initial: 46005 in PENDING (seller_price + platform_fee)
		pendingEntry := &domain.LedgerEntry{
			JournalUUID:   "journal-1",
			AccountUUID:   sellerAccountID,
			Amount:        46005,
			BalanceBucket: domain.BalanceBucketPending,
			EntryType:     domain.EntryTypeProductPayment,
			SourceType:    domain.SourceTypeProductTransaction,
			SourceID:      "tx-1",
		}
		redifu.InitRecord(pendingEntry)
		_ = fakes.Save(ctx, pendingEntry)

		// Check initial balance
		pending, available, err := fakes.GetAllBalances(ctx, sellerAccountID)
		require.NoError(t, err)
		assert.Equal(t, int64(46005), pending)
		assert.Equal(t, int64(0), available)

		// Settlement: DEBIT pending, CREDIT available
		debitEntry := &domain.LedgerEntry{
			JournalUUID:   "journal-settlement",
			AccountUUID:   sellerAccountID,
			Amount:        -46005,
			BalanceBucket: domain.BalanceBucketPending,
			EntryType:     domain.EntryTypeSettlement,
			SourceType:    domain.SourceTypeSettlementBatch,
			SourceID:      "batch-1",
		}
		redifu.InitRecord(debitEntry)
		creditEntry := &domain.LedgerEntry{
			JournalUUID:   "journal-settlement",
			AccountUUID:   sellerAccountID,
			Amount:        46005,
			BalanceBucket: domain.BalanceBucketAvailable,
			EntryType:     domain.EntryTypeSettlement,
			SourceType:    domain.SourceTypeSettlementBatch,
			SourceID:      "batch-1",
		}
		redifu.InitRecord(creditEntry)
		_ = fakes.SaveBatch(ctx, []*domain.LedgerEntry{debitEntry, creditEntry})

		// Check final balance
		pending, available, err = fakes.GetAllBalances(ctx, sellerAccountID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), pending)
		assert.Equal(t, int64(46005), available)
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// TEST HELPERS
// ═══════════════════════════════════════════════════════════════════════════

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
}

func createTestAccount(ownerType domain.OwnerType, ownerID, dokuSAC string) *domain.Account {
	acc := domain.NewAccount(ownerType, dokuSAC, ownerID, domain.CurrencyIDR)
	return &acc
}

func createTestProductTransaction(invoiceNumber, sellerAccountID string, fee *domain.FeeBreakdown) *domain.ProductTransaction {
	tx := domain.NewProductTransaction(
		"buyer-123",
		sellerAccountID,
		"product-456",
		"PHOTO",
		invoiceNumber,
		*fee, // Dereference pointer
		nil,
	)
	tx.MarkCompleted()
	return tx
}

// setupBasicReconciliationTest creates the minimum required data for reconciliation tests
func setupBasicReconciliationTest(t *testing.T, fakes *FakeRepositoryProvider) (*domain.Account, *domain.Account) {
	ctx := context.Background()

	// Create system accounts
	platformAcc := createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
	err := fakes.Account().Save(ctx, platformAcc)
	require.NoError(t, err)

	paymentGatewayAcc := createTestAccount(domain.OwnerTypePaymentGateway, "doku", "DOKU-SAC")
	err = fakes.Account().Save(ctx, paymentGatewayAcc)
	require.NoError(t, err)

	return platformAcc, paymentGatewayAcc
}

// ═══════════════════════════════════════════════════════════════════════════
// ADDITIONAL TEST CASES
// ═══════════════════════════════════════════════════════════════════════════

// TestFakeRepositories_Integration tests that all fake repositories work together
func TestFakeRepositories_Integration(t *testing.T) {
	t.Run("complete reconciliation scenario", func(t *testing.T) {
		// Setup
		fakes := NewFakeRepositoryProvider()
		ctx := context.Background()

		// 1. Create accounts
		platformAcc, _ := setupBasicReconciliationTest(t, fakes)
		sellerAcc := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
		err := fakes.Account().Save(ctx, sellerAcc)
		require.NoError(t, err)

		// 2. Create completed product transaction
		fee, err := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		require.NoError(t, err)
		tx := createTestProductTransaction("INV-001", sellerAcc.UUID, fee)
		err = fakes.ProductTransaction().Save(ctx, tx)
		require.NoError(t, err)

		// 3. Add pending balance to seller (from initial payment)
		journal := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, map[string]any{"description": "Payment completed"})
		err = fakes.Journal().Save(ctx, journal)
		require.NoError(t, err)

		pendingEntry := &domain.LedgerEntry{
			JournalUUID:   journal.UUID,
			AccountUUID:   sellerAcc.UUID,
			Amount:        46005,
			BalanceBucket: domain.BalanceBucketPending,
			EntryType:     domain.EntryTypeProductPayment,
			SourceType:    domain.SourceTypeProductTransaction,
			SourceID:      tx.UUID,
		}
		redifu.InitRecord(pendingEntry)
		err = fakes.LedgerEntry().Save(ctx, pendingEntry)
		require.NoError(t, err)

		// 4. Create settlement batch
		batch, err := domain.NewSettlementBatch(platformAcc.UUID, "test.csv", time.Now(), "admin@test.com", domain.CurrencyIDR)
		require.NoError(t, err)
		err = fakes.SettlementBatch().Save(ctx, batch)
		require.NoError(t, err)

		// 5. Create settlement item linking CSV to transaction
		// PayToMerchant = totalCharged - dokuFee = 51000 - 4995 = 46005 (seller_price + platform_fee)
		item, err := domain.NewSettlementItem(batch.UUID, "INV-001", "SAC-001", 51000, 46005, 4995, 1, map[string]string{})
		require.NoError(t, err)
		err = item.MatchToTransaction(tx)
		require.NoError(t, err)
		err = fakes.SettlementItem().Save(ctx, item)
		require.NoError(t, err)

		// 6. Process settlement (create ledger entries)
		settlementJournal := domain.NewJournal(domain.EventTypeSettlement, domain.SourceTypeSettlementBatch, batch.UUID, map[string]any{"description": "Settlement completed"})
		err = fakes.Journal().Save(ctx, settlementJournal)
		require.NoError(t, err)

		// DEBIT pending (clear the 46005 that was in pending)
		debitEntry := &domain.LedgerEntry{
			JournalUUID:   settlementJournal.UUID,
			AccountUUID:   sellerAcc.UUID,
			Amount:        -46005,
			BalanceBucket: domain.BalanceBucketPending,
			EntryType:     domain.EntryTypeSettlement,
			SourceType:    domain.SourceTypeSettlementBatch,
			SourceID:      batch.UUID,
		}
		redifu.InitRecord(debitEntry)
		// CREDIT available (move 46005 to available)
		creditEntry := &domain.LedgerEntry{
			JournalUUID:   settlementJournal.UUID,
			AccountUUID:   sellerAcc.UUID,
			Amount:        46005,
			BalanceBucket: domain.BalanceBucketAvailable,
			EntryType:     domain.EntryTypeSettlement,
			SourceType:    domain.SourceTypeSettlementBatch,
			SourceID:      batch.UUID,
		}
		redifu.InitRecord(creditEntry)
		err = fakes.LedgerEntry().SaveBatch(ctx, []*domain.LedgerEntry{debitEntry, creditEntry})
		require.NoError(t, err)

		// 7. Verify final state
		pending, available, err := fakes.LedgerEntry().GetAllBalances(ctx, sellerAcc.UUID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), pending, "Pending should be zero after settlement")
		assert.Equal(t, int64(46005), available, "Available should have settled amount (seller_price + platform_fee)")

		// 8. Verify transaction status
		updatedTx, err := fakes.ProductTransaction().GetByInvoiceNumber(ctx, "INV-001")
		require.NoError(t, err)
		assert.Equal(t, tx.UUID, updatedTx.UUID)

		// 9. Verify settlement item
		retrievedItems, err := fakes.SettlementItem().GetByProductTransactionID(ctx, tx.UUID)
		require.NoError(t, err)
		require.Len(t, retrievedItems, 1)
		assert.False(t, retrievedItems[0].HasAmountDiscrepancy())
	})
}

// TestCSVParsingAlone demonstrates testing CSV parsing without database
func TestCSVParsingAlone(t *testing.T) {
	t.Run("parse valid CSV and extract metadata", func(t *testing.T) {
		// CSV format expected by parser (9 metadata rows in specific order)
		csv := `Total Amount Purchase,102000
Total Fee,9990
Total Purchase,2
Total Amount Refund,0
Total Refund,0
Total Settlement Amount,92010
Total Discount,0
Total Transaction,2
Batch ID,BATCH-20260313-001

NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SAC
1,Test Seller 1,QRIS,03-12-2026,INV-001,John Doe,ACCEPTED,51000,SUCCESS,4995,0,46005,03-13-2026,Purchase,,SAC-001
2,Test Seller 2,QRIS,03-12-2026,INV-002,Jane Smith,ACCEPTED,51000,SUCCESS,4995,0,46005,03-13-2026,Purchase,,SAC-002`

		parser := domain.NewDokuSettlementCSVParser("02-01-2006", 1)
		err := parser.Parse(strings.NewReader(csv))
		require.NoError(t, err)

		// Verify metadata
		metadata := parser.GetMetadata()
		require.NotNil(t, metadata)
		assert.Equal(t, "BATCH-20260313-001", metadata.BatchID)
		assert.Equal(t, int64(102000), metadata.TotalAmountPurchase)
		assert.Equal(t, int64(9990), metadata.TotalFee)
		assert.Equal(t, int64(92010), metadata.TotalSettlement)
		assert.Equal(t, 2, metadata.TotalTransactions)

		// Verify rows
		rows := parser.GetRows()
		require.Len(t, rows, 2)

		// First row
		assert.Equal(t, "INV-001", rows[0].InvoiceNumber)
		assert.Equal(t, int64(51000), rows[0].Amount)
		assert.Equal(t, int64(4995), rows[0].Fee)
		assert.Equal(t, int64(46005), rows[0].PayToMerchant)
		assert.Equal(t, "SAC-001", rows[0].SubAccount)

		// Second row
		assert.Equal(t, "INV-002", rows[1].InvoiceNumber)
		assert.Equal(t, "SAC-002", rows[1].SubAccount)
	})
}

// TestFeeCalculationAlone demonstrates testing fee calculation logic
func TestFeeCalculationAlone(t *testing.T) {
	tests := []struct {
		name                 string
		sellerPrice          int64
		platformFee          int64
		dokuFee              int64
		feeModel             domain.FeeModel
		expectedTotalCharged int64
		expectedSellerNet    int64
	}{
		{
			name:                 "GATEWAY_ON_CUSTOMER - customer pays all",
			sellerPrice:          50000,
			platformFee:          1000,
			dokuFee:              4995,
			feeModel:             domain.FeeModelGatewayOnCustomer,
			expectedTotalCharged: 55995, // 50000 + 1000 + 4995
			expectedSellerNet:    50000, // Seller gets full price
		},
		{
			name:                 "GATEWAY_ON_SELLER - seller absorbs gateway fee",
			sellerPrice:          50000,
			platformFee:          1000,
			dokuFee:              4995,
			feeModel:             domain.FeeModelGatewayOnSeller,
			expectedTotalCharged: 51000, // 50000 + 1000 (no gateway fee shown to customer)
			expectedSellerNet:    45005, // 50000 - 4995 (seller bears DOKU fee, platform tracked separately)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fee, err := domain.NewFeeBreakdown(tt.sellerPrice, tt.platformFee, tt.dokuFee, domain.CurrencyIDR, tt.feeModel)
			require.NoError(t, err)

			assert.Equal(t, tt.expectedTotalCharged, fee.TotalCharged)
			assert.Equal(t, tt.expectedSellerNet, fee.SellerNetAmount)
			assert.Equal(t, tt.platformFee, fee.PlatformFee)
			assert.Equal(t, tt.dokuFee, fee.DokuFee)
		})
	}
}

// TestProcessReconciliation_EndToEnd validates complete reconciliation with all fake repositories
func TestProcessReconciliation_EndToEnd(t *testing.T) {
	t.Run("successful reconciliation with 2 transactions", func(t *testing.T) {
		fakes := NewFakeRepositoryProvider()
		ctx := context.Background()

		// Setup accounts
		platform := createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
		_ = fakes.Account().Save(ctx, platform)
		paymentGateway := createTestAccount(domain.OwnerTypePaymentGateway, "doku", "DOKU-SAC")
		_ = fakes.Account().Save(ctx, paymentGateway)
		seller1 := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
		_ = fakes.Account().Save(ctx, seller1)
		seller2 := createTestAccount(domain.OwnerTypeSeller, "seller-2", "SAC-002")
		_ = fakes.Account().Save(ctx, seller2)

		// Create transactions
		fee1, _ := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		tx1 := createTestProductTransaction("INV-001", seller1.UUID, fee1)
		_ = fakes.ProductTransaction().Save(ctx, tx1)
		fee2, _ := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		tx2 := createTestProductTransaction("INV-002", seller2.UUID, fee2)
		_ = fakes.ProductTransaction().Save(ctx, tx2)

		// Add pending balances — GATEWAY_ON_SELLER: Seller=45005 (SellerPrice-DokuFee), Platform=1000 each
		j1 := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx1.UUID, map[string]any{"desc": "p1"})
		_ = fakes.Journal().Save(ctx, j1)
		p1 := &domain.LedgerEntry{JournalUUID: j1.UUID, AccountUUID: seller1.UUID, Amount: 45005, BalanceBucket: domain.BalanceBucketPending, EntryType: domain.EntryTypeProductPayment, SourceType: domain.SourceTypeProductTransaction, SourceID: tx1.UUID}
		redifu.InitRecord(p1)
		_ = fakes.LedgerEntry().Save(ctx, p1)
		pp1 := &domain.LedgerEntry{JournalUUID: j1.UUID, AccountUUID: platform.UUID, Amount: 1000, BalanceBucket: domain.BalanceBucketPending, EntryType: domain.EntryTypePlatformCommission, SourceType: domain.SourceTypeProductTransaction, SourceID: tx1.UUID}
		redifu.InitRecord(pp1)
		_ = fakes.LedgerEntry().Save(ctx, pp1)
		j2 := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx2.UUID, map[string]any{"desc": "p2"})
		_ = fakes.Journal().Save(ctx, j2)
		p2 := &domain.LedgerEntry{JournalUUID: j2.UUID, AccountUUID: seller2.UUID, Amount: 45005, BalanceBucket: domain.BalanceBucketPending, EntryType: domain.EntryTypeProductPayment, SourceType: domain.SourceTypeProductTransaction, SourceID: tx2.UUID}
		redifu.InitRecord(p2)
		_ = fakes.LedgerEntry().Save(ctx, p2)
		pp2 := &domain.LedgerEntry{JournalUUID: j2.UUID, AccountUUID: platform.UUID, Amount: 1000, BalanceBucket: domain.BalanceBucketPending, EntryType: domain.EntryTypePlatformCommission, SourceType: domain.SourceTypeProductTransaction, SourceID: tx2.UUID}
		redifu.InitRecord(pp2)
		_ = fakes.LedgerEntry().Save(ctx, pp2)

		// CSV with proper format
		csv := "Total Amount Purchase,102000\nTotal Fee,9990\nTotal Purchase,2\nTotal Amount Refund,0\nTotal Refund,0\nTotal Settlement Amount,92010\nTotal Discount,0\nTotal Transaction,2\nBatch ID,BATCH-001\n\nNO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SAC\n1,Seller 1,QRIS,12-03-2026,INV-001,John,ACCEPTED,51000,SUCCESS,4995,0,46005,13-03-2026,Purchase,,SAC-001\n2,Seller 2,QRIS,12-03-2026,INV-002,Jane,ACCEPTED,51000,SUCCESS,4995,0,46005,13-03-2026,Purchase,,SAC-002"

		// Execute ProcessReconciliation
		client := &LedgerClient{
			repoProvider: fakes,
			txProvider:   NewFakeTransactionProvider(fakes),
			logger:       testLogger(),
		}
		req := &ReconciliationRequest{CSVReader: strings.NewReader(csv), ReportFileName: "test.csv", UploadedBy: "admin", SettlementDate: time.Now()}
		resp, err := client.ProcessReconciliation(ctx, req)

		// Verify response
		require.NoError(t, err)
		assert.Equal(t, 2, resp.Transactions.Total)
		assert.Equal(t, 2, resp.Transactions.Matched)

		// Verify fake repositories have expected data
		assert.Len(t, fakes.SettlementBatch().(*FakeSettlementBatchRepository).batches, 1, "Should create 1 settlement batch")
		assert.Len(t, fakes.SettlementItem().(*FakeSettlementItemRepository).items, 2, "Should create 2 settlement items")

		// Verify balances moved from pending to available
		s1Pend, s1Avail, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller1.UUID)
		assert.Equal(t, int64(0), s1Pend, "Seller 1 pending should be 0")
		assert.Equal(t, int64(45005), s1Avail, "Seller 1 available should be 45005 (SellerPrice - DokuFee)")
		s2Pend, s2Avail, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller2.UUID)
		assert.Equal(t, int64(0), s2Pend, "Seller 2 pending should be 0")
		assert.Equal(t, int64(45005), s2Avail, "Seller 2 available should be 45005 (SellerPrice - DokuFee)")
		platPend, platAvail, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(0), platPend, "Platform pending should be 0")
		assert.Equal(t, int64(2000), platAvail, "Platform available should be 2000 (2 x PlatformFee)")

		t.Log("✅ End-to-end reconciliation test passed!")
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// FEE MISMATCH RECONCILIATION TESTS
// ═══════════════════════════════════════════════════════════════════════════

// setupFeeMismatchTest creates accounts, a completed product transaction, and the
// corresponding PENDING ledger entries (simulating Phase 2 / payment webhook).
// Returns the LedgerClient and the repositories for balance assertions.
func setupFeeMismatchTest(
	t *testing.T,
	fee *domain.FeeBreakdown,
	sellerPendingAmount, platformPendingAmount, dokuPendingAmount int64,
	sellerSAC string,
) (client *LedgerClient, fakes *FakeRepositoryProvider, seller, platform, doku *domain.Account, tx *domain.ProductTransaction) {
	t.Helper()
	fakes = NewFakeRepositoryProvider()
	ctx := context.Background()

	platform = createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
	_ = fakes.Account().Save(ctx, platform)
	doku = createTestAccount(domain.OwnerTypePaymentGateway, "doku", "DOKU-SAC")
	_ = fakes.Account().Save(ctx, doku)
	seller = createTestAccount(domain.OwnerTypeSeller, "seller-1", sellerSAC)
	_ = fakes.Account().Save(ctx, seller)

	tx = createTestProductTransaction("INV-FEE-TEST", seller.UUID, fee)
	_ = fakes.ProductTransaction().Save(ctx, tx)

	// Simulate Phase 2 PENDING entries from payment webhook
	j := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	_ = fakes.Journal().Save(ctx, j)

	addPending := func(accountID string, amount int64, entryType domain.EntryType) {
		e := &domain.LedgerEntry{
			JournalUUID:   j.UUID,
			AccountUUID:   accountID,
			Amount:        amount,
			BalanceBucket: domain.BalanceBucketPending,
			EntryType:     entryType,
			SourceType:    domain.SourceTypeProductTransaction,
			SourceID:      tx.UUID,
		}
		redifu.InitRecord(e)
		_ = fakes.LedgerEntry().Save(ctx, e)
	}
	if sellerPendingAmount > 0 {
		addPending(seller.UUID, sellerPendingAmount, domain.EntryTypeProductPayment)
	}
	if platformPendingAmount > 0 {
		addPending(platform.UUID, platformPendingAmount, domain.EntryTypePlatformCommission)
	}
	if dokuPendingAmount > 0 {
		addPending(doku.UUID, dokuPendingAmount, domain.EntryTypeProcessorFee)
	}

	client = &LedgerClient{
		repoProvider: fakes,
		txProvider:   NewFakeTransactionProvider(fakes),
		logger:       testLogger(),
	}
	return
}

// buildFeeMismatchCSV builds a minimal single-row DOKU settlement CSV.
func buildFeeMismatchCSV(invoiceNumber string, totalAmount, fee, payToMerchant int64, sac string) string {
	return fmt.Sprintf(
		"Total Amount Purchase,%d\nTotal Fee,%d\nTotal Purchase,1\nTotal Amount Refund,0\nTotal Refund,0\nTotal Settlement Amount,%d\nTotal Discount,0\nTotal Transaction,1\nBatch ID,BATCH-FEE-TEST\n\n"+
			"NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SAC\n"+
			"1,Test Seller,QRIS,12-03-2026,%s,Customer,ACCEPTED,%d,SUCCESS,%d,0,%d,13-03-2026,Purchase,,%s",
		totalAmount, fee, payToMerchant-fee,
		invoiceNumber, totalAmount, fee, payToMerchant, sac,
	)
}

// runFeeMismatchReconciliation executes ProcessReconciliation with the given CSV string.
func runFeeMismatchReconciliation(t *testing.T, client *LedgerClient, csv string) *ReconciliationResponse {
	t.Helper()
	req := &ReconciliationRequest{
		CSVReader:      strings.NewReader(csv),
		ReportFileName: "fee-mismatch-test.csv",
		UploadedBy:     "test-admin",
		SettlementDate: time.Now(),
	}
	resp, err := client.ProcessReconciliation(context.Background(), req)
	require.NoError(t, err)
	return resp
}

// hasDiscrepancyOfType checks whether the response contains at least one discrepancy of the given type.
func hasDiscrepancyOfType(resp *ReconciliationResponse, discrepancyType string) bool {
	for _, d := range resp.Discrepancies {
		if d.Type == discrepancyType {
			return true
		}
	}
	return false
}

// TestProcessReconciliation_RealCSV runs reconciliation against a real DOKU settlement CSV
// with 4 transactions across 3 sellers.
//
// CSV rows (fixed across all subtests):
//
//	1  INV-20260502142631-EZUULO  QRIS    Amount=103272 ActualFee=2272 PTM=101000 SAC-5729
//	2  INV-20260503071202-AMV53W  QRIS    Amount=39000  ActualFee=858  PTM=38142  SAC-6797
//	3  INV-20260502030023-6QCSJM  VA BCA  Amount=705994 ActualFee=4995 PTM=700999 SAC-6004
//	4  INV-20260503073825-P4Q2VM  VA BCA  Amount=39000  ActualFee=4995 PTM=34005  SAC-6797
//
// Fee model per seller:
//   - SAC-5729 (row 1) : GATEWAY_ON_CUSTOMER, PlatformFee=1000
//   - SAC-6797 (rows 2,4): GATEWAY_ON_SELLER,  PlatformFee=0 (subscription — no platform fee)
//   - SAC-6004 (row 3) : GATEWAY_ON_CUSTOMER, PlatformFee=1000
func TestProcessReconciliation_RealCSV(t *testing.T) {
	const realCSV = `Total Amount Purchase_,887266
Total Fee_,13120
Total Purchase_,4
Total Amount Refund_,0
Total Refund_,0
Total Settlement Amount_,874146
Total Discount_,0
Total Transactions_,4
Batch ID_,B-BSN-0203-1761932477260-SBS-8298-20251109155312120-20260502101505879

NO,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE,SUB ACCOUNT
1,Bernino Falya,QRIS,02-05-2026,INV-20260502142631-EZUULO,Bank BCA,INV-20260502142631-EZUULO,103272,TW2026050236,2272,0,101000,04-05-2026,Purchase,,SAC-5729-1777731990867
2,Bernino Falya,QRIS,03-05-2026,INV-20260503071202-AMV53W,Bank BCA,INV-20260503071202-AMV53W,39000,TW2026050389,858,0,38142,04-05-2026,Purchase,,SAC-6797-1767941771817
3,Bernino Falya,Virtual Account BCA,02-05-2026,INV-20260502030023-6QCSJM,Est rerum reiciendis,1900800000264326,705994,1900800000264326,4995,0,700999,04-05-2026,Purchase,,SAC-6004-1772309804461
4,Bernino Falya,Virtual Account BCA,03-05-2026,INV-20260503073825-P4Q2VM,Fahar Ahnaf Azis,1900800000264481,39000,1900800000264481,4995,0,34005,04-05-2026,Purchase,,SAC-6797-1767941771817`

	type txSpec struct {
		invoice     string
		sellerPrice int64
		platformFee int64
		dokuFee     int64 // ExpectedDokuFee recorded at payment time
		feeModel    domain.FeeModel
		sac         string
	}

	// setupFakes builds fresh fakes for each subtest using the provided specs.
	setupFakes := func(t *testing.T, specs []txSpec) (
		fakes *FakeRepositoryProvider,
		sacToSeller map[string]*domain.Account,
		platform, doku *domain.Account,
	) {
		t.Helper()
		fakes = NewFakeRepositoryProvider()
		ctx := context.Background()

		platform = createTestAccount(domain.OwnerTypePlatform, "platform", "PLATFORM-SAC")
		_ = fakes.Account().Save(ctx, platform)
		doku = createTestAccount(domain.OwnerTypePaymentGateway, "doku", "DOKU-SAC")
		_ = fakes.Account().Save(ctx, doku)

		sacToSeller = make(map[string]*domain.Account)
		for _, s := range specs {
			if _, exists := sacToSeller[s.sac]; !exists {
				seller := createTestAccount(domain.OwnerTypeSeller, "owner-"+s.sac, s.sac)
				_ = fakes.Account().Save(ctx, seller)
				sacToSeller[s.sac] = seller
			}
		}

		for _, s := range specs {
			seller := sacToSeller[s.sac]
			fee, err := domain.NewFeeBreakdown(s.sellerPrice, s.platformFee, s.dokuFee, domain.CurrencyIDR, s.feeModel)
			require.NoError(t, err)

			tx := createTestProductTransaction(s.invoice, seller.UUID, fee)
			_ = fakes.ProductTransaction().Save(ctx, tx)

			j := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
			_ = fakes.Journal().Save(ctx, j)

			add := func(accountID string, amount int64, entryType domain.EntryType) {
				e := &domain.LedgerEntry{
					JournalUUID: j.UUID, AccountUUID: accountID, Amount: amount,
					BalanceBucket: domain.BalanceBucketPending, EntryType: entryType,
					SourceType: domain.SourceTypeProductTransaction, SourceID: tx.UUID,
				}
				redifu.InitRecord(e)
				_ = fakes.LedgerEntry().Save(ctx, e)
			}
			// fee.SellerNetAmount is correct for both models:
			//   GATEWAY_ON_CUSTOMER: SellerNetAmount = SellerPrice
			//   GATEWAY_ON_SELLER:   SellerNetAmount = SellerPrice - ExpectedDokuFee
			add(seller.UUID, fee.SellerNetAmount, domain.EntryTypeProductPayment)
			if s.platformFee > 0 {
				add(platform.UUID, s.platformFee, domain.EntryTypePlatformCommission)
			}
			add(doku.UUID, s.dokuFee, domain.EntryTypeProcessorFee)
		}
		return
	}

	runRecon := func(t *testing.T, fakes *FakeRepositoryProvider) *ReconciliationResponse {
		t.Helper()
		client := &LedgerClient{
			repoProvider: fakes,
			txProvider:   NewFakeTransactionProvider(fakes),
			logger:       testLogger(),
		}
		resp, err := client.ProcessReconciliation(context.Background(), &ReconciliationRequest{
			CSVReader:      strings.NewReader(realCSV),
			ReportFileName: "settlement-20260504.csv",
			UploadedBy:     "admin",
			SettlementDate: time.Now(),
		})
		require.NoError(t, err)
		return resp
	}

	logResults := func(t *testing.T, resp *ReconciliationResponse, fakes *FakeRepositoryProvider, sacToSeller map[string]*domain.Account, platform, doku *domain.Account) {
		t.Helper()
		ctx := context.Background()
		t.Logf("Matched=%d Unmatched=%d", resp.Transactions.Matched, resp.Transactions.Unmatched)
		for _, d := range resp.Discrepancies {
			t.Logf("  Discrepancy [%s] %s amount=%d", d.Type, d.InvoiceNumber, d.Amount)
		}
		for sac, seller := range sacToSeller {
			p, a, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
			t.Logf("  Seller %s  PENDING=%d AVAILABLE=%d", sac, p, a)
		}
		platP, platA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		dokuP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		t.Logf("  Platform PENDING=%d AVAILABLE=%d", platP, platA)
		t.Logf("  DOKU     PENDING=%d", dokuP)
	}

	// Baseline rows that don't vary across subtests (row 3).
	// Rows 2 and 4 (SAC-6797) vary per subtest — defined inside each t.Run.
	row1NoMismatch := txSpec{"INV-20260502142631-EZUULO", 100000, 1000, 2272, domain.FeeModelGatewayOnCustomer, "SAC-5729-1777731990867"}
	row3NoMismatch := txSpec{"INV-20260502030023-6QCSJM", 699999, 1000, 4995, domain.FeeModelGatewayOnCustomer, "SAC-6004-1772309804461"}

	// SAC-6797 rows (subscription, GATEWAY_ON_SELLER, no platform fee):
	//   SellerPrice = Amount (39000), PlatformFee = 0
	//   GATEWAY_ON_SELLER: SellerNetAmount = SellerPrice - ExpectedDokuFee
	//   TotalCharged = SellerPrice + PlatformFee = 39000 (what customer pays; DOKU fee NOT added to customer)
	//   ExpectedNetAmount = SellerNetAmount + 0 = SellerNetAmount
	//
	//   No-mismatch (ExpectedDokuFee == ActualDokuFee from CSV):
	//     Row 2: ExpectedDokuFee=858,  SellerNetAmount=38142, ExpectedNetAmount=38142=PTM ✓
	//     Row 4: ExpectedDokuFee=4995, SellerNetAmount=34005, ExpectedNetAmount=34005=PTM ✓
	row2NoMismatch := txSpec{"INV-20260503071202-AMV53W", 39000, 0, 858, domain.FeeModelGatewayOnSeller, "SAC-6797-1767941771817"}
	row4NoMismatch := txSpec{"INV-20260503073825-P4Q2VM", 39000, 0, 4995, domain.FeeModelGatewayOnSeller, "SAC-6797-1767941771817"}

	t.Run("no fee mismatch — all 4 rows exact match", func(t *testing.T) {
		// All ExpectedDokuFee == ActualDokuFee → feeDelta=0, no adjustments
		//
		// SAC-5729: SellerNet=100000 (GATEWAY_ON_CUSTOMER)
		// SAC-6797: SellerNet=38142+34005=72147 (GATEWAY_ON_SELLER, no platform fee)
		// SAC-6004: SellerNet=699999 (GATEWAY_ON_CUSTOMER)
		// Platform: rows 1+3 only → 2×1000=2000
		specs := []txSpec{row1NoMismatch, row2NoMismatch, row3NoMismatch, row4NoMismatch}

		fakes, sacToSeller, platform, doku := setupFakes(t, specs)
		resp := runRecon(t, fakes)
		logResults(t, resp, fakes, sacToSeller, platform, doku)

		ctx := context.Background()
		assert.Equal(t, 4, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.Empty(t, resp.Discrepancies)

		_, a5729, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-5729-1777731990867"].UUID)
		assert.Equal(t, int64(100000), a5729, "SAC-5729 AVAILABLE")
		_, a6797, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-6797-1767941771817"].UUID)
		assert.Equal(t, int64(72147), a6797, "SAC-6797 AVAILABLE: 38142+34005")
		_, a6004, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-6004-1772309804461"].UUID)
		assert.Equal(t, int64(699999), a6004, "SAC-6004 AVAILABLE")
		_, platA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(2000), platA, "Platform AVAILABLE: rows 1+3 only (2×1000)")
		dokuP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dokuP)
	})

	t.Run("row 1 feeDelta > 0 (SAC-5729 GATEWAY_ON_CUSTOMER) — platform absorbs delta", func(t *testing.T) {
		// Row 1: ExpectedDokuFee=2000, ActualDokuFee=2272 → feeDelta=+272
		// SellerPrice = 103272 - 1000 - 2000 = 100272
		// adjustedPlatformFee = 1000 - 272 = 728
		//
		// Phase 2 PENDING: Seller=100272, Platform=1000, DOKU=2000
		// Phase 3 entries:
		//   Seller  -100272 PENDING  SETTLEMENT_CLEAR
		//   Seller  +100272 AVAILABLE SETTLEMENT_NET
		//   Platform  -728  PENDING  SETTLEMENT_CLEAR
		//   Platform  +728  AVAILABLE SETTLEMENT_NET
		//   Platform  -272  PENDING  FEE_ADJUSTMENT (write-off)
		//   DOKU    -2000   PENDING  SETTLEMENT
		row1 := txSpec{"INV-20260502142631-EZUULO", 100272, 1000, 2000, domain.FeeModelGatewayOnCustomer, "SAC-5729-1777731990867"}
		specs := []txSpec{row1, row2NoMismatch, row3NoMismatch, row4NoMismatch}

		fakes, sacToSeller, platform, doku := setupFakes(t, specs)
		resp := runRecon(t, fakes)
		logResults(t, resp, fakes, sacToSeller, platform, doku)

		ctx := context.Background()
		assert.Equal(t, 4, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		_, a5729, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-5729-1777731990867"].UUID)
		assert.Equal(t, int64(100272), a5729, "SAC-5729: seller net unchanged (customer bore extra fee)")
		_, platA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		// Row 1: 728 (adjusted) + row 3: 1000 = 1728
		assert.Equal(t, int64(1728), platA, "Platform: 728 (row1 adjusted) + 1000 (row3)")
		dokuP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dokuP)
	})

	t.Run("row 1 feeDelta < 0 (SAC-5729 GATEWAY_ON_CUSTOMER) — surplus credited to seller", func(t *testing.T) {
		// Row 1: ExpectedDokuFee=2500, ActualDokuFee=2272 → feeDelta=-228
		// SellerPrice = 103272 - 1000 - 2500 = 99772
		// Seller surplus = 228 → AVAILABLE = 99772+228 = 100000
		//
		// Phase 2 PENDING: Seller=99772, Platform=1000, DOKU=2500
		// Phase 3 entries:
		//   Seller  -99772 PENDING  SETTLEMENT_CLEAR
		//   Seller  +99772 AVAILABLE SETTLEMENT_NET
		//   Seller  +228   AVAILABLE FEE_ADJUSTMENT (direct credit)
		//   Platform -1000 PENDING  SETTLEMENT_CLEAR
		//   Platform +1000 AVAILABLE SETTLEMENT_NET
		//   DOKU    -2500  PENDING  SETTLEMENT (always ExpectedDokuFee)
		row1 := txSpec{"INV-20260502142631-EZUULO", 99772, 1000, 2500, domain.FeeModelGatewayOnCustomer, "SAC-5729-1777731990867"}
		specs := []txSpec{row1, row2NoMismatch, row3NoMismatch, row4NoMismatch}

		fakes, sacToSeller, platform, doku := setupFakes(t, specs)
		resp := runRecon(t, fakes)
		logResults(t, resp, fakes, sacToSeller, platform, doku)

		ctx := context.Background()
		assert.Equal(t, 4, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		_, a5729, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-5729-1777731990867"].UUID)
		assert.Equal(t, int64(100000), a5729, "SAC-5729: 99772 + 228 surplus = 100000")
		_, platA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(2000), platA, "Platform: rows 1+3 unchanged (2×1000)")
		dokuP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dokuP)
	})

	t.Run("rows 2+4 feeDelta > 0 (SAC-6797 GATEWAY_ON_SELLER) — seller absorbs delta", func(t *testing.T) {
		// Row 2: ExpectedDokuFee=600,  ActualDokuFee=858  → feeDelta=+258
		//   SellerPrice=39000, SellerNetAmount=38400, adjustedSellerNet=38142
		//   Phase 2 PENDING: Seller=38400, DOKU=600
		//   Phase 3: Seller CLEAR -38142, Seller NET +38142, Seller FEE_ADJ -258 PENDING
		//
		// Row 4: ExpectedDokuFee=4000, ActualDokuFee=4995 → feeDelta=+995
		//   SellerPrice=39000, SellerNetAmount=35000, adjustedSellerNet=34005
		//   Phase 2 PENDING: Seller=35000, DOKU=4000
		//   Phase 3: Seller CLEAR -34005, Seller NET +34005, Seller FEE_ADJ -995 PENDING
		//
		// SAC-6797 AVAILABLE = 38142+34005 = 72147 (same as no-mismatch — seller price fixed)
		row2 := txSpec{"INV-20260503071202-AMV53W", 39000, 0, 600, domain.FeeModelGatewayOnSeller, "SAC-6797-1767941771817"}
		row4 := txSpec{"INV-20260503073825-P4Q2VM", 39000, 0, 4000, domain.FeeModelGatewayOnSeller, "SAC-6797-1767941771817"}
		specs := []txSpec{row1NoMismatch, row2, row3NoMismatch, row4}

		fakes, sacToSeller, platform, doku := setupFakes(t, specs)
		resp := runRecon(t, fakes)
		logResults(t, resp, fakes, sacToSeller, platform, doku)

		ctx := context.Background()
		assert.Equal(t, 4, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		_, a6797, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-6797-1767941771817"].UUID)
		// Seller always ends up with SellerPrice - ActualDokuFee regardless of expected:
		// Row 2: 39000-858=38142, Row 4: 39000-4995=34005 → total=72147
		assert.Equal(t, int64(72147), a6797, "SAC-6797: 38142+34005 (seller absorbs feeDelta)")
		_, platA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(2000), platA, "Platform: rows 1+3 only (rows 2+4 no platform fee)")
		dokuP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dokuP)
	})

	t.Run("rows 2+4 feeDelta < 0 (SAC-6797 GATEWAY_ON_SELLER) — surplus credited to seller", func(t *testing.T) {
		// Row 2: ExpectedDokuFee=1000, ActualDokuFee=858  → feeDelta=-142
		//   SellerPrice=39000, SellerNetAmount=38000, surplus=142 → AVAILABLE=38142
		//   Phase 2 PENDING: Seller=38000, DOKU=1000
		//   Phase 3: Seller CLEAR -38000, Seller NET +38000, Seller FEE_ADJ +142 AVAILABLE
		//
		// Row 4: ExpectedDokuFee=5500, ActualDokuFee=4995 → feeDelta=-505
		//   SellerPrice=39000, SellerNetAmount=33500, surplus=505 → AVAILABLE=34005
		//   Phase 2 PENDING: Seller=33500, DOKU=5500
		//   Phase 3: Seller CLEAR -33500, Seller NET +33500, Seller FEE_ADJ +505 AVAILABLE
		//
		// SAC-6797 AVAILABLE = 38142+34005 = 72147
		row2 := txSpec{"INV-20260503071202-AMV53W", 39000, 0, 1000, domain.FeeModelGatewayOnSeller, "SAC-6797-1767941771817"}
		row4 := txSpec{"INV-20260503073825-P4Q2VM", 39000, 0, 5500, domain.FeeModelGatewayOnSeller, "SAC-6797-1767941771817"}
		specs := []txSpec{row1NoMismatch, row2, row3NoMismatch, row4}

		fakes, sacToSeller, platform, doku := setupFakes(t, specs)
		resp := runRecon(t, fakes)
		logResults(t, resp, fakes, sacToSeller, platform, doku)

		ctx := context.Background()
		assert.Equal(t, 4, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		_, a6797, _ := fakes.LedgerEntry().GetAllBalances(ctx, sacToSeller["SAC-6797-1767941771817"].UUID)
		// Row 2: 38000 + 142 = 38142, Row 4: 33500 + 505 = 34005 → total=72147
		assert.Equal(t, int64(72147), a6797, "SAC-6797: surplus credited directly to AVAILABLE")
		_, platA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(2000), platA, "Platform: rows 1+3 only")
		dokuP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dokuP)
	})
}

// TestProcessReconciliation_FeeMismatch covers all six fee-mismatch reconciliation scenarios:
//
//	GATEWAY_ON_CUSTOMER: feeDelta > 0 (reconcilable), feeDelta > 0 (BLOCK), feeDelta < 0
//	GATEWAY_ON_SELLER  : feeDelta > 0 (reconcilable), feeDelta > 0 (BLOCK), feeDelta < 0
func TestProcessReconciliation_FeeMismatch(t *testing.T) {

	t.Run("GATEWAY_ON_CUSTOMER feeDelta > 0 reconcilable — platform absorbs delta", func(t *testing.T) {
		// SellerPrice=100000, PlatformFee=5000, ExpectedDokuFee=3000
		// TotalCharged=108000, SellerNetAmount=100000
		// ActualDokuFee=4000 → feeDelta=+1000 → adjustedPlatformFee=4000
		fee, err := domain.NewFeeBreakdown(100000, 5000, 3000, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
		require.NoError(t, err)

		// Phase 2 PENDING: Seller=100000, Platform=5000, DOKU=3000
		client, fakes, seller, platform, doku, _ := setupFeeMismatchTest(t, fee, 100000, 5000, 3000, "SAC-SELLER")

		// CSV: ActualDokuFee=4000, PayToMerchant=108000-4000=104000
		csv := buildFeeMismatchCSV("INV-FEE-TEST", 108000, 4000, 104000, "SAC-SELLER")
		resp := runFeeMismatchReconciliation(t, client, csv)

		ctx := context.Background()

		// Response: 1 matched, no unmatched
		assert.Equal(t, 1, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"), "should record FEE_ADJUSTMENT_APPLIED")

		// Seller: full SellerNetAmount available, PENDING cleared
		sP, sA, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
		assert.Equal(t, int64(0), sP, "seller PENDING should be 0")
		assert.Equal(t, int64(100000), sA, "seller AVAILABLE should be 100000 (unchanged)")

		// Platform: adjustedPlatformFee = 5000 - 1000 = 4000 available; PENDING cleared
		plP, plA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(0), plP, "platform PENDING should be 0")
		assert.Equal(t, int64(4000), plA, "platform AVAILABLE should be 4000 (PlatformFee - feeDelta)")

		// DOKU: PENDING cleared (always by ExpectedDokuFee=3000)
		dkP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dkP, "DOKU PENDING should be 0")
	})

	t.Run("GATEWAY_ON_CUSTOMER feeDelta > 0 BLOCK — feeDelta exceeds PlatformFee", func(t *testing.T) {
		// PlatformFee=500, ExpectedDokuFee=3000
		// ActualDokuFee=4000 → feeDelta=+1000 → adjustedPlatformFee=-500 → BLOCK
		fee, err := domain.NewFeeBreakdown(100000, 500, 3000, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
		require.NoError(t, err)

		client, fakes, seller, platform, _, _ := setupFeeMismatchTest(t, fee, 100000, 500, 3000, "SAC-SELLER")

		// CSV: ActualDokuFee=4000 (feeDelta=+1000 > PlatformFee=500)
		csv := buildFeeMismatchCSV("INV-FEE-TEST", 103500, 4000, 99500, "SAC-SELLER")
		resp := runFeeMismatchReconciliation(t, client, csv)

		ctx := context.Background()

		// Transaction should NOT be settled (unmatched)
		assert.Equal(t, 0, resp.Transactions.Matched)
		assert.Equal(t, 1, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_MISMATCH_IRRECONCILABLE"), "should record FEE_MISMATCH_IRRECONCILABLE")

		// No settlement entries written — balances unchanged from Phase 2
		sP, sA, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
		assert.Equal(t, int64(100000), sP, "seller PENDING should remain (not settled)")
		assert.Equal(t, int64(0), sA)

		plP, plA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(500), plP, "platform PENDING should remain (not settled)")
		assert.Equal(t, int64(0), plA)
	})

	t.Run("GATEWAY_ON_CUSTOMER feeDelta < 0 — surplus credited to seller", func(t *testing.T) {
		// ExpectedDokuFee=3000, ActualDokuFee=2000 → feeDelta=-1000
		// Seller AVAILABLE = SellerNetAmount + surplus = 100000 + 1000 = 101000
		fee, err := domain.NewFeeBreakdown(100000, 5000, 3000, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
		require.NoError(t, err)

		client, fakes, seller, platform, doku, _ := setupFeeMismatchTest(t, fee, 100000, 5000, 3000, "SAC-SELLER")

		// CSV: ActualDokuFee=2000, PayToMerchant=108000-2000=106000
		csv := buildFeeMismatchCSV("INV-FEE-TEST", 108000, 2000, 106000, "SAC-SELLER")
		resp := runFeeMismatchReconciliation(t, client, csv)

		ctx := context.Background()

		assert.Equal(t, 1, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		// Seller: SellerNetAmount (from PENDING) + 1000 surplus = 101000 available
		sP, sA, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
		assert.Equal(t, int64(0), sP)
		assert.Equal(t, int64(101000), sA, "seller AVAILABLE should be 100000 + 1000 surplus")

		// Platform: unchanged — 5000 available
		plP, plA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(0), plP)
		assert.Equal(t, int64(5000), plA, "platform AVAILABLE should be 5000 (unchanged)")

		// DOKU: PENDING cleared by ExpectedDokuFee=3000
		dkP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dkP)
	})

	t.Run("GATEWAY_ON_SELLER feeDelta > 0 reconcilable — seller absorbs delta", func(t *testing.T) {
		// SellerPrice=100000, PlatformFee=5000, ExpectedDokuFee=3000
		// TotalCharged=105000, SellerNetAmount=97000 (=SellerPrice-DokuFee)
		// ActualDokuFee=4000 → feeDelta=+1000 → adjustedSellerNet=96000
		fee, err := domain.NewFeeBreakdown(100000, 5000, 3000, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		require.NoError(t, err)
		require.Equal(t, int64(97000), fee.SellerNetAmount)

		// Phase 2 PENDING: Seller=97000, Platform=5000, DOKU=3000
		client, fakes, seller, platform, doku, _ := setupFeeMismatchTest(t, fee, 97000, 5000, 3000, "SAC-SELLER")

		// CSV: TotalCharged=105000, ActualDokuFee=4000, PayToMerchant=101000
		csv := buildFeeMismatchCSV("INV-FEE-TEST", 105000, 4000, 101000, "SAC-SELLER")
		resp := runFeeMismatchReconciliation(t, client, csv)

		ctx := context.Background()

		assert.Equal(t, 1, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		// Seller: adjustedSellerNet = 97000 - 1000 = 96000 available; PENDING cleared
		sP, sA, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
		assert.Equal(t, int64(0), sP, "seller PENDING should be 0")
		assert.Equal(t, int64(96000), sA, "seller AVAILABLE should be 96000 (SellerNetAmount - feeDelta)")

		// Platform: unchanged — 5000 available
		plP, plA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(0), plP)
		assert.Equal(t, int64(5000), plA, "platform AVAILABLE should be 5000 (unchanged)")

		// DOKU: PENDING cleared by ExpectedDokuFee=3000
		dkP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dkP)
	})

	t.Run("GATEWAY_ON_SELLER feeDelta > 0 BLOCK — feeDelta exceeds SellerNetAmount", func(t *testing.T) {
		// SellerPrice=100000, ExpectedDokuFee=3000, SellerNetAmount=97000
		// ActualDokuFee=101000 → feeDelta=+98000 → adjustedSellerNet=-1000 → BLOCK
		fee, err := domain.NewFeeBreakdown(100000, 5000, 3000, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		require.NoError(t, err)

		client, fakes, seller, platform, _, _ := setupFeeMismatchTest(t, fee, 97000, 5000, 3000, "SAC-SELLER")

		// CSV: ActualDokuFee=101000 (feeDelta=+98000 > SellerNetAmount=97000)
		csv := buildFeeMismatchCSV("INV-FEE-TEST", 105000, 101000, 4000, "SAC-SELLER")
		resp := runFeeMismatchReconciliation(t, client, csv)

		ctx := context.Background()

		assert.Equal(t, 0, resp.Transactions.Matched)
		assert.Equal(t, 1, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_MISMATCH_IRRECONCILABLE"))

		// No settlement entries — balances unchanged from Phase 2
		sP, sA, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
		assert.Equal(t, int64(97000), sP, "seller PENDING should remain (not settled)")
		assert.Equal(t, int64(0), sA)

		plP, plA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(5000), plP, "platform PENDING should remain (not settled)")
		assert.Equal(t, int64(0), plA)
	})

	t.Run("GATEWAY_ON_SELLER feeDelta < 0 — surplus credited to seller", func(t *testing.T) {
		// ExpectedDokuFee=3000, ActualDokuFee=2000 → feeDelta=-1000
		// Seller AVAILABLE = SellerNetAmount + surplus = 97000 + 1000 = 98000
		fee, err := domain.NewFeeBreakdown(100000, 5000, 3000, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
		require.NoError(t, err)

		client, fakes, seller, platform, doku, _ := setupFeeMismatchTest(t, fee, 97000, 5000, 3000, "SAC-SELLER")

		// CSV: TotalCharged=105000, ActualDokuFee=2000, PayToMerchant=103000
		csv := buildFeeMismatchCSV("INV-FEE-TEST", 105000, 2000, 103000, "SAC-SELLER")
		resp := runFeeMismatchReconciliation(t, client, csv)

		ctx := context.Background()

		assert.Equal(t, 1, resp.Transactions.Matched)
		assert.Equal(t, 0, resp.Transactions.Unmatched)
		assert.True(t, hasDiscrepancyOfType(resp, "FEE_ADJUSTMENT_APPLIED"))

		// Seller: 97000 from PENDING + 1000 surplus = 98000 available
		sP, sA, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
		assert.Equal(t, int64(0), sP)
		assert.Equal(t, int64(98000), sA, "seller AVAILABLE should be 97000 + 1000 surplus")

		// Platform: unchanged — 5000 available
		plP, plA, _ := fakes.LedgerEntry().GetAllBalances(ctx, platform.UUID)
		assert.Equal(t, int64(0), plP)
		assert.Equal(t, int64(5000), plA)

		// DOKU: PENDING cleared by ExpectedDokuFee=3000
		dkP, _, _ := fakes.LedgerEntry().GetAllBalances(ctx, doku.UUID)
		assert.Equal(t, int64(0), dkP)
	})
}
