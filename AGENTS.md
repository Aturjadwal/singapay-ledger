# AGENTS.md — singapay-ledger

A Go **library** (not a service) providing double-entry bookkeeping and a merchant money cycle
on top of Singapay. It is consumed by `aturjadwal-monoservice`, which owns the HTTP layer, the
workers and the commands; this repo owns the ledger and every table under it.

> The previous AGENTS.md was a pre-implementation design document from the DOKU era that had
> been put through a global find-and-replace. It described tables that were never built. It is
> archived at [`docs/archive/2026-pre-implementation-design.md`](docs/archive/2026-pre-implementation-design.md)
> and must not be used as a reference.

---

## 1. The one rule

**This package owns its tables.** `ledger_accounts`, `product_transactions`,
`payment_requests`, `journals`, `ledger_entries`, `settlement_notifications`, `disbursements`,
`fee_configs`, `ledger_verifications`.

A consumer that needs data from them gets a method on `LedgerClient`. It does not write SQL of
its own against them — that puts the consumer's `SELECT` and this package's state machine on
separate copies of the same rules, and the copies drift.

---

## 2. Layout

```
.                          # package ledger — the public surface
├── ledger.go              # LedgerClient: accounts, balances, withdrawal, platform-fee transfer
├── payment.go             # Payment creation and channel routing
├── webhook.go             # Inbound webhook verification and booking
├── settlement.go          # Settlement inbox + the per-transaction settling pass
├── gateway.go             # PaymentGateway interface and channel mapping
├── domain.go, response.go # Re-exports and response shapes
├── dummy.go               # Local seeder. Not used in production.
├── domain/                # Pure domain types and rules. No SQL, no HTTP.
├── repo/                  # Repository interfaces' PostgreSQL implementations, plus Tx
├── singapay/              # Singapay HTTP client. Knows nothing about the ledger.
├── ledgererr/             # Error codes and typed errors
├── analytics/             # Read-side queries
├── database/
│   ├── schemas/schema.sql # The current shape of the database
│   └── migrations/        # Ordered: 001_*.sql .. 025_*.sql
├── docs/                  # Reference docs — see §7
└── cmd/singapay-smoke/    # Verifies a Singapay connection end to end
```

Dependency direction: `singapay/` and `domain/` know nothing above them; `repo/` depends on
`domain/`; the root package wires them. Nothing in `domain/` imports `repo/`.

---

## 3. The money cycle

```
GeneratePayment      -> product_transactions PENDING + payment_requests (instrument issued)
money-in webhook     -> PENDING -> COMPLETED, funds credited to the PENDING bucket
settlement pass      -> COMPLETED -> SETTLED, PENDING bucket moves to AVAILABLE
Withdraw             -> AVAILABLE debited, disbursement sent
platform-fee sweep   -> platform's cut transferred between Singapay sub-accounts
```

Two balance buckets, per account: `PENDING` (captured, not yet settled) and `AVAILABLE`
(settled, withdrawable). Singapay's own pending/available split matches this, which is why the
model survived the gateway migration intact.

---

## 4. Invariants — break these and money goes wrong

**Ledger entries are insert-only.** There is no update and no delete. A wrong entry is
corrected with a compensating entry and an audit, never by editing. This is why an
approximation is more expensive than an absence anywhere in this codebase.

**Status transitions that move money go through `UpdateStatusIf`.** It is a conditional
`UPDATE` that takes the row lock and reports whether the row actually moved. It is the FIRST
statement in its database transaction, and a caller that gets `false` rolls back having
written nothing. Both money-moving transitions use it:

- `PENDING → COMPLETED` in `webhook.go` (money-in)
- `COMPLETED → SETTLED` in `settlement.go` (`bookSettlement`)

Never stamp the in-memory entity first and save it after. The row in the database is what
decides, and an in-memory answer is a second answer to the same question — the one that is
wrong when a concurrent delivery gets there first.

