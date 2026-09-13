# Balance Query - Business Logic Documentation

## Overview

The balance query functionality provides real-time visibility into a user's wallet balance. It returns both the available balance (settled funds ready for disbursement) and pending balance (funds waiting for settlement).

---

## Balance Response Structure

### WalletBalanceResponse

```go
type WalletBalanceResponse struct {
    // AvailableBalance is the settled amount (net, after fees) ready for disbursement via "KIRIM Singapay"
    AvailableBalance int64 `json:"available_balance"`

    // PendingBalance is the amount waiting for settlement (gross, typically 1-2 days after payment)
    PendingBalance int64 `json:"pending_balance"`

    // Currency code (e.g., "IDR")
    Currency string `json:"currency"`

    // Lifetime statistics
    TotalIncome    int64 `json:"total_income"`    // IncomeAccumulation (gross payments received)
    TotalWithdrawn int64 `json:"total_withdrawn"` // WithdrawAccumulation (net amount sent to bank)
}
```

### WalletBalanceSummaryResponse

```go
type WalletBalanceSummaryResponse struct {
    LedgerAccountUUID string                   `json:"ledger_account_uuid"`
    Wallets           []*WalletBalanceResponse `json:"wallets"`
}
```

---

## Balance Fields Explained

| Field | Source | Description |
|-------|--------|-------------|
| `available_balance` | `wallet.Balance` | Settled funds (net after fees), ready for "KIRIM Singapay" |
| `pending_balance` | `wallet.PendingBalance` | Funds from paid transactions waiting for settlement |
| `currency` | `wallet.Currency` | Currency code for this wallet |
| `total_income` | `wallet.IncomeAccumulation` | Lifetime gross income from payments |
| `total_withdrawn` | `wallet.WithdrawAccumulation` | Lifetime disbursements sent to bank |

---

## Balance State Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                            WALLET BALANCE STATE                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│   ┌─────────────────────────────────────────────────────────────────────────┐  │
│   │                         LedgerWallet                                     │  │
│   │                                                                          │  │
│   │   ┌─────────────────────────┐    ┌─────────────────────────┐            │  │
│   │   │     pending_balance     │    │        balance          │            │  │
│   │   │                         │    │                         │            │  │
│   │   │  • Gross amount         │    │  • Net amount           │            │  │
│   │   │  • From PAID payments   │    │  • After fee deduction  │            │  │
│   │   │  • Waiting 1-2 days     │    │  • Ready for KIRIM Singapay │            │  │
│   │   │                         │    │                         │            │  │
│   │   └───────────┬─────────────┘    └─────────────┬───────────┘            │  │
│   │               │                                │                         │  │
│   │               │     Settlement (TRANSFERRED)   │                         │  │
│   │               │ ──────────────────────────────▶│                         │  │
│   │               │     -gross, +net (fee taken)   │                         │  │
│   │               │                                │                         │  │
│   │               │                                │     Disbursement        │  │
│   │               │                                │ ──────────────────────▶ │  │
│   │               │                                │     To User's Bank      │  │
│   │                                                                          │  │
│   └─────────────────────────────────────────────────────────────────────────┘  │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Query Methods

### 1. Get Current Balance by Wallet UUID

Returns balance for a specific wallet.

```go
func (u *ledgerWalletUseCase) GetCurrentBalance(
    walletUUID string,
) (*responses.WalletBalanceResponse, *models.ErrorLog) {

    wallet, err := u.ledgerWalletRepository.GetByUUID(walletUUID)
    if err != nil {
        return nil, err
    }

    return &responses.WalletBalanceResponse{
        AvailableBalance: wallet.Balance,
        PendingBalance:   wallet.PendingBalance,
        Currency:         wallet.Currency,
        TotalIncome:      wallet.IncomeAccumulation,
        TotalWithdrawn:   wallet.WithdrawAccumulation,
    }, nil
}
```

### 2. Get Current Balance by Account and Currency

Returns balance for an account in a specific currency.

