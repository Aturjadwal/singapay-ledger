# Docs

This folder contains high-level architecture diagrams and technical documentation for the Ledger system.

## Table of Contents

1. **Payment Execution** ([`101-payment-execution.md`](./101-payment-execution.md))
    - Shows the flow from payment request to a booked money-in webhook.
    - Highlights that funds land in **PENDING** and are not withdrawable until settlement.

2. **Settlement** ([`102-settlement-reconciliation.md`](./102-settlement-reconciliation.md))
    - The per-transaction path: the webhook is a doorbell, and each open invoice is read back from Singapay directly.
    - Covers the 24-hour floor that keeps a lost webhook from stranding money, per-channel key selection, and the `COMPLETED → SETTLED` compare-and-set.
    - Records the window-replay design that was rejected, and why two of its assumptions could not be verified.
    - Still open: the refund policy, which needs a negative-balance decision rather than code.

3. **Withdrawal (Disbursement)** ([`103-withdrawal-disbursement.md`](./103-withdrawal-disbursement.md))
    - Visualizes the withdrawal process: quote the fee, carve it out of the request, reserve the requested amount under a row lock, send the net, book the outcome.
    - Documents the failure taxonomy — why `singapay.Outcome` decides whether a reservation may be released, and never the HTTP status.

4. **Fee Mismatch Reconciliation** ([`104-fee-mismatch-reconciliation.md`](./104-fee-mismatch-reconciliation.md))
    - Explains how a difference between the expected gateway fee and the one actually taken is handled.
    - Covers adjustment rules for both fee models (`GATEWAY_ON_CUSTOMER`, `GATEWAY_ON_SELLER`).
    - Documents the `FEE_ADJUSTMENT` ledger entry type and its terminal nature.

5. **Singapay Migration Reference** ([`105-singapay-migration.md`](./105-singapay-migration.md))
    - **Research, not current-state reference.** The document the migration was built from: how Singapay operates, its account hierarchy, the pending → available money cycle, the three signature schemes, and the full endpoint catalogue.
    - Records what is **not** 1:1 — no settlement file, Payment Link exposes no per-transaction fee, sub-account creation is not idempotent. The open questions it raises about settlement were answered by the design in [102](./102-settlement-reconciliation.md), not by this document.

## Maintenance

Diagrams are maintained in Markdown using Mermaid JS. To edit:
1. Open the `.md` file in VS Code.
2. Use a Markdown preview extension that supports Mermaid (e.g., `Markdown Preview Mermaid Support`).
3. Update the text-based diagram definition.
