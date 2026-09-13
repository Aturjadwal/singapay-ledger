# Settlement Flow - Business Logic Documentation

## Overview

The settlement flow handles the process of Singapay settling payments to the merchant's Singapay wallet. After a customer pays, Singapay batches transactions and settles them typically within 1-2 business days. During settlement, Singapay deducts their fees, and the net amount becomes available for disbursement.

---

## Settlement Status Flow

```
┌─────────────────┐                    ┌─────────────────┐
│   IN_PROGRESS   │───────────────────▶│   TRANSFERRED   │
│  (Settlement    │                    │  (Funds now     │
│   initiated)    │                    │   available)    │
└─────────────────┘                    └─────────────────┘
```

### Status Definitions

| Status | Description |
|--------|-------------|
| `IN_PROGRESS` | Singapay has initiated the settlement batch, funds are being processed |
| `TRANSFERRED` | Settlement complete, net amount moved to available balance |

---

## Settlement Model Fields

| Field | Type | Description |
|-------|------|-------------|
| `ledger_account_uuid` | string | Reference to the account owner |
| `batch_number` | string | Singapay's unique batch identifier |
| `settlement_date` | time.Time | Scheduled settlement date |
| `real_settlement_date` | *time.Time | Actual transfer date (filled when TRANSFERRED) |
| `currency` | string | Currency code (e.g., "IDR") |
| `gross_amount` | int64 | Total amount before fee deduction |
| `net_amount` | int64 | Amount after fee deduction (what user receives) |
| `fee_amount` | int64 | Fee deducted by Singapay (gross - net) |
| `bank_name` | string | Destination bank name |
| `bank_account_number` | string | Destination bank account number |
| `account_type` | string | "ACCOUNT" or "SUB_ACCOUNT" |
| `status` | string | "IN_PROGRESS" or "TRANSFERRED" |

---

## Fee Calculation Example

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           FEE CALCULATION EXAMPLE                                │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Service Price (what provider wants to receive):  IDR 100,000 (net_amount)     │
│                                                                                 │
│  Step 1: Calculate gross amount (before payment link creation)                 │
│    - Payment method: QRIS                                                       │
│    - gateway fee: IDR 700 (flat fee, no tax for QRIS)                             │
│    - Gross amount = 100,000 + 700 = IDR 100,700                                │
│                                                                                 │
│  Step 2: Customer pays IDR 100,700 (gross_amount)                              │
│                                                                                 │
│  Step 3: On payment confirmation, create settlement:                           │
│    - gross_amount = 100,700 (what customer paid)                               │
│    - fee_amount = 700 (gateway fee)                                               │
│    - net_amount = 100,000 (what provider receives after settlement)            │
│                                                                                 │
│  Stored in LedgerSettlement:                                                    │
│    - gross_amount = 100700                                                      │
│    - net_amount = 100000                                                        │
│    - fee_amount = 700                                                           │
│                                                                                 │
│  Wallet Impact:                                                                 │
│    - On confirm: pending_balance += 100,700 (gross)                            │
│    - On settle:  pending_balance -= 100,700, balance += 100,000 (net)          │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Amount Definitions

| Term | Description | When Used |
|------|-------------|-----------|
| **Net Amount** | Service price / what provider wants to receive | Booking creation, after settlement |
| **Gross Amount** | What customer pays (net + gateway fees) | Payment link, LedgerPayment.Amount, pending_balance |
| **Fee Amount** | Singapay transaction fee + tax (gross - net) | Settlement record |

---

## Create Settlement Flow

### When to Call

**Important**: Settlements should be created when a payment is **confirmed** (money-in webhook SUCCESS), NOT when the payment link is created.

**Correct Timing:**
- ✅ Create settlement in the Singapay notification/webhook handler after `ConfirmPayment()` succeeds
- ❌ Do NOT create settlement when generating the payment link

