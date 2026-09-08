# Singapay Payment Gateway Ledger System - Architecture Documentation

## Overview

This ledger system is designed to track and manage financial transactions from the Singapay payment gateway. It provides users with a comprehensive view of their payments, settlements, and disbursements while maintaining accurate balance tracking.

## System Purpose

- Track income from customer payments
- Record settlements from Singapay (with fee deductions)
- Manage disbursements to user bank accounts ("KIRIM Singapay")
- Provide real-time balance visibility (available + pending)

## Analytics Docs

- [Analytics API Index](06-analytics-api-index.md) for the dashboard route map and `LedgerAnalyticsClient` function inventory.
- [Revenue Breakdown API](05-revenue-breakdown-api.md) for the revenue breakdown endpoint spec.

---

## Core Entities

### 1. LedgerAccount
Represents a user/merchant account in the system.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `randid` | string | Random ID for public reference |
| `name` | string | Account holder name |
| `email` | string | Unique email (Singapay requires unique emails) |

### 2. LedgerAccountBank
Stores user's bank account information for disbursements.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `ledger_account_uuid` | string | Reference to account owner |
| `bank_account_number` | string | Bank account number |
| `bank_name` | string | Bank name |

### 3. LedgerWallet
Acts as a wallet to track balances per currency.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `ledger_account_uuid` | string | Reference to account owner |
| `balance` | int64 | Available balance (settled, after fees) |
| `pending_balance` | int64 | Pending balance (waiting for settlement) |
| `income_accumulation` | int64 | Lifetime gross income |
| `withdraw_accumulation` | int64 | Lifetime disbursements to bank |
| `currency` | string | Currency code (e.g., "IDR") |

### 4. LedgerPayment
Records individual payment transactions.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `ledger_account_uuid` | string | Reference to account owner |
| `ledger_wallet_uuid` | string | Reference to wallet |
| `ledger_settlement_uuid` | string | Reference to settlement (when settled) |
| `invoice_number` | string | Invoice/order ID |
| `amount` | int64 | Payment amount (gross) |
| `currency` | string | Currency code |
| `payment_method` | string | Payment method (QRIS, VA_BCA, etc.) |
| `status` | string | PENDING, PAID, FAILED, EXPIRED |
| `gateway_request_id` | string | Singapay request ID |

### 5. LedgerSettlement
Records Singapay settlement batches.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `ledger_account_uuid` | string | Reference to account owner |
| `batch_number` | string | Singapay batch number |
| `gross_amount` | int64 | Total before fee deduction |
| `net_amount` | int64 | Total after fee deduction |
| `fee_amount` | int64 | Fee deducted by Singapay |
| `status` | string | IN_PROGRESS, TRANSFERRED |

### 6. LedgerDisbursement
Records disbursement requests to user's bank account ("KIRIM Singapay").

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `ledger_account_uuid` | string | Reference to account owner |
| `ledger_wallet_uuid` | string | Reference to wallet |
| `ledger_account_bank_uuid` | string | Reference to destination bank |
| `amount` | int64 | Disbursement amount |
| `currency` | string | Currency code |
| `bank_name` | string | Destination bank (denormalized) |
| `bank_account_number` | string | Destination account (denormalized) |
| `status` | string | PENDING, PROCESSING, SUCCESS, FAILED |

### 7. LedgerTransaction
Audit log for all wallet balance changes.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Primary key |
| `transaction_type` | string | PAYMENT, SETTLEMENT, WITHDRAW |
| `ledger_payment_uuid` | string | Reference to payment (if applicable) |
| `ledger_settlement_uuid` | string | Reference to settlement (if applicable) |
| `ledger_wallet_uuid` | string | Reference to wallet |
| `amount` | int64 | Transaction amount |
| `description` | string | Transaction description |

