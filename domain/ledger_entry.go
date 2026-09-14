package domain

import (
	"github.com/21strive/redifu"

	"context"
)

// BalanceBucket represents which balance pool an entry affects.
// PENDING = captured but not yet settled (money held by provider)
// AVAILABLE = settled, withdrawable by the account owner
type BalanceBucket string

const (
	BalanceBucketPending   BalanceBucket = "PENDING"
	BalanceBucketAvailable BalanceBucket = "AVAILABLE"
)

// EntryType describes the financial classification of the entry.
// This follows the entry_type CHECK constraint in schema.sql.
type EntryType string

const (
	EntryTypeProductPayment     EntryType = "PRODUCT_PAYMENT"
	EntryTypePlatformCommission EntryType = "PLATFORM_COMMISSION"
	EntryTypeProcessorFee       EntryType = "PROCESSOR_FEE"
	EntryTypeDisbursement       EntryType = "DISBURSEMENT"
	// EntryTypeDisbursementReversal returns a reserved amount to the available balance
	// when a payout definitively did not happen. ledger_entries is insert-only, so a
	// reservation is undone by writing its opposite, never by deleting it.
	EntryTypeDisbursementReversal EntryType = "DISBURSEMENT_REVERSAL"
	EntryTypeSettlementClear      EntryType = "SETTLEMENT_CLEAR" // Clear pending balance
	EntryTypeSettlementNet        EntryType = "SETTLEMENT_NET"   // Add net to available balance
	EntryTypeSettlement           EntryType = "SETTLEMENT"       // Generic settlement
	EntryTypeReconciliation       EntryType = "RECONCILIATION"
	EntryTypeFeeAdjustment        EntryType = "FEE_ADJUSTMENT"
)

// SourceType describes the business origin (which table generated this entry).
type SourceType string

const (
	SourceTypeProductTransaction SourceType = "PRODUCT_TRANSACTION"
	SourceTypeDisbursement       SourceType = "DISBURSEMENT"
	SourceTypeManualAdjustment   SourceType = "MANUAL_ADJUSTMENT"

	// SourceTypeSettlementBatch is never written any more: the settlement_batches table
	// is gone and the settling pass sources its entries on the product transaction.
	//
	// It stays because entries are insert-only. Rows booked by the old batch reconciler
	// carry this source_type with a settlement_batches.uuid in source_id, the value is
	// still in the CHECK constraint on both journals and ledger_entries, and a reader
	// that cannot name it cannot read its own history. Do not remove it.
	SourceTypeSettlementBatch SourceType = "SETTLEMENT_BATCH"
)

