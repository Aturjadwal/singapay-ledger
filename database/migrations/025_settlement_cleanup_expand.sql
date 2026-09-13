-- Migration: 025_settlement_cleanup_expand.sql
-- Purpose: PHASE 1 of 2 — make the database accept both the old and the new code, so the
--          new code can be deployed without a maintenance window.
-- Date: 2026-09-13
--
-- ═══════════════════════════════════════════════════════════════════════════════════════
-- APPLY THIS **BEFORE** DEPLOYING THE NEW CODE. It is reversible and drops nothing.
-- The destructive half is 026, which is applied AFTER the rollout completes.
-- ═══════════════════════════════════════════════════════════════════════════════════════
--
-- ---------------------------------------------------------------------------------------
-- Why this is split in two
-- ---------------------------------------------------------------------------------------
-- payment_requests.status is NOT NULL with no DEFAULT, and the new code's INSERT does not
-- supply it. So there is no ordering in which a single migration is safe:
--
--   old code + old schema   ok
--   NEW code + old schema   every payment creation fails: null value in column "status"
--   old code + NEW schema   every payment creation fails: column "status" does not exist
--   NEW code + NEW schema   ok
--
-- During any rolling deploy both versions of the code run at once, so one of the two
-- failing rows is guaranteed. Dropping NOT NULL first removes that: with the column
-- nullable, the old code still writes its value and the new code simply leaves it NULL.
-- Both work, at the same time, and nothing is lost if the rollout is rolled back.
--
-- A NULL passes the existing CHECK constraint: in PostgreSQL a CHECK that evaluates to
-- NULL is satisfied, not violated. No constraint change is needed here.
--
BEGIN;

ALTER TABLE payment_requests ALTER COLUMN status DROP NOT NULL;

COMMENT ON COLUMN payment_requests.status IS
    'DEPRECATED, dropped by migration 026. It duplicated product_transactions.status, which '
    'is decided under a compare-and-set and is the only source of truth. Nullable since 025 '
    'so that old and new code can run side by side during a rollout; the new code never '
    'writes it.';

COMMENT ON COLUMN payment_requests.expires_at IS
    'Expiry the instrument was issued with. A record of what was asked of Singapay, not a '
    'lifecycle this ledger drives: nothing sweeps on it, and an instrument that lapses '
    'leaves its transaction in PENDING.';

COMMIT;
