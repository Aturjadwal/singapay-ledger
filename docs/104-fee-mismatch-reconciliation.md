# 104 — Fee Mismatch Reconciliation

## Overview

This document describes what happens when `EstimatedGatewayFee` (recorded at payment time)
and `ActualGatewayFee` (what Singapay actually took) disagree.

> **Where the actual fee comes from.** The settling pass reads each open invoice back from
> Singapay directly and takes the fee off that record — see
> [102](./102-settlement-reconciliation.md). There is no settlement file.
>
> **Virtual account, QRIS and e-wallet report their own fee. Payment link reports none.** For
> a payment link the actual fee is copied from the estimate, which makes `feeDelta` zero
> by construction — that is the absence of a reconciliation, not a perfect one, and
> `FeeReported` is what keeps the two distinguishable.

The arithmetic below is what `resolveFeeAdjustment` in [`settlement.go`](../settlement.go)
implements.

---

## The rule, in one line

**The seller is paid what they were priced. The platform sub-account balances the rest.**

```
platformAdjustment = EstimatedGatewayFee - ActualGatewayFee
```

| Sign | Meaning | Where it goes |
|---|---|---|
| Positive | Singapay charged **less** than estimated | The residual is the platform's |
| Negative | Singapay charged **more** than estimated | The platform absorbs it out of its own fee |
| Zero | The estimate was exact | Nothing moves |

`SellerNetAmount` is the same number in all three rows. That is the point of the design, not
a side effect of it.

### Why the seller and not a shared split

Money lands in the **seller's** Singapay sub-account, net of whatever fee Singapay deducted.
The only other thing that ever leaves that sub-account is the platform fee, swept out by
`ProcessPlatformFeeTransfer`. So:

```
seller's Singapay balance = TotalCharged - ActualGatewayFee - SweptPlatformFee
```

Set `SweptPlatformFee = PlatformFee - (Actual - Estimated)` and the actual fee cancels:

```
= TotalCharged - EstimatedGatewayFee - PlatformFee
= SellerPrice                                            ← exactly what the ledger booked
```

**Seller's Singapay balance = seller's ledger balance**, for every transaction, whatever
Singapay charged. That invariant is the whole reason this mechanism exists, and it holds only
because the sweep moves the *balanced* figure rather than the one quoted at checkout.

---

## Units: this is all in sen

Singapay reports a money-in fee with two decimals. A QRIS fee of `119.84` is a real figure,
not a rounding artefact, so `platformAdjustment` is routinely a fraction of a rupiah — which
is precisely the quantity the balancing exists to place.

| Thing | Unit | Why |
|---|---|---|
| `FeeBreakdown` (`SellerPrice`, `PlatformFee`, `GatewayFee`, …) | whole rupiah | every figure quoted at checkout is whole |
| `SettledTransaction` (`GrossMinor`, `NetMinor`, `FeeMinor`) | **sen** | it comes from Singapay, which uses decimals |
| `product_transactions.settled_platform_fee` / `settled_gateway_fee` / `platform_residual` | **sen** | what the fees turned out to be |
| `ledger_entries.amount` | whole rupiah | unchanged |
| `TransferRequest.Amount` | **decimal** | the account-transfer endpoint takes one |

> **The ×100 bug this replaced.** `SettledTransaction`'s fields were once named
> `GrossAmount` / `NetAmount` / `Fee` and carried no unit, while `readSettledTransaction`
> filled them with `.Minor()` for VA, QRIS and e-wallet and with whole rupiah for payment
> link. `feeDelta = settled.Fee - tx.Fee.GatewayFee` was therefore sen minus rupiah: a real
> fee of Rp120 read as `12000` produced a delta of `11880`. Most channels blocked loudly
> (the platform fee went negative); payment link was unaffected because its delta is zero
> by construction. The `Minor` suffix on every field is the fix, and the unit test now
> builds the struct through a `settledWithFeeMinor` helper rather than inline in rupiah —
> building it by hand in the wrong unit is how the bug stayed green.

