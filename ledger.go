package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	dokumodels "github.com/21strive/doku/app/models"
	"github.com/21strive/doku/app/requests"
	"github.com/21strive/doku/app/usecases"
	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/ledgererr"
	"github.com/21strive/ledger/repo"
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
	dokuClient   usecases.DokuUseCaseInterface
}

// NewLedgerClient takes no object-storage handle: this package keeps ledgers, and
// nothing it does touches a bucket. It used to accept an aws.Config purely to build
// an S3 client for the seller KYC upload helpers, which have moved to the service
// that owns KYC.
func NewLedgerClient(db *sql.DB, dokuClient usecases.DokuUseCaseInterface, logger *slog.Logger) *LedgerClient {
	txProvider := repo.NewTransactionProvider(db)
	repoProvider := repo.NewRepositoryProvider(db)

	return &LedgerClient{
		db:           db,
		txProvider:   txProvider,
		logger:       logger,
		dokuClient:   dokuClient,
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

// GetAccountByDokuSubAccountID returns an account by its DOKU sub-account ID.
func (c *LedgerClient) GetAccountByDokuSubAccountID(ctx context.Context, dokuSubAccountID string) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetByDokuSubAccountID(ctx, dokuSubAccountID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get account by doku sub account ID", err)
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

// CreateAccount provisions a DOKU sub-account and persists an Account record.
// Idempotent: if an account for accountID already exists, returns ErrLedgerAlreadyExists.
func (c *LedgerClient) CreateAccount(ctx context.Context, accountID string, email, name string, currency domain.Currency) (*domain.Account, error) {
	// Check for existing account
	existing, err := c.repoProvider.Account().GetByOwner(ctx, domain.OwnerTypeSeller, accountID)
	if err == nil {
		c.logger.InfoContext(ctx, "Account already exists for owner ID", "owner_id", accountID, "account_id", existing.UUID)
		return nil, ledgererr.ErrLedgerAlreadyExists
	} else if !ledgererr.IsErrorCode(ledgererr.CodeNotFound, err) {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to check existing account", err)
	}

	// DOKU constrains both fields (email max 40 chars, name alphabetic only, max 100).
	// Neither is checked anywhere upstream, and this call sits inside the seller's first
	// paid booking — the worst possible place to discover a 4xx from an unusual name.
	sanitizedName := sanitizeSubAccountName(name)
	if err := validateSubAccountEmail(email); err != nil {
		return nil, err
	}
	if sanitizedName != name {
		c.logger.InfoContext(ctx, "Sub-account name sanitized for DOKU", "owner_id", accountID, "original", name, "sanitized", sanitizedName)
	}

	// Provision DOKU sub-account
	response, dokuErr := c.dokuClient.CreateAccount(&requests.DokuCreateSubAccountRequest{
		Email: email,
		Name:  sanitizedName,
	})
	c.logger.DebugContext(ctx, "DOKU CreateAccount response", "response", response, "error", dokuErr)

	// Any DOKU failure ends this, 409 included. A 409 means the email already owns a
	// sub-account, and the SAC ID in that message is deliberately not reused: it may
	// belong to a different user, and a sub-account holds money. The raw DOKU message
	// reaches the log, so an admin can bind the account by hand after checking who owns
	// it. See T6b/T6c in docs/doku-sac-api-compliance.md (aturjadwal-monoservice).
	if dokuErr != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDokuAPIError,
			"failed to create DOKU sub account",
			fmt.Errorf("Status Code: %d, Error: %v: %v", dokuErr.StatusCode, dokuErr.Err, dokuErr.Message))
	}
	dokuSubAccountID := response.ID.String

	account := domain.NewSellerAccount(dokuSubAccountID, accountID, currency)
	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if err := tx.Account().Save(ctx, &account); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to create account", err)
		}
		return nil
	})
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "transaction failed while creating account", err)
	}

	return &account, nil
}

// The platform account is deliberately not creatable from here. It used to be:
// CreatePlatformAccount provisioned a DOKU sub-account under whatever email the caller
// passed, and callers invoked it lazily from the booking path. That made the account that
// collects every platform fee a side effect of somebody else's booking — and since DOKU
// binds a sub-account to an email permanently, a caller with a stale constant would
// quietly open a *second* sub-account and route the fees into one nobody watches, without
// raising a single error.
//
// Provisioning is now a deliberate, once-per-environment act: run
// scripts/doku-subaccount in aturjadwal-monoservice to create the sub-account at DOKU,
// then insert the ledger_accounts row by hand (T6c runbook / T9 in
// docs/doku-sac-api-compliance.md). Readers stay: GetPlatformAccount is what
// ProcessPlatformFeeTransfer and the reconciliation path use, and both already refuse to
// run when the account is missing or has no sub-account id — which is now a real safety
// net rather than a formality.

// CreatePaymentGatewayAccount creates a PAYMENT_GATEWAY-type account (singleton, no DOKU sub-account).
func (c *LedgerClient) CreatePaymentGatewayAccount(ctx context.Context, currency domain.Currency) (*domain.Account, error) {
	ownerID := "DOKU"
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
// This is a pure read from ledger_accounts — no DOKU sync.
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
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"` // Optional: for verification
}

// ValidateBankAccountResponse contains the result of bank account validation
type ValidateBankAccountResponse struct {
	IsValid       bool   `json:"is_valid"`
	BankCode      string `json:"bank_code"`
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"` // From DOKU response
}