---

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              LEDGER SYSTEM                                       │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  ┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐             │
│  │  LedgerAccount  │───▶│  LedgerWallet   │───▶│LedgerAccountBank│             │
│  │  (User/Merchant)│    │  (per currency) │    │ (Bank Details)  │             │
│  └────────┬────────┘    └────────┬────────┘    └─────────────────┘             │
│           │                      │                                              │
│           │                      ▼                                              │
│           │         ┌────────────────────────┐                                  │
│           │         │      BALANCE FIELDS    │                                  │
│           │         ├────────────────────────┤                                  │
│           │         │ • balance (available)  │                                  │
│           │         │ • pending_balance      │                                  │
│           │         │ • income_accumulation  │                                  │
│           │         │ • withdraw_accumulation│                                  │
│           │         └────────────────────────┘                                  │
│           │                                                                     │
│           ▼                                                                     │
│  ┌─────────────────────────────────────────────────────────────────┐           │
│  │                        MONEY FLOW                                │           │
│  ├─────────────────────────────────────────────────────────────────┤           │
│  │                                                                  │           │
│  │  LedgerPayment ──▶ LedgerSettlement ──▶ LedgerDisbursement      │           │
│  │  (PENDING→PAID)    (IN_PROGRESS→       (PENDING→PROCESSING→     │           │
│  │                     TRANSFERRED)         SUCCESS)                │           │
│  │                                                                  │           │
│  └─────────────────────────────────────────────────────────────────┘           │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Money Flow

### Complete Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              COMPLETE MONEY FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  ┌─────────────┐                                                                │
│  │  Customer   │                                                                │
│  │   Pays      │                                                                │
│  └──────┬──────┘                                                                │
│         │                                                                       │
│         ▼                                                                       │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │ STEP 1: PAYMENT CONFIRMED (PAID)                                        │   │
│  │                                                                          │   │
│  │   LedgerPayment.status = PAID                                           │   │
│  │   LedgerWallet.pending_balance += gross_amount                          │   │
│  │   LedgerWallet.income_accumulation += gross_amount                      │   │
│  │   LedgerTransaction (type: PAYMENT)                                     │   │
│  │                                                                          │   │
│  │   Example: Customer pays IDR 50,000                                     │   │
│  │   → pending_balance: 0 → 50,000                                         │   │
│  │   → balance: 0 (unchanged)                                              │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│         │                                                                       │
│         │ (Wait 1-2 days)                                                       │
│         ▼                                                                       │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │ STEP 2: SETTLEMENT TRANSFERRED                                          │   │
│  │                                                                          │   │
│  │   LedgerSettlement.status = TRANSFERRED                                 │   │
│  │   LedgerPayment.ledger_settlement_uuid = settlement.uuid                │   │
│  │   LedgerWallet.pending_balance -= gross_amount                          │   │
│  │   LedgerWallet.balance += net_amount (after fee deduction)              │   │
│  │   LedgerTransaction (type: SETTLEMENT)                                  │   │
│  │                                                                          │   │
│  │   Example: Settlement with 3% fee (IDR 1,500)                           │   │
│  │   → pending_balance: 50,000 → 0                                         │   │
│  │   → balance: 0 → 48,500                                                 │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│         │                                                                       │
│         │ (User initiates)                                                      │
│         ▼                                                                       │
│  ┌─────────────────────────────────────────────────────────────────────────┐   │
│  │ STEP 3: DISBURSEMENT ("KIRIM Singapay")                                     │   │
│  │                                                                          │   │
│  │   LedgerDisbursement.status = PENDING → PROCESSING → SUCCESS            │   │
│  │   LedgerWallet.balance -= amount (when PENDING)                         │   │
│  │   LedgerWallet.withdraw_accumulation += amount (when SUCCESS)           │   │
│  │   LedgerTransaction (type: WITHDRAW)                                    │   │
│  │                                                                          │   │
│  │   Example: User withdraws IDR 48,500                                    │   │
│  │   → balance: 48,500 → 0                                                 │   │
│  │   → withdraw_accumulation: 0 → 48,500                                   │   │
│  │   → Money arrives in user's bank account                                │   │
│  └─────────────────────────────────────────────────────────────────────────┘   │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Balance Explanation

### Wallet Balance Fields

