package singapay

import (
	"context"
	"net/http"
	"strconv"
)

// VABank is a bank that can issue a virtual account.
type VABank string

const (
	BankBCA      VABank = "BCA"
	BankBNI      VABank = "BNI"
	BankBRI      VABank = "BRI"
	BankMandiri  VABank = "MANDIRI"
	BankPermata  VABank = "PERMATA"
	BankMaybank  VABank = "MAYBANK"
	BankCIMB     VABank = "CIMB"
	BankBSI      VABank = "BSI"
	BankMuamalat VABank = "MUAMALAT"
	BankBNC      VABank = "BNC"
	BankOCBC     VABank = "OCBC"
	BankDanamon  VABank = "DANAMON"
)

// VAAmountType selects whether the payer must pay an exact amount.
type VAAmountType string

const (
	// VAClosed fixes the amount — the right choice for an invoice.
	VAClosed VAAmountType = "closed"
	// VAOpen accepts anything within a min/max range.
	VAOpen VAAmountType = "open"
)

// VAKind selects whether the account expires.
type VAKind string

const (
	// VATemporary expires and has a usage cap. Both are required for this kind.
	VATemporary VAKind = "temporary"
	// VAPermanent persists — a customer-scoped account rather than an invoice.
	VAPermanent VAKind = "permanent"
)

// VirtualAccount is an issued virtual account number.
type VirtualAccount struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Name   string `json:"name"`
	// MerchantReffNo is the reconciliation key, echoed on transactions and webhooks.
	MerchantReffNo string `json:"merchant_reff_no"`
	// Code is the channel code, e.g. "VA_BNI" — the same spelling the payment-method
	// catalogue and a fee table use.
	Code string `json:"code"`

	Bank struct {
		ShortName string `json:"short_name"`
		Number    string `json:"number"`
		SwiftCode string `json:"swift_code"`
	} `json:"bank"`

	Amount     Amount       `json:"amount"`
	MinAmount  Amount       `json:"min_amount"`
	MaxAmount  Amount       `json:"max_amount"`
	AmountType VAAmountType `json:"amount_type"`

	Status       string     `json:"status"`
	Kind         VAKind     `json:"kind"`
	CurrentUsage int        `json:"current_usage"`
	ExpiredAt    MillisTime `json:"expired_at"`
}

// CreateVirtualAccountRequest is the body of POST /api/v1.0/virtual-accounts/{account_id}.
type CreateVirtualAccountRequest struct {
	BankCode VABank `json:"bank_code"`
	Kind     VAKind `json:"kind"`
	// AmountType defaults to closed.
	AmountType VAAmountType `json:"amount_type,omitempty"`

	Name string `json:"name,omitempty"`
	// MerchantReffNo is the reconciliation key. Always set it — it is what links the
	// eventual payment back to an invoice.
	MerchantReffNo string `json:"merchant_reff_no,omitempty"`

	// ExpiredAt is Unix milliseconds as a 13-digit string. Required for a temporary
	// account. Note the encoding differs from a payment link, which takes ISO 8601.
	ExpiredAt string `json:"expired_at,omitempty"`
	// MaxUsage is required for a temporary account; 1 for a single invoice.
	MaxUsage int `json:"max_usage,omitempty"`

	// Amount is required for a closed account, in whole rupiah.
	Amount int64 `json:"amount,omitempty"`
	// MinAmount and MaxAmount are required for an open account.
	MinAmount int64 `json:"min_amount,omitempty"`
	MaxAmount int64 `json:"max_amount,omitempty"`
}

