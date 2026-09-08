-- Analytics Schema
-- Microbatch Log
CREATE TABLE analytics_microbatch_log (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    job_name VARCHAR(50) NOT NULL,
    batch_start TIMESTAMP NOT NULL,
    batch_end TIMESTAMP NOT NULL,
    status VARCHAR(20) NOT NULL,
    rows_processed INT NOT NULL DEFAULT 0,
    message TEXT
);

-- Dimension Tables
-- 1. dim_date
CREATE TABLE dim_date (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    date_key INT NOT NULL UNIQUE,
    -- YYYYMMDD
    date DATE NOT NULL
);

-- 2. dim_account (SCD Type 2)
CREATE TABLE dim_account (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    account_id VARCHAR(255) NOT NULL,
    -- FK to ledger_accounts.uuid
    owner_type VARCHAR(50),
    owner_id VARCHAR(255),
    currency VARCHAR(3),
    singapay_account_id VARCHAR(255),
    effective_date DATE,
    end_date DATE,
    is_current BOOLEAN DEFAULT FALSE
);

-- 3. dim_bank_account
CREATE TABLE dim_bank_account (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    account_uuid VARCHAR(255) NOT NULL,
    -- FK to dim_account.uuid
    bank_code VARCHAR(50),
    account_number VARCHAR(50),
    account_name VARCHAR(255),
    is_verified BOOLEAN DEFAULT FALSE,
    first_used_at TIMESTAMP,
    last_used_at TIMESTAMP,
    UNIQUE(account_uuid, bank_code, account_number)
);

-- 4. dim_transaction_type
CREATE TABLE dim_transaction_type (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    transaction_type_key INT NOT NULL UNIQUE,
    source_type VARCHAR(50),
    payment_channel VARCHAR(50),
    transaction_category VARCHAR(50)
);

-- 5. dim_ledger_bucket
CREATE TABLE dim_ledger_bucket (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    bucket_key VARCHAR(50) NOT NULL UNIQUE
);

-- 6. dim_ledger_entry_type
CREATE TABLE dim_ledger_entry_type (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    entry_type_key VARCHAR(50) NOT NULL UNIQUE
);

-- 7. dim_subscription
CREATE TABLE dim_subscription (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    subscription_status VARCHAR(50) NOT NULL UNIQUE
);

-- 8. dim_bank
CREATE TABLE dim_bank (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    bank_code VARCHAR(10) NOT NULL UNIQUE,
    bank_name VARCHAR(255) NOT NULL,
    swift_code VARCHAR(20)
);

-- 9. dim_payment_channel
CREATE TABLE dim_payment_channel (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    payment_channel_key VARCHAR(50) NOT NULL UNIQUE,
    is_virtual_account BOOLEAN DEFAULT FALSE
);

-- 10. dim_account_status
CREATE TABLE dim_account_status (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    status_key VARCHAR(50) NOT NULL UNIQUE
);

-- 11. dim_transaction_status
CREATE TABLE dim_transaction_status (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    status_key VARCHAR(50) NOT NULL UNIQUE,
    is_terminal BOOLEAN DEFAULT FALSE
);

-- 12. dim_product_type
CREATE TABLE dim_product_type (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    product_type_key VARCHAR(50) NOT NULL UNIQUE
);

-- 13. dim_account_owner_type
CREATE TABLE dim_account_owner_type (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    owner_type_key VARCHAR(50) NOT NULL UNIQUE
);

-- Fact Tables
-- 1. fact_revenue_timeseries
CREATE TABLE fact_revenue_timeseries (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    date_key INT NOT NULL,
    interval_type VARCHAR(20) NOT NULL,
    interval_label VARCHAR(50),
    convenience_fee_total BIGINT DEFAULT 0,
    subscription_fee_total BIGINT DEFAULT 0,
    gateway_fee_paid_total BIGINT DEFAULT 0,
    total_revenue BIGINT DEFAULT 0,
    transaction_count INT DEFAULT 0,
    settlement_transaction_count INT DEFAULT 0,
    UNIQUE(date_key, interval_type)
);

