# 104 — Fee Mismatch Reconciliation

## Overview

This document describes the reconciliation mechanism when a discrepancy exists between `ExpectedGatewayFee` (recorded at payment time) and `ActualGatewayFee` (from the Singapay settlement).

---

## Terminology

| Term | Definition |
|---|---|
| `ExpectedGatewayFee` | gateway fee predicted at payment time (stored in `ProductTransaction.Fee.GatewayFee`) |
| `ActualGatewayFee` | Actual gateway fee from the `FEE` column in the settlement CSV |
| `feeDelta` | `ActualGatewayFee - ExpectedGatewayFee` |
| `PayToMerchant` | `PAY TO MERCHANT` column in CSV — amount Singapay sends to the merchant SAC |
| `ExpectedNetAmount` | Amount we expect in `PayToMerchant` based on the fee model |
| `AmountDiscrepancy` | `PayToMerchant - ExpectedNetAmount` |

---

## Fee Models

### `GATEWAY_ON_CUSTOMER`

Customer bears the gateway fee.

```
TotalCharged      = SellerPrice + PlatformFee + GatewayFee
SellerNetAmount   = SellerPrice  (seller receives 100% of their price)
ExpectedNetAmount = SellerNetAmount + PlatformFee
PayToMerchant     = TotalCharged - ActualGatewayFee
```

### `GATEWAY_ON_SELLER`

Seller bears the gateway fee.

```
TotalCharged      = SellerPrice + PlatformFee
SellerNetAmount   = SellerPrice - GatewayFee   (seller's share only; platform tracked separately)
ExpectedNetAmount = SellerNetAmount + PlatformFee
PayToMerchant     = TotalCharged - ActualGatewayFee
```

---

## Reconciliation Rules

Adjustment logic differs by fee model because who bears the gateway cost determines who absorbs the discrepancy.

### `GATEWAY_ON_CUSTOMER`

Customer already paid `ExpectedGatewayFee` upfront. Any delta is absorbed internally.

| Case | Rule | BLOCK condition |
|---|---|---|
| feeDelta > 0 | `adjustedPlatformFee = PlatformFee - feeDelta` | `adjustedPlatformFee < 0` |
| feeDelta < 0 | `adjustedSellerNet = SellerNetAmount + abs(feeDelta)` | — |
| feeDelta = 0 | Normal flow | — |

### `GATEWAY_ON_SELLER`

Seller agreed to bear the gateway fee. Any delta on the gateway cost falls on the seller.

| Case | Rule | BLOCK condition |
|---|---|---|
| feeDelta > 0 | `adjustedSellerNet = SellerNetAmount - feeDelta` | `adjustedSellerNet < 0` |
| feeDelta < 0 | `adjustedSellerNet = SellerNetAmount + abs(feeDelta)` | — |
| feeDelta = 0 | Normal flow | — |

> For `GATEWAY_ON_SELLER`, `PlatformFee` is always unchanged. Only `SellerNetAmount` adjusts.

---

## Example: `GATEWAY_ON_CUSTOMER` — feeDelta > 0

### Setup

```
SellerPrice     = 100,000
PlatformFee     =   5,000
ExpectedGatewayFee =   3,000
TotalCharged    = 108,000

ActualGatewayFee (from CSV) =  4,000
feeDelta                 = +1,000
adjustedPlatformFee      =  4,000
```

### Phase 2 — Payment Entries

| # | Account | Amount | Bucket | EntryType |
|---|---|---|---|---|
| 1 | Seller | +100,000 | PENDING | `PRODUCT_PAYMENT` |
| 2 | Platform | +5,000 | PENDING | `PLATFORM_COMMISSION` |
| 3 | Singapay | +3,000 | PENDING | `PROCESSOR_FEE` |

### Phase 3 — Settlement Entries

