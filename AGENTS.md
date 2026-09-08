# AGENTS.md - Fotafoto Ledger System

## Project Overview

This is a **Domain-Driven Design (DDD) Ledger System** for the Fotafoto photography marketplace platform. The system manages financial operations including:

- **Wallet balances** (pending and available)
- **Product sales** (photographers selling photos)
- **Disbursements** (withdrawals to bank accounts)
- **Balance reconciliation** with Singapay payment gateway
- **Fee management** (platform fees + Singapay payment gateway fees)
- **Discrepancy detection** with safety gates

## Architecture

### Architecture Layers

This project uses **two coexisting architectural patterns**:

#### 1. New DDD Pattern (Recommended for new features)

- **Domain Layer** (`domain/`): Core business logic, entities (Ledger, ReconciliationLog, ReconciliationDiscrepancy), value objects (Money, Currency, Wallet), and repository interfaces
- **Repository Layer** (`repo/`): PostgreSQL implementations of domain repositories
- **Error Handling**: `ledgererr.AppError`

#### 2. Legacy Pattern (Being phased out)

- **Models Layer** (`models/`): Database models (LedgerWallet, LedgerPayment, LedgerSettlement, etc.)
- **Repositories Layer** (`repositories/`): Data access with `models.ErrorLog` error handling
- **Use Cases Layer** (`usecases/`): Business logic orchestration
- **Note**: New features should prefer the DDD pattern above

### Key Design Patterns

- **Aggregate Roots**: Ledger, ProductTransaction, LedgerTransaction, Disbursement
- **Repository Pattern**: Separate data access from domain logic
- **Anti-Corruption Layer**: Singapay client translates between external API and domain model
- **Manual Reconciliation**: Balance updates via explicit CSV upload by admin
- **CQRS-lite**: Separate aggregates for commands (Ledger) and queries (Transactions)
- **Domain Error Handling**: Uses `ledgererr.AppError` pattern
  - **Creating New Custom Errors** (CRITICAL!):
    - **Always create specific, domain-relevant custom errors** - NEVER use generic errors for domain logic
    - **Error Codes** (`ledgererr/error.go`):
      - `CodeInternal` (500): Internal server errors
      - `CodeNotFound` (404): Resource not found
      - `CodeDatabaseError` (500001): Database operation failed
      - `CodeGatewayAPIError` (500002): Singapay payment gateway error
      - `CodeSubaccountAlreadyExists` (409001): Duplicate subaccount
    - **All domain errors are defined in `ledgererr/error.go`**:
      - **Ledger**: `ErrLedgerNotFound` (404001), `ErrLedgerAlreadyExists` (409002), `ErrReconciliationDiscrepancyFound` (409003)
      - **ProductTransaction**: `ErrProductTransactionNotFound` (404002), `ErrProductTransactionAlreadyExists` (409004), `ErrInvalidTransactionStatus` (400001), `ErrInvalidFeeBreakdown` (400002)
      - **PaymentRequest**: `ErrPaymentRequestNotFound` (404003), `ErrPaymentRequestAlreadyExists` (409005), `ErrInvalidPaymentStatus` (400003), `ErrPaymentExpired` (400004)
      - **Repository** (`repo/error.go`): `ErrNotFound`, `ErrFailedInsertSQL`, `ErrFailedQuerySQL`, etc.
    - **When adding new errors**: Define them in `ledgererr/error.go` with code format `HTTPXXX` (e.g., 404001, 409002)
      - Example: `var ErrLedgerNotFound = NewError(404001, "ledger not found", nil)`
  - **Error Wrapping Strategy** (Critical!):
    - Use `NewError(code, message, origin)` when creating new errors
      - Example: `return ledgererr.NewError(ledgererr.CodeNotFound, "ledger not found", nil)`
    - Use `.WithError(originErr)` to attach origin error to existing AppError
      - Example: `return ledgererr.ErrLedgerNotFound.WithError(err)`
    - Use `errors.As()` to check error types
      - Example: `if ledgererr.IsAppError(err, ledgererr.ErrLedgerNotFound) { ... }`
  - **Error Stack Building**: Errors chain naturally via origin error field
  - **Error Checking**: Use `ledgererr.IsAppError(target, err)` or `ledgererr.IsErrorCode(code, err)`
  - Example: `if ledgererr.IsErrorCode(ledgererr.CodeNotFound, err) { return http.StatusNotFound }`

## Domain Model

### Core Aggregates

#### 1. Ledger

**Location**: `domain/ledger.go`

**Purpose**: Manages user's wallet balances

**Invariants**:

- Balance cannot go negative
- Currency must be consistent
- Expected balance must be tracked for reconciliation gate

**State**:

- `PendingBalance`: Money received but not yet settled by Singapay (actual from Singapay)
- `AvailableBalance`: Money that can be withdrawn (actual from Singapay)
- `ExpectedPendingBalance`: What we calculate based on our transactions
- `ExpectedAvailableBalance`: What we calculate based on our transactions
- `LastSyncedAt`: Timestamp of last Singapay sync

**Key Methods**:

- `AddPendingBalance(amount Money)`: Credit pending balance (increments ONLY expected_pending, not actual)
- `DebitAvailableBalance(amount Money)`: Debit available balance (decrements BOTH expected_available AND actual_available)
- `AddAvailableBalance(amount Money)`: Rollback available balance (increments both, used on Singapay failure)
- `SyncWithSingapay(actualPending, actualAvailable Money)`: Update actual balances from Singapay and reset expected balances
- `GetSafeDisbursableBalance()`: Returns MIN(expected_available, actual_available) for safe withdrawals
- `HasDiscrepancy()`: Check if expected and actual balances differ
- `GetDiscrepancyDetails()`: Returns detailed discrepancy information
- `NeedsSyncWithSingapay()`: Check if sync needed (before 2 PM Jakarta time or >24h stale)
- `GetExpectedDiff()`: Returns difference between expected and actual (for monitoring)

