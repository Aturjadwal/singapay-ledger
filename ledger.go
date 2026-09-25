package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
	"github.com/google/uuid"
)

// LedgerClient is the entry point for all ledger operations.
// It works with Account (entity) and LedgerEntry (immutable records).
// Balances are always derived by summing ledger_entries — never stored.
type LedgerClient struct {
	db           *sql.DB
	txProvider   repo.TransactionProvider
	logger       *slog.Logger
	repoProvider repo.RepositoryProvider
	gateway      PaymentGateway
}

// NewLedgerClient wires the ledger to a payment gateway and a database.
//
// gateway is normally a *singapay.Client built with singapay.NewFromEnv. It is taken as
// an interface so the paths that decide what happens to money — a payout whose outcome is
// unknown, a settlement that does not match — can be tested without a merchant account.
//
// It takes no object-storage handle: this package keeps ledgers, and nothing it does
// touches a bucket.
func NewLedgerClient(db *sql.DB, gateway PaymentGateway, logger *slog.Logger) *LedgerClient {
	txProvider := repo.NewTransactionProvider(db)
	repoProvider := repo.NewRepositoryProvider(db)

	return &LedgerClient{
		db:           db,
		txProvider:   txProvider,
		logger:       logger,
		gateway:      gateway,
		repoProvider: repoProvider,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Account management
// ─────────────────────────────────────────────────────────────────────────────

// GetAccountByID returns an account by its internal UUID.
func (c *LedgerClient) GetAccountByID(ctx context.Context, id string) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetByID(ctx, id)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by ID", err)
	}
	return account, nil
}

// GetAccountByOwner returns an account by owner type + owner ID.
func (c *LedgerClient) GetAccountByOwner(ctx context.Context, ownerType domain.OwnerType, ownerID string) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetByOwner(ctx, ownerType, ownerID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by owner", err)
	}
	return account, nil
}

// GetAccountBySingapayAccountID returns an account by its Singapay sub-account ULID.
//
// Not by account number: the number appears only as a transfer beneficiary, and rows that
// have no number would be unreachable through it.
func (c *LedgerClient) GetAccountBySingapayAccountID(ctx context.Context, singapayAccountID string) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetBySingapayAccountID(ctx, singapayAccountID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account", err)
	}
	return account, nil
}

// GetAccountBySellerID returns an account by its seller ID (owner_type=SELLER, owner_id=sellerID).
func (c *LedgerClient) GetAccountBySellerID(ctx context.Context, sellerID string) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetBySellerID(ctx, sellerID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by seller ID", err)
	}
	return account, nil
}

// CreateAccount provisions a Singapay sub-account and persists an Account record.
// Idempotent against our own table: if an account for accountID already exists, returns
// ErrLedgerAlreadyExists without calling Singapay.
//
// That local check is the only idempotency there is. Singapay's create endpoint has no
// idempotency key and no duplicate detection — it will happily open a second account with
// the same name — so a retry after a timeout can leave behind an account that holds money
// and that nothing references. The existing-row check runs first for exactly that reason,
// and a caller retrying a timeout must check for the account before calling again.
//
// No email is sent, and that is deliberate rather than an omission. The only place an
// email could go is invite_members, and Singapay rejects any address that is not already
// a member of *our* merchant — "http 422: One or more emails do not belong to a member of
// this merchant." A seller's own address never is, so passing it failed every seller's
// first paid booking. invite_members grants an existing colleague dashboard access to a
// sub-account; it is not a way to enrol a stranger. Sellers get no Singapay dashboard,
// which is what an `owned` sub-account means in the first place.
func (c *LedgerClient) CreateAccount(ctx context.Context, accountID string, name string, currency domain.Currency) (*domain.Account, error) {
	// Check for existing account
	existing, err := c.repoProvider.Account().GetByOwner(ctx, domain.OwnerTypeSeller, accountID)
	if err == nil {
		c.logger.InfoContext(ctx, "Account already exists for owner ID", "owner_id", accountID, "account_id", existing.UUID)
		return nil, ledgererr.ErrLedgerAlreadyExists
	} else if !ledgererr.IsErrorCode(ledgererr.CodeNotFound, err) {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to check existing account", err)
	}

	// This call sits inside the seller's first paid booking — the worst possible place
	// to discover a 4xx from an unusual display name.
	sanitizedName := sanitizeSubAccountName(name)
	if sanitizedName != name {
		c.logger.InfoContext(ctx, "Sub-account name sanitized for Singapay", "owner_id", accountID, "original", name, "sanitized", sanitizedName)
	}

	// AccountTypeOwned is the only type usable here. personal_managed behaves the same,
	// but business_managed creates the account *inactive* pending a KYB review a human
	// at Singapay must approve — and an inactive account cannot accept the payment that
	// is waiting on this call.
	account, gwErr := c.gateway.CreateAccount(ctx, singapay.CreateAccountRequest{
		Name: sanitizedName,
		Type: singapay.AccountTypeOwned,
	})
	if gwErr != nil {
		c.logger.ErrorContext(ctx, "Singapay CreateAccount failed", "owner_id", accountID, "error", gwErr)
		return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError,
			"failed to create Singapay sub-account", gwErr)
	}

	c.logger.InfoContext(ctx, "Singapay sub-account created",
		"owner_id", accountID,
		"singapay_account_id", account.ID,
		"singapay_account_number", account.Number,
		"status", account.Status,
	)

	// An account with no number cannot be the beneficiary of an account transfer, and
	// the platform fee reaches the platform by exactly that route. Singapay declares the
	// field nullable, so this is a real possibility rather than a defensive check — and
	// it is worth a loud line now instead of a platform-fee transfer that fails silently
	// for months. The account is still saved: it can take payments, and the number can
	// be backfilled from GET /accounts/{id}.
	if account.Number == "" {
		c.logger.WarnContext(ctx, "Singapay issued no account number — this account cannot receive platform-fee transfers until one is backfilled",
			"owner_id", accountID,
			"singapay_account_id", account.ID,
		)
	}

	ledgerAccount := domain.NewSellerAccount(account.ID, accountID, currency)
	ledgerAccount.SetSingapayAccount(account.ID, account.Number)

	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if err := tx.Account().Save(ctx, &ledgerAccount); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to create account", err)
		}
		return nil
	})
	if err != nil {
		// The sub-account exists at Singapay and nothing here references it. Loud,
		// because the fix is to bind the row by hand — creating another would leave the
		// first one holding money nobody watches.
		c.logger.ErrorContext(ctx, "CRITICAL: Singapay sub-account created but the ledger row could not be saved — bind it by hand, do not re-create",
			"owner_id", accountID,
			"singapay_account_id", account.ID,
			"singapay_account_number", account.Number,
			"error", err,
		)
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "transaction failed while creating account", err)
	}

	return &ledgerAccount, nil
}

// BackfillSingapayAccountNumber reads an account's number from Singapay and stores it.
//
// It exists because Singapay may create a sub-account without a number, and an account
// with no number silently cannot receive a platform-fee transfer — the one operation
// where Singapay names its destination by number and rejects the ULID. This is the
// repair, and it is safe to call on an account that already has one.
func (c *LedgerClient) BackfillSingapayAccountNumber(ctx context.Context, accountID string) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetByID(ctx, accountID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account", err)
	}
	if account.SingapayAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"account has no Singapay sub-account id to read a number from", nil)
	}
	if account.CanReceiveTransfer() {
		return account, nil
	}

	remote, gwErr := c.gateway.GetAccount(ctx, account.SingapayAccountID)
	if gwErr != nil {
		return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "failed to read Singapay sub-account", gwErr)
	}
	if remote.Number == "" {
		return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError,
			"Singapay still reports no account number for this sub-account", nil)
	}

	account.SetSingapayAccount(account.SingapayAccountID, remote.Number)
	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		return tx.Account().Save(ctx, account)
	})
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save backfilled account number", err)
	}

	c.logger.InfoContext(ctx, "Backfilled Singapay account number",
		"account_id", account.UUID,
		"singapay_account_id", account.SingapayAccountID,
		"singapay_account_number", account.SingapayAccountNumber,
	)
	return account, nil
}