| # | Account | Amount | Bucket | EntryType | Notes |
|---|---|---|---|---|---|
| 4 | Seller | -100,000 | PENDING | `SETTLEMENT_CLEAR` | clear seller PENDING |
| 5 | Seller | +100,000 | AVAILABLE | `SETTLEMENT_NET` | seller can withdraw |
| 6 | Platform | -4,000 | PENDING | `SETTLEMENT_CLEAR` | clear platform PENDING (adjusted) |
| 7 | Platform | +4,000 | AVAILABLE | `SETTLEMENT_NET` | platform receives 4,000 |
| 8 | Platform | -1,000 | PENDING | `FEE_ADJUSTMENT` | write-off remaining PENDING |
| 9 | Singapay | -3,000 | PENDING | `SETTLEMENT` | clear Singapay PENDING |

### Final State

```
Seller   PENDING   = +100,000 - 100,000         =       0  ✓
Seller   AVAILABLE = +100,000                   = 100,000

Platform PENDING   = +5,000 - 4,000 - 1,000    =       0  ✓
Platform AVAILABLE = +4,000                     =   4,000

Singapay     PENDING   = +3,000 - 3,000             =       0  ✓
Singapay     AVAILABLE =                            =       0
```

**PayToMerchant check:**
```
Seller AVAILABLE + Platform AVAILABLE = 100,000 + 4,000 = 104,000
PayToMerchant from CSV                = 108,000 - 4,000 = 104,000  ✓
```

---

## Example: `GATEWAY_ON_CUSTOMER` — feeDelta < 0

### Setup

```
ActualGatewayFee (from CSV) =  2,000
feeDelta                 = -1,000
adjustedSellerNet        = 101,000
```

### Phase 3 — Settlement Entries

| # | Account | Amount | Bucket | EntryType | Notes |
|---|---|---|---|---|---|
| 4 | Seller | -100,000 | PENDING | `SETTLEMENT_CLEAR` | clear seller PENDING |
| 5 | Seller | +100,000 | AVAILABLE | `SETTLEMENT_NET` | from PENDING |
| 6 | Seller | +1,000 | AVAILABLE | `FEE_ADJUSTMENT` | surplus credited directly to AVAILABLE |
| 7 | Platform | -5,000 | PENDING | `SETTLEMENT_CLEAR` | clear platform PENDING |
| 8 | Platform | +5,000 | AVAILABLE | `SETTLEMENT_NET` | platform unchanged |
| 9 | Singapay | -3,000 | PENDING | `SETTLEMENT` | clear Singapay PENDING |

### Final State

```
Seller   PENDING   = +100,000 - 100,000         =       0  ✓
Seller   AVAILABLE = +100,000 + 1,000           = 101,000

Platform PENDING   = +5,000 - 5,000             =       0  ✓
Platform AVAILABLE = +5,000                     =   5,000

Singapay     PENDING   = +3,000 - 3,000             =       0  ✓
```

**PayToMerchant check:**
```
Seller AVAILABLE + Platform AVAILABLE = 101,000 + 5,000 = 106,000
PayToMerchant from CSV                = 108,000 - 2,000 = 106,000  ✓
```

---

## Example: `GATEWAY_ON_SELLER` — feeDelta > 0

### Setup

```
SellerPrice     = 100,000
PlatformFee     =   5,000
ExpectedGatewayFee =   3,000
TotalCharged    = 105,000    (= SellerPrice + PlatformFee; customer does NOT pay gateway fee)
SellerNetAmount =  97,000    (= SellerPrice - ExpectedGatewayFee)

ActualGatewayFee (from CSV) =  4,000
feeDelta                 = +1,000
adjustedSellerNet        =  96,000   (= 97,000 - 1,000)
```

### Phase 2 — Payment Entries

| # | Account | Amount | Bucket | EntryType |
|---|---|---|---|---|
| 1 | Seller | +97,000 | PENDING | `PRODUCT_PAYMENT` |
| 2 | Platform | +5,000 | PENDING | `PLATFORM_COMMISSION` |
| 3 | Singapay | +3,000 | PENDING | `PROCESSOR_FEE` |

> Total PENDING = 97,000 + 5,000 + 3,000 = 105,000 = TotalCharged ✓

### Phase 3 — Settlement Entries