#### 2. ProductTransaction

**Location**: `domain/product_transaction.go`

**Purpose**: Represents product transactions between buyer and seller

**Lifecycle**: `PENDING` → `COMPLETED` / `FAILED` / `REFUNDED`

**Responsibilities**:

- Track fee breakdown (seller price, platform fee, gateway fee)
- Calculate seller payout (full seller price - no deductions!)
- Track payment channel and gateway transaction ID

**Fee Structure** (Critical!):

```
Seller sets price: 10,000 IDR
Platform fee (markup): 1,000 IDR
Base amount: 11,000 IDR (seller + platform)
gateway fee (2.2% QRIS): 247 IDR (reverse calculated)
Total charged to buyer: 11,247 IDR

Photographer gets: 10,000 IDR (full amount!)
Platform gets: 1,000 IDR
Singapay gets: 247 IDR
```

**Gateway fee Reverse Calculation** (Important!):

Singapay charges percentage on the total amount they receive, not the base amount.
To ensure seller+platform receive exact amounts, we reverse calculate:

```
Formula: total_charged = base_amount / (1 - percentage/100)
         gateway_fee = total_charged - base_amount

Example with 2.2% QRIS:
  base_amount = 11,000 IDR
  total_charged = 11,000 / (1 - 0.022) = 11,000 / 0.978 = 11,247 IDR
  gateway_fee = 11,247 - 11,000 = 247 IDR

Verification: Singapay takes 2.2% of 11,247 = 247 IDR → leaves 11,000 ✓
```

#### 3. LedgerTransaction

**Location**: `domain/ledger_transaction.go`

**Purpose**: Audit trail of all financial movements

**Types**:

- `CREDIT`: Money added to ledger
- `DEBIT`: Money removed from ledger
- `SETTLEMENT`: Singapay settled pending funds to available
- `FEE`: Singapay settlement fees
- `ADJUSTMENT`: Reconciliation adjustments

**References**: Links to ProductTransaction or Disbursement via `ReferenceType` + `ReferenceID`

#### 4. Disbursement

**Location**: `domain/disbursement.go`

**Purpose**: Withdrawal request to external bank account

**Lifecycle**: `PENDING` → `PROCESSING` → `COMPLETED` / `FAILED` / `CANCELLED`

**State Machine**:

```
PENDING → PROCESSING → COMPLETED
       ↘ FAILED
       ↘ CANCELLED
```

#### 5. ReconciliationLog

**Location**: `domain/reconciliation_log.go`

**Purpose**: Audit trail of balance syncs with Singapay

**Captures**:

- Previous state (pending/available before sync)
- Current state (pending/available from Singapay)
- Detected changes (diffs)
- Settlement detection (pattern matching)

#### 6. ReconciliationDiscrepancy

**Location**: `domain/reconciliation_discrepancy.go`

**Purpose**: Log detected balance mismatches for investigation

**Policy**: ALL discrepancies are logged and tracked

**Handling Strategy**:

- Discrepancies are logged with detailed information
- Finance team is alerted for review
- Operations are NOT blocked - users can withdraw up to safe balance
- Investigation happens in parallel without blocking user transactions

**Usage**: Created when expected vs actual balance differs at any amount, logged for finance team review

**Discrepancy Types**: PENDING_MISMATCH, AVAILABLE_MISMATCH, BOTH_MISMATCH

- **Money**: Amount (int64 in smallest currency unit) + Currency
- **Currency**: IDR, USD
- **BankAccountInfo**: BankCode, AccountNumber, AccountName
- **FeeBreakdown**: SellerPrice, PlatformFee, GatewayFee, TotalCharged
- **ProductMetadata**: PhotoTitle, PhotoResolution, LicenseType, DownloadURL, PaymentGatewayID

## Critical Business Rules

### 1. Fee Structure (BUYER PAYS ALL FEES)

**Problem**: How to charge fees without deducting from photographer?

**Solution**: Markup model - buyer pays seller price + platform fee + gateway fee

**Example**:

```
Seller price: 10,000 IDR
Platform fee: 1,000 IDR (10% or fixed amount)
Base amount: 11,000 IDR
gateway fee: 247 IDR (2.2% QRIS, reverse calculated) OR 4,500 IDR (flat for VA)

Buyer pays: 11,247 IDR (QRIS) or 15,500 IDR (VA)
Seller receives: 10,000 IDR (100% of their price!)
Platform earns: 1,000 IDR
Singapay earns: 247 IDR (QRIS) or 4,500 IDR (VA)
```

**Note**: Singapay percentage fees use reverse calculation to ensure seller+platform get exact amounts.

**Configuration**: Fees stored in database (`fee_configs` table), can be updated per payment channel

### 2. Balance Reconciliation via Settlement CSV

**Problem**: Need to reconcile our transaction records with Singapay's settlement reports

**Solution**: Admin uploads Singapay settlement via API endpoint, system processes and updates all balances

**Ledger Fields**:

- `pending_balance`: Actual current balance (from settlement report)
- `available_balance`: Actual current balance (from settlement report)
- `expected_pending_balance`: What we calculate based on our transactions
- `expected_available_balance`: What we calculate based on our transactions

