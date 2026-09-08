# Testing Guide for Ledger Package

## Testing Strategy Using Fakes

This guide explains how to test `ProcessReconciliation` and other ledger operations using **fake repositories** (hand-written test doubles) instead of mockery or database tests.

## Key Concepts

### 1. Fake Repositories (`ledger_test.go`)

Hand-written in-memory implementations of repository interfaces:

- `FakeAccountRepository`
- `FakeLedgerEntryRepository`
- `FakeProductTransactionRepository`
- `FakeSettlement BatchRepository`
- `FakeSettlementItemRepository`
- `FakeJournalRepository`
- `FakeReconciliationDiscrepancyRepository`

Each fake stores data in memory (maps/slices) and implements the full domain repository interface.

### 2. Testing Individual Process

es

Instead of testing the entire `ProcessReconciliation` flow (which is coupled to `LedgerClient`), test**individual sub-processes**:

#### A. CSV Parsing (No Database Needed)

```go
func TestCSVParsingAlone(t *testing.T) {
    csv := `Report Type,Transaction Report
Batch ID,BATCH-001
...`

    parser := domain.(removed — Singapay publishes no settlement file)("test.csv", 1)
    err := parser.Parse(strings.NewReader(csv))

    // Assert metadata and rows
}
```

#### B.Fee Calculation (Pure Domain Logic)

```go
func TestFeeCalculationAlone(t *testing.T) {
    fee, err := domain.NewFeeBreakdown(
        50000,  // seller price
        1000,   // platform fee
        4995,   // singapay fee
        domain.CurrencyIDR,
        domain.FeeModelGatewayOnSeller,
    )

    assert.Equal(t, int64(46005), fee.SellerNetAmount)
}
```

#### C. Transaction Matching (Using Fakes)

```go
func TestTransactionMatching(t *testing.T) {
    fakeRepo := NewFakeProductTransactionRepository()

    // Create transaction
    tx := &domain.ProductTransaction{...}
    _ = fakeRepo.Save(ctx, tx)

    // Try to match by invoice
    matched, err := fakeRepo.GetByInvoiceNumber(ctx, "INV-001")

    assert.NoError(t, err)
    assert.Equal(t, tx.UUID, matched.UUID)
}
```

#### D. Amount Discrepancy Detection (Domain Logic)

```go
func TestAmountDiscrepancy(t *testing.T) {
    fee, _ := domain.NewFeeBreakdown(...)

    // Create settlement item
    item, _ := domain.NewSettlementItem(
        "batch-123",
        "INV-001",
        "SAC-001",
        51000,      // amount
        45000,      // payToMerchant (WRONG!)
        4995,       // fee
        1,          // row number
        map[string]string{},
    )

    // Match to transaction
    tx := createTestProductTransaction("INV-001", fee)
    _ = item.MatchToTransaction(tx)

    // Check for discrepancy
    assert.True(t, item.HasAmountDiscrepancy())
    assert.Equal(t, int64(-1005), item.AmountDiscrepancy)
}
```

#### E. SAC Verification (Using Fakes)

```go
func TestSACVerification(t *testing.T) {
    fakeAccounts := NewFakeAccountRepository()

    // Seller with SAC-001
    seller := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
    _ = fakeAccounts.Save(ctx, seller)

    // CSV says SAC-999 (MISMATCH!)
    csvSAC := "SAC-999"

    // Retrieve and compare
    dbAcc, _ := fakeAccounts.GetBySel lerID(ctx, "seller-1")
    assert.NotEqual(t, csvSAC, dbAcc.SingapayAccountID)
}
```

#### F. Balance Movement (Using Fakes)

```go
func TestBalanceMovement(t *testing.T) {
    fakeEntries := NewFakeLedgerEntryRepository()
ctx := context.Background()

    sellerAccountID := "seller-123"

    // Initial: 46005 in PENDING
    pendingEntry := &domain.LedgerEntry{
        AccountUUID:   sellerAccountID,
        Amount:        46005,
        BalanceBucket: domain.BalanceBucketPending,
        // ... other fields
    }
    _ = fakeEntries.Save(ctx, pendingEntry)

    // Check initial balance
    pending, available, _ := fakeEntries.GetAllBalances(ctx, sellerAccountID)
    assert.Equal(t, int64(46005), pending)
    assert.Equal(t, int64(0), available)

    // Settlement: DEBIT pending, CREDIT available
    debitEntry := &domain.LedgerEntry{
        AccountUUID:   sellerAccountID,
        Amount:        -46005,
        BalanceBucket: domain.BalanceBucketPending,
    }
    creditEntry := &domain.LedgerEntry{
        AccountUUID:   sellerAccountID,
        Amount:        46005,
        BalanceBucket: domain.BalanceBucketAvailable,
    }
    _ = fakeEntries.SaveBatch(ctx, []*domain.LedgerEntry{debitEntry, creditEntry})

    // Verify final balance
    pending, available, _ = fakeEntries.GetAllBalances(ctx, sellerAccountID)
    assert.Equal(t, int64(0), pending)
    assert.Equal(t, int64(46005), available)
}
```

