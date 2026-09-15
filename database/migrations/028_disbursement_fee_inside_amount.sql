-- Migration: 028_disbursement_fee_inside_amount.sql
-- Purpose: change what disbursements.amount MEANS, and rewrite the existing rows so they
--          still say what they always said.
-- Date: 2026-09-15
--
-- READ THIS BEFORE APPLYING. It rewrites amounts on a money table. It is not additive and
-- it is not reversible by re-running anything.
--
-- ---------------------------------------------------------------------------------------
-- 1. What changed
-- ---------------------------------------------------------------------------------------
-- The transfer fee used to be charged ON TOP of a withdrawal. A seller asking for Rp 15.000
-- against a Rp 3.000 fee had Rp 18.000 taken off their balance and received Rp 15.000:
--
--     amount        15.000   the net the beneficiary received, and what was sent to Singapay
--     gateway_fee    3.000   charged on top
--     ledger debit  18.000   amount + gateway_fee
--
-- That is wrong. The amount a seller asks to withdraw is the whole of what it costs them.
-- The fee comes out of it:
--
--     amount        15.000   what was requested, and the whole of the ledger debit
--     gateway_fee    3.000   deducted from it
--     net           12.000   what is sent to Singapay and what the beneficiary receives
--
-- So `amount` stops meaning "the net that was sent" and starts meaning "what the seller
-- asked for". `gateway_fee` stops being an addition and becomes a deduction. Nothing about
-- Singapay changes — its disbursement amount is still the net and it still adds the fee on
-- top — the ledger simply sends the net instead of the request, which is what makes the
-- sub-account debit land on the requested amount.
--
-- ---------------------------------------------------------------------------------------
-- 2. Why the rows have to be rewritten
-- ---------------------------------------------------------------------------------------
-- The ledger entry that reserved each historical payout was written for amount +
-- gateway_fee. The new code reserves, reverses and retries against `amount` alone. Leaving
-- the old rows as they are would therefore break all three, silently and differently:
--
--   * a reversal on an old PENDING row would release `amount`, leaving `gateway_fee` of the
--     seller's money held against a payout that failed — invisible, and per-row;
--   * a retry would re-send amount - gateway_fee, which for an old row is LESS than the
--     net originally sent, so the beneficiary would be short-paid;
--   * every historical row would read as a withdrawal that cost the seller less than it did.
--
-- Adding gateway_fee into amount fixes all three at once, because for an old row
-- amount + gateway_fee is exactly the debit its ledger entry already carries. After this
-- runs, `amount` equals the reservation for every row, old and new, and every later
-- calculation — reversal, retry net, history display — lands on the same numbers the row
-- has always meant.
--
-- Rows with gateway_fee = 0 (a quote that failed, or a row predating the column) are
-- already correct under both readings and are left alone.
--
-- ---------------------------------------------------------------------------------------
-- 3. ledger_accounts.total_withdrawal_amount
-- ---------------------------------------------------------------------------------------
-- It was incremented by the old `amount` — the net — on every COMPLETED payout, so it
-- under-states what sellers were actually charged by exactly the fees they paid. The same
-- correction is applied, restricted to COMPLETED rows, because those are the only ones that
-- ever incremented it.
--
-- ---------------------------------------------------------------------------------------
-- 4. Not idempotent
-- ---------------------------------------------------------------------------------------
-- Running this twice adds the fee twice. There is no marker column to guard on and no
-- honest way to tell an already-converted row from an unconverted one by looking at it.
-- Run it ONCE, inside the transaction below, and check the before/after counts printed by
-- the verification queries at the foot of this file.
--
-- ---------------------------------------------------------------------------------------
-- 5. Order this against the deploy
-- ---------------------------------------------------------------------------------------
-- The migration and the code change are two halves of one switch, and either half alone is
-- wrong in a different direction:
--
--   old code + converted rows  reserves amount + fee on a row whose amount already
--                              includes the fee — over-reserving by the fee, twice-charged
--   new code + unconverted rows  under-releases reversals and short-pays retries by the fee
--
-- So run them together, and keep the window small:
--
--   1. Stop accepting new withdrawals (or pick a window with none in flight).
--   2. Drain: no disbursement should be left PENDING or PROCESSING. Check with
--        SELECT uuid, status, created_at FROM disbursements
--        WHERE status IN ('PENDING','PROCESSING') ORDER BY created_at;
--      Resolve anything listed with the retry-disbursement command BEFORE converting.
--      A row converted while in flight is the one case that cannot be reasoned about
--      afterwards: its reservation was written by the old rules, its outcome will be booked
--      by the new ones.
--   3. Apply this migration.
--   4. Deploy the ledger version that carries the new Withdraw.
--   5. Run the verification queries below.
--
-- Step 2 is the one that matters. Steps 3 and 4 in the other order is survivable for a few
-- minutes; an in-flight payout straddling the change is not.

BEGIN;

-- The withdrawal rows: fold the fee into the requested amount.
UPDATE disbursements
SET amount     = amount + gateway_fee,
    updated_at = NOW()
WHERE gateway_fee > 0;

-- The per-account total: only COMPLETED payouts ever incremented it.
UPDATE ledger_accounts la
SET total_withdrawal_amount = la.total_withdrawal_amount + fees.total,
    updated_at              = NOW()
FROM (
    SELECT account_uuid, SUM(gateway_fee) AS total
    FROM disbursements
    WHERE gateway_fee > 0
      AND status = 'COMPLETED'
    GROUP BY account_uuid
) AS fees
WHERE la.uuid = fees.account_uuid;

COMMENT ON COLUMN disbursements.amount IS
    'What the seller asked to withdraw, and the whole of what their balance is debited. The transfer fee is taken OUT of it: the beneficiary receives amount - gateway_fee, which is also what is sent to Singapay.';

COMMENT ON COLUMN disbursements.gateway_fee IS
    'Transfer fee quoted before the payout was sent, DEDUCTED from amount rather than added to it. The reservation and its reversal are both amount; the payout sent to Singapay is amount - gateway_fee.';

COMMIT;

-- ---------------------------------------------------------------------------------------
-- Verification (run by hand, after the commit)
-- ---------------------------------------------------------------------------------------
-- Every disbursement's amount should now equal the debit its ledger entry holds. Any row
-- returned here is a row where the two disagree, and each one wants explaining before the
-- new code is trusted with it.
--
--     SELECT d.uuid, d.status, d.amount, d.gateway_fee, le.amount AS reserved
--     FROM disbursements d
--     JOIN ledger_entries le
--       ON le.source_id = d.uuid AND le.entry_type = 'DISBURSEMENT'
--     WHERE le.amount <> d.amount;
--
-- And no row should now be left with nothing to pay out — an amount at or below its own
-- fee would be a payout of zero or less, which the new code refuses at request time:
--
--     SELECT uuid, status, amount, gateway_fee
--     FROM disbursements
--     WHERE gateway_fee > 0 AND amount <= gateway_fee;