**Singapay Settlement Report Format**:

```csv
No,MERCHANT NAME,PAYMENT CHANNEL NAME,TRANSACTION DATE,INVOICE NUMBER,CUSTOMER NAME,REPORT CODE,AMOUNT,RECON CODE,FEE,DISCOUNT,PAY TO MERCHANT,PAY OUT DATE,TRANSACTION TYPE,PROMO CODE
1,Mandiri DW,QRIS,08-10-2024,INV_TEST_042,QRIS Singapay,,90000000,,4500,0,20000,08-10-2024,Purchase,
```

**Key Fields**:

- **INVOICE NUMBER**: Maps to our `product_transactions.id` or external reference
- **PAY TO MERCHANT**: Net amount seller receives (after gateway fees)
- **FEE**: Singapay payment gateway fee
- **PAY OUT DATE**: When funds settled to available balance

**Reconciliation Flow**:

```
1. Admin downloads Singapay settlement portal
   ↓
2. Admin uploads CSV via POST /api/v1/ledger/reconciliation
   ↓
3. System parses CSV and validates format
   ↓
4. Match transactions by INVOICE NUMBER
   ↓
5. Mark matched transactions as SETTLED
   ↓
6. Calculate expected_available_balance:
   ├─ Sum all (seller_price + platform_fee) from SETTLED transactions
   └─ This is what WE think should be available
   ↓
7. Get actual_available_balance from Singapay GetBalance API:
   ├─ Singapay returns: total_charged - gateway_fee
   └─ Which equals: seller_price + platform_fee
   ↓
8. Update ledger with BOTH values:
   ├─ expected_available_balance = our calculation
   ├─ actual_available_balance = Singapay's truth
   └─ last_synced_at = NOW()
   ↓
9. Compare and detect discrepancies:
   ├─ IF expected != actual THEN
   │  ├─ Create ReconciliationDiscrepancy record
   │  └─ Alert finance team for investigation
   └─ ELSE reconciliation successful
   ↓
10. Create ReconciliationLog entry
   ↓
11. Payout platform fees to main SAC (Sub Account Collector)
   ↓
12. Return reconciliation summary to admin
```

**Key Points**:

- **GetBalance is read-only**: No sync logic, just returns current database values
- **Reconciliation is explicit**: Admin-triggered via CSV upload
- **CSV matching via INVOICE NUMBER**: Match product_transactions.invoice_number to CSV INVOICE NUMBER field
- **Dual balance calculation**:
  - `expected_available`: Sum(seller_price + platform_fee) from our SETTLED transactions
  - `actual_available`: Singapay GetBalance API response (equals total_charged - gateway_fee)
  - Both should be the same: seller_price + platform_fee
- **Discrepancy detection**: If expected ≠ actual, create ReconciliationDiscrepancy
- **Safe balance**: MIN(expected, actual) prevents overdrafts even with discrepancies
- **Platform fee payout**: Automatic transfer to main SAC after reconciliation

### 3. Singapay Balance Model

Singapay enforces a two-tier balance system:

- **Pending Balance**: Funds received but not yet settled (typically 1-7 days)
- **Available Balance**: Funds that can be disbursed

Our ledger **mirrors this model** to maintain consistency with Singapay's actual state.

### 4. Settlement Transaction Matching

**Problem**: Singapay settlement lists transactions but we need to match them to our records

**Solution**: Match by INVOICE NUMBER field in CSV to our product_transactions.invoice_number

**Matching Strategy**:

1. **Exact Match**: INVOICE NUMBER from CSV = `product_transactions.invoice_number`
2. **FIFO Fallback**: If no match found, use oldest COMPLETED transaction (last resort)

**Settlement Processing**:

```go
// For each row in CSV:
1. Parse INVOICE NUMBER field
2. Find ProductTransaction by invoice_number
3. Update transaction status: COMPLETED → SETTLED
4. Record PAY TO MERCHANT amount (photographer's net)
5. Record FEE amount (Singapay's cut)
6. Link to settlement_batch_id
7. Update settled_at timestamp
8. Update ledger balances:
   - expected_available_balance = Sum(seller_price + platform_fee WHERE status='SETTLED')
   - actual_available_balance = Singapay.GetBalance().available_balance
   - Note: Both should equal (total_charged - gateway_fee)
   - IF expected != actual THEN create ReconciliationDiscrepancy
9. Update LedgerTransaction status: PENDING → COMPLETED
```

**Edge Cases**:

- **Unmatched CSV entries**: Log as "unknown settlement" for investigation
- **Missing transactions**: Transactions not in CSV remain PENDING
- **Duplicate invoice numbers**: Flag as error, requires manual resolution
- **Amount mismatch**: Log discrepancy but proceed with CSV amount (Singapay is authoritative)

### 5. Payment Flow

```
1. Buyer purchases product
   ↓
2. Calculate fees (seller price + platform fee + gateway fee)
   ↓
3. Charge buyer the TOTAL amount via Singapay
   ↓
4. Create ProductTransaction (PENDING) with invoice_number
   ↓
5. Create payment_request (PENDING)
   ↓
6. Return payment URL to user
   ↓
   [User pays via Singapay]
   ↓
7. money-in webhook received
   ↓
8. Update ProductTransaction status: PENDING → COMPLETED
   ↓
9. Update payment_request status: PENDING → COMPLETED
   ↓
10. Create LedgerTransaction (CREDIT, PENDING) - NO balance update yet
   ↓
11. Store platform_fee in product_transaction for later payout
   ↓
   [Days later: Admin uploads Singapay settlement]
   ↓
12. Reconciliation endpoint processes CSV
    ↓
13. Match transaction by INVOICE NUMBER (invoice_number = Singapay CSV INVOICE NUMBER)
    ↓
14. Update ProductTransaction status: COMPLETED → SETTLED
    ↓
15. Calculate and verify ledger balances:
    ├─ expected_available_balance = Sum(seller_price + platform_fee) from settled txs
    ├─ actual_available_balance = Singapay GetBalance API (total_charged - gateway_fee)
    └─ If expected != actual → Create ReconciliationDiscrepancy
    ↓
16. Update LedgerTransaction status: PENDING → COMPLETED
    ↓
17. Verify with Singapay GetBalance API
    ↓
18. Payout accumulated platform fees to main SAC
```