// The platform account is deliberately not creatable from here. It used to be:
// CreatePlatformAccount provisioned a sub-account under whatever email the caller passed,
// and callers invoked it lazily from the booking path. That made the account that collects
// every platform fee a side effect of somebody else's booking — and because Singapay's
// create endpoint has no duplicate check at all, a caller with a stale constant would
// quietly open a *second* sub-account and route the fees into one nobody watches, without
// raising a single error.
//
// Provisioning is a deliberate, once-per-environment act: create the sub-account against
// Singapay, then insert the ledger_accounts row by hand with its ULID *and* its account
// number. Readers stay: GetPlatformAccount is what ProcessPlatformFeeTransfer and the
// reconciliation path use, and both refuse to run when the account is missing or has no
// Singapay identifiers.

// CreatePaymentGatewayAccount creates the PAYMENT_GATEWAY-type expense account (singleton).
//
// It is a bookkeeping account, not a Singapay sub-account: it holds the gateway fees this
// ledger has recognised so they can be cleared on settlement. Nothing is provisioned at
// Singapay for it, which is why it carries no sub-account identifiers.
func (c *LedgerClient) CreatePaymentGatewayAccount(ctx context.Context, currency domain.Currency) (*domain.Account, error) {
	ownerID := "SINGAPAY"
	existing, err := c.repoProvider.Account().GetPaymentGatewayAccount(ctx)
	if err == nil {
		c.logger.InfoContext(ctx, "Payment gateway account already exists, skipping creation", "owner_id", ownerID, "account_id", existing.UUID)
		return existing, nil
	}
	if !ledgererr.IsAppError(err, repo.ErrNotFound) {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to check existing payment gateway account", err)
	}

	account := domain.NewPaymentGatewayAccount("", ownerID, currency)
	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if err := tx.Account().Save(ctx, &account); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to create payment gateway account", err)
		}
		return nil
	})
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "transaction failed while creating payment gateway account", err)
	}

	return &account, nil
}