| # | Account | Amount | Bucket | EntryType | Notes |
|---|---|---|---|---|---|
| 4 | Seller | -96,000 | PENDING | `SETTLEMENT_CLEAR` | clear adjusted amount from PENDING |
| 5 | Seller | +96,000 | AVAILABLE | `SETTLEMENT_NET` | seller receives adjusted amount |
| 6 | Seller | -1,000 | PENDING | `FEE_ADJUSTMENT` | write-off extra fee absorbed by seller |
| 7 | Platform | -5,000 | PENDING | `SETTLEMENT_CLEAR` | clear platform PENDING |
| 8 | Platform | +5,000 | AVAILABLE | `SETTLEMENT_NET` | platform unchanged |
| 9 | Singapay | -3,000 | PENDING | `SETTLEMENT` | clear Singapay PENDING (ExpectedGatewayFee) |

### Final State

```
Seller   PENDING   = +97,000 - 96,000 - 1,000  =       0  ✓
Seller   AVAILABLE = +96,000                    =  96,000

Platform PENDING   = +5,000 - 5,000            =       0  ✓
Platform AVAILABLE = +5,000                     =   5,000

Singapay     PENDING   = +3,000 - 3,000             =       0  ✓
Singapay     AVAILABLE =                            =       0
```

**PayToMerchant check:**
```
Seller AVAILABLE + Platform AVAILABLE = 96,000 + 5,000 = 101,000
PayToMerchant from CSV                = 105,000 - 4,000 = 101,000  ✓
```

---

## Example: `GATEWAY_ON_SELLER` — feeDelta < 0

### Setup

```
ActualGatewayFee (from CSV) =  2,000
feeDelta                 = -1,000
adjustedSellerNet        =  98,000   (= 97,000 + 1,000)
```

### Phase 3 — Settlement Entries

| # | Account | Amount | Bucket | EntryType | Notes |
|---|---|---|---|---|---|
| 4 | Seller | -97,000 | PENDING | `SETTLEMENT_CLEAR` | clear original PENDING |
| 5 | Seller | +97,000 | AVAILABLE | `SETTLEMENT_NET` | from PENDING |
| 6 | Seller | +1,000 | AVAILABLE | `FEE_ADJUSTMENT` | surplus — Singapay charged less than expected |
| 7 | Platform | -5,000 | PENDING | `SETTLEMENT_CLEAR` | clear platform PENDING |
| 8 | Platform | +5,000 | AVAILABLE | `SETTLEMENT_NET` | platform unchanged |
| 9 | Singapay | -3,000 | PENDING | `SETTLEMENT` | clear Singapay PENDING (ExpectedGatewayFee) |

### Final State

```
Seller   PENDING   = +97,000 - 97,000           =       0  ✓
Seller   AVAILABLE = +97,000 + 1,000            =  98,000

Platform PENDING   = +5,000 - 5,000             =       0  ✓
Platform AVAILABLE = +5,000                     =   5,000

Singapay     PENDING   = +3,000 - 3,000             =       0  ✓
```

**PayToMerchant check:**
```
Seller AVAILABLE + Platform AVAILABLE = 98,000 + 5,000 = 103,000
PayToMerchant from CSV                = 105,000 - 2,000 = 103,000  ✓
```

---

## BLOCK Conditions

A transaction is **irreconcilable** (BLOCK) when the absorbing party would receive a negative net amount — meaning Singapay's actual fee exceeds what is available to absorb.

### `GATEWAY_ON_CUSTOMER`

The platform absorbs `feeDelta > 0`.

```
BLOCK when: PlatformFee - feeDelta < 0
        i.e. ActualGatewayFee - ExpectedGatewayFee > PlatformFee
```

This means Singapay's overcharge exceeds the entire platform fee. The platform would owe money it never collected — there is no valid accounting outcome. The transaction must be investigated and resolved manually.

**Example:** PlatformFee = 500, feeDelta = +600 → adjustedPlatformFee = −100 → **BLOCK**

### `GATEWAY_ON_SELLER`

The seller absorbs `feeDelta > 0`.

```
BLOCK when: SellerNetAmount - feeDelta < 0
        i.e. ActualGatewayFee > SellerPrice
             (since SellerNetAmount = SellerPrice - ExpectedGatewayFee,
              and feeDelta = ActualGatewayFee - ExpectedGatewayFee,
              so SellerNetAmount - feeDelta = SellerPrice - ActualGatewayFee)
```

