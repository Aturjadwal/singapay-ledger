package singapay

import (
	"context"
	"net/http"
	"strconv"
)

// EwalletTransaction is a native e-wallet checkout.
type EwalletTransaction struct {
	ID             int64  `json:"id"`
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
