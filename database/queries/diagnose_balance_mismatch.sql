-- ============================================================================
-- BALANCE MISMATCH DIAGNOSTIC FOR: rifqoi@fotafoto.com
-- Account UUID: 045e6347-4008-4480-97a6-d174e1dccb09
-- ============================================================================
-- This script helps diagnose why total_deposits and total_withdrawals
-- don't match expected values
-- ============================================================================
-- ============================================================================
-- CURRENT ACCOUNT STATE
-- ============================================================================
SELECT
    'Current Account State' AS section,
    uuid,
    owner_id,
    pending_balance,
    available_balance,
    total_withdrawal_amount,
    total_deposit_amount,
    updated_at
FROM
    ledger_accounts
WHERE
    uuid = '045e6347-4008-4480-97a6-d174e1dccb09';

-- ============================================================================
-- WHAT SHOULD total_deposit_amount BE?
-- ============================================================================
-- Sum of all SETTLED product transactions where this account is the seller
-- (Only SETTLED transactions should count as deposits)
SELECT
    'Expected Deposits (SETTLED only)' AS calculation,
    COUNT(*) AS transaction_count,
    SUM(seller_price + platform_fee) AS expected_total_deposits,
    STRING_AGG(
        invoice_number || ' (' || status || '): ' || (seller_price + platform_fee),
        ', '
    ) AS breakdown
FROM
    product_transactions
WHERE
    seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
    AND status = 'SETTLED';

-- All product transactions (any status)
SELECT
    'All Product Transactions' AS calculation,
    status,
    COUNT(*) AS count,
    SUM(seller_price + platform_fee) AS total_amount
FROM
    product_transactions
WHERE
    seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    status;

-- ============================================================================
-- WHAT SHOULD total_withdrawal_amount BE?
-- ============================================================================
-- Sum of all COMPLETED disbursements
SELECT
    'Expected Withdrawals (COMPLETED only)' AS calculation,
    COUNT(*) AS disbursement_count,
    COALESCE(SUM(amount), 0) AS expected_total_withdrawals,
    STRING_AGG(
        uuid || ' (' || status || '): ' || amount,
        ', '
    ) AS breakdown
FROM
    disbursements
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
    AND status = 'COMPLETED';

-- All disbursements (any status)
SELECT
    'All Disbursements' AS calculation,
    status,
    COUNT(*) AS count,
    COALESCE(SUM(amount), 0) AS total_amount
FROM
    disbursements
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    status;

-- ============================================================================
-- LEDGER ENTRIES ANALYSIS
-- ============================================================================
-- All ledger entries to understand the money flow
SELECT
    'Ledger Entries Detail' AS section,
    le.created_at,
    le.entry_type,
    le.balance_bucket,
    le.amount,
    le.balance_after,
    le.source_type,
    le.source_id,
    j.event_type
FROM
    ledger_entries le
    LEFT JOIN journals j ON le.journal_uuid = j.uuid
WHERE
    le.account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    le.created_at ASC;

-- Sum of ledger entries by type
SELECT
    'Ledger Entries Summary' AS section,
    entry_type,
    balance_bucket,
    COUNT(*) AS entry_count,
    SUM(amount) AS total_amount
FROM
    ledger_entries
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    entry_type,
    balance_bucket;

-- ============================================================================
-- COMPARE: What we have vs What we should have
-- ============================================================================
WITH account_state AS (
    SELECT
        pending_balance,
        available_balance,
        total_deposit_amount,
        total_withdrawal_amount
    FROM
        ledger_accounts
    WHERE
        uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
),
expected_deposits AS (
    SELECT
        COALESCE(SUM(seller_price + platform_fee), 0) AS amount
    FROM
        product_transactions
    WHERE
        seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
        AND status = 'SETTLED'
),
expected_withdrawals AS (
    SELECT
        COALESCE(SUM(amount), 0) AS amount
    FROM
        disbursements
    WHERE
        account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
        AND status = 'COMPLETED'
),
ledger_pending AS (
    SELECT
        COALESCE(SUM(amount), 0) AS amount
    FROM
        ledger_entries
    WHERE
        account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
        AND balance_bucket = 'PENDING'
),
ledger_available AS (
    SELECT
        COALESCE(SUM(amount), 0) AS amount
    FROM
        ledger_entries
    WHERE
        account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
        AND balance_bucket = 'AVAILABLE'
)
SELECT
    'Comparison Report' AS section,
    'Pending Balance' AS field,
    a.pending_balance AS account_field,
    lp.amount AS ledger_sum,
    a.pending_balance - lp.amount AS difference