-- 2 & 3 & 4. fact_platform_balance
CREATE TABLE fact_platform_balance (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    date_key INT NOT NULL UNIQUE,
    -- Revenue YTD
    total_revenue_ytd BIGINT DEFAULT 0,
    convenience_fee_ytd BIGINT DEFAULT 0,
    subscription_fee_ytd BIGINT DEFAULT 0,
    gateway_fee_ytd BIGINT DEFAULT 0,
    -- Operations
    settlement_pending_count INT DEFAULT 0,
    settlement_completed_count INT DEFAULT 0,
    active_transactions_count INT DEFAULT 0,
    -- Platform Wallet
    platform_total_balance BIGINT DEFAULT 0,
    platform_available_balance BIGINT DEFAULT 0,
    platform_pending_balance BIGINT DEFAULT 0,
    -- Aggregate Seller Stats
    total_seller_accounts INT DEFAULT 0,
    total_user_available_balance BIGINT DEFAULT 0,
    total_user_pending_balance BIGINT DEFAULT 0,
    total_user_earnings BIGINT DEFAULT 0,
    total_user_withdrawn BIGINT DEFAULT 0
);

-- 5. fact_user_accumulation
CREATE TABLE fact_user_accumulation (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    account_uuid VARCHAR(255) NOT NULL UNIQUE,
    -- One row per seller account
    dim_account_uuid VARCHAR(255) NOT NULL,
    -- FK to dim_account
    total_earnings BIGINT DEFAULT 0,
    current_pending_balance BIGINT DEFAULT 0,
    current_available_balance BIGINT DEFAULT 0,
    total_withdrawn BIGINT DEFAULT 0,
    safe_balance_to_withdraw BIGINT DEFAULT 0,
    account_status VARCHAR(50),
    has_pending_balance BOOLEAN DEFAULT FALSE,
    has_available_balance BOOLEAN DEFAULT FALSE
);

-- 6. fact_withdrawal_timeseries
CREATE TABLE fact_withdrawal_timeseries (
    uuid VARCHAR(255) PRIMARY KEY,
    randid VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    date_key INT NOT NULL,
    interval_type VARCHAR(20) NOT NULL,
    attempt_count INT DEFAULT 0,
    success_count INT DEFAULT 0,
    failed_count INT DEFAULT 0,
    total_requested_amount BIGINT DEFAULT 0,
    total_disbursed_amount BIGINT DEFAULT 0,
    avg_processing_time_sec INT DEFAULT 0,
    UNIQUE(date_key, interval_type)
);

-- Analytics Indexes
-- Watermark lookup: latest completed batch per job.
CREATE INDEX idx_analytics_microbatch_job_status_end ON analytics_microbatch_log(job_name, status, batch_end DESC);

-- Optional troubleshooting view: recent runs by status.
CREATE INDEX idx_analytics_microbatch_status_updated ON analytics_microbatch_log(status, updated_at DESC);

-- Dim account SCD2 access patterns.
CREATE INDEX idx_dim_account_account_current ON dim_account(account_id, is_current);

CREATE INDEX idx_dim_account_account_effective ON dim_account(account_id, effective_date DESC);

-- Transactional bank dimension lookups.
CREATE INDEX idx_dim_bank_account_account ON dim_bank_account(account_uuid);

CREATE INDEX idx_dim_bank_account_last_used ON dim_bank_account(last_used_at DESC);

-- Payment channel filters.
CREATE INDEX idx_dim_payment_channel_va ON dim_payment_channel(is_virtual_account);

-- Fact filters for common dashboard queries.
CREATE INDEX idx_fact_revenue_interval_date ON fact_revenue_timeseries(interval_type, date_key DESC);

CREATE INDEX idx_fact_platform_balance_date ON fact_platform_balance(date_key DESC);

CREATE INDEX idx_fact_withdrawal_interval_date ON fact_withdrawal_timeseries(interval_type, date_key DESC);

CREATE INDEX idx_fact_user_accumulation_dim_account ON fact_user_accumulation(dim_account_uuid);

CREATE INDEX idx_fact_user_accumulation_updated ON fact_user_accumulation(updated_at DESC);