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
-- 3. idx_settlement_items_product_tx_unique  — the emergency brake
-- ---------------------------------------------------------------------------------------
-- Settlement idempotency currently lives at BATCH level: migration 013's unique index on
-- settlement_batches.batch_id. That was right when one DOKU CSV was one batch was one
-- authoritative list.
--
-- It does not protect the per-transaction design. Two paths can now settle the same
-- transaction — a notification-driven pass and a later sweep — and they do not share a
-- batch id, so nothing at batch level is violated. The grain of the guarantee has to match
-- the grain of the work.
--
-- The normal brake is the conditional UPDATE in UpdateStatusIf(COMPLETED -> SETTLED),
-- which takes the row lock and reports whether the row actually moved; a caller that gets
-- false rolls back having written nothing. This index is the emergency brake underneath
-- it: it makes a second settlement item for one transaction impossible regardless of which
-- caller inserts, whether the status check was skipped, or whether two workers run at once.
--
-- NULLs are distinct in a Postgres unique index, so unmatched items — which carry no
-- product_transaction_uuid and are exactly the rows we want many of — coexist freely. The
-- partial predicate makes that explicit.
--
-- CREATING THIS INDEX FAILS IF THE TABLE ALREADY HOLDS TWO ITEMS FOR ONE TRANSACTION.
-- Check first:
--
--   SELECT product_transaction_uuid, COUNT(*), array_agg(uuid)
--   FROM settlement_items
--   WHERE product_transaction_uuid IS NOT NULL
--   GROUP BY product_transaction_uuid
--   HAVING COUNT(*) > 1;
--
-- If that returns rows, do not "fix" it with this migration. Duplicate items mean
-- duplicate ledger entries, which are insert-only and must be corrected with compensating
-- entries and an audit. Resolve that first.

ALTER TABLE product_transactions
    ADD COLUMN IF NOT EXISTS settled_platform_fee BIGINT;

ALTER TABLE product_transactions
    ADD COLUMN IF NOT EXISTS settled_gateway_fee BIGINT;

CREATE INDEX IF NOT EXISTS idx_product_transactions_awaiting_settlement
    ON product_transactions (completed_at)
    WHERE status = 'COMPLETED';

CREATE UNIQUE INDEX IF NOT EXISTS idx_settlement_items_product_tx_unique
    ON settlement_items (product_transaction_uuid)
    WHERE product_transaction_uuid IS NOT NULL;
