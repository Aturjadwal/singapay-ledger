# Disbursement Flow - Business Logic Documentation

## Overview

The disbursement flow handles the transfer of funds from the user's Singapay wallet to their bank account. This is triggered when the user initiates a "KIRIM Singapay" request. The system deducts from the available balance immediately and tracks the disbursement status until completion.

---

## Disbursement Status Flow

```
┌──────────┐     ┌─────────────┐     ┌──────────┐
│ PENDING  │────▶│ PROCESSING  │────▶│ SUCCESS  │
└──────────┘     └─────────────┘     └──────────┘
     │                  │
     │                  │
     ▼                  ▼
┌──────────┐     ┌──────────┐
│  FAILED  │◀────│  FAILED  │
│ (refund) │     │ (refund) │
└──────────┘     └──────────┘
```

### Status Definitions

| Status | Description | Balance Impact |
|--------|-------------|----------------|
| `PENDING` | Disbursement created, awaiting Singapay API call | balance -= amount |
| `PROCESSING` | Singapay accepted the request, transfer in progress | - |
| `SUCCESS` | Transfer completed to user's bank | withdraw_accumulation += amount |
| `FAILED` | Transfer failed, balance refunded | balance += amount (refund) |

---

## Disbursement Model Fields

| Field | Type | Description |
|-------|------|-------------|
| `ledger_account_uuid` | string | Reference to account owner |
| `ledger_wallet_uuid` | string | Reference to wallet |
| `ledger_account_bank_uuid` | string | Reference to destination bank |
| `amount` | int64 | Disbursement amount |
| `currency` | string | Currency code |
| `bank_name` | string | Destination bank (denormalized) |
| `bank_account_number` | string | Destination account (denormalized) |
| `gateway_request_id` | string | Singapay request ID |
| `gateway_reference_number` | string | Singapay reference number |
| `requested_at` | time.Time | When user initiated the request |
| `processed_at` | *time.Time | When Singapay accepted the request |
| `completed_at` | *time.Time | When transfer completed/failed |
| `status` | string | PENDING, PROCESSING, SUCCESS, FAILED |
| `failure_reason` | string | Reason if failed |

---

## Create Disbursement Flow ("KIRIM Singapay")

### When to Call
Called when user initiates a withdrawal/disbursement from their Singapay wallet to their bank account.

### Request Structure

```go
type LedgerDisbursementCreateRequest struct {
    LedgerAccountUUID     string `json:"ledger_account_uuid"`
    LedgerWalletUUID      string `json:"ledger_wallet_uuid"`
    LedgerAccountBankUUID string `json:"ledger_account_bank_uuid"`
    Amount                int64  `json:"amount"`
    Currency              string `json:"currency"`
}
```

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                        CREATE DISBURSEMENT FLOW                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  1. User initiates "KIRIM Singapay" from their dashboard                           │
│     - Selects destination bank account                                          │
│     - Enters amount to disburse                                                 │
│                                                                                 │
│  2. System validates:                                                           │
│     a. Check wallet has sufficient available balance                           │
│     b. Validate currency matches wallet currency                               │
│     c. Validate bank account exists and belongs to user                        │
│                                                                                 │
│  3. System creates disbursement:                                               │
│     a. Deduct amount from wallet.balance (immediate)                           │
│     b. Create LedgerDisbursement record with status = PENDING                  │
│     c. Store bank details (denormalized for historical reference)              │
│                                                                                 │
│  4. Wallet state after creation:                                               │
│     - balance -= amount (funds reserved for transfer)                          │
│     - No change to withdraw_accumulation yet                                   │
│                                                                                 │
│  5. Return disbursement record to caller                                       │
│     - Caller should then call Singapay "KIRIM" API                                 │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Implementation Logic

