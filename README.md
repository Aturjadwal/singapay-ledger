<div align="center">
<pre style="white-space: pre-wrap; overflow-x: hidden; background: transparent;">
                █████████████           █████████                
                ████████████          ███████████                
                ██████████          █████████████                
                ███████           ███████████████                
                █████████████████████████████████                
                ████████████████         ████████                
                ██████████████           ████████                
                ███████████              ████████                
                █████████                ████████                

</pre>
</div>

# Ledger

**Plug-and-play merchant payment layer, powered by DOKU.**

Accept payments via QRIS, Virtual Account, and more — with built-in balance tracking, settlement reconciliation, and disbursement. No manual ledger wiring required.

> **Every `LedgerClient` operation still runs on DOKU.** Account creation, payments,
> balance inquiry, bank account validation, withdrawals and settlement reconciliation are
> implemented against DOKU APIs and CSV formats.
>
> A complete **Singapay** API client ships alongside in [`singapay/`](singapay/) and is
> usable on its own today — but it is **not wired into `LedgerClient` yet**. Calling it
> directly transacts against Singapay; it does not record anything in the ledger. See
> [Singapay](#singapay-migration-in-progress) below.

---

## What it does

- Records product sales as immutable double-entry ledger entries
- Tracks seller balances across two buckets: `PENDING` (captured, not yet settled) and `AVAILABLE` (settled, withdrawable)
- Reconciles DOKU settlement CSVs — matches CSV rows to product transactions, applies fee adjustments, and moves balances from `PENDING` → `AVAILABLE`
- Handles seller withdrawals via DOKU sub-account payout
- Transfers platform fees to the platform sub-account after settlement

## What it does NOT do

- **No top-up / balance loading** — seller balances only grow through settled product transactions. There is no API to credit a seller's balance directly.
- **No payment gateway abstraction** — all payment, sub-account, and disbursement operations are wired to DOKU APIs only.

---

## Architecture

```
ledger/
├── ledger.go              # LedgerClient — all public operations
├── domain/                # Pure domain types and business rules
│   ├── account.go         # Account (Seller, Platform, PaymentGateway)
│   ├── product_transaction.go  # ProductTransaction + FeeBreakdown
│   ├── ledger_entry.go    # LedgerEntry (immutable), factory functions
│   ├── fee_config.go      # FeeConfig, FeeCalculator
│   ├── settlement_csv.go  # DOKU settlement CSV parser
│   ├── settlement_batch.go
│   └── settlement_item.go
├── repo/                  # Repository interfaces + PostgreSQL implementations
├── singapay/              # Singapay API client — standalone, not yet wired to LedgerClient
├── cmd/singapay-smoke/    # Verifies a Singapay connection end to end
├── docs/                  # Architecture docs and reconciliation flow diagrams
└── analytics/             # Read-side analytics queries
```

Ledger entries are **insert-only** — no row is ever updated or deleted. Balances are always derived by summing entries.

---

## Fee Models

Two fee models control who bears the DOKU gateway fee:

| Model | Customer pays | Seller receives | DOKU fee borne by |
|---|---|---|---|
| `GATEWAY_ON_CUSTOMER` | SellerPrice + PlatformFee + DokuFee | SellerPrice (100%) | Customer |
| `GATEWAY_ON_SELLER` | SellerPrice + PlatformFee | SellerPrice − DokuFee | Seller |

Subscription transactions typically use `GATEWAY_ON_SELLER` with `PlatformFee = 0`.

---

## Usage

```go
import (
    "github.com/21strive/ledger"
    "github.com/21strive/doku/app/usecases"
)

dokuClient := usecases.NewDokuUseCase(...)
client := ledger.NewLedgerClient(db, dokuClient, logger)
```

### Account management

```go
// Register a seller account (also provisions a DOKU sub-account)
account, err := client.CreateAccount(ctx, sellerID, email, name, domain.CurrencyIDR)

// Look up by seller ID
account, err := client.GetAccountBySellerID(ctx, sellerID)
```

### Generating payments

`GeneratePayment` creates a product payment between a buyer and a seller. It calculates fees, calls the DOKU payment API, and saves a `ProductTransaction` + `PaymentRequest` atomically.

```go
resp, err := client.GeneratePayment(ctx, &ledger.GeneratePaymentRequest{
    SellerAccountID: "seller-uuid",
    BuyerAccountID:  "buyer-uuid",
    BuyerName:       "Jane Doe",
    BuyerEmail:      "jane@example.com",
    ProductID:       "prod-123",
    ProductType:     "PHOTO",
    SellerPrice:     100000,       // in smallest currency unit (e.g. IDR cents)
    Currency:        "IDR",
    PaymentChannel:  "QRIS",
    FeeModel:        ledger.FeeModelGatewayOnCustomer,
    Metadata:        map[string]any{"title": "Sunset Photo"},
})
// resp.PaymentURL   — redirect buyer here to complete payment
// resp.TotalCharged — what buyer will pay
// resp.SellerNetAmount — what seller will receive after settlement
```

Two convenience wrappers set the fee model explicitly:

```go
// Customer pays all fees (seller receives 100% of SellerPrice)
resp, err := client.GeneratePaymentGatewayOnCustomer(ctx, req)

// Seller absorbs the gateway fee (customer pays SellerPrice + PlatformFee only)
resp, err := client.GeneratePaymentGatewayOnSeller(ctx, req)
```

### Subscription payments

`GenerateSubscriptionPayment` creates a platform subscription payment. There is no seller — the platform receives all net proceeds. The buyer selects the payment channel via the DOKU Checkout page.

```go
resp, err := client.GenerateSubscriptionPayment(ctx, &ledger.GenerateSubscriptionPaymentRequest{
    BuyerAccountID:    "buyer-uuid",
    BuyerName:         "Jane Doe",
    BuyerEmail:        "jane@example.com",
    ProductID:         "plan-pro",
    SubscriptionPrice: 99000,
    Currency:          "IDR",
    Metadata:          map[string]any{"plan": "pro", "duration_days": 30},
})
// Fee model is always GATEWAY_ON_SELLER: buyer pays SubscriptionPrice, platform absorbs DOKU fee.
```

### Handling payment webhooks

After a buyer completes payment, DOKU sends a notification. Pass the raw request to `HandlePaymentSuccess` — it validates the notification, marks the `ProductTransaction` as completed, and writes the `PENDING` ledger entries for the seller, platform, and DOKU accounts.

```go
err := client.HandlePaymentSuccess(ctx, dokuNotificationRequest)
```

### Fee calculation (dry-run)

Preview the full fee breakdown before creating a payment:

```go
// Normal: multiplier=1, platform fee charged once
resp, err := client.CalculateFeesForCustomer(ctx, 100000, "QRIS", "IDR", 1)
// resp.FeeBreakdown      — full breakdown (SellerPrice, PlatformFee, DokuFee, TotalCharged, SellerNetAmount)
// resp.CheapestPaymentChannel — channel with the lowest DOKU fee for the same seller price

// Skip platform fee: multiplier=0
resp, err = client.CalculateFeesForCustomer(ctx, 100000, "QRIS", "IDR", 0)

// Multiply platform fee (e.g. 2 installment terms): multiplier=2
resp, err = client.CalculateFeesForCustomer(ctx, 100000, "QRIS", "IDR", 2)

// List all supported payment channels and their fee config
configs, err := client.GetPaymentChannelFeeConfigs(ctx)
```

### Merchant balance management

Seller balances are derived entirely from ledger entries — never stored as a mutable field. There are two balance buckets:

| Bucket | When it grows | When it shrinks |
|---|---|---|
| `PENDING` | After `HandlePaymentSuccess` | After `ProcessReconciliation` |
| `AVAILABLE` | After `ProcessReconciliation` | After `Withdraw` |

> **There is no top-up.** The only way to increase a seller's balance is through a completed + settled product sale.

```go
// Read merchant balance
balance, err := client.GetAllBalancesBySellerID(ctx, sellerID)
// balance.PendingBalance   — captured, awaiting settlement CSV
// balance.AvailableBalance — settled, withdrawable

// View pending and settled transactions
earnings, err := client.GetEarnings(ctx, sellerID, cursor, 20, "DESC")
```

### Settlement reconciliation

Settlement is triggered by uploading the DOKU settlement CSV. The reconciliation moves balances from `PENDING` → `AVAILABLE` for every matched seller.

```go
resp, err := client.ProcessReconciliation(ctx, &ledger.ReconciliationRequest{
    CSVReader:      file,
    ReportFileName: "settlement-20260504.csv",
    UploadedBy:     "admin@company.com",
    SettlementDate: time.Now(),
})
```

### Withdrawal

```go
// Validate destination bank account first
valid, err := client.ValidateBankAccount(ctx, &ledger.ValidateBankAccountRequest{
    BankCode:      "BCA",
    AccountNumber: "1234567890",
})

// Disburse from AVAILABLE balance to external bank
resp, err := client.Withdraw(ctx, sellerID, &ledger.WithdrawRequest{
    AccountID:     account.UUID,
    Amount:        500000,
    BankCode:      "BCA",
    AccountNumber: "1234567890",
    AccountName:   "John Doe",
})

// Paginated disbursement history
history, err := client.GetDisbursements(ctx, sellerID, cursor, 20, "DESC")
```

### Seller KYC verification — removed

KYC belongs to the service that owns the user, not to the ledger. The upload
helpers and `SubmitVerification` that used to live here were removed on
2026-08-26, along with this package's dependency on the AWS SDK: nothing the
ledger does touches object storage, so `NewLedgerClient` no longer asks for an
`aws.Config`.

The `ledger_verifications` table and its repository (`repo.Verification()`) are
still here, because the table exists and may hold rows. Nothing writes to it.

---

## Settlement & Reconciliation internals

The reconciliation process:

1. Parses the CSV (DOKU-specific format with 9 metadata rows + data rows)
2. Matches each CSV row to a `ProductTransaction` by invoice number
3. Detects fee mismatches (`ActualDokuFee` from CSV vs `ExpectedDokuFee` recorded at payment time)
4. Applies `FEE_ADJUSTMENT` entries when reconcilable; blocks when not
5. Writes settlement ledger entries atomically: `SETTLEMENT_CLEAR` (debit PENDING) + `SETTLEMENT_NET` (credit AVAILABLE)

See [`docs/104-fee-mismatch-reconciliation.md`](docs/104-fee-mismatch-reconciliation.md) for full fee mismatch rules.

---

## Database

Requires PostgreSQL. Schema is in `schema.sql` (not included in this package — managed by the host application).

Key tables: `accounts`, `product_transactions`, `ledger_entries`, `journals`, `settlement_batches`, `settlement_items`, `fee_configs`.

---

## DOKU Integration Points

| Operation | DOKU API |
|---|---|
| `CreateAccount` | Create sub-account |
| `ValidateBankAccount` | Bank account inquiry + token |
| `Withdraw` | Send payout to sub-account |
| `ProcessPlatformFeeTransfer` | Transfer between sub-accounts |
| `GetBalance` | Get sub-account balance |
| `ProcessReconciliation` | Parses DOKU settlement CSV format |

The **platform** account is not on that list, and that is deliberate. It is provisioned
once per environment by hand — `scripts/doku-subaccount` in aturjadwal-monoservice, then an
inserted `ledger_accounts` row — because a sub-account bound to the wrong email silently
becomes the wrong place for every platform fee. This package only ever reads it.

---

## Singapay (migration in progress)

[`singapay/`](singapay/) is a full client for the Singapay merchant API — the intended
replacement for DOKU. It speaks HTTP and returns Singapay's own shapes; it touches no
database and knows nothing about the ledger domain.

**What is ready**

| Area | Covered |
|---|---|
| Security | access-token, request and webhook signatures (three separate HMAC-SHA512 schemes) |
| Sub-accounts | create, get, list, update |
| Money in | Payment Link, Virtual Account, QRIS, e-wallet |
| Money out | disbursement, check-fee, check-beneficiary, inquiry-status |
| Transfers | between sub-accounts |
| Balances | merchant and per-account |
| Webhooks | parsers for money-in (4 channels), disbursement and settlement |

**What is not**

- `LedgerClient` does not call it. `GeneratePayment`, `HandlePaymentSuccess`, `Withdraw`
  and `ProcessReconciliation` all still go to DOKU, and `NewLedgerClient` still takes a
  DOKU client. A caller wanting payments recorded in the ledger must still use DOKU.
- Reconciliation is unsolved: Singapay has no equivalent of DOKU's settlement CSV.
- Nothing has been verified against a live Singapay environment.

`ledger_accounts` already carries `singapay_account_id` and `singapay_account_number`
(migrations 016/017), and `domain.Account` reads both — Singapay names one sub-account
with two identifiers, and an account transfer accepts only the number.

**Verifying a connection**

```bash
go run ./cmd/singapay-smoke                    # credentials, IP allowlist, token
go run ./cmd/singapay-smoke -step signature    # which X-Timestamp format is accepted
go run ./cmd/singapay-smoke -step va   -account-id 01K9... -amount 10000
go run ./cmd/singapay-smoke -step qris -account-id 01K9... -amount 10000
```

Full mapping of every DOKU call to its Singapay equivalent, plus the open questions:
[105 — Singapay Migration](docs/105-singapay-migration.md).

---

## Environment variables

This package reads no environment itself — `LedgerClient` takes a `*sql.DB` and a gateway
client, both constructed by the host application. The variables below are read by the
gateway clients and by the tooling in `cmd/`.

### Database

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL DSN. Consumed by the host application, which passes the `*sql.DB` to `NewLedgerClient`. |

### DOKU — required today

Read by `github.com/21strive/doku` via `config.InitConfigFromEnv()`.

| Variable | Required | Notes |
|---|---|---|
| `DOKU_API_CLIENT_ID` | yes | |
| `DOKU_API_SECRET_KEY` | yes | |
| `DOKU_API_PRIVATE_KEY` | yes | RSA key for request signing |
| `DOKU_PRINT_CURL` | no | Mirrors outgoing calls to the log as curl, **headers included**. Off in production by default; set explicitly to override either way. |
| `TRANSACTION_FEE_*` | no | Per-channel rates with built-in defaults. Used only by DOKU's own settlement fee calculator — this package's fee maths reads the `fee_configs` table instead. |

### Singapay — required only when using `singapay/`

Read by `singapay.ConfigFromEnv()` / `singapay.NewFromEnv()`, and by
`cmd/singapay-smoke`. Passing a `singapay.Config` directly needs none of them.

| Variable | Required | Notes |
|---|---|---|
| `SINGAPAY_CLIENT_ID` | yes | |
| `SINGAPAY_CLIENT_SECRET` | yes | HMAC key for **every** signature, and the key that verifies inbound webhooks. Never leaves the process. |
| `SINGAPAY_PARTNER_ID` | yes | Merchant API key, sent as `X-PARTNER-ID` |
| `SINGAPAY_PRODUCTION` | no | `true` targets production. Anything else — including unset — stays on sandbox. |
| `SINGAPAY_BASE_URL` | no | Overrides the host entirely. For tests against a stub. |
| `SINGAPAY_TIMESTAMP_FORMAT` | no | `unix` (default) or `iso`. |

Two notes worth reading before deploying:

- **`SINGAPAY_TIMESTAMP_FORMAT` exists because Singapay's documentation contradicts
  itself.** The signing guide says `X-Timestamp` is Unix seconds; the OpenAPI spec for
  the disbursement endpoint says ISO-8601. A wrong guess surfaces only as `SP016`. It is
  an environment variable so flipping it needs no release — `cmd/singapay-smoke -step
  signature` reports which one the server actually accepts.
- **Singapay requires an IP allowlist**, sandbox included. Register every outbound IP,
  workers as well as web, in the merchant dashboard. Requests from elsewhere fail
  `SP017`, not with a network error.

Webhook URLs (`transaction_notif_url`, `disbursement_notif_url`, `settlement_notif_url`)
are configured in the Singapay dashboard, not through environment variables.

---

## Docs

- [101 — Payment Execution](docs/101-payment-execution.md)
- [102 — Settlement & Reconciliation](docs/102-settlement-reconciliation.md)
- [103 — Withdrawal / Disbursement](docs/103-withdrawal-disbursement.md)
- [104 — Fee Mismatch Reconciliation](docs/104-fee-mismatch-reconciliation.md)
- [105 — Singapay Migration](docs/105-singapay-migration.md)