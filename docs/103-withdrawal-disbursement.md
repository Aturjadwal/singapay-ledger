# Withdrawal (Disbursement) — Architecture Diagram

This diagram outlines how a seller withdraws their available balance to a bank account.

```mermaid
sequenceDiagram
    participant Seller
    participant LedgerAPI
    participant LedgerStore
    participant Singapay

    %% Step 1: Quote the fee
    Seller->>LedgerAPI: Withdraw (net amount, bank, account)
    LedgerAPI->>Singapay: check-fee (SWIFT code, net amount)
    Singapay-->>LedgerAPI: transfer_fee
    Note right of LedgerAPI: Singapay's amount is the NET the beneficiary<br/>receives; the fee is charged ON TOP.<br/>A failed quote does not fail the withdrawal.

    %% Step 2: Reserve, under a row lock
    rect rgb(240, 240, 240)
    Note over LedgerAPI, LedgerStore: One transaction, account row locked
    LedgerAPI->>LedgerStore: SELECT ... FOR UPDATE on the account
    LedgerAPI->>LedgerStore: Derive AVAILABLE by summing entries
    alt gross > available
        LedgerStore-->>Seller: ErrInsufficientBalance
    else sufficient
        LedgerAPI->>LedgerStore: Journal (DISBURSEMENT, stage RESERVED)
        LedgerAPI->>LedgerStore: Disbursement (PENDING) + reference_number
        LedgerAPI->>LedgerStore: LedgerEntry (Seller): -(net + fee) AVAILABLE
    end
    end

    %% Step 3: Send
    LedgerAPI->>Singapay: POST disbursement/transfer (reference_number)
    Note right of Singapay: SP000 means the instruction was ACCEPTED,<br/>not that money moved

    %% Step 4: Book the outcome
    alt transaction_status 00 — succeeded
        LedgerAPI->>LedgerStore: Disbursement → COMPLETED
        Note right of LedgerStore: Reservation stands. Nothing further.
    else transaction_status 04/05/06/07 — terminal failure
        LedgerAPI->>LedgerStore: Disbursement → FAILED
        LedgerAPI->>LedgerStore: LedgerEntry (Seller): +(net + fee) AVAILABLE
    else transaction_status 01/02/03 — in flight
        LedgerAPI->>LedgerStore: Disbursement → PROCESSING
        Note right of LedgerStore: Reservation HELD until the webhook lands
    else call failed
        Note over LedgerAPI: Outcome decides, NOT the HTTP status
        LedgerAPI->>LedgerStore: Refused → FAILED + reversal
        LedgerAPI->>LedgerStore: Unknown/Duplicate → stays PENDING, reservation held
    end

    %% Step 5: Async outcome
    rect rgb(240, 240, 240)
    Singapay->>LedgerAPI: POST disbursement_notif_url
    LedgerAPI->>LedgerAPI: Verify signature, resolve by reference_number
    LedgerAPI->>LedgerStore: Book through the same path as step 4
    end
```

## Key concepts

- **Journal**: EventType `DISBURSEMENT`, with a `stage` of `RESERVED`, `SETTLED` or `REVERSED`.
- **Seller entry on request**: `-(net + fee)` from **AVAILABLE**.
- **Seller entry on a known failure**: `+(net + fee)` into **AVAILABLE**.

## Why the order is what it is

**The row and its reference are written before Singapay is called.** If the call came first,
a crash between the payout and the commit would lose the reference — and the payout could
never be asked about again, only judged by hand.

**The balance is reserved, not merely checked.** Balances are derived by summing
`ledger_entries`, so two *different* withdrawals racing each other both pass a bare check and
both pay out. Idempotency does not help: each is a distinct payout with its own reference.
Only holding the money at request time does, and only under a row lock — checking outside the
transaction and writing inside it is the same race with extra steps.

**The reservation covers the fee.** Singapay debits `amount + fee`. Reserving only the amount
leaves the ledger over-stating every seller's balance by the fee on every payout, and the
error is silent: the books stay internally consistent and disagree only with Singapay.

## The failure taxonomy

This is the part that decides whether money can be paid out twice.

Singapay returns **HTTP 400** for `SP001`, `SP002`, `SP004` and `SP005`, and its own
documentation says to call inquiry-status for every one of them because the transfer may
still settle. A "4xx means definite refusal" heuristic would release the reservation on all
four.

| `singapay.Outcome` | Meaning | Reservation |
|---|---|---|
| `OutcomeRefused` | Singapay declined before moving anything | **Released**, disbursement `FAILED` |
| `OutcomeDuplicate` | The reference already exists; the original may have succeeded | **Held** — inquire, never re-send |
| `OutcomeUnknown` | No answer, or an answer that says nothing about the money | **Held** — the only state from which the truth is recoverable |

`RetryDisbursement` resolves the held cases. It **inquires first**: if Singapay knows the
reference, its answer is booked and nothing is sent. Only a reference Singapay has never seen
(`SP009`) is actually transmitted. Any other inquiry failure leaves the question open, and an
open question is not a licence to send money again.

`COMPLETED` and `FAILED` are terminal and are refused outright. A row with no stored
reference is refused too: minting one would look like a protected retry while handing
Singapay a reference it has never seen, so a payout that already went out would go out again.
