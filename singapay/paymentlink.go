package singapay

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// PaymentMethod is one channel from the payment-link catalogue.
type PaymentMethod struct {
	// Code is what goes in WhitelistedPaymentMethod, e.g. "VA_BRI", "QRIS".
	Code  string `json:"code"`
	Name  string `json:"name"`
	Group string `json:"group"`
	Desc  string `json:"desc"`
}

// ListPaymentMethods returns the active channel catalogue.
//
// These codes are the source of truth for a fee table: they replace DOKU's channel
// constants wholesale, and nothing else in the API accepts DOKU's spellings. The
// catalogue carries no rates — Singapay treats those as commercial terms, readable
// per transaction after the fact but never quoted up front for money-in.
func (c *Client) ListPaymentMethods(ctx context.Context) ([]PaymentMethod, error) {
	var out struct {
		PaymentMethods []PaymentMethod `json:"payment_methods"`
		AvailableCodes []string        `json:"available_codes"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1.0/payment-link-manage/payment-methods", nil, &out, false); err != nil {
		return nil, err
	}
	return out.PaymentMethods, nil
}

// PaymentLinkType selects how a link's amount is expressed.
type PaymentLinkType string

const (
	// PaymentLinkTotal takes a single total amount and ignores items.
	PaymentLinkTotal PaymentLinkType = "total"
	// PaymentLinkItems computes the total from line items.
	PaymentLinkItems PaymentLinkType = "items"
)

// PaymentLinkItem is one line on an itemised link.
type PaymentLinkItem struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	// UnitPrice may be negative, which is how a discount line is expressed.
	UnitPrice int64 `json:"unit_price"`
}

// PaymentLink is a hosted checkout page that can accept one or many payments.
type PaymentLink struct {
	ID         int64  `json:"id"`
	ReffNo     string `json:"reff_no"`
	Title      string `json:"title"`
	Descriptin string `json:"description"`
	Source     string `json:"source"`
	// PaymentURL is the page to send the payer to.
	PaymentURL string `json:"payment_url"`

	Status         string `json:"status"`
	StatusComputed string `json:"status_computed"`
	IsExpired      bool   `json:"is_expired"`

	MaxUsage     int    `json:"max_usage"`
	CurrentUsage int    `json:"current_usage"`
	TotalAmount  Amount `json:"total_amount"`

	// CustomerPaysFee reports who bears the channel fee. It is read-only: the create
	// and update bodies do not accept it, and links made through v2 are always false.
	// Charging the fee to the customer therefore has to be done by grossing up
	// TotalAmount before sending it, which is what the ledger's fee calculator
	// already does.
	CustomerPaysFee bool `json:"customer_pays_fee"`

	WhitelistedPaymentMethod []string `json:"whitelisted_payment_method"`

	SuccessRedirectURL string `json:"success_redirect_url"`
	ExpiredRedirectURL string `json:"expired_redirect_url"`

	CustomerName  string `json:"customer_name"`
	CustomerEmail string `json:"customer_email"`
	CustomerPhone string `json:"customer_phone"`

	OptionalMetadata map[string]any `json:"optional_metadata"`

	ExpiredAt   ISOTime `json:"expired_at"`
	PaymentDate ISOTime `json:"payment_date"`
	CreatedAt   ISOTime `json:"created_at"`
	UpdatedAt   ISOTime `json:"updated_at"`
}

// CreatePaymentLinkRequest is the body of POST /api/v2.0/payment-link/{account_id}.
type CreatePaymentLinkRequest struct {
	// ReffNo is the merchant reference. It is the reconciliation key: it comes back on
	// the webhook (nested on the payment link, not on the transaction) and on every
	// history row.
	ReffNo      string `json:"reff_no"`
	Description string `json:"description,omitempty"`

	Type PaymentLinkType `json:"payment_link_type"`
	// TotalAmount is required when Type is total, in whole rupiah.
	TotalAmount int64 `json:"total_amount,omitempty"`
	// Items is required when Type is items.
	Items []PaymentLinkItem `json:"items,omitempty"`

	// MaxUsage defaults to 1 (single use). 0 means unlimited.
	MaxUsage *int `json:"max_usage,omitempty"`

	// ExpiredAt is an absolute timestamp, unlike DOKU's payment_due_date which is a
	// number of minutes. Compute it from the desired lifetime before calling.
	ExpiredAt string `json:"expired_at,omitempty"`

	// WhitelistedPaymentMethod restricts the channels offered. One code pins the link
	// to a single channel; empty lets the payer choose from everything active.
	WhitelistedPaymentMethod []string `json:"whitelisted_payment_method,omitempty"`

	SuccessRedirectURL string `json:"success_redirect_url,omitempty"`
	ExpiredRedirectURL string `json:"expired_redirect_url,omitempty"`

	// CustomerName, CustomerEmail and CustomerPhone pre-fill a known payer. They are
	// only accepted when MaxUsage resolves to 1; sending them on a multi-use link is
	// a 422.
	CustomerName  string `json:"customer_name,omitempty"`
	CustomerEmail string `json:"customer_email,omitempty"`
	CustomerPhone string `json:"customer_phone,omitempty"`

	OptionalMetadata map[string]any `json:"optional_metadata,omitempty"`
}

// CreatePaymentLink creates a hosted checkout page for a sub-account.
//
// This is the closest analogue to DOKU's /checkout/v1/payment: the payer picks a channel
// on Singapay's page. The cost is reconciliation — a payment link exposes no
// per-transaction fee anywhere, so [Client.CreateVirtualAccount] or
// [Client.GenerateQRIS] are the better choice whenever the channel is known up front.
func (c *Client) CreatePaymentLink(ctx context.Context, accountID string, req CreatePaymentLinkRequest) (*PaymentLink, error) {
	if req.Type == "" {
		req.Type = PaymentLinkTotal
	}
	var out PaymentLink
	if err := c.call(ctx, http.MethodPost, "/api/v2.0/payment-link/"+accountID, req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPaymentLink reads a link by its numeric id.
func (c *Client) GetPaymentLink(ctx context.Context, linkID int64) (*PaymentLink, error) {
	var out PaymentLink
	path := "/api/v2.0/payment-link/" + strconv.FormatInt(linkID, 10)
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePaymentLink removes a link. Singapay refuses if it already has payment history,
// and there is no v2 counterpart for this.
func (c *Client) DeletePaymentLink(ctx context.Context, accountID string, linkID int64) error {
	path := "/api/v1.0/payment-link-manage/" + accountID + "/" + strconv.FormatInt(linkID, 10)
	return c.call(ctx, http.MethodDelete, path, nil, nil, false)
}

// PaymentLinkHistory is one payment attempt against a link.
//
// It carries no fee and no net amount — the one money-in record in the API that does not.
// Anything that needs the actual channel fee has to come from VA, QRIS or e-wallet
// records instead.
type PaymentLinkHistory struct {
	ID                      int64         `json:"id"`
	ReffNo                  string        `json:"reff_no"`
	PaymentLinkReffNo       string        `json:"payment_link_reff_no"`
	PaymentMethodName       string        `json:"payment_method_name"`
	PaymentMethodValue      string        `json:"payment_method_value"`
	PaymentMethodAdditional string        `json:"payment_method_additional"`
	Amount                  Amount        `json:"amount"`
	BalanceAfter            Amount        `json:"balance_after"`
	Status                  PaymentStatus `json:"status"`
	StatusComputed          PaymentStatus `json:"status_computed"`
	IsExpired               bool          `json:"is_expired"`

	CustomerName  string `json:"customer_name"`
	CustomerEmail string `json:"customer_email"`
	CustomerPhone string `json:"customer_phone"`

	HasSettle bool    `json:"has_settle"`
	SettleAt  ISOTime `json:"settle_at"`

	OptionalMetadata map[string]any `json:"optional_metadata"`

	PaymentDate ISOTime `json:"payment_date"`
	ExpiredAt   ISOTime `json:"expired_at"`
	CreatedAt   ISOTime `json:"created_at"`
}

// SettlementWindow filters transaction lists by when funds were settled to the merchant.
//
// This is the closest thing to DOKU's settlement CSV: given a batch's date range from a
// settlement webhook, these filters return the rows that batch covered.
type SettlementWindow struct {
	// SettleFrom and SettleTo are ISO 8601 and inclusive.
	SettleFrom string
	SettleTo   string
	// Settled, when set, filters on whether funds have settled at all.
	Settled *bool
	// ReffNo matches partially.
	ReffNo  string
	Status  string
	Page    int
	PerPage int
}

func (w SettlementWindow) values() url.Values {
	q := url.Values{}
	if w.SettleFrom != "" {
		q.Set("settle_at_from", w.SettleFrom)
	}
	if w.SettleTo != "" {
		q.Set("settle_at_to", w.SettleTo)
	}
	if w.Settled != nil {
		q.Set("has_settle", strconv.FormatBool(*w.Settled))
	}
	if w.ReffNo != "" {
		q.Set("reff_no", w.ReffNo)
	}
	if w.Status != "" {
		q.Set("status", w.Status)
	}
	if w.Page > 0 {
		q.Set("page", strconv.Itoa(w.Page))
	}
	if w.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(w.PerPage))
	}
	return q
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// ListPaymentLinkHistories returns payment attempts for an account, newest first.
func (c *Client) ListPaymentLinkHistories(ctx context.Context, accountID string, w SettlementWindow) ([]PaymentLinkHistory, Pagination, error) {
	var out []PaymentLinkHistory
	var page Pagination
	path := withQuery("/api/v1.0/payment-link-histories/"+accountID, w.values())
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}