**`product_transactions.status` is the only source of truth for transaction state.** Nothing
else keeps a parallel copy. `payment_requests` deliberately has no status of its own
(migration 025 removed it) for exactly this reason.

**Underpayment is refused; overpayment is booked and warned about.** A money-in webhook proves
the delivery came from Singapay, not that the payer paid what was asked. Booking a short
payment would credit a seller money that never arrived. An overpayment is legitimate on QRIS,
where `total_amount` includes the payer's tip.

**Idempotency keys are derived, never generated.** `merchant_ref_no` for a platform-fee
transfer comes from the invoice number, so a retry after a timeout presents the same reference
and Singapay replays its original answer instead of moving money twice.

**A redelivered webhook is ordinary traffic, and is answered with success.** Singapay retries.
Answering anything else teaches it to keep retrying a delivery that was already accepted.

---

## 5. Conventions

- **Errors**: wrap with `ledgererr.NewError(code, message, err)`. Codes live in
  `ledgererr/error.go`, grouped by domain. Check with `ledgererr.IsAppError(err, target)`.
- **Domain types embed `*redifu.Record`** (`UUID`, `RandId`, `CreatedAt`, `UpdatedAt`),
  initialised with `redifu.InitRecord`. A type that does NOT embed it is a value type with no
  table — `domain.SettledTransaction` is the example.
- **`redifu.InitRecord` sets pointer time fields to the zero time, not nil.** Constructors that
  have nullable timestamps must set them back to `nil` explicitly, or the database gets
  `0001-01-01`.
- **Repository scans are positional.** Domain types carry no `db` tags, so a column list and
  its scan must be edited together.
- **Money is whole rupiah, `int64`.** An amount that cannot be represented is refused rather
  than rounded.

---

## 6. Commands

There is no Makefile.

```bash
GOTOOLCHAIN=go1.25.5 go build ./...
GOTOOLCHAIN=go1.25.5 go test ./...
GOTOOLCHAIN=go1.25.5 go test . -run TestLedgerEntries_PaymentThroughSettlement -v
gofmt -l .                     # must print nothing
go run ./cmd/singapay-smoke    # needs sandbox credentials
```

**`repo.TestInsertStatementsHaveMatchingArity` is the safety net for schema edits.** It reads
every `INSERT` in `repo/` as text and compares the column list against the placeholder list,
because go-sqlmock never parses SQL and will happily pass a statement that fails in
production. Dropping a column and forgetting its `$n` is the natural way to make that mistake.

**Never run a migration against a live database.** Write the file; a person applies it.

---

## 7. Where the truth lives

| Question | Read |
|---|---|
| What each table is and why | [`docs/ENTITIES.md`](docs/ENTITIES.md) |
| How a payment is created and booked | [`docs/101-payment-execution.md`](docs/101-payment-execution.md) |
| How settlement works, and what was rejected | [`docs/102-settlement-reconciliation.md`](docs/102-settlement-reconciliation.md) |
| How a withdrawal works, and the failure taxonomy | [`docs/103-withdrawal-disbursement.md`](docs/103-withdrawal-disbursement.md) |
| What happens when the real fee differs from the priced one | [`docs/104-fee-mismatch-reconciliation.md`](docs/104-fee-mismatch-reconciliation.md) |
| How Singapay itself behaves (research) | [`docs/105-singapay-migration.md`](docs/105-singapay-migration.md) |
| The public surface and quick start | [`README.md`](README.md) |

When code and a document disagree, the code is right and the document is a bug. Fix it in the
same change.

---

## 8. What this library does NOT do

- **No top-up.** A seller's balance only grows through a settled product sale.
- **No refund handling.** `settlement.refunded` is stored as `NEEDS_REVIEW` and never booked
  automatically: pulling back funds that may already have been withdrawn needs a
  negative-balance policy that does not exist. A business decision, not a coding gap.
- **No second gateway.** `PaymentGateway` is an interface so the money paths can be tested,
  not so another gateway can be plugged in.
- **No HTTP server, worker or scheduler.** Those belong to the consuming service.