// CreateVirtualAccount issues a VA number for a sub-account.
//
// For a single invoice this means kind=temporary, amount_type=closed, max_usage=1 and an
// expiry. Unlike a payment link, the response carries the number to show the payer
// immediately, and the eventual payment reports its own channel fee — which is what makes
// fee reconciliation possible.
func (c *Client) CreateVirtualAccount(ctx context.Context, accountID string, req CreateVirtualAccountRequest) (*VirtualAccount, error) {
	if req.AmountType == "" {
		req.AmountType = VAClosed
	}
	var out VirtualAccount
	if err := c.call(ctx, http.MethodPost, "/api/v1.0/virtual-accounts/"+accountID, req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetVirtualAccount reads one VA by its ULID.
func (c *Client) GetVirtualAccount(ctx context.Context, accountID, virtualAccountID string) (*VirtualAccount, error) {
	var out VirtualAccount
	path := "/api/v1.0/virtual-accounts/" + accountID + "/" + virtualAccountID
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVirtualAccount removes a VA. Singapay refuses if it has transactions.
func (c *Client) DeleteVirtualAccount(ctx context.Context, accountID, virtualAccountID string) error {
	path := "/api/v1.0/virtual-accounts/" + accountID + "/" + virtualAccountID
	return c.call(ctx, http.MethodDelete, path, nil, nil, false)
}

// VATransaction is a payment received into a virtual account.
//
// Fees is the actual channel fee for this payment — the field a fee-mismatch
// reconciliation needs, and the reason a VA beats a payment link for that purpose.
type VATransaction struct {
	TransactionID  string `json:"transaction_id"`
	MerchantReffNo string `json:"merchant_reff_no"`
	VANumber       string `json:"va_number"`

	Account struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	} `json:"account"`

	Bank ChannelBank `json:"bank"`

	Amount Amount        `json:"amount"`
	Status PaymentStatus `json:"status"`
	Notes  string        `json:"notes"`

	Fees struct {
		Name     string `json:"name"`
		Amount   Amount `json:"amount"`
		Currency string `json:"currency"`
	} `json:"fees"`

	HasSettle bool       `json:"has_settle"`
	SettleAt  MillisTime `json:"settle_at"`

	PostedAt    MillisTime `json:"post_timestamp"`
	ProcessedAt MillisTime `json:"processed_timestamp"`
}

// NetAmount returns what the merchant keeps: the amount paid less the channel fee.
func (t *VATransaction) NetAmount() Amount {
	return Amount{
		minor:    t.Amount.Minor() - t.Fees.Amount.Minor(),
		Currency: t.Amount.Currency,
		Set:      t.Amount.Set,
	}
}

// ListVATransactions returns VA payments for an account. Filter on the settlement window
// to reconstruct what a settlement batch covered.
func (c *Client) ListVATransactions(ctx context.Context, accountID string, w SettlementWindow) ([]VATransaction, Pagination, error) {
	var out []VATransaction
	var page Pagination
	path := withQuery("/api/v1.0/va-transactions/"+accountID, w.values())
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}

// GetVATransaction reads one VA payment by its business transaction id.
func (c *Client) GetVATransaction(ctx context.Context, accountID, transactionID string) (*VATransaction, error) {
	var out VATransaction
	path := "/api/v1.0/va-transactions/" + accountID + "/" + transactionID
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetVATransactionsByVANumber reads the payments made into one virtual account.
//
// This is the VA route into the per-transaction settlement read, and it exists because
// GetVATransaction cannot be reached from what a payment records. Creating a VA returns
// the VA's own ULID; the transaction that later arrives in it carries a different,
// business identifier ("VA-20251024-0001H9X8ZK"), and only the second one opens
// GetVATransaction. The VA number, by contrast, is known at creation and stored.
//
// Singapay answers with a collection because a virtual account can in general be paid into
// more than once. The ones this ledger issues cannot: they are created temporary, closed
// and MaxUsage 1, so exactly one transaction can exist per VA and the caller can take the
// single element. A second element would mean the VA was not issued by this code path,
// which is worth noticing rather than silently taking the first.
func (c *Client) GetVATransactionsByVANumber(ctx context.Context, accountID, vaNumber string) ([]VATransaction, Pagination, error) {
	var out []VATransaction
	var page Pagination
	path := "/api/v1.0/va-transactions/" + accountID + "/detail-by-va-number/" + vaNumber
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}

// MillisTimestamp renders a time as the 13-digit millisecond string Singapay expects in
// virtual-account request bodies.
func MillisTimestamp(t interface{ UnixMilli() int64 }) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}
