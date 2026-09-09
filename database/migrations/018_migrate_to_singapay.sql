-- Migration: move the ledger schema from DOKU to Singapay
--
-- This is the schema half of replacing the payment gateway. The Go code in this repo no
-- longer imports the DOKU client at all, and every column below is renamed or added to
-- match what Singapay actually provides.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- READ THIS FIRST: one step is destructive
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Step 1 DROPs ledger_accounts.doku_subaccount_id. The values in it cannot be recovered
-- afterwards, and nothing in this repo reads them any more. Before running this on an
-- environment that ever transacted through DOKU, take the column somewhere durable:
--
--   CREATE TABLE ledger_accounts_doku_archive AS
--     SELECT uuid, owner_type, owner_id, doku_subaccount_id
--     FROM ledger_accounts
--     WHERE doku_subaccount_id IS NOT NULL;
--
-- That archive is how a historical DOKU settlement or payout gets attributed to an account
-- months from now. Losing it costs nothing today and everything the first time somebody
-- asks which sub-account an old payout came from.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- What this does NOT do
-- ─────────────────────────────────────────────────────────────────────────────
--
-- It does not populate singapay_account_id for existing accounts. There is no derivation
-- from a DOKU sub-account id to a Singapay ULID — they are different accounts at different
-- companies. Every seller has to be provisioned at Singapay, and the platform account has
-- to be created by hand and its ULID *and* 12-digit account number inserted. Until an
-- account carries a Singapay ULID it can neither take a payment nor pay out, and the code
-- refuses both explicitly rather than failing at the API.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. ledger_accounts — drop the DOKU identifier
-- ─────────────────────────────────────────────────────────────────────────────
-- singapay_account_id and singapay_account_number were added in migration 016 and stay as
-- they are. This removes the column they replace. See the archive note above.

-- Dropping the column takes its UNIQUE constraint and that constraint's index with it.
-- An explicit DROP INDEX here would fail: ledger_accounts_doku_subaccount_id_key is owned
-- by the constraint, and Postgres refuses to drop a constraint's index on its own.
ALTER TABLE ledger_accounts DROP COLUMN IF EXISTS doku_subaccount_id;

-- The PAYMENT_GATEWAY account is a bookkeeping account holding recognised gateway fees.
-- Its owner_id named the gateway; it now names the current one.
UPDATE ledger_accounts
   SET owner_id = 'SINGAPAY', updated_at = NOW()
 WHERE owner_type = 'PAYMENT_GATEWAY'
   AND owner_id = 'DOKU';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Fee columns — doku_fee becomes gateway_fee
-- ─────────────────────────────────────────────────────────────────────────────
-- A rename rather than a new column: the values are unchanged in meaning — what the
-- payment gateway charged — and copying them into a new column would leave two sources of
-- truth for the same number on a table that feeds balance arithmetic.

ALTER TABLE product_transactions RENAME COLUMN doku_fee TO gateway_fee;
ALTER TABLE settlement_batches   RENAME COLUMN doku_fee TO gateway_fee;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. fee_configs — DOKU becomes GATEWAY
-- ─────────────────────────────────────────────────────────────────────────────
-- config_type distinguishes the platform's own markup from the gateway's channel fee. The
-- value named the gateway; it now names the role, so the next migration of this kind
-- touches no rows at all.
--
-- Migration 017 already dropped the payment_channel whitelist, because Singapay's channel
-- codes (VA_BCA, EWALLET_DANA) are not the ones that whitelist allowed. Only the
-- config_type constraint is left to rewrite.

DO $$
DECLARE
    c record;
