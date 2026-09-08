-- Migration: load Singapay's money-in rate card into fee_configs
--
-- Source: Singapay "Daftar Transaction Fee" rate card.
--
--   Withdraw          Rp 1.200        <- NOT loaded here; see below
--   Virtual Account   Rp 2.800        flat, all banks
--   QRIS              0,7%
--   DANA              2%
--   OVO               3,5%
--   ShopeePay         2%
--   Credit Card       2,7% + Rp 2.000
--
-- All rates are stated as PPN-inclusive, so nothing here grosses up for VAT. Adding 11% on
-- top would over-charge every payer and then show up as a negative fee delta on every
-- settlement.
--
-- fee_configs is the ONLY place the expected fee exists before a transaction settles:
-- Singapay does not publish money-in rates through its API. ListPaymentMethods returns the
-- channel catalogue with no prices, because rates are commercial terms. So this table is
-- not a cache of something authoritative elsewhere — it IS the source, and a wrong row here
-- is a wrong price quoted to a payer.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- Why the withdrawal fee is NOT in this table
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Rp 1.200 is a money-OUT fee. fee_configs holds money-IN channel rates and is read by
-- domain.FeeCalculator, which does two things with every active GATEWAY row: it prices
-- payments, and it feeds GetCheapestChannel, whose answer is shown to payers as a
-- suggestion. A 'WITHDRAW' row would therefore be offered to buyers as a payment method.
--
-- The payout fee is quoted per payout by Singapay's check-fee endpoint and stored on
-- disbursements.gateway_fee (migration 018), because it varies by destination bank and
-- Singapay is the authority on it at the moment the payout is sent.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- Re-running this file UPDATES the rates
-- ─────────────────────────────────────────────────────────────────────────────
--
-- ON CONFLICT DO UPDATE, so a rate change is applied by editing and re-running. That also
-- means it overwrites any hand-tuning done directly in the table — check before re-running
-- if someone has adjusted a row.
--
-- Changing a rate affects FUTURE pricing only. product_transactions.gateway_fee records what
-- was expected at payment time and is never rewritten; that is what a settlement's fee delta
-- is measured against.

BEGIN;

INSERT INTO fee_configs (
    uuid, randid, config_type, payment_channel, name,
    fee_type, fixed_amount, percentage, is_active, created_at, updated_at
)
VALUES
    -- ── QRIS ────────────────────────────────────────────────────────────────────
    -- The one channel whose rate can be verified against reality: a settled QRIS
    -- transaction reports mdr_percentage — the rate itself, not just the amount taken.
    -- Compare it against this row after the first live QRIS payment.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'QRIS', 'QRIS',
     'PERCENTAGE', 0, 0.7, TRUE, NOW(), NOW()),

    -- ── Virtual account ─────────────────────────────────────────────────────────
    -- Rp 2.800 flat, same for every bank — but fee_configs is keyed by payment_channel and
    -- Singapay has no generic 'VIRTUAL_ACCOUNT' code, so this is one row per bank.
    --
    -- TRIM THIS LIST to the banks actually offered. An active row here is a channel the
    -- ledger will price, accept and recommend; a bank the merchant is not enabled for will
    -- fail at Singapay, on a payer's booking.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BCA', 'Virtual Account BCA',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BNI', 'Virtual Account BNI',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BRI', 'Virtual Account BRI',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_MANDIRI', 'Virtual Account Mandiri',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_PERMATA', 'Virtual Account Permata',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_CIMB', 'Virtual Account CIMB',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BSI', 'Virtual Account BSI',
     'FIXED', 2800, 0, TRUE, NOW(), NOW()),

    -- ── E-wallet ────────────────────────────────────────────────────────────────
    -- payment_channel doubles as the ewallet_vendor sent to Singapay, so these strings must
    -- match the catalogue exactly. Confirm them against ListPaymentMethods before enabling —
    -- see the note at the bottom of this file.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'EWALLET_DANA', 'DANA',
     'PERCENTAGE', 0, 2.0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'EWALLET_SHOPEEPAY', 'ShopeePay',
     'PERCENTAGE', 0, 2.0, TRUE, NOW(), NOW()),
    -- OVO is the most expensive channel on the card at 3,5%. It also requires a customer
    -- phone number (push-to-pay); GeneratePaymentRequest.CustomerPhone carries it.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'EWALLET_OVO', 'OVO',
     'PERCENTAGE', 0, 3.5, TRUE, NOW(), NOW()),

    -- ── Credit card ─────────────────────────────────────────────────────────────
    -- HYBRID: fixed + percentage. This is the fee type migration 017 unblocked —
    -- domain.FeeConfig has supported it for a long time but the CHECK constraint rejected it.
    --
    -- INSERTED INACTIVE ON PURPOSE. This ledger has no way to charge a card directly:
    -- PaymentGateway exposes virtual account, QRIS and e-wallet, and nothing else. A card
    -- can only be paid through a payment link, where the payer picks the channel and the
    -- ledger never names it.
    --
    -- Left active, this row would be priced and recommended by GetCheapestChannel as though
    -- it were bookable, and GeneratePayment would then refuse it at issue time — on a
    -- payer's booking. Inactive rows are skipped by NewFeeCalculator, so this records the
    -- rate without offering it. Flip is_active when direct card support exists.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'CREDIT_CARD', 'Credit Card',
     'HYBRID', 2000, 2.7, FALSE, NOW(), NOW())