| Field | Meaning | When It Changes |
|-------|---------|-----------------|
| `pending_balance` | Money from successful payments waiting to be settled | +gross when PAID, -gross when SETTLED |
| `balance` | Available funds ready for disbursement | +net when SETTLED, -amount when DISBURSE |
| `income_accumulation` | Lifetime gross income (for reporting) | +gross when PAID |
| `withdraw_accumulation` | Lifetime disbursements to bank | +amount when DISBURSE SUCCESS |

### Current Balance Response

```json
{
  "available_balance": 48500,    // Ready for "KIRIM Singapay"
  "pending_balance": 50000,      // Waiting for settlement
  "currency": "IDR",
  "total_income": 500000,        // Lifetime gross
  "total_withdrawn": 400000      // Lifetime to bank
}
```

---

## Project Structure

```
ledger/
├── models/
│   ├── base_model.go
│   ├── ledger_account_model.go
│   ├── ledger_account_bank_model.go
│   ├── ledger_wallet_model.go
│   ├── ledger_payment_model.go
│   ├── ledger_settlement_model.go
│   ├── ledger_disbursement_model.go
│   └── ledger_transaction_model.go
│
├── repositories/
│   ├── ledger_account_repository.go
│   ├── ledger_account_bank_repository.go
│   ├── ledger_wallet_repository.go
│   ├── ledger_payment_repository.go
│   ├── ledger_settlement_repository.go
│   ├── ledger_disbursement_repository.go
│   └── ledger_transaction_repository.go
│
├── usecases/
│   ├── ledger_account_usecase.go
│   ├── ledger_account_bank_usecase.go
│   ├── ledger_wallet_usecase.go
│   ├── ledger_payment_usecase.go
│   ├── ledger_settlement_usecase.go
│   ├── ledger_disbursement_usecase.go
│   └── ledger_transaction_usecase.go
│
├── requests/
│   ├── ledger_payment_request.go
│   ├── ledger_disbursement_request.go
│   └── ledger_transaction_request.go
│
├── responses/
│   └── wallet_balance_response.go
│
├── utils/
│   └── helper/
│
└── markdown/
    ├── 00-system-architecture.md
    ├── 01-payment-flow.md
    ├── 02-settlement-flow.md
    ├── 03-disbursement-flow.md
    └── 04-balance-query.md
```

---

## Database Schema

### Entity Relationship Diagram

```
┌─────────────────┐       ┌─────────────────┐       ┌─────────────────────┐
│  ledger_accounts│       │  ledger_wallets │       │ ledger_account_banks│
├─────────────────┤       ├─────────────────┤       ├─────────────────────┤
│ uuid (PK)       │──┐    │ uuid (PK)       │    ┌──│ uuid (PK)           │
│ randid          │  │    │ ledger_account_ │◀───┘  │ ledger_account_uuid │
│ name            │  │    │   uuid (FK)     │       │ bank_account_number │
│ email           │  │    │ balance         │       │ bank_name           │
└─────────────────┘  │    │ pending_balance │       └─────────────────────┘
                     │    │ currency        │
                     │    └─────────────────┘
                     │             ▲
                     │             │
        ┌────────────┼─────────────┼────────────────────────────┐
        │            │             │                            │
        ▼            ▼             ▼                            ▼
┌───────────────┐ ┌─────────────────────┐ ┌─────────────────────────────┐
│ledger_payments│ │ ledger_settlements  │ │   ledger_disbursements      │
├───────────────┤ ├─────────────────────┤ ├─────────────────────────────┤
│ uuid (PK)     │ │ uuid (PK)           │ │ uuid (PK)                   │
│ ledger_account│ │ ledger_account_uuid │ │ ledger_account_uuid         │
│   _uuid (FK)  │ │ batch_number        │ │ ledger_wallet_uuid          │
│ ledger_wallet │ │ gross_amount        │ │ ledger_account_bank_uuid    │
│   _uuid (FK)  │ │ net_amount          │ │ amount                      │
│ invoice_number│ │ fee_amount          │ │ status                      │
│ amount        │ │ status              │ └─────────────────────────────┘
│ status        │ └─────────────────────┘
└───────────────┘
        │
        ▼
┌────────────────────┐
│ ledger_transactions│
├────────────────────┤
│ uuid (PK)          │
│ transaction_type   │
│ ledger_payment_uuid│
│ ledger_wallet_uuid │
│ amount             │
└────────────────────┘
```