// DeleteAccount deletes an account only when it has no remaining balance.
func (c *LedgerClient) DeleteAccount(ctx context.Context, id string) error {
	err := c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		c.logger.InfoContext(ctx, "Attempting to delete account", "account_id", id)

		account, err := tx.Account().GetByID(ctx, id)
		if err != nil {
			if ledgererr.IsAppError(err, repo.ErrNotFound) {
				return ledgererr.ErrLedgerNotFound.WithError(err)
			}
			return ledgererr.NewError(ledgererr.CodeInternal, "failed to get account for deletion", err)
		}

		// Derive current balances from entries
		pending, available, err := tx.LedgerEntry().GetAllBalances(ctx, account.UUID)
		if err != nil {
			return ledgererr.NewError(ledgererr.CodeInternal, "failed to derive account balance", err)
		}
		if pending != 0 || available != 0 {
			return ledgererr.NewError(ledgererr.CodeInternal,
				fmt.Sprintf("cannot delete account with non-zero balance (pending=%d, available=%d)", pending, available), nil)
		}

		if err := tx.Account().Delete(ctx, id); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to delete account", err)
		}
		return nil
	})

	if err != nil {
		return ledgererr.NewError(ledgererr.CodeInternal, "transaction failed while deleting account", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Balance queries (always derived from ledger_entries)
// ─────────────────────────────────────────────────────────────────────────────

// GetBalance returns the cached balances for an account.
// This reads from ledger_accounts table (cached values updated on each ledger entry save).
func (c *LedgerClient) GetBalance(ctx context.Context, accountID string) (*BalanceResponse, error) {
	account, err := c.repoProvider.Account().GetByOwner(ctx, domain.OwnerTypeSeller, accountID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account", err)
	}

	currency := string(account.Currency)
	return &BalanceResponse{
		PendingBalance: MoneyResponse{
			Amount:   account.PendingBalance,
			Currency: currency,
		},
		AvailableBalance: MoneyResponse{
			Amount:   account.AvailableBalance,
			Currency: currency,
		},
		TotalWithdrawalAmount: MoneyResponse{
			Amount:   account.TotalWithdrawalAmount,
			Currency: currency,
		},
		TotalDepositAmount: MoneyResponse{
			Amount:   account.TotalDepositAmount,
			Currency: currency,
		},
		Currency: currency,
	}, nil
}

// GetBalanceByAccountUUID returns cached balances directly by the account's internal UUID.
func (c *LedgerClient) GetBalanceByAccountUUID(ctx context.Context, accountUUID string) (*BalanceResponse, error) {
	account, err := c.repoProvider.Account().GetByID(ctx, accountUUID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account", err)
	}

	currency := string(account.Currency)
	return &BalanceResponse{
		PendingBalance: MoneyResponse{
			Amount:   account.PendingBalance,
			Currency: currency,
		},
		AvailableBalance: MoneyResponse{
			Amount:   account.AvailableBalance,
			Currency: currency,
		},
		TotalWithdrawalAmount: MoneyResponse{
			Amount:   account.TotalWithdrawalAmount,
			Currency: currency,
		},
		TotalDepositAmount: MoneyResponse{
			Amount:   account.TotalDepositAmount,
			Currency: currency,
		},
		Currency: currency,
	}, nil
}

// GetAllBalancesBySellerID returns cached balances for a seller's account.
// This is a pure read from ledger_accounts — no gateway call.
func (c *LedgerClient) GetAllBalancesBySellerID(ctx context.Context, sellerID string) (*BalanceResponse, error) {
	account, err := c.repoProvider.Account().GetBySellerID(ctx, sellerID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by seller ID", err)
	}

	currency := string(account.Currency)
	return &BalanceResponse{
		PendingBalance: MoneyResponse{
			Amount:   account.PendingBalance,
			Currency: currency,
		},
		AvailableBalance: MoneyResponse{
			Amount:   account.AvailableBalance,
			Currency: currency,
		},
		TotalWithdrawalAmount: MoneyResponse{
			Amount:   account.TotalWithdrawalAmount,
			Currency: currency,
		},
		TotalDepositAmount: MoneyResponse{
			Amount:   account.TotalDepositAmount,
			Currency: currency,
		},
		Currency: currency,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Bank account validation
// ─────────────────────────────────────────────────────────────────────────────

// ValidateBankAccountRequest contains the parameters to validate a bank account
type ValidateBankAccountRequest struct {
	// BankCode accepts a three-digit national code ("002") or a SWIFT code
	// ("BRINIDJA"). Storing SWIFT is the safer choice: the payout fee quote is a v1.0
	// endpoint with no v2 counterpart and accepts SWIFT only, so a three-digit code
	// makes "quote the fee, then transfer" break at the first step.
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"` // Optional: for the caller's own comparison
}

// ValidateBankAccountResponse contains the result of bank account validation
type ValidateBankAccountResponse struct {
	IsValid       bool   `json:"is_valid"`
	BankCode      string `json:"bank_code"`
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"` // The name the bank holds for this account
	// Message carries the bank's reason when the account was not found.
	Message string `json:"message,omitempty"`
}

// ValidateBankAccount performs a real-time bank account name inquiry.
//
// Note what "not valid" means here. Singapay answers HTTP 200 with SP000 for an account
// that does not exist, reporting the outcome in the body — so a nil error does not mean
// the account is real. IsValid is the answer; err means the inquiry itself could not be
// made, which is a different thing and is returned as an error rather than folded into a
// false.
func (c *LedgerClient) ValidateBankAccount(ctx context.Context, req *ValidateBankAccountRequest) (*ValidateBankAccountResponse, error) {
	if req.BankCode == "" || req.AccountNumber == "" {
		return nil, ledgererr.ErrInvalidBankAccount.WithError(fmt.Errorf("bank_code and account_number are required"))
	}

	beneficiary, gwErr := c.gateway.CheckBeneficiary(ctx, req.BankCode, req.AccountNumber)
	if gwErr != nil {
		c.logger.ErrorContext(ctx, "Singapay CheckBeneficiary failed",
			"bank_code", req.BankCode,
			"account_number", maskAccountNumber(req.AccountNumber),
			"error", gwErr,
		)
		return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "bank account inquiry failed", gwErr)
	}

	if !beneficiary.IsValid() {
		c.logger.InfoContext(ctx, "Bank account inquiry returned invalid",
			"bank_code", req.BankCode,
			"account_number", maskAccountNumber(req.AccountNumber),
			"message", beneficiary.Message,
		)
		return &ValidateBankAccountResponse{
			IsValid:       false,
			BankCode:      req.BankCode,
			AccountNumber: req.AccountNumber,
			Message:       beneficiary.Message,
		}, nil
	}

	return &ValidateBankAccountResponse{
		IsValid:       true,
		BankCode:      beneficiary.BankCode,
		BankName:      beneficiary.BankName,
		AccountNumber: beneficiary.AccountNumber,
		AccountName:   beneficiary.AccountName,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Withdrawal (Disbursement)
// ─────────────────────────────────────────────────────────────────────────────

// WithdrawRequest contains the parameters to withdraw funds to a bank account
type WithdrawRequest struct {
	AccountID string `json:"account_id"`

	// Amount is what the seller asked to withdraw and the whole of what their balance is
	// debited. The transfer fee is taken out of it, not charged on top, so they receive
	// Amount less the fee.
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
	Description   string `json:"description"`
}

// WithdrawResponse contains the result of a withdrawal request.
//
// The three amounts are one subtraction: Amount - TransferFee = NetAmount. Amount is what
// the seller asked for and what their balance moved by; NetAmount is what lands in their
// bank account.
type WithdrawResponse struct {
	DisbursementID string `json:"disbursement_id"`
	Status         string `json:"status"`

	// Amount is what was requested, and exactly what the seller's balance was debited.
	Amount int64 `json:"amount"`

	// TransferFee is Singapay's charge for the transfer, deducted from Amount.
	TransferFee int64 `json:"transfer_fee"`

	// NetAmount is what the beneficiary receives: Amount less TransferFee.
	NetAmount int64 `json:"net_amount"`

	Currency string `json:"currency"`
	Message  string `json:"message"`
}

// Withdraw initiates a withdrawal from an account to an external bank account.
//
// The fee comes out of the requested amount, it is not charged on top of it. A seller who
// asks to withdraw Rp 15.000 against a Rp 3.000 transfer fee has their balance debited
// Rp 15.000 and receives Rp 12.000 — never Rp 18.000 off the balance for Rp 15.000
// received. The requested amount is the whole cost to the seller, and every number here
// derives from it:
//
//	requested (req.Amount, debited, reserved)  15.000
//	  − transfer fee (quoted)                   3.000
//	  = net (sent to Singapay, received)       12.000
//
// Singapay's arithmetic runs the other way: its disbursement amount is the net the
// beneficiary receives and it adds the fee on top, debiting the sub-account net + fee.
// Sending the net is therefore exactly what makes the sub-account debit come to the
// requested amount, so the ledger's reservation and Singapay's debit agree.
//
// Flow:
//  1. Look up Account by sellerID (owner_id)
//  2. Quote the transfer fee, because it has to be carved out of the requested amount
//  3. Refuse a request the fee would swallow — a net of zero or less is not a payout
//  4. Reserve: under a row lock, check the available balance against the REQUESTED amount
//     and — in the same transaction — write the journal, the PENDING Disbursement carrying
//     the reference number, and the debit that holds the money
//  5. Send the payout under that reference, for the NET
//  6. Book the answer: complete it, leave it in flight, or release the reservation if the
//     payout is known not to have happened
//
// Three things go wrong here if this order is disturbed, and they are different problems.
//
// The gateway call must not come first. Payout succeeds, process dies before the commit,
// and the money is gone with only a log line naming it — and no stored reference, so it
// could never be asked about again. Writing the row and its reference first makes that
// recoverable: see RetryDisbursement.
//
// The balance must be reserved, not merely checked. Balances are derived by summing
// ledger_entries, so two *different* withdrawals racing each other both pass a bare check
// and both pay out. Idempotency does not help — each is a distinct payout with its own
// reference. Only holding the money at request time does.
//
// And the fee must be quoted before the net is worked out, because the net is the
// subtraction. A fee that could not be quoted is treated as zero, which sends the full
// requested amount as the net and leaves the platform absorbing the fee — see
// quotePayoutFee for why that is the chosen failure.
//
// One error comes with a response: ErrGatewayOutcomeUnknown. The payout was sent, or may
// have been, and nobody knows yet whether it landed — so the disbursement stays PENDING
// with its balance reserved, and the response names it. A caller should show that payout
// as in progress rather than invite a second attempt, which would be a new payout under a
// new reference, not a retry of this one.
func (c *LedgerClient) Withdraw(ctx context.Context, sellerID string, req *WithdrawRequest) (*WithdrawResponse, error) {
	if req.AccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "account_id is required", nil)
	}
	if req.Amount <= 0 {
		return nil, ledgererr.ErrInvalidDisbursementAmount
	}

	// Resolve the account
	account, err := c.repoProvider.Account().GetByOwner(ctx, domain.OwnerTypeSeller, sellerID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account", err)
	}

	return c.withdrawFrom(ctx, account, req)
}

// withdrawFrom runs a withdrawal against an account that has already been resolved.
//
// Withdraw and WithdrawFromPlatform differ only in how they find the account. Everything
// from here on — and every reason the order matters, set out on Withdraw — is shared, so a
// seller's payout and the platform's cannot drift apart in how they reserve, price or book.
func (c *LedgerClient) withdrawFrom(ctx context.Context, account *domain.Account, req *WithdrawRequest) (*WithdrawResponse, error) {
	if account.SingapayAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"account has no Singapay sub-account id; nothing can be paid out of it", nil)
	}

	// Generate disbursement ID upfront — it travels with the payout as its note.
	disbursementID := domain.GenerateID()

	currency := domain.Currency(req.Currency)
	if currency == "" {
		currency = account.Currency
	}

	bankAccount := domain.BankAccount{
		BankCode:      req.BankCode,
		AccountNumber: req.AccountNumber,
		AccountName:   req.AccountName,
	}

	disbursement, err := domain.NewDisbursementWithID(disbursementID, account.UUID, req.Amount, currency, bankAccount, req.Description)
	if err != nil {
		return nil, err
	}

	// The reference this payout will travel under, stored with the row so a retry can
	// ask about the same one instead of sending a second payout.
	disbursement.PayoutRequestID = uuid.NewString()
	disbursement.GatewayFee = c.quotePayoutFee(ctx, account, disbursement)

	// The fee is carved out of the request, so a request the fee would swallow has nothing
	// left to send. Refused here rather than at the gateway: Singapay would reject a
	// zero-or-negative transfer anyway, and by then the balance is already reserved and a
	// row is already written for a payout that was never possible.
	if disbursement.NetAmount() <= 0 {
		return nil, ledgererr.ErrInvalidDisbursementAmount.WithError(
			fmt.Errorf("requested %d leaves nothing after the %d transfer fee; withdraw more than the fee",
				disbursement.Amount, disbursement.GatewayFee),
		)
	}

	if err := c.reserveBalance(ctx, account, disbursement); err != nil {
		return nil, err
	}

	resp, err := c.executePayout(ctx, account, disbursement)
	if err != nil && ledgererr.IsErrorCode(ledgererr.CodeGatewayOutcomeUnknown, err) {
		// The row and its reservation stand, and the money may be on its way. Naming the row
		// is the difference between a caller that waits for this payout and one that asks
		// again — and asking again is a second payout under a new reference, not a retry.
		return inFlightWithdrawResponse(disbursement), err
	}
	return resp, err
}

// inFlightWithdrawResponse describes a payout whose outcome is not known: sent, or possibly
// sent, with its balance still reserved and its row still open. RetryDisbursement or the
// money-out webhook settles it.
func inFlightWithdrawResponse(disbursement *domain.Disbursement) *WithdrawResponse {
	return &WithdrawResponse{
		DisbursementID: disbursement.UUID,
		Status:         string(disbursement.Status),
		Amount:         disbursement.Amount,
		TransferFee:    disbursement.GatewayFee,
		NetAmount:      disbursement.NetAmount(),
		Currency:       string(disbursement.Currency),
		Message:        "Payout outcome unknown; it stays reserved until resolved by inquiry",
	}
}

// quotePayoutFee asks Singapay what this payout will cost, so the fee can be carved out of
// the requested amount.
//
// It quotes against the requested amount rather than the net it is about to work out,
// which is only correct because the fee is flat rather than a percentage: for a flat fee
// both quotes return the same number, whereas quoting the net would need the very fee the
// quote is meant to produce. For bank transfers — the only destination this ledger pays
// out to — Rp 3.000 flat is contractually agreed with Singapay, not merely observed, so
// the assumption is guaranteed rather than inferred. Other destinations are flat too but
// priced differently (Rp 2.500 to a wallet; see migration 020).
//
// It is still quoted per payout rather than hardcoded, so that a rate Singapay changes is
// followed automatically instead of silently mispricing every withdrawal — and if the fee
// ever becomes a percentage, this is the one place that assumption breaks: the quote would
// then have to be solved for rather than read off.
//
// A failed quote is not a failed withdrawal. The quote endpoint takes a SWIFT code where
// the transfer itself takes either a SWIFT or a three-digit national code, so an account
// stored with a three-digit code cannot be quoted at all — and refusing every such payout
// would be a worse outcome than mispricing the fee. The fee is left at zero and logged:
// the seller then receives the full amount they asked for, their balance still moves by
// exactly that amount, and it is the platform sub-account that absorbs the transfer fee.
// That is the right way round for the error to fall, and it is visible in the log rather
// than invisible in the books.
//
// The quote is also a balance check: Singapay refuses it when the resulting gross would
// exceed the account's available balance. That refusal is deliberately not fatal here
// either — the ledger's own reservation is the authority on whether the seller may
// withdraw, and Singapay's view of the balance can lag a settlement.
func (c *LedgerClient) quotePayoutFee(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement) int64 {
	quote, err := c.gateway.CheckFee(ctx, account.SingapayAccountID, disbursement.BankAccount.BankCode, disbursement.Amount)
	if err != nil {
		c.logger.WarnContext(ctx, "Could not quote the payout fee — sending the full requested amount, so the platform absorbs the transfer fee",
			"disbursement_id", disbursement.UUID,
			"account_id", account.UUID,
			"bank_code", disbursement.BankAccount.BankCode,
			"requested_amount", disbursement.Amount,
			"error", err,
		)
		return 0
	}

	fee, err := quote.TransferFee.Rupiah()
	if err != nil {
		// Rupiah() refuses to round rather than silently truncating. A fractional fee
		// is not something to guess at on the path that moves money.
		c.logger.WarnContext(ctx, "Payout fee quote is not a whole rupiah amount — sending the full requested amount instead",
			"disbursement_id", disbursement.UUID,
			"transfer_fee", quote.TransferFee.String(),
			"error", err,
		)
		return 0
	}

	c.logger.InfoContext(ctx, "Quoted payout fee",
		"disbursement_id", disbursement.UUID,
		"requested_amount", disbursement.Amount,
		"transfer_fee", fee,
		"net_amount", disbursement.Amount-fee,
	)
	return fee
}

// reserveBalance takes the money out of the available balance and records the intent,
// atomically, before anything is sent to the gateway.
//
// The lock is the part that matters. Balances are derived by summing ledger_entries, so
// without it two concurrent withdrawals both read the balance before either has written
// its debit, both find it sufficient, and both pay out. Locking the account row first
// makes the second one wait until the first's entries are committed, so it reads a
// balance that already accounts for them.
//
// The balance check therefore lives inside this transaction. Checking outside and writing
// inside would be the same race with extra steps.
//
// What is reserved is the requested amount and nothing more. The transfer fee is already
// inside it — the payout goes out for the net — so Singapay's debit of net + fee comes to
// the same number. Reserving the requested amount plus the fee would charge the seller the
// fee twice: once by shrinking what they receive, once again against their balance.
func (c *LedgerClient) reserveBalance(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement) error {
	err := c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if _, err := tx.Account().GetByIDForUpdate(ctx, account.UUID); err != nil {
			return ledgererr.NewError(ledgererr.CodeInternal, "failed to lock account for withdrawal", err)
		}

		_, available, err := tx.LedgerEntry().GetAllBalances(ctx, account.UUID)
		if err != nil {
			return ledgererr.NewError(ledgererr.CodeInternal, "failed to derive available balance", err)
		}

		if disbursement.Amount > available {
			c.logger.WarnContext(ctx, "Insufficient available balance for withdrawal",
				"account_id", account.UUID,
				"requested_amount", disbursement.Amount,
				"transfer_fee", disbursement.GatewayFee,
				"net_amount", disbursement.NetAmount(),
				"available_balance", available,
			)
			return ledgererr.ErrInsufficientBalance.WithError(
				fmt.Errorf("requested: %d, available: %d", disbursement.Amount, available),
			)
		}

		return c.writeReservation(ctx, tx, account, disbursement)
	})
	if err != nil {
		// Insufficient balance is the caller's answer, not a database fault — pass it
		// through as-is so the API keeps returning a 4xx for it.
		if ledgererr.IsAppError(err, ledgererr.ErrInsufficientBalance) {
			return err
		}
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to reserve balance for withdrawal", err)
	}
	return nil
}

