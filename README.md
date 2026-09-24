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

**Plug-and-play merchant payment layer, powered by Singapay.**

Accept payments via QRIS, Virtual Account and e-wallet — with built-in balance tracking,
double-entry bookkeeping and bank disbursement. No manual ledger wiring required.

> ### Settlement runs per transaction, not per batch
>
> The whole money cycle runs on Singapay: account provisioning, payments, webhooks, balance
> inquiry, bank account validation, settlement, payouts and platform-fee transfers.
>
> Singapay's settlement webhook announces totals and a date window but **no list of the
> transactions it covered**. Rather than reconstruct that list from the window — which
> depends on two facts Singapay does not document — this ledger treats the webhook as a
> doorbell and asks Singapay about each of its own open invoices directly. See
> [Settlement](#settlement).
>
> **Still open: the refund policy.** `settlement.refunded` is stored for review and never
> acted on automatically, because pulling back funds that may already have been withdrawn
> needs a negative-balance decision this ledger does not have.

---

## What it does

- Records product sales as immutable double-entry ledger entries
- Tracks seller balances across two buckets: `PENDING` (captured, not yet settled) and `AVAILABLE` (settled, withdrawable)
- Handles seller withdrawals via Singapay bank disbursement, reserving the transfer fee along with the amount
- Settles those balances by asking Singapay about each open invoice in turn, and reconciles the fee it actually took
- Transfers platform fees to the platform sub-account after settlement
- Verifies and books Singapay's money-in and money-out webhooks

## What it does NOT do

- **No top-up / balance loading** — seller balances only grow through settled product transactions. There is no API to credit a seller's balance directly.
- **No refund handling** — `settlement.refunded` is stored as `NEEDS_REVIEW` and never booked automatically. See the callout above.
- **No payment gateway abstraction** — every gateway call goes to Singapay. `PaymentGateway` is an interface so the money paths can be tested, not so a second gateway can be plugged in.

---

## Architecture

```
ledger/
├── ledger.go              # LedgerClient — accounts, balances, withdrawal, platform-fee transfer
├── payment.go             # Payment creation and channel routing
├── webhook.go             # Inbound webhook verification and booking
├── settlement.go          # Settlement inbox + the per-transaction settling pass
├── gateway.go             # PaymentGateway interface and channel mapping
├── singapay/              # Singapay API client — HTTP only, knows nothing about the ledger
├── domain/                # Pure domain types and business rules
│   ├── account.go         # Account (Seller, Platform, PaymentGateway)
│   ├── product_transaction.go  # ProductTransaction + FeeBreakdown
│   ├── ledger_entry.go    # LedgerEntry (immutable), factory functions
│   ├── fee_config.go      # FeeConfig, FeeCalculator
│   ├── settlement_notification.go  # The settlement webhook inbox
│   └── settled_transaction.go      # One settled row, normalised across four channels
├── repo/                  # Repository interfaces + PostgreSQL implementations
├── cmd/singapay-smoke/    # Verifies a Singapay connection end to end
├── docs/                  # Architecture docs and flow diagrams
└── analytics/             # Read-side analytics queries
```

Ledger entries are **insert-only** — no row is ever updated or deleted. Balances are always
derived by summing entries.

---

## Fee Models

Two fee models control who bears the Singapay channel fee:

| Model | Customer pays | Seller receives | Gateway fee borne by |
|---|---|---|---|
| `GATEWAY_ON_CUSTOMER` | SellerPrice + PlatformFee + GatewayFee | SellerPrice (100%) | Customer |
| `GATEWAY_ON_SELLER` | SellerPrice + PlatformFee | SellerPrice − GatewayFee | Seller |

Subscription transactions use `GATEWAY_ON_SELLER` with `PlatformFee = 0`.

Singapay does not publish money-in rates through its API — `ListPaymentMethods` returns the
channel catalogue with no prices, because rates are commercial terms. The `fee_configs`
table is therefore the only place the expected fee exists before a transaction settles.

---

## Usage

```go
import (
    "github.com/Aturjadwal/singapay-ledger"
    "github.com/Aturjadwal/singapay-ledger/singapay"
)

gateway, err := singapay.NewFromEnv()
if err != nil {
    return err
}
client := ledger.NewLedgerClient(db, gateway, logger)
```

### Account management

```go
// Register a seller account (also provisions a Singapay sub-account)
account, err := client.CreateAccount(ctx, sellerID, email, name, domain.CurrencyIDR)

// Look up by seller ID, or by the ULID Singapay puts on webhooks
account, err := client.GetAccountBySellerID(ctx, sellerID)
account, err := client.GetAccountBySingapayAccountID(ctx, "01K946KF851RK7FX075GJHBVKF")
```

Two things worth knowing before you call `CreateAccount`:

**Singapay's create endpoint has no idempotency key and no duplicate detection.** It will
happily open a second account with the same name, and nothing in the response tells you it
did. A retry after a timeout can therefore leave an account that holds money and that
nothing references. `CreateAccount` checks its own table first for exactly that reason;
a caller retrying a timeout must check for the account before calling again.

**Singapay names one sub-account with two identifiers, and they are not interchangeable.**
The ULID (`singapay_account_id`) is what every endpoint takes. The 12-digit number
(`singapay_account_number`) is accepted in exactly one place — as the beneficiary of an
account transfer — and the ULID is rejected there. Singapay declares the number nullable,
so an account can arrive without one, and an account with no number **cannot receive a
platform-fee transfer**. `CreateAccount` logs a warning when that happens;
`BackfillSingapayAccountNumber` is the repair.

### Generating payments

`GeneratePayment` calculates fees, issues the payment instrument at Singapay, and saves a
`ProductTransaction` + `PaymentRequest` atomically.

```go
resp, err := client.GeneratePayment(ctx, &ledger.GeneratePaymentRequest{
    SellerAccountID: "seller-uuid",
    BuyerAccountID:  "buyer-uuid",
    BuyerName:       "Jane Doe",
    BuyerEmail:      "jane@example.com", // optional
    ProductID:       "prod-123",
    ProductType:     "PHOTO",
    SellerPrice:     100000,       // whole rupiah
    Currency:        "IDR",
    PaymentChannel:  "QRIS",       // Singapay channel code
    FeeModel:        ledger.FeeModelGatewayOnCustomer,
    Metadata:        map[string]any{"title": "Sunset Photo"},
})
// resp.PaymentURL     — redirect the payer here (VA and QRIS have none; see below)
// resp.PaymentCode    — the VA number, or the QRIS payload to render
// resp.PaymentChannel — the channel actually used
```

**The channel decides which Singapay product issues the payment, and that decision is not
reversible after the fact:**

| `PaymentChannel` | Singapay product | Reports its own fee? |
|---|---|---|
| `QRIS` | QRIS dynamic | yes — with the MDR rate itself |
| `VA_BCA`, `VA_BNI`, `VA_BRI`, `VA_MANDIRI`, … | Virtual account | yes |
| `EWALLET_DANA`, `EWALLET_OVO`, … | E-wallet native | yes |
| *(empty)* | Payment link — the payer chooses | **no, nowhere in the API** |

A payment link exposes no per-transaction fee on the webhook, on the history row, or on any
endpoint. Anything paid through one can never have its fee reconciled, so leaving
`PaymentChannel` empty is a deliberate trade, not a convenience. Name the channel whenever
you know it.

The codes are Singapay's own, from `GET /payment-link-manage/payment-methods`, and no other
spelling is accepted anywhere in the API.

Two convenience wrappers set the fee model explicitly:

```go
resp, err := client.GeneratePaymentGatewayOnCustomer(ctx, req)
resp, err := client.GeneratePaymentGatewayOnSeller(ctx, req)
```

### Reading a payment back

`GetPaymentByInvoiceNumber` returns an issued payment as it stands now, so a payer who
walked away can be handed the same instrument instead of a second one.

```go
payment, err := client.GetPaymentByInvoiceNumber(ctx, "INV-20260910143012-A1B2C3")
// payment.Status      — PENDING, COMPLETED, SETTLED, FAILED, REFUNDED
// payment.PaymentCode — the VA number or QRIS payload to render again
// payment.IsExpired   — the instrument lapsed at the gateway; issue a new one
```

Read `Status` and `IsExpired` together. They answer different questions and neither is
enough alone: `Status` says whether the invoice still wants paying, and only
`product_transactions.status` knows that; `IsExpired` says whether the number on the page
is still alive, and nothing in this library sweeps on it — a lapsed instrument leaves its
transaction `PENDING` forever. Still `PENDING` and not expired is the one case where the
old instrument can be shown again.

### Subscription payments

`GenerateSubscriptionPayment` creates a platform subscription payment. There is no seller —
the platform receives all net proceeds, and the payment is issued against the platform's own
sub-account.

```go
resp, err := client.GenerateSubscriptionPayment(ctx, &ledger.GenerateSubscriptionPaymentRequest{
    BuyerAccountID:    "buyer-uuid",
    BuyerName:         "Jane Doe",
    BuyerEmail:        "jane@example.com", // optional
    ProductID:         "plan-pro",
    SubscriptionPrice: 99000,
    Currency:          "IDR",
    Metadata:          map[string]any{"plan": "pro", "duration_days": 30},
})
```

It always issues a payment link, because the buyer picks the channel. That means a
subscription's fee delta is always zero by construction — see the fee table above.

### Webhooks

Singapay delivers to three URLs, configured in the merchant dashboard. Pass the raw request
to the matching handler; each verifies the HMAC signature **before** reading any field.

```go
// transaction_notif_url — VA, QRIS, e-wallet and payment link all arrive here
body, _ := io.ReadAll(r.Body)
err := client.HandlePaymentSuccess(ctx, singapay.WebhookRequestFromHTTP(r, "/webhooks/singapay/payment", body))

// disbursement_notif_url — the outcome of a payout
err := client.HandleDisbursementNotification(ctx, singapay.WebhookRequestFromHTTP(r, "/webhooks/singapay/payout", body))
```

The `endpoint` argument must match what is registered in the dashboard **exactly** —
Singapay signed that string, not whatever a reverse proxy rewrote it to.

`HandleDisbursementNotification` is not optional. A Singapay payout is asynchronous: the
transfer call answers `SP000` to say the instruction was accepted, and the money may still
be hours from moving or may never move. Without this webhook a seller's balance stays
reserved against a payout that failed until somebody runs the pending sweep by hand.

`settlement_notif_url` is handled by `HandleSettlementNotification`, which stores the delivery and books nothing — see [Settlement](#settlement).

Duplicate deliveries are ordinary traffic, not incidents: Singapay retries, and both
handlers no-op on a transaction or disbursement that is already past the state they book.

### Fee calculation (dry-run)

```go
// Normal: multiplier=1, platform fee charged once
resp, err := client.CalculateFeesForCustomer(ctx, 100000, "QRIS", "IDR", 1)
// resp.FeeBreakdown           — SellerPrice, PlatformFee, GatewayFee, TotalCharged, SellerNetAmount
// resp.CheapestPaymentChannel — the channel with the lowest gateway fee for the same price

// Skip platform fee: multiplier=0. Multiply it (e.g. 2 installment terms): multiplier=2.
configs, err := client.GetPaymentChannelFeeConfigs(ctx)
```

### Merchant balance management

| Bucket | When it grows | When it shrinks |
|---|---|---|
| `PENDING` | After `HandlePaymentSuccess` | After a settlement pass books the transaction |
| `AVAILABLE` | After a settlement pass books the transaction | After `Withdraw` |

> **There is no top-up.** The only way to increase a seller's balance is through a completed
> and settled product sale.

```go
balance, err := client.GetAllBalancesBySellerID(ctx, sellerID)
earnings, err := client.GetEarnings(ctx, sellerID, cursor, 20, "DESC")
```

### Withdrawal

```go
// Validate the destination first. Note: a nil error does NOT mean the account exists —
// Singapay answers HTTP 200 with SP000 for an account that does not. Read IsValid.
valid, err := client.ValidateBankAccount(ctx, &ledger.ValidateBankAccountRequest{
    BankCode:      "BNINIDJA",   // prefer SWIFT; see below
    AccountNumber: "1234567890",
})

resp, err := client.Withdraw(ctx, sellerID, &ledger.WithdrawRequest{
    AccountID:     account.UUID,
    Amount:        500000,       // what the seller asked for = the whole balance debit
    BankCode:      "BNINIDJA",
    AccountNumber: "1234567890",
    AccountName:   "John Doe",
})
// resp.Amount      — 500000, requested and debited
// resp.TransferFee —   4000, DEDUCTED from it, not charged on top
// resp.NetAmount   — 496000, what reaches the beneficiary's bank account

history, err := client.GetDisbursements(ctx, sellerID, cursor, 20, "DESC")
```

**The transfer fee comes out of the withdrawal, not on top of it.** `Amount` is what the
seller asked for and the whole of what their balance moves by; the fee is carved out of it,
so `NetAmount = Amount - TransferFee` is what reaches the bank. Singapay's own API runs the
other way — its disbursement amount is the net and it adds the fee — which is exactly why
the ledger sends the net: that makes Singapay's sub-account debit land on the requested
amount. A withdrawal whose fee would swallow it is refused with
`ErrInvalidDisbursementAmount`.

**Store SWIFT bank codes, not three-digit national codes.** The transfer endpoint accepts
either, but the fee quote (`check-fee`) is a v1.0 endpoint with no v2 counterpart and
accepts SWIFT only. With a three-digit code the quote fails, the fee falls to zero, and the
full requested amount goes out as the net — so the seller receives everything they asked for
and the platform sub-account absorbs the transfer fee. The withdrawal still goes out
(refusing it would be worse), and the failure is logged.

**A failed payout is not the same as a payout that did not happen.** Singapay returns HTTP
400 for `SP001`, `SP002`, `SP004` and `SP005`, and its own documentation says to call
inquiry-status for every one of them, because the transfer may still settle. The ledger
reads Singapay's `Outcome` rather than the HTTP status:

| Outcome | What happens to the reservation |
|---|---|
| `OutcomeRefused` | Released. The disbursement is `FAILED`. |
| `OutcomeDuplicate` | Held. Resolve with `RetryDisbursement`. |
| `OutcomeUnknown` | Held. The money may be on its way. |

```go
// Sweep payouts whose outcome was never learned
stuck, err := client.GetPendingDisbursements(ctx, time.Now().Add(-2*time.Hour), 50)

// Resolve one. This INQUIRES first and only re-sends when Singapay has never seen the
// reference — re-sending a used reference returns SP004, and the original may well have
// succeeded.
resp, err := client.RetryDisbursement(ctx, disbursementID)
```

### Platform fee transfer

```go
result, err := client.ProcessPlatformFeeTransfer(ctx, 50)
```

It finds nothing today: its input is `SETTLED` transactions, and nothing reaches `SETTLED`
while reconciliation is unimplemented. The routine itself is complete and wired to
Singapay's account transfer, so running it on a schedule now is harmless.

The transfer's `merchant_ref_no` is **derived from the invoice number, never generated**.
Singapay's `merchant_ref_no` is unique per merchant across every account movement, and
re-sending a used value returns the original transfer and moves nothing further — which is
what makes a retry safe. A random value per attempt would forfeit that protection without
raising any error.

### Seller KYC verification — removed

KYC belongs to the service that owns the user, not to the ledger. The upload helpers and
`SubmitVerification` were removed on 2026-08-26, along with this package's dependency on the
AWS SDK. The `ledger_verifications` table and `repo.Verification()` remain because the table
exists and may hold rows. Nothing writes to it.

---

## Settlement

Settlement converts a seller's `PENDING` balance into `AVAILABLE`. It runs as a worker pass,
`ProcessSettlementNotifications`, which is safe to call on a schedule forever: on a quiet tick
it does two cheap reads and returns.

**The webhook is a doorbell.** Singapay announces a batch on `settlement_notif_url` with
totals, a settlement method and a date window — and no list of the transactions it covered.
`HandleSettlementNotification` verifies the signature and stores the delivery verbatim in
`settlement_notifications`. It books nothing, because nothing in that payload could justify a
ledger entry.

**The ledger asks about its own invoices.** A pass takes what is still `COMPLETED`, oldest
first, and asks Singapay about each one directly: the channel decides the endpoint, the
payment request decides the key. No window is ever used to select rows, so no window can be
misread.

That matters, because reconstructing a batch from its window would depend on two things
Singapay does not document, each with a wrong answer that looks plausible: what timezone its
offsetless window text is in (`"26 Dec 2025 13:35:45"`), and which of a QRIS transaction's two
settlement timestamps the window keys on. The per-transaction path does not ask either.

**A pass also triggers on age.** If no notification arrives — a lost delivery, an allowlist
change, retries exhausted — a pass runs anyway once the oldest unsettled transaction has been
waiting longer than `SettlementFloorAge` (24 hours). Without that, one dropped webhook would
strand money silently. The number to alarm on is `OldestAwaitingAge`, which climbs whenever
settlement stops working even if every pass reports success.

**Booking is guarded by a compare-and-set.** Each settlement happens in one database
transaction whose first statement moves the row `COMPLETED → SETTLED` conditionally. Two
passes racing on one transaction produce one settlement, not two sets of insert-only entries.
Where the fee Singapay actually took differs from the one priced, the difference is absorbed
per the rules in [`docs/104`](./docs/104-fee-mismatch-reconciliation.md); where it cannot be
absorbed the transaction is left `COMPLETED` for a person.

Full detail: [`docs/102-settlement-reconciliation.md`](./docs/102-settlement-reconciliation.md).

**Still open: `settlement.refunded`.** It can pull back funds that have already become
`AVAILABLE` and may already have been withdrawn. This ledger has no negative-balance policy,
so refund deliveries are stored as `NEEDS_REVIEW` and left for a person. That is a business
decision, not a coding gap.

---

## Database

Requires PostgreSQL. Schema is in [`database/schemas/schema.sql`](database/schemas/schema.sql);
migrations are in [`database/migrations/`](database/migrations/).

Key tables: `ledger_accounts`, `product_transactions`, `payment_requests`, `ledger_entries`,
`journals`, `settlement_notifications`, `fee_configs`, `disbursements`.

Removing the batch reconciler's tables is a **two-phase migration**, because
`payment_requests.status` is `NOT NULL` and the new code no longer supplies it:

1. [`025_settlement_cleanup_expand.sql`](database/migrations/025_settlement_cleanup_expand.sql)
   — makes `status` nullable. Apply it **before** deploying. Reversible, drops nothing, and
   lets old and new code run side by side through a rolling deploy.
2. [`026_settlement_cleanup_contract.sql`](database/migrations/026_settlement_cleanup_contract.sql)
   — drops `settlement_batches`, `settlement_items`, `reconciliation_discrepancies` and the
   three `payment_requests` columns. Apply it **after** the rollout is complete and settled.
   **Irreversible** — read its first section first.

### Migrating an existing database

[`018_migrate_to_singapay.sql`](database/migrations/018_migrate_to_singapay.sql) renames the
gateway columns and adds the ones Singapay needs.
[`019_analytics_singapay_account_id.sql`](database/migrations/019_analytics_singapay_account_id.sql)
does the same for the analytics database.

**018 drops `ledger_accounts.doku_subaccount_id`, and the values cannot be recovered.**
Archive the column first if the environment ever transacted through the previous gateway —
the migration file carries the exact `CREATE TABLE ... AS SELECT` to run. It is how a
historical settlement or payout gets attributed to an account months from now.

**Neither migration populates `singapay_account_id`.** There is no derivation from an old
sub-account id to a Singapay ULID — they are different accounts at different companies.
Every seller has to be provisioned at Singapay, and the platform account created by hand with
both its ULID and its 12-digit number inserted. Until an account carries a ULID it can
neither take a payment nor pay out, and the code refuses both explicitly.

---

## Singapay Integration Points

| Operation | Singapay API |
|---|---|
| `CreateAccount` | `POST /api/v1.0/accounts` |
| `GeneratePayment` | virtual account, QRIS, e-wallet, or payment link — by channel |
| `HandlePaymentSuccess` | `transaction_notif_url` webhook |
| `ValidateBankAccount` | `POST /api/v2.0/disbursement/check-beneficiary` |
| `Withdraw` | `POST /api/v2.0/disbursement/check-fee` then `.../transfer` |
| `RetryDisbursement` | `POST /api/v2.0/disbursement/inquiry-status` |
| `HandleDisbursementNotification` | `disbursement_notif_url` webhook |
| `ProcessPlatformFeeTransfer` | `POST /api/v1.0/account-transfer/{id}/transfer` |
| `GetBalance` (verification) | `GET /api/v1.0/balance-inquiry/{id}` |

Singapay versions each module separately — accounts, balances and account transfer exist only
at v1.0, disbursement only at v2.0. There is no "API v2" that supersedes v1; mixing is
expected.

The **platform** account is not on that list, and that is deliberate. It is provisioned once
per environment by hand, because Singapay's create endpoint has no duplicate check at all: a
caller with a stale constant would quietly open a *second* sub-account and route every
platform fee into one nobody watches, without raising a single error. This package only ever
reads it.

---

## Environment variables

This package reads no environment itself — `LedgerClient` takes a `*sql.DB` and a
`PaymentGateway`, both constructed by the host application. The variables below are read by
`singapay.ConfigFromEnv()` and by the tooling in `cmd/`.

### Database

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL DSN. Consumed by the host application, which passes the `*sql.DB` to `NewLedgerClient`. |

### Singapay

Read by `singapay.ConfigFromEnv()` / `singapay.NewFromEnv()`, and by `cmd/singapay-smoke`.
Passing a `singapay.Config` directly needs none of them.

| Variable | Required | Notes |
|---|---|---|
| `SINGAPAY_CLIENT_ID` | yes | |
| `SINGAPAY_CLIENT_SECRET` | yes | HMAC key for every **outbound** signature. Never leaves the process. |
| `SINGAPAY_PARTNER_ID` | yes | Merchant API key, sent as `X-PARTNER-ID`. The dashboard labels it as a merchant or API key, not a "partner id". |
| `SINGAPAY_WEBHOOK_KEY` | no | HMAC key for verifying **inbound** webhooks. Unset means the client secret. See below. |
| `SINGAPAY_PRODUCTION` | no | `true` targets production. Anything else — including unset — stays on sandbox. |
| `SINGAPAY_BASE_URL` | no | Overrides the host entirely. For tests against a stub. |
| `SINGAPAY_TIMESTAMP_FORMAT` | no | `unix` (default) or `iso`. |

Sandbox is the default on purpose: an unset or misspelled variable must not move real money.
`SINGAPAY_PRODUCTION=yes` is an error rather than a silent `false` — that is exactly how a
production deploy ends up pointed at sandbox.

Three notes worth reading before deploying:

- **`SINGAPAY_TIMESTAMP_FORMAT` exists because Singapay's documentation contradicts itself.**
  The signing guide says `X-Timestamp` is Unix seconds; the OpenAPI spec for the disbursement
  endpoint says ISO-8601. A wrong guess surfaces only as `SP016`. It is an environment
  variable so flipping it needs no release — `go run ./cmd/singapay-smoke -step signature`
  reports which one the server actually accepts.
- **Singapay requires an IP allowlist**, sandbox included. Register every outbound IP —
  workers as well as web — in the merchant dashboard. Requests from elsewhere fail `SP017`,
  not with a network error, so they are easy to misdiagnose.
- **Whitespace is trimmed from every value.** A newline from a secrets mount would otherwise
  end up in the HMAC key and fail every signature with no useful clue.
- **`SINGAPAY_WEBHOOK_KEY` exists because it is not settled which secret signs a callback.**
  The API documentation describes one `client_secret` used for everything, but the merchant
  dashboard also issues something it calls an HMAC validation key. Only one of them verifies
  a real delivery, and the wrong choice fails in a misleading way: every callback is
  rejected for a signature mismatch, which reads like a canonicalisation bug and sends you
  looking at JSON encoding rather than at credentials.

  Leaving it unset keeps today's behaviour exactly. To settle it, capture one real delivery
  and replay it:

  ```bash
  go run ./cmd/singapay-smoke -step verify-webhook \
      -webhook-body ./delivery.json \
      -webhook-endpoint /singapay/notification \
      -webhook-signature "<X-Signature>" \
      -webhook-timestamp "<X-Timestamp>" \
      -webhook-authorization "<Authorization>"
  ```

  It tries each candidate against the untouched bytes and names the one that matches.
  Nothing is sent anywhere. If neither matches, the callback signature *scheme* differs
  from the request scheme and no key will fix it — which the output says, so the search
  does not turn into a hunt for a third secret.

Webhook URLs (`transaction_notif_url`, `disbursement_notif_url`, `settlement_notif_url`) are
configured in the Singapay dashboard, not through environment variables.

### Verifying a connection

```bash
go run ./cmd/singapay-smoke                    # credentials, IP allowlist, token
go run ./cmd/singapay-smoke -step signature    # which X-Timestamp format is accepted
go run ./cmd/singapay-smoke -step va   -account-id 01K9... -amount 10000
go run ./cmd/singapay-smoke -step qris -account-id 01K9... -amount 10000
```

---

## Docs

- [101 — Payment Execution](docs/101-payment-execution.md)
- [102 — Settlement & Reconciliation](docs/102-settlement-reconciliation.md)
- [103 — Withdrawal / Disbursement](docs/103-withdrawal-disbursement.md)
- [104 — Fee Mismatch Reconciliation](docs/104-fee-mismatch-reconciliation.md)
- [105 — Singapay Migration](docs/105-singapay-migration.md)
