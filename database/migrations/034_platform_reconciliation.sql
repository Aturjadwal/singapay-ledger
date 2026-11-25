-- Migration: 034_platform_reconciliation.sql
-- Purpose: give the platform balance reconciliation (v0.13.0) its two tables:
--          platform_reconciliations (one row per check of the platform's ledger against its
--          Singapay sub-account) and platform_adjustments (every movement of the platform's
--          AVAILABLE balance that no sale and no payout explains: the cent rounding, a
--          difference an admin booked, the DOKU-era archive).
-- Date: 2026-11-25
--
-- ---------------------------------------------------------------------------------------
-- HOW TO APPLY — one transaction, BEFORE the deploy that carries v0.13.0
-- ---------------------------------------------------------------------------------------
-- Run the whole file with psql, through the session pooler (:5432) or a direct connection:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f database/migrations/034_platform_reconciliation.sql
--
-- It only adds tables and indexes. Nothing in v0.11.x reads or writes them, no existing
-- table, column or CHECK constraint changes, so the code running now is unaffected and the
-- file may be applied at any time before the deploy (also across a 13:00 WIB pass). The new
-- code reads both tables on its first pass, about two minutes after the worker boots, so the
-- order is: this file, then the deploy.
--
-- Running it again is safe: every statement is IF NOT EXISTS.
--
-- Rollback, if the deploy is rolled back before anything was booked: the old code ignores
-- the tables, so they can simply stay. Dropping them (platform_adjustments first) is only
-- right while platform_adjustments is empty: its rows explain journals and ledger entries
-- that are insert-only and stay.
--
-- Check the result:
--
--   SELECT to_regclass('platform_reconciliations'), to_regclass('platform_adjustments');
--   SELECT indexname FROM pg_indexes
--    WHERE tablename IN ('platform_reconciliations', 'platform_adjustments') ORDER BY 1;
--
-- ---------------------------------------------------------------------------------------
-- WHY
-- ---------------------------------------------------------------------------------------
-- ledger_entries.amount is whole rupiah, while the platform-fee sweep moves the fee to the
-- sen (4000.16). The sen are kept on the row (product_transactions.platform_residual) and the
-- platform's ledger balance trails its Singapay balance by their sum. Other money the ledger
-- never saw reaches or leaves the sub-account too (CSV7MW, a staging test on production
-- credentials), and the DOKU era sits in the ledger with no money behind it at Singapay.
--
-- The reconciliation reads the equation, every term in sen and recomputed from rows that
-- never change:
--
--   unexplained = S − L − K + B
--   S  Singapay's available balance of the platform sub-account
--   L  the platform's AVAILABLE ledger balance (Σ of its entries) × 100
--   K  the residual pool = Σ platform_residual of swept rows + Σ platform_adjustments.residual_minor
--                         − Σ platform_adjustments.amount × 100 of the ROUNDING rows
--   B  platform fee settled but not swept yet, at the whole rupiah the ledger holds for it, × 100
--
-- and records every check here. Only the pool is ever booked by itself (ROUNDING, whole
-- rupiah once it reaches Rp1); any other difference is an alert that an admin books with a
-- reason (DIFFERENCE). The journals of these rows use the event types RECONCILIATION and
-- MANUAL_ADJUSTMENT and their entries the entry type RECONCILIATION, all already allowed by
-- the CHECK constraints, with source_type MANUAL_ADJUSTMENT and source_id = the adjustment's
-- uuid, so the platform statement's income lines and the Platform Earnings chart (which sum
-- the PRODUCT_TRANSACTION entries) do not move.

BEGIN;
SET LOCAL lock_timeout = '5s';

