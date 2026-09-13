# Testing guide

156 tests across five packages. No database and no network: every test runs with `go test ./...`.

```bash
GOTOOLCHAIN=go1.25.5 go test ./...
GOTOOLCHAIN=go1.25.5 go test . -run TestLedgerEntries_PaymentThroughSettlement -v
GOTOOLCHAIN=go1.25.5 go test ./repo/ -run TestInsertStatementsHaveMatchingArity
gofmt -l .    # must print nothing
```

---

## Where the tests are, and what each layer proves

| Package | Files | What it covers |
|---|---|---|
| `.` (root) | `ledger_test.go`, `settlement_fee_test.go`, `webhook_test.go`, `balance_reservation_test.go`, `payout_idempotency_test.go`, `payout_request_log_test.go` | The money paths, against in-memory fakes |
| `domain/` | `disbursement_test.go`, `fee_config_test.go` | Pure rules: state transitions, fee arithmetic |
| `repo/` | `insert_arity_test.go`, `postgres_account_test.go`, `postgres_disbursement_test.go` | SQL, against `go-sqlmock` |
| `singapay/` | 8 files | The HTTP client: signatures, amounts, error classification, webhook parsing |
| `ledgererr/` | `error_test.go` | Error wrapping and code matching |

---

## The fakes

`fakes_test.go` provides in-memory implementations of every repository, assembled by
`NewFakeRepositoryProvider()`. Two compile-time assertions keep them honest:

```go
var _ repo.RepositoryProvider = (*FakeRepositoryProvider)(nil)
var _ repo.Tx                 = (*FakeRepositoryProvider)(nil)
```

Add a method to a repository interface and the fakes stop compiling, which is the point.

`NewFakeTransactionProvider(fakes)` satisfies `repo.TransactionProvider` by handing back the
same fakes. **It provides no transaction semantics** — no rollback, no isolation. A test that
needs to prove rollback behaviour cannot use it.

`FakeProductTransactionRepository.UpdateStatusIf` is the fake's compare-and-set. It is not
concurrency-safe, and it exposes a `beforeCAS` hook so a test can simulate another writer
winning the race without needing real concurrency.

---

## Three tests worth knowing about

### `TestInsertStatementsHaveMatchingArity` (`repo/`)

The most valuable test in the repository, and the least obvious. It reads every `INSERT` in
`repo/` **as text** and compares the column list against the placeholder list.

It exists because `go-sqlmock` never parses the SQL it is handed — it regex-matches the query
text and compares the argument list. An `INSERT` whose `VALUES` carries one more placeholder
than its column list passes every other test and then fails in production with
`INSERT has more expressions than target columns`. That is not hypothetical: dropping
`doku_subaccount_id` in the Singapay migration left `VALUES` at `$14` against 13 columns, and
the first seller to reach a paid booking hit it — after their Singapay sub-account had already
been created.

**Run it whenever you add or remove a column.**

### `TestLedgerEntries_PaymentThroughSettlement` (root)

Walks one sale end to end and asserts the invariant the whole ledger exists for: after
settlement, what payment put into `PENDING` nets to zero, and `AVAILABLE` holds exactly the
settled amount. It drives the real `bookSettlement`, so it also covers the
`COMPLETED → SETTLED` compare-and-set and the settled fee figures the platform-fee transfer
later reads.

### `TestResolveFeeAdjustment` (root, `settlement_fee_test.go`)

Eight cases over the fee rules in [`docs/104`](docs/104-fee-mismatch-reconciliation.md),
including both conditions that must **block** rather than clamp: the platform would owe more
than it ever charged, or the seller would receive less than nothing. A clamped answer there
looks reasonable and quietly moves the wrong amount of money.

---

## Conventions

- Assert on behaviour, not on how it was reached. `testify` `require` for preconditions
  (stop the test), `assert` for the claims being made (report them all).
- Money in tests is whole rupiah `int64`, like everywhere else.
- Arithmetic is written out in the test — `50000 - 4995` rather than a bare `45005` — so a
  failure says which rule broke rather than only that a number moved.
- A test that needs a `LedgerClient` builds one directly with the fakes:

  ```go
  client := &LedgerClient{
      txProvider:   NewFakeTransactionProvider(fakes),
      repoProvider: fakes,
      logger:       testLogger(),
      gateway:      gw,   // only when the path calls Singapay
  }
  ```

---

## What is not covered

- **No real database.** `repo/` uses `go-sqlmock`, which checks the SQL text and arguments,
  not that PostgreSQL accepts them. Constraints, triggers and types are unproven here.
- **No real transaction semantics.** See the fakes note above. Rollback and isolation are
  reasoned about, not exercised.
- **No real concurrency.** Races on `UpdateStatusIf` are simulated through the `beforeCAS`
  hook, not run in parallel.
- **No live gateway.** `cmd/singapay-smoke` is the only thing that talks to Singapay, and it
  needs sandbox credentials.
