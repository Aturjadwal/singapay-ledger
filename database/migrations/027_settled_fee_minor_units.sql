-- Migration: 027_settled_fee_minor_units.sql
-- Purpose: move the settled fee columns to minor units (sen), and add the column that
--          records the sub-rupiah residual the platform carries.
--          The columns KEEP THEIR NAMES and change meaning — see DEPLOY ORDER in section 3.
-- Date: 2026-09-14
--
-- READ ALL OF SECTION 1 BEFORE APPLYING. The existing data in these two columns is in two
-- different units depending on the payment channel, and a blanket × 100 corrupts half of it.
--
-- ---------------------------------------------------------------------------------------
-- 1. Why the existing values are not all in the same unit
-- ---------------------------------------------------------------------------------------
-- Singapay reports a money-in fee with two decimals: a QRIS fee of 119.84 is a real figure.
-- The client type that reads it (singapay.Amount) stores sen and never loses it. But
-- readSettledTransaction filled domain.SettledTransaction by calling .Minor() on those
-- amounts — sen — for virtual account, QRIS and e-wallet, while the payment link branch
-- filled the same fields from product_transactions.gateway_fee, which is whole rupiah.
--
-- Nothing downstream knew the difference, because the struct's fields were named
-- GrossAmount / NetAmount / Fee and carried no unit. resolveFeeAdjustment then computed
--
--     feeDelta = settled.Fee - tx.Fee.GatewayFee        -- sen minus rupiah
--
-- so a real fee of Rp120 read as 12000 against an expected 120 produced a delta of 11880.
--
-- What that did depends on the channel:
--
--   VA / QRIS / E-WALLET   the delta was ~100× the fee. adjustedPlatformFee went negative
--                          for any ordinary transaction and the settlement was BLOCKED —
--                          the transaction stayed COMPLETED and nothing was written. The
--                          bug was loud here, which is why little data exists. It only got
--                          through where the platform fee happened to exceed roughly 100×
--                          the gateway fee, and those rows booked a platform fee far below
--                          what was earned.
--
--   PAYMENT LINK           a payment link reports no fee at all, so the branch copied the
--                          expected fee into the actual and the delta was zero by
--                          construction. Those rows are correct, and they are in RUPIAH.
--
-- So: payment-link rows must be multiplied by 100; every other channel's settled_gateway_fee
-- is already sen and must NOT be. settled_platform_fee is rupiah on every row that exists —
-- it was always derived from tx.Fee.PlatformFee, which is rupiah — so it scales uniformly.
--
-- ---------------------------------------------------------------------------------------
-- 2. Inspect before you apply
-- ---------------------------------------------------------------------------------------
-- Run this first. It is the whole population this migration touches, and it is expected to
-- be small. Any row outside PAYMENT_LINK with a non-NULL settled_gateway_fee settled while
-- the unit bug was live and its booked platform fee should be checked by hand against the
-- gateway before you trust it.
--
--     SELECT pr.payment_channel,
--            count(*)                                AS rows,
--            min(pt.settled_gateway_fee)             AS min_gateway_fee,
--            max(pt.settled_gateway_fee)             AS max_gateway_fee,
--            min(pt.settled_platform_fee)            AS min_platform_fee,
--            max(pt.settled_platform_fee)            AS max_platform_fee
--     FROM product_transactions pt
--     JOIN payment_requests pr ON pr.product_transaction_uuid = pt.uuid
--     WHERE pt.settled_gateway_fee IS NOT NULL
--        OR pt.settled_platform_fee IS NOT NULL
--     GROUP BY pr.payment_channel
--     ORDER BY rows DESC;
--
-- An empty payment_channel in that output is a payment link, not missing data.
--
-- ---------------------------------------------------------------------------------------
-- 3. platform_residual
-- ---------------------------------------------------------------------------------------
-- The platform sub-account is the balancing account for the gateway fee delta, and that
-- delta is routinely a fraction of a rupiah. Singapay's account-transfer endpoint takes a
-- decimal amount, so the sweep moves the fraction; ledger_entries.amount is whole rupiah
-- and cannot express it. This column holds what the entry could not:
--
--     platform_residual = settled_platform_fee - (ledger entry rupiah × 100)
--
-- It is how the platform's ledger balance and its Singapay balance are reconciled — their
-- difference is SUM(platform_residual) over settled rows, exactly, and a query that returns
-- a number is a discrepancy that can be explained. The seller's two balances agree to the
-- sen with no residual at all, which is the invariant this whole mechanism protects.
--
-- It is in sen like the two columns beside it, and carries no _minor suffix for the same
-- reason they do not: all three are sen, and naming only the new one for its unit would
-- imply the other two are rupiah, which is the confusion this migration exists to end.
--
-- NULLABLE ON PURPOSE, for the same reason as migration 024's columns: NULL means "this row
-- settled before the residual was recorded", not zero.
--
-- ---------------------------------------------------------------------------------------
-- DEPLOY ORDER — the columns keep their names, so nothing fails loudly
-- ---------------------------------------------------------------------------------------
-- settled_platform_fee and settled_gateway_fee keep their names and change meaning, which
-- means a build that predates this migration will read sen and believe it is rupiah. There
-- is no error when that happens: ProcessPlatformFeeTransfer would sweep a settled fee of
-- 400016 as Rp400,016 instead of Rp4,000.16 — a hundredfold transfer out of a seller's
-- sub-account that the gateway has no reason to refuse.
--
-- So the old build must be STOPPED BEFORE this migration is applied, not after:
--
--     1. stop the settlement worker and the platform-fee transfer command
--     2. apply this migration
--     3. deploy the new build
--
-- Renaming the columns would have made step 1 unnecessary by turning that window into a
-- loud failure. It was dropped deliberately — the columns are near-empty on staging and the
-- rename would have churned a dozen queries for a window that can be closed by ordering
-- instead. On a populated production database, reconsider that trade.