### The residual

`ledger_entries.amount` is whole rupiah and the settled platform fee is not, so the two are
reconciled explicitly rather than by rounding:

```
settled_platform_fee = (ledger entry rupiah × 100) + platform_residual
```

- The **transfer** moves `SettledPlatformFeeMinor` exactly, fraction included.
- The **ledger entry** books the whole-rupiah part.
- `product_transactions.platform_residual` holds the difference (in sen, like the two settled-fee columns beside it).

The platform's ledger balance therefore trails its Singapay balance by
`SUM(platform_residual)` over settled rows — a number that can be produced on demand,
not a drift. The seller's two balances agree exactly, with no residual at all.

> **Rp1 minimum.** Singapay rejects an account transfer below 1 rupiah. A balanced platform
> fee under Rp1 is not swept: the row keeps its untransferred flag, stays in
> `GetSettledWithoutPlatformFeeTransfer`, and is reported as a failure rather than marked
> done with the money still in the seller's sub-account.

---

## Terminology

| Term | Definition |
|---|---|
| `EstimatedGatewayFee` | gateway fee predicted at payment time from `fee_configs` (`ProductTransaction.Fee.GatewayFee`, rupiah) |
| `ActualGatewayFee` | what Singapay really took, from the per-transaction read (`SettledTransaction.FeeMinor`, sen) |
| `feeDelta` | `ActualGatewayFee - EstimatedGatewayFee`, in sen |
| `SettledPlatformFee` | `PlatformFee - feeDelta` — the balanced figure the sweep moves |
| `FeeReported` | whether `ActualGatewayFee` is a fact or a copy of the estimate (false for payment link) |

---

## Fee Models

### `GATEWAY_ON_CUSTOMER`

Customer bears the gateway fee. This is the ordinary path.

```
TotalCharged    = SellerPrice + PlatformFee + EstimatedGatewayFee
SellerNetAmount = SellerPrice                      (seller receives 100% of their price)
```

### `GATEWAY_ON_SELLER`

Used for subscriptions, where the platform is itself the beneficiary and `SkipPlatformFee`
is set — so `PlatformFee` is zero and there is nothing to balance out of.

```
TotalCharged    = SellerPrice + PlatformFee
SellerNetAmount = SellerPrice - EstimatedGatewayFee
```

Its `feeDelta` is zero by construction: `GATEWAY_ON_SELLER` only ever runs over a payment
link, and a payment link reports no fee, so the estimate is copied to the actual. Should a
non-zero delta ever appear there, the settlement **blocks** rather than quietly repricing a
seller who was promised that net.

---

## Blocking

The one condition:

```
SettledPlatformFeeMinor < 0
```

Singapay took so much more than estimated that the platform would have to pay in more than
it ever charged. That is not a rounding to swallow — it means the estimate in `fee_configs`
and the real rate disagree by more than the transaction can carry, and no arithmetic makes
the result correct.

When it blocks:

- No ledger entries are written.
- The transaction stays `COMPLETED`, not `SETTLED`.
- It stays visible in `GetAwaitingSettlement` and keeps pushing up the oldest-awaiting age.
- The seller's net in the returned adjustment is still the priced figure — blocking is a
  decision about the platform's share and never reprices the seller.

A block is almost always a stale `fee_configs` row for that channel. Fix the rate, then the
next pass settles it.

---

## Example — the fractional case

```
SellerPrice           = 13,000
PlatformFee           =  4,000
EstimatedGatewayFee   =    120
TotalCharged          = 17,120

ActualGatewayFee      = 119.84      (Singapay charged 0.16 less than estimated)
feeDelta              =  -0.16
SettledPlatformFee    = 4,000.16
```

### Phase 2 — Payment entries (at payment time, all rupiah)

| # | Account | Amount | Bucket | EntryType |
|---|---|---|---|---|
| 1 | Seller | +13,000 | PENDING | `PRODUCT_PAYMENT` |
| 2 | Platform | +4,000 | PENDING | `PLATFORM_COMMISSION` |
| 3 | Singapay | +120 | PENDING | `PROCESSOR_FEE` |