```go
func (u *ledgerDisbursementUseCase) CreateDisbursement(
    sqlTransaction *sqlx.Tx,
    ledgerAccountUUID string,
    ledgerWalletUUID string,
    ledgerAccountBankUUID string,
    amount int64,
    currency string,
    bankName string,
    bankAccountNumber string,
) (*models.LedgerDisbursement, *models.ErrorLog) {

    // 1. Get the wallet to check balance
    wallet, err := u.ledgerWalletRepository.GetByUUID(ledgerWalletUUID)
    if err != nil {
        return nil, err
    }

    // 2. Check if sufficient balance
    if wallet.Balance < amount {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Insufficient balance for disbursement",
        }
    }

    // 3. Validate currency matches
    if wallet.Currency != currency {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Currency mismatch with wallet",
        }
    }

    // 4. Deduct from wallet balance immediately
    wallet.Balance -= amount

    err = u.ledgerWalletRepository.Update(sqlTransaction, wallet)
    if err != nil {
        return nil, err
    }

    // 5. Create disbursement record
    timeNow := time.Now().UTC()

    disbursement := &models.LedgerDisbursement{
        LedgerAccountUUID:     ledgerAccountUUID,
        LedgerWalletUUID:      ledgerWalletUUID,
        LedgerAccountBankUUID: ledgerAccountBankUUID,
        Amount:                amount,
        Currency:              currency,
        BankName:              bankName,
        BankAccountNumber:     bankAccountNumber,
        RequestedAt:           timeNow,
        Status:                models.DisbursementStatusPending,
    }

    err = u.ledgerDisbursementRepository.Insert(sqlTransaction, disbursement)
    if err != nil {
        return nil, err
    }

    return disbursement, nil
}
```

---

## Confirm Disbursement Flow

### When to Call
Called after Singapay API accepts the disbursement request.

### Request Structure

```go
type LedgerDisbursementConfirmRequest struct {
    DisbursementUUID       string `json:"disbursement_uuid"`
    GatewayRequestId       string `json:"gateway_request_id"`
    GatewayReferenceNumber string `json:"gateway_reference_number"`
}
```

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                       CONFIRM DISBURSEMENT FLOW                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  1. External service calls Singapay "KIRIM" API                                    │
│                                                                                 │
│  2. Singapay returns success response:                                             │
│     - request_id (for webhook matching)                                        │
│     - reference_number                                                          │
│                                                                                 │
│  3. External service calls Ledger.ConfirmDisbursement:                         │
│     - disbursement_uuid                                                         │
│     - gateway_request_id                                                        │
│     - gateway_reference_number                                                  │
│                                                                                 │
│  4. Ledger updates disbursement:                                               │
│     - Status = PROCESSING                                                       │
│     - Store gateway references                                                  │
│     - Set processed_at timestamp                                               │
│                                                                                 │
│  5. No wallet changes (balance already deducted)                               │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Implementation Logic

```go
func (u *ledgerDisbursementUseCase) ConfirmDisbursement(
    sqlTransaction *sqlx.Tx,
    uuid string,
    gatewayRequestId string,
    gatewayReferenceNumber string,
) (*models.LedgerDisbursement, *models.ErrorLog) {

    // 1. Get disbursement
    disbursement, err := u.ledgerDisbursementRepository.GetByUUID(uuid)
    if err != nil {
        return nil, err
    }

    // 2. Validate status
    if disbursement.Status != models.DisbursementStatusPending {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Disbursement is not in pending status",
        }
    }

    // 3. Update status and gateway references
    timeNow := time.Now().UTC()

    disbursement.Status = models.DisbursementStatusProcessing
    disbursement.GatewayRequestId = gatewayRequestId
    disbursement.GatewayReferenceNumber = gatewayReferenceNumber
    disbursement.ProcessedAt = &timeNow

    err = u.ledgerDisbursementRepository.Update(sqlTransaction, disbursement)
    if err != nil {
        return nil, err
    }

    return disbursement, nil
}
```

---

## Complete Disbursement Flow

### When to Call
Called when money-in webhook confirms the transfer has been sent to the user's bank account.

### Request Structure

```go
type LedgerDisbursementCompleteRequest struct {
    GatewayRequestId string `json:"gateway_request_id"`
}
```

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                       COMPLETE DISBURSEMENT FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  1. Singapay sends webhook confirming transfer complete                            │
│     - Status: SUCCESS                                                           │
│     - request_id matches our gateway_request_id                                │
│                                                                                 │
│  2. External service calls Ledger.CompleteDisbursement:                        │
│     - gateway_request_id (to find the disbursement)                            │
│                                                                                 │
│  3. Ledger completes disbursement:                                             │
│     a. Find disbursement by gateway_request_id                                 │
│     b. Validate status is PROCESSING                                           │
│     c. Update status to SUCCESS                                                │
│     d. Set completed_at timestamp                                              │
│     e. Update wallet:                                                          │
│        - withdraw_accumulation += amount                                       │
│        - last_withdraw = now                                                   │
│     f. Create LedgerTransaction (type: WITHDRAW)                               │
│                                                                                 │
│  4. Money is now in user's bank account!                                       │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Implementation Logic

