package ledger

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// A seller's account — and the Singapay sub-account behind it — is created once, however many
// callers ask at the same moment: under the seller's creation lock, with the row read again
// under it, and a caller that finds the lock taken waiting for the holder's row with nothing
// held. The fakes play the concurrent caller through TryLockOwner's script; the same paths
// against Postgres (and a transaction-mode pooler) are in account_create_postgres_test.go.

const raceSeller = "seller-race-1"

func newAccountClient(gateway *fakeGateway) (*LedgerClient, *FakeRepositoryProvider) {
	fakes := NewFakeRepositoryProvider()
	return &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		logger:       testLogger(),
		gateway:      gateway,
	}, fakes
}

// capturedLog is a logger whose lines a test can read back.
func capturedLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// failingTransactions is a transaction provider whose BEGIN or COMMIT fails (the fakes keep
// what fn wrote: there is no rollback in memory).
type failingTransactions struct {
	fakes     *FakeRepositoryProvider
	beginErr  error
	commitErr error
}

func (f *failingTransactions) Transact(ctx context.Context, fn func(tx repo.Tx) error) error {
	return f.run(fn)
}

func (f *failingTransactions) TransactReadCommitted(ctx context.Context, fn func(tx repo.Tx) error) error {
	return f.run(fn)
}

func (f *failingTransactions) run(fn func(tx repo.Tx) error) error {
	if f.beginErr != nil {
		return f.beginErr
	}
	if err := fn(f.fakes); err != nil {
		return err
	}
	return f.commitErr
}

// openingGateway opens a sub-account per request, numbered in order.
func openingGateway() *fakeGateway {
	g := &fakeGateway{}
	g.createAccount = func(context.Context, singapay.CreateAccountRequest) (*singapay.Account, error) {
		n := len(g.accountRequests)
		return &singapay.Account{ID: "01SUBACCOUNT" + string(rune('A'+n-1)), Number: "88000000000" + string(rune('0'+n)), Status: singapay.AccountActive}, nil
	}
	return g
}

// sellerRows counts the seller rows the fake holds for owner.
func sellerRows(fakes *FakeRepositoryProvider, owner string) int {
	n := 0
	for _, account := range fakes.accountRepo.accounts {
		if account.OwnerType == domain.OwnerTypeSeller && account.OwnerID == owner {
			n++
		}
	}
	return n
}

func saveSeller(fakes *FakeRepositoryProvider, owner, singapayID string) *domain.Account {
	account := domain.NewSellerAccount(singapayID, owner, domain.CurrencyIDR)
	account.SetSingapayAccount(singapayID, "880000000099")
	_ = fakes.accountRepo.Save(context.Background(), &account)
	return &account
}

func TestCreateAccount_OpensTheSubAccountOnceUnderTheSellersLock(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)

	account, err := client.CreateAccount(context.Background(), raceSeller, "  Salon   Rara ", domain.CurrencyIDR)

	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Equal(t, raceSeller, account.OwnerID)
	assert.Equal(t, "01SUBACCOUNTA", account.SingapayAccountID)
	assert.Equal(t, "880000000001", account.SingapayAccountNumber)
	assert.Equal(t, []string{"SELLER:" + raceSeller}, fakes.accountRepo.creationLocks, "the creation took the seller's lock")
	require.Len(t, gateway.accountRequests, 1)
	assert.Equal(t, singapay.CreateAccountRequest{Name: "Salon Rara", Type: singapay.AccountTypeOwned}, gateway.accountRequests[0])
	assert.Equal(t, 1, sellerRows(fakes, raceSeller))
}

func TestCreateAccount_AnExistingAccountTakesNoLockAndNoSingapayCall(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	saveSeller(fakes, raceSeller, "01EXISTING")

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists), "err = %v", err)
	assert.Empty(t, fakes.accountRepo.creationLocks)
	assert.Empty(t, gateway.accountRequests)
}

// The previous holder committed between this caller's first read and its lock: the read under
// the lock finds the row, and Singapay is not asked again.
func TestCreateAccount_TheReadUnderTheLockFindsTheHoldersAccount(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	fakes.accountRepo.tryLockOwner = func(domain.OwnerType, string) (bool, error) {
		saveSeller(fakes, raceSeller, "01BYTHEOTHERCALLER")
		return true, nil
	}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists), "err = %v", err)
	assert.Empty(t, gateway.accountRequests, "the holder's sub-account is the seller's; no second one")
	assert.Equal(t, 1, sellerRows(fakes, raceSeller))
}

