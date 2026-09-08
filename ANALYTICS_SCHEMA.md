# Analytics Dashboard Data Specifications

**Document Status**: Draft
**Target Audience**: Data Engineers, Backend Developers

## Overview

This specification defines the data architecture powering the internal financial dashboard. It maps visual dashboard components to underlying analytics tables, defining the ETL logic required to transform raw ledger data into query-optimized metrics.

- **Update Frequency**: 5-minute micro-batches (Incremental Delta Load)
- **Currency Standard**: IDR (Integer/Sen)
- **Time Zone**: UTC (unless specified otherwise)

## Table of Contents

- [Dimension Tables Reference](#dimension-tables-reference)
- [1. Platform Time-Series Accumulation](#1-platform-time-series-accumulation)
- [2. Platform Master Accumulation](#2-platform-master-accumulation)
- [3. Platform Balance](#3-platform-balance)
- [4. User / Seller Master Accumulation](#4-user--seller-master-accumulation)
- [5. User Wallet Accumulation](#5-user-wallet-accumulation-per-seller)
- [6. Ledger Entries — Time Series](#6-ledger-entries--time-series)
- [7. Ledger Master Accumulation](#7-ledger-master-accumulation)
- [8. Account Profile](#8-account-profile)
- [9. Withdrawal Master Accumulation](#9-withdrawal-master-accumulation)
- [10. Withdrawal — Per Account](#10-withdrawal--per-account-inner-join-to-account)
- [Summary of Mappings](#summary-table)

## Preliminary: ETL Control & Watermarks

All incremental ETL jobs rely on a central watermark log to track progress.

### `analytics_microbatch_log`

Tracks the execution state of each micro-batch job.

```sql
analytics_microbatch_log {
  uuid              VARCHAR(255) PRIMARY KEY
  randid            VARCHAR(255)
  created_at        TIMESTAMP
  updated_at        TIMESTAMP

  job_name          VARCHAR(50)    -- e.g., 'fact_revenue_timeseries_loader'
  batch_start       TIMESTAMP      -- When this job execution started
  batch_end         TIMESTAMP      -- The data cutoff timestamp (high watermark)
  status            VARCHAR(20)    -- RUNNING | COMPLETED | FAILED
  rows_processed    INT
  message           TEXT
}
```

### Watermark Logic

The `:last_watermark` parameter used in ETL queries is derived dynamically at runtime:

```sql
-- Get the high watermark from the last successful run
SELECT COALESCE(MAX(batch_end), '1970-01-01'::TIMESTAMP)
FROM analytics_microbatch_log
WHERE job_name = :current_job_name
  AND status = 'COMPLETED';
```

**Runtime Variables**:

- `:last_watermark` = The result of the query above.
- `:batch_end` = `NOW()` (captured at the start of the current job execution).

---

## Dimension Tables Reference

Definitions of the shared dimension tables used across all metric facts.

### 1. `dim_date`

> **Type**: Conformed Dimension
> **Grain**: Daily
> **Source**: Static Generation

```sql
dim_date {
  uuid              VARCHAR(255) PRIMARY KEY
  randid            VARCHAR(255)
  created_at        TIMESTAMP
  updated_at        TIMESTAMP

  date_key          INT            -- YYYYMMDD
  date              DATE, ...
}
```

### 2. `dim_account` (SCD Type 2)

> **Type**: Slowly Changing Dimension (Type 2)
> **Grain**: Account + Validity Period
> **Source**: `ledger_accounts`

Tracks changes in account ownership type, owner_id, or currency over time.

```sql
dim_account {
  uuid                    VARCHAR(255) PRIMARY KEY
  randid                  VARCHAR(255)
  created_at              TIMESTAMP
  updated_at              TIMESTAMP

  account_id              UUID           -- FK to ledger_accounts.id
  owner_type              VARCHAR(50)    -- SELLER | BUYER | PLATFORM
  owner_id                VARCHAR(255)
  currency                VARCHAR(3)
  singapay_account_id      VARCHAR(255)

  -- SCD2 Validity Columns
  effective_date          DATE
  end_date                DATE
  is_current              BOOLEAN
}
```

**ETL Logic (Simplified):**

```sql
-- Insert new record on change detection
INSERT INTO dim_account (uuid, randid, created_at, updated_at, ...)
SELECT gen_random_uuid(), gen_random_uuid(), NOW(), NOW(), uuid, owner_type, ...
FROM ledger_accounts la
WHERE la.updated_at > :last_watermark
```

### 3. `dim_bank_account`

> **Type**: Transactional Dimension
> **Grain**: Unique Bank Account per Ledger Account
> **Source**: `disbursements`

Captures historical bank accounts used for withdrawals.

```sql
dim_bank_account {
  uuid              VARCHAR(255) PRIMARY KEY
  randid            VARCHAR(255)
  created_at        TIMESTAMP
  updated_at        TIMESTAMP

  account_uuid      VARCHAR(255)   -- FK to dim_account.uuid
  bank_code         VARCHAR(50)
  account_number    VARCHAR(50)
  account_name      VARCHAR(255)
  is_verified       BOOLEAN
  first_used_at     TIMESTAMP
  last_used_at      TIMESTAMP
}
```

**ETL Logic (Upsert):**

```sql
-- 1. Scan new disbursements for unique bank details
-- 2. Insert into dim_bank_account if combination (account_id, bank, number) is new
-- 3. Update last_used_at if exists
```

### 4. `dim_transaction_type`

**Source**: Static Reference

```sql
dim_transaction_type {
  uuid                     VARCHAR(255) PRIMARY KEY
  randid                   VARCHAR(255)
  created_at               TIMESTAMP
  updated_at               TIMESTAMP

  transaction_type_key     INT
  source_type              VARCHAR(50)   -- PAYMENT | DISBURSEMENT
  payment_channel          VARCHAR(50)   -- QRIS | VA
  transaction_category     VARCHAR(50)   -- SALE | WITHDRAWAL
}
```

### 5. `dim_ledger_bucket`

**Source**: Static Reference

```sql
dim_ledger_bucket {
  uuid          VARCHAR(255) PRIMARY KEY
  randid        VARCHAR(255)
  created_at    TIMESTAMP
  updated_at    TIMESTAMP

  bucket_key    VARCHAR(50)    -- PENDING | AVAILABLE
}
```

### 6. `dim_ledger_entry_type`

**Source**: Static Reference

```sql
dim_ledger_entry_type {
  uuid              VARCHAR(255) PRIMARY KEY
  randid            VARCHAR(255)
  created_at        TIMESTAMP
  updated_at        TIMESTAMP

  entry_type_key    VARCHAR(50)    -- CREDIT | DEBIT
}
```

### 7. `dim_subscription`

**Source**: Future Implementation

```sql
dim_subscription {
  uuid                  VARCHAR(255) PRIMARY KEY
  randid                VARCHAR(255)
  created_at            TIMESTAMP
  updated_at            TIMESTAMP

  subscription_status   VARCHAR(50)
}
```

### 8. `dim_bank`

**Source**: Static Reference (Bank Codes)

```sql
dim_bank {
  uuid        VARCHAR(255) PRIMARY KEY
  randid      VARCHAR(255)
  created_at  TIMESTAMP
  updated_at  TIMESTAMP

  bank_code   VARCHAR(10) UNIQUE   -- 014 (BCA)
  bank_name   VARCHAR(255)
  swift_code  VARCHAR(20)
}
```

### 9. `dim_payment_channel`

**Source**: `fee_configs`

```sql
dim_payment_channel {
  uuid                  VARCHAR(255) PRIMARY KEY
  randid                VARCHAR(255)
  created_at            TIMESTAMP
  updated_at            TIMESTAMP

  payment_channel_key   VARCHAR(50)
  is_virtual_account    BOOLEAN
}
```

### 10. `dim_account_status`

**Source**: Static Reference

```sql
dim_account_status {
  uuid          VARCHAR(255) PRIMARY KEY
  randid        VARCHAR(255)
  created_at    TIMESTAMP
  updated_at    TIMESTAMP

  status_key    VARCHAR(50)    -- ACTIVE | SUSPENDED
}
```

### 11. `dim_transaction_status`

**Source**: Static Reference

```sql
dim_transaction_status {
  uuid          VARCHAR(255) PRIMARY KEY
  randid        VARCHAR(255)
  created_at    TIMESTAMP
  updated_at    TIMESTAMP

  status_key    VARCHAR(50)    -- PENDING | COMPLETED | SETTLED
  is_terminal   BOOLEAN
}
```

### 12. `dim_product_type`

**Source**: Static Reference

```sql
dim_product_type {
  uuid                VARCHAR(255) PRIMARY KEY
  randid              VARCHAR(255)
  created_at          TIMESTAMP
  updated_at          TIMESTAMP

  product_type_key    VARCHAR(50)    -- PHOTO | FOLDER | SUBSCRIPTION
}
```

### 13. `dim_account_owner_type`

**Source**: Static Reference

```sql
dim_account_owner_type {
  uuid              VARCHAR(255) PRIMARY KEY
  randid            VARCHAR(255)
  created_at        TIMESTAMP
  updated_at        TIMESTAMP

  owner_type_key    VARCHAR(50)    -- SELLER | BUYER | PLATFORM
}
```

---

## 1. Platform Time-Series Accumulation

**Dashboard Section**: Revenue Breakdown chart (Daily / Monthly views)  
**Backed By**: `fact_revenue_timeseries`

### Schema

```sql
fact_revenue_timeseries {
  uuid                        VARCHAR(255) PRIMARY KEY
  randid                      VARCHAR(255)
  created_at                  TIMESTAMP
  updated_at                  TIMESTAMP

  -- Time grain
  date_key                    INT           -- YYYYMMDD of interval start (e.g. 20260313)
  interval_type               VARCHAR(20)   -- 'DAILY' | 'WEEKLY' | 'MONTHLY' | 'YEARLY'
  interval_label              VARCHAR(50)   -- "2026-03-13" | "W11-2026" | "2026-03"

  -- Revenue metrics
  convenience_fee_total       BIGINT        -- SUM(platform_fee) WHERE product_type != 'SUBSCRIPTION'
  subscription_fee_total      BIGINT        -- SUM(seller_price) WHERE product_type = 'SUBSCRIPTION'
  gateway_fee_paid_total      BIGINT        -- SUM(gateway_fee) from SETTLED transactions
  total_revenue               BIGINT        -- convenience_fee_total + subscription_fee_total

  -- Count metrics
  transaction_count           INT           -- COUNT COMPLETED transactions in interval
  settlement_transaction_count INT          -- COUNT SETTLED transactions in interval
}
```

### ETL Strategy

- **Trigger**: Incremental Delta Load (Micro-batch every 5m)
- **Source**: `product_transactions` (status = 'SETTLED' updated since last watermark)
- **Logic**:
  1. Identify settlements in current batch window
  2. Map each settlement to its Daily, Weekly, Monthly, and Yearly bucket
  3. Recalculate full metrics for affected buckets (UPSERT on conflict)
- **Key Fields**: `platform_fee`, `gateway_fee`, `seller_price`, `product_type`

#### Source Query

```sql
-- Incremental delta load for fact_revenue_timeseries
WITH watermark_delta AS (
  SELECT pt.*
  FROM product_transactions pt
  WHERE pt.status = 'SETTLED' AND pt.updated_at > :last_watermark AND pt.updated_at <= :batch_end
),
affected_intervals AS (
  SELECT DISTINCT
    TO_CHAR(DATE_TRUNC(i.trunc_unit, wd.settled_at), 'YYYYMMDD')::INT AS date_key,
    i.interval_type
  FROM watermark_delta wd
  CROSS JOIN ( VALUES ('day', 'DAILY'), ('week', 'WEEKLY'), ('month', 'MONTHLY'), ('year', 'YEARLY') ) AS i(trunc_unit, interval_type)
),
recalculated AS (
  SELECT
    ai.date_key, ai.interval_type,
    COALESCE(SUM(pt.platform_fee) FILTER (WHERE pt.product_type != 'SUBSCRIPTION'), 0) AS convenience_fee_total,
    COALESCE(SUM(pt.seller_price) FILTER (WHERE pt.product_type = 'SUBSCRIPTION'), 0)  AS subscription_fee_total,
    COALESCE(SUM(pt.gateway_fee), 0)                                                        AS gateway_fee_paid_total,
    COUNT(*)                                                                             AS settlement_transaction_count
  FROM affected_intervals ai
  JOIN product_transactions pt ON pt.status = 'SETTLED'
    AND TO_CHAR(DATE_TRUNC(
          CASE ai.interval_type WHEN 'DAILY' THEN 'day' WHEN 'WEEKLY' THEN 'week' WHEN 'MONTHLY' THEN 'month' WHEN 'YEARLY' THEN 'year' END,
          pt.settled_at
        ), 'YYYYMMDD')::INT = ai.date_key
  GROUP BY ai.date_key, ai.interval_type
)
INSERT INTO fact_revenue_timeseries (
  uuid, randid, created_at, updated_at,
  date_key, interval_type, convenience_fee_total, subscription_fee_total, gateway_fee_paid_total,
  total_revenue, settlement_transaction_count
)
SELECT
  gen_random_uuid(), gen_random_uuid(), NOW(), NOW(),
  date_key, interval_type,
  convenience_fee_total, subscription_fee_total, gateway_fee_paid_total,
  convenience_fee_total + subscription_fee_total,
  settlement_transaction_count
FROM recalculated
ON CONFLICT (date_key, interval_type) DO UPDATE SET
  convenience_fee_total = EXCLUDED.convenience_fee_total,
  updated_at = NOW();

  subscription_fee_total = EXCLUDED.subscription_fee_total, updated_at = NOW();
```

---

## 2. Platform Master Accumulation

**Dashboard Section**: Overview cards — Platform Revenue, Gateway Fees  
**Backed By**: `fact_platform_balance` (yearly snapshot keyed by `date_key`)

### Schema

```sql
fact_platform_balance {
  uuid                        VARCHAR(255) PRIMARY KEY
  randid                      VARCHAR(255)
  created_at                  TIMESTAMP
  updated_at                  TIMESTAMP
  date_key                    INT      -- YYYY0101 (one row per year)

  -- Platform Revenue (YTD)
  total_revenue_ytd           BIGINT   -- convenience_fee_ytd + subscription_fee_ytd
  convenience_fee_ytd         BIGINT   -- SUM(platform_fee) SETTLED non-SUBSCRIPTION YTD
  subscription_fee_ytd        BIGINT   -- SUM(seller_price) SETTLED SUBSCRIPTION YTD
  gateway_fee_ytd             BIGINT   -- SUM(gateway_fee) SETTLED YTD (Cost)

  -- Operations Metrics
  settlement_pending_count    INT      -- Transactions awaiting settlement (COMPLETED state)
  settlement_completed_count  INT      -- Transactions fully settled (SETTLED state)
  active_transactions_count   INT      -- Total transaction volume YTD
}
```

### ETL Strategy

- **Trigger**: Incremental Delta Update (Micro-batch every 5m)
- **Source**: `product_transactions`, `ledger_accounts`, `ledger_entries`
- **Logic**:
  1. **Revenue (YTD)**: Calculate deltas (SUM of fees) from transactions settled _since watermark_ and add to running total.
  2. **Counts**: Recompute counts (cheap) or add deltas from new transactions.
  3. **Platform Balances**: Snapshot read of the single Platform account.
  4. **Seller Totals**:
     - _Counts_: Add newly created seller accounts.
     - _Balances_: Sum `ledger_entries.amount` for all SELLER accounts in batch and add to running totals.
- **Drift Protection**: An optional "Full Recompute" job should run nightly to correct any arithmetic drift.

#### Source Query

```sql
WITH revenue_deltas AS (
  SELECT
    COALESCE(SUM(platform_fee) FILTER (WHERE product_type != 'SUBSCRIPTION'), 0) AS delta_convenience,
    COALESCE(SUM(seller_price) FILTER (WHERE product_type = 'SUBSCRIPTION'), 0)  AS delta_subscription,
    COALESCE(SUM(gateway_fee), 0)                                                    AS delta_gateway,
    COUNT(*)                                                                      AS delta_settled_count
  FROM product_transactions
  WHERE status = 'SETTLED' AND updated_at > :last_watermark AND updated_at <= :batch_end
    AND EXTRACT(YEAR FROM settled_at) = EXTRACT(YEAR FROM NOW())
),
balance_deltas AS (
  SELECT
    COALESCE(SUM(amount) FILTER (WHERE balance_bucket = 'AVAILABLE'), 0) AS delta_user_available,
    COALESCE(SUM(amount) FILTER (WHERE balance_bucket = 'PENDING'), 0)   AS delta_user_pending,
    -- Note: Earnings/Withdrawn tracked via transaction types/entries
    COALESCE(SUM(amount) FILTER (WHERE entry_type = 'CREDIT'), 0)        AS delta_user_earnings, -- Simplified proxy
    COALESCE(SUM(ABS(amount)) FILTER (WHERE source_type = 'DISBURSEMENT'), 0) AS delta_user_withdrawn
  FROM ledger_entries le
  JOIN ledger_accounts la ON le.account_uuid = la.uuid
  WHERE la.owner_type = 'SELLER'
    AND le.created_at > :last_watermark AND le.created_at <= :batch_end
),
platform_snapshot AS (
  SELECT pending_balance, available_balance, pending_balance + available_balance AS total
  FROM ledger_accounts WHERE owner_type = 'PLATFORM' LIMIT 1
)
INSERT INTO fact_platform_balance (uuid, randid, created_at, updated_at, date_key, ...)
VALUES (gen_random_uuid(), gen_random_uuid(), NOW(), NOW(), :year_date_key, ...)
ON CONFLICT (date_key) DO UPDATE SET
  convenience_fee_ytd        = fact_platform_balance.convenience_fee_ytd + (SELECT delta_convenience FROM revenue_deltas),
  subscription_fee_ytd       = fact_platform_balance.subscription_fee_ytd + (SELECT delta_subscription FROM revenue_deltas),
  gateway_fee_ytd            = fact_platform_balance.gateway_fee_ytd + (SELECT delta_gateway FROM revenue_deltas),

  -- Snapshot updates
  platform_total_balance     = (SELECT total FROM platform_snapshot),
  platform_available_balance = (SELECT available_balance FROM platform_snapshot),

  -- Balance deltas
  total_user_available_balance = fact_platform_balance.total_user_available_balance + (SELECT delta_user_available FROM balance_deltas),
  total_user_pending_balance   = fact_platform_balance.total_user_pending_balance + (SELECT delta_user_pending FROM balance_deltas),

  updated_at = NOW();
```

---

## 3. Platform Balance

**Dashboard Section**: Platform Wallet — Total Balance, Active Balance, Pending Balance  
**Backed By**: `fact_platform_balance` (same table as Section 2)

### Schema (Additional Fields)

```sql
fact_platform_balance {
  -- ... (keys from Section 2)

  -- Platform Wallet
  platform_total_balance      BIGINT   -- pending + available
  platform_available_balance  BIGINT   -- Funds ready for payout/withdrawal
  platform_pending_balance    BIGINT   -- Funds in settlement process
}
```

### ETL Strategy

- **Trigger**: Part of Section 2 ETL
- **Source**: `ledger_accounts` where `owner_type = 'PLATFORM'`
- **Logic**: Direct read of current platform wallet balances into the snapshot row

---

## 4. User / Seller Master Accumulation

**Dashboard Section**: Overview cards — Total Users, Total User Balances, Total Withdrawn  
**Backed By**: `fact_platform_balance` (same table as Section 2)

### Schema (Additional Fields)

```sql
fact_platform_balance {
  -- ... (keys from Section 2)

  -- Aggregated Seller Stats
  total_seller_accounts       INT      -- Total registered sellers
  total_user_available_balance BIGINT  -- Sum of all seller available balances (Liability)
  total_user_pending_balance  BIGINT   -- Sum of all seller pending balances
  total_user_earnings         BIGINT   -- Lifetime gross earnings (Deposit total)
  total_user_withdrawn        BIGINT   -- Lifetime withdrawals (Withdrawal total)
}
```

### ETL Strategy

- **Trigger**: Part of Section 2 ETL
- **Source**: `ledger_accounts` where `owner_type = 'SELLER'`
- **Logic**: Aggregate SUMs across all seller accounts

---

## 5. User Wallet Accumulation Per Seller

**Dashboard Section**: User Wallets Table (Row per seller)  
**Backed By**: `fact_user_accumulation`

### Schema

```sql
fact_user_accumulation {
  uuid                        VARCHAR(255) PRIMARY KEY
  randid                      VARCHAR(255)
  created_at                  TIMESTAMP
  updated_at                  TIMESTAMP

  account_uuid                VARCHAR(255)             -- FK to ledger_accounts
  dim_account_uuid            VARCHAR(255)             -- FK to dim_account (owner details)

  -- Wallet Metrics
  total_earnings              BIGINT   -- Lifetime total_deposit_amount
  current_pending_balance     BIGINT   -- Current pending_balance
  current_available_balance   BIGINT   -- Current available_balance
  total_withdrawn             BIGINT   -- Lifetime total_withdrawal_amount
  safe_balance_to_withdraw    BIGINT   -- LEAST(available, expected_available)

  -- Status
  account_status              VARCHAR(50)
  has_pending_balance         BOOLEAN
  has_available_balance       BOOLEAN

  updated_at                  TIMESTAMP
}
```

### ETL Strategy

- **Trigger**: Incremental Upsert (Micro-batch every 5m)
- **Source**: `ledger_accounts` (updated since watermark)
- **Logic**:
  1. Detect seller accounts modified since last batch
  2. Upsert current balances and lifetime totals into `fact_user_accumulation`
  3. Join with `dim_account` for latest owner details
- **Target**: High-performance dashboard list view (avoids joining raw ledger tables at read time)

#### Source Query

```sql
INSERT INTO fact_user_accumulation (
  uuid, randid, created_at, updated_at,
  account_uuid, dim_account_uuid, total_earnings, current_pending_balance,
  current_available_balance, total_withdrawn, safe_balance_to_withdraw,
  account_status, has_pending_balance, has_available_balance
)
SELECT
  gen_random_uuid(), gen_random_uuid(), NOW(), NOW(),
  la.uuid, da.uuid,
  la.total_deposit_amount, la.pending_balance, la.available_balance,
  la.total_withdrawal_amount, LEAST(la.available_balance, la.expected_available_balance),
  'ACTIVE', (la.pending_balance > 0), (la.available_balance > 0)
FROM ledger_accounts la
JOIN dim_account da ON da.account_id = la.uuid AND da.is_current = true
WHERE la.owner_type = 'SELLER'
  AND la.updated_at > :last_watermark
  AND la.updated_at <= :batch_end
ON CONFLICT (account_uuid) DO UPDATE SET
  current_available_balance = EXCLUDED.current_available_balance,
  total_earnings = EXCLUDED.total_earnings,
  updated_at = NOW();
```

---

## 6. Account Profile

**Dashboard Section**: Account Details View  
**Backed By**: `dim_account` + `dim_bank_account` + `fact_user_accumulation`

### Schema Relationships

1. **Identity**: `dim_account` (SCD Type 2) provides point-in-time snapshot of owner_id, type, status.
2. **Banking**: `dim_bank_account` provides history of all bank accounts associated with the user.
3. **Financials**: `fact_user_accumulation` provides current wallet balances and lifetime stats.

### ETL Strategy

- **`dim_account`**: Managed via SCD2 loader (detects profile changes).
- **`dim_bank_account`**: Managed via transactional detection (new bank details in `disbursements` trigger insert).
- **`fact_user_accumulation`**: Managed via Section 5 ETL.

---

## 7. Withdrawal Master Accumulation

**Dashboard Section**: Withdrawal Overview — Total Payouts, Pending Requests, Success Rate  
**Backed By**: `fact_withdrawal_timeseries`

### Schema

```sql
fact_withdrawal_timeseries {
  uuid                        VARCHAR(255) PRIMARY KEY
  randid                      VARCHAR(255)
  created_at                  TIMESTAMP
  updated_at                  TIMESTAMP

  -- Time grain
  date_key                    INT            -- YYYYMMDD
  interval_type               VARCHAR(20)    -- DAILY | MONTHLY

  -- Metrics
  attempt_count               INT            -- Total withdrawal requests
  success_count               INT            -- Successfully processed
  failed_count                INT            -- Failed/Rejected
  total_requested_amount      BIGINT         -- Sum of amounts requested
  total_disbursed_amount      BIGINT         -- Sum of amounts successfully sent

  avg_processing_time_sec     INT            -- Average time from REQUEST -> COMPLETE

  updated_at                  TIMESTAMP
}
```

### ETL Strategy

- **Trigger**: Incremental Delta (Micro-batch every 5m)
- **Source**: `disbursements` (updated since watermark)
- **Logic**:
  1. Identify disbursements updated in batch
  2. Group by Date + Status
  3. Update daily/monthly success/failure counts and amounts

#### Source Query

```sql
WITH watermark_delta AS (
  SELECT * FROM disbursements WHERE updated_at > :last_watermark AND updated_at <= :batch_end
),
affected_intervals AS (
  SELECT DISTINCT
    TO_CHAR(DATE_TRUNC(i.trunc_unit, d.created_at), 'YYYYMMDD')::INT AS date_key,
    i.interval_type
  FROM watermark_delta d
  CROSS JOIN ( VALUES ('day', 'DAILY'), ('month', 'MONTHLY') ) AS i(trunc_unit, interval_type)
),
recalculated AS (
  SELECT
    ai.date_key, ai.interval_type,
    COUNT(*)                                         AS attempt_count,
    COUNT(*) FILTER (WHERE status = 'COMPLETED')     AS success_count,
    COUNT(*) FILTER (WHERE status = 'FAILED')        AS failed_count,
    COALESCE(SUM(amount), 0)                         AS total_requested_amount,
    COALESCE(SUM(amount) FILTER (WHERE status = 'COMPLETED'), 0) AS total_disbursed_amount,
    COALESCE(AVG(EXTRACT(EPOCH FROM (processed_at - created_at)))
             FILTER (WHERE status = 'COMPLETED'), 0)::INT AS avg_processing_time_sec
  FROM affected_intervals ai
  JOIN disbursements d
    ON TO_CHAR(DATE_TRUNC(
         CASE ai.interval_type WHEN 'DAILY' THEN 'day' WHEN 'MONTHLY' THEN 'month' END,
         d.created_at
       ), 'YYYYMMDD')::INT = ai.date_key
  GROUP BY ai.date_key, ai.interval_type
)
INSERT INTO fact_withdrawal_timeseries (
  uuid, randid, created_at, updated_at,
  date_key, interval_type,
  attempt_count, success_count, failed_count,
  total_requested_amount, total_disbursed_amount, avg_processing_time_sec
)
SELECT
  gen_random_uuid(), gen_random_uuid(), NOW(), NOW(),
  r.date_key, r.interval_type,
  r.attempt_count, r.success_count, r.failed_count,
  r.total_requested_amount, r.total_disbursed_amount, r.avg_processing_time_sec
FROM recalculated r
ON CONFLICT (date_key, interval_type) DO UPDATE SET
  success_count = EXCLUDED.success_count,
  total_disbursed_amount = EXCLUDED.total_disbursed_amount,
  updated_at = NOW();
```

---

## 8. Withdrawal — Per Account

**Dashboard Section**: Withdrawal History (per user)  
**Backed By**: `dim_bank_account` + `fact_withdrawal_timeseries`

### Query Strategy

To show withdrawal history for a specific account:

1. Filter `disbursements` (Raw Table) or `fact_withdrawal_timeseries` by `account_id`.
2. Join `dim_bank_account` to show which bank was used for each transaction.

#### View Query

```sql
SELECT
  d.id,
  d.created_at,
  d.amount,
  d.status,
  d.description,
  dba.bank_name,
  dba.account_number,
  dba.account_name
FROM disbursements d
LEFT JOIN dim_bank_account dba
  ON d.account_id = dba.account_id
  AND d.bank_code = dba.bank_code
  AND d.account_number = dba.account_number
WHERE d.account_id = :account_id
ORDER BY d.created_at DESC;
```

---

## Summary of Mappings

| Dashboard Section          | Primary Fact Table                  | Granularity           | Update Trigger      |
| :------------------------- | :---------------------------------- | :-------------------- | :------------------ |
| **1. Platform Revenue**    | `fact_revenue_timeseries`           | Daily/Monthly         | Settlement Events   |
| **2. Platform Overview**   | `fact_platform_balance`             | Yearly Snapshot       | Every Batch         |
| **3. Platform Wallet**     | `fact_platform_balance`             | Yearly Snapshot       | Every Batch         |
| **4. User/Seller Master**  | `fact_platform_balance`             | Yearly Snapshot       | Every Batch         |
| **5. User Wallets**        | `fact_user_accumulation`            | One row per Seller    | Balance Change      |
| **6. Account Profile**     | `dim_account` + `dim_bank_account`  | Per Account           | Profile/Bank Change |
| **7. Withdrawal Master**   | `fact_withdrawal_timeseries`        | Daily                 | Disbursement Update |
| **8. Withdrawal History**  | `disbursements` (Raw)               | Transaction           | Real-time           |

---

## 9. Dashboard Query Recipes

Sample SQL queries for populating specific dashboard pages.

### A. Dashboard Overview (`/`)

**Components**:

1. **Cards**: Total Revenue | Platform Balance in Wallet | Total Active Sellers
2. **Chart**: Revenue Trend (Last 30 Days)

```sql
-- 1. Overview Cards (Single Query)
SELECT
  -- Revenue Card (YTD)
  total_revenue_ytd,
  (convenience_fee_ytd + subscription_fee_ytd) AS current_revenue,

  -- Platform Wallet Card
  platform_total_balance,
  platform_available_balance,

  -- Users Card
  total_seller_accounts,
  active_transactions_count AS active_users_last_30d
FROM fact_platform_balance
WHERE date_key = CAST(TO_CHAR(DATE_TRUNC('year', NOW()), 'YYYYMMDD') AS INT);

-- Optional fallback (if current year row is not present yet)
SELECT
  total_revenue_ytd,
  (convenience_fee_ytd + subscription_fee_ytd) AS current_revenue,
  platform_total_balance,
  platform_available_balance,
  total_seller_accounts,
  active_transactions_count AS active_users_last_30d
FROM fact_platform_balance
ORDER BY date_key DESC
LIMIT 1;

-- 2. Revenue Trend Chart (Daily)
SELECT
  date_key,
  convenience_fee_total + subscription_fee_total AS total_revenue,
  gateway_fee_paid_total AS expense_gateway,
  settlement_transaction_count
FROM fact_revenue_timeseries
WHERE interval_type = 'DAILY'
  AND date_key >= CAST(TO_CHAR(NOW() - INTERVAL '30 days', 'YYYYMMDD') AS INT)
ORDER BY date_key ASC;
```

### B. Platform Wallet (`/platform-wallet`)

**Components**:

1. **Detail Cards**: Available Balance | Pending In-Flight | Total Withdrawn
2. **Chart**: Fee Collection vs Expenses (Monthly)

```sql
-- 1. Wallet Detail Cards
SELECT
  platform_available_balance,
  platform_pending_balance,
  platform_total_balance,

  -- Pending Settlements (In-flight money)
  settlement_pending_count,

  -- Lifetime Stats
  total_revenue_ytd,
  gateway_fee_ytd
FROM fact_platform_balance
WHERE date_key = CAST(TO_CHAR(DATE_TRUNC('year', NOW()), 'YYYYMMDD') AS INT);

-- Optional fallback (if current year row is not present yet)
SELECT
  platform_available_balance,
  platform_pending_balance,
  platform_total_balance,
  settlement_pending_count,
  total_revenue_ytd,
  gateway_fee_ytd
FROM fact_platform_balance
ORDER BY date_key DESC
LIMIT 1;

-- 2. Financial Performance Chart (Monthly)
SELECT
  date_key, -- YYYYMM
  interval_label, -- "2026-03"
  (convenience_fee_total + subscription_fee_total) AS revenue_in,
  gateway_fee_paid_total AS expenses_out
FROM fact_revenue_timeseries
WHERE interval_type = 'MONTHLY'
  AND date_key >= CAST(TO_CHAR(NOW() - INTERVAL '12 months', 'YYYYMM') AS INT)
ORDER BY date_key DESC;
```

### C. User Wallets (`/user-wallets`)

**Components**:

1. **Filterable Table**: List of sellers with balances and status
2. **Action**: View details

```sql
-- 1. Seller Wallet List (Pagination: Offset/Limit)
SELECT
  da.account_id,        -- Link to detailed view
  da.owner_id,
  da.owner_type,        -- Should be SELLER

  fua.current_available_balance, -- Primary Sort Column
  fua.current_pending_balance,
  fua.total_earnings,   -- Lifetime value
  fua.total_withdrawn,

  fua.account_status,   -- Badge color
  fua.updated_at        -- Last activity
FROM fact_user_accumulation fua
JOIN dim_account da ON fua.dim_account_uuid = da.uuid
WHERE da.is_current = TRUE
  AND (:search IS NULL OR da.owner_id ILIKE :search)
ORDER BY fua.current_available_balance DESC
LIMIT :limit OFFSET :offset;
```

### D. Withdrawals (`/withdrawals`)

**Components**:

1. **Status Cards**: Pending Requests | Successfully Disbursed Today
2. **Table**: Recent withdrawal history with bank info

```sql
-- 1. Withdrawal Status Overview (Today)
SELECT
  count(*) FILTER (WHERE status = 'PENDING') as pending_count,
  count(*) FILTER (WHERE status = 'COMPLETED') as completed_today,
  COALESCE(sum(amount) FILTER (WHERE status = 'COMPLETED'), 0) as amount_disbursed_today
FROM disbursements
WHERE created_at >= CURRENT_DATE;

-- 2. Withdrawal List (Rich Details)
SELECT
  d.id,
  d.created_at,
  d.amount,
  d.currency,
  d.status,          -- PENDING | COMPLETED | FAILED
  d.failure_reason,

  -- Bank Details
  dim_bank.bank_name,
  d.account_number,
  d.account_name
FROM disbursements d
LEFT JOIN dim_bank ON d.bank_code = dim_bank.bank_code
ORDER BY d.created_at DESC
LIMIT :limit OFFSET :offset;
```

### E. Transactions (`/transactions`)

**Components**:

1. **Table**: Master list of all financial movements (Sales, Settlements, Fees)

```sql
-- 1. Master Transaction List
-- Note: Hits Raw Ledger Entries for auditability, joined with Product info
SELECT
  le.id,
  le.created_at,

  -- Amount & Flow
  le.amount,
  le.currency,
  le.entry_type,      -- CREDIT | DEBIT
  le.balance_bucket,  -- AVAILABLE | PENDING

  -- Context
  le.source_type,     -- PRODUCT_TRANSACTION | DISBURSEMENT
  le.description,

  -- Related Product Info (Index usage critical here)
  pt.invoice_number,
  pt.product_type
FROM ledger_entries le
LEFT JOIN product_transactions pt
  ON le.source_id = pt.id
  AND le.source_type = 'PRODUCT_TRANSACTION'
WHERE (:account_id IS NULL OR le.ledger_account_id = :account_id)
  AND (:date_from IS NULL OR le.created_at >= :date_from)
ORDER BY le.created_at DESC
LIMIT :limit OFFSET :offset;
```