### 3. Integration Test (All Fakes Together)

```go
func TestCompleteReconciliationScenario(t *testing.T) {
    fakes := NewFakeRepositoryProvider()
    ctx := context.Background()

    // 1. Setup accounts
    platform, _ := setupBasicReconciliationTest(t, fakes)
    seller := createTestAccount(domain.OwnerTypeSeller, "seller-1", "SAC-001")
    _ = fakes.Account().Save(ctx, seller)

    // 2. Create completed transaction
    fee, _ := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
    tx := createTestProductTransaction("INV-001", seller.UUID, fee)
    _ = fakes.ProductTransaction().Save(ctx, tx)

    // 3. Add initial PENDING balance
    journal := &domain.Journal{/* ... */}
    _ = fakes.Journal().Save(ctx, journal)

    pendingEntry := &domain.LedgerEntry{
        AccountUUID:   seller.UUID,
        Amount:        46005,
        BalanceBucket: domain.BalanceBucketPending,
        JournalUUID:   journal.UUID,
    }
    _ = fakes.LedgerEntry().Save(ctx, pendingEntry)

    // 4. Create settlement batch
    batch, _ := domain.NewSettlementBatch(platform.UUID, "test.csv", time.Now(), "admin")
    _ = fakes.SettlementBatch().Save(ctx, batch)

    // 5. Match settlement item to transaction
    item, _ := domain.NewSettlementItem(batch.UUID, "INV-001", "SAC-001", 51000, 46005, 4995, 1, map[string]string{})
    _ = item.MatchToTransaction(tx)
    _ = fakes.SettlementItem().Save(ctx, item)

    // 6. Process settlement (create ledger entries)
    settlementJournal := &domain.Journal{/* ... */}
    _ = fakes.Journal().Save(ctx, settlementJournal)

    debitEntry := &domain.LedgerEntry{
        AccountUUID:   seller.UUID,
        Amount:        -46005,
        BalanceBucket: domain.BalanceBucketPending,
        JournalUUID:   settlementJournal.UUID,
    }
    creditEntry := &domain.LedgerEntry{
        AccountUUID:   seller.UUID,
        Amount:        46005,
        BalanceBucket: domain.BalanceBucketAvailable,
        JournalUUID:   settlementJournal.UUID,
    }
    _ = fakes.LedgerEntry().SaveBatch(ctx, []*domain.LedgerEntry{debitEntry, creditEntry})

    // 7. Verify final state
    pending, available, _ := fakes.LedgerEntry().GetAllBalances(ctx, seller.UUID)
    assert.Equal(t, int64(0), pending)
    assert.Equal(t, int64(46005), available)

    // 8. Verify settlement item
    retrievedItem, _ := fakes.SettlementItem().GetByProductTransactionID(ctx, tx.UUID)
    assert.False(t, retrievedItem.HasAmountDiscrepancy())
}
```

## Benefits of This Approach

1. **No Database Required**: Tests run entirely in memory
2. **Fast**: No I/O, no network calls
3. **Isolated**: Test each process independently
4. **Debuggable**: Easy to inspect fake repository state
5. **Maintainable**: No mock code generation, just plain Go

## Running Tests

```bash
# Run all CSV parsing tests
go test -v -run "TestCSVParsing"

# Run fee calculation tests
go test -v -run "TestFeeCalculation"

# Run all fake repository tests
go test -v -run "TestFake"

# Run specific test
go test -v -run "TestAmountDiscrepancy"
```

## Current Limitations

The full `ProcessReconciliation` method cannot easily be tested with fakes because:

1. It's tightly coupled to `LedgerClient` struct
2. `LedgerClient.repoProvider` is a concrete type (`repo.RepositoryProvider`), not an interface
3. Need database transaction support (`txProvider`)

**Solution**: Test individual sub-processes as shown above, rather than the entire orchestration.

## Future Improvements

To make `ProcessReconciliation` fully testable:

1. Extract reconciliation logic into a separate service with interface dependencies
2. Create `RepositoryProvider` interface in domain layer
3. Make `LedgerClient` accept interface instead of concrete `repo.RepositoryProvider`

Example:

```go
// domain/repository_provider.go
type RepositoryProvider interface {
    Account() AccountRepository
    LedgerEntry() LedgerEntryRepository
    // ... etc
}

// ledger.go
type LedgerClient struct {
    repoProvider domain.RepositoryProvider  // interface, not concrete type!
    // ...
}
```

Then fakes can be injected directly into `LedgerClient` for full end-to-end testing.
