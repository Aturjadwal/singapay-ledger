-- Migration: 024_settled_fee_amounts.sql
-- Purpose: record what a transaction's fees turned out to be, and stop the platform fee
--          transfer from moving an amount the ledger no longer agrees with.
-- Date: 2026-09-11
--
-- READ THE FIRST SECTION BEFORE APPLYING. It describes a live divergence between real
-- money and the ledger, not a tidy-up.
--
-- ---------------------------------------------------------------------------------------
-- 1. settled_platform_fee / settled_gateway_fee
-- ---------------------------------------------------------------------------------------
-- product_transactions records fees as they were PRICED, at checkout: platform_fee and
-- gateway_fee are what the fee calculator worked out before anyone paid. Reconciliation
-- then discovers what Singapay ACTUALLY took, and the two can differ.
--
-- Where the difference lands depends on the fee model (docs/104-fee-mismatch-reconciliation.md):
--
--   GATEWAY_ON_CUSTOMER  the platform absorbs it: adjustedPlatformFee = PlatformFee - feeDelta
--   GATEWAY_ON_SELLER    the seller absorbs it:   adjustedSellerNet   = SellerNetAmount - feeDelta
--
-- So on GATEWAY_ON_CUSTOMER the platform fee that survives settlement may be smaller than
-- the one priced at checkout. The ledger books the smaller one, as a FEE_ADJUSTMENT entry.
--
-- ProcessPlatformFeeTransfer does not read ledger entries. It reads the column:
--
--     TransferBetweenAccounts(..., Amount: tx.Fee.PlatformFee)     <- product_transactions.platform_fee
--
-- which is still the checkout figure. So it moves the full platform fee out of the
-- seller's Singapay sub-account while the ledger says the platform only earned part of it.
-- That is real money at the gateway diverging from the ledger, and it always diverges in
-- the same direction: against the seller.
--
-- These two columns are the fix. The settling pass writes what the fees turned out to be;
-- the transfer reads settled_platform_fee and falls back to platform_fee when it is NULL.
--
-- NULLABLE ON PURPOSE. NULL means "this transaction settled before we recorded this" —
-- i.e. use the priced figure. It does not mean zero. A DEFAULT 0 here would silently
-- transfer nothing for every historical row, which is the worst available failure: no
-- error, no entry, and the platform quietly stops collecting.
--
-- No backfill. Rows that already settled were transferred under the old rule and their
-- money has already moved; writing a value now would describe an event that did not happen.
--
-- ---------------------------------------------------------------------------------------
-- 2. idx_product_transactions_awaiting_settlement
-- ---------------------------------------------------------------------------------------
-- GetAwaitingSettlement runs on every worker tick:
--
--     SELECT ... FROM product_transactions WHERE status = 'COMPLETED'
--     ORDER BY completed_at ASC LIMIT $1
--
-- The existing indexes are (status) and (status, settled_at). The first can find the rows
-- but then has to sort all of them; the second is useless here because settled_at is NULL
-- for exactly the rows being selected. A partial index on completed_at serves both the
-- predicate and the ordering, and stays the size of the backlog rather than the history.
--
-- It also serves the health check the worker leans on — MIN(completed_at) WHERE
-- status = 'COMPLETED', the age of the oldest unsettled transaction — as an index scan
-- of one row.
--
-- ---------------------------------------------------------------------------------------
-- Removed before this was ever applied: idx_settlement_items_product_tx_unique
-- ---------------------------------------------------------------------------------------
-- An earlier draft added a unique index on settlement_items(product_transaction_uuid) as
-- an emergency brake under the per-transaction design. settlement_items is dropped by
-- migration 026 and nothing writes it, so the index would have guarded an empty table on
-- its way to being deleted.
--
-- The brake itself is not lost. It was always the conditional UPDATE in
-- UpdateStatusIf(COMPLETED -> SETTLED): that takes the row lock and reports whether the
-- row actually moved, and a caller that gets false rolls back having written nothing.
-- That guard is on product_transactions, which is the grain the work is done at.

ALTER TABLE product_transactions
    ADD COLUMN IF NOT EXISTS settled_platform_fee BIGINT;

ALTER TABLE product_transactions
    ADD COLUMN IF NOT EXISTS settled_gateway_fee BIGINT;

CREATE INDEX IF NOT EXISTS idx_product_transactions_awaiting_settlement
    ON product_transactions (completed_at)
    WHERE status = 'COMPLETED';
