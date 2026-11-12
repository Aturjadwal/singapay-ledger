-- Migration: 032_card_platform_fee_reconciliation.sql
-- Purpose: record where a settled fee came from, and give the card platform-fee
--          reconciliation (LedgerClient.ReconcileCardPlatformFees, v0.11.0) its two tables.
-- Date: 2026-11-12
--
-- ---------------------------------------------------------------------------------------
-- HOW TO APPLY — one transaction, BEFORE the deploy, never across a 13:00 WIB pass
-- ---------------------------------------------------------------------------------------
-- Run the whole file with psql, through the session pooler (:5432) or a direct connection:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f database/migrations/032_card_platform_fee_reconciliation.sql
--
-- It is safe for the code running now: the column is nullable and the tables are new, and
-- nothing in v0.10.x reads either. The new code writes and reads settled_fee_source from its
-- first pass, which the worker runs about two minutes after it boots, so:
--
--   1. Apply this file before deploying the service that carries v0.11.0.
--   2. Let no 13:00 WIB settlement pass run between this file and the deploy. A card that
--      the old code settles in between is left NULL, and the new sweep reads NULL as
--      GATEWAY — it would move that card's fee on the estimate. If a pass did run in
--      between, run this file again right before the deploy.
--   3. Deploy outside 13:00 WIB.
--   4. After the deploy the file may be run once more, as a precaution.
--
-- Running it again is always safe: every statement is IF NOT EXISTS or touches only rows
-- that are still NULL (or SWIMLQ, once).
--
-- Then apply 033 (the index), on its own.
--
-- Check the result:
--
--   SELECT settled_fee_source, count(*) FROM product_transactions
--   WHERE status = 'SETTLED' GROUP BY 1 ORDER BY 1;
--
--   -- MANUAL, once the SWIMLQ correction of 2026-11-11 is in the database:
--   SELECT invoice_number, settled_fee_source, settled_platform_fee, platform_fee_transferred
--   FROM product_transactions WHERE invoice_number = 'INV-20260925161538-SWIMLQ';
--
-- A SETTLED row left NULL settled before migration 024, when settlement journals did not yet
-- record fee_reported. NULL is read as GATEWAY, which is how the sweep has always treated it.
--
-- ---------------------------------------------------------------------------------------
-- WHY
-- ---------------------------------------------------------------------------------------
-- Singapay reports no fee and no net per transaction for a payment link, and a card payment
-- is one. Settlement therefore books a card on the gateway fee quoted at checkout, in whole
-- rupiah, while Singapay takes 3% + Rp2.000 to the sen. When it takes more, the seller's
-- sub-account holds a few sen less than the ledger thinks, and the platform-fee transfer is
-- refused (422 Insufficient remitter account balance) once the seller has withdrawn the rest:
-- INV-20260925161538-SWIMLQ, booked 4041.00, taken 4041.24, 3999.76 left for a 4000.00 sweep.
--
-- The new tables hold the reconciliation that fixes this before the sweep: for each seller,
--
--   card platform share = sub-account available at Singapay
--                       − the seller's AVAILABLE balance in the ledger × 100
--                       − platform fee already final but not transferred yet
--
-- split over the seller's cards. settled_fee_source is what tells the sweep which figures it
-- may move: an ESTIMATE waits until the reconciliation has made it BALANCE.

BEGIN;
SET LOCAL lock_timeout = '5s';

-- 1. Where a settled transaction's fee figures came from.
--      GATEWAY  : reported by Singapay for this transaction (VA, QRIS, e-wallet). Final at
--                 settlement.
--      ESTIMATE : a copy of the checkout estimate (card, payment link). The sweep waits.
--      BALANCE  : recomputed from the seller's sub-account balance. Final.
--      MANUAL   : corrected by hand (SWIMLQ). Final.
--      NULL     : settled before this migration, or not settled. Read as GATEWAY.
ALTER TABLE product_transactions
    ADD COLUMN IF NOT EXISTS settled_fee_source VARCHAR(20)
        CHECK (settled_fee_source IN ('GATEWAY', 'ESTIMATE', 'BALANCE', 'MANUAL'));

