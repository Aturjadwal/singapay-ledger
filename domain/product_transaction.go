package domain

import (
	"github.com/21strive/redifu"

	"context"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// TransactionStatus represents the lifecycle state of a product transaction
type TransactionStatus string

const (
	TransactionStatusPending   TransactionStatus = "PENDING"
	TransactionStatusCompleted TransactionStatus = "COMPLETED"
	TransactionStatusSettled   TransactionStatus = "SETTLED"
	TransactionStatusFailed    TransactionStatus = "FAILED"
	TransactionStatusRefunded  TransactionStatus = "REFUNDED"
)

// FeeModel specifies who pays the payment gateway fee
type FeeModel string

const (
	// FeeModelGatewayOnCustomer - Customer pays: seller_price + platform_fee + gateway_fee
	// Seller receives: seller_price (100%)
	FeeModelGatewayOnCustomer FeeModel = "GATEWAY_ON_CUSTOMER"

	// FeeModelGatewayOnSeller - Customer pays: seller_price + platform_fee
	// Seller receives: seller_price - gateway_fee (absorbs gateway cost)
	FeeModelGatewayOnSeller FeeModel = "GATEWAY_ON_SELLER"
)

// FeeBreakdown represents the pricing breakdown for a transaction
type FeeBreakdown struct {
	SellerPrice     int64    // Seller's listed price
	PlatformFee     int64    // Platform markup
	GatewayFee      int64    // Payment gateway fee, as expected at payment time
	TotalCharged    int64    // What customer pays (varies by fee model)
	SellerNetAmount int64    // What seller actually receives (varies by fee model)
	FeeModel        FeeModel // Who pays the gateway fee
	Currency        Currency // IDR or USD
}

// ProductTransaction represents a product sale between buyer and seller
// Supports different product types: PHOTO, FOLDER, SUBSCRIPTION, etc.
type ProductTransaction struct {
	*redifu.Record           `json:",inline" bson:",inline" db:"-"`
	BuyerAccountID           string
	SellerAccountID          string
	ProductID                string // Product identifier (references external product system)
	ProductType              string // Type of product: PHOTO, FOLDER, SUBSCRIPTION, etc.
	InvoiceNumber            string
	Fee                      FeeBreakdown
	Status                   TransactionStatus
	Metadata                 map[string]any // Caller-defined metadata (product details, buyer/seller info, etc.)
	CompletedAt              *time.Time     // When the payer paid (money-in webhook)
	SettledAt                *time.Time     // When Singapay settled the funds into the available balance
	PlatformFeeTransferred   bool           // Whether platform fee has been transferred to platform sub-account
	PlatformFeeTransferredAt *time.Time     // When platform fee was successfully transferred between sub-accounts
	TransferRequestID        string         // merchant_ref_no used for the platform fee transfer (for idempotent retries)

	// SettledPlatformFeeMinor and SettledGatewayFeeMinor are what the fees turned out to
	// be once Singapay reported what it actually took, as opposed to Fee.PlatformFee and
	// Fee.GatewayFee, which are what was priced at checkout.
	//
	// IN SEN, not rupiah — the Minor suffix is the whole point of the name. Singapay's
	// money-in fee carries two decimals, the difference between it and the estimate is
	// routinely a fraction of a rupiah, and that fraction is the quantity this pair exists
	// to preserve. Fee.PlatformFee and Fee.GatewayFee beside them are whole rupiah.
	//
	// They differ from the priced figures whenever the gateway's real fee misses the
	// estimate. The seller never absorbs that difference in either direction — the seller
	// is paid what they were priced — so it lands entirely on the platform fee:
	//
	//     SettledPlatformFeeMinor = (Fee.PlatformFee × 100) - (actual - estimated)
	//
	// This matters beyond bookkeeping: ProcessPlatformFeeTransfer moves real money out of
	// the seller's Singapay sub-account, and moving the priced figure when the ledger
	// booked a different one puts the gateway and the ledger out of step. The transfer
	// reads SettledPlatformFeeMinor and falls back to Fee.PlatformFee.
	//
	// nil means "not recorded" — a transaction that settled before these were kept, or
	// one that has not settled at all. It never means zero.
	SettledPlatformFeeMinor *int64
	SettledGatewayFeeMinor  *int64

	// PlatformResidualMinor is the sub-rupiah part of SettledPlatformFeeMinor, in sen:
	// what the account transfer moves but a whole-rupiah ledger entry cannot express.
	//
	// It is the reconciliation handle for the platform's own two balances. The seller's
	// agree exactly and always; the platform's differ by SUM(PlatformResidualMinor) over
	// settled transactions, which is a number that can be produced on demand rather than
	// a drift nobody can account for.
	//
	// nil means "not recorded", as above. Zero is a real value and the common one.
	PlatformResidualMinor *int64
}

// ProductTransactionRepository defines data access for product transactions
type ProductTransactionRepository interface {
	GetByID(ctx context.Context, id string) (*ProductTransaction, error)
	GetByInvoiceNumber(ctx context.Context, invoiceNumber string) (*ProductTransaction, error)
	GetBySellerAccountID(ctx context.Context, sellerAccountID string, page, pageSize int) ([]*ProductTransaction, error)
	GetByBuyerAccountID(ctx context.Context, buyerAccountID string, page, pageSize int) ([]*ProductTransaction, error)
	GetPendingBySellerAccountID(ctx context.Context, sellerAccountID string) ([]*ProductTransaction, error)
	GetCompletedNotSettled(ctx context.Context, sellerAccountID string) ([]*ProductTransaction, error)
	GetAllBySellerID(ctx context.Context, sellerAccountID string) ([]*ProductTransaction, error)
	// GetBySellerAccountIDWithCursor returns transactions with cursor-based pagination using RandId
	// cursor: RandId of last item from previous page (empty for first page)
	// sortOrder: "ASC" or "DESC" for created_at ordering
	GetBySellerAccountIDWithCursor(ctx context.Context, sellerAccountID string, cursor string, pageSize int, sortOrder string) ([]*ProductTransaction, error)

	// GetPlatformIncomes returns the paid transactions (COMPLETED or SETTLED) that credited
	// the platform account — its own sales, and every sale that carries a platform fee —
	// each with the sum of the platform's ledger entries for it.
	//
	// Ordered by when the payer paid (completed_at, or created_at for a row that predates
	// it) and then by uuid, descending unless ascending is set. A non-nil after continues
	// strictly past that position.
	GetPlatformIncomes(ctx context.Context, platformAccountID string, after *KeysetCursor, limit int, ascending bool) ([]*PlatformIncome, error)

	Save(ctx context.Context, tx *ProductTransaction) error
	UpdateStatus(ctx context.Context, id string, status TransactionStatus, timestamp time.Time) error

	// UpdateStatusIf moves a transaction from one status to another only if it is
	// currently in `from`, and reports whether the row actually moved.
	//
	// This is the idempotency boundary for the money-in webhook, and it is a
	// conditional write rather than a read-then-write because Singapay retries and can
	// deliver the same confirmation twice at once. Two concurrent deliveries both read
	// PENDING, and without a condition on the write both go on to save a journal and a
	// full set of ledger entries — crediting the seller twice for one payment, in a
	// table that is insert-only and cannot be corrected without an audit.
	//
	// Called inside a transaction, the conditional UPDATE takes the row lock first: the
	// loser blocks, re-evaluates against the committed row, matches nothing, and reports
	// false. Its caller rolls back having written nothing.
	UpdateStatusIf(ctx context.Context, id string, from, to TransactionStatus, timestamp time.Time) (bool, error)
	SaveTransferRequestID(ctx context.Context, id string, requestID string) error
	MarkPlatformFeeTransferred(ctx context.Context, id string) error
	GetSettledWithoutPlatformFeeTransfer(ctx context.Context, limit int) ([]*ProductTransaction, error)

	// GetAwaitingSettlement returns COMPLETED transactions whose funds have not settled
	// yet, oldest first. The reconciler needs this to know which invoices it is waiting
	// on: Singapay's settlement webhook carries totals and a date window but no list of
	// the transactions the batch covered.
	GetAwaitingSettlement(ctx context.Context, limit int) ([]*ProductTransaction, error)

	// SaveSettledFees records what the fees turned out to be, IN SEN. Called inside the
	// same transaction as the status move, so a transaction never reaches SETTLED with the
	// figures the transfer step reads still unset.
	//
	// residualMinor is the sub-rupiah part of platformFeeMinor — the part the ledger entry
	// could not carry. Passing it here rather than deriving it later keeps the figure the
	// settling pass actually decided, instead of one recomputed from a rounded entry.
	SaveSettledFees(ctx context.Context, id string, platformFeeMinor, gatewayFeeMinor, residualMinor int64) error

	// OldestAwaitingSettlement returns when the oldest unsettled COMPLETED transaction
	// was completed, and false when there are none.
	//
	// This is the health signal the settlement worker leans on, and the one alarm that
	// cannot be fooled by a reconciler that runs cleanly and books nothing: if settlement
	// stops, this age climbs monotonically. A "worker succeeded" metric stays green
	// throughout.
	OldestAwaitingSettlement(ctx context.Context) (time.Time, bool, error)
}

// NewFeeBreakdown creates a FeeBreakdown with specified fee model and validates amounts
func NewFeeBreakdown(sellerPrice, platformFee, gatewayFee int64, currency Currency, feeModel FeeModel) (*FeeBreakdown, error) {
	if sellerPrice < 0 || platformFee < 0 || gatewayFee < 0 {
		return nil, ledgererr.ErrInvalidFeeBreakdown
	}

	var totalCharged, sellerNetAmount int64

	switch feeModel {
	case FeeModelGatewayOnCustomer:
		// Customer pays everything: seller_price + platform_fee + gateway_fee
		totalCharged = sellerPrice + platformFee + gatewayFee
		sellerNetAmount = sellerPrice // Seller gets 100% of their price

	case FeeModelGatewayOnSeller:
		// Customer pays: seller_price + platform_fee (no gateway fee)
		totalCharged = sellerPrice + platformFee
		sellerNetAmount = sellerPrice - gatewayFee // Seller bears the gateway fee; platform fee tracked separately

	default:
		// Default to customer pays all (backward compatibility)
		totalCharged = sellerPrice + platformFee + gatewayFee
		sellerNetAmount = sellerPrice
	}

	return &FeeBreakdown{
		SellerPrice:     sellerPrice,
		PlatformFee:     platformFee,
		GatewayFee:      gatewayFee,
		TotalCharged:    totalCharged,
		SellerNetAmount: sellerNetAmount,
		FeeModel:        feeModel,
		Currency:        currency,
	}, nil
}

// NewProductTransaction creates a new product transaction in PENDING status
// Supports multiple product types (PHOTO, FOLDER, SUBSCRIPTION, etc.)
func NewProductTransaction(
	buyerAccountID, sellerAccountID string,
	productID string,
	productType string,
	invoiceNumber string,
	fee FeeBreakdown,
	metadata map[string]any,
) *ProductTransaction {
	pt := &ProductTransaction{
		BuyerAccountID:  buyerAccountID,
		SellerAccountID: sellerAccountID,
		ProductID:       productID,
		ProductType:     productType,
		InvoiceNumber:   invoiceNumber,
		Fee:             fee,
		Status:          TransactionStatusPending,
		Metadata:        metadata,
	}

	redifu.InitRecord(pt)

	// CRITICAL FIX: redifu.InitRecord initializes pointer fields to zero time instead of nil
	// Explicitly set timestamp fields to nil to prevent "0001-01-01" in database
	pt.CompletedAt = nil
	pt.SettledAt = nil

	return pt
}

// GetSellerPayout returns the amount seller actually receives (net after fees)
func (pt *ProductTransaction) GetSellerPayout() Money {
	return Money{
		Amount:   pt.Fee.SellerNetAmount,
		Currency: pt.Fee.Currency,
	}
}

// GetPlatformRevenue returns the platform fee amount
func (pt *ProductTransaction) GetPlatformRevenue() Money {
	return Money{
		Amount:   pt.Fee.PlatformFee,
		Currency: pt.Fee.Currency,
	}
}

// GetTotalCharged returns the total amount charged to buyer
func (pt *ProductTransaction) GetTotalCharged() Money {
	return Money{
		Amount:   pt.Fee.TotalCharged,
		Currency: pt.Fee.Currency,
	}
}

// IsPending checks if transaction is waiting for payment
func (pt *ProductTransaction) IsPending() bool {
	return pt.Status == TransactionStatusPending
}

// IsCompleted checks if payment has been received
func (pt *ProductTransaction) IsCompleted() bool {
	return pt.Status == TransactionStatusCompleted
}

// IsSettled checks if transaction has been settled via reconciliation
func (pt *ProductTransaction) IsSettled() bool {
	return pt.Status == TransactionStatusSettled
}

// IsFailed checks if transaction has failed
func (pt *ProductTransaction) IsFailed() bool {
	return pt.Status == TransactionStatusFailed
}

// IsRefunded checks if transaction has been refunded
func (pt *ProductTransaction) IsRefunded() bool {
	return pt.Status == TransactionStatusRefunded
}

// CanTransitionTo validates if status transition is allowed
func (pt *ProductTransaction) CanTransitionTo(newStatus TransactionStatus) bool {
	switch pt.Status {
	case TransactionStatusPending:
		// PENDING can transition to COMPLETED, FAILED, or REFUNDED
		return newStatus == TransactionStatusCompleted ||
			newStatus == TransactionStatusFailed ||
			newStatus == TransactionStatusRefunded
	case TransactionStatusCompleted:
		// COMPLETED can transition to SETTLED or REFUNDED
		return newStatus == TransactionStatusSettled ||
			newStatus == TransactionStatusRefunded
	case TransactionStatusSettled:
		// SETTLED can only transition to REFUNDED (rare case)
		return newStatus == TransactionStatusRefunded
	case TransactionStatusFailed, TransactionStatusRefunded:
		// Terminal states - no transitions allowed
		return false
	default:
		return false
	}
}

// MarkCompleted transitions from PENDING to COMPLETED (when the money-in webhook is booked)
func (pt *ProductTransaction) MarkCompleted() error {
	if !pt.CanTransitionTo(TransactionStatusCompleted) {
		return ledgererr.ErrInvalidTransactionStatus
	}
	now := time.Now()
	pt.Status = TransactionStatusCompleted
	pt.CompletedAt = &now
	return nil
}

// MarkSettled transitions from COMPLETED to SETTLED (when the funds appear in a Singapay settlement)
func (pt *ProductTransaction) MarkSettled() error {
	if !pt.CanTransitionTo(TransactionStatusSettled) {
		return ledgererr.ErrInvalidTransactionStatus
	}
	now := time.Now()
	pt.Status = TransactionStatusSettled
	pt.SettledAt = &now
	return nil
}

// MarkFailed transitions from PENDING to FAILED
func (pt *ProductTransaction) MarkFailed() error {
	if !pt.CanTransitionTo(TransactionStatusFailed) {
		return ledgererr.ErrInvalidTransactionStatus
	}
	pt.Status = TransactionStatusFailed
	return nil
}

// MarkRefunded transitions to REFUNDED status
func (pt *ProductTransaction) MarkRefunded() error {
	if !pt.CanTransitionTo(TransactionStatusRefunded) {
		return ledgererr.ErrInvalidTransactionStatus
	}
	pt.Status = TransactionStatusRefunded
	return nil
}

// MarkPlatformFeeTransferred marks that platform fee has been transferred to platform sub-account
func (pt *ProductTransaction) MarkPlatformFeeTransferred() {
	now := time.Now()
	pt.PlatformFeeTransferred = true
	pt.PlatformFeeTransferredAt = &now
}

// NeedsPlatformFeeTransfer checks if this transaction is settled but platform fee not yet transferred
func (pt *ProductTransaction) NeedsPlatformFeeTransfer() bool {
	return pt.IsSettled() && pt.Fee.PlatformFee > 0 && !pt.PlatformFeeTransferred
}
