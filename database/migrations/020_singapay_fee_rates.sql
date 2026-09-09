-- Migration: load Singapay's money-in rate card into fee_configs
--
-- Source: the merchant's Singapay channel pricing screen (final commercial terms).
--
--   Virtual Account   Rp 3.000        except Permata and Maybank at Rp 2.500
--   QRIS Acquirer     0,7%
--   E-Wallet          2,5%            DANA, OVO, ShopeePay — all three the same
--   Credit Card       3,0% + Rp 2.000
--   Offline Store     Rp 3.000        <- NOT loaded here; see below
--
--   Money out         Rp 3.000 bank transfer, Rp 2.500 e-wallet
--                                     <- NOT loaded here; see below
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
-- Why the money-out fees are NOT in this table
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Rp 3.000 (bank transfer) and Rp 2.500 (e-wallet disbursement) are money-OUT fees.
-- fee_configs holds money-IN channel rates and is read by domain.FeeCalculator, which does
-- two things with every active GATEWAY row: it prices payments, and it feeds
-- GetCheapestChannel, whose answer is shown to payers as a suggestion. A payout row would
-- therefore be offered to buyers as a payment method.
--
-- The payout fee is quoted per payout by Singapay's check-fee endpoint and stored on
-- disbursements.gateway_fee (migration 018), because it varies by destination — Rp 3.000 to
-- a bank, Rp 2.500 to a wallet, Rp 450.000 cross-border — and Singapay is the authority on
-- it at the moment the payout is sent. The numbers above are here so a quote that comes back
-- wildly different is recognisable as wrong; they are not what the ledger reserves against.
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
    --
    -- This is the QRIS *acquirer* rate, which is what the ledger pays on money in. The
    -- issuer rate on the pricing screen (0,7% + Rp 15) is a money-out number and does not
    -- belong here.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'QRIS', 'QRIS',
     'PERCENTAGE', 0, 0.7, TRUE, NOW(), NOW()),

    -- ── Virtual account, Rp 3.000 ───────────────────────────────────────────────
    -- fee_configs is keyed by payment_channel and Singapay has no generic
    -- 'VIRTUAL_ACCOUNT' code, so this is one row per bank even where the price is identical.
    --
    -- These are the banks the merchant is enabled for. Singapay also issues for OCBC
    -- (singapay.BankOCBC), but it is not on this rate card, so there is no row for it — an
    -- active row without a price the merchant has actually agreed to is a guess quoted to a
    -- payer. TRIM THIS LIST if any of these are later switched off: an active row is a
    -- channel the ledger will price, accept and recommend, and a bank the merchant is not
    -- enabled for fails at Singapay, on a payer's booking.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BCA', 'Virtual Account BCA',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BNI', 'Virtual Account BNI',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BRI', 'Virtual Account BRI',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_MANDIRI', 'Virtual Account Mandiri',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_CIMB', 'Virtual Account CIMB',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BSI', 'Virtual Account BSI',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_DANAMON', 'Virtual Account Danamon',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_MUAMALAT', 'Virtual Account Muamalat',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_BNC', 'Virtual Account BNC',
     'FIXED', 3000, 0, TRUE, NOW(), NOW()),

    -- ── Virtual account, Rp 2.500 ───────────────────────────────────────────────
    -- Permata and Maybank are the two cheap banks on the card. Because every VA row is
    -- FIXED and GetCheapestChannel compares actual amounts, these two win the VA comparison
    -- outright — which is correct, and worth knowing before someone "tidies" the list into
    -- one uniform price.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_PERMATA', 'Virtual Account Permata',
     'FIXED', 2500, 0, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'VA_MAYBANK', 'Virtual Account Maybank',
     'FIXED', 2500, 0, TRUE, NOW(), NOW()),

    -- ── E-wallet ────────────────────────────────────────────────────────────────
    -- All three wallets price at 2,5% on this card — OVO is no longer the expensive one it
    -- was on the previous rate sheet, so nothing here should be steering payers between
    -- wallets on price.
    --
    -- payment_channel doubles as the ewallet_vendor sent to Singapay, so these strings must
    -- match the catalogue exactly. Confirm them against ListPaymentMethods before enabling —
    -- see the note at the bottom of this file.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'EWALLET_DANA', 'DANA',
     'PERCENTAGE', 0, 2.5, TRUE, NOW(), NOW()),
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'EWALLET_SHOPEEPAY', 'ShopeePay',
     'PERCENTAGE', 0, 2.5, TRUE, NOW(), NOW()),
    -- OVO requires a customer phone number (push-to-pay);
    -- GeneratePaymentRequest.CustomerPhone carries it.
    (gen_random_uuid()::text, substring(md5(random()::text) from 1 for 16),
     'GATEWAY', 'EWALLET_OVO', 'OVO',
     'PERCENTAGE', 0, 2.5, TRUE, NOW(), NOW()),

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
     'HYBRID', 2000, 3.0, FALSE, NOW(), NOW())

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
-- What the rate card lists and this file does not load
-- ─────────────────────────────────────────────────────────────────────────────
--
-- OFFLINE STORE (Alfamart and friends, Rp 3.000). Not a code paymentChannelKind routes:
-- it is neither QRIS nor VA_* nor EWALLET_*, so GeneratePayment resolves it to
-- channelUnknown and refuses. Like a credit card it is reachable only through a payment
-- link, where the ledger never names the channel — but unlike the credit card there is no
-- confirmed channel code for it here, so there is nothing truthful to record even as an
-- inactive row. Add it when the code is known and the ledger can route it.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- BEFORE RUNNING THIS: two things to verify
-- ─────────────────────────────────────────────────────────────────────────────
--
-- 1. THE CHANNEL CODES.
--
--    VA_* and EWALLET_* are the prefixes this ledger routes on (gateway.go
--    paymentChannelKind), and VA_BNI / EWALLET_DANA appear verbatim in Singapay's own
--    documentation. The rest — EWALLET_OVO, EWALLET_SHOPEEPAY, and the exact VA bank
--    suffixes — are inferred from that pattern. The bank halves are known good:
--    vaBankFromChannel accepts every bank named above.
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
--      QRIS      0,7%   gross-up 360   vs  base 357    diff  3
--      E-wallet  2,5%   gross-up 1.308 vs  base 1.275  diff 33
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
-- The earlier rate sheet stated its rates EXCLUDE Scalev Fee and Scalev Video Fee, and the
-- pricing screen these numbers come from does not say either way. If this merchant uses
-- either, Singapay deducts more than the rows above predict — so the expected fee is
-- understated on every transaction, and once reconciliation is implemented every settled row
-- will carry a positive fee delta.
--
-- Under GATEWAY_ON_CUSTOMER the platform absorbs that difference out of its own fee; under
-- GATEWAY_ON_SELLER the seller does. Neither is an error the code can detect — the rows here
-- would simply be wrong. If Scalev is in use, fold its fee into these rates before running.
