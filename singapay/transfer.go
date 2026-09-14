package singapay

import (
	"context"
	"net/http"
	"net/url"
)

// TransferParty is one side of an account transfer.
type TransferParty struct {
	AccountID     string `json:"account_id"`
	AccountName   string `json:"account_name"`
	AccountNumber string `json:"account_number"`
	BalanceAfter  Amount `json:"balance_after"`
}

// AccountTransfer is a completed movement between two sub-accounts of one merchant.
type AccountTransfer struct {
	TransactionID string `json:"transaction_id"`
	// MerchantRefNo echoes the idempotency reference. When none was sent, Singapay
	// generated one (AM-<timestamp>-<random>) and returns it here.
	MerchantRefNo string `json:"merchant_ref_no"`
	// Status is always "success" — the transfer is synchronous and a failure is an
	// error, not a pending record.
	Status      string        `json:"status"`
	Amount      Amount        `json:"amount"`
	Remitter    TransferParty `json:"remitter"`
	Beneficiary TransferParty `json:"beneficiary"`

	ProcessedAt MillisTime `json:"processed_timestamp"`
	CreatedAt   MillisTime `json:"created_at"`
}

// TransferRequest is the body of POST /api/v1.0/account-transfer/{account_id}/transfer.
type TransferRequest struct {
	// Amount may carry decimals, and needs to.
	//
	// Singapay types this field as a number with decimals and echoes it back as a string
	// "to preserve decimal precision", so a transfer is the one outbound path in this
	// client that is not whole rupiah. That matters because this endpoint is how the
	// platform sub-account balances the gateway fee: the difference between the fee quoted
	// at checkout and the one Singapay actually took is usually a fraction of a rupiah, and
	// an int64 here would silently drop it — leaving that fraction stranded in the seller's
	// sub-account, which is precisely the divergence the balancing exists to prevent.
	//
	// Amount marshals to two decimals without going through float64, so 4000.16 is sent as
	// 4000.16 rather than 4000.1599999999999.
	//
	// Singapay rejects anything below 1: a transfer must be at least one rupiah, so a
	// balanced platform fee that lands under Rp1 cannot be swept and must not be attempted.
	Amount Amount `json:"amount"`

	// BeneficiaryAccountNumber is the destination's 12-digit account *number* — not
	// its ULID. This is the asymmetry to watch: the remitter is named by ULID in the
	// path, the beneficiary by number in the body.
	BeneficiaryAccountNumber string `json:"beneficiary_account_number"`

	// MerchantRefNo is the idempotency key, unique per merchant across every account
	// movement. Re-sending a used value returns the original transfer with HTTP 200
	// and moves nothing further — which makes a deterministic reference the whole
	// safety mechanism, not merely a label.
	//
	// Leaving it empty makes Singapay generate one, which forfeits that protection.
	MerchantRefNo string `json:"merchant_ref_no,omitempty"`
}

// TransferBetweenAccounts moves funds from one sub-account to another within the same
// merchant. Both accounts must be active, and the request is signed.
//
// It is synchronous: HTTP 200 means the money has moved.
func (c *Client) TransferBetweenAccounts(ctx context.Context, fromAccountID string, req TransferRequest) (*AccountTransfer, error) {
	var out AccountTransfer
	path := "/api/v1.0/account-transfer/" + fromAccountID + "/transfer"
	if err := c.call(ctx, http.MethodPost, path, req, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccountTransfer reads one transfer. accountID must be either side of it.
func (c *Client) GetAccountTransfer(ctx context.Context, accountID, transactionID string) (*AccountTransfer, error) {
	var out AccountTransfer
	path := "/api/v1.0/account-transfer/" + accountID + "/" + transactionID
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAccountTransfersOptions filters [Client.ListAccountTransfers].
type ListAccountTransfersOptions struct {
	// TransactionID matches partially.
	TransactionID string
	// Status is one of success, pending, failed.
	Status string
}

// ListAccountTransfers returns transfers where the account is remitter or beneficiary,
// limited by Singapay to the last year.
func (c *Client) ListAccountTransfers(ctx context.Context, accountID string, opts ListAccountTransfersOptions) ([]AccountTransfer, Pagination, error) {
	q := url.Values{}
	if opts.TransactionID != "" {
		q.Set("transaction_id", opts.TransactionID)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}

	path := "/api/v1.0/account-transfer/" + accountID
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var out []AccountTransfer
	var page Pagination
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}