```go
func (u *ledgerDisbursementUseCase) CompleteDisbursement(
    sqlTransaction *sqlx.Tx,
    uuid string,
) (*models.LedgerDisbursement, *models.ErrorLog) {

    // 1. Get disbursement
    disbursement, err := u.ledgerDisbursementRepository.GetByUUID(uuid)
    if err != nil {
        return nil, err
    }

    // 2. Validate status
    if disbursement.Status != models.DisbursementStatusProcessing {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Disbursement is not in processing status",
        }
    }

    // 3. Update status
    timeNow := time.Now().UTC()

    disbursement.Status = models.DisbursementStatusSuccess
    disbursement.CompletedAt = &timeNow

    err = u.ledgerDisbursementRepository.Update(sqlTransaction, disbursement)
    if err != nil {
        return nil, err
    }

    // 4. Update wallet withdraw accumulation
    wallet, err := u.ledgerWalletRepository.GetByUUID(disbursement.LedgerWalletUUID)
    if err != nil {
        return nil, err
    }

    wallet.WithdrawAccumulation += disbursement.Amount
    wallet.LastWithdraw = &timeNow

    err = u.ledgerWalletRepository.Update(sqlTransaction, wallet)
    if err != nil {
        return nil, err
    }

    // 5. Create transaction record
    transaction := &models.LedgerTransaction{
        TransactionType:  models.TransactionTypeWithdraw,
        LedgerWalletUUID: disbursement.LedgerWalletUUID,
        Amount:           disbursement.Amount,
        Description:      fmt.Sprintf("Disbursement to %s %s",
            disbursement.BankName,
            disbursement.BankAccountNumber),
    }

    err = u.ledgerTransactionRepository.Insert(sqlTransaction, transaction)
    if err != nil {
        return nil, err
    }

    return disbursement, nil
}
```

---

## Fail Disbursement Flow

### When to Call
Called when Singapay rejects the disbursement or the transfer fails.

### Request Structure

```go
type LedgerDisbursementFailRequest struct {
    GatewayRequestId string `json:"gateway_request_id"`
    Reason           string `json:"reason"`
}
```

### Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         FAIL DISBURSEMENT FLOW                                   │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  1. Singapay sends failure notification or API returns error                       │
│     - Could be: invalid bank account, insufficient funds at Singapay, etc.         │
│                                                                                 │
│  2. External service calls Ledger.FailDisbursement:                            │
│     - gateway_request_id or disbursement_uuid                                  │
│     - reason (for audit trail)                                                 │
│                                                                                 │
│  3. Ledger fails disbursement and refunds:                                     │
│     a. Validate status is PENDING or PROCESSING                                │
│     b. Update status to FAILED                                                 │
│     c. Store failure_reason                                                    │
│     d. Set completed_at timestamp                                              │
│     e. REFUND wallet:                                                          │
│        - balance += amount (money back to available)                           │
│                                                                                 │
│  4. User can retry disbursement with corrected details                         │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Implementation Logic

```go
func (u *ledgerDisbursementUseCase) FailDisbursement(
    sqlTransaction *sqlx.Tx,
    uuid string,
    reason string,
) (*models.LedgerDisbursement, *models.ErrorLog) {

    // 1. Get disbursement
    disbursement, err := u.ledgerDisbursementRepository.GetByUUID(uuid)
    if err != nil {
        return nil, err
    }

    // 2. Validate status - can only fail pending or processing
    if disbursement.Status != models.DisbursementStatusPending &&
       disbursement.Status != models.DisbursementStatusProcessing {
        return nil, &models.ErrorLog{
            StatusCode: http.StatusBadRequest,
            Message:    "Disbursement cannot be failed in current status",
        }
    }

    // 3. Update status
    timeNow := time.Now().UTC()

    disbursement.Status = models.DisbursementStatusFailed
    disbursement.FailureReason = reason
    disbursement.CompletedAt = &timeNow

    err = u.ledgerDisbursementRepository.Update(sqlTransaction, disbursement)
    if err != nil {
        return nil, err
    }

    // 4. REFUND the wallet balance
    wallet, err := u.ledgerWalletRepository.GetByUUID(disbursement.LedgerWalletUUID)
    if err != nil {
        return nil, err
    }

    wallet.Balance += disbursement.Amount

    err = u.ledgerWalletRepository.Update(sqlTransaction, wallet)
    if err != nil {
        return nil, err
    }

    return disbursement, nil
}
```

