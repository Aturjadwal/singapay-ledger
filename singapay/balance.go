package singapay

import (
	"context"
	"net/http"
)

// Balance is a balance snapshot for a merchant or one sub-account.
//
// Pending and Available are the two the ledger cares about: a money-in transaction lands
// in Pending, and settlement moves it to Available, which is what disbursement and
// account transfer draw on. That is the same shape as the ledger's own PENDING and
// AVAILABLE, which is why the reconciliation model survives the migration.
//
// Held has no counterpart in the ledger. Confirm with Singapay whether it is already
// counted inside Total before comparing anything against it.
type Balance struct {
	Held      Amount `json:"held_balance"`
	Available Amount `json:"available_balance"`
	Pending   Amount `json:"pending_balance"`
	Total     Amount `json:"balance"`

	// AccountID is set on the per-account response only.
	AccountID string `json:"account_id"`
}

// GetMerchantBalance returns the aggregate balance across every sub-account.
func (c *Client) GetMerchantBalance(ctx context.Context) (*Balance, error) {
	var out Balance
	if err := c.call(ctx, http.MethodGet, "/api/v1.0/balance-inquiry", nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccountBalance returns the balance of one sub-account, by ULID.
func (c *Client) GetAccountBalance(ctx context.Context, accountID string) (*Balance, error) {
	var out Balance
	if err := c.call(ctx, http.MethodGet, "/api/v1.0/balance-inquiry/"+accountID, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}