**Important Notes**:

- **NO balances updated** during payment completion (Step 2)
- **Both expected_available and actual_available updated** during reconciliation (Step 3)
- **Singapay settlement is source of truth** for all balance updates
- **Platform fees accumulate** and payout after reconciliation
- **LedgerTransaction created as PENDING** during payment, marked COMPLETED during reconciliation
- **invoice_number generated immediately** when transaction created (not from Singapay CSV)

### 6. Disbursement Flow (with Safe Balance)

```
1. Photographer requests withdrawal
   ↓
2. Check balance freshness (last_synced_at)
   ↓
3. Calculate safe_balance = MIN(expected_available, actual_available)
   ↓
4. Check: requestedAmount <= safe_balance
   ↓
5. If discrepancy exists: Log asynchronously, alert finance (NON-BLOCKING)
   ↓
6. Debit expected_available (actual_available unchanged)
   ↓
7. Create Disbursement (PENDING) and LedgerTransaction (DEBIT)
   ↓
8. Save ledger and disbursement in transaction
   ↓
9. Call Singapay disbursement API
   ↓
   ├─ SUCCESS → Mark as PROCESSING
   │              Actual_available will update on next reconciliation
   │
   └─ FAILURE → Rollback expected_available
               Mark as FAILED
               User can retry
```

**Key Safety Mechanisms**:

1. **Safe Balance**: MIN(expected, actual) prevents overdrafts
2. **Non-blocking Discrepancies**: Users can withdraw up to safe amount
3. **Automatic Rollback**: On Singapay failure, restore expected balance
4. **Staleness Monitoring**: Warn if balance not reconciled >24h

### 7. Balance Update Strategy

**Critical Design Decision**: Only reconciliation updates actual balances

| Operation                  | Expected Balance                    | Actual Balance         | Rationale                               |
| -------------------------- | ----------------------------------- | ---------------------- | --------------------------------------- |
| **Payment (Product Sale)** | ❌ Leave unchanged                  | ❌ Leave unchanged     | Wait for CSV reconciliation             |
| **Disbursement (Success)** | ✅ Debit immediately                | ❌ Leave unchanged     | Waits for CSV reconciliation to confirm |
| **Disbursement (Failed)**  | ✅ Rollback                         | ❌ Leave unchanged     | Restore expected balance only           |
| **CSV Reconciliation**     | ✅ Sum(seller_price + platform_fee) | ✅ Singapay GetBalance API | Two sources compared for discrepancy    |

**Why NOT Update Actual During Operations?**

- **Single Source of Truth**: Only Singapay settlement updates actual balances
- **Simpler Rollback**: On failure, rollback expected only (actual never changed)
- **Clear Semantics**: actual = "from Singapay CSV", expected = "from our transactions"
- **Reconciliation Resets**: Reconciliation makes expected = actual (fresh start)

**Balance Semantics**:

```
actual_available = "From Singapay GetBalance API" (authoritative, what Singapay says)

Sources that update actual_available:
1. ✅ CSV reconciliation (calls Singapay GetBalance API)
2. ❌ Disbursements do NOT update actual (waits for next reconciliation)
3. ❌ Nothing else touches actual!

expected_available = "What we calculate from our transactions"

Sources that update expected_available:
1. ✅ CSV reconciliation (Sum of [seller_price + platform_fee] from settled transactions)
2. ✅ Disbursements (debit immediately when requested)
3. ✅ Disbursement rollback (credit back on Singapay failure)
4. ❌ NOT updated during payments!

Discrepancy Detection:
- After reconciliation: Compare expected vs actual
- Formula: expected = Sum(seller_price + platform_fee), actual = Singapay API (total_charged - gateway_fee)
- Both should equal: seller_price + platform_fee
- If expected != actual → Create ReconciliationDiscrepancy record
- Alert finance team for investigation
- Safe balance = MIN(expected, actual) prevents overdrafts
```

### 8. Transaction Settlement Tracking (FIFO)

**Problem**: Singapay only provides aggregate balance changes, not per-transaction settlement notifications

**Solution**: FIFO matching algorithm to link transactions to settlements

### 8. Transaction Settlement Tracking (FIFO)

**Problem**: Singapay settlement lists transactions but we need to match them to our records and track settlement history

**Solution**: Match by INVOICE NUMBER field in CSV, with FIFO fallback

**New Entities**:

**SettlementBatch** (`domain/settlement_batch.go`):

- Represents a CSV settlement upload from Singapay
- Contains gross amount, net amount, and gateway fees
- Links to multiple ProductTransactions via SettlementItems
- Tracks upload metadata (filename, uploaded_by, processed_at)

**SettlementItem** (`domain/settlement_item.go`):

- Join entity between SettlementBatch and ProductTransaction
- Contains proportionally allocated gateway fees
- Links transaction to specific CSV row

