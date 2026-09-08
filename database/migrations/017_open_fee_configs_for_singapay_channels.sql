-- Migration: Let fee_configs hold Singapay channels and HYBRID fees
-- Purpose: Two CHECK constraints on fee_configs reject rows the system needs.
--
--          1. payment_channel is a closed whitelist written for DOKU's channel names:
--
--               'QRIS', 'VIRTUAL_ACCOUNT_MANDIRI', 'VIRTUAL_ACCOUNT_BCA',
--               'VIRTUAL_ACCOUNT_BNI', 'VIRTUAL_ACCOUNT_BRI', 'VIRTUAL_ACCOUNT',
--               'CREDIT_CARD', 'E_WALLET', 'PLATFORM'
--
--             Singapay spells its channels differently — VA_BRI, VA_BNI, EWALLET_DANA —
--             and those spellings are not interchangeable: they come from
--             GET /api/v1.0/payment-link-manage/payment-methods and are the only form
--             any Singapay endpoint accepts. 'QRIS' happens to coincide; nothing else
--             does. Inserting a Singapay fee config today fails at the database.
--
--             The whitelist is dropped rather than extended. The authoritative list of
--             channels lives at the gateway and changes when the gateway adds one; a
--             copy pinned in a CHECK constraint can only ever be a stale copy, and the
--             failure it produces (a rejected INSERT during a fee-table update) is worse
--             than the typo it guards against. FeeCalculator already refuses a channel
--             it has no config for, which is the check that actually matters.
--
--          2. fee_type allows only FIXED and PERCENTAGE, but domain.FeeConfig has
--             defined FeeTypeHybrid = 'HYBRID' since before this migration — fixed
--             amount plus a percentage, which is how several gateway channels price.
--             GetFeeBreakdownWithOptions implements it and no migration ever added it
--             to the constraint, so a HYBRID config cannot be saved. That is a
--             pre-existing bug, unrelated to Singapay, fixed here because this
--             migration is already rewriting the constraint next to it.

-- Constraint names: an inline column CHECK gets an auto-generated name
-- (<table>_<column>_check), but that is a convention rather than a guarantee, and this
-- schema has been through enough hands to be worth not assuming. Drop whatever check
-- constraints actually reference each column.
DO $$
DECLARE
    c record;
BEGIN
    FOR c IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'fee_configs'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%payment_channel%'
    LOOP
        EXECUTE format('ALTER TABLE fee_configs DROP CONSTRAINT %I', c.conname);
    END LOOP;

    FOR c IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'fee_configs'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%fee_type%'
    LOOP
        EXECUTE format('ALTER TABLE fee_configs DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;

-- payment_channel is left unconstrained beyond NOT NULL-ness, which it never had.
-- fee_type keeps a constraint: unlike a channel list, these three values are defined by
-- this codebase and adding a fourth is a code change, not a gateway change.
ALTER TABLE fee_configs
    ADD CONSTRAINT fee_configs_fee_type_check
    CHECK (fee_type IN ('FIXED', 'PERCENTAGE', 'HYBRID'));

COMMENT ON COLUMN fee_configs.payment_channel IS
    'Gateway channel code, as spelled by the gateway itself (Singapay: VA_BRI, QRIS, EWALLET_DANA — from GET /payment-link-manage/payment-methods). Intentionally unconstrained: the authoritative list lives at the gateway.';

-- The UNIQUE(config_type, payment_channel) constraint is untouched and still does its
-- job: one fee config per gateway per channel.