FROM
    account_state a,
    ledger_pending lp
UNION
ALL
SELECT
    'Comparison Report',
    'Available Balance',
    a.available_balance,
    la.amount,
    a.available_balance - la.amount
FROM
    account_state a,
    ledger_available la
UNION
ALL
SELECT
    'Comparison Report',
    'Total Deposits',
    a.total_deposit_amount,
    ed.amount,
    a.total_deposit_amount - ed.amount
FROM
    account_state a,
    expected_deposits ed
UNION
ALL
SELECT
    'Comparison Report',
    'Total Withdrawals',
    a.total_withdrawal_amount,
    ew.amount,
    a.total_withdrawal_amount - ew.amount
FROM
    account_state a,
    expected_withdrawals ew;

-- ============================================================================
-- POSSIBLE ISSUES TO CHECK
-- ============================================================================
-- 1. Are there COMPLETED transactions that aren't SETTLED?
SELECT
    'COMPLETED but not SETTLED transactions' AS issue,
    COUNT(*) AS count,
    SUM(seller_price + platform_fee) AS total_amount
FROM
    product_transactions
WHERE
    seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
    AND status = 'COMPLETED';

-- 2. Are there ledger entries not matching transactions?
SELECT
    'Ledger entries with no source' AS issue,
    COUNT(*) AS count,
    SUM(amount) AS total_amount
FROM
    ledger_entries le
WHERE
    le.account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
    AND le.source_type = 'PRODUCT_TRANSACTION'
    AND NOT EXISTS (
        SELECT
            1
        FROM
            product_transactions pt
        WHERE
            pt.uuid = le.source_id
    );

-- 3. Check if there are orphaned disbursements affecting totals
SELECT
    'Pending/Failed Disbursements affecting totals' AS issue,
    status,
    COUNT(*) AS count,
    SUM(amount) AS total_amount
FROM
    disbursements
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
    AND status NOT IN ('COMPLETED')
GROUP BY
    status;

-- ============================================================================
-- DISCREPANCY DETAIL
-- ============================================================================
SELECT
    'Discrepancy from API' AS source,
    'Gateway says: Pending = 204000, Available = 138015' AS gateway_values,
    'Our DB says: Pending = 0, Available = 46005' AS our_values,
    'Difference: Pending diff = 204000, Available diff = 92010' AS differences;

-- ============================================================================
-- RECOMMENDATION
-- ============================================================================
-- Based on the data, here's what likely happened:
-- 
-- 1. total_deposit_amount (92010) might be tracking something incorrectly
--    - Should be: Sum of SETTLED transactions (seller_price + platform_fee)
--    - Currently showing: 92010
-- 
-- 2. total_withdrawal_amount (46005) but 0 disbursements
--    - This suggests the field was updated incorrectly
--    - OR there was a disbursement that was deleted
--    - OR the field is being updated at the wrong time
-- 
-- 3. The gateway mismatch (expected vs actual) suggests:
--    - Our ledger_accounts balances (pending=0, available=46005) don't match the gateway
--    - The gateway says: pending=204000, available=138015
--    - This is a major discrepancy that needs reconciliation
-- 
-- NEXT STEPS:
-- 1. Run a settlement reconciliation to sync with the gateway
-- 2. Check when total_deposit_amount and total_withdrawal_amount are updated
-- 3. Verify the logic in the code that updates these fields
-- ============================================================================