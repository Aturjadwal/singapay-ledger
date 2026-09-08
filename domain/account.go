package domain

import (
	"context"

	"github.com/21strive/redifu"
)

type Money struct {
	Amount   int64
	Currency Currency
}

type Currency string

const (
	CurrencyIDR Currency = "IDR"
	CurrencyUSD Currency = "USD"
)

type OwnerType string

const (
	OWNER_TYPE_PLATFORM               = "PLATFORM"
	OwnerTypeSeller         OwnerType = "SELLER"
	OwnerTypePlatform       OwnerType = OWNER_TYPE_PLATFORM
	OwnerTypePaymentGateway OwnerType = "PAYMENT_GATEWAY"
)

type Account struct {
	*redifu.Record   `json:",inline" bson:",inline" db:"-"`
	DokuSubAccountID string `json:"doku_sub_account_id,omitempty"`

	// SingapayAccountID is the sub-account's ULID. It identifies the account on every
	// Singapay endpoint — payment link, virtual account, QRIS, e-wallet, balance,
	// statements, disbursement — and as the remitter of an account transfer.
	SingapayAccountID string `json:"singapay_account_id,omitempty"`

	// SingapayAccountNumber is the sub-account's 12-digit number. Singapay accepts it
	// in exactly one place: beneficiary_account_number on an account transfer. It is
	// not derivable from SingapayAccountID, and Singapay may not assign one at
	// creation — see CanReceiveTransfer.
	SingapayAccountNumber string `json:"singapay_account_number,omitempty"`

	OwnerType             OwnerType `json:"owner_type"`
	OwnerID               string    `json:"owner_id"`
	Currency              Currency  `json:"currency"`
	PendingBalance        int64     `json:"pending_balance"`         // Cached pending balance
	AvailableBalance      int64     `json:"available_balance"`       // Cached available balance
	TotalWithdrawalAmount int64     `json:"total_withdrawal_amount"` // Sum of all withdrawals
	TotalDepositAmount    int64     `json:"total_deposit_amount"`    // Sum of all deposits
}

func NewAccount(ownerType OwnerType, dokuSubAccountID string, ownerID string, currency Currency) Account {
	a := Account{
		DokuSubAccountID: dokuSubAccountID,
		OwnerType:        ownerType,
		OwnerID:          ownerID,
		Currency:         currency,
	}
	redifu.InitRecord(&a)
	return a
}

func NewPlatformAccount(dokuSubAccountID string, ownerID string, currency Currency) Account {
	return NewAccount(OwnerTypePlatform, dokuSubAccountID, ownerID, currency)
}

func NewSellerAccount(dokuSubAccountID string, sellerId string, currency Currency) Account {
	return NewAccount(OwnerTypeSeller, dokuSubAccountID, sellerId, currency)
}

func NewPaymentGatewayAccount(dokuSubAccountID string, ownerID string, currency Currency) Account {
	return NewAccount(OwnerTypePaymentGateway, dokuSubAccountID, ownerID, currency)
}

// SetSingapayAccount records both identifiers Singapay issues for one sub-account.
//
// It is a setter rather than a constructor parameter because an account is created with
// whichever gateway backs it, and the three constructors above already carry a positional
// gateway id. accountNumber may legitimately be empty: Singapay declares it nullable in
// the create response.
func (a *Account) SetSingapayAccount(accountID, accountNumber string) {
	a.SingapayAccountID = accountID
	a.SingapayAccountNumber = accountNumber
}

// CanReceiveTransfer reports whether this account can be the beneficiary of a Singapay
// account transfer.
//
// An account transfer names its destination by account number and by nothing else — the
// ULID is rejected there — so an account that arrived without a number cannot be paid
// into from a sibling account. Worth checking when the account is created rather than
// when a platform-fee transfer silently fails for the hundredth time.
func (a *Account) CanReceiveTransfer() bool {
	return a.SingapayAccountNumber != ""
}

type AccountRepository interface {
	GetByID(ctx context.Context, id string) (*Account, error)

	// GetByIDForUpdate reads the account under a row lock (SELECT ... FOR UPDATE) and
	// must be called inside a transaction. Balances are derived by summing
	// ledger_entries, so a plain read gives every concurrent caller the same answer
	// until one of them commits — two withdrawals can both see enough balance and both
	// pay out. Taking this lock first serialises them: the second waits for the first's
	// entries to land, then reads a balance that includes them.
	GetByIDForUpdate(ctx context.Context, id string) (*Account, error)
	GetByOwner(ctx context.Context, ownerType OwnerType, ownerID string) (*Account, error)
	GetByDokuSubAccountID(ctx context.Context, dokuSubAccountID string) (*Account, error)
	GetBySingapayAccountID(ctx context.Context, singapayAccountID string) (*Account, error)
	GetBySellerID(ctx context.Context, sellerId string) (*Account, error)
	GetPlatformAccount(ctx context.Context) (*Account, error)
	GetPaymentGatewayAccount(ctx context.Context) (*Account, error)
	Save(ctx context.Context, account *Account) error
	Delete(ctx context.Context, id string) error

	// Balance update methods
	UpdateBalances(ctx context.Context, accountID string, pendingDelta, availableDelta int64) error
	IncrementDeposit(ctx context.Context, accountID string, amount int64) error
	IncrementWithdrawal(ctx context.Context, accountID string, amount int64) error
}
