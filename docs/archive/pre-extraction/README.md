# ARCHIVED — pre-extraction flow documentation

> **STATUS: HISTORICAL. Do not use as a reference for this repository.**
> Archived 2026-09-13. These five files were the top-level `markdown/` folder.

They describe the ledger **as it was when it still lived inside the monoservice**, before it
was extracted into this package. They are not merely out of date; they describe a different
codebase.

What gives it away:

- `00-system-architecture.md` maps a folder tree of `usecases/ledger_payment_usecase.go`,
  `requests/ledger_payment_request.go`, `responses/`, `utils/helper/`. This repository has
  none of those. Its layout is `domain/`, `repo/`, `singapay/`, plus flat files at the root.
- `01-payment-flow.md` and `02-settlement-flow.md` call
  `SingapaySettlementUseCase.CalculateGrossAmount()` and `.CalculateSettlementFee()`. No such
  type has ever existed here — the name is what a DOKU → Singapay find-and-replace made of
  `DokuSettlementUseCase`, a monoservice usecase.
- `02-settlement-flow.md` documents settlement statuses `IN_PROGRESS` and `TRANSFERRED`.
  This ledger has never had them. Transaction status is `PENDING → COMPLETED → SETTLED`.
- `04-balance-query.md` documents `WalletBalanceResponse`, which is a monoservice HTTP
  response type, not a ledger one.

They carry no DOKU-era vocabulary, which can make them look freshly maintained. They are not:
they simply predate the rename.

**For how this library actually works, read** [`../../../AGENTS.md`](../../../AGENTS.md) and
the numbered documents in [`../../`](../../).