**Enhanced ProductTransaction Status**:

```
PENDING → SETTLED → COMPLETED
```

**CSV Matching Algorithm**:

```
1. Admin uploads Singapay settlement
2. For each row in CSV:
   a. Parse INVOICE NUMBER field
   b. Try to find ProductTransaction by:
      - Exact match: tx.id = INVOICE NUMBER
      - External ref: tx.external_transaction_id = INVOICE NUMBER
   c. If not found: Use FIFO (oldest PENDING transaction)
3. Update transaction status: PENDING → SETTLED
4. Record PAY TO MERCHANT amount (photographer's net)
5. Record FEE amount (Singapay's cut)
6. Create SettlementItem linking transaction to batch
7. Allocate gateway fees proportionally if needed
8. Mark with settlement_batch_id and settled_at timestamp
```

**Database Schema**:

```sql
-- Settlement batch tracking
CREATE TABLE settlement_batches (
  id UUID PRIMARY KEY,
  ledger_id UUID NOT NULL,
  report_file_name VARCHAR(255) NOT NULL,
  settlement_date DATE NOT NULL,
  gross_amount BIGINT NOT NULL,
  net_amount BIGINT NOT NULL,
  gateway_fee BIGINT NOT NULL,
  uploaded_by VARCHAR(255) NOT NULL,
  uploaded_at TIMESTAMP NOT NULL,
  processed_at TIMESTAMP,
  processing_status VARCHAR(50) NOT NULL, -- PENDING, PROCESSING, COMPLETED, FAILED
  matched_count INT DEFAULT 0,
  unmatched_count INT DEFAULT 0,
  metadata JSONB,
  UNIQUE(ledger_id, settlement_date)
);

-- Settlement item linking
CREATE TABLE settlement_items (
  id UUID PRIMARY KEY,
  settlement_batch_id UUID NOT NULL REFERENCES settlement_batches(id),
  product_transaction_id UUID NOT NULL REFERENCES product_transactions(id),
  gateway_transaction_id VARCHAR(255), -- Singapay's own transaction id
  transaction_amount BIGINT NOT NULL,
  allocated_fee BIGINT NOT NULL,
  matched_strategy VARCHAR(50) NOT NULL, -- EXACT_ID, EXTERNAL_REF, FIFO
  created_at TIMESTAMP NOT NULL,
  UNIQUE(settlement_batch_id, product_transaction_id)
);

-- Add columns to product_transactions
ALTER TABLE product_transactions ADD COLUMN settlement_batch_id UUID REFERENCES settlement_batches(id);
ALTER TABLE product_transactions ADD COLUMN settled_at TIMESTAMP;
ALTER TABLE product_transactions ADD COLUMN gateway_transaction_id VARCHAR(255); -- Singapay transaction id

-- Indexes for fast matching
CREATE INDEX idx_product_tx_gateway_id ON product_transactions(gateway_transaction_id);
CREATE INDEX idx_settlement_batch_date ON settlement_batches(ledger_id, settlement_date);
```

**Benefits**:

- Finance team can answer: "Which transactions were settled on Feb 15?"
- Fee tracking: Know exact gateway fees per transaction
- Audit trail: Complete settlement history with CSV source
- ~95% accuracy with INVOICE NUMBER matching (good enough for business needs)
- Unmatched transactions logged for investigation

### 9. Balance Staleness and Reconciliation Frequency

**Problem**: If reconciliation doesn't happen regularly, balances become stale and discrepancies accumulate

**Solution**: Admin-driven reconciliation schedule with monitoring

#### Recommended Reconciliation Schedule

**1. Daily Reconciliation** (Recommended):

- Admin downloads Singapay settlement portal after 2 PM Jakarta time
- Uploads to `/api/v1/ledger/reconciliation` endpoint
- System processes all settled transactions
- Platform fees paid out to main SAC

**2. Staleness Monitoring**:

```go
// Check when last reconciliation occurred
if time.Since(ledger.LastSyncedAt) > 24*time.Hour {
    // Alert admin: "Balance data is stale, please upload settlement CSV"
}

if time.Since(ledger.LastSyncedAt) > 72*time.Hour {
    // Critical alert: "Balance data severely stale (>3 days)"
}
```

**3. Discrepancy Handling**:

- **Minor discrepancies**: Expected vs actual differs by small amount
  - Users can still withdraw up to `MIN(expected_available, actual_available)`
  - Finance team investigates in parallel
- **Major discrepancies**: Large difference or very stale data
  - Admin uploads reconciliation CSV
  - Both balances reset to CSV values
  - Discrepancies resolved automatically

**4. Safe Disbursement Strategy**:

```go
// Always use safe balance for withdrawals
safe_balance = MIN(expected_available, actual_available)

// This prevents overdrafts even if:
// - Reconciliation is delayed
// - Discrepancies exist
// - Data is stale
```

#### Balance Staleness Lifecycle

```
Day 0: Reconciliation uploaded
       ├─ expected = actual (perfect sync)
       └─ last_synced_at = now

Day 1: Payments and disbursements occur
       ├─ expected balances diverge from actual
       └─ Still acceptable, within normal range

Day 2: No reconciliation uploaded
       ├─ Discrepancy grows
       ├─ GetBalance returns stale actual_balance
       └─ ⚠️ Warning: "Balance data is 2 days old"

Day 3+: Extended staleness
       ├─ 🚨 Critical alert to admin
       ├─ Users can still withdraw (safe balance)
       └─ Admin should upload CSV immediately
```

#### Admin Dashboard Indicators

**Balance Health Status**:

