-- Migration: 029_activate_credit_card.sql
-- Purpose: offer card payments — activate the CREDIT_CARD fee config that 020 loaded inactive.
-- Date: 2026-09-25
--
-- 020 recorded Singapay's card rate (3,0% + Rp 2.000, HYBRID) but left the row inactive,
-- because the ledger had no way to issue a card payment: an active row would have been
-- priced, listed and accepted by GeneratePayment, then refused at issue time on a payer's
-- booking. It said to flip is_active once card support existed. This is that flip.
--
-- The support is a payment link pinned to the catalogue's card methods (channelCard in
-- gateway.go). Singapay's hosted page takes the card number, CVV and 3-D Secure; the card
-- never touches our servers.
--
-- ---------------------------------------------------------------------------------------
-- Apply AFTER the service runs a ledger version with card support
-- ---------------------------------------------------------------------------------------
-- An older ledger has no route for CREDIT_CARD. With this row active, it would price the
-- channel and let GeneratePayment accept it, then fail issuing it. Applying this first is
-- only harmless while the consuming service still refuses the channel at its edge, so do
-- not rely on that: deploy, then apply.
--
-- ---------------------------------------------------------------------------------------
-- Before applying: confirm the merchant takes cards
-- ---------------------------------------------------------------------------------------
-- Card payments are pinned to whatever Singapay's payment-link catalogue files under the
-- "card" group. A merchant without card enabled has no such method, and every card payment
-- is then refused with ErrUnsupportedPaymentChannel before a link is created. Check first,
-- from a host whose IP Singapay has whitelisted:
--
--   go run ./cmd/singapay-smoke -step methods
--
-- and look for a row in the `card` group. If there is none, ask Singapay to enable cards on
-- payment links before applying this.
--
-- ---------------------------------------------------------------------------------------
-- What a card payment cannot do
-- ---------------------------------------------------------------------------------------
-- A payment link reports no per-transaction fee, so a card payment's fee is never
-- reconciled: settlement books the priced fee (FeeReported = false), exactly as it does for
-- any payment link. If Singapay's real card fee differs from the row below, the difference
-- stays on the seller's Singapay sub-account unobserved. Keep this rate equal to the one in
-- the merchant's contract.
--
-- Reversible: set is_active back to FALSE. Nothing else references the flag.

UPDATE fee_configs
   SET is_active = TRUE,
       updated_at = NOW()
 WHERE config_type = 'GATEWAY'
   AND payment_channel = 'CREDIT_CARD';