**Why?**
1. **Customer may abandon payment**: Creating settlement at payment link creation results in orphaned records
2. **Payment method may differ**: Customer might choose QRIS instead of VA, affecting fee calculation
3. **Payment may expire**: Unused settlements require cleanup
4. **Accurate fees**: The actual payment method from money-in webhook determines the correct fee

**Trigger**: Called in the payment notification handler (webhook) when `transaction.status == "SUCCESS"`.

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                          CREATE SETTLEMENT FLOW                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  1. Singapay sends webhook notification (payment SUCCESS):                         │
│     - transaction.status = "SUCCESS"                                           │
│     - channel.id = actual payment method (e.g., "VIRTUAL_ACCOUNT_BCA", "QRIS") │
│     - order.invoice_number = original invoice                                  │
│     - order.amount = gross amount paid by customer                             │
│                                                                                 │
│  2. Webhook handler confirms payment:                                          │
│     - Calls LedgerPaymentUseCase.ConfirmPayment()                             │
│     - Updates payment status to PAID                                           │
│     - Adds amount to pending_balance and income_accumulation                  │
│                                                                                 │
│  3. Calculate settlement fee using actual payment method:                      │
│     - Calls SingapaySettlementUseCase.CalculateSettlementFee(paymentMethod, amount)│
│     - Returns: grossAmount, netAmount, transactionFee, tax                    │
│                                                                                 │
│  4. Create LedgerSettlement record:                                            │
│     - Status = IN_PROGRESS                                                      │
│     - batch_number = invoice_number (for idempotency)                          │
│     - settlement_date = estimated (next business day)                          │
│     - gross_amount = customer paid amount                                       │
│     - net_amount = amount after gateway fees                                       │
│     - fee_amount = gross_amount - net_amount                                   │
│                                                                                 │
│  Note: Wallet pending_balance is already updated in step 2.                    │
│        When Singapay actually settles, reconciliation moves to TRANSFERRED.        │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Integration Example (Webhook Handler)

```go
func (h *webhookHandler) processSuccessfulPayment(notification *SingapayNotification) error {
    tx, err := h.db.BeginTx()
    if err != nil {
        return err
    }
    defer tx.Rollback()
    
    // 1. Get actual payment method from Singapay notification
    paymentMethod := notification.Channel.ID.String
    
    // 2. Confirm payment in ledger (updates wallet pending_balance)
    confirmedPayment, err := h.ledgerPaymentUseCase.ConfirmPayment(tx, &ConfirmPaymentRequest{
        GatewayRequestId: notification.Transaction.OriginalRequestID.String,
        PaymentMethod:    paymentMethod,
        PaymentDate:      time.Now(),
    })
    if err != nil {
        return err
    }
    
    // 3. Calculate fee using actual payment method
    settlementResult, err := h.gateway.CalculateSettlementFee(
        paymentMethod,
        float64(confirmedPayment.Amount),
    )
    if err != nil {
        return err
    }
    
    // 4. Create settlement record
    estimatedSettlementDate := time.Now().AddDate(0, 0, 1) // Singapay settles next day ~1 PM
    
    _, err = h.ledgerSettlementUseCase.CreateSettlement(
        tx,
        confirmedPayment.LedgerAccountUUID,
        confirmedPayment.InvoiceNumber,  // Use as batch_number for idempotency
        estimatedSettlementDate,
        confirmedPayment.Currency,
        int64(settlementResult.GrossAmount),
        int64(settlementResult.NetAmount),
        "", // bankName - filled during disbursement
        "", // bankAccountNumber - filled during disbursement
        AccountTypeSubAccount,
    )
    if err != nil {
        return err
    }
    
    return tx.Commit()
}
```

### Implementation Logic

