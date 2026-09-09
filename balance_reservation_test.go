package ledger

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// The lock has to be taken before the balance is read. Reading first and locking later is
// the same race with extra steps.
func TestWithdraw_LocksTheAccountBeforeReadingTheBalance(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.NoError(t, err)

	assert.Contains(t, fakes.accountRepo.lockedForUpdate, account.UUID,
		"the withdrawal read the balance without holding a row lock, so two of them can both pass")
}

// The debit exists before the gateway is called, not after it answers.
func TestWithdraw_ReservesBeforeCallingTheGateway(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}

	var reservedAtCallTime int
	client, fakes, _ := newPayoutTestClient(t, gw, 100000)
	gw.beforeCall = func() { reservedAtCallTime = countDebits(fakes) }

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.NoError(t, err)

	assert.Equal(t, 1, reservedAtCallTime,
		"the money was still spendable at the moment the payout went out")
}

// The point of the whole change: money held by an in-flight payout cannot be spent again.
func TestWithdraw_ReservedMoneyCannotBeWithdrawnTwice(t *testing.T) {
	// First withdrawal times out — outcome unknown, so the reservation is held.
	gw := &fakeGateway{err: &singapay.Error{Err: context.DeadlineExceeded}}
	client, fakes, account := newPayoutTestClient(t, gw, 50000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)
	require.Equal(t, int64(0), availableBalance(fakes, account.UUID),
		"the in-flight payout must not still be counted as spendable")

	// The seller tries again for the same money. Idempotency cannot help here — this is a
	// different disbursement with a different reference — so the balance has to stop it.
	_, err = client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrInsufficientBalance),
		"expected insufficient balance, got: %v", err)

	assert.Len(t, gw.references, 1, "a second payout was sent for money that was already committed")
	assert.Len(t, fakes.disbursementRepo.disbursements, 1)
}

// Insufficient balance must stay a 4xx-shaped answer for the caller, not become a database
// error just because the check now happens inside a transaction.
func TestWithdraw_InsufficientBalanceIsStillTheCallersAnswer(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, fakes, _ := newPayoutTestClient(t, gw, 10000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest()) // asks 50000
	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrInsufficientBalance),
		"expected insufficient balance, got: %v", err)

	assert.Empty(t, gw.references, "the gateway must not be called when the balance cannot cover it")
	assert.Empty(t, fakes.disbursementRepo.disbursements, "a refused withdrawal must leave no row")
	assert.Zero(t, countDebits(fakes))
}

// Rows created before reservation existed have no debit. Settling one without backfilling
// it would mean money that left the bank but never left the books.
func TestRetryDisbursement_BackfillsAMissingReservation(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	legacy, err := domain.NewDisbursementWithID(domain.GenerateID(), account.UUID, 50000, domain.CurrencyIDR,
		domain.BankAccount{BankCode: "BNINIDJA", AccountNumber: "712739123020001", AccountName: "Ria Florensi"}, "")
	require.NoError(t, err)
	legacy.PayoutRequestID = "reference-from-before-reservations"
	require.NoError(t, fakes.Disbursement().Save(context.Background(), legacy))
	require.Zero(t, countDebits(fakes), "fixture should start with no reservation")

	_, err = client.RetryDisbursement(context.Background(), legacy.UUID)
	require.NoError(t, err)

	assert.Equal(t, 1, countDebits(fakes), "the retry settled a payout that was never deducted")
	assert.Equal(t, int64(50000), availableBalance(fakes, account.UUID))
}

// And a row that already has its reservation must not get a second one.
func TestRetryDisbursement_DoesNotDoubleReserve(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{StatusCode: http.StatusGatewayTimeout, Message: "timeout"}}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)

	var pending *domain.Disbursement
	for _, d := range fakes.disbursementRepo.disbursements {
		pending = d
	}
	require.NotNil(t, pending)

	gw.err = nil
	gw.disburse = payoutSuccess(t)
	_, err = client.RetryDisbursement(context.Background(), pending.UUID)
	require.NoError(t, err)

	assert.Equal(t, 1, countDebits(fakes), "the retry reserved the money a second time")
	assert.Equal(t, int64(50000), availableBalance(fakes, account.UUID))
}
