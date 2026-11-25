# 106 — Platform Balance Reconciliation

## Overview

The platform account keeps its money twice: as ledger entries and in its Singapay sub-account.
The two are never simply equal, and v0.13.0 makes the difference explicit instead of leaving it
as a drift nobody can account for. Everything here is in [`platform_reconciliation.go`](../platform_reconciliation.go)
and [`domain/platform_reconciliation.go`](../domain/platform_reconciliation.go); the tables are
migration [034](../database/migrations/034_platform_reconciliation.sql).

```
unexplained = S − L − K + B                                  (all in sen)

S  the platform sub-account's available balance at Singapay
L  the platform's AVAILABLE balance in the ledger: Σ of its entries, × 100
K  the residual pool:  Σ platform_residual of swept sellers' rows
                     + Σ platform_adjustments.residual_minor
                     − Σ platform_adjustments.amount × 100 of the ROUNDING rows
B  platform fee settled but not swept yet, at the whole rupiah the ledger holds for it
   (its SETTLEMENT_NET and FEE_ADJUSTMENT entries), × 100
```

Every term is recomputed from rows that never change once written — entries, swept rows,
adjustments — never kept as a running counter, so the figure of any moment can be produced
again and argued about.

---

## Why the two balances differ

| Cause | Direction | Handled by |
|---|---|---|
| Residual sen of every sweep: the sweep moves `4000.16`, the entry records `4000` | Singapay > ledger | `K`, then `BookPlatformResidual` books each whole rupiah |
| Fee settled, sweep not done or failed, card fee recomputation held | ledger > Singapay | `B`: never booked, it goes away when the sweep succeeds |
| Money the ledger never saw (CSV7MW: a staging run on production credentials, swept into the same sub-account) | either | an alert, then an admin books it (`BookPlatformDifference`) with a reason |
| The DOKU era: in the ledger, never in the Singapay sub-account | ledger > Singapay | `ArchiveToReserve`, once |
| A payout in flight: reserved in the ledger, not yet debited at Singapay | ledger < Singapay for a while | the check is skipped |

Normal operation keeps the figure at exactly 0:

- a **settlement** moves L and B by the same rupiah (an unswept fee is in both);
- a **sweep** moves S by the fee in sen, B by its rupiah and K by its sen;
- a **rounding** moves L and K by the same rupiah;
- a **booked difference** moves L by its rupiah and K by its sen, which is the whole figure.

So anything that is not 0 is something none of these explain, and a person has to look at it.

### Why B is the ledger's rupiah, not the sweep's sen

An unswept fee of 4000.72 is 4000 in L (its entry) and nothing in S. Its 72 sen are in neither S
nor K: K only counts swept rows. Adding the sen-exact 400072 back would leave 72 sen unexplained
every day until the row is swept, a false alert for each fee with sen whose sweep failed or whose
card recomputation was held. Adding the 400000 the ledger holds explains it exactly.

### Why only swept rows' residual is in K

The sen of an unswept fee are still in the seller's sub-account; they are not the platform's
money at Singapay yet. They join K the moment the sweep marks the row.

---

## The three operations

### `BookPlatformResidual` — automatic, after every sweep

