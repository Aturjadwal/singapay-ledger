CREATE TABLE ledger_accounts (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    -- Singapay names one sub-account with two different identifiers (migration 016).
    -- singapay_account_id is the ULID used by every endpoint; singapay_account_number is
    -- the 12-digit form, accepted only as an account transfer's beneficiary and nullable
    -- because Singapay may not assign one at creation.
    singapay_account_id VARCHAR(64),
    singapay_account_number VARCHAR(32),
    owner_type VARCHAR(20) NOT NULL CHECK (
        owner_type IN (
            'SELLER',
            'PLATFORM',
            'PAYMENT_GATEWAY',
            'RESERVE'
        )
    ),
    owner_id VARCHAR(255),
    -- e.g. seller_id for SELLER ledger_accounts, platform for PLATFORM ledger_accounts,
    -- payment gateway name for PAYMENT_GATEWAY_EXPENSE ledger_accounts, etc.
    currency VARCHAR(3) NOT NULL,
    pending_balance BIGINT NOT NULL DEFAULT 0,
    available_balance BIGINT NOT NULL DEFAULT 0,
    total_withdrawal_amount BIGINT NOT NULL DEFAULT 0,
    total_deposit_amount BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- Ensure there is at most one PLATFORM and one PAYMENT_GATEWAY account
CREATE UNIQUE INDEX idx_accounts_unique_platform ON ledger_accounts(owner_type)
WHERE
    owner_type = 'PLATFORM';

CREATE UNIQUE INDEX idx_accounts_unique_payment_gateway ON ledger_accounts(owner_type)
WHERE
    owner_type = 'PAYMENT_GATEWAY';

-- Singapay identifiers (migration 016). Partial, because most rows legitimately have
-- neither: the PAYMENT_GATEWAY expense account has no sub-account behind it, and a
-- sub-account can be issued without a number.
CREATE UNIQUE INDEX idx_ledger_accounts_singapay_account_id ON ledger_accounts(singapay_account_id)
WHERE
    singapay_account_id IS NOT NULL;

CREATE UNIQUE INDEX idx_ledger_accounts_singapay_account_number ON ledger_accounts(singapay_account_number)
WHERE
    singapay_account_number IS NOT NULL;

-- Journals: Represents atomic accounting events
-- Each journal groups related ledger_entries into a single business event
-- Examples: payment success, settlement batch, disbursement, reconciliation
CREATE TABLE journals (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    -- Type of financial event
    event_type VARCHAR(50) NOT NULL CHECK (
        event_type IN (
            'PAYMENT_SUCCESS',
            'SETTLEMENT',
            'DISBURSEMENT',
            'RECONCILIATION',
            'MANUAL_ADJUSTMENT'
        )
    ),
    -- What business entity triggered this journal
    -- 'SETTLEMENT_BATCH' is never written any more — settlement_batches was dropped by
    -- migration 025 — but rows the old batch reconciler booked still carry it, and
    -- journals are insert-only. It stays so that history remains readable.
    source_type VARCHAR(50) NOT NULL CHECK (
        source_type IN (
            'PRODUCT_TRANSACTION',
            'SETTLEMENT_BATCH',
            'DISBURSEMENT',
            'MANUAL_ADJUSTMENT'
        )
    ),
    source_id VARCHAR(255) NOT NULL,
    -- Additional context about the event
    metadata JSONB,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP NOT NULL DEFAULT now()
);

CREATE INDEX idx_journals_source ON journals(source_type, source_id);

CREATE INDEX idx_journals_event_type ON journals(event_type);

CREATE INDEX idx_journals_created ON journals(created_at DESC);

CREATE TABLE ledger_entries (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    -- Double-entry grouping - links to atomic accounting event
    journal_uuid VARCHAR(255) NOT NULL REFERENCES journals(uuid),
    -- Account affected
    account_uuid VARCHAR(255) NOT NULL REFERENCES ledger_accounts(uuid),
    -- Money movement
    amount BIGINT NOT NULL,
    -- positive = credit, negative = debit
    balance_bucket VARCHAR(10) NOT NULL CHECK (
        balance_bucket IN ('PENDING', 'AVAILABLE')
    ),
    -- Financial classification (accounting meaning)
    entry_type VARCHAR(50) NOT NULL CHECK (
        entry_type IN (
            'PRODUCT_PAYMENT',
            'PLATFORM_COMMISSION',
            'PROCESSOR_FEE',
            'DISBURSEMENT',
            'SETTLEMENT_CLEAR',
            'SETTLEMENT_NET',
            'SETTLEMENT',
            'RECONCILIATION'
        )
    ),
    -- Business origin (what table generated this). 'SETTLEMENT_BATCH' is historical
    -- only — see the note on journals.source_type.
    source_type VARCHAR(50) NOT NULL CHECK (
        source_type IN (
            'PRODUCT_TRANSACTION',
            'DISBURSEMENT',
            'SETTLEMENT_BATCH',
            'MANUAL_ADJUSTMENT'
        )
    ),
    source_id VARCHAR(255) NOT NULL,
    balance_after BIGINT NOT NULL,
    -- Running balance for this account+bucket after this entry
    -- Calculated as: previous balance_after + amount
    -- Enables fast balance queries and historical tracking
    metadata JSONB,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_entries_account_bucket_created ON ledger_entries(account_uuid, balance_bucket, created_at DESC);

CREATE INDEX idx_ledger_entries_journal ON ledger_entries(journal_uuid);

CREATE INDEX idx_ledger_entries_source ON ledger_entries(source_type, source_id);

-- ProductTransaction: BUSINESS TRANSACTION RECORD
-- Purpose: Records WHO bought WHAT from WHOM for HOW MUCH
-- Status lifecycle: PENDING → COMPLETED → SETTLED
-- PENDING: Created with invoice_number, waiting for payment
-- COMPLETED: the payer paid (money-in webhook booked), funds credited to PENDING
-- SETTLED: Singapay settled the funds, PENDING converted to AVAILABLE
--
-- Balance Updates:
-- - Photo Sale (COMPLETED): credited to PENDING
-- - Reconciliation (SETTLED): PENDING converted to AVAILABLE
-- expected_available = Sum(seller_price + platform_fee) from our transactions
-- actual_available = Singapay balance inquiry (returns total_charged - gateway_fee)
-- Both should equal: seller_price + platform_fee
-- Compare and create discrepancy if mismatch
--
-- Metadata JSONB contains product details:
-- {"photo_id": "...", "title": "Sunset Beach", "resolution": "4K", 
-- "license_type": "Commercial", "download_url": "https://..."}
CREATE TABLE IF NOT EXISTS product_transactions (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    buyer_account_id VARCHAR(255) NOT NULL,
    seller_account_id VARCHAR(255) NOT NULL,
    product_id VARCHAR(255) NOT NULL,
    product_type VARCHAR(50) NOT NULL,
    -- CHECK ( product_type IN ( 'PHOTO', 'FOLDER', 'SUBSCRIPTION')),
    -- Our internal invoice number
    invoice_number VARCHAR(50) NOT NULL UNIQUE,
    -- Pricing breakdown
    seller_price BIGINT NOT NULL,
    -- Seller's listed price
    platform_fee BIGINT NOT NULL,
    -- Platform markup
    gateway_fee BIGINT NOT NULL,
    -- Payment gateway fee
    total_charged BIGINT NOT NULL,
    -- What customer pays (varies by fee model)
    seller_net_amount BIGINT NOT NULL,
    -- What seller actually receives (varies by fee model)
    fee_model VARCHAR(50) DEFAULT 'GATEWAY_ON_CUSTOMER' NOT NULL CHECK (
        fee_model IN ('GATEWAY_ON_CUSTOMER', 'GATEWAY_ON_SELLER')
    ),
    -- Who pays the gateway fee
    currency VARCHAR(3) NOT NULL CHECK (currency IN ('IDR', 'USD')),
    -- Transaction status and lifecycle
    status VARCHAR(20) NOT NULL CHECK (
        status IN (
            'PENDING',
            'COMPLETED',
            'SETTLED',
            'FAILED',
            'REFUNDED'
        )
    ),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP,
    -- When the payer paid (money-in webhook)
    settled_at TIMESTAMP,
    -- When Singapay settled the funds
    platform_fee_transferred BOOLEAN NOT NULL DEFAULT false,
    -- Whether platform fee has been transferred to platform sub-account
    platform_fee_transferred_at TIMESTAMP,
    -- When platform fee was successfully transferred between sub-accounts
    -- merchant_ref_no for the platform fee transfer, reused on every retry so Singapay
    -- replays its original answer instead of transferring twice. Added by migration 011;
    -- it was missing here, which would have made the next Atlas diff drop it.
    transfer_request_id TEXT,
    -- What the fees turned out to be once Singapay reported what it actually took
    -- (migration 024). NULLABLE ON PURPOSE: NULL means "this settled before we recorded
    -- it", i.e. use the priced figure. A DEFAULT 0 here would silently transfer nothing.
    settled_platform_fee BIGINT,
    settled_gateway_fee BIGINT,
    -- Product details (what was purchased)
    metadata JSONB -- Buyer name, product title, resolution, license type, etc.
);

CREATE INDEX idx_product_transactions_buyer ON product_transactions(buyer_account_id, created_at DESC);

CREATE INDEX idx_product_transactions_seller ON product_transactions(seller_account_id, created_at DESC);

CREATE INDEX idx_product_transactions_product ON product_transactions(product_id);

CREATE INDEX idx_product_transactions_invoice ON product_transactions(invoice_number);

CREATE INDEX idx_product_transactions_status ON product_transactions(status);

CREATE INDEX idx_product_transactions_fee_model ON product_transactions(fee_model);

CREATE INDEX idx_product_transactions_status_settled ON product_transactions(status, settled_at);

-- Index for platform fee transfer retry job (GetSettledWithoutPlatformFeeTransfer)
CREATE INDEX idx_product_transactions_pending_transfer ON product_transactions(status, platform_fee_transferred, settled_at)
WHERE
    status = 'SETTLED'
    AND platform_fee_transferred = false;

-- Serves GetAwaitingSettlement and the worker's oldest-awaiting health check, which run on
-- every settlement tick. Partial on the backlog rather than the history (migration 024).
CREATE INDEX idx_product_transactions_awaiting_settlement ON product_transactions (completed_at)
WHERE
    status = 'COMPLETED';

-- payment_requests: PAYMENT GATEWAY INTEGRATION
-- Purpose: records which payment instrument was issued for a transaction, and the keys
--          that read the payment back from Singapay later.
--
-- It carries NO status of its own. A payment request is created 1:1 with its product
-- transaction, in the same database transaction, so "has this been paid?" is a question
-- about product_transactions.status — which is also where it is decided, under a
-- compare-and-set. A second copy here could only agree with that one or be wrong about it.
-- The columns were dropped by migration 025.
CREATE TABLE IF NOT EXISTS payment_requests (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    product_transaction_uuid VARCHAR(255) NOT NULL,
    -- Payment gateway details
    request_id VARCHAR(100) NOT NULL UNIQUE,
    -- Singapay's own id for the payment instrument (VA ULID, QRIS/link id, e-wallet id)
    payment_code TEXT,
    -- VA number, or the whole EMVCo QRIS payload the buyer scans - the gateway decides the
    -- length, so this is TEXT rather than a fixed width (see migration 021)
    payment_channel VARCHAR(50) NOT NULL,
    -- QRIS, VA_BCA, VA_BRI, etc.
    payment_url TEXT,
    -- URL for user to complete payment
    amount BIGINT NOT NULL,
    -- Total charged to buyer
    currency VARCHAR(3) NOT NULL CHECK (currency IN ('IDR', 'USD')),
    -- Singapay's identifiers for the PAYMENT, not the instrument. Filled in from the
    -- money-in webhook (migration 023): request_id above names the instrument, which for
    -- VA and payment link is a different entity from the transaction, so neither can read
    -- a settled transaction back. Two columns because the detail endpoints disagree about
    -- which one they take -- the numeric id for QRIS, e-wallet and payment link, the
    -- business id for VA.
    gateway_transaction_id VARCHAR(100),
    gateway_transaction_ref VARCHAR(100),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    -- The expiry the instrument was issued with. A record of what was asked of Singapay,
    -- not a lifecycle this ledger drives: nothing sweeps on it, and an instrument that
    -- lapses leaves its transaction in PENDING.
    FOREIGN KEY (product_transaction_uuid) REFERENCES product_transactions(uuid)
);

CREATE INDEX idx_payment_requests_product_transaction ON payment_requests(product_transaction_uuid);

CREATE INDEX idx_payment_requests_request_id ON payment_requests(request_id);

CREATE INDEX idx_payment_requests_payment_code ON payment_requests(payment_code);

CREATE INDEX idx_payment_requests_expires ON payment_requests(expires_at);

CREATE INDEX idx_payment_requests_gateway_transaction ON payment_requests (gateway_transaction_id)
WHERE
    gateway_transaction_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS disbursements (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    account_uuid VARCHAR(255) NOT NULL,
    amount BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL CHECK (currency IN ('IDR', 'USD')),
    status VARCHAR(20) NOT NULL CHECK (
        status IN (
            'PENDING',
            'PROCESSING',
            'COMPLETED',
            'FAILED',
            'CANCELLED'
        )
    ),
    bank_code VARCHAR(10) NOT NULL,
    account_number VARCHAR(50) NOT NULL,
    account_name VARCHAR(255) NOT NULL,
    description TEXT,
    external_transaction_id VARCHAR(100),
    failure_reason TEXT,
    -- Singapay reference_number for this payout, written before the call so a retry can
    -- inquire on it rather than send a second one
    -- rather than issue a second payout. Write-once (migration 014).
    payout_request_id TEXT,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    processed_at TIMESTAMP,
    FOREIGN KEY (account_uuid) REFERENCES ledger_accounts(uuid)
);

CREATE INDEX idx_disbursements_account_id ON disbursements(account_uuid);

CREATE INDEX idx_disbursements_account_created ON disbursements(account_uuid, created_at DESC);

CREATE INDEX idx_disbursements_status ON disbursements(status);

CREATE TABLE IF NOT EXISTS fee_configs (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    config_type VARCHAR(20) NOT NULL CHECK (config_type IN ('PLATFORM', 'GATEWAY')),
    -- Deliberately unconstrained (migration 017): channel codes are spelled by the
    -- gateway itself and change when it adds one, so a whitelist here can only go stale.
    payment_channel VARCHAR(50),
    name VARCHAR(100) NOT NULL DEFAULT '',
    fee_type VARCHAR(20) NOT NULL CHECK (fee_type IN ('FIXED', 'PERCENTAGE', 'HYBRID')),
    fixed_amount BIGINT DEFAULT 0,
    percentage DECIMAL(10, 6) DEFAULT 0,
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(config_type, payment_channel)
);

CREATE INDEX idx_fee_configs_type_channel ON fee_configs(config_type, payment_channel);

CREATE INDEX idx_fee_configs_active ON fee_configs(is_active);

CREATE UNIQUE INDEX idx_fee_configs_platform ON fee_configs(payment_channel);

-- Insert default configurations
INSERT INTO
    fee_configs (
        uuid,
        randid,
        config_type,
        payment_channel,
        fee_type,
        fixed_amount,
        percentage,
        created_at,
        updated_at
    )
VALUES
    (
        gen_random_uuid() :: text,
        substring(
            md5(random() :: text)
            from
                1 for 16
        ),
        'PLATFORM',
        'PLATFORM',
        'FIXED',
        1000,
        0,
        NOW(),
        NOW()
    ),
    (
        gen_random_uuid() :: text,
        substring(
            md5(random() :: text)
            from
                1 for 16
        ),
        'GATEWAY',
        'QRIS',
        'PERCENTAGE',
        0,
        0.7,
        NOW(),
        NOW()
    ),
    (
        gen_random_uuid() :: text,
        substring(
            md5(random() :: text)
            from
                1 for 16
        ),
        -- Singapay spells its virtual-account channels per bank (VA_BCA, VA_BNI, …); there
        -- is no generic 'VIRTUAL_ACCOUNT'. Rp 2.800 is flat across banks, so the other
        -- banks are identical rows — see migration 020, which loads the full rate card.
        -- Singapay does not publish money-in rates through its API, so this table is the
        -- only place the expected fee exists.
        'GATEWAY',
        'VA_BCA',
        'FIXED',
        2800,
        0,
        NOW(),
        NOW()
    ) ON CONFLICT (config_type, payment_channel) DO NOTHING;

-- settlement_notifications: THE SETTLEMENT INBOX
-- Purpose: store each settlement webhook exactly as it arrived, and track whether a
--          settling pass has acted on it.
--
-- The webhook is a doorbell, not a source of truth. It announces a batch with totals and a
-- date window and no list of what it covered, so nothing here justifies a ledger entry.
-- What it does is tell the settling pass it is worth asking Singapay about the open
-- invoices, and record what Singapay said for when an amount is argued about later.
--
-- Status lifecycle: PENDING -> PROCESSING -> PROCESSED, or NEEDS_REVIEW / FAILED.
-- NEEDS_REVIEW parks anything that is not a completed balance settlement: refunds need a
-- negative-balance policy that does not exist, and a bank-account settlement pays out
-- rather than moving PENDING into AVAILABLE.
CREATE TABLE IF NOT EXISTS settlement_notifications (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    -- Singapay's own identifiers for the settlement batch.
    settlement_id VARCHAR(255) NOT NULL,
    settlement_reference VARCHAR(255) NOT NULL,
    -- settlement.completed | settlement.refunded | settlement.refund_cancelled
    event VARCHAR(50) NOT NULL,
    -- balance | auto-balance | bank-account | e-wallet.
    -- Only balance and auto-balance move PENDING into AVAILABLE.
    settlement_method VARCHAR(30),
    -- ALL | VA | QRIS | EWALLET. Scopes the batch by product, not by account.
    settlement_type VARCHAR(20),
    -- The window the batch covered. Stored for audit only: Singapay writes these as text
    -- with no offset, so the settling pass never filters on them.
    start_date TIMESTAMP,
    end_date TIMESTAMP,
    -- Totals as announced. Reported, never trusted as the basis for a ledger entry.
    total_transactions INT,
    amount BIGINT,
    total_fee BIGINT,
    currency VARCHAR(3),
    -- The delivery exactly as it arrived, after signature verification.
    raw_payload JSONB NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING' CHECK (
        status IN (
            'PENDING',
            'PROCESSING',
            'PROCESSED',
            'NEEDS_REVIEW',
            'FAILED'
        )
    ),
    failure_reason TEXT,
    received_at TIMESTAMP NOT NULL,
    processed_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- The identity of a delivery. Singapay retries, so a redelivery must be absorbed rather
-- than stored twice; this is what ON CONFLICT DO NOTHING keys on.
CREATE UNIQUE INDEX idx_settlement_notifications_identity ON settlement_notifications (settlement_id, event);

CREATE INDEX idx_settlement_notifications_pending ON settlement_notifications (received_at)
WHERE
    status IN ('PENDING', 'FAILED');

CREATE INDEX idx_settlement_notifications_reference ON settlement_notifications (settlement_reference);

-- Ledger verifications for KYC (Know Your Customer)
-- Purpose: Track seller identity verification using Indonesian KTP (ID card)
-- Status lifecycle: PENDING → APPROVED/REJECTED
-- PENDING: KTP and selfie photos uploaded, awaiting admin review
-- APPROVED: Admin verified identity, seller can create disbursements
-- REJECTED: Identity verification failed, seller must resubmit
--
-- Photos stored in S3:
-- - KTP photo: verification/ktp/{seller_id}/ktp.{ext}
-- - Selfie photo: verification/kyc/{seller_id}/kyc-selfie.{ext}
CREATE TABLE IF NOT EXISTS ledger_verifications (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    account_uuid VARCHAR(255) NOT NULL,
    -- KTP form information
    identity_id VARCHAR(16) NOT NULL,
    -- Indonesian KTP number (16 digits)
    fullname VARCHAR(255) NOT NULL,
    birth_date DATE NOT NULL,
    province VARCHAR(255) NOT NULL,
    city VARCHAR(255) NOT NULL,
    district VARCHAR(255) NOT NULL,
    postal_code VARCHAR(10) NOT NULL,
    -- Photo URLs from S3
    ktp_photo_url TEXT NOT NULL,
    selfie_photo_url TEXT NOT NULL,
    -- Approval workflow
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'APPROVED', 'REJECTED')
    ),
    approved_by VARCHAR(255),
    -- Admin user ID who approved/rejected
    approved_at TIMESTAMP,
    rejection_reason TEXT,
    -- Additional data
    metadata JSONB,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    FOREIGN KEY (account_uuid) REFERENCES ledger_accounts(uuid),
    UNIQUE (account_uuid),
    -- One verification per account
    UNIQUE (identity_id) -- One KTP per system (prevent duplicate)
);

CREATE INDEX idx_verifications_account_uuid ON ledger_verifications(account_uuid);

CREATE INDEX idx_verifications_identity_id ON ledger_verifications(identity_id);

CREATE INDEX idx_verifications_status ON ledger_verifications(status);

CREATE INDEX idx_verifications_pending ON ledger_verifications(status, created_at ASC)
WHERE
    status = 'PENDING';