```go
func (u *ledgerSettlementUseCase) CreateSettlement(
    sqlTransaction *sqlx.Tx,
    ledgerAccountUUID string,
    batchNumber string,
    settlementDate time.Time,
    currency string,
    grossAmount int64,
    netAmount int64,
    bankName string,
    bankAccountNumber string,
    accountType string,
) (*models.LedgerSettlement, *models.ErrorLog) {

    // 1. Check if settlement with this batch number already exists
    existingSettlement, err := u.ledgerSettlementRepository.GetByBatchNumber(batchNumber)
    if err != nil && err.StatusCode != http.StatusNotFound {
        return nil, err
    }

    // Return early if settlement already exists (idempotency)
    if existingSettlement != nil {
        return existingSettlement, nil
    }

    // 2. Calculate fee amount
    feeAmount := grossAmount - netAmount

    // 3. Create settlement record
    settlement := &models.LedgerSettlement{
        LedgerAccountUUID:  ledgerAccountUUID,
        BatchNumber:        batchNumber,
        SettlementDate:     settlementDate,
        RealSettlementDate: nil, // Filled when TRANSFERRED
        Currency:           currency,
        GrossAmount:        grossAmount,
        NetAmount:          netAmount,
        FeeAmount:          feeAmount,
        BankName:           bankName,
        BankAccountNumber:  bankAccountNumber,
        AccountType:        accountType,
        Status:             models.SettlementStatusInProgress,
    }

    // 4. Insert settlement
    err = u.ledgerSettlementRepository.Insert(sqlTransaction, settlement)
    if err != nil {
        return nil, err
    }

    return settlement, nil
}
```

---

## Complete Settlement Flow (TRANSFERRED)

### When to Call
Called when Singapay confirms the settlement has been transferred to the merchant's Singapay wallet.

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                       COMPLETE SETTLEMENT FLOW                                   │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  1. Singapay confirms settlement is complete                                       │
│     - Funds have been transferred to Singapay wallet                               │
│                                                                                 │
│  2. System calls CompleteSettlement:                                           │
│     - settlement_uuid or batch_number                                          │
│     - real_settlement_date                                                      │
│                                                                                 │
│  3. Ledger updates settlement and wallet:                                      │
│     a. Update settlement status to TRANSFERRED                                 │
│     b. Set real_settlement_date                                                │
│     c. Get the wallet for this account + currency                              │
│     d. Update wallet:                                                          │
│        - pending_balance -= gross_amount                                       │
│        - balance += net_amount                                                 │
│     e. Create LedgerTransaction (type: SETTLEMENT)                             │
│                                                                                 │
│  4. Wallet state after settlement:                                             │
│     BEFORE:                          AFTER:                                    │
│     - pending_balance: 50,000        - pending_balance: 0                      │
│     - balance: 0                     - balance: 48,500                         │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Implementation Logic

```go
func (u *ledgerSettlementUseCase) CompleteSettlement(
    sqlTransaction *sqlx.Tx,
    uuid string,
    realSettlementDate time.Time,
) (*models.LedgerSettlement, *models.ErrorLog) {

    // 1. Get settlement
    settlement, err := u.ledgerSettlementRepository.GetByUUID(uuid)
    if err != nil {
        return nil, err
    }

    // 2. Validate status
    if settlement.Status != models.SettlementStatusInProgress {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Settlement is not in progress",
        }
    }

    // 3. Update settlement status
    settlement.Status = models.SettlementStatusTransferred
    settlement.RealSettlementDate = &realSettlementDate

    err = u.ledgerSettlementRepository.Update(sqlTransaction, settlement)
    if err != nil {
        return nil, err
    }

    // 4. Get wallet for this account + currency
    wallet, err := u.ledgerWalletUseCase.GetWalletByLedgerAccountAndCurrency(
        settlement.LedgerAccountUUID,
        settlement.Currency,
    )
    if err != nil {
        return nil, err
    }

    // 5. Update wallet balances
    // Move from pending to available (with fee deduction)
    _, err = u.ledgerWalletUseCase.SettlePendingBalance(
        sqlTransaction,
        wallet.UUID,
        settlement.GrossAmount, // Amount to deduct from pending
        settlement.NetAmount,   // Amount to add to available
    )
    if err != nil {
        return nil, err
    }

    // 6. Create transaction record
    transaction := &models.LedgerTransaction{
        TransactionType:      models.TransactionTypeSettlement,
        LedgerSettlementUUID: settlement.UUID,
        LedgerWalletUUID:     wallet.UUID,
        Amount:               settlement.NetAmount,
        Description:          fmt.Sprintf("Settlement %s: gross=%d, fee=%d, net=%d",
            settlement.BatchNumber,
            settlement.GrossAmount,
            settlement.FeeAmount,
            settlement.NetAmount),
    }

    err = u.ledgerTransactionRepository.Insert(sqlTransaction, transaction)
    if err != nil {
        return nil, err
    }

    return settlement, nil
}
```

