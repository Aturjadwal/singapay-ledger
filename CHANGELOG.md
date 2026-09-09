# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Changed — DOKU replaced by Singapay

The payment gateway is now Singapay throughout. `github.com/21strive/doku` is no longer a
dependency, and no DOKU-specific type, column, constant or code path remains.

**Constructor.** `NewLedgerClient` takes a `PaymentGateway` instead of a
`usecases.DokuUseCaseInterface`. `*singapay.Client` satisfies it.

```go
// Before
dokuClient := usecases.NewDokuUseCase(...)
client := ledger.NewLedgerClient(db, dokuClient, logger)

// After
gateway, err := singapay.NewFromEnv()
client := ledger.NewLedgerClient(db, gateway, logger)
```

`analytics.NewLedgerAnalyticsClient` drops its gateway argument entirely — it needed one
only to read the supported-bank catalogue, which Singapay does not publish. `dim_bank` is
now derived from observed disbursements.

**Payment webhooks replace the DOKU notification.** `HandlePaymentSuccess` takes a
`singapay.WebhookRequest` instead of a `*requests.DokuNotificationRequest`, and verifies the
HMAC signature before reading any field. `HandleDisbursementNotification` is new and is
**required**: a Singapay payout is asynchronous, so without it a seller's balance stays
reserved against a payout that already failed.

**`CreateAccount` takes no email.** The signature is now
`CreateAccount(ctx, accountID, name string, currency)` — the `email` argument is gone, and
`validateSubAccountEmail` with it.

```go
// Before
client.CreateAccount(ctx, seller.UUID, seller.Email, seller.Name, domain.CurrencyIDR)
// After
client.CreateAccount(ctx, seller.UUID, seller.Name, domain.CurrencyIDR)
```

The email had one destination, `invite_members`, and Singapay rejects any address there
that is not already a member of the calling merchant — `http 422: One or more emails do not
belong to a member of this merchant`. A seller's own address never is, and there is no API
for adding a merchant member, so the field cannot carry a seller at all. Because this call
sits inside a seller's **first paid booking**, sending it failed that booking outright with
a 500. Sellers get no Singapay dashboard access; that is what an `owned` sub-account has
always meant here.

**Payment channels are Singapay's codes.** `QRIS`, `VA_BCA`, `EWALLET_DANA` — from
`GET /payment-link-manage/payment-methods`. `VIRTUAL_ACCOUNT_MANDIRI` and the other DOKU
spellings are not accepted anywhere in the API. The channel now selects the product that
issues the payment; an empty channel issues a payment link, which reports no per-transaction
fee and so cannot be fee-reconciled.

**Withdrawal reserves the transfer fee.** Singapay's disbursement amount is the NET the
beneficiary receives and the fee is charged on top, so the reservation is now
`Amount + TransferFee`, quoted with `check-fee` before the payout is sent. Reserving only
the net drifted from Singapay by the fee on every payout, silently.

**A failed payout is decided by `Outcome`, not by the HTTP status.** Singapay answers HTTP
400 for `SP001`, `SP002`, `SP004` and `SP005` and documents every one of them as "call
inquiry-status". The previous 4xx-means-refused heuristic would release a reservation for a
payout that may still settle — which is how a payout gets made twice.

**`RetryDisbursement` inquires before it re-sends,** and only transmits a reference Singapay
has never seen.

**Renames.** `FeeBreakdown.DokuFee` → `GatewayFee`; `FeeConfigTypeDoku` (`"DOKU"`) →
`FeeConfigTypeGateway` (`"GATEWAY"`); `NewDokuFeeSettlementEntry` →
`NewGatewayFeeSettlementEntry`; `ledgererr.CodeDokuAPIError` → `CodeGatewayAPIError`;
`Account.DokuSubAccountID` removed in favour of `SingapayAccountID` +
`SingapayAccountNumber`; `GetAccountByDokuSubAccountID` removed
(`GetAccountBySingapayAccountID` replaces it).

New error codes: `CodeGatewayOutcomeUnknown`, `CodeWebhookVerificationFailed`,
`CodeReconciliationNotImplemented`.

**Migrations.** `018_migrate_to_singapay.sql` (ledger DB) and
`019_analytics_singapay_account_id.sql` (analytics DB). **018 drops
`ledger_accounts.doku_subaccount_id` irreversibly** — archive it first; the file carries the
statement to run. Neither migration populates `singapay_account_id`: there is no derivation
from a DOKU sub-account to a Singapay ULID, so every account must be provisioned at Singapay.

### Removed

- The DOKU settlement CSV parser (`domain.DokuSettlementCSVParser`) and everything that read
  a CSV. Singapay publishes no settlement file.
- `LedgerClient.FilterIngestedReportFiles`, replaced by `FilterIngestedSettlements`, which
  keys on the gateway settlement id rather than an object-storage filename.

### Not implemented

**`ProcessReconciliation` returns `ErrReconciliationNotImplemented` and books nothing.** It
is the step that converts `PENDING` to `AVAILABLE`, so nothing new becomes withdrawable until
it lands, and `ProcessPlatformFeeTransfer` finds no work because nothing reaches `SETTLED`.

It is absent rather than approximated because ledger entries are insert-only: a settlement
booked on a wrong assumption cannot be undone, only compensated with an audit. Four questions
need answering against a live sandbox first — the settlement window's timezone and
boundaries, which settlement event it keys on, whether the per-account reconstruction scales,
and what `settlement.refunded` should do when it pulls back money already withdrawn. See
`reconciliation.go` and the README.

## [Earlier]

### Changed

- `CalculateFeesWithModel` removed and replaced by `CalculateFeesForCustomer`.
  - Fee model hardcoded to `GATEWAY_ON_CUSTOMER` (customer pays all fees).
  - Accepts `platformFeeMultiplier int`:
    - `0` → skip platform fee entirely
    - `1` → normal platform fee, no multiplication
    - `>1` → platform fee multiplied by the given value (e.g. installment with 2 due terms → `2`)
  - The gateway fee is never multiplied regardless of the multiplier value.

**Before:**
```go
resp, err := client.CalculateFeesWithModel(ctx, 100000, "QRIS", "IDR", domain.FeeModelGatewayOnCustomer)
```

**After:**
```go
resp, err := client.CalculateFeesForCustomer(ctx, 100000, "QRIS", "IDR", 1)
```
