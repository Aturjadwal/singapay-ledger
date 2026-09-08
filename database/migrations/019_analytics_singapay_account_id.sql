-- Migration: rename dim_account.doku_subaccount_id to singapay_account_id
--
-- Runs against the ANALYTICS database, not the ledger one. It is separate from 018 for
-- that reason alone: the two live in different databases and are usually applied by
-- different credentials.
--
-- dim_account is an SCD Type 2 dimension, so the column appears on historical rows too.
-- This renames rather than adds, for the same reason as the fee columns in 018: the column
-- records which gateway sub-account an account was backed by, and the ETL now reads that
-- from ledger_accounts.singapay_account_id. Two columns would mean two answers.
--
-- Historical rows keep their old DOKU values under the new name. That is a known
-- imprecision and the honest alternative — nulling them — would erase the only record of
-- what those rows described. Anything reading this column across the migration boundary
-- should treat the ETL run date as the point the meaning changed.

BEGIN;

ALTER TABLE dim_account RENAME COLUMN doku_subaccount_id TO singapay_account_id;

COMMENT ON COLUMN dim_account.singapay_account_id IS
    'Singapay sub-account ULID from ledger_accounts. Rows loaded before the gateway migration carry the previous gateway''s identifier under this name.';

COMMIT;