// LedgerEntry is an immutable financial record.
// Positive amount = credit to the account's bucket.
// Negative amount = debit from the account's bucket.
// BalanceAfter = running balance for this account+bucket after applying this entry.
type LedgerEntry struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`
	JournalUUID    string // Double-entry grouping
	AccountUUID    string
	Amount         int64 // positive = credit, negative = debit
	BalanceBucket  BalanceBucket
	BalanceAfter   int64 // Running balance after this entry (for quick queries)
	EntryType      EntryType
	SourceType     SourceType
	SourceID       string // product_transaction_uuid, settlement_batch_uuid, disbursement_id, etc.
	Metadata       map[string]any
}

// LedgerEntryRepository defines data access for immutable ledger entries.
// Entries are insert-only; no Update or Delete methods are intentionally provided.
type LedgerEntryRepository interface {
	// Save inserts a single entry. Returns an error if the ID already exists.
	Save(ctx context.Context, entry *LedgerEntry) error

	// SaveBatch inserts multiple entries atomically (within the same transaction).
	SaveBatch(ctx context.Context, entries []*LedgerEntry) error

	// GetBalance returns the derived balance for an account bucket by summing all entries.
	GetBalance(ctx context.Context, accountID string, bucket BalanceBucket) (int64, error)

	// GetAllBalances returns both PENDING and AVAILABLE derived balances for an account.
	GetAllBalances(ctx context.Context, accountID string) (pending, available int64, err error)

	// SumPendingBalanceBySellerID returns the total PENDING balance for a seller,
	// resolved by joining ledger_entries → accounts on owner_type='SELLER' AND owner_id=sellerID.
	SumPendingBalanceBySellerID(ctx context.Context, sellerID string) (int64, error)

	// SumAvailableBalanceBySellerID returns the total AVAILABLE balance for a seller,
	// resolved by joining ledger_entries → accounts on owner_type='SELLER' AND owner_id=sellerID.
	SumAvailableBalanceBySellerID(ctx context.Context, sellerID string) (int64, error)

	// GetAllBalancesBySellerID returns both PENDING and AVAILABLE derived balances
	// for a seller in a single query, resolved via owner_type='SELLER' AND owner_id=sellerID.
	GetAllBalancesBySellerID(ctx context.Context, sellerID string) (pending, available int64, err error)

	// GetByJournalID returns all entries grouped in a specific journal.
	GetByJournalID(ctx context.Context, journalID string) ([]*LedgerEntry, error)

	// GetBySourceID returns all entries originating from a given source.
	GetBySourceID(ctx context.Context, sourceID string) ([]*LedgerEntry, error)

	// GetByAccountID returns paginated entries for an account, newest first.
	GetByAccountID(ctx context.Context, accountID string, limit, offset int) ([]*LedgerEntry, error)

	// GetLastBalanceAfter returns the most recent balance_after for an account+bucket.
	// Returns 0 if no entries exist yet.
	GetLastBalanceAfter(ctx context.Context, accountID string, bucket BalanceBucket) (int64, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Factory helpers — produce the canonical entry sets for each business event.
// Each function returns a slice ready for SaveBatch inside a transaction.
// ─────────────────────────────────────────────────────────────────────────────

// NewPaymentEntries creates the three PENDING ledger entries written on webhook
// success (Phase 2, Step D of the flow).
//
//	seller account   +sellerAmount  PENDING  PAYMENT
//	platform account +platformFee   PENDING  PLATFORM_FEE
//	gateway account  +gatewayFee    PENDING  PAYMENT
//
// productTransactionID is used as the reference_id for all three entries.
// journalUUID groups these entries as part of a single PAYMENT_SUCCESS event.
func NewPaymentEntries(
	journalUUID string,
	productTransactionID string,
	sellerAccountID string,
	sellerAmount int64,
	platformAccountID string,
	platformFee int64,
	gatewayAccountID string,
	gatewayFee int64,
) []*LedgerEntry {
	sellerEntry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   sellerAccountID,
		Amount:        sellerAmount,
		BalanceBucket: BalanceBucketPending,
		EntryType:     EntryTypeProductPayment,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(sellerEntry)

	platformEntry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   platformAccountID,
		Amount:        platformFee,
		BalanceBucket: BalanceBucketPending,
		EntryType:     EntryTypePlatformCommission,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(platformEntry)

	gatewayEntry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   gatewayAccountID,
		Amount:        gatewayFee,
		BalanceBucket: BalanceBucketPending,
		EntryType:     EntryTypeProcessorFee,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(gatewayEntry)

	return []*LedgerEntry{sellerEntry, platformEntry, gatewayEntry}
}

// NewSettlementEntriesForAccount creates the PENDING→AVAILABLE conversion pair
// for a single account on settlement (Phase 3, Steps B & C).
//
//	account -amount  PENDING    SETTLEMENT   (debit pending)
//	account +amount  AVAILABLE  SETTLEMENT   (credit available)
//
// productTransactionID is used as the source_id to link back to the business transaction.
// journalUUID groups these entries as part of a single SETTLEMENT event.
func NewSettlementEntriesForAccount(
	journalUUID string,
	productTransactionID string,
	accountID string,
	amount int64,
) []*LedgerEntry {
	return NewSettlementEntriesForAccountSplit(journalUUID, productTransactionID, accountID, amount, amount)
}

// NewSettlementEntriesForAccountSplit is [NewSettlementEntriesForAccount] for an account
// whose two legs differ:
//
//	account -pending    PENDING    SETTLEMENT_CLEAR
//	account +available  AVAILABLE  SETTLEMENT_NET
//
// They differ for exactly one account per settlement — the one balancing the gateway fee
// delta. Its PENDING was credited the fee quoted at checkout and has to clear by precisely
// that or the bucket never empties; what it actually earned is that figure adjusted by the
// difference between the quoted gateway fee and the one Singapay really took. Passing a
// single amount for both legs, as the settling pass once did, leaves the difference stranded
// in PENDING and needs a second write-off entry to mop up — two entries describing one fact,
// which is how the same delta ends up counted twice.
//
// The seller is never this account. Its two legs are always equal by design, which is the
// ledger-side statement of the rule that a seller is paid what they were priced.
func NewSettlementEntriesForAccountSplit(
	journalUUID string,
	productTransactionID string,
	accountID string,
	pending int64,
	available int64,
) []*LedgerEntry {
	// TODO: VALIDATE ALL THE LEDGER ENTRY

	pendingEntry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountID,
		Amount:        -pending,
		BalanceBucket: BalanceBucketPending,
		EntryType:     EntryTypeSettlementClear,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(pendingEntry)

	availableEntry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountID,
		Amount:        available,
		BalanceBucket: BalanceBucketAvailable,
		EntryType:     EntryTypeSettlementNet,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(availableEntry)

	return []*LedgerEntry{pendingEntry, availableEntry}
}

// NewGatewayFeeSettlementEntry creates the single PENDING clearance entry for the
// gateway expense account on settlement (Phase 3, Step D).
//
//	gateway account  -gatewayFee  PENDING  SETTLEMENT_FEE_CLEAR
//
// There is intentionally no AVAILABLE credit — Singapay keeps the fee.
// journalUUID groups this entry with other settlement entries.
func NewGatewayFeeSettlementEntry(
	journalUUID string,
	productTransactionID string,
	gatewayAccountID string,
	gatewayFee int64,
) *LedgerEntry {
	entry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   gatewayAccountID,
		Amount:        -gatewayFee,
		BalanceBucket: BalanceBucketPending,
		EntryType:     EntryTypeSettlement,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(entry)
	return entry
}

// NewFeeAdjustmentWriteOffEntry creates a PENDING debit (write-off) for the absorbing party
// when the fee Singapay actually took exceeds the one expected at payment time.
//
//	account  -amount  PENDING  FEE_ADJUSTMENT  (terminal — no AVAILABLE counterpart)
//
// Used for:
//   - Platform account (GATEWAY_ON_CUSTOMER): platform absorbs the delta
//   - Seller account (GATEWAY_ON_SELLER): seller absorbs the delta
func NewFeeAdjustmentWriteOffEntry(
	journalUUID string,
	productTransactionID string,
	accountID string,
	amount int64,
) *LedgerEntry {
	entry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountID,
		Amount:        -amount,
		BalanceBucket: BalanceBucketPending,
		EntryType:     EntryTypeFeeAdjustment,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(entry)
	return entry
}

// NewFeeAdjustmentCreditEntry creates an AVAILABLE credit (surplus) for the seller
// when the fee Singapay actually took is below the one expected at payment time.
//
//	seller account  +amount  AVAILABLE  FEE_ADJUSTMENT  (terminal — no PENDING source)
//
// The surplus comes from Singapay charging less than expected; credited directly to seller.
func NewFeeAdjustmentCreditEntry(
	journalUUID string,
	productTransactionID string,
	accountID string,
	amount int64,
) *LedgerEntry {
	entry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountID,
		Amount:        amount,
		BalanceBucket: BalanceBucketAvailable,
		EntryType:     EntryTypeFeeAdjustment,
		SourceType:    SourceTypeProductTransaction,
		SourceID:      productTransactionID,
	}
	redifu.InitRecord(entry)
	return entry
}

// NewDisbursementEntry creates the AVAILABLE debit entry for a seller withdrawal
// (Optional Phase — Seller Withdrawal).
//
//	seller account  -amount  AVAILABLE  DISBURSEMENT
//
// journalUUID groups this entry as part of a DISBURSEMENT event.
func NewDisbursementEntry(
	journalUUID string,
	disbursementID string,
	accountID string,
	amount int64,
) *LedgerEntry {
	entry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountID,
		Amount:        -amount,
		BalanceBucket: BalanceBucketAvailable,
		EntryType:     EntryTypeDisbursement,
		SourceType:    SourceTypeDisbursement,
		SourceID:      disbursementID,
	}
	redifu.InitRecord(entry)
	return entry
}

// NewDisbursementReversalEntry gives a reserved amount back to the available balance.
//
// Write this only when the payout is known not to have happened — Singapay refused it
// outright (singapay.OutcomeRefused), or reported transaction status 04/05/06. An unknown
// outcome must NOT be reversed:
// the money may be on its way, and handing it back to the available balance is what lets
// it be withdrawn a second time.
func NewDisbursementReversalEntry(
	journalUUID string,
	disbursementID string,
	accountID string,
	amount int64,
) *LedgerEntry {
	entry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountID,
		Amount:        amount,
		BalanceBucket: BalanceBucketAvailable,
		EntryType:     EntryTypeDisbursementReversal,
		SourceType:    SourceTypeDisbursement,
		SourceID:      disbursementID,
	}
	redifu.InitRecord(entry)
	return entry
}

// ─────────────────────────────────────────────────────────────────────────────
// DerivedBalance is the result of querying a balance from ledger_entries.
// It is never persisted — always calculated on the fly.
// ─────────────────────────────────────────────────────────────────────────────

// DerivedBalance holds the computed balances for an account derived from entries.
type DerivedBalance struct {
	AccountUUID string
	Pending     int64
	Available   int64
	Currency    Currency // populated by the caller from the account record
}

// Total returns the sum of pending and available balances.
func (b DerivedBalance) Total() int64 {
	return b.Pending + b.Available
}
