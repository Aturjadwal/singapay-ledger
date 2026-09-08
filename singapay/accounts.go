package singapay

import (
	"context"
	"net/http"
)

// AccountType selects how a sub-account is provisioned.
type AccountType string

const (
	// AccountTypeOwned creates the account active immediately, with no KYB. This is
	// the type a platform provisioning sellers on the fly needs: it is the only one
	// that can accept a payment in the same request that created it.
	AccountTypeOwned AccountType = "owned"

	// AccountTypePersonalManaged behaves identically to owned — active immediately,
	// no KYB — per Singapay's own documentation.
	AccountTypePersonalManaged AccountType = "personal_managed"

	// AccountTypeBusinessManaged creates the account *inactive*, pending a KYB review
	// a human at Singapay must approve, and returns a KYBOnboardingURL to forward to
	// the sub-merchant.
	//
	// It cannot be used from a synchronous checkout path: the account will not accept
	// payment until that review completes, so the booking that created it fails.
	AccountTypeBusinessManaged AccountType = "business_managed"
)

// KYBStatus applies to managed sub-accounts only; it is empty for owned accounts.
type KYBStatus string

const (
	KYBInReview KYBStatus = "kyb_in_review"
	KYBVerified KYBStatus = "kyb_verified"
)

// AccountStatus is an account's lifecycle state. Only active accounts can transact.
type AccountStatus string

const (
	AccountActive   AccountStatus = "active"
	AccountInactive AccountStatus = "inactive"
)

// Account is a Singapay sub-account.
type Account struct {
	// ID is the ULID. It is the {account_id} path parameter and the account_id body
	// field everywhere else in this package.
	ID string `json:"id"`

	// Number is the 12-digit account number. It is *not* interchangeable with ID: an
	// account transfer names its destination by Number and nothing else, so an account
	// stored without it can receive a platform-fee transfer from nowhere.
	//
	// Singapay declares it nullable, so it can genuinely be absent.
	Number string `json:"account_number"`

	Name         string        `json:"name"`
	Status       AccountStatus `json:"status"`
	Type         AccountType   `json:"account_type"`
	KYBStatus    KYBStatus     `json:"kyb_status"`
	LegalName    string        `json:"legal_name"`
	BrandName    string        `json:"brand_name"`
	BusinessType string        `json:"business_type"`

	// KYBOnboardingURL is the self-onboarding link for a business_managed account. It
	// becomes null once KYB is verified, and can be re-read from GetAccount until then.
	KYBOnboardingURL string `json:"kyb_onboarding_url"`

	// InviteMembers lists dashboard users with access to this account.
	InviteMembers []string `json:"invite_members"`
}

// IsActive reports whether the account can transact.
func (a *Account) IsActive() bool { return a.Status == AccountActive }

// CreateAccountRequest is the body of POST /api/v1.0/accounts.
type CreateAccountRequest struct {
	Name string      `json:"name"`
	Type AccountType `json:"account_type"`

	// InviteMembers grants dashboard access to additional emails. The calling
	// merchant's own login is always granted regardless.
	//
	// Note this is the only place an email appears. Singapay does not identify a
	// sub-account by email — the ULID does that — and imposes no length limit on one.
	InviteMembers []string `json:"invite_members,omitempty"`
}

// CreateAccount provisions a sub-account.
//
// There is no idempotency key and no duplicate check: Singapay will happily create a
// second account with the same name, and nothing in the response distinguishes it from the
// first. A retry after a timeout therefore risks creating an account that holds money and
// that nothing references — the caller must guarantee at-most-once itself, ideally by
// checking its own records before calling.
func (c *Client) CreateAccount(ctx context.Context, req CreateAccountRequest) (*Account, error) {
	if req.Type == "" {
		req.Type = AccountTypeOwned
	}
	var out Account
	if err := c.call(ctx, http.MethodPost, "/api/v1.0/accounts", req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccount reads one sub-account by ULID.
func (c *Client) GetAccount(ctx context.Context, id string) (*Account, error) {
	var out Account
	if err := c.call(ctx, http.MethodGet, "/api/v1.0/accounts/"+id, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAccounts returns the merchant's sub-accounts.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, Pagination, error) {
	var out []Account
	var page Pagination
	if err := c.callPaged(ctx, http.MethodGet, "/api/v1.0/accounts", nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}

// UpdateAccountRequest is the body of PATCH /api/v1.0/accounts/update/{id}. At least one
// field must be set.
type UpdateAccountRequest struct {
	Name   string        `json:"name,omitempty"`
	Status AccountStatus `json:"status,omitempty"`
	// InviteMembers *replaces* the member list entirely. Omitting it leaves the
	// current members alone; sending an empty list is not the same thing.
	InviteMembers []string `json:"invite_members,omitempty"`
}

// UpdateAccount changes an account's name, status, or member list.
//
// This is also how an account is retired: Singapay has no delete endpoint, so
// deactivation means Status: [AccountInactive].
func (c *Client) UpdateAccount(ctx context.Context, id string, req UpdateAccountRequest) (*Account, error) {
	var out Account
	if err := c.call(ctx, http.MethodPatch, "/api/v1.0/accounts/update/"+id, req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}
