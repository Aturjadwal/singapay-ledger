-- Migration: 022_settlement_notifications.sql
-- Purpose: the inbox for Singapay settlement webhooks.
-- Date: 2026-09-11
--
-- DO NOT RUN THIS AGAINST A LIVE DATABASE WITHOUT READING THE NOTE BELOW.
--
-- ---------------------------------------------------------------------------------------
-- WHY THIS TABLE EXISTS
-- ---------------------------------------------------------------------------------------
-- Singapay announces a settlement on settlement_notif_url with totals and a date window,
-- and no list of the transactions the batch covered. The design that reads this table does
-- NOT try to reconstruct that list from the window. It treats the webhook as a doorbell:
-- the payload is stored verbatim for audit, and the question "which transactions settled?"
-- is answered by asking Singapay about each transaction we are still waiting on, one at a
-- time, through the per-transaction detail endpoints.
--
-- That is why this table carries no foreign key to settlement_batches and no per-seller
-- column. A notification is a merchant-level event. The settlement webhook does not name
-- an account at all (settlement.refunded does; settlement.completed does not), so a
-- per-seller row here would be an invention rather than a record.
--
-- ---------------------------------------------------------------------------------------
-- IDEMPOTENCY: UNIQUE (settlement_id, event)
-- ---------------------------------------------------------------------------------------
-- Webhook redelivery is ordinary traffic, not an error. Singapay retries, and the same
-- settlement can legitimately produce three different events (completed, refunded,
-- refund_cancelled) which must all be stored. So identity is the pair, not the id alone.
--
-- The insert is expected to be written as ON CONFLICT DO NOTHING: a repeat delivery is a
-- success that stores nothing, and the handler answers 200 either way. Answering anything
-- else teaches Singapay to retry a delivery that was already accepted.
--
-- ---------------------------------------------------------------------------------------
-- STATUS
-- ---------------------------------------------------------------------------------------
--   PENDING       accepted, not yet acted on. The worker's input.
--   PROCESSING    a worker has claimed it. Guards against two workers on one row.
--   PROCESSED     the worker finished its pass for this notification.
--   NEEDS_REVIEW  a person has to look. settlement.refunded and refund_cancelled land
--                 here by default: they can pull back funds that are already AVAILABLE and
--                 may already have been withdrawn, and this ledger has no negative-balance
--                 policy. Booking them automatically would be inventing one.
--   FAILED        the pass errored. failure_reason says how. Retried on a later tick.
--
-- Note that PROCESSED does NOT mean "every transaction in the window was settled". It
-- means "the worker ran its pass after seeing this". Whether a given transaction settled
-- is recorded on that transaction, not here. Keeping those two facts apart is deliberate:
-- conflating them is how a partial pass gets recorded as a complete one.

CREATE TABLE IF NOT EXISTS settlement_notifications (
    uuid   VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,

    -- Singapay's own identifiers for the settlement batch.
    settlement_id        VARCHAR(255) NOT NULL,
    settlement_reference VARCHAR(255) NOT NULL,

    -- settlement.completed | settlement.refunded | settlement.refund_cancelled
    event VARCHAR(50) NOT NULL,

    -- balance | auto-balance | bank-account | e-wallet.
    -- Only balance and auto-balance move PENDING into AVAILABLE; bank-account pays out to
    -- a nominated bank account instead and is a different event entirely.
    settlement_method VARCHAR(30),

    -- ALL | VA | QRIS | EWALLET. Scopes the batch by product, not by account.
    settlement_type VARCHAR(20),

    -- The window the batch covered. Stored for audit and for support conversations. The
    -- settling pass does not filter on it: Singapay writes these as text with no offset
    -- ("01 Jun 2026 00:00:00"), so a seven-hour error in either direction is possible and
    -- would move rows between batches. The per-transaction path never needs them.
    start_date TIMESTAMP,
    end_date   TIMESTAMP,

    -- Totals as announced. Reported, never trusted as the basis for a ledger entry.
    total_transactions INT,
    amount             BIGINT,
    total_fee          BIGINT,
    currency           VARCHAR(3),

    -- The delivery exactly as it arrived, after signature verification. This is the
    -- record of what Singapay actually said, which is the only thing worth having when a
    -- settled amount is disputed months later.
    raw_payload JSONB NOT NULL,

    status VARCHAR(20) NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'PROCESSING', 'PROCESSED', 'NEEDS_REVIEW', 'FAILED')
    ),
    failure_reason TEXT,

    received_at  TIMESTAMP NOT NULL,
    processed_at TIMESTAMP,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);

-- Identity. See the note above: the pair, not the id alone.
CREATE UNIQUE INDEX IF NOT EXISTS idx_settlement_notifications_identity
    ON settlement_notifications (settlement_id, event);

-- The worker's only read path: "is there anything to act on?", oldest first. Partial so
-- the index stays the size of the backlog rather than the size of the history.
CREATE INDEX IF NOT EXISTS idx_settlement_notifications_pending
    ON settlement_notifications (received_at)
    WHERE status IN ('PENDING', 'FAILED');

-- For the operator asking "what happened to settlement SETTLEMENT-1-ABC123?"
CREATE INDEX IF NOT EXISTS idx_settlement_notifications_reference
    ON settlement_notifications (settlement_reference);