-- 3.1 settled_platform_fee was rupiah on every row that exists, whatever the channel.
UPDATE product_transactions
SET settled_platform_fee = settled_platform_fee * 100
WHERE settled_platform_fee IS NOT NULL;

-- 3.2 settled_gateway_fee was rupiah ONLY for payment link. Everything else was already sen.
--
--     An EMPTY payment_channel is a payment link too, and this is the easy row to miss:
--     paymentChannelKind routes both '' and 'PAYMENT_LINK' to the same branch, because a
--     caller that pins no channel gets a link and lets the payer choose. Subscriptions take
--     exactly that path, so '' is not a rare edge here — it is most of them.
UPDATE product_transactions pt
SET settled_gateway_fee = pt.settled_gateway_fee * 100
FROM payment_requests pr
WHERE pr.product_transaction_uuid = pt.uuid
  AND pt.settled_gateway_fee IS NOT NULL
  AND (pr.payment_channel = 'PAYMENT_LINK' OR pr.payment_channel = '');

-- 3.3 The residual column.
ALTER TABLE product_transactions
    ADD COLUMN IF NOT EXISTS platform_residual BIGINT;

-- 3.4 Rows settled before this migration carried no residual: the old arithmetic worked in
--     whole rupiah, so there was never a fraction to strand. Zero is the truthful value for
--     them, and it is what keeps SUM(platform_residual) a complete answer rather than one
--     that silently skips history.
UPDATE product_transactions
SET platform_residual = 0
WHERE settled_platform_fee IS NOT NULL
  AND platform_residual IS NULL;

-- ---------------------------------------------------------------------------------------
-- 4. Verify after applying
-- ---------------------------------------------------------------------------------------
-- Every settled fee should now be sen. A payment-link row's gateway fee must be an exact
-- multiple of 100 (its fee is a copy of the whole-rupiah estimate); other channels may
-- carry any two-decimal value. This should return no rows:
--
--     SELECT pt.uuid, pr.payment_channel, pt.settled_gateway_fee
--     FROM product_transactions pt
--     JOIN payment_requests pr ON pr.product_transaction_uuid = pt.uuid
--     WHERE (pr.payment_channel = 'PAYMENT_LINK' OR pr.payment_channel = '')
--       AND pt.settled_gateway_fee IS NOT NULL
--       AND pt.settled_gateway_fee % 100 <> 0;