// Another caller holds the lock (it is asking Singapay): this one waits, without a transaction,
// and finds the account the holder committed.
func TestCreateAccount_WaitsForTheHolderAndFindsItsAccount(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	saved := false
	fakes.accountRepo.tryLockOwner = func(domain.OwnerType, string) (bool, error) {
		if !saved { // committed while this caller waits
			saveSeller(fakes, raceSeller, "01BYTHEHOLDER")
			saved = true
		}
		return false, nil
	}

	start := time.Now()
	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists), "err = %v", err)
	assert.Empty(t, gateway.accountRequests)
	assert.Len(t, fakes.accountRepo.creationLocks, 1, "one attempt, then looks")
	assert.Equal(t, 1, fakes.accountRepo.probes, "found at the first look after the first pause")
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, 1, sellerRows(fakes, raceSeller))
}

// The holder failed (its Singapay call did): the lock comes free with no row, and the waiting
// caller creates the account itself.
func TestCreateAccount_TakesOverWhenTheHolderGaveUp(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	asked := 0
	fakes.accountRepo.tryLockOwner = func(domain.OwnerType, string) (bool, error) {
		asked++
		return asked > 2, nil // held for the attempt and the first look, then free
	}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Len(t, gateway.accountRequests, 1)
	assert.Len(t, fakes.accountRepo.creationLocks, 2, "the attempt that found it held, and the one that got it")
	assert.Equal(t, 2, fakes.accountRepo.probes)
	assert.Equal(t, 1, sellerRows(fakes, raceSeller))
}

func TestCreateAccount_TheDeadlineEndsTheWaitWithNothingCreated(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	fakes.accountRepo.tryLockOwner = func(domain.OwnerType, string) (bool, error) { return false, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	account, err := client.CreateAccount(ctx, raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeAccountCreationInProgress, err), "err = %v", err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "the cause is kept: %v", err)
	assert.Less(t, time.Since(start), time.Second)
	assert.Empty(t, gateway.accountRequests)
	assert.Zero(t, sellerRows(fakes, raceSeller))
}

// The deadline ends inside a look — queued for a connection the holder of a pool of one keeps
// across the Singapay call: that is still giving up the wait, not an internal error. A look
// that fails for another reason is one.
func TestCreateAccount_ADeadlineInALookIsStillTheWait(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	fakes.accountRepo.tryLockOwner = func(domain.OwnerType, string) (bool, error) { return false, nil }
	fakes.accountRepo.probeErr = func(ctx context.Context) error {
		<-ctx.Done()
		return repo.ErrFailedQuerySQL.WithError(ctx.Err())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	account, err := client.CreateAccount(ctx, raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeAccountCreationInProgress, err), "err = %v", err)
	assert.Empty(t, gateway.accountRequests)

	fakes.accountRepo.probeErr = func(context.Context) error { return repo.ErrFailedQuerySQL.WithError(errors.New("connection reset")) }
	_, err = client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)
	require.Error(t, err)
	assert.False(t, ledgererr.IsErrorCode(ledgererr.CodeAccountCreationInProgress, err), "err = %v", err)
	assert.Contains(t, err.Error(), "failed to check existing account")
}

// Singapay refuses: no row is written, so nothing is half made, and the next call — Singapay
// answering again — creates the account.
func TestCreateAccount_ASingapayFailureLeavesNoRowAndTheNextCallCreates(t *testing.T) {
	gateway := openingGateway()
	open := gateway.createAccount
	gateway.createAccount = func(ctx context.Context, req singapay.CreateAccountRequest) (*singapay.Account, error) {
		if len(gateway.accountRequests) == 1 {
			return nil, &singapay.Error{StatusCode: 503, Message: "service unavailable"}
		}
		return open(ctx, req)
	}
	client, fakes := newAccountClient(gateway)

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)
	assert.Nil(t, account)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeGatewayAPIError, err), "err = %v", err)
	assert.Zero(t, sellerRows(fakes, raceSeller), "no account without a sub-account")

	account, err = client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)
	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Len(t, gateway.accountRequests, 2)
	assert.Equal(t, 1, sellerRows(fakes, raceSeller))
}

// A row for the seller written without the lock (a consumer on an older version mid-deploy)
// makes the insert hit the unique index: the seller has an account, so the caller is told so
// and reads it — the sub-account just opened is the orphan, logged for a human.
func TestCreateAccount_AUniqueViolationMeansTheSellerHasAnAccount(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	logger, logged := capturedLog()
	client.logger = logger
	fakes.accountRepo.saveErr = func(*domain.Account) error {
		return repo.ErrFailedInsertSQL.WithError(&pq.Error{Code: "23505", Constraint: repo.ConstraintUniqueSeller,
			Message: "duplicate key value violates unique constraint \"idx_accounts_unique_seller\""})
	}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists), "err = %v", err)
	assert.Len(t, gateway.accountRequests, 1)
	assert.Contains(t, logged.String(), "CRITICAL: Singapay sub-account created but the ledger row could not be saved", "the orphan is logged for a human")
	assert.Contains(t, logged.String(), "singapay_account_id=01SUBACCOUNTA")
}