```go
func (u *ledgerWalletUseCase) GetCurrentBalanceByAccount(
    ledgerAccountUUID string,
    currency string,
) (*responses.WalletBalanceResponse, *models.ErrorLog) {

    wallet, err := u.ledgerWalletRepository.GetByLedgerAccountUUIDAndCurrency(
        ledgerAccountUUID,
        currency,
    )
    if err != nil {
        return nil, err
    }

    return &responses.WalletBalanceResponse{
        AvailableBalance: wallet.Balance,
        PendingBalance:   wallet.PendingBalance,
        Currency:         wallet.Currency,
        TotalIncome:      wallet.IncomeAccumulation,
        TotalWithdrawn:   wallet.WithdrawAccumulation,
    }, nil
}
```

### 3. Get Balance Summary by Account (All Currencies)

Returns balance summary for all wallets under an account.

```go
func (u *ledgerWalletUseCase) GetBalanceSummaryByAccount(
    ledgerAccountUUID string,
) (*responses.WalletBalanceSummaryResponse, *models.ErrorLog) {

    wallets, err := u.ledgerWalletRepository.GetAllByLedgerAccountUUID(
        ledgerAccountUUID,
    )
    if err != nil {
        return nil, err
    }

    walletBalances := make([]*responses.WalletBalanceResponse, len(wallets))
    for i, wallet := range wallets {
        walletBalances[i] = &responses.WalletBalanceResponse{
            AvailableBalance: wallet.Balance,
            PendingBalance:   wallet.PendingBalance,
            Currency:         wallet.Currency,
            TotalIncome:      wallet.IncomeAccumulation,
            TotalWithdrawn:   wallet.WithdrawAccumulation,
        }
    }

    return &responses.WalletBalanceSummaryResponse{
        LedgerAccountUUID: ledgerAccountUUID,
        Wallets:           walletBalances,
    }, nil
}
```

---

## Sample API Responses

### Single Wallet Balance

**Request:** `GET /ledger/wallets/{uuid}/balance`

**Response:**
```json
{
    "available_balance": 135800,
    "pending_balance": 50000,
    "currency": "IDR",
    "total_income": 500000,
    "total_withdrawn": 300000
}
```

### Account Balance Summary (Multiple Currencies)

**Request:** `GET /ledger/accounts/{uuid}/balance`

**Response:**
```json
{
    "ledger_account_uuid": "acc-123-456",
    "wallets": [
        {
            "available_balance": 135800,
            "pending_balance": 50000,
            "currency": "IDR",
            "total_income": 500000,
            "total_withdrawn": 300000
        },
        {
            "available_balance": 100,
            "pending_balance": 50,
            "currency": "USD",
            "total_income": 500,
            "total_withdrawn": 300
        }
    ]
}
```

---

## Balance Calculation Examples