BEGIN
    FOR c IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'fee_configs'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%config_type%'
    LOOP
        EXECUTE format('ALTER TABLE fee_configs DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;

UPDATE fee_configs SET config_type = 'GATEWAY' WHERE config_type = 'DOKU';

ALTER TABLE fee_configs
    ADD CONSTRAINT fee_configs_config_type_check
    CHECK (config_type IN ('PLATFORM', 'GATEWAY'));

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. settlement_batches — a settlement is an event with a window, not a file
-- ─────────────────────────────────────────────────────────────────────────────
-- DOKU published a settlement CSV. Singapay publishes a webhook carrying totals, a
-- settlement method and a date window — and no list of the transactions the batch covered.
-- Those rows have to be reconstructed by replaying the window against the per-product
-- transaction lists, which makes settle_from/settle_to the only handle on a batch's
-- contents. A batch without them can never be reprocessed or audited.
--
-- They are nullable here because rows predating this migration have no window and cannot
-- be given a truthful one. The domain constructor requires both, so nothing written from
-- now on can omit them; the nullability describes history, not intent.

ALTER TABLE settlement_batches RENAME COLUMN report_file_name TO settlement_reference;
ALTER TABLE settlement_batches RENAME COLUMN uploaded_by      TO initiated_by;
ALTER TABLE settlement_batches RENAME COLUMN uploaded_at      TO initiated_at;

ALTER TABLE settlement_batches
    ADD COLUMN IF NOT EXISTS settle_from TIMESTAMP,
    ADD COLUMN IF NOT EXISTS settle_to   TIMESTAMP;

COMMENT ON COLUMN settlement_batches.settlement_reference IS
    'Singapay reference_no for the settlement. Human-facing; batch_id is the idempotency key.';
COMMENT ON COLUMN settlement_batches.settle_from IS
    'Start of the window the settlement covered. Required for new rows: it is the only way to find the transactions the batch contained.';
COMMENT ON COLUMN settlement_batches.settle_to IS
    'End of the window the settlement covered.';
COMMENT ON COLUMN settlement_batches.initiated_by IS
    'Who caused this reconciliation to run: the settlement webhook, or the operator replaying it.';

-- The index from migration 013 follows its column's rename automatically, but its name
-- would keep saying report_file_name. Renamed so an EXPLAIN plan is readable.
ALTER INDEX IF EXISTS idx_settlement_batches_report_file_name
    RENAME TO idx_settlement_batches_settlement_reference;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. settlement_items — a settled gateway transaction, not a CSV row
-- ─────────────────────────────────────────────────────────────────────────────
-- An item is now assembled from a VA, QRIS, e-wallet or payment-link record rather than
-- parsed out of a file, so the CSV-shaped columns go and three gateway-shaped ones arrive.
--
-- fee_reported is the one that matters. Virtual account, QRIS and e-wallet each report the
-- fee Singapay actually took; a payment link reports none, anywhere in the API. For those
-- rows the fee is copied from what was expected at payment time, which makes the fee delta
-- zero by construction. That is the absence of a reconciliation, not a successful one, and
-- it has to be visible in the data rather than inferred from a suspiciously perfect match.

ALTER TABLE settlement_items RENAME COLUMN sub_account  TO gateway_account_id;
ALTER TABLE settlement_items RENAME COLUMN raw_csv_data TO raw_gateway_data;

ALTER TABLE settlement_items
    ADD COLUMN IF NOT EXISTS gateway_transaction_id VARCHAR(100),
    ADD COLUMN IF NOT EXISTS payment_channel        VARCHAR(50),
    ADD COLUMN IF NOT EXISTS fee_reported           BOOLEAN NOT NULL DEFAULT FALSE;

-- csv_row_number described a position in a file. There is no file, and nothing orders
-- items by it any more (the repository orders by created_at).
ALTER TABLE settlement_items DROP COLUMN IF EXISTS csv_row_number;

COMMENT ON COLUMN settlement_items.gateway_account_id IS
    'Singapay sub-account ULID the funds landed in. The check that a settled row belongs to the seller we think it does.';
COMMENT ON COLUMN settlement_items.fee_reported IS
    'FALSE means allocated_fee was copied from the expected fee because the channel reports none (payment link). The fee delta on such a row is zero by construction, not by agreement.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. disbursements — the transfer fee is part of what gets reserved
-- ─────────────────────────────────────────────────────────────────────────────
-- Singapay's disbursement amount is the NET the beneficiary receives; the transfer fee is
-- added on top, so the sub-account is debited amount + fee. A ledger that reserves only the
-- net drifts from Singapay by the fee on every single payout, and the drift is silent: the
-- books stay internally consistent and disagree only with the gateway.
--
-- The fee is quoted before the payout is sent and stored here so the reversal, when one is
-- written, releases exactly what was held. Like payout_request_id, it is written once and
-- never updated on a later save.
--
-- DEFAULT 0 is right for existing rows: they were reserved at the net, so zero is what
-- their reservation actually held.

ALTER TABLE disbursements
    ADD COLUMN IF NOT EXISTS gateway_fee BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN disbursements.gateway_fee IS
    'Transfer fee quoted before the payout was sent. The reservation and its reversal are both amount + gateway_fee.';

-- payout_request_id is Singapay's reference_number now, and it is what an inbound
-- disbursement webhook resolves a payout by — the only identifier this ledger chose and
-- stored before the gateway was called. That lookup needs an index; before this it was
-- only ever read by primary key.
CREATE INDEX IF NOT EXISTS idx_disbursements_payout_request_id
    ON disbursements (payout_request_id)
    WHERE payout_request_id IS NOT NULL;

COMMIT;
