-- Migration: widen payment_requests.payment_code so a QRIS payload fits
--
-- payment_code holds whatever string the buyer needs in order to pay, and what that string
-- is depends entirely on the channel:
--
--   Virtual Account   the VA number             10-16 digits
--   QRIS              the whole EMVCo payload   ~250-400 characters
--   E-wallet / link   nothing                   those return a hosted page in payment_url
--
-- VARCHAR(100) was sized for the VA case, and it held as long as QRIS came with a hosted
-- page of its own. Singapay's GenerateQRIS has no page: qr.QRData IS the code, and the
-- caller renders it. So the first QRIS payment after the Singapay cutover failed at
-- insert:
--
--   pq: value too long for type character varying(100) (22001)
--
-- The failure lands *after* the QR is issued at Singapay, which is the part that makes it
-- worse than a plain constraint error: the payment instrument exists at the gateway, and
-- the rows that describe it do not. A buyer who scanned it would be paying into an invoice
-- the ledger never recorded.
--
-- TEXT is the right type here for the same reason payment_url is TEXT: the length is the
-- gateway's to decide, not ours, and a second gateway would pick a different one again.
--
-- Notes on applying it:
--   * varchar(n) -> text is binary coercible, so Postgres drops the length constraint
--     without rewriting the table. It still rebuilds idx_payment_requests_payment_code
--     under an ACCESS EXCLUSIVE lock — brief at this table's size, but it is a lock.
--   * That index stays valid afterwards: a QRIS payload is a few hundred bytes, well under
--     the ~2704-byte btree entry limit.
--
-- Reversing it is only safe while no QRIS row has been written; narrowing the column back
-- to VARCHAR(100) would fail on the first stored payload (or truncate it, if forced).

BEGIN;

ALTER TABLE payment_requests
    ALTER COLUMN payment_code TYPE TEXT;

COMMENT ON COLUMN payment_requests.payment_code IS
    'Channel-dependent code the buyer pays with: a VA number, or the full EMVCo QRIS payload to render as a QR. Null for channels that hand back a hosted page in payment_url instead.';

COMMIT;
