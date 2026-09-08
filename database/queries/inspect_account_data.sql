-- ============================================================================
-- INSPECTION QUERIES FOR ACCOUNT: rifqoi@fotafoto.com
-- Account UUID: 045e6347-4008-4480-97a6-d174e1dccb09
-- ============================================================================
-- These are READ-ONLY queries to inspect account data before/after cleanup
-- ============================================================================
-- ============================================================================
-- ACCOUNT OVERVIEW & BALANCES
-- ============================================================================
SELECT
    uuid,
    randid,
    singapay_account_id,
    singapay_account_number,
    owner_type,
    owner_id,
    currency,
    pending_balance,
    available_balance,
    total_withdrawal_amount,
    total_deposit_amount,
    created_at,
    updated_at
FROM
    ledger_accounts
WHERE
    uuid = '045e6347-4008-4480-97a6-d174e1dccb09';

-- ============================================================================
-- LEDGER ENTRIES (Double-Entry Bookkeeping Records)
-- ============================================================================
-- All financial movements for this account
SELECT
    le.uuid,
    le.randid,
    le.journal_uuid,
    le.amount,
    le.balance_bucket,
    le.entry_type,
    le.source_type,
    le.source_id,
    le.balance_after,
    le.metadata,
    le.created_at,
    j.event_type AS journal_event_type
FROM
    ledger_entries le
    LEFT JOIN journals j ON le.journal_uuid = j.uuid
WHERE
    le.account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    le.created_at DESC;

-- Summary by entry type
SELECT
    entry_type,
    balance_bucket,
    COUNT(*) AS entry_count,
    SUM(amount) AS total_amount,
    MIN(created_at) AS first_entry,
    MAX(created_at) AS last_entry
FROM
    ledger_entries
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    entry_type,
    balance_bucket
ORDER BY
    entry_type,
    balance_bucket;

-- ============================================================================
-- PRODUCT TRANSACTIONS (Sales/Purchases)
-- ============================================================================
-- Transactions where this account is the seller
SELECT
    'SELLER' AS role,
    pt.uuid,
    pt.randid,
    pt.invoice_number,
    pt.buyer_account_id,
    pt.product_id,
    pt.product_type,
    pt.seller_price,
    pt.platform_fee,
    pt.gateway_fee,
    pt.total_charged,
    pt.seller_net_amount,
    pt.fee_model,
    pt.currency,
    pt.status,
    pt.created_at,
    pt.completed_at,
    pt.settled_at,
    pt.platform_fee_transferred,
    pt.platform_fee_transferred_at,
    pt.metadata
FROM
    product_transactions pt
WHERE
    pt.seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    pt.created_at DESC;

-- Transactions where this account is the buyer
SELECT
    'BUYER' AS role,
    pt.uuid,
    pt.randid,
    pt.invoice_number,
    pt.seller_account_id,
    pt.product_id,
    pt.product_type,
    pt.seller_price,
    pt.platform_fee,
    pt.gateway_fee,
    pt.total_charged,
    pt.seller_net_amount,
    pt.fee_model,
    pt.currency,
    pt.status,
    pt.created_at,
    pt.completed_at,
    pt.settled_at,
    pt.metadata
FROM
    product_transactions pt
WHERE
    pt.buyer_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    pt.created_at DESC;

-- Transaction summary
SELECT
    status,
    fee_model,
    COUNT(*) AS count,
    SUM(seller_price) AS total_seller_price,
    SUM(platform_fee) AS total_platform_fee,
    SUM(gateway_fee) AS total_gateway_fee,
    SUM(total_charged) AS total_charged,
    SUM(seller_net_amount) AS total_seller_net
FROM
    product_transactions
WHERE
    seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    status,
    fee_model
ORDER BY
    status,
    fee_model;

-- ============================================================================
-- PAYMENT REQUESTS (Singapay)
-- ============================================================================
SELECT
    pr.uuid,
    pr.randid,
    pr.product_transaction_uuid,
    pr.request_id,
    pr.payment_code,
    pr.payment_channel,
    pr.payment_url,
    pr.amount,
    pr.currency,
    pr.status,
    pr.created_at,
    pr.completed_at,
    pr.expires_at,
    pr.failure_reason,
    pt.invoice_number
FROM
    payment_requests pr
    JOIN product_transactions pt ON pr.product_transaction_uuid = pt.uuid
WHERE
    pt.seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
    OR pt.buyer_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    pr.created_at DESC;

-- ============================================================================
-- DISBURSEMENTS (Withdrawals)
-- ============================================================================
SELECT
    uuid,
    randid,
    amount,
    currency,
    status,
    bank_code,
    account_number,
    account_name,
    description,
    external_transaction_id,
    failure_reason,
    created_at,
    updated_at,
    processed_at
FROM
    disbursements
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    created_at DESC;

-- Disbursement summary
SELECT
    status,
    COUNT(*) AS count,
    SUM(amount) AS total_amount,
    MIN(created_at) AS first_disbursement,
    MAX(created_at) AS last_disbursement
FROM
    disbursements
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    status
ORDER BY
    status;

