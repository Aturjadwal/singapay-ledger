-- Migration: 026_settlement_cleanup_contract.sql
-- Purpose: PHASE 2 of 2 — drop the DOKU batch reconciler's tables and the payment request's
--          duplicate status columns.
-- Date: 2026-09-13
--
-- ═══════════════════════════════════════════════════════════════════════════════════════
-- IRREVERSIBLE. Apply ONLY after 025, and ONLY once no instance of the old code is left.
-- Read section 0 before applying it anywhere.
-- ═══════════════════════════════════════════════════════════════════════════════════════
--
-- Ordering, in full:
--
--   1. Apply 025                  (nullable status; old and new code both work)
--   2. Deploy the new code        (rolling deploy is safe throughout)
--   3. Confirm the rollout is complete and is not going to be rolled back
--   4. Apply THIS migration
--
-- Running this at step 1 or 2 breaks every payment creation still served by an old
-- instance: its INSERT names a status column that no longer exists.
--
-- ---------------------------------------------------------------------------------------
-- 0. Archive first, if this database ever ran DOKU
-- ---------------------------------------------------------------------------------------
-- Ledger entries are insert-only, and rows the old reconciler booked carry
--
--     journals.source_type       = 'SETTLEMENT_BATCH'
--     ledger_entries.source_type = 'SETTLEMENT_BATCH'
--     ...with source_id = settlement_batches.uuid
--
-- There is no foreign key holding that together, so DROP TABLE succeeds and the references
-- silently stop resolving. The entries remain correct — the money they describe is real
-- and already booked — but the batch they name becomes unreadable.
--
-- Count what would be orphaned:
--
--   SELECT count(*) FROM settlement_batches;
--   SELECT count(*) FROM settlement_items;
--   SELECT count(*) FROM reconciliation_discrepancies;
--   SELECT count(*) FROM ledger_entries WHERE source_type = 'SETTLEMENT_BATCH';
--
-- If any is non-zero, archive before dropping. Either is acceptable:
--
--   CREATE TABLE settlement_batches_archive           AS TABLE settlement_batches;
--   CREATE TABLE settlement_items_archive             AS TABLE settlement_items;
--   CREATE TABLE reconciliation_discrepancies_archive AS TABLE reconciliation_discrepancies;
--
-- ...or a pg_dump of the three tables kept somewhere an auditor can reach.
--
-- On a database that only ever ran Singapay, all four counts are zero and this is a no-op.
--
-- ---------------------------------------------------------------------------------------
-- 1. Why the three tables go
-- ---------------------------------------------------------------------------------------
-- One routine wrote them: ProcessReconciliation, which reconstructed a settlement batch by
-- replaying its date window against the per-product transaction lists. It has refused to
-- run since the Singapay migration, because two questions it depends on could not be
-- answered from Singapay's documentation: what timezone its offsetless window text is in,
-- and which of a QRIS transaction's two settlement timestamps the window keys on.
--
-- The per-transaction settling pass does not ask either question. It takes the ledger's own
-- open invoices and asks Singapay about each one directly, so no window is ever used to
-- select rows. It writes a journal, ledger entries, and the transaction's status and
-- settled fees. It has never written these three tables.
--
-- Keeping them is not neutral. settlement_batches has account_uuid NOT NULL — one row per
-- seller — against idx_settlement_batches_batch_id_unique, which is unique globally. One
-- Singapay settlement covering fifty sellers would be accepted for the first and refused
-- for the second. That trap is only harmless while nothing writes the table.
--
-- The audit trail they were meant to carry already exists and is immutable: the settlement
-- journal's metadata holds actual_gateway_fee, fee_delta, settled_platform_fee,
-- expected_gateway_fee, fee_reported and raw_gateway_data, and journals are insert-only.
--
-- 'SETTLEMENT_BATCH' STAYS in the source_type CHECK constraints on journals and
-- ledger_entries. It is not written by anything any more, but historical rows carry it and
-- a constraint that rejects existing data makes that history unreadable.
--
-- ---------------------------------------------------------------------------------------
-- 2. Why payment_requests loses three columns
-- ---------------------------------------------------------------------------------------
-- status had exactly one writer that mattered — the money-in webhook, which set COMPLETED
-- immediately after the compare-and-set that moved product_transactions from PENDING to
-- COMPLETED, in the same database transaction — and no readers at all.
--
-- A payment request is created 1:1 with its product transaction and never independently, so
-- "has this been paid?" is a question about the transaction, and product_transactions.status
-- is where it is asked and answered under a row lock. A second copy could only agree with
-- that one or be wrong about it.
--
-- failure_reason was never written by anything. completed_at duplicated
-- product_transactions.completed_at. Three of the four states were unreachable: a failed
-- money-in webhook is logged and left PENDING by design, and the expiry sweep was never
-- written.
--
-- expires_at STAYS. It records what was asked of Singapay when the instrument was issued —
-- a fact about the instrument, not a lifecycle this ledger drives.

BEGIN;

-- Drop in foreign-key order: discrepancies and items both reference batches.
DROP TABLE IF EXISTS reconciliation_discrepancies;
DROP TABLE IF EXISTS settlement_items;
DROP TABLE IF EXISTS settlement_batches;

-- The CHECK constraint names status, so it has to go before the column can.
ALTER TABLE payment_requests DROP CONSTRAINT IF EXISTS payment_requests_status_check;

DROP INDEX IF EXISTS idx_payment_requests_status;

ALTER TABLE payment_requests DROP COLUMN IF EXISTS status;
ALTER TABLE payment_requests DROP COLUMN IF EXISTS failure_reason;
ALTER TABLE payment_requests DROP COLUMN IF EXISTS completed_at;

COMMIT;