---

## Wallet Balance Update (SettlePendingBalance)

### Implementation Logic

```go
// SettlePendingBalance moves funds from pending to available when Singapay settles
// pendingAmount: the gross amount to deduct from pending_balance
// netAmount: the net amount after fees (now available in Singapay wallet for disbursement)
func (u *ledgerWalletUseCase) SettlePendingBalance(
    sqlTransaction *sqlx.Tx,
    walletUUID string,
    pendingAmount int64,
    netAmount int64,
) (*models.LedgerWallet, *models.ErrorLog) {

    wallet, err := u.ledgerWalletRepository.GetByUUID(walletUUID)
    if err != nil {
        return nil, err
    }

    // Validate sufficient pending balance
    if wallet.PendingBalance < pendingAmount {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Insufficient pending balance for settlement",
        }
    }

    // Deduct from pending balance (the gross amount that was pending)
    wallet.PendingBalance -= pendingAmount

    // Add to available balance (net amount after fee deduction)
    // This money is now available in Singapay wallet for disbursement via "KIRIM Singapay"
    wallet.Balance += netAmount

    err = u.ledgerWalletRepository.Update(sqlTransaction, wallet)
    if err != nil {
        return nil, err
    }

    return wallet, nil
}
```

---

## Wallet Impact Summary

| Action | pending_balance | balance | Notes |
|--------|-----------------|---------|-------|
| Create Settlement (IN_PROGRESS) | - | - | No wallet change yet |
| Complete Settlement (TRANSFERRED) | -gross_amount | +net_amount | Fee deducted |

---

## Settlement Timeline Example

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           SETTLEMENT TIMELINE                                    │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Day 1 (Monday):                                                                │
│  ├─ 09:00 - Customer A pays IDR 50,000 → pending_balance = 50,000             │
│  ├─ 14:00 - Customer B pays IDR 30,000 → pending_balance = 80,000             │
│  └─ 20:00 - Customer C pays IDR 20,000 → pending_balance = 100,000            │
│                                                                                 │
│  Day 2 (Tuesday):                                                               │
│  ├─ 10:00 - Customer D pays IDR 40,000 → pending_balance = 140,000            │
│  └─ 23:59 - Singapay cuts off for settlement batch                                 │
│                                                                                 │
│  Day 3 (Wednesday):                                                             │
│  ├─ 08:00 - Singapay creates settlement batch:                                     │
│  │          - gross_amount = 140,000                                           │
│  │          - fee (3%) = 4,200                                                 │
│  │          - net_amount = 135,800                                             │
│  │          - Status = IN_PROGRESS                                             │
│  │                                                                              │
│  └─ 15:00 - Singapay confirms transfer complete:                                   │
│             - Status = TRANSFERRED                                              │
│             - pending_balance: 140,000 → 0                                     │
│             - balance: 0 → 135,800                                             │
│                                                                                 │
│  Now merchant can "KIRIM Singapay" (disburse) IDR 135,800 to their bank           │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Query Methods

### Get Settlements by Account

```go
func (u *ledgerSettlementUseCase) GetSettlementsByAccount(
    ledgerAccountUUID string,
) ([]*models.LedgerSettlement, *models.ErrorLog) {

    settlements, err := u.ledgerSettlementRepository.GetByLedgerAccountUUID(
        ledgerAccountUUID,
    )
    if err != nil {
        return nil, err
    }

    return settlements, nil
}
```

