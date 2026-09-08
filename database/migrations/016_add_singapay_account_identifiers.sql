-- Migration: Add Singapay account identifiers to ledger_accounts
-- Purpose: Singapay names one sub-account with TWO different identifiers, and which one
--          an endpoint wants is not negotiable:
--
--            singapay_account_id      ULID, e.g. 01K946KF851RK7FX075GJHBVKF
--                                     Used everywhere — payment link, virtual account,
--                                     QRIS, e-wallet, balance inquiry, statements,
--                                     disbursement, and as the REMITTER of an account
--                                     transfer.
--
--            singapay_account_number  12 digits, e.g. 000000000123
--                                     Used in exactly one place: beneficiary_account_number
--                                     on POST /api/v1.0/account-transfer/{id}/transfer.
--                                     There is no other way to name the destination of a
--                                     transfer, and the ULID is rejected there.
--
--          These are two distinct values, not two encodings of one — neither can be
--          derived from the other. Storing only the ULID is a workable alternative
--          (fetch the number from GET /accounts/{id} when a transfer needs it), but that
--          trades a column for an API call on a path that moves money.
--
--          One call site in this repo needs the number today: the platform-fee transfer's
--          destination (ledger.go ProcessPlatformFeeTransfer, and the inline best-effort
--          transfer during reconciliation). Both swallow their errors and leave the retry
--          job to sort it out — so sending a ULID where a 12-digit number belongs fails
--          silently, forever, with the platform fee sitting in seller balances and nothing
--          raised anywhere. That is the failure this column exists to prevent.
--
--          singapay_account_number is nullable on purpose: Singapay declares it nullable
--          in the create response, so an account can genuinely arrive without one. An
--          account with no number cannot receive a transfer, and that is worth knowing at
--          creation rather than discovering months later.
--
--          doku_subaccount_id is deliberately left in place and untouched.

ALTER TABLE ledger_accounts
    ADD COLUMN IF NOT EXISTS singapay_account_id     VARCHAR(64),
    ADD COLUMN IF NOT EXISTS singapay_account_number VARCHAR(32);

COMMENT ON COLUMN ledger_accounts.singapay_account_id IS
    'Singapay sub-account ULID. Path/body identifier for every endpoint except an account transfer beneficiary.';

COMMENT ON COLUMN ledger_accounts.singapay_account_number IS
    'Singapay 12-digit account number. The only accepted form of beneficiary_account_number on an account transfer. Nullable: Singapay may not assign one at creation.';

-- Partial unique indexes rather than plain UNIQUE constraints, matching the approach
-- taken for settlement_batches.batch_id in migration 013. Postgres treats NULLs as
-- distinct either way, but the WHERE clause states the intent instead of relying on it:
-- many rows legitimately have neither identifier while they are still DOKU-only.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_accounts_singapay_account_id
    ON ledger_accounts (singapay_account_id)
    WHERE singapay_account_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_accounts_singapay_account_number
    ON ledger_accounts (singapay_account_number)
    WHERE singapay_account_number IS NOT NULL;
