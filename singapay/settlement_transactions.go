package singapay

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SettlementTransaction is one money-in transaction inside a settlement, as
// GET /api/v1.0/settlement-transactions/{identifier} lists it.
//
// It is what the settlement webhook leaves out. A settlement.completed delivery names the
// batch and its totals and nothing the batch covered; this listing is the batch's contents,
// read back from Singapay when someone asks. Each row mirrors the money-in transaction
// shape the VA list uses, less what only one product has: there is no status, and no
// product type — the type exists only as a filter
// ([ListSettlementTransactionsOptions.TransactionType]).
type SettlementTransaction struct {
	// TransactionID is Singapay's business id for the payment: a VA payment's
	// transaction_id ("VA-20251024-0001H9X8ZK"), every other product's reff_no.
	TransactionID string `json:"transaction_id"`
	// MerchantReffNo is the reference the merchant created the payment with. This ledger
	// sends the invoice number as merchant_reff_no on VA, QRIS and e-wallet; a payment
	// link is created with it as reff_no instead.
	MerchantReffNo string `json:"merchant_reff_no"`

	// Account is the sub-account the payment was made to, nil when Singapay sent null.
	Account *SettlementTransactionAccount `json:"account"`

	Fees struct {
		// Name is the payment method as Singapay names it ("BRI Virtual Account").
		Name string `json:"name"`
		// Amount is the merchant fee; for QRIS, vendor_fee + our_margin. Unset (Set
		// false) when Singapay sent null, which is not the same as a zero fee.
		Amount   Amount `json:"amount"`
		Currency string `json:"currency"`
	} `json:"fees"`

	// Amount is what the payment was for, two decimals ({"value":"1234.56",...}).
	Amount Amount `json:"amount"`

	PostedAt MillisTime `json:"post_timestamp"`
	// ProcessedAt is when the payment was paid; Singapay sends none for QRIS.
	ProcessedAt MillisTime `json:"processed_timestamp"`

	HasSettle bool       `json:"has_settle"`
	SettleAt  MillisTime `json:"settle_at"`
}

// SettlementTransactionAccount names the sub-account a settled payment belongs to.
type SettlementTransactionAccount struct {
	// ID is the account ULID — the id the ledger stores as singapay_account_id.
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Settlement transaction types, the values [ListSettlementTransactionsOptions] filters on.
const (
	SettlementTransactionTypeVA          = "VA"
	SettlementTransactionTypeQRIS        = "QRIS"
	SettlementTransactionTypePaymentLink = "PAYMENT_LINK"
	SettlementTransactionTypeEwallet     = "EWALLET"
)

// ListSettlementTransactionsOptions pages and filters [Client.ListSettlementTransactions].
// The zero value asks for Singapay's first page at its default size (25), unfiltered.
type ListSettlementTransactionsOptions struct {
	// Page starts at 1; 0 leaves it to Singapay.
	Page int
	// PerPage is the page size; 0 leaves it to Singapay. No maximum is documented.
	PerPage int
	// TransactionType is VA, QRIS, PAYMENT_LINK or EWALLET; empty lists every type.
	TransactionType string
	// Refunded, when set, filters on the settlement-level refund state of each row.
	Refunded *bool
}

func (o ListSettlementTransactionsOptions) values() url.Values {
	q := url.Values{}
	if o.Page > 0 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	if o.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(o.PerPage))
	}
	if o.TransactionType != "" {
		q.Set("transaction_type", o.TransactionType)
	}
	if o.Refunded != nil {
		q.Set("is_refunded", strconv.FormatBool(*o.Refunded))
	}
	return q
}

// ListSettlementTransactions lists the transactions a settlement covered, one page at a
// time.
//
// identifier is the settlement's numeric id (the settlement webhook's settlement.id) or its
// reference_no; Singapay accepts either. It answers 404 — an [*Error] — for a settlement
// that is not this merchant's, and for one whose status is not completed.
//
// Nothing here is booked. The settling pass never selects on a batch's contents (see the
// ledger's settlement.go); this is for reading what a batch was, after the fact.
func (c *Client) ListSettlementTransactions(ctx context.Context, identifier string, opts ListSettlementTransactionsOptions) ([]SettlementTransaction, Pagination, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		// An empty identifier would request the collection itself, which is not this call.
		return nil, Pagination{}, fmt.Errorf("singapay: a settlement id or reference is required")
	}

	path := withQuery("/api/v1.0/settlement-transactions/"+url.PathEscape(identifier), opts.values())

	var out []SettlementTransaction
	var page Pagination
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}