```
✅ FRESH (< 24h): Last reconciliation today
⚠️ STALE (24-72h): Reconciliation needed soon
🚨 CRITICAL (> 72h): Upload settlement CSV immediately
```

**Discrepancy Status**:

```
✅ NO DISCREPANCY: expected = actual (perfect)
⚠️ MINOR DISCREPANCY: |expected - actual| < 5% of balance
🚨 MAJOR DISCREPANCY: |expected - actual| >= 5% of balance
```

#### Benefits of Manual Reconciliation

1. **Explicit Control**: Finance team decides when reconciliation happens
2. **Batch Processing**: Efficient - one CSV per settlement period
3. **Audit Trail**: Clear record of who uploaded what and when
4. **Error Recovery**: Failed uploads can be retried immediately
5. **No Background Jobs**: Simpler architecture, fewer moving parts
6. **Testability**: Easy to test with sample CSV files

## Repository Interfaces

### Domain Layer Contracts

All repositories are **interfaces defined in the domain layer**, implemented in infrastructure/repository layer.

**Pattern**:

```go
// Domain layer defines what it needs
type LedgerRepository interface {
    GetByID(id string) (*Ledger, error)
    GetByAccountID(accountID string) (*Ledger, error)
    Save(ledger *Ledger) error
}

// Infrastructure layer implements
type PostgresLedgerRepository struct {
    db *sql.DB
}
```

This follows **Dependency Inversion Principle** - domain doesn't depend on infrastructure.

## External Dependencies

### Singapay Payment Gateway

**Location**: `internal/infrastructure/payment/singapay.go` (already exists)

**Anti-Corruption Layer**: Translate between Singapay's API model and our domain model

**Key APIs**:

1. **GetBalance**: Fetch current pending and available balance (used to verify after reconciliation)
2. **RequestDisbursement**: Initiate withdrawal to bank account

**Rate Limiting**: Singapay APIs have rate limits. GetBalance only called during reconciliation for verification.

**Failure Handling**:

- If GetBalance fails during reconciliation → Log warning, continue with CSV data (CSV is authoritative)
- If RequestDisbursement fails → mark as FAILED, alert ops team

## Application Services

### LedgerService

**Location**: `internal/service/ledger_service.go` (to be created)

**Responsibilities**:

- Get balance (simple database read)
- Process reconciliation CSV upload
- Match transactions via INVOICE NUMBER
- Update ledger balances from settlement report
- Verify with Singapay GetBalance API
- Create reconciliation logs
- Payout platform fees to main SAC
- Handle discrepancy detection

**Key Methods**:

```go
GetBalance(ctx, accountID) (*BalanceResponse, error)
ProcessReconciliation(ctx, csvFile, uploadedBy) (*ReconciliationSummary, error)
verifyWithSingapay(ctx, ledger) error // private, compares CSV totals with Singapay API
payoutPlatformFees(ctx, amount) error // private, transfer to main SAC
```

### ProductSaleService

**Location**: `internal/service/product_sale_service.go` (to be created)

**Responsibilities**:

- Calculate purchase price with fees
- Process product sales
- Credit seller and platform ledgers
- Update expected balances
- Create audit trail

**Key Methods**:

```go
CalculatePurchasePrice(ctx, sellerPrice, paymentChannel) (*FeeBreakdown, error)
ProcessProductSale(ctx, buyerID, sellerID, productID, sellerPrice, paymentChannel, metadata) (transactionID string, error)
```

### DisbursementService

**Location**: `internal/service/disbursement_service.go` (to be created)

**Responsibilities**:

- Validate withdrawal request
- Debit available balance
- Update expected balance
- Call Singapay API
- Handle failures

**Key Methods**:

```go
RequestDisbursement(ctx, accountID, amount, bankAccount, description) (disbursementID string, error)
GetDisbursementHistory(ctx, accountID, page, pageSize) ([]Disbursement, error)
```

## API Endpoints

### Balance Management

```
GET /api/v1/balance
- Returns: pending_balance, available_balance, expected_pending_balance, expected_available_balance, last_synced_at
- Note: Simple read from database, NO sync logic

POST /api/v1/ledger/reconciliation
- Uploads Singapay settlement and processes reconciliation
- Body: multipart/form-data with CSV file
- Returns: ReconciliationSummary (matched/unmatched counts, balance updates, discrepancies)
- Side effects: Updates all balances, payouts platform fees to main SAC

GET /api/v1/ledger/reconciliation-history?page=1&page_size=20
- Returns: List of ReconciliationLog entries
- Use case: Audit trail for finance team
```

### Disbursement

```
POST /api/v1/disbursement
Body: {
  "amount": 1000000,
  "currency": "IDR",
  "bank_code": "014",
  "account_number": "1234567890",
  "account_name": "John Doe",
  "description": "Withdrawal to bank"
}
Returns: { "disbursement_id": "...", "message": "..." }

GET /api/v1/disbursement/history?page=1&page_size=20
- Returns: List of disbursements
```

### Sales

```
POST /api/v1/sales/purchase
Body: {
  "seller_account_id": "seller-123",
  "photo_id": "photo-456",
  "seller_price": 10000,
  "currency": "IDR",
  "payment_channel": "QRIS",
  "photo_title": "Sunset Beach",
  "photo_resolution": "4K",
  "license_type": "Commercial"
}
Returns: {
  "transaction_id": "...",
  "seller_price": 10000,
  "platform_fee": 1000,
  "gateway_fee": 247,
  "total_charged": 11247,
  "payment_url": "/payment/...",
  "message": "Purchase initiated"
}

GET /api/v1/sales/calculate-price?seller_price=10000&payment_channel=QRIS
- Returns fee breakdown before purchase
- Use case: Show buyer total cost upfront

GET /api/v1/sales/history?page=1&page_size=20
- Returns: List of ProductTransactions for seller
```