### Example 1: After Multiple Payments

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      SCENARIO: Multiple Payments                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Events:                                                                        │
│  1. Payment A: IDR 50,000 (PAID) → pending_balance += 50,000                   │
│  2. Payment B: IDR 30,000 (PAID) → pending_balance += 30,000                   │
│  3. Payment C: IDR 20,000 (PAID) → pending_balance += 20,000                   │
│                                                                                 │
│  Balance Response:                                                              │
│  {                                                                              │
│      "available_balance": 0,                                                    │
│      "pending_balance": 100000,                                                 │
│      "currency": "IDR",                                                         │
│      "total_income": 100000,                                                    │
│      "total_withdrawn": 0                                                       │
│  }                                                                              │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Example 2: After Settlement

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      SCENARIO: After Settlement                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Previous State:                                                                │
│  - pending_balance: 100,000                                                     │
│  - balance: 0                                                                   │
│                                                                                 │
│  Settlement:                                                                    │
│  - gross_amount: 100,000                                                        │
│  - fee (3%): 3,000                                                              │
│  - net_amount: 97,000                                                           │
│                                                                                 │
│  After SettlePendingBalance:                                                    │
│  - pending_balance: 100,000 - 100,000 = 0                                      │
│  - balance: 0 + 97,000 = 97,000                                                │
│                                                                                 │
│  Balance Response:                                                              │
│  {                                                                              │
│      "available_balance": 97000,                                                │
│      "pending_balance": 0,                                                      │
│      "currency": "IDR",                                                         │
│      "total_income": 100000,                                                    │
│      "total_withdrawn": 0                                                       │
│  }                                                                              │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Example 3: After Disbursement

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      SCENARIO: After Disbursement                                │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Previous State:                                                                │
│  - pending_balance: 0                                                           │
│  - balance: 97,000                                                              │
│  - withdraw_accumulation: 0                                                     │
│                                                                                 │
│  Disbursement (KIRIM Singapay):                                                    │
│  - amount: 50,000                                                               │
│  - Status: SUCCESS                                                              │
│                                                                                 │
│  After CompleteDisbursement:                                                   │
│  - balance: 97,000 - 50,000 = 47,000                                           │
│  - withdraw_accumulation: 0 + 50,000 = 50,000                                  │
│                                                                                 │
│  Balance Response:                                                              │
│  {                                                                              │
│      "available_balance": 47000,                                                │
│      "pending_balance": 0,                                                      │
│      "currency": "IDR",                                                         │
│      "total_income": 100000,                                                    │
│      "total_withdrawn": 50000                                                   │
│  }                                                                              │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Example 4: Mixed State (Payments + Settlement + Pending)

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      SCENARIO: Mixed State                                       │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  Timeline:                                                                      │
│  Day 1: Payment A IDR 50,000 (PAID) → pending += 50,000                        │
│  Day 2: Payment B IDR 30,000 (PAID) → pending += 30,000                        │
│  Day 2: Settlement for Payment A (net: 48,500) → pending -= 50,000             │
│                                                    balance += 48,500            │
│  Day 3: Payment C IDR 40,000 (PAID) → pending += 40,000                        │
│  Day 3: Disbursement IDR 20,000 (SUCCESS) → balance -= 20,000                  │
│                                              withdraw_accumulation += 20,000    │
│                                                                                 │
│  Balance Response:                                                              │
│  {                                                                              │
│      "available_balance": 28500,     // 48,500 - 20,000                        │
│      "pending_balance": 70000,       // 30,000 + 40,000 (not yet settled)      │
│      "currency": "IDR",                                                         │
│      "total_income": 120000,         // 50,000 + 30,000 + 40,000               │
│      "total_withdrawn": 20000        // Disbursed to bank                      │
│  }                                                                              │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Balance Interpretation Guide

### For End Users (Dashboard Display)

| Display Label | Value | Meaning |
|---------------|-------|---------|
| "Saldo Tersedia" | `available_balance` | Money you can withdraw now |
| "Saldo Pending" | `pending_balance` | Money waiting for settlement (1-2 days) |
| "Total Pendapatan" | `total_income` | Total money earned (before fees) |
| "Total Ditarik" | `total_withdrawn` | Total money sent to your bank |

### For Developers

| Check | Formula | Use Case |
|-------|---------|----------|
| Can disburse amount? | `available_balance >= amount` | Validate before KIRIM Singapay |
| Total fees paid? | `total_income - (available_balance + pending_balance + total_withdrawn)` | Fee reporting |
| Money in transit? | Query pending disbursements | Show processing withdrawals |

---

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/ledger/wallets/{uuid}/balance` | GET | Get balance for specific wallet |
| `/ledger/accounts/{uuid}/balance` | GET | Get balance for account (specific currency) |
| `/ledger/accounts/{uuid}/balance/summary` | GET | Get balance summary (all currencies) |

---

## Error Handling

### Wallet Not Found

```json
{
    "status_code": 404,
    "message": "Ledger Wallet not found"
}
```

### Account Not Found

```json
{
    "status_code": 404,
    "message": "Ledger Account not found"
}
```

---

## Performance Considerations

1. **No Calculations Required**: Balance values are pre-computed and stored in the wallet
2. **Single Query**: Each balance query requires only one database read
3. **Index Usage**: Queries use indexed fields (uuid, ledger_account_uuid, currency)
4. **Caching Potential**: Balance responses can be cached with short TTL if needed

---

## Consistency Guarantees

1. **Transactional Updates**: All balance modifications happen within database transactions
2. **Atomic Operations**: AddPendingBalance, SettlePendingBalance, and disbursement operations are atomic
3. **No Race Conditions**: Balance checks and deductions happen in the same transaction
4. **Audit Trail**: LedgerTransaction records provide full history of balance changes