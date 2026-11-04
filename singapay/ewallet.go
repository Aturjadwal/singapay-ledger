package singapay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// EwalletTransaction is a native e-wallet checkout. It is decoded by its own UnmarshalJSON,
// which reads three fields more widely than their Go types suggest.
type EwalletTransaction struct {
	ID int64 `json:"id"`
	// AccountID is the sub-account as the transaction names it: Singapay's own numeric id
	// for it ("35"), not the ULID the order was created with. It arrives as a JSON number
	// and is kept as its digits. Nothing in this module acts on it.
	AccountID      string `json:"account_id"`
	ReffNo         string `json:"reff_no"`
	MerchantReffNo string `json:"merchant_reff_no"`

	Status         PaymentStatus `json:"status"`
	StatusComputed PaymentStatus `json:"status_computed"`
	IsExpired      bool          `json:"is_expired"`

	Vendor string `json:"ewallet_vendor"`

	// Amount, TotalAmount and NetAmount are all present and not interchangeable:
	// MerchantFee is deducted from what the payer was charged. Confirm with Singapay
	// which of Amount and TotalAmount is the gross before booking either — the
	// webhook sample shows amount 95000 against total_amount 100000.
	Amount      Amount `json:"amount"`
	TotalAmount Amount `json:"total_amount"`
	MerchantFee Amount `json:"merchant_fee"`
	NetAmount   Amount `json:"net_amount"`

	// CheckoutURL is where the payer completes the payment; CheckoutURLApp is the
	// deep link for the vendor's own app.
	CheckoutURL    string `json:"checkout_url"`
	CheckoutURLApp string `json:"checkout_url_app"`

	VendorReferenceNo   string `json:"vendor_reference_no"`
	MerchantRedirectURL string `json:"merchant_redirect_url"`

	CustomerName  string `json:"customer_name"`
	CustomerEmail string `json:"customer_email"`
	CustomerPhone string `json:"customer_phone"`

	PaymentChannel string `json:"payment_channel"`
	VendorCode     string `json:"payment_vendor_code"`

	HasSettle bool    `json:"has_settle"`
	SettleAt  ISOTime `json:"settle_at"`

	ExpiredAt ISOTime `json:"expired_at"`
	CreatedAt ISOTime `json:"created_at"`
}

// UnmarshalJSON reads an e-wallet transaction the way Singapay actually writes it.
//
// account_id is a JSON number, as on the payment-link webhook, even though the order was
// created with the ULID. When it was read as a string, the first create-order answered in
// production (2026-11-04) failed to decode after Singapay had opened the checkout, and every
// e-wallet checkout failed the same way. It is now read as its digits, or as a string if a
// string arrives. Since nothing acts on it, no value of it can fail a read.
//
// is_expired and has_settle are booleans that Singapay's PHP service may print as 1 and 0, as
// its settlement listing does. true, false, 1 and 0 are read, quoted or not, and null is false.
// Anything else is refused, because has_settle decides whether a payment has settled.
//
// Every other field is read exactly as its type reads it.
func (t *EwalletTransaction) UnmarshalJSON(b []byte) error {
	// plain has the fields without this method; the three raw fields below sit shallower
	// than plain's own, so they take those keys.
	type plain EwalletTransaction
	var v struct {
		plain
		AccountID json.RawMessage `json:"account_id"`
		IsExpired json.RawMessage `json:"is_expired"`
		HasSettle json.RawMessage `json:"has_settle"`
	}
	// Errors name the type: through plain, a mistyped field reads as ".plain.id".
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("e-wallet transaction: %w", err)
	}
	isExpired, err := readFlag("is_expired", v.IsExpired)
	if err != nil {
		return fmt.Errorf("e-wallet transaction: %w", err)
	}
	hasSettle, err := readFlag("has_settle", v.HasSettle)
	if err != nil {
		return fmt.Errorf("e-wallet transaction: %w", err)
	}

	*t = EwalletTransaction(v.plain)
	t.AccountID = looseString(v.AccountID)
	t.IsExpired = isExpired
	t.HasSettle = hasSettle
	return nil
}

// readFlag reads a JSON boolean that may also be printed as 1 or 0, with or without quotes.
// An absent field, null and "" are false. Anything else is an error that names the field.
func readFlag(field string, raw json.RawMessage) (bool, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return false, nil
	}
	value := text
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	switch strings.ToLower(value) {
	case "true", "1":
		return true, nil
	case "false", "0", "":
		return false, nil
	}
	return false, fmt.Errorf("%s is not a boolean: %s", field, text)
}

// CreateEwalletOrderRequest is the body of POST /api/v2.0/ewallet-native/create-order.
type CreateEwalletOrderRequest struct {
	// AccountID is the sub-account ULID. On v2 it lives in the body, not the path.
	AccountID string `json:"account_id"`
	// Amount is in whole rupiah.
	Amount int64 `json:"amount"`
	// Vendor is the wallet code, e.g. "EWALLET_DANA". Take it from
	// [Client.ListPaymentMethods] rather than hardcoding.
	Vendor string `json:"ewallet_vendor"`

	// ExpiredAt is ISO 8601 and optional.
	ExpiredAt string `json:"expired_at,omitempty"`

	CustomerName  string `json:"customer_name,omitempty"`
	CustomerEmail string `json:"customer_email,omitempty"`
	// CustomerPhone is required by some vendors — OVO push-to-pay among them.
	CustomerPhone string `json:"customer_phone,omitempty"`

	MerchantRedirectURL string `json:"merchant_redirect_url,omitempty"`
	// MerchantReffNo is the reconciliation key. Always set it.
	MerchantReffNo string `json:"merchant_reff_no,omitempty"`
}

// CreateEwalletOrder opens a native e-wallet checkout.
func (c *Client) CreateEwalletOrder(ctx context.Context, req CreateEwalletOrderRequest) (*EwalletTransaction, error) {
	var out EwalletTransaction
	if err := c.call(ctx, http.MethodPost, "/api/v2.0/ewallet-native/create-order", req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEwalletTransaction reads one e-wallet checkout by its business transaction id.
func (c *Client) GetEwalletTransaction(ctx context.Context, accountID, transactionID string) (*EwalletTransaction, error) {
	var out EwalletTransaction
	path := "/api/v1.0/ewallet-native-transactions/" + accountID + "/" + transactionID
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListEwalletTransactions returns e-wallet checkouts for an account.
func (c *Client) ListEwalletTransactions(ctx context.Context, accountID string, w SettlementWindow) ([]EwalletTransaction, Pagination, error) {
	var out []EwalletTransaction
	var page Pagination
	path := withQuery("/api/v1.0/ewallet-native-transactions/"+accountID, w.values())
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}

// InquiryEwalletStatus refreshes an e-wallet checkout's status from the vendor.
func (c *Client) InquiryEwalletStatus(ctx context.Context, accountID string, id int64) (*EwalletTransaction, error) {
	var out EwalletTransaction
	path := "/api/v1.0/ewallet-native/" + accountID + "/inquiry-status/" + strconv.FormatInt(id, 10)
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}