---

## Status Constants

### Payment Status
- `PENDING` - Payment link created, waiting for customer
- `PAID` - Customer completed payment
- `FAILED` - Payment failed
- `EXPIRED` - Payment link expired

### Settlement Status
- `IN_PROGRESS` - Settlement initiated by Singapay
- `TRANSFERRED` - Funds moved to available balance

### Disbursement Status
- `PENDING` - Disbursement requested, balance deducted
- `PROCESSING` - Singapay accepted the request
- `SUCCESS` - Funds transferred to bank
- `FAILED` - Disbursement failed, balance refunded

### Transaction Types
- `PAYMENT` - Successful payment (affects pending_balance)
- `SETTLEMENT` - Settlement processed (moves pending to available)
- `WITHDRAW` - Disbursement to bank (affects balance)

---

## On-Demand Settlement Reconciliation

### Overview

Singapay settles payments daily at **1PM on weekdays**, but **does not provide a webhook** for settlement completion. To detect when settlements have been processed, the system uses an **on-demand reconciliation** approach.

### Why On-Demand?

| Approach | Pros | Cons |
|----------|------|------|
| Webhook (not available) | Real-time updates | Singapay doesn't offer this for settlements |
| Scheduled Job | Predictable timing | Adds complexity, still has delay |
| **On-Demand** ✓ | No extra infrastructure, updates when user needs it | Only updates when user checks balance |

### How It Works

When a user accesses their balance page, the backend:

1. **Fetches Singapay Balance** - Calls `GetBalance` API to get real-time pending/available
2. **Compares with Ledger** - Calculates delta between Singapay pending and our pending_balance
3. **Detects Settlements** - If Singapay pending < Ledger pending, settlements occurred
4. **Reconciles** - Processes settlements FIFO, updates ledger to match Singapay

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                     ON-DEMAND RECONCILIATION                                     │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  User visits Balance Page                                                       │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  Backend: Get Singapay Balance        │◀─── Singapay GetBalance API                  │
│  │  (pending: 0, available: 95,560)  │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  Compare with Ledger Wallet       │                                          │
│  │  (pending_balance: 100,000)       │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  Delta = 100,000 - 0 = 100,000    │                                          │
│  │  Settlement detected!              │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  ┌───────────────────────────────────┐                                          │
│  │  Process IN_PROGRESS settlements  │                                          │
│  │  (FIFO order)                     │                                          │
│  │  - Status → TRANSFERRED           │                                          │
│  │  - pending_balance -= gross       │                                          │
│  │  - balance += net                 │                                          │
│  └───────────────────────────────────┘                                          │
│          │                                                                      │
│          ▼                                                                      │
│  Return updated balance to user                                                 │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Data Flow

| Step | Singapay State | Ledger State | Action |
|------|------------|--------------|--------|
| Payment confirmed | pending: +100K | pending_balance: +100K | Create settlement (IN_PROGRESS) |
| Singapay settles (1PM) | pending: 0, available: +95.5K | pending_balance: 100K (stale) | No webhook - we don't know! |
| User checks balance | pending: 0, available: 95.5K | pending_balance: 100K | Detect delta, reconcile |
| After reconciliation | pending: 0, available: 95.5K | pending_balance: 0, balance: 95.5K | Ledger matches Singapay |

### Implementation Location

The reconciliation logic is implemented in:

- **setter-service**: `app/usecases/wallet_usecase.go` → `GetBalance()` method
- **Ledger**: `GetSettlementsByAccountAndStatus()` for querying IN_PROGRESS settlements

See `02-settlement-flow.md` for detailed implementation documentation.