### Admin (Discrepancy Resolution)

```
GET /api/v1/admin/discrepancies?status=PENDING&severity=CRITICAL
- Returns: List of pending discrepancies requiring manual review

POST /api/v1/admin/discrepancies/{id}/resolve
Body: {
  "action": "ACCEPT_Singapay" | "ACCEPT_EXPECTED" | "MANUAL_ADJUST",
  "notes": "Investigation notes",
  "manual_pending": 12345, // optional, for MANUAL_ADJUST
  "manual_available": 67890
}
- Resolves discrepancy and updates ledger
```

## Database Schema

### Key Tables

```sql
-- Core ledger table
ledgers (
  id, account_id, singapay_account_id,
  pending_balance, available_balance, currency,
  last_synced_at, created_at, updated_at
)

-- Audit trail of all balance changes
ledger_transactions (
  id, ledger_id, type, amount, currency, status,
  description, reference_type, reference_id,
  bank_code, account_number, account_name,
  created_at, processed_at
)

-- Product sales
product_transactions (
  id, type, buyer_account_id, seller_account_id,
  product_id, invoice_number,
  seller_price, platform_fee, gateway_fee, total_charged, currency,
  status, metadata, created_at, completed_at, settled_at
)

-- Withdrawals
disbursements (
  id, ledger_id, amount, currency, status,
  bank_code, account_number, account_name,
  description, external_transaction_id, failure_reason,
  created_at, processed_at
)

-- Reconciliation audit
reconciliation_logs (
  id, ledger_id, previous_pending, previous_available,
  current_pending, current_available, pending_diff, available_diff,
  is_settlement, settled_amount, fee_amount, notes, created_at
)

-- Safety gate: track expected balances
expected_balances (
  id, ledger_id, expected_pending, expected_available,
  last_calculated_at, verified_with_gateway, last_verified_at,
  created_at, updated_at
)

-- Safety gate: track discrepancies
reconciliation_discrepancies (
  id, ledger_id, discrepancy_type,
  expected_pending, actual_pending, expected_available, actual_available,
  pending_diff, available_diff, severity, status,
  detected_at, resolved_at, resolution_notes, related_tx_ids
)

-- Fee configuration (database-driven!)
fee_configs (
  id, config_type, payment_channel, fee_type,
  fixed_amount, percentage, is_active,
  created_at, updated_at
)
```

### Important Indexes

```sql
-- Optimize balance lookups
INDEX idx_account_id ON ledgers(account_id)
INDEX idx_singapay_account ON ledgers(singapay_account_id)

-- Optimize transaction history queries
INDEX idx_ledger_created ON ledger_transactions(ledger_id, created_at DESC)
INDEX idx_reference ON ledger_transactions(reference_type, reference_id)

-- Optimize sales history queries
INDEX idx_seller ON product_transactions(seller_account_id, created_at DESC)
INDEX idx_buyer ON product_transactions(buyer_account_id, created_at DESC)

-- Optimize discrepancy monitoring
INDEX idx_discrepancy_status ON reconciliation_discrepancies(status, severity)
```

## Configuration

### Environment Variables

```bash
# Database
DATABASE_URL=postgresql://user:pass@localhost:5432/fotafoto_db

# Singapay API
Singapay_API_KEY=your-api-key
Singapay_API_SECRET=your-api-secret
Singapay_BASE_URL=https://api.singapay.com
Singapay_MAIN_SAC_ID=main-sac-account-id  # For platform fee payouts

# Server
SERVER_PORT=8080

# Application
RECONCILIATION_TOLERANCE=0.01  # 1% tolerance when comparing CSV totals with Singapay API
STALENESS_WARNING_HOURS=24     # Warn admin if last reconciliation > 24h ago
STALENESS_CRITICAL_HOURS=72    # Critical alert if last reconciliation > 72h ago
```

## Implementation Status

### ✅ Completed

- Domain entities: Ledger, ProductTransaction, LedgerTransaction, Disbursement
- Value objects: Money, FeeBreakdown, BankAccountInfo
- Reconciliation entities: ReconciliationLog, ReconciliationDiscrepancy
- Repository interfaces (in domain layer)

### 🚧 In Progress / TODO

- Application services (LedgerService, ProductSaleService, DisbursementService)
- Infrastructure repositories (Postgres implementations)
- Infrastructure: Fee repository
- API controllers (Balance, Sales, Disbursement, Admin)
- Database migrations
- Integration with existing Singapay client

### 📝 Migration Files Needed

```
migrations/001_create_ledgers.sql
migrations/002_create_ledger_transactions.sql
migrations/003_create_product_transactions.sql
migrations/004_create_disbursements.sql
migrations/005_create_reconciliation_logs.sql
migrations/006_create_fee_configs.sql
migrations/007_create_expected_balances.sql
migrations/008_create_reconciliation_discrepancies.sql
```

## Testing Strategy

### Unit Tests

- Domain logic: Ledger reconciliation, settlement detection, fee calculations
- Application services: Mock repositories and external clients

### Integration Tests

- Database repositories: Use testcontainers for PostgreSQL
- Singapay client: Mock HTTP responses (client already exists at `github.com/21strive/singapay`)

### End-to-End Tests

- Full flow: Purchase product → Check balance → Request disbursement
- Use test Singapay sandbox environment