// writeReservation persists the journal, the PENDING disbursement and the debit that holds
// the money. Caller owns the transaction and any locking.
//
// The debit is the requested amount — the fee is inside it, not on top of it. A reversal,
// when one is written, reverses the same amount; the two must always agree, or releasing a
// refused payout would hand back more or less than was held.
func (c *LedgerClient) writeReservation(ctx context.Context, tx repo.Tx, account *domain.Account, disbursement *domain.Disbursement) error {
	journal := domain.NewJournal(
		domain.EventTypeDisbursement,
		domain.SourceTypeDisbursement,
		disbursement.UUID,
		map[string]any{
			// "amount" is the requested amount and the size of the debit. net_amount is
			// what the beneficiary receives. Both are recorded because the difference
			// between them is the only trace the fee leaves on the seller's books.
			"amount":       disbursement.Amount,
			"transfer_fee": disbursement.GatewayFee,
			"net_amount":   disbursement.NetAmount(),
			"bank_code":    disbursement.BankAccount.BankCode,
			"stage":        "RESERVED",
		},
	)
	if err := tx.Journal().Save(ctx, journal); err != nil {
		return err
	}
	if err := tx.Disbursement().Save(ctx, disbursement); err != nil {
		return err
	}
	return tx.LedgerEntry().Save(ctx, domain.NewDisbursementEntry(journal.UUID, disbursement.UUID, account.UUID, disbursement.Amount))
}

// RetryDisbursement resolves a payout that never reached a settled outcome.
//
// It asks before it sends. Singapay keys a payout on the reference_number this row already
// carries, and re-sending a used reference returns SP004 rather than paying twice — but
// SP004 is an error, not an answer, and the honest way to learn what happened is to
// inquire on the reference. So this inquires first: if Singapay knows the reference, its
// answer is booked and nothing is sent. Only a reference Singapay has never seen is
// actually transmitted.
//
// This is the reason payout_request_id exists. Without a caller that reuses the reference,
// storing it protects nothing.
//
// COMPLETED and FAILED are terminal and are refused: the first has already been paid and
// booked, the second is a definite refusal. Only PENDING (sent, outcome unknown) and
// PROCESSING (accepted, awaiting confirmation) may be resolved.
func (c *LedgerClient) RetryDisbursement(ctx context.Context, disbursementID string) (*WithdrawResponse, error) {
	disbursement, err := c.repoProvider.Disbursement().GetByID(ctx, disbursementID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrDisbursementNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to load disbursement", err)
	}

	return c.replay(ctx, disbursement)
}

// GetPendingDisbursement returns one disbursement by id, whatever its status.
//
// It exists so a caller deciding what to replay can say why a row is not eligible —
// "it is already COMPLETED" — instead of guessing, and so that deciding never requires
// SQL against the disbursements table from outside this package.
func (c *LedgerClient) GetPendingDisbursement(ctx context.Context, disbursementID string) (*domain.Disbursement, error) {
	disbursement, err := c.repoProvider.Disbursement().GetByID(ctx, disbursementID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrDisbursementNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to load disbursement", err)
	}

	return disbursement, nil
}

// GetPendingDisbursements lists PENDING disbursements across every account, created
// before the cutoff, oldest first.
//
// This is the ledger's answer to "which payouts never reached an outcome?". The question
// is about ledger state, so it is answered here rather than by a caller running its own
// SQL against a table this package owns.
//
// The cutoff is the caller's judgement about how long an in-flight payout may reasonably
// take: a row is written PENDING before the gateway is called, so a young one is not
// stuck.
func (c *LedgerClient) GetPendingDisbursements(ctx context.Context, cutoff time.Time, limit int) ([]*domain.Disbursement, error) {
	if limit <= 0 {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "limit must be greater than zero", nil)
	}

	disbursements, err := c.repoProvider.Disbursement().GetPendingOlderThan(ctx, cutoff, limit)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to list pending disbursements", err)
	}

	return disbursements, nil
}