> Total PENDING = 17,120 = TotalCharged ✓

### Phase 3 — Settlement entries

| # | Account | Amount | Bucket | EntryType | Notes |
|---|---|---|---|---|---|
| 4 | Seller | -13,000 | PENDING | `SETTLEMENT_CLEAR` | clears exactly what payment put in |
| 5 | Seller | +13,000 | AVAILABLE | `SETTLEMENT_NET` | the priced net, unconditionally |
| 6 | Platform | -4,000 | PENDING | `SETTLEMENT_CLEAR` | clears the priced fee |
| 7 | Platform | +4,000 | AVAILABLE | `SETTLEMENT_NET` | the whole-rupiah part of 4,000.16 |
| 8 | Singapay | -120 | PENDING | `SETTLEMENT` | clears the estimated fee |

Plus `platform_residual = 16`.

The platform's two legs differ, and **that difference is the fee delta** — there is no
separate `FEE_ADJUSTMENT` write-off entry any more. Booking the delta both as a gap between
the legs and as its own entry was counting it twice; one settlement, one statement of it.

### Final state

```
Seller   PENDING   = +13,000 - 13,000 = 0  ✓
Seller   AVAILABLE = +13,000              = 13,000     ← exactly the priced net

Platform PENDING   = +4,000 - 4,000   = 0  ✓
Platform AVAILABLE = +4,000               =  4,000     (+ 16 sen residual, recorded)

Singapay PENDING   = +120 - 120       = 0  ✓
```

### Phase 4 — the sweep

`ProcessPlatformFeeTransfer` moves **4,000.16** from the seller's sub-account to the
platform's.

```
seller's Singapay balance = 17,120 - 119.84 - 4,000.16 = 13,000.00
seller's ledger balance   =                              13,000
                                                         ✓ equal
```

### The mirror case

`ActualGatewayFee = 120.30` → `feeDelta = +0.30` → `SettledPlatformFee = 3,999.70`. The
platform's AVAILABLE leg books 3,999, the residual is 70, the sweep moves 3,999.70, and the
seller still ends at exactly 13,000.

---

## Entry Type Reference

| EntryType | Bucket | Direction | Event |
|---|---|---|---|
| `PRODUCT_PAYMENT` | PENDING | + | Phase 2: payment success (Seller) |
| `PLATFORM_COMMISSION` | PENDING | + | Phase 2: payment success (Platform) |
| `PROCESSOR_FEE` | PENDING | + | Phase 2: payment success (Singapay) |
| `SETTLEMENT_CLEAR` | PENDING | - | Phase 3: settlement |
| `SETTLEMENT_NET` | AVAILABLE | + | Phase 3: settlement |
| `SETTLEMENT` | PENDING | - | Phase 3: clear Singapay PENDING |
| `DISBURSEMENT` | AVAILABLE | - | Seller withdrawal |

`FEE_ADJUSTMENT` is no longer written. It stays in the `CHECK` constraint and in
`domain.EntryType` because `ledger_entries` is insert-only and rows booked under the old
rules carry it — a reader that cannot name it cannot read its own history.

---

## What does not change

- `ProductTransaction.Fee` — the values priced at payment time, kept as the historical record.
- Existing `ledger_entries` rows — never modified; insert-only by design.
- The Singapay expense account is always cleared using `EstimatedGatewayFee`, because that is
  what was credited to it at payment time.

---

## Early warning

The money-in webhook carries a channel fee for virtual accounts. `HandleMoneyIn` compares it
against the estimate and logs a warning when they differ — a day's notice that a
`fee_configs` rate has drifted, before settlement has to balance it.

That comparison reads the fee in **sen**. It once went through `Amount.Rupiah()`, which
refuses a fractional amount rather than rounding it: a fee of `119.84` made it error, the
branch fell through, and the warning never fired in exactly the case worth warning about.
