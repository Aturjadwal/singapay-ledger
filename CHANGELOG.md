# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Fixed — the disbursement fee comes out of the withdrawal, not on top of it

**Breaking.** `disbursements.amount` changes meaning, `WithdrawResponse` gains a field, and
migration `028` rewrites existing rows. Read it before applying — it is not idempotent.

A seller asking to withdraw Rp 15.000 against a Rp 3.000 transfer fee had **Rp 18.000**
taken off their balance and received Rp 15.000. The requested amount was treated as the net
the beneficiary receives, and the fee was reserved on top of it. So the fee was an extra
charge beside the withdrawal, and a seller could never withdraw their whole balance: the
last Rp 3.000 was always out of reach.

The requested amount is now the whole of what a withdrawal costs. The fee is carved out of
it:

```
  requested (debited, reserved)   15.000
  − transfer fee (quoted)          3.000
  = net (sent, received)          12.000
```

Singapay's own arithmetic is unchanged and still runs the other way — its disbursement
amount is the net and it adds the fee on top — which is exactly why the ledger now sends the
net. That makes Singapay's sub-account debit land on the requested amount, so the
reservation and the gateway agree on one number instead of two.

```go
// Before — Amount was the net; TransferFee was charged on top of it.
resp.Amount      // 15000, what the beneficiary receives
resp.TransferFee // 3000, charged on top: balance moved 18000

// After — Amount is the request and the whole balance movement.
resp.Amount      // 15000, requested and debited
resp.TransferFee // 3000, deducted from it
resp.NetAmount   // 12000, what the beneficiary receives
```

What changed, concretely:

- `Disbursement.Amount` is the requested amount; `Disbursement.NetAmount()` is what is sent
  and received. `GatewayFee` is a deduction, not an addition.
- The reservation, its reversal and `total_withdrawal_amount` all move by the requested
  amount. They were `amount + fee`.
- `executePayout` sends `NetAmount()`. It sent `Amount`.
- A withdrawal whose fee would swallow it — a net of zero or less — is refused with
  `ErrInvalidDisbursementAmount`, before anything is reserved or any row is written.
- A fee that cannot be quoted still does not fail the withdrawal, but the error now falls
  the other way: the full request goes out as the net and the **platform** absorbs the
  transfer fee. The seller is never short-changed by a quote that failed.
- Journal metadata records `net_amount` where it recorded `gross_amount`.

`quotePayoutFee` quotes against the requested amount rather than the net it is about to
compute. That is correct only because Indonesian payout fees are flat per destination
(Rp 3.000 to a bank, Rp 2.500 to a wallet — migration 020); a percentage fee would have to
be solved for instead, and the comment there says so.

Migration `028` folds `gateway_fee` into `amount` for every existing row, because for an old
row `amount + gateway_fee` is exactly the debit its ledger entry already carries. Without
it, reversals would under-release by the fee, retries would short-pay the beneficiary, and
every historical payout would read as cheaper than it was.

### Fixed — the settled gateway fee is read in sen, and the platform balances it

**Breaking.** Field names and one field type change on exported types.

Singapay reports a money-in fee with two decimals: a QRIS fee of `119.84` is a real figure.
`readSettledTransaction` filled `domain.SettledTransaction` by calling `.Minor()` on those
amounts — sen — for virtual account, QRIS and e-wallet, and with whole rupiah for payment
link. The fields were named `GrossAmount` / `NetAmount` / `Fee` and carried no unit, so
`resolveFeeAdjustment` computed `settled.Fee - tx.Fee.GatewayFee`: sen minus rupiah. A real
fee of Rp120 read as `12000` against an estimate of `120` gave a delta of `11880`.

Most channels blocked loudly — the platform fee went negative and the settlement wrote
nothing — so the damage accumulated as an unsettled backlog rather than as wrong entries.
Payment link was unaffected; its delta is zero by construction.

```go
// Before
type SettledTransaction struct {
    GrossAmount int64
    NetAmount   int64
    Fee         int64
}

// After — the suffix is the fix; a compiler cannot catch a rupiah figure
// assigned to a sen field when neither name says which is which.
type SettledTransaction struct {
    GrossMinor int64
    NetMinor   int64
    FeeMinor   int64
}
```

**The seller is now paid what they were priced, unconditionally.** A surplus used to be
credited to the seller, so a seller's net moved by a few sen depending on what Singapay
charged. The platform sub-account is now the balancing account in both directions:

```
platformAdjustment = estimatedGatewayFee - actualGatewayFee
```

This is what holds the invariant the design exists for — a seller's Singapay sub-account
balance equals their ledger balance — because the only two things that leave that
sub-account are Singapay's own deduction and the platform fee sweep, and the sweep now
moves the balanced figure rather than the one quoted at checkout.

**`TransferRequest.Amount` is now an `Amount`, not an `int64`.** Singapay's account-transfer
endpoint types its amount as a number with decimals and returns it as a string to preserve
precision, so the fraction moves with the transfer. Amounts below the Rp1 minimum the
endpoint enforces are reported as failures rather than marked transferred.

```go
// Before
singapay.TransferRequest{Amount: platformFee}

// After
singapay.TransferRequest{Amount: singapay.NewAmountFromMinor(platformFeeMinor, "IDR")}
```

**`PlatformFeeTransferError.PlatformFee` and `PlatformFeeTransferSuccess.PlatformFee` are
now `PlatformFeeMinor`,** in sen.

**`SaveSettledFees` takes a third argument,** `residualMinor`.

**The platform's two settlement legs may now differ, and that difference is the fee delta.**
The separate `FEE_ADJUSTMENT` entry is gone — booking the delta both as a gap between the
legs and as its own entry counted it twice. The entry type stays in the `CHECK` constraint
and in `domain.EntryType`, because `ledger_entries` is insert-only and historical rows carry
it.

`ledger_entries.amount` stays whole rupiah. The sub-rupiah part it cannot carry is recorded
per transaction in `platform_residual`, so the platform's ledger balance and its Singapay
balance reconcile to `SUM(platform_residual)` — a number that can be queried, not a drift.
The seller's two balances agree exactly.

Also fixes the money-in webhook's early warning, which read the fee through `Amount.Rupiah()`
and so fell through silently in exactly the case worth warning about: a fractional fee.

**Migration 027** converts `settled_platform_fee` and `settled_gateway_fee` to sen and adds
`platform_residual`. It is channel-aware — only payment-link rows held rupiah — and the
columns keep their names, so a build that predates it reads sen as rupiah. **Stop the
settlement worker and the platform fee transfer before applying it, not after.**

Full rules: [docs/104-fee-mismatch-reconciliation.md](docs/104-fee-mismatch-reconciliation.md).

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