Books `floor(K / 100)` rupiah as one `ROUNDING` adjustment (journal `RECONCILIATION`, entry
`RECONCILIATION` on the platform's AVAILABLE, sourced on the adjustment), when K ≥ 100. The sen
under Rp1 are carried to the next run. The rounding names the swept transactions whose sen
entered the pool since the previous rounding (`source_transaction_uuids`).

It takes the platform account's row lock (`SELECT … FOR UPDATE`) and recomputes K inside the
transaction, so two processes running it at once book once: the second reads the pool the
first emptied.

### `CheckPlatformBalance` — automatic, after the rounding

1. One statement reads L, K, B and the platform payouts in flight.
2. A payout in `PENDING`/`PROCESSING` skips the check (`PAYOUT_IN_FLIGHT`).
3. Singapay's balance inquiry (a read). A failure skips the check (`BALANCE_READ_FAILED`).
4. The statement again: a payout that started skips; any other change re-reads the balance, up
   to three times, then skips (`LEDGER_MOVED`).
5. `MATCHED` at exactly 0, `MISMATCH` otherwise. The check is recorded in either case.

It never books anything. The consuming service alerts on `MISMATCH` (at most once a day).

A sweep running in another process at the same moment is the one case the double read cannot
see: Singapay has moved the fee and the row is not marked yet. The check would report the fee
as unexplained until the next one. Run the check after the sweep, in the same process.

### `BookPlatformDifference` — an admin, with a reason

Books the unexplained figure of the **latest** check, never an amount typed by hand:

- its whole rupiah **rounded down** to the platform's AVAILABLE (journal `MANUAL_ADJUSTMENT`),
  its sen (0..99) to the pool: `4.000,16` → Rp4.000 + 16 sen, `−41,30` → −Rp42 + 70 sen. Rounded
  down so the ledger never claims more than Singapay holds — a payout of the whole ledger
  balance must never be refused for a few sen;
- a reason (`TEST_FUNDS`, `OFF_SYSTEM_WITHDRAWAL`, `SINGAPAY_CORRECTION`, `OTHER` with a note)
  and the admin are recorded;
- refused when the check is not the latest, is older than 10 minutes
  (`PlatformDifferenceMaxCheckAge`), found nothing or was skipped, is already booked, or a
  platform payout is in flight. The unique index on `reconciliation_uuid` holds "once" against
  two requests at once, behind the row lock.

Check again afterwards: the next check is 0.

### `ArchiveToReserve` — once, by an operator

The DOKU era is the platform's AVAILABLE entries for the transactions settled before the ledger
recorded settled fees (`settled_platform_fee IS NULL` — every settlement since migration 024
records one). In production: seven fees of Rp1.000 and the subscription YTWGBR of Rp38.727,
28 Aug – 8 Sep 2026, Rp45.727 in all, never in the Singapay sub-account.

One `MANUAL_ADJUSTMENT` journal moves it: PLATFORM −45.727, RESERVE +45.727 (a new account,
`owner_type RESERVE`, `owner_id PLATFORM_DOKU_ARCHIVE`). The amount is computed from the
entries and must equal the caller's expected amount; a second archive is refused. A dry run
(`Apply: false`) computes the same plan and writes nothing.

Why not the alternatives:

- a second PLATFORM row is refused by `idx_accounts_unique_platform`;
- the old entries cannot move: `ledger_entries` is insert-only and every entry holds its
  running `balance_after`;
- reversing each DOKU transaction would show those sales at Rp0 on the statement and on the
  Platform Earnings chart, which sum the PRODUCT_TRANSACTION entries.

The archive's entries are sourced on the adjustment, so the DOKU-era statement lines and the
chart do not move. No total reads a RESERVE account.

---

## The statement

`GetPlatformTransactions` has a third source: `platform_adjustments`, as `ADJUSTMENT` lines
(Type `ADJUSTMENT` narrows to them). Read from the table, not from an entry type: a
`RECONCILIATION` journal is also the card fee recomputation's, which belongs on its sale's
income line. An adjustment's `Amount` is signed. Income + adjustments − payouts is the AVAILABLE
balance.

---

## What the consuming service runs

```
13:00 WIB, the catch-up tick after boot, and after a manual sweep:
  settlement pass → card fees → sweep → BookPlatformResidual → CheckPlatformBalance → alert
```

With a report-only switch: the check runs and is recorded, the rounding is not booked and no
alert is sent. That is how it is first deployed, before the DOKU archive: until then the check
reports −Rp41.726,84 (the DOKU era −45.727 plus CSV7MW 4.000,16), which is not news.