// replay applies the eligibility rules to a disbursement already read from storage, then
// resolves it — by inquiry where possible, by re-sending only when Singapay has never seen
// the reference. Split out of RetryDisbursement only so the rules live in one place.
func (c *LedgerClient) replay(ctx context.Context, disbursement *domain.Disbursement) (*WithdrawResponse, error) {
	if !disbursement.IsPending() && !disbursement.IsProcessing() {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidDisbursementStatus,
			fmt.Sprintf("disbursement is %s and cannot be retried", disbursement.Status), nil)
	}

	// A row written before this feature existed has no reference to ask about. Minting
	// one now would be worse than refusing: it would look like a protected retry while
	// giving Singapay a reference it has never seen, so a payout that already went out
	// would go out again.
	if disbursement.PayoutRequestID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"disbursement has no stored payout reference; it predates idempotent retries and must be settled by hand", nil)
	}

	account, err := c.repoProvider.Account().GetByID(ctx, disbursement.LedgerUUID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account for disbursement retry", err)
	}

	// A row created before balance reservation existed carries no debit. executePayout
	// assumes one is already there and only ever writes reversals, so without this the
	// disbursement would settle as COMPLETED having never been deducted — money that left
	// the bank but never left the books.
	if err := c.ensureReserved(ctx, account, disbursement); err != nil {
		return nil, err
	}

	c.logger.InfoContext(ctx, "Resolving disbursement by inquiry before considering a re-send",
		"disbursement_id", disbursement.UUID,
		"status", disbursement.Status,
		"payout_reference", disbursement.PayoutRequestID,
	)

	known, err := c.gateway.InquiryDisbursement(ctx, account.SingapayAccountID, disbursement.PayoutRequestID)
	if err == nil && known != nil {
		c.logger.InfoContext(ctx, "Singapay already knows this payout reference — booking its answer instead of re-sending",
			"disbursement_id", disbursement.UUID,
			"payout_reference", disbursement.PayoutRequestID,
			"transaction_status", known.TransactionStatus(),
		)
		return c.bookPayoutOutcome(ctx, account, disbursement, known)
	}

	// Not found is the one answer that makes re-sending safe: Singapay has no record of
	// this reference, so nothing can have been paid under it. Any other failure leaves
	// the question open, and an open question is not a licence to send money again.
	if e, ok := singapay.AsError(err); ok && e.Code == singapay.CodeTransactionNotFound {
		c.logger.InfoContext(ctx, "Singapay has no record of this payout reference — sending it",
			"disbursement_id", disbursement.UUID,
			"payout_reference", disbursement.PayoutRequestID,
		)
		return c.executePayout(ctx, account, disbursement)
	}

	c.logger.WarnContext(ctx, "Payout inquiry gave no usable answer — leaving the disbursement in flight, balance stays reserved",
		"disbursement_id", disbursement.UUID,
		"payout_reference", disbursement.PayoutRequestID,
		"error", err,
	)
	return nil, ledgererr.ErrGatewayOutcomeUnknown.WithError(err)
}

// ensureReserved backfills the reservation for a disbursement that predates it.
//
// Deliberately no balance check. The money was committed to this payout when it was
// requested and may already be gone; refusing to record that would not bring it back, it
// would only leave the books claiming a balance the seller does not have. If the entry
// drives the available balance negative, that is the truth being stated, and it is logged
// so someone looks.
func (c *LedgerClient) ensureReserved(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement) error {
	entries, err := c.repoProvider.LedgerEntry().GetBySourceID(ctx, disbursement.UUID)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeInternal, "failed to check disbursement ledger entries", err)
	}
	for _, e := range entries {
		if e.EntryType == domain.EntryTypeDisbursement {
			return nil
		}
	}

	c.logger.WarnContext(ctx, "Disbursement has no reservation entry — backfilling before retry",
		"disbursement_id", disbursement.UUID,
		"account_id", account.UUID,
		"requested_amount", disbursement.Amount,
		"transfer_fee", disbursement.GatewayFee,
		"net_amount", disbursement.NetAmount(),
	)

	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		return c.writeReservation(ctx, tx, account, disbursement)
	})
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to backfill disbursement reservation", err)
	}
	return nil
}

// executePayout sends the payout and books whatever comes back.
//
// The disbursement row already exists when this runs, and the reservation debit is already
// in the ledger — written before the call went out. So the only question left is whether
// to give it back, and that question is answered by Outcome, never by the HTTP status.
func (c *LedgerClient) executePayout(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement) (*WithdrawResponse, error) {
	// The NET goes on the wire, never the requested amount. Singapay treats this field as
	// what the beneficiary receives and debits the sub-account this plus the fee, so
	// sending the net is what brings that debit to the requested amount — the very amount
	// already reserved. Sending the requested amount here would debit the sub-account
	// requested + fee and put the drift back.
	payoutReq := singapay.DisburseRequest{
		AccountID:         account.SingapayAccountID,
		ReferenceNumber:   disbursement.PayoutRequestID,
		BankCode:          disbursement.BankAccount.BankCode,
		BankAccountNumber: disbursement.BankAccount.AccountNumber,
		Amount:            disbursement.NetAmount(),
		Notes:             disbursement.UUID,
	}

	// The body is built before the log line, not after, so the line can carry it. A payout
	// Singapay refuses is argued over the body it received.
	c.logger.InfoContext(ctx, "Sending Singapay disbursement",
		"disbursement_id", disbursement.UUID,
		"account_id", account.UUID,
		"singapay_account_id", account.SingapayAccountID,
		// Empty is the failure worth spotting at a glance: the body still goes out with a
		// blank account_id, and Singapay answers with a not-found rather than a clear
		// validation error.
		"singapay_account_id_empty", account.SingapayAccountID == "",
		"requested_amount", disbursement.Amount,
		"transfer_fee", disbursement.GatewayFee,
		"net_amount", disbursement.NetAmount(),
		"payout_reference", disbursement.PayoutRequestID,
		"request_target", "/api/v2.0/disbursement/transfer",
		"request_body", payoutRequestLogBody(payoutReq),
	)

	result, gwErr := c.gateway.Disburse(ctx, payoutReq)
	if gwErr != nil {
		return nil, c.recordPayoutFailure(ctx, account, disbursement, gwErr)
	}

	return c.bookPayoutOutcome(ctx, account, disbursement, result)
}