// ValidateBankAccount validates a bank account with DOKU
func (c *LedgerClient) ValidateBankAccount(ctx context.Context, req *ValidateBankAccountRequest) (*ValidateBankAccountResponse, error) {
	if req.BankCode == "" || req.AccountNumber == "" {
		return nil, ledgererr.ErrInvalidBankAccount.WithError(fmt.Errorf("bank_code and account_number are required"))
	}

	tokenResp, tokenErr := c.dokuClient.GetToken()
	if tokenErr != nil {
		c.logger.ErrorContext(ctx, "Failed to get DOKU access token",
			"error", tokenErr.Err,
			"message", tokenErr.Message,
		)
		return nil, ledgererr.NewError(ledgererr.CodeDokuAPIError, "failed to get DOKU access token", fmt.Errorf("%v", tokenErr.Message))
	}

	dokuReq := &requests.DokuBankAccountInquiryRequest{
		BeneficiaryAccountNumber: req.AccountNumber,
	}
	dokuReq.AdditionalInfo.BeneficiaryBankCode = req.BankCode
	dokuReq.AdditionalInfo.BeneficiaryAccountName = req.AccountName

	resp, dokuErr := c.dokuClient.BankAccountInquiry(dokuReq, tokenResp.AccessToken)
	if dokuErr != nil {
		c.logger.ErrorContext(ctx, "DOKU BankAccountInquiry failed",
			"bank_code", req.BankCode,
			"account_number", req.AccountNumber,
			"error", dokuErr.Err,
			"message", dokuErr.Message,
			"status_code", dokuErr.StatusCode,
		)
		return &ValidateBankAccountResponse{
			IsValid:       false,
			BankCode:      req.BankCode,
			AccountNumber: req.AccountNumber,
		}, nil
	}

	return &ValidateBankAccountResponse{
		IsValid:       true,
		BankCode:      resp.BeneficiaryBankCode,
		BankName:      resp.BeneficiaryBankName,
		AccountNumber: resp.BeneficiaryAccountNumber,
		AccountName:   resp.BeneficiaryAccountName,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Withdrawal (Disbursement)
// ─────────────────────────────────────────────────────────────────────────────

// WithdrawRequest contains the parameters to withdraw funds to a bank account
type WithdrawRequest struct {
	AccountID     string `json:"account_id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
	Description   string `json:"description"`
}

// WithdrawResponse contains the result of a withdrawal request
type WithdrawResponse struct {
	DisbursementID string `json:"disbursement_id"`
	Status         string `json:"status"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	Message        string `json:"message"`
}

// Withdraw initiates a withdrawal from an account to an external bank account.
// Flow:
//  1. Look up Account by sellerID (owner_id)
//  2. Reserve: under a row lock, check the available balance and — in the same
//     transaction — write the journal, the PENDING Disbursement carrying the DOKU
//     Request-Id, and the debit that holds the money
//  3. Call DOKU SendPayoutSubAccount under that Request-Id
//  4. Book the answer: complete it, or release the reservation if the payout is known
//     not to have happened
//
// Two things used to go wrong here, and they are different problems with different fixes.
//
// The DOKU call came first, before any DB write. Payout succeeds, process dies before the
// commit, and the money is gone with only a log line naming it — and no stored Request-Id,
// so it could never be asked about again, only judged by hand. Writing the row and its id
// first makes that recoverable: see RetryDisbursement.
//
// And the balance was checked but never reserved, so two *different* withdrawals racing
// each other both passed the check and both paid out. Idempotency does not help there —
// each is a distinct payout with its own Request-Id. Only holding the money at request
// time does, which is what step 2 is.
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

	// Generate disbursement ID upfront (used as DOKU invoice number)
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

	// The Request-Id this payout will travel under, stored with the row so a retry can
	// present the same one and get DOKU's original answer instead of a second payout.
	disbursement.PayoutRequestID = uuid.NewString()

	if err := c.reserveBalance(ctx, account, disbursement); err != nil {
		return nil, err
	}

	return c.executePayout(ctx, account, disbursement)
}

// reserveBalance takes the money out of the available balance and records the intent,
// atomically, before anything is sent to DOKU.
//
// The lock is the part that matters. Balances are derived by summing ledger_entries, so
// without it two concurrent withdrawals both read the balance before either has written
// its debit, both find it sufficient, and both pay out. Locking the account row first
// makes the second one wait until the first's entries are committed, so it reads a
// balance that already accounts for them.
//
// The balance check therefore lives inside this transaction. Checking outside and writing
// inside would be the same race with extra steps.
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
func (c *LedgerClient) writeReservation(ctx context.Context, tx repo.Tx, account *domain.Account, disbursement *domain.Disbursement) error {
	journal := domain.NewJournal(
		domain.EventTypeDisbursement,
		domain.SourceTypeDisbursement,
		disbursement.UUID,
		map[string]any{
			"amount":    disbursement.Amount,
			"bank_code": disbursement.BankAccount.BankCode,
			"stage":     "RESERVED",
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

// RetryDisbursement re-sends a payout that never reached a settled outcome, reusing the
// Request-Id stored on the row. Because DOKU keys idempotency on that id, this is safe to
// call even when the first attempt did succeed: DOKU replays its original answer under a
// 409 rather than paying again, and the ledger is then written from that answer.
//
// This is the reason payout_request_id exists. Without a caller that reuses the id, storing
// it protects nothing.
//
// COMPLETED and FAILED are terminal and are refused: the first has already been paid and
// booked, the second is a definite refusal from DOKU. Only PENDING (sent, outcome unknown)
// and PROCESSING (accepted, awaiting confirmation) may be replayed.
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
// take: a row is written PENDING before DOKU is called, so a young one is not stuck.
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
// re-sends it. Split out of RetryDisbursement only so the rules live in one place.
func (c *LedgerClient) replay(ctx context.Context, disbursement *domain.Disbursement) (*WithdrawResponse, error) {
	if !disbursement.IsPending() && !disbursement.IsProcessing() {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidDisbursementStatus,
			fmt.Sprintf("disbursement is %s and cannot be retried", disbursement.Status), nil)
	}

	// A row written before this feature existed has no id to replay. Minting one now would
	// be worse than refusing: it would look like a protected retry while giving DOKU a key
	// it has never seen, so a payout that already went out would go out again.
	if disbursement.PayoutRequestID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"disbursement has no stored payout request id; it predates idempotent retries and must be settled by hand", nil)
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

	c.logger.InfoContext(ctx, "Retrying disbursement with stored request id",
		"disbursement_id", disbursement.UUID,
		"status", disbursement.Status,
		"payout_request_id", disbursement.PayoutRequestID,
	)

	return c.executePayout(ctx, account, disbursement)
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
		"amount", disbursement.Amount,
	)

	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		return c.writeReservation(ctx, tx, account, disbursement)
	})
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to backfill disbursement reservation", err)
	}
	return nil
}

// executePayout sends the payout to DOKU and books whatever comes back. It is shared by the
// first attempt and by every replay, so both go out under the same Request-Id and are
// settled by identical rules.
//
// The disbursement row already exists when this runs.
func (c *LedgerClient) executePayout(ctx context.Context, account *domain.Account, disbursement *domain.Disbursement) (*WithdrawResponse, error) {
	dokuReq := requests.DokuSendPayoutSubAccountRequest{}
	dokuReq.Account.ID = account.DokuSubAccountID
	dokuReq.Payout.Amount = int(disbursement.Amount)
	dokuReq.Payout.InvoiceNumber = disbursement.UUID
	dokuReq.Beneficiary.BankCode = disbursement.BankAccount.BankCode
	dokuReq.Beneficiary.BankAccountNumber = disbursement.BankAccount.AccountNumber
	dokuReq.Beneficiary.BankAccountName = disbursement.BankAccount.AccountName

	// The body is built before the log line, not after, so the line can carry it. A payout
	// DOKU refuses is argued over the body it received, and until now the log described the
	// call in our own field names and left the beneficiary out entirely.
	c.logger.InfoContext(ctx, "Calling DOKU SendPayoutSubAccount",
		"disbursement_id", disbursement.UUID,
		"account_id", account.UUID,
		"doku_sub_account_id", account.DokuSubAccountID,
		// Empty is the failure worth spotting at a glance: the body still goes out with a
		// blank account.id, and DOKU answers "Request or data not found".
		"doku_sub_account_id_empty", account.DokuSubAccountID == "",
		"amount", disbursement.Amount,
		"payout_request_id", disbursement.PayoutRequestID,
		"request_target", "/sac-merchant/v1/payouts",
		"request_body", payoutRequestLogBody(dokuReq),
	)

	dokuResp, dokuErr := c.dokuClient.SendPayoutSubAccount(disbursement.PayoutRequestID, dokuReq)
	if dokuErr != nil {
		return nil, c.recordPayoutFailure(ctx, disbursement, dokuErr)
	}

	c.logger.InfoContext(ctx, "DOKU disbursement response received",
		"disbursement_id", disbursement.UUID,
		"doku_status", dokuResp.Payout.Status,
		"doku_invoice", dokuResp.Payout.InvoiceNumber,
	)

	// The debit is already in the ledger — it was written when the balance was reserved,
	// before this call went out. So the only question left is whether to give it back.
	dokuStatus := dokuResp.Payout.Status
	reverse := false
	switch dokuStatus {
	case "SUCCESS":
		_ = disbursement.MarkCompleted(dokuResp.Payout.InvoiceNumber)
	case "FAILED", "REJECTED":
		c.logger.WarnContext(ctx, "DOKU payout returned failed status",
			"disbursement_id", disbursement.UUID,
			"doku_status", dokuStatus,
			"doku_invoice", dokuResp.Payout.InvoiceNumber,
		)
		_ = disbursement.MarkFailed(fmt.Sprintf("DOKU payout status: %s", dokuStatus))
		// A status DOKU states outright is a known outcome: no money left, so the
		// reservation is released.
		reverse = true
	default:
		_ = disbursement.MarkProcessing(dokuResp.Payout.InvoiceNumber)
	}

	settlementJournal := domain.NewJournal(
		domain.EventTypeDisbursement,
		domain.SourceTypeDisbursement,
		disbursement.UUID,
		map[string]any{
			"amount":       disbursement.Amount,
			"bank_code":    disbursement.BankAccount.BankCode,
			"doku_status":  dokuStatus,
			"doku_invoice": dokuResp.Payout.InvoiceNumber,
			"stage":        "SETTLED",
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
			if err := tx.LedgerEntry().Save(ctx, domain.NewDisbursementReversalEntry(settlementJournal.UUID, disbursement.UUID, account.UUID, disbursement.Amount)); err != nil {
				return err
			}
		}

		// If disbursement is COMPLETED, increment total_withdrawal_amount
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
		// Recoverable now, unlike before: the row and its Request-Id were written ahead of
		// the call, so RetryDisbursement can replay and book the same answer.
		c.logger.ErrorContext(ctx, "CRITICAL: DOKU succeeded but DB save failed — retry with the stored request id",
			"disbursement_id", disbursement.UUID,
			"payout_request_id", disbursement.PayoutRequestID,
			"doku_status", dokuStatus,
			"doku_invoice", dokuResp.Payout.InvoiceNumber,
			"amount", disbursement.Amount,
			"account_id", account.UUID,
			"error", err,
		)
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save disbursement after DOKU success", err)
	}

	c.logger.InfoContext(ctx, "Withdrawal completed",
		"disbursement_id", disbursement.UUID,
		"status", disbursement.Status,
		"amount", disbursement.Amount,
	)

	return &WithdrawResponse{
		DisbursementID: disbursement.UUID,
		Status:         string(disbursement.Status),
		Amount:         disbursement.Amount,
		Currency:       string(disbursement.Currency),
		Message:        fmt.Sprintf("Withdrawal %s", dokuStatus),
	}, nil
}