-- 1. One row per check. Every amount is sen.
CREATE TABLE IF NOT EXISTS platform_reconciliations (
    uuid                  VARCHAR(255) PRIMARY KEY,
    randid                VARCHAR(255) NOT NULL UNIQUE,
    platform_account_uuid VARCHAR(255) NOT NULL REFERENCES ledger_accounts(uuid),
    singapay_account_id   VARCHAR(255) NOT NULL,
    singapay_available    BIGINT,          -- S; NULL when the balance was not read
    singapay_pending      BIGINT,
    singapay_held         BIGINT,
    ledger_available      BIGINT NOT NULL, -- L × 100
    residual_pool         BIGINT NOT NULL, -- K
    unswept_platform_fee  BIGINT NOT NULL, -- B × 100
    unswept_count         INT NOT NULL DEFAULT 0,
    payouts_in_flight     INT NOT NULL DEFAULT 0,
    unexplained           BIGINT,          -- S − L − K + B; NULL when skipped
    status                VARCHAR(20) NOT NULL CHECK (status IN ('MATCHED', 'MISMATCH', 'SKIPPED')),
    skip_reason           VARCHAR(30) CHECK (skip_reason IN (
                              'PAYOUT_IN_FLIGHT',
                              'BALANCE_READ_FAILED',
                              'LEDGER_MOVED'
                          )),
    skip_detail           TEXT,
    raw_balance           JSONB,           -- the balance inquiry's answer, as decoded
    checked_at            TIMESTAMP NOT NULL,
    emailed_at            TIMESTAMP,       -- when the admins were emailed about this check
    created_at            TIMESTAMP NOT NULL,
    CONSTRAINT platform_reconciliations_outcome_check CHECK (
        ((status = 'SKIPPED') = (skip_reason IS NOT NULL))
        AND ((status = 'SKIPPED') = (unexplained IS NULL))
        AND (status = 'SKIPPED' OR singapay_available IS NOT NULL)
        AND (status <> 'MATCHED' OR unexplained = 0)
        AND (status <> 'MISMATCH' OR unexplained <> 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_platform_reconciliations_latest
    ON platform_reconciliations (platform_account_uuid, checked_at DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_reconciliations_emailed
    ON platform_reconciliations (platform_account_uuid, emailed_at DESC) WHERE emailed_at IS NOT NULL;

-- 2. One row per adjustment of the platform's AVAILABLE balance. Insert-only.
CREATE TABLE IF NOT EXISTS platform_adjustments (
    uuid                     VARCHAR(255) PRIMARY KEY,
    randid                   VARCHAR(255) NOT NULL UNIQUE,
    platform_account_uuid    VARCHAR(255) NOT NULL REFERENCES ledger_accounts(uuid),
    kind                     VARCHAR(30) NOT NULL CHECK (kind IN ('ROUNDING', 'DIFFERENCE', 'RESERVE_ARCHIVE')),
    amount                   BIGINT NOT NULL,           -- whole rupiah, signed: what PLATFORM AVAILABLE moved by
    residual_minor           BIGINT NOT NULL DEFAULT 0, -- sen put into the pool: a DIFFERENCE's 0..99
    pool_before_minor        BIGINT,                    -- ROUNDING: the pool it booked from
    reason                   VARCHAR(40) CHECK (reason IN (
                                 'TEST_FUNDS',
                                 'OFF_SYSTEM_WITHDRAWAL',
                                 'SINGAPAY_CORRECTION',
                                 'OTHER'
                             )),
    note                     TEXT,
    admin_id                 VARCHAR(255),
    admin_name               VARCHAR(255),
    journal_uuid             VARCHAR(255) REFERENCES journals(uuid), -- NULL for a difference under Rp1
    reconciliation_uuid      VARCHAR(255) REFERENCES platform_reconciliations(uuid),
    reserve_account_uuid     VARCHAR(255) REFERENCES ledger_accounts(uuid),
    -- The product transactions the amount comes from: a ROUNDING's swept transactions whose
    -- sen entered the pool since the previous rounding, the archive's DOKU-era transactions.
    source_transaction_uuids TEXT[] NOT NULL DEFAULT '{}',
    created_at               TIMESTAMP NOT NULL,
    CONSTRAINT platform_adjustments_shape_check CHECK (
        (kind = 'ROUNDING'
            AND amount > 0 AND residual_minor = 0 AND pool_before_minor >= amount * 100
            AND reason IS NULL AND reconciliation_uuid IS NULL AND reserve_account_uuid IS NULL
            AND journal_uuid IS NOT NULL)
        OR (kind = 'DIFFERENCE'
            AND residual_minor BETWEEN 0 AND 99 AND (amount <> 0 OR residual_minor <> 0)
            AND reason IS NOT NULL AND (reason <> 'OTHER' OR btrim(COALESCE(note, '')) <> '')
            AND reconciliation_uuid IS NOT NULL AND reserve_account_uuid IS NULL
            AND admin_id IS NOT NULL
            AND ((amount = 0) = (journal_uuid IS NULL)))
        OR (kind = 'RESERVE_ARCHIVE'
            AND amount < 0 AND residual_minor = 0
            AND reason IS NULL AND reconciliation_uuid IS NULL AND reserve_account_uuid IS NOT NULL
            AND journal_uuid IS NOT NULL)
    )
);
-- One booking per check: two admins pressing the button at once book it once.
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_adjustments_reconciliation
    ON platform_adjustments (reconciliation_uuid) WHERE reconciliation_uuid IS NOT NULL;
-- The DOKU era is archived once.
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_adjustments_reserve_archive
    ON platform_adjustments (platform_account_uuid) WHERE kind = 'RESERVE_ARCHIVE';
-- The platform statement pages them by (created_at, uuid).
CREATE INDEX IF NOT EXISTS idx_platform_adjustments_statement
    ON platform_adjustments (platform_account_uuid, created_at, uuid);

COMMIT;