-- ============================================================================
-- SETTLEMENT BATCHES (CSV Upload History)
-- ============================================================================
SELECT
    uuid,
    randid,
    report_file_name,
    settlement_date,
    batch_id,
    gross_amount,
    net_amount,
    gateway_fee,
    currency,
    uploaded_by,
    uploaded_at,
    processed_at,
    processing_status,
    matched_count,
    unmatched_count,
    failure_reason,
    metadata,
    created_at
FROM
    settlement_batches
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    settlement_date DESC;

-- ============================================================================
-- SETTLEMENT ITEMS (Individual CSV Rows)
-- ============================================================================
SELECT
    si.uuid,
    si.randid,
    si.settlement_batch_uuid,
    si.product_transaction_uuid,
    si.invoice_number,
    si.sub_account,
    si.transaction_amount,
    si.pay_to_merchant,
    si.allocated_fee,
    si.is_matched,
    si.expected_net_amount,
    si.amount_discrepancy,
    si.csv_row_number,
    si.created_at,
    sb.settlement_date,
    sb.report_file_name
FROM
    settlement_items si
    JOIN settlement_batches sb ON si.settlement_batch_uuid = sb.uuid
WHERE
    si.seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    sb.settlement_date DESC,
    si.csv_row_number;

-- ============================================================================
-- RECONCILIATION DISCREPANCIES
-- ============================================================================
SELECT
    rd.uuid,
    rd.randid,
    rd.settlement_batch_uuid,
    rd.discrepancy_type,
    rd.expected_pending,
    rd.actual_pending,
    rd.expected_available,
    rd.actual_available,
    rd.pending_diff,
    rd.available_diff,
    rd.item_discrepancy_count,
    rd.total_item_discrepancy,
    rd.status,
    rd.detected_at,
    rd.resolved_at,
    rd.resolution_notes,
    sb.settlement_date,
    sb.report_file_name
FROM
    reconciliation_discrepancies rd
    JOIN settlement_batches sb ON rd.settlement_batch_uuid = sb.uuid
WHERE
    rd.account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
ORDER BY
    rd.detected_at DESC;

-- ============================================================================
-- JOURNALS (Accounting Events)
-- ============================================================================
-- Journals where this account participated
SELECT
    DISTINCT j.uuid,
    j.randid,
    j.event_type,
    j.source_type,
    j.source_id,
    j.metadata,
    j.created_at,
    COUNT(le.uuid) AS entry_count
FROM
    journals j
    JOIN ledger_entries le ON j.uuid = le.journal_uuid
WHERE
    le.account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
GROUP BY
    j.uuid,
    j.randid,
    j.event_type,
    j.source_type,
    j.source_id,
    j.metadata,
    j.created_at
ORDER BY
    j.created_at DESC;

-- ============================================================================
-- COMPLETE FINANCIAL SUMMARY
-- ============================================================================
SELECT
    'Account Balance' AS metric,
    'Pending' AS category,
    pending_balance AS value
FROM
    ledger_accounts
WHERE
    uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Account Balance',
    'Available',
    available_balance
FROM
    ledger_accounts
WHERE
    uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Account Balance',
    'Total Deposits',
    total_deposit_amount
FROM
    ledger_accounts
WHERE
    uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Account Balance',
    'Total Withdrawals',
    total_withdrawal_amount
FROM
    ledger_accounts
WHERE
    uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Ledger Entries',
    'Count',
    COUNT(*)
FROM
    ledger_entries
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Product Transactions (Seller)',
    'Count',
    COUNT(*)
FROM
    product_transactions
WHERE
    seller_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Product Transactions (Buyer)',
    'Count',
    COUNT(*)
FROM
    product_transactions
WHERE
    buyer_account_id = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Disbursements',
    'Count',
    COUNT(*)
FROM
    disbursements
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Settlement Batches',
    'Count',
    COUNT(*)
FROM
    settlement_batches
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
UNION
ALL
SELECT
    'Journals Participated',
    'Count',
    COUNT(DISTINCT journal_uuid)
FROM
    ledger_entries
WHERE
    account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09';

-- ============================================================================
-- BALANCE RECONCILIATION CHECK
-- ============================================================================
-- Compare account balance fields with sum of ledger entries
WITH entry_sums AS (
    SELECT
        SUM(
            CASE
                WHEN balance_bucket = 'PENDING' THEN amount
                ELSE 0
            END
        ) AS pending_sum,
        SUM(
            CASE
                WHEN balance_bucket = 'AVAILABLE' THEN amount
                ELSE 0
            END
        ) AS available_sum
    FROM
        ledger_entries
    WHERE
        account_uuid = '045e6347-4008-4480-97a6-d174e1dccb09'
)
SELECT
    'Balance Check' AS check_type,
    la.pending_balance AS account_pending,
    es.pending_sum AS entries_pending,
    la.pending_balance - es.pending_sum AS pending_diff,
    la.available_balance AS account_available,
    es.available_sum AS entries_available,
    la.available_balance - es.available_sum AS available_diff
FROM
    ledger_accounts la,
    entry_sums es
WHERE
    la.uuid = '045e6347-4008-4480-97a6-d174e1dccb09';