---

## Wallet Impact Summary

| Action | balance | withdraw_accumulation | Notes |
|--------|---------|----------------------|-------|
| Create Disbursement | -amount | - | Funds reserved |
| Confirm Disbursement | - | - | No change |
| Complete Disbursement | - | +amount | Transfer complete |
| Fail Disbursement | +amount (refund) | - | Funds returned |

---

## Disbursement Timeline Example

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         DISBURSEMENT TIMELINE                                    │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Initial State:                                                                 │
│  - balance: 135,800                                                             │
│  - withdraw_accumulation: 0                                                     │
│                                                                                 │
│  10:00 - User initiates "KIRIM Singapay" for IDR 100,000                           │
│          ├─ Status = PENDING                                                    │
│          └─ balance: 135,800 → 35,800 (reserved)                               │
│                                                                                 │
│  10:01 - External service calls Singapay "KIRIM" API                               │
│          ├─ Singapay accepts request                                               │
│          ├─ Status = PROCESSING                                                 │
│          └─ gateway_request_id stored                                          │
│                                                                                 │
│  10:30 - money-in webhook confirms transfer complete                               │
│          ├─ Status = SUCCESS                                                    │
│          ├─ withdraw_accumulation: 0 → 100,000                                 │
│          └─ Money arrived in user's bank!                                      │
│                                                                                 │
│  Final State:                                                                   │
│  - balance: 35,800                                                              │
│  - withdraw_accumulation: 100,000                                               │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Failed Disbursement Timeline Example

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      FAILED DISBURSEMENT TIMELINE                                │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Initial State:                                                                 │
│  - balance: 135,800                                                             │
│                                                                                 │
│  10:00 - User initiates "KIRIM Singapay" for IDR 100,000                           │
│          └─ balance: 135,800 → 35,800                                          │
│                                                                                 │
│  10:01 - External service calls Singapay API                                       │
│          └─ Singapay rejects: "Invalid bank account number"                        │
│                                                                                 │
│  10:02 - FailDisbursement called                                               │
│          ├─ Status = FAILED                                                     │
│          ├─ failure_reason = "Invalid bank account number"                     │
│          └─ balance: 35,800 → 135,800 (REFUNDED)                               │
│                                                                                 │
│  User can now correct bank details and retry                                   │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Query Methods

### Get Disbursements by Account

```go
func (u *ledgerDisbursementUseCase) GetDisbursementsByAccount(
    ledgerAccountUUID string,
) ([]*models.LedgerDisbursement, *models.ErrorLog) {

    disbursements, err := u.ledgerDisbursementRepository.GetByLedgerAccountUUID(
        ledgerAccountUUID,
    )
    if err != nil {
        return nil, err
    }

    return disbursements, nil
}
```

### Get Pending Disbursements

```go
func (u *ledgerDisbursementUseCase) GetPendingDisbursements() ([]*models.LedgerDisbursement, *models.ErrorLog) {

    disbursements, err := u.ledgerDisbursementRepository.GetPendingDisbursements()
    if err != nil {
        return nil, err
    }

    return disbursements, nil
}
```

---

## Why Denormalize Bank Details?

The `bank_name` and `bank_account_number` are stored directly on the disbursement record even though they exist in `LedgerAccountBank`. This is because:

1. **Historical Accuracy**: If the user updates their bank details later, we still want to know where the money was actually sent
2. **Audit Trail**: Financial records should be immutable for compliance
3. **Query Performance**: No need to join tables when viewing disbursement history

---

## API Endpoints (for integration)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/ledger/disbursements` | POST | Create new disbursement |
| `/ledger/disbursements/{uuid}/confirm` | POST | Confirm disbursement (Singapay accepted) |
| `/ledger/disbursements/{uuid}/complete` | POST | Complete disbursement (transfer done) |
| `/ledger/disbursements/{uuid}/fail` | POST | Fail disbursement (refund) |
| `/ledger/disbursements/{uuid}` | GET | Get disbursement by UUID |
| `/ledger/disbursements/account/{account_uuid}` | GET | Get all disbursements for account |
| `/ledger/disbursements/pending` | GET | Get all pending/processing disbursements |