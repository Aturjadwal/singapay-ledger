-- Migration: 023_payment_request_gateway_transaction.sql
-- Purpose: persist the gateway's own transaction identifiers, so a settled transaction can
--          be looked up directly instead of searched for.
-- Date: 2026-09-11
--
-- ---------------------------------------------------------------------------------------
-- THE PROBLEM THESE COLUMNS SOLVE
-- ---------------------------------------------------------------------------------------
-- payment_requests.request_id holds the id Singapay returned when the payment INSTRUMENT
-- was created. For QRIS and e-wallet that happens to be the transaction id too, because
-- those products create the transaction up front. For the other two it is not:
--
--   VA            request_id is the virtual account's ULID. The VA is a container; the
--                 transaction is the payment that later arrives in it, and it carries its
--                 own business id ("VA-20251024-0001H9X8ZK"). Different entity.
--   Payment Link  request_id is the link's id. The detail endpoint wants the id of one
--                 payment ATTEMPT against that link. Different entity again.
--
-- So GET /va-transactions/{account_id}/{transaction_id} cannot be called with what we
-- store, and neither can the payment-link equivalent. Without these columns the settling
-- pass has to fall back to a search — by VA number for VA, by reff_no for payment link —
-- which works but is a list call where a point read would do.
--
-- The identifiers are already in hand and already being thrown away: the money-in webhook
-- carries both (singapay.MoneyInTransaction.ID and .TransactionID), and HandlePaymentSuccess
-- currently writes TransactionID into the journal's metadata JSONB and nowhere queryable.
-- These two columns are that same value, in a column.
--
-- ---------------------------------------------------------------------------------------
-- WHY TWO COLUMNS
-- ---------------------------------------------------------------------------------------
-- Singapay's money-in webhook reports two identifiers and the four products disagree about
-- which one the detail endpoint wants:
--
--   gateway_transaction_id   MoneyInTransaction.ID — the numeric primary key. This is what
--                            /qris-dynamic/{acc}/show/{id} and the e-wallet and
--                            payment-link detail endpoints take.
--   gateway_transaction_ref  MoneyInTransaction.TransactionID — the business id. This is
--                            what /va-transactions/{acc}/{transaction_id} takes.
--
-- Storing both costs two VARCHARs and removes a per-channel guess from the read path.
-- Both are VARCHAR rather than BIGINT on purpose: one of them is not a number, and a
-- gateway identifier is an opaque token whatever it looks like today.
--
-- ---------------------------------------------------------------------------------------
-- NULLS AND BACKFILL
-- ---------------------------------------------------------------------------------------
-- Nullable, with no default. Every row that predates this migration has NULL, which the
-- read path treats as "no direct key — use the per-channel fallback", not as an error.
-- A backfill for rows already in COMPLETED is a separate, idempotent script; it is not
-- required for the settling pass to work.

ALTER TABLE payment_requests
    ADD COLUMN IF NOT EXISTS gateway_transaction_id VARCHAR(100);

ALTER TABLE payment_requests
    ADD COLUMN IF NOT EXISTS gateway_transaction_ref VARCHAR(100);

-- Supports the reverse lookup: given a gateway identifier from a webhook, a report or a
-- support ticket, find the payment it belongs to. Not unique — a NULL-heavy column, and
-- uniqueness here would be a claim about Singapay's id space that we cannot make.
CREATE INDEX IF NOT EXISTS idx_payment_requests_gateway_transaction
    ON payment_requests (gateway_transaction_id)
    WHERE gateway_transaction_id IS NOT NULL;