// A duplicate Singapay identifier is not "the seller has an account": it is an error, and the
// sub-account is logged as an orphan.
func TestCreateAccount_AnotherUniqueViolationIsAnError(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	logger, logged := capturedLog()
	client.logger = logger
	fakes.accountRepo.saveErr = func(*domain.Account) error {
		return repo.ErrFailedInsertSQL.WithError(&pq.Error{Code: "23505", Constraint: "idx_ledger_accounts_singapay_account_id"})
	}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	require.Error(t, err)
	assert.False(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists), "err = %v", err)
	assert.Contains(t, logged.String(), "CRITICAL: Singapay sub-account created but the ledger row could not be saved")
}

func TestCreateAccount_ABeginFailureIsAnErrorWithNothingAsked(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	client.txProvider = &failingTransactions{fakes: fakes, beginErr: errors.New("pool exhausted")}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeDatabaseError, err), "err = %v", err)
	assert.Empty(t, gateway.accountRequests)
}

// The commit fails after Singapay opened the sub-account: an error, and a CRITICAL line that
// says the row may exist after all (a commit whose answer was lost).
func TestCreateAccount_ACommitFailureIsLoggedForAHuman(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	logger, logged := capturedLog()
	client.logger = logger
	client.txProvider = &failingTransactions{fakes: fakes, commitErr: errors.New("connection reset")}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	require.Error(t, err)
	assert.False(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists), "err = %v", err)
	assert.Len(t, gateway.accountRequests, 1)
	assert.Contains(t, logged.String(), "CRITICAL: Singapay sub-account created but the commit of its ledger row failed")
	assert.Contains(t, logged.String(), "singapay_account_id=01SUBACCOUNTA")
}

func TestCreateAccount_AnInsertFailureIsAnError(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	fakes.accountRepo.saveErr = func(*domain.Account) error {
		return repo.ErrFailedInsertSQL.WithError(errors.New("connection reset"))
	}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeDatabaseError, err), "err = %v", err)
	assert.False(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists))
}

func TestCreateAccount_ALockFailureIsAnError(t *testing.T) {
	gateway := openingGateway()
	client, fakes := newAccountClient(gateway)
	fakes.accountRepo.tryLockOwner = func(domain.OwnerType, string) (bool, error) {
		return false, repo.ErrFailedQuerySQL.WithError(errors.New("connection reset"))
	}

	account, err := client.CreateAccount(context.Background(), raceSeller, "Rara", domain.CurrencyIDR)

	assert.Nil(t, account)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeDatabaseError, err), "err = %v", err)
	assert.Empty(t, gateway.accountRequests)
}

// The singleton payment-gateway account under a unique index on owner_type: a concurrent
// caller that created it first wins, and this one returns that account instead of an error.
func TestCreatePaymentGatewayAccount_ReturnsTheAccountAConcurrentCallerCreated(t *testing.T) {
	client, fakes := newAccountClient(&fakeGateway{})
	var theirs domain.Account
	fakes.accountRepo.saveErr = func(*domain.Account) error {
		theirs = domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
		fakes.accountRepo.saveErr = nil
		_ = fakes.accountRepo.Save(context.Background(), &theirs)
		return repo.ErrFailedInsertSQL.WithError(&pq.Error{Code: "23505", Constraint: repo.ConstraintUniquePaymentGateway})
	}

	account, err := client.CreatePaymentGatewayAccount(context.Background(), domain.CurrencyIDR)

	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Equal(t, theirs.UUID, account.UUID)
}

func TestAccountCreationIdle_OutlastsTheCallersDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	assert.InDelta(t, (70 * time.Second).Seconds(), accountCreationIdle(ctx).Seconds(), 1)
	assert.Equal(t, 2*time.Minute, accountCreationIdle(context.Background()))

	// Bounded both ways: a deadline weeks away would overflow Postgres's milliseconds, and
	// one already past must not become 0, which is no bound at all.
	far, cancelFar := context.WithTimeout(context.Background(), 30*24*time.Hour)
	defer cancelFar()
	assert.Equal(t, 10*time.Minute, accountCreationIdle(far))
	past, cancelPast := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancelPast()
	assert.Equal(t, 30*time.Second, accountCreationIdle(past))
}