// bookPayoutOutcome writes the ledger consequences of a payout Singapay has described,
// whether that description arrived from the transfer call, from an inquiry, or from a
// webhook. One code path for all three, so all three settle by identical rules.
//
// SP000 on the transfer meant the instruction was accepted, not that money moved. The
// payment outcome is the two-digit transaction status, and this is where it is read:
//
//	00           succeeded — complete, count the withdrawal
//	04, 05, 06, 07  terminally failed — release the reservation
//	01, 02, 03   still in flight — hold the reservation and wait
func (c *LedgerClient) bookPayoutOutcome(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement, result *singapay.Disbursement) (*WithdrawResponse, error) {
	status := result.TransactionStatus()

	c.logger.InfoContext(ctx, "Singapay disbursement outcome",
		"disbursement_id", disbursement.UUID,
		"transaction_status", status,
		"transaction_id", result.TransactionID,
		"failed_code", result.FailedCode,
		"failed_reason", result.FailedReason,
	)

	// If Singapay charges a fee other than the one quoted, the payout has already gone out
	// for a net computed from the quote, so the sub-account is debited net + actual fee
	// while the seller's balance moved by the requested amount. The difference lands on
	// the platform. It is recorded but not adjusted here: a compensating entry written
	// after the money has moved, on this path, is exactly the kind of quiet correction
	// that makes a balance impossible to explain afterwards.
	if actualFee, err := result.Fee.Rupiah(); err == nil && result.Fee.Set && actualFee != disbursement.GatewayFee {
		c.logger.WarnContext(ctx, "Singapay charged a different transfer fee than was quoted — the platform absorbs the difference",
			"disbursement_id", disbursement.UUID,
			"quoted_fee", disbursement.GatewayFee,
			"actual_fee", actualFee,
			"difference", actualFee-disbursement.GatewayFee,
			"requested_amount", disbursement.Amount,
			"net_sent", disbursement.NetAmount(),
		)
	}

	reverse := false
	switch {
	case status.Succeeded():
		_ = disbursement.MarkCompleted(result.TransactionID)
	case status.Terminal():
		reason := fmt.Sprintf("Singapay payout status %s", status)
		if result.FailedReason != "" {
			reason = fmt.Sprintf("%s: %s (%s)", reason, result.FailedReason, result.FailedCode)
		}
		c.logger.WarnContext(ctx, "Singapay payout terminally failed",
			"disbursement_id", disbursement.UUID,
			"transaction_status", status,
			"failed_code", result.FailedCode,
			"failed_reason", result.FailedReason,
		)
		_ = disbursement.MarkFailed(reason)
		// A terminal failure is a known outcome: no money left, so the reservation is
		// released.
		reverse = true
	default:
		// 01, 02, 03 — accepted and still moving. The reservation stays.
		_ = disbursement.MarkProcessing(result.TransactionID)
	}

	settlementJournal := domain.NewJournal(
		domain.EventTypeDisbursement,
		domain.SourceTypeDisbursement,
		disbursement.UUID,
		map[string]any{
			"amount":             disbursement.Amount,
			"transfer_fee":       disbursement.GatewayFee,
			"net_amount":         disbursement.NetAmount(),
			"bank_code":          disbursement.BankAccount.BankCode,
			"transaction_status": string(status),
			"transaction_id":     result.TransactionID,
			"stage":              "SETTLED",
		},
	)

	err := c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if err := tx.Journal().Save(ctx, settlementJournal); err != nil {
			return err
		}
		if err := tx.Disbursement().Save(ctx, disbursement); err != nil {
			return err
		}

		if reverse {
			// Reverses exactly what writeReservation held: the requested amount.
			if err := tx.LedgerEntry().Save(ctx, domain.NewDisbursementReversalEntry(settlementJournal.UUID, disbursement.UUID, account.UUID, disbursement.Amount)); err != nil {
				return err
			}
		}

		// If disbursement is COMPLETED, increment total_withdrawal_amount by what the
		// seller was actually charged — the requested amount, fee included.
		if disbursement.IsCompleted() {
			if err := tx.Account().IncrementWithdrawal(ctx, account.UUID, disbursement.Amount); err != nil {
				c.logger.WarnContext(ctx, "Failed to increment withdrawal amount",
					"account_id", account.UUID,
					"amount", disbursement.Amount,
					"error", err,
				)
				return err
			}
		}

		return nil
	})
	if err != nil {
		// Recoverable: the row and its reference were written ahead of the call, so
		// RetryDisbursement can inquire and book the same answer.
		c.logger.ErrorContext(ctx, "CRITICAL: Singapay answered but the ledger write failed — resolve with RetryDisbursement, which inquires rather than re-sends",
			"disbursement_id", disbursement.UUID,
			"payout_reference", disbursement.PayoutRequestID,
			"transaction_status", status,
			"transaction_id", result.TransactionID,
			"amount", disbursement.Amount,
			"account_id", account.UUID,
			"error", err,
		)
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save disbursement after gateway answered", err)
	}

	c.logger.InfoContext(ctx, "Withdrawal booked",
		"disbursement_id", disbursement.UUID,
		"status", disbursement.Status,
		"requested_amount", disbursement.Amount,
		"transfer_fee", disbursement.GatewayFee,
		"net_amount", disbursement.NetAmount(),
	)

	return &WithdrawResponse{
		DisbursementID: disbursement.UUID,
		Status:         string(disbursement.Status),
		Amount:         disbursement.Amount,
		TransferFee:    disbursement.GatewayFee,
		NetAmount:      disbursement.NetAmount(),
		Currency:       string(disbursement.Currency),
		Message:        fmt.Sprintf("Payout %s", status),
	}, nil
}

// payoutRequestLogBody renders the payout request as the JSON body Singapay will receive.
//
// The individual fields are logged beside it, but a payout Singapay rejects is disputed
// over the body it was sent, not over our field names. This marshals the very struct the
// client marshals, so the log holds what went on the wire — reproduced verbatim, empty
// account_id included.
//
// The one departure is the beneficiary account number, masked to its last four digits.
// These logs are shipped off-process; a full account number does not need to travel with
// them to answer the question a log line is asked, which is which of a seller's accounts
// this went to.
//
// req is taken by value: the mask must not touch what is about to be sent.
func payoutRequestLogBody(req singapay.DisburseRequest) string {
	req.BankAccountNumber = maskAccountNumber(req.BankAccountNumber)

	body, err := json.Marshal(req)
	if err != nil {
		// A struct of strings and ints cannot fail to marshal, but say so if it somehow
		// does rather than returning an empty body that reads as one that was sent empty.
		return fmt.Sprintf("<could not render payout body: %v>", err)
	}
	return string(body)
}

// maskAccountNumber keeps the last four digits of a bank account number and replaces
// everything before them.
//
// Four is enough to tell two of a seller's accounts apart, which is all a log line is
// asked. Anything four digits or shorter is masked whole instead of partially, because
// "1234" masked to "1234" is not a mask.
func maskAccountNumber(accountNumber string) string {
	const visible = 4

	if len(accountNumber) <= visible {
		return strings.Repeat("*", len(accountNumber))
	}
	return strings.Repeat("*", len(accountNumber)-visible) + accountNumber[len(accountNumber)-visible:]
}

// recordPayoutFailure decides what a failed money-out call means for the row, and it turns
// on one question: do we know the payout did not happen?
//
// Singapay answers that question itself, through Outcome. Deciding it from the HTTP status
// instead is the trap the previous gateway's code fell into: Singapay returns HTTP 400 for
// SP001, SP002, SP004 and SP005, and its own documentation says to call inquiry-status for
// every one of them, because the transfer may still settle. Releasing the reservation on a
// 4xx is precisely how a payout gets made twice.
//
//	OutcomeRefused    Singapay declined before moving anything. FAILED, reservation released.
//	OutcomeDuplicate  This reference already exists — the original may well have succeeded.
//	                  Left in flight; RetryDisbursement resolves it by inquiry.
//	OutcomeUnknown    No answer, or an answer that says nothing about the money. Left in
//	                  flight with the reservation held, which is the only state from which
//	                  the truth can still be recovered.
func (c *LedgerClient) recordPayoutFailure(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement, gwErr error) error {
	outcome := singapay.OutcomeUnknown
	var code singapay.ResponseCode
	var statusCode int
	if e, ok := singapay.AsError(gwErr); ok {
		outcome = e.Outcome()
		code = e.Code
		statusCode = e.StatusCode
	}

	c.logger.ErrorContext(ctx, "Singapay disbursement failed",
		"disbursement_id", disbursement.UUID,
		"payout_reference", disbursement.PayoutRequestID,
		"response_code", string(code),
		"status_code", statusCode,
		"outcome", outcome.String(),
		"error", gwErr,
	)

	if outcome != singapay.OutcomeRefused {
		// Left in flight on purpose, and the reservation stays with it. The money may be
		// on its way; handing it back to the available balance is precisely how it would
		// be withdrawn a second time. PENDING plus a stored reference is the only state
		// from which the truth can still be recovered.
		c.logger.WarnContext(ctx, "Payout outcome not known to be a refusal — disbursement left in flight, balance stays reserved",
			"disbursement_id", disbursement.UUID,
			"payout_reference", disbursement.PayoutRequestID,
			"outcome", outcome.String(),
			"requested_amount", disbursement.Amount,
			"net_amount", disbursement.NetAmount(),
		)
		return ledgererr.ErrGatewayOutcomeUnknown.WithError(
			fmt.Errorf("outcome %s (code %s, http %d): %w", outcome, code, statusCode, gwErr))
	}

	// Known refusal: no money left, so the reservation goes back to the available balance
	// and the row reaches its terminal state. What goes back is the requested amount,
	// because that is what writeReservation held.
	if err := disbursement.MarkFailed(fmt.Sprintf("Singapay refused the payout (%s): %v", code, gwErr)); err == nil {
		reversalJournal := domain.NewJournal(
			domain.EventTypeDisbursement,
			domain.SourceTypeDisbursement,
			disbursement.UUID,
			map[string]any{
				"amount":        disbursement.Amount,
				"transfer_fee":  disbursement.GatewayFee,
				"net_amount":    disbursement.NetAmount(),
				"bank_code":     disbursement.BankAccount.BankCode,
				"response_code": string(code),
				"stage":         "REVERSED",
			},
		)

		saveErr := c.txProvider.Transact(ctx, func(tx repo.Tx) error {
			if err := tx.Journal().Save(ctx, reversalJournal); err != nil {
				return err
			}
			if err := tx.Disbursement().Save(ctx, disbursement); err != nil {
				return err
			}
			return tx.LedgerEntry().Save(ctx, domain.NewDisbursementReversalEntry(reversalJournal.UUID, disbursement.UUID, account.UUID, disbursement.Amount))
		})
		if saveErr != nil {
			// The seller's money is held by a reservation that no longer corresponds to
			// anything. Loud, because only a human can put this right.
			c.logger.ErrorContext(ctx, "CRITICAL: payout refused but the reservation could not be released — balance is understated",
				"disbursement_id", disbursement.UUID,
				"account_id", account.UUID,
				"reserved_amount", disbursement.Amount,
				"error", saveErr,
			)
		}
	}

	return ledgererr.NewError(ledgererr.CodeGatewayAPIError, "Singapay refused the disbursement", gwErr)
}