### Get Pending Settlements

```go
func (u *ledgerSettlementUseCase) GetPendingSettlements() ([]*models.LedgerSettlement, *models.ErrorLog) {

    settlements, err := u.ledgerSettlementRepository.GetByStatus(
        models.SettlementStatusInProgress,
    )
    if err != nil {
        return nil, err
    }

    return settlements, nil
}
```

---

## API Endpoints (for integration)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/ledger/settlements` | POST | Create new settlement |
| `/ledger/settlements/{uuid}/complete` | POST | Mark settlement as transferred |
| `/ledger/settlements/{uuid}` | GET | Get settlement by UUID |
| `/ledger/settlements/batch/{batch_number}` | GET | Get settlement by batch number |
| `/ledger/settlements/account/{account_uuid}` | GET | Get all settlements for account |
| `/ledger/settlements/pending` | GET | Get all pending settlements |

---

## Settlement Reconciliation (On-Demand)

### Overview

Singapay settles payments daily at 1PM on weekdays, but **does not provide a webhook** for settlement completion. To detect when settlements have been processed, the system uses an **on-demand reconciliation** approach.

When a user accesses their balance page, the backend:
1. Fetches the real-time balance from Singapay's GetBalance API
2. Compares Singapay's pending balance with our ledger's pending balance
3. If Singapay's pending is lower, it means settlements have been processed
4. Updates our ledger to reflect the completed settlements

### Reconciliation Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                     SETTLEMENT RECONCILIATION FLOW                               │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  User visits Balance Page                                                       │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  1. Call Singapay GetBalance API      │                                          │
│  │     Returns: pending, available   │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  2. Get Ledger Wallet Balance     │                                          │
│  │     Returns: pending_balance,     │                                          │
│  │              balance              │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  3. Calculate Delta               │                                          │
│  │     delta = ledger_pending -      │                                          │
│  │             gateway_pending          │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ├─── delta <= 0 ───▶ No reconciliation needed                          │
│          │                                                                      │
│          ▼ (delta > 0)                                                          │
│  ┌───────────────────────────────────┐                                          │
│  │  4. Get IN_PROGRESS Settlements   │                                          │
│  │     (FIFO - oldest first)         │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  5. Process Settlements           │                                          │
│  │     - Update status → TRANSFERRED │                                          │
│  │     - pending_balance -= gross    │                                          │
│  │     - balance += net              │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  6. Return Updated Balance        │                                          │
│  └───────────────────────────────────┘                                          │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Query Method: Get Settlements by Account and Status

This method is used for reconciliation to get all IN_PROGRESS settlements for a specific account, ordered by creation date (FIFO).

```go
// GetSettlementsByAccountAndStatus retrieves settlements for a specific account with a specific status
// Used for settlement reconciliation - get all IN_PROGRESS settlements for an account (FIFO order)
func (u *ledgerSettlementUseCase) GetSettlementsByAccountAndStatus(
    ledgerAccountUUID string,
    status string,
) ([]*models.LedgerSettlement, *models.ErrorLog) {

    settlements, errorLog := u.ledgerSettlementRepository.GetByLedgerAccountUUIDAndStatus(
        ledgerAccountUUID,
        status,
    )
    if errorLog != nil {
        return nil, errorLog
    }

    return settlements, nil
}
```

### Repository Implementation

