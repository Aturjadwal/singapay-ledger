package domain

import (
	"github.com/21strive/redifu"

	"context"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// SettlementItem is one settled gateway transaction, matched to a ProductTransaction by
// merchant reference.
//
// Singapay publishes no settlement file. A settlement webhook announces a batch and its
// date window, and the rows it covered are read back from the per-product transaction
// lists filtered on that window. An item is therefore assembled from a VA, QRIS, e-wallet
// or payment-link record, and it records which of those it came from — because the four
// do not report the same things.
type SettlementItem struct {
	*redifu.Record         `json:",inline" bson:",inline" db:"-"`
	SettlementBatchUUID    string
	ProductTransactionUUID string // Empty if unmatched
	SellerAccountID        string // Cached from ProductTransaction for efficient grouping

	// InvoiceNumber is the merchant reference Singapay echoed back — our own
	// product_transactions.invoice_number, sent as merchant_reff_no when the payment
	// instrument was created.
	InvoiceNumber string

	// GatewayAccountID is the Singapay sub-account ULID the funds landed in. It is the
	// safety check that a settled row belongs to the seller we think it does.
	GatewayAccountID string

	// GatewayTransactionID is Singapay's own id for the payment. Kept because a dispute
	// about a settled amount is argued in Singapay's identifiers, not ours.
	GatewayTransactionID string

	// PaymentChannel is the Singapay channel code the row came from (VA_BCA, QRIS,
	// EWALLET_DANA, PAYMENT_LINK). It explains FeeReported: see below.
	PaymentChannel string

	TransactionAmount int64 // Gross the payer was charged
	PayToMerchant     int64 // Net credited to the sub-account, after the channel fee
	AllocatedFee      int64 // The channel fee Singapay actually took

	// FeeReported says whether AllocatedFee is a fact or a fallback.
	//
	// Virtual account, QRIS and e-wallet each report their own fee. Payment link does
	// not — no fee and no net amount anywhere in its schema. For those rows AllocatedFee
	// is copied from the fee expected at payment time, which makes the fee delta zero by
	// construction. That is not a reconciliation; it is the absence of one, and it must
	// be visible rather than inferred from a suspiciously perfect match.
	FeeReported bool

	IsMatched      bool              // Whether this item was matched to a transaction
	RawGatewayData map[string]string // The gateway record's own fields, for audit

	// Reconciliation fields (populated when matched)
	ExpectedNetAmount int64 // SellerNetAmount + PlatformFee from ProductTransaction
	AmountDiscrepancy int64 // PayToMerchant - ExpectedNetAmount (should be 0 if matched correctly)
	FeeAdjustment     int64 // feeDelta = ActualGatewayFee - ExpectedGatewayFee (0 if no mismatch)
}

// SettlementItemRepository defines data access for settlement items
type SettlementItemRepository interface {
	GetByID(ctx context.Context, id string) (*SettlementItem, error)
	GetBySettlementBatchID(ctx context.Context, batchID string) ([]*SettlementItem, error)
	GetByProductTransactionID(ctx context.Context, productTxID string) ([]*SettlementItem, error)
	GetUnmatchedByBatchID(ctx context.Context, batchID string) ([]*SettlementItem, error)
	Save(ctx context.Context, item *SettlementItem) error
	SaveBatch(ctx context.Context, items []*SettlementItem) error
}

// NewSettlementItem creates an unmatched settlement item from a settled gateway record.
func NewSettlementItem(settlementBatchID string, settled SettledTransaction) (*SettlementItem, error) {
	if settlementBatchID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "settlement_batch_uuid is required", nil)
	}
	if settled.GrossAmount < 0 || settled.Fee < 0 || settled.NetAmount < 0 {
		return nil, ledgererr.ErrInvalidSettlementItem
	}

	si := &SettlementItem{
		SettlementBatchUUID:    settlementBatchID,
		ProductTransactionUUID: "", // Will be set when matched
		InvoiceNumber:          settled.MerchantReference,
		GatewayAccountID:       settled.GatewayAccountID,
		GatewayTransactionID:   settled.GatewayTransactionID,
		PaymentChannel:         settled.PaymentChannel,
		TransactionAmount:      settled.GrossAmount,
		PayToMerchant:          settled.NetAmount,
		AllocatedFee:           settled.Fee,
		FeeReported:            settled.FeeReported,
		RawGatewayData:         settled.Raw,
		IsMatched:              false,
	}
	redifu.InitRecord(si)
	return si, nil
}

// MatchToTransaction links this item to a product transaction and reconciles amounts.
//
// Both fee models expect the same net: the gateway keeps its fee and credits the rest, so
// what reaches the sub-account is the seller's share plus the platform's. The two models
// differ in who bore the fee when the price was set, not in what settles.
func (si *SettlementItem) MatchToTransaction(productTx *ProductTransaction) error {
	if productTx == nil {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "product_transaction is required", nil)
	}
	if productTx.Record.UUID == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "product_transaction_uuid is required", nil)
	}

	si.ProductTransactionUUID = productTx.Record.UUID
	si.SellerAccountID = productTx.SellerAccountID // Cache for efficient grouping
	si.IsMatched = true

	si.ExpectedNetAmount = productTx.Fee.SellerNetAmount + productTx.Fee.PlatformFee
	si.AmountDiscrepancy = si.PayToMerchant - si.ExpectedNetAmount

	return nil
}

// HasAmountDiscrepancy returns true if the settled net doesn't match the expected amount
func (si *SettlementItem) HasAmountDiscrepancy() bool {
	return si.IsMatched && si.AmountDiscrepancy != 0
}

// GetNetAmount returns the amount credited to the sub-account, after the channel fee.
func (si *SettlementItem) GetNetAmount() int64 {
	return si.PayToMerchant
}

// GetSellerAndPlatformAmount returns what seller + platform should receive.
// This is the settled net (total_charged - gateway_fee), which equals
// seller_price + platform_fee.
func (si *SettlementItem) GetSellerAndPlatformAmount() int64 {
	return si.PayToMerchant
}