type EarningsResponse struct {
	PendingTransactions []*domain.ProductTransaction `json:"pending_transactions"`
	SettledTransactions []*domain.ProductTransaction `json:"settled_transactions"`
	NextCursor          string                       `json:"next_cursor,omitempty"` // RandId for next page (empty if no more)
	HasMore             bool                         `json:"has_more"`              // True if more results available
}

// DisbursementsResponse contains paginated disbursement history
type DisbursementsResponse struct {
	Disbursements []*domain.Disbursement `json:"disbursements"`
	NextCursor    string                 `json:"next_cursor,omitempty"` // RandId for next page (empty if no more)
	HasMore       bool                   `json:"has_more"`              // True if more results available
}

// GetEarnings returns pending (COMPLETED) and settled (SETTLED) transactions for a seller
// with cursor-based pagination using RandId (mimicking redifu's infinite scroll pattern).
// Pass empty cursor string to get first page.
// sortOrder: "ASC" or "DESC" for created_at ordering (defaults to DESC)
func (c *LedgerClient) GetEarnings(ctx context.Context, sellerID string, cursor string, pageSize int, sortOrder string) (*EarningsResponse, error) {
	account, err := c.repoProvider.Account().GetBySellerID(ctx, sellerID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by seller ID", err)
	}

	// Default page size if not specified
	if pageSize <= 0 {
		pageSize = 20
	}

	// Default sort order
	if sortOrder != "ASC" && sortOrder != "DESC" {
		sortOrder = "DESC"
	}

	// Fetch one extra to determine if there are more results
	transactions, err := c.repoProvider.ProductTransaction().GetBySellerAccountIDWithCursor(ctx, account.Record.UUID, cursor, pageSize+1, sortOrder)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get transactions", err)
	}

	resp := &EarningsResponse{
		PendingTransactions: []*domain.ProductTransaction{},
		SettledTransactions: []*domain.ProductTransaction{},
	}

	// Check if there are more results
	hasMore := len(transactions) > pageSize
	if hasMore {
		// Remove the extra item used for hasMore check
		transactions = transactions[:pageSize]
	}

	// Separate by status
	for _, tx := range transactions {
		switch tx.Status {
		case domain.TransactionStatusCompleted:
			resp.PendingTransactions = append(resp.PendingTransactions, tx)
		case domain.TransactionStatusSettled:
			resp.SettledTransactions = append(resp.SettledTransactions, tx)
		}
	}

	// Set pagination info
	resp.HasMore = hasMore
	if hasMore && len(transactions) > 0 {
		// Next cursor is the RandId of the last item
		resp.NextCursor = transactions[len(transactions)-1].Record.RandId
	}

	return resp, nil
}

// GetDisbursements returns disbursement history for a seller account
// with cursor-based pagination using RandId (mimicking redifu's infinite scroll pattern).
// Pass empty cursor string to get first page.
// sortOrder: "ASC" or "DESC" for created_at ordering (defaults to DESC)
func (c *LedgerClient) GetDisbursements(ctx context.Context, sellerID string, cursor string, pageSize int, sortOrder string) (*DisbursementsResponse, error) {
	account, err := c.repoProvider.Account().GetBySellerID(ctx, sellerID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by seller ID", err)
	}

	// Default page size if not specified
	if pageSize <= 0 {
		pageSize = 20
	}

	// Default sort order
	if sortOrder != "ASC" && sortOrder != "DESC" {
		sortOrder = "DESC"
	}

	// Fetch one extra to determine if there are more results
	disbursements, err := c.repoProvider.Disbursement().GetByAccountIDWithCursor(ctx, account.Record.UUID, cursor, pageSize+1, sortOrder)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get disbursements", err)
	}

	resp := &DisbursementsResponse{
		Disbursements: []*domain.Disbursement{},
	}

	// Check if there are more results
	hasMore := len(disbursements) > pageSize
	if hasMore {
		// Remove the extra item used for hasMore check
		disbursements = disbursements[:pageSize]
	}

	resp.Disbursements = disbursements
	resp.HasMore = hasMore
	if hasMore && len(disbursements) > 0 {
		// Next cursor is the RandId of the last item
		resp.NextCursor = disbursements[len(disbursements)-1].Record.RandId
	}

	return resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Platform Fee Transfer (Background Job / Post-Reconciliation)
// ─────────────────────────────────────────────────────────────────────────────

// PlatformFeeTransferResult contains the results of a platform fee transfer batch
type PlatformFeeTransferResult struct {
	Succeeded int                          `json:"succeeded"`
	Failed    int                          `json:"failed"`
	Errors    []PlatformFeeTransferError   `json:"errors,omitempty"`
	Transfers []PlatformFeeTransferSuccess `json:"transfers,omitempty"`
}

// PlatformFeeTransferError contains error details for a failed transfer.
//
// PlatformFeeMinor is in sen, like every settled fee figure: the amount a sweep moves is
// the platform fee after it has balanced the gateway fee delta, and that is routinely not
// a whole rupiah.
type PlatformFeeTransferError struct {
	TransactionID    string `json:"transaction_id"`
	InvoiceNumber    string `json:"invoice_number"`
	PlatformFeeMinor int64  `json:"platform_fee_minor"`
	ErrorMessage     string `json:"error_message"`
}

// PlatformFeeTransferSuccess contains details for a successful transfer.
// PlatformFeeMinor is in sen — see [PlatformFeeTransferError].
type PlatformFeeTransferSuccess struct {
	TransactionID    string `json:"transaction_id"`
	InvoiceNumber    string `json:"invoice_number"`
	PlatformFeeMinor int64  `json:"platform_fee_minor"`
	FromSubAccount   string `json:"from_sub_account"`
	ToSubAccount     string `json:"to_sub_account"`
}

