-- Migration: 033_product_transactions_fee_estimate_index.sql
-- Purpose: serve the card platform-fee reconciliation's two reads — the sellers holding
--          cards still settled on the estimate, and each seller's cards — from an index of
--          those rows only.
-- Date: 2026-11-12
--
-- ---------------------------------------------------------------------------------------
-- HOW TO APPLY — one statement, outside a transaction, AFTER 032
-- ---------------------------------------------------------------------------------------
-- The predicate names settled_fee_source, which 032 adds, so 032 comes first.
--
-- CREATE INDEX CONCURRENTLY keeps product_transactions writable while the index builds (a
-- payment's webhook and the settling pass keep booking), and it cannot run inside a
-- transaction block. Run this file on its own, with psql's autocommit and WITHOUT -1 /
-- --single-transaction, through the session pooler (:5432) or a direct connection:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f database/migrations/033_product_transactions_fee_estimate_index.sql
--
-- A CONCURRENTLY build that fails (a deadlock, a cancelled session) leaves an INVALID index
-- behind, which the planner ignores and IF NOT EXISTS would then skip. Drop it and run the
-- file again:
--
--   DROP INDEX CONCURRENTLY IF EXISTS idx_product_transactions_fee_estimate;
--
-- Check the result: indisvalid must be true.
--
--   SELECT indexrelid::regclass, indisvalid FROM pg_index
--   WHERE indexrelid = 'idx_product_transactions_fee_estimate'::regclass;
--
-- Order relative to the deploy: either way. Nothing depends on the index; without it the two
-- reads return the same rows, only by scanning the settled backlog.
--
-- ---------------------------------------------------------------------------------------
-- WHY
-- ---------------------------------------------------------------------------------------
-- Both reads select (estimatedCardFeeSQL in repo/postgres_platform_fee_reconciliation.go)
--
--     WHERE status = 'SETTLED'
--       AND platform_fee_transferred = false
--       AND settled_fee_source = 'ESTIMATE'
--       AND ...                                -- card, GATEWAY_ON_CUSTOMER, platform fee
--
-- by seller, oldest settlement first. The rows that match are few and transient — a card is
-- ESTIMATE from settlement until the next round — while idx_product_transactions_pending_transfer
-- holds every unswept row of every channel. The predicate is written as the repository writes
-- it: the planner proves a partial index's predicate only against the same conditions.

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_product_transactions_fee_estimate
    ON product_transactions (seller_account_id, settled_at)
    WHERE status = 'SETTLED'
      AND platform_fee_transferred = false
      AND settled_fee_source = 'ESTIMATE';