This means Singapay's actual fee exceeded the seller's entire price — the seller would receive negative proceeds. This is an abnormal situation (likely a data entry or integration error) and must be handled manually.

**Example:** SellerPrice = 10,000, ExpectedGatewayFee = 500, SellerNetAmount = 9,500, ActualGatewayFee = 11,000, feeDelta = +10,500 → adjustedSellerNet = −1,000 → **BLOCK**

### Handling BLOCKed Transactions

When a BLOCK condition is detected:
- The settlement item is marked as **unmatched** (`IsMatched = false`)
- A `DiscrepancySummary` of type `FEE_MISMATCH_IRRECONCILABLE` is recorded in the batch result
- No ledger entries are written for that transaction
- The transaction remains in `COMPLETED` status (not `SETTLED`)
- Manual investigation is required before the transaction can be settled

---

## Nature of `FEE_ADJUSTMENT` Entries

`FEE_ADJUSTMENT` entries are **terminal** — they have no counterpart and no subsequent phase.

| Fee Model | Case | Account | Bucket | Direction | Nature |
|---|---|---|---|---|---|
| `GATEWAY_ON_CUSTOMER` | feeDelta > 0 | Platform | PENDING | - (debit) | Write-off. Singapay took more than expected; platform absorbs the delta. Does not reduce AVAILABLE. |
| `GATEWAY_ON_CUSTOMER` | feeDelta < 0 | Seller | AVAILABLE | + (credit) | Direct credit. Singapay charged less; surplus passed to seller. |
| `GATEWAY_ON_SELLER` | feeDelta > 0 | Seller | PENDING | - (debit) | Write-off. Singapay took more than expected; seller absorbs the delta. Does not reduce AVAILABLE. |
| `GATEWAY_ON_SELLER` | feeDelta < 0 | Seller | AVAILABLE | + (credit) | Direct credit. Singapay charged less; surplus passed to seller. |

---

## Entry Type Reference

| EntryType | Bucket | Direction | Event |
|---|---|---|---|
| `PRODUCT_PAYMENT` | PENDING | + | Phase 2: payment success (Seller) |
| `PLATFORM_COMMISSION` | PENDING | + | Phase 2: payment success (Platform) |
| `PROCESSOR_FEE` | PENDING | + | Phase 2: payment success (Singapay) |
| `SETTLEMENT_CLEAR` | PENDING | - | Phase 3: settlement CSV |
| `SETTLEMENT_NET` | AVAILABLE | + | Phase 3: settlement CSV |
| `SETTLEMENT` | PENDING | - | Phase 3: clear Singapay PENDING |
| `FEE_ADJUSTMENT` | PENDING / AVAILABLE | - / + | Phase 3: fee mismatch adjustment |
| `DISBURSEMENT` | AVAILABLE | - | Seller withdrawal |

---

## Implementation

### Required Changes

**`domain/ledger_entry.go`** — add new entry type:
```go
EntryTypeFeeAdjustment EntryType = "FEE_ADJUSTMENT"
```

**`domain/settlement_item.go`** — add field for tracking (optional, for reporting):
```go
FeeAdjustment int64  // feeDelta applied (0 if no mismatch)
```

**`ledger.go`** — replace `HasAmountDiscrepancy()` block with fee adjustment logic:
```
feeDelta = ActualGatewayFee - ExpectedGatewayFee

if feeDelta > 0:
    switch feeModel:
        GATEWAY_ON_CUSTOMER:
            adjustedPlatformFee = PlatformFee - feeDelta
            if adjustedPlatformFee < 0 → BLOCK (irreconcilable)
        GATEWAY_ON_SELLER:
            adjustedSellerNet = SellerNetAmount - feeDelta
            if adjustedSellerNet < 0 → BLOCK (irreconcilable)
    → proceed with adjustment entries

elif feeDelta < 0:
    adjustedSellerNet = SellerNetAmount + abs(feeDelta)
    → proceed with adjustment entries (both models: surplus always to seller)

else:
    → normal settlement
```

### What Does Not Change

- `ProductTransaction.Fee` — retains original values from payment time (historical record)
- Existing `ledger_entries` rows — never modified (immutable by design)
- Singapay is always cleared using `ExpectedGatewayFee`
- Matching logic is unchanged