-- 2. One row per seller per round: what was read, and what was decided. Every amount is sen.
CREATE TABLE IF NOT EXISTS platform_fee_reconciliations (
    uuid                         VARCHAR(255) PRIMARY KEY,
    randid                       VARCHAR(255) NOT NULL UNIQUE,
    seller_account_id            VARCHAR(255) NOT NULL REFERENCES ledger_accounts(uuid),
    singapay_account_id          VARCHAR(255) NOT NULL,
    singapay_available           BIGINT,          -- NULL when the balance was not read
    singapay_pending             BIGINT,
    singapay_held                BIGINT,
    seller_ledger_available      BIGINT NOT NULL, -- the seller's AVAILABLE balance × 100
    certain_unswept_platform_fee BIGINT NOT NULL, -- final platform fee not transferred yet
    computed_platform_fee        BIGINT,          -- available − seller − certain
    nominal_platform_fee         BIGINT NOT NULL, -- the cards' platform fee as priced
    tolerance                    BIGINT NOT NULL, -- the allowed deviation: per card × cards
    card_count                   INT NOT NULL CHECK (card_count > 0),
    outcome                      VARCHAR(20) NOT NULL CHECK (outcome IN ('APPLIED', 'HELD')),
    hold_reason                  VARCHAR(50) CHECK (hold_reason IN (
                                     'DEVIATION_ABOVE_TOLERANCE',
                                     'DISBURSEMENT_IN_FLIGHT',
                                     'BALANCE_READ_FAILED'
                                 )),
    raw_balance                  JSONB,           -- the balance inquiry's answer, as decoded
    created_at                   TIMESTAMP NOT NULL,
    CONSTRAINT platform_fee_reconciliations_outcome_reason_check
        CHECK ((outcome = 'HELD') = (hold_reason IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_platform_fee_reconciliations_seller
    ON platform_fee_reconciliations (seller_account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_fee_reconciliations_held
    ON platform_fee_reconciliations (created_at DESC) WHERE outcome = 'HELD';

-- 3. One row per card per round. On a HELD round the result columns are NULL.
CREATE TABLE IF NOT EXISTS platform_fee_reconciliation_items (
    uuid                          VARCHAR(255) PRIMARY KEY,
    randid                        VARCHAR(255) NOT NULL UNIQUE,
    reconciliation_uuid           VARCHAR(255) NOT NULL REFERENCES platform_fee_reconciliations(uuid),
    product_transaction_uuid      VARCHAR(255) NOT NULL REFERENCES product_transactions(uuid),
    priced_platform_fee           BIGINT NOT NULL, -- sen
    previous_settled_gateway_fee  BIGINT NOT NULL, -- sen
    previous_settled_platform_fee BIGINT NOT NULL, -- sen
    allocated_platform_fee        BIGINT,          -- sen: the recomputed platform share
    settled_gateway_fee           BIGINT,          -- sen: total_charged×100 − seller_net×100 − allocated
    platform_residual             BIGINT,          -- sen: allocated − (allocated / 100) × 100
    journal_uuid                  VARCHAR(255) REFERENCES journals(uuid),
    ledger_entry_uuid             VARCHAR(255) REFERENCES ledger_entries(uuid), -- NULL when the rupiah part did not change
    created_at                    TIMESTAMP NOT NULL,
    CONSTRAINT platform_fee_reconciliation_items_applied_check
        CHECK ((allocated_platform_fee IS NULL) = (settled_gateway_fee IS NULL)
           AND (allocated_platform_fee IS NULL) = (platform_residual IS NULL)
           AND (allocated_platform_fee IS NULL) = (journal_uuid IS NULL))
);
-- A transaction's platform fee is recomputed (APPLIED) once.
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_fee_reconciliation_items_applied
    ON platform_fee_reconciliation_items (product_transaction_uuid)
    WHERE allocated_platform_fee IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_platform_fee_reconciliation_items_run
    ON platform_fee_reconciliation_items (reconciliation_uuid);

-- 4. Backfill from the SETTLEMENT journals, which have recorded fee_reported since migration
--    024. Only rows still NULL, so it is idempotent.
UPDATE product_transactions pt
   SET settled_fee_source = CASE WHEN (j.metadata ->> 'fee_reported')::boolean THEN 'GATEWAY' ELSE 'ESTIMATE' END
  FROM journals j
 WHERE j.source_type = 'PRODUCT_TRANSACTION'
   AND j.source_id = pt.uuid
   AND j.event_type = 'SETTLEMENT'
   AND j.metadata ? 'fee_reported'
   AND pt.status = 'SETTLED'
   AND pt.settled_fee_source IS NULL;

-- SWIMLQ, once the manual correction of 2026-11-11 (koreksi_swimlq_fee_kartu.sql) is in the
-- database: its figures are the corrected ones, final, and not the reconciliation's to touch.
UPDATE product_transactions pt
   SET settled_fee_source = 'MANUAL'
 WHERE pt.settled_fee_source IS DISTINCT FROM 'MANUAL'
   AND EXISTS (SELECT 1 FROM journals j
                WHERE j.source_type = 'PRODUCT_TRANSACTION'
                  AND j.source_id = pt.uuid
                  AND j.metadata ->> 'koreksi' = 'koreksi-fee-kartu-SWIMLQ');

COMMIT;