// payoutRequestLogBody renders the payout request as the JSON body DOKU will receive.
//
// The individual fields are logged beside it, but a payout DOKU rejects is disputed over
// the body it was sent, not over our field names. This marshals the very struct the client
// marshals, so the log holds what went on the wire — reproduced verbatim, empty account.id
// included, which is the shape "Request or data not found" comes back to.
//
// The one departure is the beneficiary account number, masked to its last four digits.
// These logs are shipped off-process; a full account number does not need to travel with
// them to answer the question a log line is asked, which is which of a seller's accounts
// this went to.
//
// req is taken by value: the mask must not touch what is about to be sent.
func payoutRequestLogBody(req requests.DokuSendPayoutSubAccountRequest) string {
	req.Beneficiary.BankAccountNumber = maskAccountNumber(req.Beneficiary.BankAccountNumber)

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

// recordPayoutFailure decides what a DOKU error means for the row, and it turns on one
// question: do we know the payout did not happen?
//
// A 4xx is DOKU refusing — invalid bank account, insufficient balance on their side. Nothing
// left, so the disbursement becomes FAILED, which is terminal here.
//
// A timeout (status 0) or a 5xx says nothing about the money. The payout may be on its way.
// Marking that FAILED would be a claim we cannot support, and worse, it would lock the row
// out of any replay, because FAILED cannot transition to COMPLETED. Those stay PENDING with
// their Request-Id intact, which is exactly the state RetryDisbursement is built for.
func (c *LedgerClient) recordPayoutFailure(ctx context.Context, disbursement *domain.Disbursement, dokuErr *dokumodels.ErrorLog) error {
	outcomeKnown := dokuErr.StatusCode >= 400 && dokuErr.StatusCode < 500

	c.logger.ErrorContext(ctx, "DOKU SendPayoutSubAccount failed",
		"disbursement_id", disbursement.UUID,
		"payout_request_id", disbursement.PayoutRequestID,
		"error", dokuErr.Err,
		"message", dokuErr.Message,
		"status_code", dokuErr.StatusCode,
		"outcome_known", outcomeKnown,
	)

	if !outcomeKnown {
		// Left in flight on purpose, and the reservation stays with it. The money may be
		// on its way; handing it back to the available balance is precisely how it would
		// be withdrawn a second time. PENDING plus a stored Request-Id is the only state
		// from which the truth can still be recovered.
		c.logger.WarnContext(ctx, "Payout outcome unknown — disbursement left in flight, balance stays reserved",
			"disbursement_id", disbursement.UUID,
			"payout_request_id", disbursement.PayoutRequestID,
			"amount", disbursement.Amount,
		)
		return ledgererr.NewError(ledgererr.CodeDokuAPIError,
			"DOKU disbursement outcome unknown; it will be resolved by retrying with the stored request id",
			fmt.Errorf("status code: %d, error: %v", dokuErr.StatusCode, dokuErr.Message))
	}

	// Known refusal: no money left, so the reservation goes back to the available balance
	// and the row reaches its terminal state.
	if err := disbursement.MarkFailed(fmt.Sprintf("DOKU rejected the payout: %v", dokuErr.Message)); err == nil {
		reversalJournal := domain.NewJournal(
			domain.EventTypeDisbursement,
			domain.SourceTypeDisbursement,
			disbursement.UUID,
			map[string]any{
				"amount":      disbursement.Amount,
				"bank_code":   disbursement.BankAccount.BankCode,
				"status_code": dokuErr.StatusCode,
				"stage":       "REVERSED",
			},
		)

		saveErr := c.txProvider.Transact(ctx, func(tx repo.Tx) error {
			if err := tx.Journal().Save(ctx, reversalJournal); err != nil {
				return err
			}
			if err := tx.Disbursement().Save(ctx, disbursement); err != nil {
				return err
			}
			return tx.LedgerEntry().Save(ctx, domain.NewDisbursementReversalEntry(reversalJournal.UUID, disbursement.UUID, disbursement.LedgerUUID, disbursement.Amount))
		})
		if saveErr != nil {
			// The seller's money is held by a reservation that no longer corresponds to
			// anything. Loud, because only a human can put this right.
			c.logger.ErrorContext(ctx, "CRITICAL: payout rejected but the reservation could not be released — balance is understated",
				"disbursement_id", disbursement.UUID,
				"account_id", disbursement.LedgerUUID,
				"amount", disbursement.Amount,
				"error", saveErr,
			)
		}
	}

	return ledgererr.NewError(ledgererr.CodeDokuAPIError, "DOKU disbursement failed", fmt.Errorf("%v", dokuErr.Message))
}

// ─────────────────────────────────────────────────────────────────────────────
// Reconciliation (Settlement CSV processing)
// ─────────────────────────────────────────────────────────────────────────────

// ReconciliationRequest contains the parameters for reconciliation
type ReconciliationRequest struct {
	CSVReader      io.Reader // CSV file reader
	ReportFileName string    // Original filename
	SettlementDate time.Time // Date of settlement
	UploadedBy     string    // Admin/System who uploaded
}

// ReconciliationResponse contains the result of reconciliation
type ReconciliationResponse struct {
	ReconciliationID string `json:"reconciliation_id"`
	// AlreadyIngested reports that this CSV's batch_id was already booked and
	// nothing was posted. It is a success, not an error: re-presenting a settlement
	// file is the ordinary case for a caller that discovers files by listing object
	// storage, and the answer it needs is "already done", not a failure.
	//
	// When it is true, only ReconciliationID, IngestedAs, UploadedBy, UploadedAt,
	// SettlementDate and Transactions describe the ORIGINAL ingest; BalanceUpdates,
	// Discrepancies and Verification are zero, because this call moved no money.
	AlreadyIngested bool `json:"already_ingested,omitempty"`
	// IngestedAs is the report_file_name the batch was originally ingested under,
	// set only when AlreadyIngested is true.
	//
	// It is returned rather than a bare boolean so the caller can tell two very
	// different situations apart: the SAME file presented again (benign, and worth
	// no more than an info log), versus a DIFFERENT file carrying a batch_id that
	// has already been booked — which is also what a DOKU correction would look
	// like, and must not be swallowed quietly.
	IngestedAs     string                  `json:"ingested_as,omitempty"`
	UploadedBy     string                  `json:"uploaded_by"`
	UploadedAt     time.Time               `json:"uploaded_at"`
	SettlementDate string                  `json:"settlement_date"`
	Transactions   ReconciliationTxSummary `json:"transactions"`
	BalanceUpdates ReconciliationBalances  `json:"balance_updates"`
	Discrepancies  []DiscrepancySummary    `json:"discrepancies"`
	Verification   ReconciliationVerify    `json:"verification"`
}

// ReconciliationTxSummary contains transaction counts
type ReconciliationTxSummary struct {
	Total     int `json:"total"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
}

// ReconciliationBalances contains balance changes
type ReconciliationBalances struct {
	Pending   BalanceChange `json:"pending"`
	Available BalanceChange `json:"available"`
}

// BalanceChange represents before/after/diff for a balance
type BalanceChange struct {
	Before int64 `json:"before"`
	After  int64 `json:"after"`
	Diff   int64 `json:"diff"`
}

// DiscrepancySummary contains discrepancy information
type DiscrepancySummary struct {
	Type          string `json:"type"`
	InvoiceNumber string `json:"invoice_number,omitempty"`
	Amount        int64  `json:"amount,omitempty"`
	Message       string `json:"message"`
}

// ReconciliationVerify contains DOKU verification results
type ReconciliationVerify struct {
	DokuAPIChecked     bool   `json:"doku_api_checked"`
	DokuPending        int64  `json:"doku_pending,omitempty"`   // Deprecated: use seller-level verification
	DokuAvailable      int64  `json:"doku_available,omitempty"` // Deprecated: use seller-level verification
	MatchStatus        string `json:"match_status"`
	SellersVerified    int    `json:"sellers_verified"`     // Number of sellers with DOKU sub-accounts verified
	SellersMatched     int    `json:"sellers_matched"`      // Number of sellers with exact balance match
	SellersMismatched  int    `json:"sellers_mismatched"`   // Number of sellers with balance discrepancies
	SellersNotVerified int    `json:"sellers_not_verified"` // Number of sellers without DOKU sub-accounts
}

// FilterIngestedReportFiles returns the subset of reportFileNames that this ledger
// has already ingested as settlement batches.
//
// It exists for callers that discover settlement files by listing object storage:
// they list, ask here which ones are already booked, and process the difference.
// Asking the ledger is the point — "have I processed this file?" is a question
// about ledger state, and answering it by reaching into settlement_batches from
// outside this package couples that caller to a schema it does not own.
//
// The answer is a set rather than a slice because the caller's next move is
// always a membership test, one per listed key.
//
// This is the cheap filter, not the safety net. It keys on report_file_name, which
// a rename or a re-download under a new path defeats; the guarantee that a batch is
// booked at most once comes from batch_id, enforced inside ProcessReconciliation
// and by the unique index behind it. A caller may skip this entirely and simply
// hand every file over — it would just do more parsing to reach the same outcome.
func (c *LedgerClient) FilterIngestedReportFiles(ctx context.Context, reportFileNames []string) (map[string]struct{}, error) {
	ingested, err := c.repoProvider.SettlementBatch().FilterIngestedReportFiles(ctx, reportFileNames)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to filter ingested report files", err)
	}

	return ingested, nil
}

// ProcessReconciliation processes a DOKU settlement CSV and writes immutable
// ledger entries to convert PENDING → AVAILABLE for seller and platform accounts,
// and clears the DOKU expense PENDING balance.
//
// NOTE: Settlement CSV is PLATFORM-WIDE and contains invoices from ALL sellers.
// Each transaction is matched to its respective seller account during processing.
//
// Phase 3 flow:
// 1. Validate + parse CSV
// 2. Insert settlement_batch record (tied to platform account)
// 3. For each CSV row: match → mark ProductTransaction SETTLED
// 4. Write settlement ledger entries (PENDING→AVAILABLE) for each seller
// 5. Optionally verify with DOKU GetBalance API
func (c *LedgerClient) ProcessReconciliation(ctx context.Context, req *ReconciliationRequest) (*ReconciliationResponse, error) {
	if req.CSVReader == nil {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "csv_reader is required", nil)
	}
	if req.UploadedBy == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "uploaded_by is required", nil)
	}

	// Fetch system accounts
	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.NewError(ledgererr.CodeInternal, "platform account not found - please create platform account first", err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get platform account", err)
	}
	dokuAccount, err := c.repoProvider.Account().GetPaymentGatewayAccount(ctx)
	if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get payment gateway account", err)
	}

	// Derive pre-settlement balances for platform account
	// (Settlement CSV contains transactions from ALL sellers, so we track at platform level)
	previousPending, previousAvailable, err := c.repoProvider.LedgerEntry().GetAllBalances(ctx, platformAccount.UUID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to derive pre-settlement balances", err)
	}

	// Parse CSV
	parser := domain.NewDokuSettlementCSVParser("", 1)
	if err := parser.Parse(req.CSVReader); err != nil {
		return nil, err
	}

	csvRows := parser.GetRows()
	if len(csvRows) == 0 {
		return nil, ledgererr.ErrInvalidSettlementCSVFormat.WithError(fmt.Errorf("CSV contains no data rows"))
	}

	c.logger.InfoContext(ctx, "Parsed settlement CSV",
		"platform_account_id", platformAccount.UUID,
		"total_rows", len(csvRows),
		"skipped_rows", parser.GetSkippedRows(),
		"parse_errors", len(parser.GetParseErrors()),
	)

	// Extract metadata from CSV
	csvMetadata := parser.GetMetadata()
	if csvMetadata == nil || csvMetadata.BatchID == "" {
		return nil, ledgererr.ErrInvalidSettlementCSVFormat.WithError(fmt.Errorf("CSV metadata missing Batch ID"))
	}

	c.logger.InfoContext(ctx, "Extracted CSV metadata",
		"batch_id", csvMetadata.BatchID,
		"total_amount_purchase", csvMetadata.TotalAmountPurchase,
		"total_fee", csvMetadata.TotalFee,
		"total_settlement", csvMetadata.TotalSettlement,
		"total_transactions", csvMetadata.TotalTransactions,
	)

	// Idempotency check — the normal brake.
	//
	// A batch_id already present in settlement_batches has been booked, and the
	// ledger entries behind it are immutable. Processing it a second time would
	// double-post: there is no undo, only compensating entries and an audit.
	//
	// This runs after the parse (batch_id lives in the CSV metadata, so there is no
	// cheaper way to learn it) but before anything is written, so a repeat costs one
	// parse and one indexed lookup and touches nothing.
	//
	// It does not replace the unique index from migration 013. Check-then-insert is
	// a race between two concurrent callers, and it only protects the path that
	// remembers to check. The index is the emergency brake; this exists so the
	// ordinary repeat is a describable answer instead of a constraint violation
	// indistinguishable from the database being down.
	existingBatch, err := c.repoProvider.SettlementBatch().GetByBatchID(ctx, csvMetadata.BatchID)
	if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to look up settlement batch by batch_id", err)
	}
	if existingBatch != nil {
		c.logger.InfoContext(ctx, "Settlement batch already ingested - nothing posted",
			"batch_id", csvMetadata.BatchID,
			"ingested_as", existingBatch.ReportFileName,
			"presented_as", req.ReportFileName,
			"reconciliation_id", existingBatch.UUID,
		)

		return &ReconciliationResponse{
			ReconciliationID: existingBatch.UUID,
			AlreadyIngested:  true,
			IngestedAs:       existingBatch.ReportFileName,
			UploadedBy:       existingBatch.UploadedBy,
			UploadedAt:       existingBatch.UploadedAt,
			SettlementDate:   existingBatch.SettlementDate.Format("2006-01-02"),
			Transactions: ReconciliationTxSummary{
				Total:     existingBatch.MatchedCount + existingBatch.UnmatchedCount,
				Matched:   existingBatch.MatchedCount,
				Unmatched: existingBatch.UnmatchedCount,
			},
		}, nil
	}

	settlementDate := req.SettlementDate
	if settlementDate.IsZero() && len(csvRows) > 0 {
		settlementDate = csvRows[0].PayOutDate
	}

	// Create settlement batch tied to PLATFORM account
	// (CSV contains transactions from ALL sellers, tracked at platform level)
	batch, err := domain.NewSettlementBatch(
		platformAccount.UUID,
		req.ReportFileName,
		settlementDate,
		req.UploadedBy,
		platformAccount.Currency,
	)
	if err != nil {
		return nil, err
	}
	batch.BatchID = csvMetadata.BatchID // Set DOKU Batch ID from CSV metadata
	batch.MarkProcessing()

	c.logger.InfoContext(ctx, "Created settlement batch",
		"batch", batch,
	)

	// Process CSV rows - cache ProductTransactions to avoid N+1 queries
	var settlementItems []*domain.SettlementItem
	var discrepancies []DiscrepancySummary
	productTxCache := make(map[string]*domain.ProductTransaction) // Cache by ProductTransaction.UUID
	sellerItemDiscrepancies := make(map[string]struct {
		count int
		total int64
	}) // Track per seller
	var totalSettledSellerAmount int64   // seller_net_amount from matched transactions
	var totalSettledPlatformAmount int64 // platform_fee from matched transactions
	var totalDokuFee int64

	for _, csvRow := range csvRows {
		item, err := csvRow.ToSettlementItem(batch.UUID)
		if err != nil {
			c.logger.WarnContext(ctx, "Failed to create settlement item",
				"row_number", csvRow.RowNumber,
				"invoice_number", csvRow.InvoiceNumber,
				"error", err,
			)
			batch.IncrementUnmatched()
			discrepancies = append(discrepancies, DiscrepancySummary{
				Type:          "INVALID_CSV_ROW",
				InvoiceNumber: csvRow.InvoiceNumber,
				Message:       fmt.Sprintf("Row %d: %v", csvRow.RowNumber, err),
			})
			continue
		}

		productTx, err := c.repoProvider.ProductTransaction().GetByInvoiceNumber(ctx, csvRow.InvoiceNumber)
		if err != nil {
			if ledgererr.IsAppError(err, repo.ErrNotFound) {
				c.logger.WarnContext(ctx, "No matching transaction for invoice",
					"invoice_number", csvRow.InvoiceNumber,
					"row_number", csvRow.RowNumber,
				)
				batch.IncrementUnmatched()
				discrepancies = append(discrepancies, DiscrepancySummary{
					Type:          "UNMATCHED_CSV_ENTRY",
					InvoiceNumber: csvRow.InvoiceNumber,
					Amount:        csvRow.Amount,
					Message:       fmt.Sprintf("No matching transaction found for invoice: %s", csvRow.InvoiceNumber),
				})
				settlementItems = append(settlementItems, item)
				continue
			}
			return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to query product transaction", err)
		}

		// Skip if transaction is already settled (duplicate CSV entry or re-upload)
		if productTx.IsSettled() {
			c.logger.InfoContext(ctx, "Skipping already settled transaction",
				"invoice_number", csvRow.InvoiceNumber,
				"product_tx_id", productTx.UUID,
				"settled_at", productTx.SettledAt,
			)
			batch.IncrementUnmatched()
			discrepancies = append(discrepancies, DiscrepancySummary{
				Type:          "ALREADY_SETTLED",
				InvoiceNumber: csvRow.InvoiceNumber,
				Amount:        csvRow.Amount,
				Message:       fmt.Sprintf("Transaction already settled (settled_at: %v)", productTx.SettledAt),
			})
			settlementItems = append(settlementItems, item)
			continue
		}

		if err := item.MatchToTransaction(productTx); err != nil {
			c.logger.WarnContext(ctx, "Failed to match settlement item",
				"invoice_number", csvRow.InvoiceNumber,
				"product_tx_id", productTx.UUID,
				"error", err,
			)
			batch.IncrementUnmatched()
			settlementItems = append(settlementItems, item)
			continue
		}

		// Verify SubAccount from CSV matches seller's DOKU sub-account for safety
		if csvRow.SubAccount != "" {
			sellerAccount, err := c.repoProvider.Account().GetByID(ctx, productTx.SellerAccountID)
			if err != nil {
				c.logger.WarnContext(ctx, "Failed to get seller account for SubAccount verification",
					"invoice_number", csvRow.InvoiceNumber,
					"product_tx_id", productTx.UUID,
					"seller_account_id", productTx.SellerAccountID,
					"error", err,
				)
			} else if sellerAccount.DokuSubAccountID != "" && sellerAccount.DokuSubAccountID != csvRow.SubAccount {
				c.logger.WarnContext(ctx, "SubAccount mismatch - CSV SubAccount differs from seller's account, transaction will not be settled",
					"invoice_number", csvRow.InvoiceNumber,
					"product_tx_id", productTx.UUID,
					"csv_sub_account", csvRow.SubAccount,
					"seller_doku_sac", sellerAccount.DokuSubAccountID,
				)
				discrepancies = append(discrepancies, DiscrepancySummary{
					Type:          "SUBACCOUNT_MISMATCH",
					InvoiceNumber: csvRow.InvoiceNumber,
					Message:       fmt.Sprintf("CSV SubAccount (%s) != Seller Account (%s) - Transaction NOT settled", csvRow.SubAccount, sellerAccount.DokuSubAccountID),
				})
				// Mark as unmatched and skip settlement for this transaction
				item.IsMatched = false
				batch.IncrementUnmatched()
				settlementItems = append(settlementItems, item)
				continue
			}
		}

		// Fee mismatch reconciliation
		// feeDelta = ActualDokuFee (from CSV) - ExpectedDokuFee (from ProductTransaction)
		feeDelta := csvRow.Fee - productTx.Fee.DokuFee
		if feeDelta != 0 {
			blocked := false
			var blockReason string

			switch productTx.Fee.FeeModel {
			case domain.FeeModelGatewayOnCustomer:
				// Platform absorbs when DOKU is more expensive
				if feeDelta > 0 {
					adjustedPlatformFee := productTx.Fee.PlatformFee - feeDelta
					if adjustedPlatformFee < 0 {
						blocked = true
						blockReason = fmt.Sprintf("feeDelta (%d) exceeds PlatformFee (%d) — platform cannot absorb", feeDelta, productTx.Fee.PlatformFee)
					}
				}
			case domain.FeeModelGatewayOnSeller:
				// Seller absorbs when DOKU is more expensive
				if feeDelta > 0 {
					adjustedSellerNet := productTx.Fee.SellerNetAmount - feeDelta
					if adjustedSellerNet < 0 {
						blocked = true
						blockReason = fmt.Sprintf("feeDelta (%d) exceeds SellerNetAmount (%d) — seller cannot absorb", feeDelta, productTx.Fee.SellerNetAmount)
					}
				}
			}

			if blocked {
				c.logger.WarnContext(ctx, "Fee mismatch irreconcilable - transaction will not be settled",
					"invoice_number", csvRow.InvoiceNumber,
					"product_tx_id", productTx.UUID,
					"fee_model", productTx.Fee.FeeModel,
					"expected_doku_fee", productTx.Fee.DokuFee,
					"actual_doku_fee", csvRow.Fee,
					"fee_delta", feeDelta,
					"reason", blockReason,
				)
				discrepancies = append(discrepancies, DiscrepancySummary{
					Type:          "FEE_MISMATCH_IRRECONCILABLE",
					InvoiceNumber: csvRow.InvoiceNumber,
					Amount:        feeDelta,
					Message:       fmt.Sprintf("Invoice %s: %s", csvRow.InvoiceNumber, blockReason),
				})
				item.IsMatched = false
				batch.IncrementUnmatched()
				settlementItems = append(settlementItems, item)
				continue
			}

			// Reconcilable — store feeDelta on item for use in settlement entries
			item.FeeAdjustment = feeDelta
			c.logger.InfoContext(ctx, "Fee mismatch will be adjusted during settlement",
				"invoice_number", csvRow.InvoiceNumber,
				"product_tx_id", productTx.UUID,
				"fee_model", productTx.Fee.FeeModel,
				"expected_doku_fee", productTx.Fee.DokuFee,
				"actual_doku_fee", csvRow.Fee,
				"fee_delta", feeDelta,
			)
			discrepancies = append(discrepancies, DiscrepancySummary{
				Type:          "FEE_ADJUSTMENT_APPLIED",
				InvoiceNumber: csvRow.InvoiceNumber,
				Amount:        feeDelta,
				Message:       fmt.Sprintf("Invoice %s: feeDelta=%d applied (fee_model=%s)", csvRow.InvoiceNumber, feeDelta, productTx.Fee.FeeModel),
			})
		}

		// Only process matching transactions from here onwards
		if productTx.IsCompleted() {
			productTx.MarkSettled()
		}

		batch.IncrementMatched()
		batch.AddToTotals(csvRow.Amount, csvRow.Fee)

		// SellerNetAmount is always the seller's real share (platform fee tracked separately)
		totalSettledSellerAmount += productTx.Fee.SellerNetAmount
		totalSettledPlatformAmount += productTx.Fee.PlatformFee
		totalDokuFee += csvRow.Fee

		// Cache ProductTransaction for later use
		productTxCache[productTx.UUID] = productTx

		settlementItems = append(settlementItems, item)
	}

	batch.MarkCompleted(batch.GrossAmount, batch.NetAmount, batch.DokuFee, batch.MatchedCount, batch.UnmatchedCount)

	now := time.Now()

	// Create journal for SETTLEMENT event
	settlementJournal := domain.NewJournal(
		domain.EventTypeSettlement,
		domain.SourceTypeSettlementBatch,
		batch.UUID,
		map[string]any{
			"report_file_name":      req.ReportFileName,
			"matched_count":         batch.MatchedCount,
			"unmatched_count":       batch.UnmatchedCount,
			"total_seller_amount":   totalSettledSellerAmount,
			"total_platform_amount": totalSettledPlatformAmount,
			"total_doku_fee":        totalDokuFee,
		},
	)

	// pendingFeeTransfer holds data needed to execute a platform fee transfer after the DB commit.
	type pendingFeeTransfer struct {
		productTxUUID   string
		invoiceNumber   string
		sellerAccountID string
		amount          int64
	}
	pendingFeeTransfers := make([]pendingFeeTransfer, 0)

	// Build settlement ledger entries for EACH matched product transaction (using cached data)
	allSettlementEntries := make([]*domain.LedgerEntry, 0)

	for _, item := range settlementItems {
		if !item.IsMatched {
			continue
		}

		// Use cached ProductTransaction
		productTx, ok := productTxCache[item.ProductTransactionUUID]
		if !ok {
			c.logger.WarnContext(ctx, "Product transaction not in cache",
				"product_tx_id", item.ProductTransactionUUID,
			)
			continue
		}

		feeDelta := item.FeeAdjustment
		sellerSettleAmount := productTx.Fee.SellerNetAmount
		platformSettleAmount := productTx.Fee.PlatformFee

		if feeDelta > 0 {
			switch productTx.Fee.FeeModel {
			case domain.FeeModelGatewayOnCustomer:
				// Platform absorbs: reduce platform settlement amount
				platformSettleAmount = productTx.Fee.PlatformFee - feeDelta
			case domain.FeeModelGatewayOnSeller:
				// Seller absorbs: reduce seller settlement amount
				sellerSettleAmount = productTx.Fee.SellerNetAmount - feeDelta
			}
		}

		// Seller: PENDING → AVAILABLE
		if sellerSettleAmount > 0 {
			allSettlementEntries = append(allSettlementEntries,
				domain.NewSettlementEntriesForAccount(settlementJournal.UUID, productTx.UUID, item.SellerAccountID, sellerSettleAmount)...,
			)
		}
		// Seller adjustments
		if feeDelta > 0 && productTx.Fee.FeeModel == domain.FeeModelGatewayOnSeller {
			// Write-off: clear remaining seller PENDING (feeDelta absorbed)
			allSettlementEntries = append(allSettlementEntries,
				domain.NewFeeAdjustmentWriteOffEntry(settlementJournal.UUID, productTx.UUID, item.SellerAccountID, feeDelta),
			)
		} else if feeDelta < 0 {
			// Surplus: credit seller AVAILABLE directly (both fee models)
			allSettlementEntries = append(allSettlementEntries,
				domain.NewFeeAdjustmentCreditEntry(settlementJournal.UUID, productTx.UUID, item.SellerAccountID, -feeDelta),
			)
		}

		// Platform: PENDING → AVAILABLE
		if platformSettleAmount > 0 && platformAccount != nil {
			allSettlementEntries = append(allSettlementEntries,
				domain.NewSettlementEntriesForAccount(settlementJournal.UUID, productTx.UUID, platformAccount.UUID, platformSettleAmount)...,
			)

			pendingFeeTransfers = append(pendingFeeTransfers, pendingFeeTransfer{
				productTxUUID:   productTx.UUID,
				invoiceNumber:   productTx.InvoiceNumber,
				sellerAccountID: item.SellerAccountID,
				amount:          platformSettleAmount,
			})
		}
		// Platform write-off: clear remaining platform PENDING (GATEWAY_ON_CUSTOMER, feeDelta absorbed)
		if feeDelta > 0 && productTx.Fee.FeeModel == domain.FeeModelGatewayOnCustomer && platformAccount != nil {
			allSettlementEntries = append(allSettlementEntries,
				domain.NewFeeAdjustmentWriteOffEntry(settlementJournal.UUID, productTx.UUID, platformAccount.UUID, feeDelta),
			)
		}

		// DOKU expense: clear PENDING (always ExpectedDokuFee — actual delta absorbed above)
		if productTx.Fee.DokuFee > 0 && dokuAccount != nil {
			allSettlementEntries = append(allSettlementEntries,
				domain.NewDokuFeeSettlementEntry(settlementJournal.UUID, productTx.UUID, dokuAccount.UUID, productTx.Fee.DokuFee),
			)
		}
	}

	c.logger.DebugContext(ctx, "Built settlement ledger entries", "entries", allSettlementEntries)

	c.logger.InfoContext(ctx, "Settlement balance calculations",
		"previous_pending", previousPending,
		"previous_available", previousAvailable,
		"total_seller_amount", totalSettledSellerAmount,
		"total_platform_fee", totalSettledPlatformAmount,
		"total_doku_fee", totalDokuFee,
		"settlement_entries", len(allSettlementEntries),
	)

	// Persist everything atomically
	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		// Save settlement journal first
		c.logger.InfoContext(ctx, "Saving settlement journal and batch",
			"journal", settlementJournal,
			"batch", batch,
		)
		if err := tx.Journal().Save(ctx, settlementJournal); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save settlement journal", err)
		}

		if err := tx.SettlementBatch().Save(ctx, batch); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save settlement batch", err)
		}

		c.logger.InfoContext(ctx, "Saving settlement items", "items", settlementItems)
		if err := tx.SettlementItem().SaveBatch(ctx, settlementItems); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save settlement items", err)
		}

		// Update matched product transactions to SETTLED and increment seller's total_deposit_amount
		for _, item := range settlementItems {
			if item.IsMatched {
				if err := tx.ProductTransaction().UpdateStatus(ctx, item.ProductTransactionUUID, domain.TransactionStatusSettled, now); err != nil {
					c.logger.WarnContext(ctx, "Failed to update product transaction status",
						"product_tx_id", item.ProductTransactionUUID,
						"error", err,
					)
				}

				// Increment seller's total_deposit_amount with the amount that actually settled
				productTx, ok := productTxCache[item.ProductTransactionUUID]
				if ok && productTx.Fee.SellerNetAmount > 0 {
					if err := tx.Account().IncrementDeposit(ctx, item.SellerAccountID, productTx.Fee.SellerNetAmount); err != nil {
						c.logger.WarnContext(ctx, "Failed to increment seller deposit amount",
							"seller_account_id", item.SellerAccountID,
							"amount", productTx.Fee.SellerNetAmount,
							"error", err,
						)
					}
				}
			}
		}

		c.logger.InfoContext(ctx, "Saving settlement ledger entries", "entries", allSettlementEntries)
		// Write all settlement ledger entries (immutable, insert-only)
		if err := tx.LedgerEntry().SaveBatch(ctx, allSettlementEntries); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save settlement ledger entries", err)
		}

		return nil
	})
	if err != nil {
		// Transaction failed and rolled back completely (including batch record)
		// Return error to allow retry without UNIQUE constraint violation
		return nil, err
	}

	// Execute platform fee transfers (inline, best-effort — failures are retried by ProcessPlatformFeeTransfer)
	for _, pft := range pendingFeeTransfers {
		sellerAccount, err := c.repoProvider.Account().GetByID(ctx, pft.sellerAccountID)
		if err != nil || sellerAccount.DokuSubAccountID == "" {
			c.logger.WarnContext(ctx, "Platform fee transfer skipped - seller account unavailable",
				"product_tx_id", pft.productTxUUID,
				"seller_account_id", pft.sellerAccountID,
				"error", err,
			)
			continue
		}

		requestID := uuid.NewString()
		if err := c.repoProvider.ProductTransaction().SaveTransferRequestID(ctx, pft.productTxUUID, requestID); err != nil {
			c.logger.WarnContext(ctx, "Platform fee transfer skipped - failed to save request ID",
				"product_tx_id", pft.productTxUUID,
				"error", err,
			)
			continue
		}

		transferReq := requests.DokuTransferSubAccountRequest{}
		transferReq.Transfer.Origin = sellerAccount.DokuSubAccountID
		transferReq.Transfer.Destination = platformAccount.DokuSubAccountID
		transferReq.Transfer.Amount = int(pft.amount)
		transferReq.Transfer.InvoiceNumber = platformFeeInvoiceNumber(pft.invoiceNumber)

		_, dokuErr := c.dokuClient.TransferSubAccount(requestID, transferReq)
		if dokuErr != nil {
			c.logger.WarnContext(ctx, "Platform fee transfer failed - will retry via background job",
				"product_tx_id", pft.productTxUUID,
				"invoice_number", pft.invoiceNumber,
				"from_sac", sellerAccount.DokuSubAccountID,
				"to_sac", platformAccount.DokuSubAccountID,
				"amount", pft.amount,
				"error", dokuErr.Message,
			)
			continue
		}

		if err := c.repoProvider.ProductTransaction().MarkPlatformFeeTransferred(ctx, pft.productTxUUID); err != nil {
			c.logger.ErrorContext(ctx, "CRITICAL: DOKU transfer succeeded but DB update failed - requires manual reconciliation",
				"product_tx_id", pft.productTxUUID,
				"invoice_number", pft.invoiceNumber,
				"from_sac", sellerAccount.DokuSubAccountID,
				"to_sac", platformAccount.DokuSubAccountID,
				"amount", pft.amount,
				"error", err,
			)
			continue
		}

		c.logger.InfoContext(ctx, "Platform fee transferred successfully",
			"product_tx_id", pft.productTxUUID,
			"invoice_number", pft.invoiceNumber,
			"from_sac", sellerAccount.DokuSubAccountID,
			"to_sac", platformAccount.DokuSubAccountID,
			"amount", pft.amount,
		)
	}

	// Derive post-settlement balances for the response (platform account)
	postPending, postAvailable, _ := c.repoProvider.LedgerEntry().GetAllBalances(ctx, platformAccount.UUID)

	// ============================================================
	// PER-SELLER RECONCILIATION AND DOKU VERIFICATION
	// ============================================================
	// Group transactions by seller (using cached SellerAccountID)
	sellerTransactions := make(map[string][]*domain.SettlementItem)
	uniqueSellerIDs := make([]string, 0)
	for _, item := range settlementItems {
		if !item.IsMatched || item.SellerAccountID == "" {
			continue
		}
		if _, exists := sellerTransactions[item.SellerAccountID]; !exists {
			uniqueSellerIDs = append(uniqueSellerIDs, item.SellerAccountID)
		}
		sellerTransactions[item.SellerAccountID] = append(sellerTransactions[item.SellerAccountID], item)
	}

	// Query previous balances for all sellers BEFORE settlement
	sellerPreviousBalances := make(map[string]struct{ pending, available int64 })
	for _, sellerID := range uniqueSellerIDs {
		// Note: These are POST-settlement balances now. For true previous balances,
		// we'd need to query before the transaction commit. This is a limitation.
		prevPending, prevAvailable, err := c.repoProvider.LedgerEntry().GetAllBalances(ctx, sellerID)
		if err == nil {
			sellerPreviousBalances[sellerID] = struct{ pending, available int64 }{prevPending, prevAvailable}
		}
	}

	c.logger.InfoContext(ctx, "Starting per-seller reconciliation verification",
		"unique_sellers", len(sellerTransactions),
	)

	// For each seller, verify their balance with DOKU and create reconciliation logs
	sellerReconciliationResults := make(map[string]ReconciliationVerify)
	for sellerAccountID, items := range sellerTransactions {
		// Get seller account
		sellerAccount, err := c.repoProvider.Account().GetByID(ctx, sellerAccountID)
		if err != nil {
			c.logger.WarnContext(ctx, "Failed to get seller account for reconciliation",
				"seller_account_id", sellerAccountID,
				"error", err,
			)
			continue
		}

		// Calculate this seller's settled amount (using cached ProductTransactions)
		var sellerSettledAmount int64
		for _, item := range items {
			if productTx, ok := productTxCache[item.ProductTransactionUUID]; ok {
				sellerSettledAmount += productTx.Fee.SellerNetAmount
			}
		}

		// Get seller's post-settlement balances from ledger entries
		sellerPostPending, sellerPostAvailable, err := c.repoProvider.LedgerEntry().GetAllBalances(ctx, sellerAccount.UUID)
		if err != nil {
			c.logger.WarnContext(ctx, "Failed to get seller balances",
				"seller_account_id", sellerAccountID,
				"error", err,
			)
			continue
		}

		// Verify with DOKU GetBalance API for this seller's sub-account
		verification := ReconciliationVerify{
			DokuAPIChecked: false,
			MatchStatus:    "NOT_VERIFIED",
		}

		if sellerAccount.DokuSubAccountID != "" && c.dokuClient != nil {
			dokuBalance, dokuErr := c.dokuClient.GetBalance(sellerAccount.DokuSubAccountID)
			if dokuErr == nil && dokuBalance != nil && dokuBalance.Balance != nil {
				verification.DokuAPIChecked = true

				var dokuPending, dokuAvailable int64
				if dokuBalance.Balance.Pending.Valid {
					fmt.Sscanf(dokuBalance.Balance.Pending.String, "%d", &dokuPending)
				}
				if dokuBalance.Balance.Available.Valid {
					fmt.Sscanf(dokuBalance.Balance.Available.String, "%d", &dokuAvailable)
				}

				verification.DokuPending = dokuPending
				verification.DokuAvailable = dokuAvailable

				pendingMatch := dokuPending == sellerPostPending
				availableMatch := dokuAvailable == sellerPostAvailable

				if !pendingMatch || !availableMatch {
					verification.MatchStatus = "MISMATCH"

					var discrepancyType domain.DiscrepancyType
					if !pendingMatch && !availableMatch {
						discrepancyType = domain.DiscrepancyTypeBothMismatch
					} else if !pendingMatch {
						discrepancyType = domain.DiscrepancyTypePendingMismatch
					} else {
						discrepancyType = domain.DiscrepancyTypeAvailableMismatch
					}

					// Get item-level discrepancies for this seller
					sellerDisc := sellerItemDiscrepancies[sellerAccountID]
					discrepancy := domain.NewReconciliationDiscrepancy(
						sellerAccount.UUID,
						batch.UUID,
						discrepancyType,
						sellerPostPending,
						dokuPending,
						sellerPostAvailable,
						dokuAvailable,
						sellerDisc.count, // itemDiscrepancyCount for this seller
						sellerDisc.total, // totalItemDiscrepancy for this seller
					)
					discrepancy.CreatedAt = now
					discrepancy.UpdatedAt = now

					if err := c.repoProvider.ReconciliationDiscrepancy().Save(ctx, discrepancy); err != nil {
						c.logger.ErrorContext(ctx, "Failed to save reconciliation discrepancy",
							"seller_account_id", sellerAccountID,
							"error", err,
						)
					}

					discrepancies = append(discrepancies, DiscrepancySummary{
						Type:   string(discrepancyType),
						Amount: dokuAvailable - sellerPostAvailable,
						Message: fmt.Sprintf("Seller %s: DOKU balance mismatch — Pending: expected %d, got %d; Available: expected %d, got %d",
							sellerAccount.OwnerID, sellerPostPending, dokuPending, sellerPostAvailable, dokuAvailable),
					})

					c.logger.WarnContext(ctx, "Balance discrepancy detected for seller",
						"seller_account_id", sellerAccountID,
						"seller_owner_id", sellerAccount.OwnerID,
						"expected_pending", sellerPostPending,
						"doku_pending", dokuPending,
						"expected_available", sellerPostAvailable,
						"doku_available", dokuAvailable,
					)
				} else {
					verification.MatchStatus = "EXACT_MATCH"
					c.logger.InfoContext(ctx, "Seller balance verified successfully",
						"seller_account_id", sellerAccountID,
						"seller_owner_id", sellerAccount.OwnerID,
						"pending", sellerPostPending,
						"available", sellerPostAvailable,
					)
				}
			} else {
				c.logger.WarnContext(ctx, "Failed to verify seller with DOKU GetBalance API",
					"seller_account_id", sellerAccountID,
					"seller_owner_id", sellerAccount.OwnerID,
					"doku_sub_account_id", sellerAccount.DokuSubAccountID,
					"error", dokuErr,
				)
			}
		} else {
			c.logger.WarnContext(ctx, "Seller has no DOKU sub-account ID, skipping verification",
				"seller_account_id", sellerAccountID,
				"seller_owner_id", sellerAccount.OwnerID,
			)
		}

		sellerReconciliationResults[sellerAccountID] = verification
	}

	// Summary verification for response (platform-level)
	matchedSellers := 0
	mismatchedSellers := 0
	notVerifiedSellers := 0
	for _, result := range sellerReconciliationResults {
		if result.MatchStatus == "EXACT_MATCH" {
			matchedSellers++
		} else if result.MatchStatus == "MISMATCH" {
			mismatchedSellers++
		} else {
			notVerifiedSellers++
		}
	}

	verification := ReconciliationVerify{
		DokuAPIChecked: len(sellerReconciliationResults) > 0,
		MatchStatus: fmt.Sprintf("SELLERS_VERIFIED: %d matched, %d mismatched, %d not verified",
			matchedSellers, mismatchedSellers, notVerifiedSellers),
		SellersVerified:    len(sellerReconciliationResults),
		SellersMatched:     matchedSellers,
		SellersMismatched:  mismatchedSellers,
		SellersNotVerified: notVerifiedSellers,
	}

	c.logger.InfoContext(ctx, "Reconciliation completed",
		"platform_account_id", platformAccount.UUID,
		"batch_id", batch.UUID,
		"matched", batch.MatchedCount,
		"unmatched", batch.UnmatchedCount,
		"seller_settled", totalSettledSellerAmount,
		"platform_settled", totalSettledPlatformAmount,
		"doku_fee", totalDokuFee,
		"post_pending", postPending,
		"post_available", postAvailable,
		"unique_sellers", len(sellerTransactions),
		"sellers_verified_match", matchedSellers,
		"sellers_verified_mismatch", mismatchedSellers,
		"sellers_not_verified", notVerifiedSellers,
	)

	return &ReconciliationResponse{
		ReconciliationID: batch.UUID,
		UploadedBy:       req.UploadedBy,
		UploadedAt:       batch.UploadedAt,
		SettlementDate:   settlementDate.Format("2006-01-02"),
		Transactions: ReconciliationTxSummary{
			Total:     len(csvRows),
			Matched:   batch.MatchedCount,
			Unmatched: batch.UnmatchedCount,
		},
		BalanceUpdates: ReconciliationBalances{
			Pending: BalanceChange{
				Before: previousPending,
				After:  postPending,
				Diff:   postPending - previousPending,
			},
			Available: BalanceChange{
				Before: previousAvailable,
				After:  postAvailable,
				Diff:   postAvailable - previousAvailable,
			},
		},
		Discrepancies: discrepancies,
		Verification:  verification,
	}, nil
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

// PlatformFeeTransferError contains error details for a failed transfer
type PlatformFeeTransferError struct {
	TransactionID string `json:"transaction_id"`
	InvoiceNumber string `json:"invoice_number"`
	PlatformFee   int64  `json:"platform_fee"`
	ErrorMessage  string `json:"error_message"`
}

// PlatformFeeTransferSuccess contains details for a successful transfer
type PlatformFeeTransferSuccess struct {
	TransactionID  string `json:"transaction_id"`
	InvoiceNumber  string `json:"invoice_number"`
	PlatformFee    int64  `json:"platform_fee"`
	FromSubAccount string `json:"from_sub_account"`
	ToSubAccount   string `json:"to_sub_account"`
}

// ProcessPlatformFeeTransfer processes platform fee transfers for settled transactions
// that haven't had their platform fees transferred yet.
//
// This should be called:
// - After reconciliation completes successfully
// - As a periodic background job (e.g., every 5 minutes)
//
// Flow for each transaction:
// 1. Fetch seller account (to get seller's DOKU sub-account ID)
// 2. Call DOKU intra-sub-account transfer API
// 3. On success: Mark transaction as platform_fee_transferred = true
// 4. On failure: Log error, continue to next (will retry on next run)
//
// Parameters:
// - batchSize: Maximum number of transactions to process in one call (recommended: 50-100)
//
// Returns:
// - PlatformFeeTransferResult with success/failure counts and details
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

	if platformAccount.DokuSubAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "platform account has no DOKU sub-account ID", nil)
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
		"platform_doku_sac", platformAccount.DokuSubAccountID,
	)

	// Process each transaction
	for _, tx := range transactions {
		if tx.Fee.PlatformFee <= 0 {
			c.logger.WarnContext(ctx, "Transaction has zero platform fee, skipping",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
			)
			continue
		}

		// Get seller account to retrieve DOKU sub-account ID
		sellerAccount, err := c.repoProvider.Account().GetByID(ctx, tx.SellerAccountID)
		if err != nil {
			errMsg := fmt.Sprintf("failed to get seller account: %v", err)
			c.logger.ErrorContext(ctx, "Platform fee transfer failed - seller account not found",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"seller_account_id", tx.SellerAccountID,
				"error", err,
			)
			result.Failed++
			result.Errors = append(result.Errors, PlatformFeeTransferError{
				TransactionID: tx.UUID,
				InvoiceNumber: tx.InvoiceNumber,
				PlatformFee:   tx.Fee.PlatformFee,
				ErrorMessage:  errMsg,
			})
			continue
		}

		if sellerAccount.DokuSubAccountID == "" {
			errMsg := "seller account has no DOKU sub-account ID"
			c.logger.ErrorContext(ctx, "Platform fee transfer failed - invalid seller account",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"seller_account_id", tx.SellerAccountID,
			)
			result.Failed++
			result.Errors = append(result.Errors, PlatformFeeTransferError{
				TransactionID: tx.UUID,
				InvoiceNumber: tx.InvoiceNumber,
				PlatformFee:   tx.Fee.PlatformFee,
				ErrorMessage:  errMsg,
			})
			continue
		}

		// Ensure transfer_request_id exists before calling DOKU (enables idempotent retries)
		requestID := tx.TransferRequestID
		if requestID == "" {
			requestID = uuid.NewString()
			if err := c.repoProvider.ProductTransaction().SaveTransferRequestID(ctx, tx.UUID, requestID); err != nil {
				errMsg := fmt.Sprintf("failed to save transfer request ID: %v", err)
				c.logger.ErrorContext(ctx, "Platform fee transfer failed - could not persist request ID",
					"transaction_id", tx.UUID,
					"invoice_number", tx.InvoiceNumber,
					"error", err,
				)
				result.Failed++
				result.Errors = append(result.Errors, PlatformFeeTransferError{
					TransactionID: tx.UUID,
					InvoiceNumber: tx.InvoiceNumber,
					PlatformFee:   tx.Fee.PlatformFee,
					ErrorMessage:  errMsg,
				})
				continue
			}
		}

		transferReq := requests.DokuTransferSubAccountRequest{}
		transferReq.Transfer.Origin = sellerAccount.DokuSubAccountID
		transferReq.Transfer.Destination = platformAccount.DokuSubAccountID
		transferReq.Transfer.Amount = int(tx.Fee.PlatformFee)
		transferReq.Transfer.InvoiceNumber = platformFeeInvoiceNumber(tx.InvoiceNumber)

		_, dokuErr := c.dokuClient.TransferSubAccount(requestID, transferReq)
		if dokuErr != nil {
			errMsg := fmt.Sprintf("DOKU transfer API failed: %v", dokuErr.Message)
			c.logger.ErrorContext(ctx, "Platform fee transfer failed - DOKU API error",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"platform_fee", tx.Fee.PlatformFee,
				"from_sac", sellerAccount.DokuSubAccountID,
				"to_sac", platformAccount.DokuSubAccountID,
				"error", dokuErr.Message,
			)
			result.Failed++
			result.Errors = append(result.Errors, PlatformFeeTransferError{
				TransactionID: tx.UUID,
				InvoiceNumber: tx.InvoiceNumber,
				PlatformFee:   tx.Fee.PlatformFee,
				ErrorMessage:  errMsg,
			})
			continue
		}

		if err := c.repoProvider.ProductTransaction().MarkPlatformFeeTransferred(ctx, tx.UUID); err != nil {
			c.logger.ErrorContext(ctx, "CRITICAL: DOKU transfer succeeded but DB update failed - requires manual reconciliation",
				"transaction_id", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"platform_fee", tx.Fee.PlatformFee,
				"from_sac", sellerAccount.DokuSubAccountID,
				"to_sac", platformAccount.DokuSubAccountID,
				"db_error", err,
			)
			result.Failed++
			result.Errors = append(result.Errors, PlatformFeeTransferError{
				TransactionID: tx.UUID,
				InvoiceNumber: tx.InvoiceNumber,
				PlatformFee:   tx.Fee.PlatformFee,
				ErrorMessage:  fmt.Sprintf("DOKU succeeded but DB update failed: %v", err),
			})
			continue
		}

		c.logger.InfoContext(ctx, "Platform fee transferred successfully",
			"transaction_id", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
			"platform_fee", tx.Fee.PlatformFee,
			"from_sac", sellerAccount.DokuSubAccountID,
			"to_sac", platformAccount.DokuSubAccountID,
		)
		result.Succeeded++
		result.Transfers = append(result.Transfers, PlatformFeeTransferSuccess{
			TransactionID:  tx.UUID,
			InvoiceNumber:  tx.InvoiceNumber,
			PlatformFee:    tx.Fee.PlatformFee,
			FromSubAccount: sellerAccount.DokuSubAccountID,
			ToSubAccount:   platformAccount.DokuSubAccountID,
		})
	}

	c.logger.InfoContext(ctx, "Platform fee transfer batch completed",
		"total_processed", len(transactions),
		"succeeded", result.Succeeded,
		"failed", result.Failed,
	)

	return result, nil
}
