package singapay

import (
	"context"
	"net/http"
	"strconv"
)

// QRISTransaction is a dynamic QRIS charge.
//
// Its fee breakdown is the most detailed in the API: MDRPercentage is the *rate* itself,
// not just the amount charged. That makes QRIS the one channel where a fee table can be
// verified against what Singapay actually applied, rather than trusted from a rate card.
type QRISTransaction struct {
	ID             int64  `json:"id"`
	ReffNo         string `json:"reff_no"`
	MerchantReffNo string `json:"merchant_reff_no"`

	Status         PaymentStatus `json:"status"`
	StatusComputed PaymentStatus `json:"status_computed"`
	IsExpired      bool          `json:"is_expired"`

	// QRData is the payload to render as a QR code for the payer.
	QRData string `json:"qr_data"`
	Kind   string `json:"kind"`
	Type   string `json:"type"`

	Amount      Amount `json:"amount"`
	TotalAmount Amount `json:"total_amount"`

	MDRPercentage float64 `json:"mdr_percentage"`
	MDRCost       Amount  `json:"mdr_cost"`
	OurMargin     Amount  `json:"our_margin"`
	VendorFee     Amount  `json:"vendor_fee"`
	// SettledToMerchant is what actually reaches the balance.
	SettledToMerchant Amount `json:"settled_to_merchant_amount"`

	AcquirerName string `json:"acquirer_name"`
	IssuerName   string `json:"issuer_name"`
	CustomerPAN  string `json:"customer_pan"`
	CustomerName string `json:"customer_name"`

	HasReconcile bool `json:"has_reconcile"`

	HasSettleToMerchant bool    `json:"had_settled_to_merchant"`
	SettledToMerchantAt ISOTime `json:"settled_to_merchant_at"`
	HasSettle           bool    `json:"has_settle"`
	SettleAt            ISOTime `json:"settle_at"`

	CreatedAt ISOTime `json:"created_at"`
	ExpiredAt ISOTime `json:"expired_at"`
}

// TotalFee returns everything Singapay deducted from this charge.
func (t *QRISTransaction) TotalFee() Amount {
	return Amount{
		minor:    t.MDRCost.Minor() + t.VendorFee.Minor() + t.OurMargin.Minor(),
		Currency: t.Amount.Currency,
		Set:      t.Amount.Set,
	}
}

// GenerateQRISRequest is the body of POST /api/v1.0/qris-dynamic/{account_id}/generate-qr.
type GenerateQRISRequest struct {
	// Amount is in whole rupiah.
	Amount int64 `json:"amount"`
	// ExpiredAt is ISO 8601 and optional.
	ExpiredAt string `json:"expired_at,omitempty"`
	// MerchantReffNo is the reconciliation key. Always set it.
	MerchantReffNo string `json:"merchant_reff_no,omitempty"`
}

// GenerateQRIS creates a dynamic QRIS charge for a fixed amount.
//
// The response carries QRData to render for the payer, and — once paid — the transaction
// record carries the full MDR breakdown.
func (c *Client) GenerateQRIS(ctx context.Context, accountID string, req GenerateQRISRequest) (*QRISTransaction, error) {
	var out QRISTransaction
	path := "/api/v1.0/qris-dynamic/" + accountID + "/generate-qr"
	if err := c.call(ctx, http.MethodPost, path, req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetQRISTransaction reads one QRIS charge by its numeric id.
func (c *Client) GetQRISTransaction(ctx context.Context, accountID string, id int64) (*QRISTransaction, error) {
	var out QRISTransaction
	path := "/api/v1.0/qris-dynamic/" + accountID + "/show/" + strconv.FormatInt(id, 10)
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListQRISTransactions returns QRIS charges for an account.
func (c *Client) ListQRISTransactions(ctx context.Context, accountID string, w SettlementWindow) ([]QRISTransaction, Pagination, error) {
	var out []QRISTransaction
	var page Pagination
	path := withQuery("/api/v1.0/qris-dynamic/"+accountID, w.values())
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}