ON CONFLICT (config_type, payment_channel) DO UPDATE SET
    name         = EXCLUDED.name,
    fee_type     = EXCLUDED.fee_type,
    fixed_amount = EXCLUDED.fixed_amount,
    percentage   = EXCLUDED.percentage,
    is_active    = EXCLUDED.is_active,
    updated_at   = NOW();

-- ─────────────────────────────────────────────────────────────────────────────
-- Retire the placeholder rows
-- ─────────────────────────────────────────────────────────────────────────────
-- schema.sql seeded a generic 'VIRTUAL_ACCOUNT' channel under the previous gateway's
-- naming. Singapay has no such code, so nothing can ever be issued against it — but while
-- it is active, FeeCalculator prices it and GetCheapestChannel can recommend it.
--
-- Deactivated rather than deleted: a historical product_transaction may have been priced
-- from it, and the row is the record of that rate.
UPDATE fee_configs
   SET is_active = FALSE, updated_at = NOW()
 WHERE config_type = 'GATEWAY'
   AND payment_channel IN ('VIRTUAL_ACCOUNT', 'E_WALLET', 'VIRTUAL_ACCOUNT_BCA',
                           'VIRTUAL_ACCOUNT_BNI', 'VIRTUAL_ACCOUNT_BRI',
                           'VIRTUAL_ACCOUNT_MANDIRI');

COMMIT;

-- ─────────────────────────────────────────────────────────────────────────────
-- BEFORE RUNNING THIS: two things to verify
-- ─────────────────────────────────────────────────────────────────────────────
--
-- 1. THE CHANNEL CODES.
--
--    VA_* and EWALLET_* are the prefixes this ledger routes on (gateway.go
--    paymentChannelKind), and VA_BNI / EWALLET_DANA appear verbatim in Singapay's own
--    documentation. The rest — EWALLET_OVO, EWALLET_SHOPEEPAY, and the exact VA bank
--    suffixes — are inferred from that pattern.
--
--    Confirm against the catalogue, which is authoritative:
--
--      go run ./cmd/singapay-smoke -step methods
--
--    A wrong code fails loudly rather than silently: FeeCalculator has no config for it,
--    so GeneratePayment refuses with ErrUnsupportedPaymentChannel. Nothing is mispriced —
--    the channel is simply unusable until the spelling is fixed.
--
-- 2. WHAT THE PERCENTAGE IS CHARGED ON.
--
--    domain.FeeCalculator assumes the gateway takes its percentage of TOTAL CHARGED — what
--    the payer pays — and therefore grosses up so the merchant nets the intended amount:
--
--      total_charged = base_amount / (1 - rate)
--
--    That is the standard MDR convention and almost certainly right. If Singapay instead
--    charges the percentage on the base amount, every percentage channel is off by a small,
--    systematic margin. On a Rp 51.000 base:
--
--      QRIS  0,7%   gross-up 360   vs  base 357    diff  3
--      DANA  2%     gross-up 1.041 vs  base 1.020  diff 21
--      OVO   3,5%   gross-up 1.850 vs  base 1.785  diff 65
--
--    Small per transaction, but it is the same direction every time — so it accumulates as
--    a fee delta on literally every settled row rather than averaging out.
--
--    QRIS settles this exactly. A settled QRIS transaction reports mdr_percentage AND
--    mdr_cost against amount and total_amount, so one real payment answers the question for
--    every percentage channel at once.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- One thing the rate card does not cover
-- ─────────────────────────────────────────────────────────────────────────────
--
-- The card states these rates EXCLUDE Scalev Fee and Scalev Video Fee. If this merchant
-- uses either, Singapay deducts more than the rows above predict — so the expected fee is
-- understated on every transaction, and once reconciliation is implemented every settled row
-- will carry a positive fee delta.
--
-- Under GATEWAY_ON_CUSTOMER the platform absorbs that difference out of its own fee; under
-- GATEWAY_ON_SELLER the seller does. Neither is an error the code can detect — the rows here
-- would simply be wrong. If Scalev is in use, fold its fee into these rates before running.