## Known Limitations & Trade-offs

### 1. Manual Reconciliation Required

- **Limitation**: Singapay doesn't provide webhooks or automated settlement data feed
- **Trade-off**: Admin must manually download and upload settlement CSV
- **Impact**: Balance freshness depends on admin uploading daily reports
- **Mitigation**: Monitor staleness, alert admin if >24h without reconciliation

### 2. Disbursement Failure Risk

- **Risk**: If Singapay API fails after we debit expected balance, we have a temporary mismatch
- **Mitigation**:
  - Mark as FAILED
  - Rollback expected_available balance immediately
  - Alert ops team for manual review
  - Next reconciliation will sync actual state

### 3. Transaction Matching Not 100% Accurate

- **Approach**: Match by INVOICE NUMBER, FIFO fallback if not found
- **Edge cases**: Refunds, test transactions, manual adjustments
- **Fallback**: Log as "unmatched settlement" for finance team investigation

## Deployment Considerations

### Database Migrations

Run migrations in order using migration tool (e.g., golang-migrate, Goose)

### Monitoring & Alerts

**Metrics to track**:

- `ledger.reconciliation.processed`: Number of transactions processed per reconciliation
- `ledger.reconciliation.unmatched`: Unmatched transactions requiring investigation
- `ledger.reconciliation.latency`: Time to process reconciliation CSV
- `disbursement.failures`: Failed disbursement attempts
- `reconciliation.discrepancies`: Count of balance mismatches

**Alerts**:

- Singapay API error rate > 5% → Alert DevOps
- Disbursement failure → Immediate alert for manual review
- Balance discrepancy detected → Alert finance team (non-blocking)
- Staleness > 24h → Warn admin to upload CSV
- Staleness > 72h → Critical alert to admin and finance

### Scaling Considerations

**Current bottleneck**: CSV parsing and database transactions during reconciliation

- **Mitigation**: Batch inserts, transaction batching
- **Future**: Parallel processing of CSV rows if needed

**Database scaling**:

- Partition `ledger_transactions` by `created_at` (time-series data)
- Read replicas for transaction history queries
- Index on `product_transactions.external_transaction_id` for fast CSV matching

## References

### Analytics Source of Truth (CRITICAL)

For any analytics-related task (schema changes, ETL logic, dimension/fact mapping, dashboard metrics, or reconciliation reporting), always reference and align with:

- [`ANALYTICS_SCHEMA.md`](ANALYTICS_SCHEMA.md)

If there is any conflict between implementation details and analytics assumptions, treat `ANALYTICS_SCHEMA.md` as the primary source for analytics behavior and naming.

### Architecture Diagrams

Visual documentation of the ledger system architecture is available in the `diagrams/` directory:

| Diagram                     | File                                                                                     | Description                                                              |
| --------------------------- | ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| Payment Execution           | [`diagrams/101-payment-execution.md`](diagrams/101-payment-execution.md)                 | Payment request, completion, and initial ledger recording                |
| Settlement & Reconciliation | [`diagrams/102-settlement-reconciliation.md`](diagrams/102-settlement-reconciliation.md) | Settlement CSV upload, matching, and balance updates (Actual & Expected) |
| Withdrawal (Disbursement)   | [`diagrams/103-withdrawal-disbursement.md`](diagrams/103-withdrawal-disbursement.md)     | Withdrawal flow using Safe Balance strategy and rollback mechanism       |

See [`diagrams/README.md`](diagrams/README.md) for full documentation of all diagrams and architecture rationale.

### DDD Patterns

- **Eric Evans**: "Domain-Driven Design" (2003) - Aggregate boundaries
- **Vaughn Vernon**: "Implementing Domain-Driven Design" (2013) - Keep aggregates small
- **Martin Fowler**: "Patterns of Enterprise Application Architecture" - Repository pattern

### Specific Design Decisions

**Why separate ProductTransaction and LedgerTransaction?**

- Different bounded contexts (Sales vs Financial)
- Different lifecycles and query patterns

**Why mirror Singapay's pending/available model?**

- External system constraint (can't disburse from pending)
- Anti-Corruption Layer pattern

**Why buyer pays all fees?**

- Seller satisfaction (gets 100% of their price)
- Transparent pricing
- Platform revenue predictability

**Why expected balance tracking?**

- Safety gate against Singapay incidents
- Early detection of discrepancies
- Audit trail for investigations

**Why manual CSV reconciliation instead of automatic API polling?**

- Explicit control by finance team
- Settlement CSV is single source of truth from Singapay
- Simpler architecture (no background jobs, no cron schedulers)
- Better auditability (clear record of who uploaded when)
- Easy error recovery (retry failed uploads immediately)

## Contributing Guidelines

### Code Style

- Follow Go standard library conventions
- Use `gofmt` for formatting
- Domain errors use custom `ledgererr` package (already implemented)

### Adding New Features

1. **Start with Domain Layer**
   - Define entities, value objects, invariants
   - Write domain tests first

2. **Application Layer**
   - Orchestrate domain logic
   - Handle cross-aggregate operations

3. **Infrastructure Layer**
   - Implement repository interfaces
   - Add database migrations

4. **Interface Layer**
   - Create HTTP handlers
   - Add to router

### Repository Boundaries

- Each aggregate has its own repository
- No cross-aggregate queries in repositories
- Use application layer for multi-aggregate operations

---

**Last Updated**: 2026-01-19  
**Version**: 1.0.0  
**Maintainer**: Development Team

**Module**: `github.com/21strive/fotafoto-api`