```go
// GetByLedgerAccountUUIDAndStatus retrieves settlements for a specific account with a specific status
// Results are ordered by created_at ASC (oldest first) for FIFO settlement processing
func (r *ledgerSettlementRepository) GetByLedgerAccountUUIDAndStatus(
    ledgerAccountUUID string,
    status string,
) ([]*models.LedgerSettlement, *models.ErrorLog) {

    var ledgerSettlements []*models.LedgerSettlement

    sqlQuery := `
        SELECT
            ls.uuid,
            ls.randid,
            ls.created_at,
            ls.updated_at,
            ls.ledger_account_uuid,
            ls.batch_number,
            ls.settlement_date,
            ls.real_settlement_date,
            ls.currency,
            ls.gross_amount,
            ls.net_amount,
            ls.fee_amount,
            ls.bank_name,
            ls.bank_account_number,
            ls.account_type,
            ls.status
        FROM
            ledger_settlements ls
        WHERE
            ls.ledger_account_uuid = $1
            AND ls.status = $2
        ORDER BY
            ls.created_at ASC
    `

    err := r.dbRead.Select(&ledgerSettlements, sqlQuery, ledgerAccountUUID, status)
    if err != nil {
        logData := helper.WriteLog(err, http.StatusInternalServerError, helper.DefaultStatusText[http.StatusInternalServerError])
        return nil, logData
    }

    return ledgerSettlements, nil
}
```

### Timeline Example with Reconciliation

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    SETTLEMENT TIMELINE WITH RECONCILIATION                       │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Day 1 (Monday) 10:00 AM - Payment Confirmed                                    │
│  ─────────────────────────────────────────────                                  │
│  Singapay Balance:                                                                  │
│    pending: 100,000                                                             │
│    available: 0                                                                 │
│                                                                                 │
│  Ledger Wallet:                                                                 │
│    pending_balance: 100,000                                                     │
│    balance: 0                                                                   │
│                                                                                 │
│  LedgerSettlement:                                                              │
│    status: IN_PROGRESS                                                          │
│    gross_amount: 100,000                                                        │
│    net_amount: 95,560 (after VA fee + tax)                                      │
│                                                                                 │
│  ═══════════════════════════════════════════════════════════════════════════    │
│                                                                                 │
│  Day 2 (Tuesday) 1:00 PM - Singapay Settlement (No Webhook)                         │
│  ──────────────────────────────────────────────────────                         │
│  Singapay Balance (real):                                                           │
│    pending: 0                                                                   │
│    available: 95,560                                                            │
│                                                                                 │
│  Ledger Wallet (stale - not yet updated):                                       │
│    pending_balance: 100,000                                                     │
│    balance: 0                                                                   │
│                                                                                 │
│  ═══════════════════════════════════════════════════════════════════════════    │
│                                                                                 │
│  Day 2 (Tuesday) 3:00 PM - User Visits Balance Page                             │
│  ───────────────────────────────────────────────────                            │
│  1. Backend calls Singapay GetBalance API                                           │
│     → pending: 0, available: 95,560                                             │
│                                                                                 │
│  2. Compare with Ledger:                                                        │
│     → delta = 100,000 - 0 = 100,000 (settlement detected!)                      │
│                                                                                 │
│  3. Get IN_PROGRESS settlements (FIFO)                                          │
│     → Found 1 settlement with gross_amount: 100,000                             │
│                                                                                 │
│  4. Process settlement:                                                         │
│     → Update status: IN_PROGRESS → TRANSFERRED                                  │
│     → pending_balance: 100,000 → 0                                              │
│     → balance: 0 → 95,560                                                       │
│                                                                                 │
│  5. Return to user:                                                             │
│     available_balance: 95,560                                                   │
│     pending_balance: 0                                                          │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Edge Cases

| Scenario | Handling |
|----------|----------|
| Singapay API is down | Return cached ledger balance, log warning |
| Multiple settlements same day | Process FIFO until delta is satisfied |
| Delta exceeds available settlements | Process all available, log discrepancy |
| Singapay pending > Ledger pending | Log data integrity warning, no action |
| Concurrent balance requests | Database transaction ensures consistency |

### Important Notes

1. **FIFO Order**: Settlements are processed oldest-first based on `created_at`
2. **Idempotency**: Only IN_PROGRESS settlements are processed; TRANSFERRED ones are skipped
3. **Atomic Updates**: Settlement status and wallet balance are updated in a single transaction
4. **Graceful Degradation**: If Singapay API fails, users still see cached ledger balance