// ProcessPlatformFeeTransfer moves the platform's share out of seller sub-accounts and
// into the platform sub-account, for settled transactions that have not had it moved yet.
//
// Its input is transactions in SETTLED status, which the settling pass produces — see
// settlement.go. Nothing transfers a fee inline any more, so every settled transaction's
// platform fee waits for this sweep and for nothing else.
//
// This should be called:
//   - After a settlement pass, which is what creates the work
//   - As a periodic background job (e.g. every 5 minutes)
//
// Two Singapay facts shape it.
//
// The transfer names its destination by ACCOUNT NUMBER and by nothing else — the ULID is
// rejected there, even though every other endpoint in the API takes the ULID. A platform
// account stored without a number cannot be transferred into at all, which is why that is
// checked once, up front, rather than discovered per-row.
//
// And merchant_ref_no is the idempotency key, unique per merchant across every account
// movement: re-sending a used reference returns the original transfer and moves nothing
// further. The reference is derived from the invoice number, so a retry after a timeout is
// safe by construction — which is the whole reason it must never be random.
//
// Parameters:
//   - batchSize: maximum number of transactions to process in one call (recommended: 50-100)
func (c *LedgerClient) ProcessPlatformFeeTransfer(ctx context.Context, batchSize int) (*PlatformFeeTransferResult, error) {
	if batchSize <= 0 {
		batchSize = 50 // Default batch size
	}

	result := &PlatformFeeTransferResult{
		Errors:    []PlatformFeeTransferError{},
		Transfers: []PlatformFeeTransferSuccess{},
	}

	// Get platform account for transfers
	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.NewError(ledgererr.CodeInternal, "platform account not found", err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get platform account", err)
	}

	// Checked here rather than per row: without a number every transfer in the batch
	// fails identically, and the fix is one configuration change, not N retries.
	if !platformAccount.CanReceiveTransfer() {
		return nil, ledgererr.NewError(ledgererr.CodeInternal,
			"platform account has no Singapay account number; an account transfer names its destination by number and rejects the ULID", nil)
	}

	// Fetch settled transactions that need platform fee transfer
	transactions, err := c.repoProvider.ProductTransaction().GetSettledWithoutPlatformFeeTransfer(ctx, batchSize)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to query transactions needing platform fee transfer", err)
	}

	if len(transactions) == 0 {
		c.logger.InfoContext(ctx, "No transactions requiring platform fee transfer")
		return result, nil
	}

	c.logger.InfoContext(ctx, "Processing platform fee transfers",
		"batch_size", batchSize,
		"transactions_found", len(transactions),
		"platform_account_id", platformAccount.UUID,
		"platform_singapay_account_number", platformAccount.SingapayAccountNumber,
	)

	for _, tx := range transactions {
		// The amount to move is what the platform actually earned, which is not always
		// what was priced at checkout. The platform sub-account balances the difference
		// between the gateway fee quoted at checkout and the one Singapay really took, in
		// both directions, so settlement can book a platform fee either side of the quoted
		// one — and moving the quoted figure would leave the seller's sub-account holding
		// an amount the ledger never agreed with.
		//
		// This is in SEN and is transferred as a decimal, because the balanced figure is
		// routinely not a whole rupiah. Rounding it here would strand the fraction in the
		// seller's sub-account and reintroduce exactly the drift the balancing removes.
		//
		// SettledPlatformFeeMinor is nil for transactions that settled before it was
		// recorded. Those were transferred under the old rule and their money has already
		// moved, so the priced figure is the right fallback. nil means "not recorded",
		// never zero.
		platformFeeMinor := domain.RupiahToMinor(tx.Fee.PlatformFee)
		if tx.SettledPlatformFeeMinor != nil {
			platformFeeMinor = *tx.SettledPlatformFeeMinor
		}

		if platformFeeMinor <= 0 {
			c.logger.WarnContext(ctx, "Transaction has zero platform fee, skipping",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"priced_platform_fee", tx.Fee.PlatformFee,
				"settled_platform_fee_recorded", tx.SettledPlatformFeeMinor != nil,
			)
			continue
		}

		// Singapay rejects a transfer below 1 rupiah. A balanced platform fee that lands
		// under it is left for a person rather than retried forever: the row keeps its
		// untransferred flag, so it stays in GetSettledWithoutPlatformFeeTransfer and stays
		// counted, instead of being marked done with the money still in the seller's
		// sub-account.
		if platformFeeMinor < domain.MinorPerRupiah {
			result.recordFailure(tx, fmt.Sprintf(
				"settled platform fee %s is below the Rp1 minimum Singapay accepts for a transfer",
				formatMinor(platformFeeMinor)))
			c.logger.WarnContext(ctx, "Platform fee transfer skipped - below the Rp1 gateway minimum",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"settled_platform_fee_minor", platformFeeMinor,
				"priced_platform_fee", tx.Fee.PlatformFee,
			)
			continue
		}

		sellerAccount, err := c.repoProvider.Account().GetByID(ctx, tx.SellerAccountID)
		if err != nil {
			result.recordFailure(tx, fmt.Sprintf("failed to get seller account: %v", err))
			c.logger.ErrorContext(ctx, "Platform fee transfer failed - seller account not found",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"seller_account_id", tx.SellerAccountID,
				"error", err,
			)
			continue
		}

		if sellerAccount.SingapayAccountID == "" {
			result.recordFailure(tx, "seller account has no Singapay sub-account id")
			c.logger.ErrorContext(ctx, "Platform fee transfer failed - invalid seller account",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"seller_account_id", tx.SellerAccountID,
			)
			continue
		}

		// The reference is derived from the invoice, not generated, so a retry presents
		// the same one. It is still persisted: the stored value is what proves which
		// reference a given row went out under if the derivation ever changes.
		reference := platformFeeTransferReference(tx.InvoiceNumber)
		if tx.TransferRequestID != reference {
			if err := c.repoProvider.ProductTransaction().SaveTransferRequestID(ctx, tx.UUID, reference); err != nil {
				result.recordFailure(tx, fmt.Sprintf("failed to save transfer reference: %v", err))
				c.logger.ErrorContext(ctx, "Platform fee transfer failed - could not persist transfer reference",
					"transaction_id", tx.UUID,
					"invoice_number", tx.InvoiceNumber,
					"error", err,
				)
				continue
			}
		}

		_, gwErr := c.gateway.TransferBetweenAccounts(ctx, sellerAccount.SingapayAccountID, singapay.TransferRequest{
			Amount:                   singapay.NewAmountFromMinor(platformFeeMinor, string(domain.CurrencyIDR)),
			BeneficiaryAccountNumber: platformAccount.SingapayAccountNumber,
			MerchantRefNo:            reference,
		})
		if gwErr != nil {
			result.recordFailure(tx, fmt.Sprintf("Singapay account transfer failed: %v", gwErr))
			c.logger.ErrorContext(ctx, "Platform fee transfer failed - Singapay API error",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"platform_fee", formatMinor(platformFeeMinor),
				"from_account", sellerAccount.SingapayAccountID,
				"to_account_number", platformAccount.SingapayAccountNumber,
				"merchant_ref_no", reference,
				"error", gwErr,
			)
			continue
		}

		if err := c.repoProvider.ProductTransaction().MarkPlatformFeeTransferred(ctx, tx.UUID); err != nil {
			// Not lost: the next run re-sends the same merchant_ref_no, Singapay returns
			// the original transfer without moving anything, and the flag is set then.
			c.logger.ErrorContext(ctx, "Singapay transfer succeeded but the DB update failed - the next run will re-present the same reference",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"platform_fee", formatMinor(platformFeeMinor),
				"merchant_ref_no", reference,
				"db_error", err,
			)
			result.recordFailure(tx, fmt.Sprintf("Singapay transfer succeeded but DB update failed: %v", err))
			continue
		}

		c.logger.InfoContext(ctx, "Platform fee transferred successfully",
			"transaction_id", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
			"platform_fee", formatMinor(platformFeeMinor),
			"from_account", sellerAccount.SingapayAccountID,
			"to_account_number", platformAccount.SingapayAccountNumber,
		)
		result.Succeeded++
		result.Transfers = append(result.Transfers, PlatformFeeTransferSuccess{
			TransactionID:    tx.UUID,
			InvoiceNumber:    tx.InvoiceNumber,
			PlatformFeeMinor: platformFeeMinor,
			FromSubAccount:   sellerAccount.SingapayAccountID,
			ToSubAccount:     platformAccount.SingapayAccountNumber,
		})
	}

	c.logger.InfoContext(ctx, "Platform fee transfer batch completed",
		"total_processed", len(transactions),
		"succeeded", result.Succeeded,
		"failed", result.Failed,
	)

	return result, nil
}

// recordFailure appends one failed transfer. The four call sites above all built the same
// struct by hand and differed only in the message, which is exactly the kind of repetition
// that lets one of them quietly forget to increment the counter.
func (r *PlatformFeeTransferResult) recordFailure(tx *domain.ProductTransaction, message string) {
	r.Failed++
	// The settled figure where it exists, the priced one where it does not — the same
	// fallback the sweep itself makes, so a failure report names the amount that was
	// actually attempted rather than the one from checkout.
	platformFeeMinor := domain.RupiahToMinor(tx.Fee.PlatformFee)
	if tx.SettledPlatformFeeMinor != nil {
		platformFeeMinor = *tx.SettledPlatformFeeMinor
	}

	r.Errors = append(r.Errors, PlatformFeeTransferError{
		TransactionID:    tx.UUID,
		InvoiceNumber:    tx.InvoiceNumber,
		PlatformFeeMinor: platformFeeMinor,
		ErrorMessage:     message,
	})
}
