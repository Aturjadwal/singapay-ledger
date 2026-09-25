package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// The platform account is the seller's balance, withdrawal and history with the account
// resolved by type. These tests hold the two things that make that safe to expose: a
// platform payout runs through exactly the seller's money path against the platform's own
// account and nobody else's, and the statement pages through two tables without skipping or
// repeating a line.

const platformULID = "01PLATFORMACCOUNTULID"

// newPlatformTestClient builds a client with a provisioned platform account holding
// platformAvailable, beside a seller account holding sellerAvailable. The seller is there to
// be left alone.
func newPlatformTestClient(t *testing.T, gw *fakeGateway, platformAvailable, sellerAvailable int64) (*LedgerClient, *FakeRepositoryProvider, *domain.Account, *domain.Account) {
	t.Helper()

	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	platform := domain.NewPlatformAccount(platformULID, "platform", domain.CurrencyIDR)
	platform.SetSingapayAccount(platformULID, "000000000999")
	require.NoError(t, fakes.Account().Save(ctx, &platform))

	seller := domain.NewSellerAccount("01SELLERACCOUNTULID", "seller-1", domain.CurrencyIDR)
	seller.SetSingapayAccount("01SELLERACCOUNTULID", "000000000123")
	require.NoError(t, fakes.Account().Save(ctx, &seller))

	seedAvailable(t, fakes, platform.UUID, platformAvailable)
	seedAvailable(t, fakes, seller.UUID, sellerAvailable)

	client := &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		logger:       testLogger(),
		gateway:      gw,
	}

	return client, fakes, &platform, &seller
}

func seedAvailable(t *testing.T, fakes *FakeRepositoryProvider, accountUUID string, amount int64) {
	t.Helper()
	if amount == 0 {
		return
	}
	require.NoError(t, fakes.LedgerEntry().Save(context.Background(), &domain.LedgerEntry{
		JournalUUID:   "seed",
		AccountUUID:   accountUUID,
		Amount:        amount,
		BalanceBucket: domain.BalanceBucketAvailable,
		EntryType:     domain.EntryTypeSettlement,
		SourceType:    domain.SourceTypeManualAdjustment,
		SourceID:      "seed-" + accountUUID,
	}))
}

// outerCode is the code of the outermost AppError — the one a consumer reads with
// errors.As. ErrCode() answers a different question: it follows OriginError down to the
// innermost AppError, which for a lookup miss is repo.ErrNotFound's generic 404.
func outerCode(t *testing.T, err error) ledgererr.ErrorCode {
	t.Helper()
	var appErr ledgererr.AppError
	require.ErrorAs(t, err, &appErr)
	return appErr.Code
}

// ─────────────────────────────────────────────────────────────────────────────
// WithdrawFromPlatform
// ─────────────────────────────────────────────────────────────────────────────

func TestWithdrawFromPlatform_PaysOutOfThePlatformAccountOnly(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t), fee: 3_000}
	client, fakes, platform, seller := newPlatformTestClient(t, gw, 100_000, 80_000)

	resp, err := client.WithdrawFromPlatform(context.Background(), withdrawRequest())

	require.NoError(t, err)
	assert.Equal(t, string(domain.DisbursementStatusCompleted), resp.Status)
	assert.Equal(t, int64(50_000), resp.Amount)
	assert.Equal(t, int64(3_000), resp.TransferFee)
	assert.Equal(t, int64(47_000), resp.NetAmount)

	// The payout left the platform's sub-account, for the net.
	require.Len(t, gw.bodies, 1)
	assert.Equal(t, platformULID, gw.bodies[0].AccountID)
	assert.Equal(t, int64(47_000), gw.bodies[0].Amount)

	// The platform's balance moved by the requested amount; the seller's did not move.
	assert.Equal(t, int64(50_000), availableBalance(fakes, platform.UUID))
	assert.Equal(t, int64(80_000), availableBalance(fakes, seller.UUID))

	disbursement, err := fakes.Disbursement().GetByID(context.Background(), resp.DisbursementID)
	require.NoError(t, err)
	assert.Equal(t, platform.UUID, disbursement.LedgerUUID)

	// The reservation was taken under the platform account's row lock.
	assert.Contains(t, fakes.accountRepo.lockedForUpdate, platform.UUID)
}

func TestWithdrawFromPlatform_RefusesMoreThanTheAvailableBalance(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	// The seller holds plenty. It must not matter: the check is against the platform.
	client, fakes, platform, _ := newPlatformTestClient(t, gw, 20_000, 1_000_000)

	_, err := client.WithdrawFromPlatform(context.Background(), withdrawRequest())

	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrInsufficientBalance))
	assert.Empty(t, gw.references, "nothing may be sent for a refused reservation")
	assert.Equal(t, int64(20_000), availableBalance(fakes, platform.UUID))
}

func TestWithdrawFromPlatform_WithoutAPlatformAccountIsNotFound(t *testing.T) {
	gw := &fakeGateway{}
	fakes := NewFakeRepositoryProvider()
	client := &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		logger:       testLogger(),
		gateway:      gw,
	}

	_, err := client.WithdrawFromPlatform(context.Background(), withdrawRequest())

	assert.Equal(t, ledgererr.CodeLedgerNotFound, outerCode(t, err))
	assert.Empty(t, gw.references)
}

// SP003 is how an over-reaching platform payout ends: the ledger balance covered it, the
// sub-account did not (fees not yet swept). Singapay refuses before moving anything, so the
// reservation comes back and the row is FAILED rather than left in flight.
func TestWithdrawFromPlatform_SubAccountShortfallIsARefusal(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{StatusCode: 400, Code: singapay.CodeInsufficientFunds, Message: "insufficient funds"}}
	client, fakes, platform, _ := newPlatformTestClient(t, gw, 100_000, 0)

	resp, err := client.WithdrawFromPlatform(context.Background(), withdrawRequest())

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, ledgererr.CodeGatewayAPIError, outerCode(t, err))
	assert.Equal(t, int64(100_000), availableBalance(fakes, platform.UUID), "the reservation is released")
	for _, d := range fakes.disbursementRepo.disbursements {
		assert.Equal(t, domain.DisbursementStatusFailed, d.Status)
	}
}

func TestWithdrawFromPlatform_UnknownOutcomeNamesThePayoutInFlight(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{StatusCode: 400, Code: singapay.CodeTimeout}}
	client, fakes, platform, _ := newPlatformTestClient(t, gw, 100_000, 0)

	resp, err := client.WithdrawFromPlatform(context.Background(), withdrawRequest())

	require.Error(t, err)
	assert.Equal(t, ledgererr.CodeGatewayOutcomeUnknown, outerCode(t, err))
	require.NotNil(t, resp)
	assert.Equal(t, string(domain.DisbursementStatusPending), resp.Status)

	stored, getErr := fakes.Disbursement().GetByID(context.Background(), resp.DisbursementID)
	require.NoError(t, getErr)
	assert.Equal(t, platform.UUID, stored.LedgerUUID)
	assert.Equal(t, int64(50_000), availableBalance(fakes, platform.UUID), "still reserved")
}

// Withdraw resolves sellers only. Naming the platform's owner id as a seller id must not
// reach the platform's money — that is what WithdrawFromPlatform, and only it, is for.
func TestWithdraw_CannotReachThePlatformAccountByOwnerID(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, fakes, platform, _ := newPlatformTestClient(t, gw, 100_000, 0)

	req := withdrawRequest()
	req.AccountID = "platform"
	_, err := client.Withdraw(context.Background(), "platform", req)

	assert.Equal(t, ledgererr.CodeLedgerNotFound, outerCode(t, err))
	assert.Empty(t, gw.references)
	assert.Equal(t, int64(100_000), availableBalance(fakes, platform.UUID))
}

// A platform payout is settled by the same webhook as a seller's, and the outcome says whose
// it was so the caller does not go looking for a seller called "platform".
func TestHandleDisbursementNotification_ReportsAPlatformPayoutAsThePlatforms(t *testing.T) {
	gw := &fakeGateway{
		disburse:      payoutWithStatus(t, "01"),
		fee:           3_000,
		verifyWebhook: func(singapay.WebhookRequest) error { return nil },
	}
	client, fakes, platform, _ := newPlatformTestClient(t, gw, 100_000, 0)

	resp, err := client.WithdrawFromPlatform(context.Background(), withdrawRequest())
	require.NoError(t, err)
	require.Equal(t, string(domain.DisbursementStatusProcessing), resp.Status)
	require.Len(t, gw.references, 1)

	outcome, err := client.HandleDisbursementNotification(context.Background(),
		webhookRequest(moneyOutBody(gw.references[0], "06")))

	require.NoError(t, err)
	require.NotNil(t, outcome.Disbursement)
	assert.True(t, outcome.Booked)
	assert.Equal(t, domain.OwnerTypePlatform, outcome.OwnerType)
	assert.Equal(t, "platform", outcome.SellerID)

	// 06 is a terminal failure: the platform's reservation came back.
	assert.Equal(t, domain.DisbursementStatusFailed, outcome.Disbursement.Status)
	assert.Equal(t, int64(100_000), availableBalance(fakes, platform.UUID))
}

// ─────────────────────────────────────────────────────────────────────────────
// GetPlatformTransactions
// ─────────────────────────────────────────────────────────────────────────────

var statementBase = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return statementBase.Add(time.Duration(minutes) * time.Minute) }

// paidSale stores a product transaction in the given status, paid at paidAt, and the
// platform's ledger entries for it.
func paidSale(t *testing.T, fakes *FakeRepositoryProvider, id, sellerAccountUUID string, platformFee int64, status domain.TransactionStatus, paidAt time.Time, platformEntries ...int64) *domain.ProductTransaction {
	t.Helper()

	tx := domain.NewProductTransaction("buyer-1", sellerAccountUUID, "product-"+id, "SERVICE", "INV-"+id,
		domain.FeeBreakdown{
			SellerPrice:     100_000,
			PlatformFee:     platformFee,
			GatewayFee:      2_000,
			TotalCharged:    100_000 + platformFee + 2_000,
			SellerNetAmount: 100_000,
			FeeModel:        domain.FeeModelGatewayOnCustomer,
			Currency:        domain.CurrencyIDR,
		}, nil)
	tx.UUID = id
	tx.Status = status
	tx.CreatedAt = paidAt.Add(-5 * time.Minute)
	if status == domain.TransactionStatusCompleted || status == domain.TransactionStatusSettled {
		completedAt := paidAt
		tx.CompletedAt = &completedAt
	}
	require.NoError(t, fakes.ProductTransaction().Save(context.Background(), tx))

	platform, err := fakes.Account().GetPlatformAccount(context.Background())
	require.NoError(t, err)
	for _, amount := range platformEntries {
		require.NoError(t, fakes.LedgerEntry().Save(context.Background(), &domain.LedgerEntry{
			JournalUUID:   "j-" + id,
			AccountUUID:   platform.UUID,
			Amount:        amount,
			BalanceBucket: domain.BalanceBucketPending,
			EntryType:     domain.EntryTypePlatformCommission,
			SourceType:    domain.SourceTypeProductTransaction,
			SourceID:      id,
		}))
	}

	return tx
}

func payoutAt(t *testing.T, fakes *FakeRepositoryProvider, id, accountUUID string, amount int64, status domain.DisbursementStatus, requestedAt time.Time) {
	t.Helper()

	d, err := domain.NewDisbursementWithID(id, accountUUID, amount, domain.CurrencyIDR,
		domain.BankAccount{BankCode: "CENAIDJA", AccountNumber: "1234567890", AccountName: "PT Platform"}, "payout "+id)
	require.NoError(t, err)
	d.Status = status
	d.CreatedAt = requestedAt
	require.NoError(t, fakes.Disbursement().Save(context.Background(), d))
}

// statementFixture is a platform statement with every case the query has to get right: a
// settled fee whose settlement moved it off the priced figure, the platform's own sale, sales
// that must not appear, another account's payout that must not appear, and an income and a
// payout stamped in the same instant.
func statementFixture(t *testing.T) (*LedgerClient, *domain.Account) {
	t.Helper()

	client, fakes, platform, seller := newPlatformTestClient(t, &fakeGateway{}, 0, 0)

	// Settled: priced 1.000 PENDING, cleared, 998 AVAILABLE after the fee delta.
	paidSale(t, fakes, "pt-1", seller.UUID, 1_000, domain.TransactionStatusSettled, at(1), 1_000, -1_000, 998)
	payoutAt(t, fakes, "d-2", platform.UUID, 20_000, domain.DisbursementStatusCompleted, at(2))
	// The platform's own sale: it is the seller, and the whole net is its credit.
	paidSale(t, fakes, "pt-3", platform.UUID, 0, domain.TransactionStatusCompleted, at(3), 50_000, 0)
	payoutAt(t, fakes, "d-4", platform.UUID, 5_000, domain.DisbursementStatusFailed, at(4))
	// Three lines in the same instant, across both tables.
	paidSale(t, fakes, "pt-5", seller.UUID, 700, domain.TransactionStatusCompleted, at(5), 700)
	paidSale(t, fakes, "pt-6", seller.UUID, 800, domain.TransactionStatusCompleted, at(5), 800)
	payoutAt(t, fakes, "d-5", platform.UUID, 1_000, domain.DisbursementStatusPending, at(5))

	// Never on the statement: no platform fee, not paid, and another account's payout.
	paidSale(t, fakes, "pt-nofee", seller.UUID, 0, domain.TransactionStatusCompleted, at(6), 0)
	paidSale(t, fakes, "pt-unpaid", seller.UUID, 1_500, domain.TransactionStatusPending, at(7))
	payoutAt(t, fakes, "d-seller", seller.UUID, 9_000, domain.DisbursementStatusCompleted, at(8))

	return client, platform
}

// collectStatement walks every page and returns the line ids in the order they came.
func collectStatement(t *testing.T, client *LedgerClient, req PlatformTransactionsRequest) []string {
	t.Helper()

	var ids []string
	for pages := 0; ; pages++ {
		require.Less(t, pages, 20, "paging did not terminate")

		resp, err := client.GetPlatformTransactions(context.Background(), req)
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp.Transactions), req.PageSize)

		for _, line := range resp.Transactions {
			ids = append(ids, line.ID)
		}
		if !resp.HasMore {
			assert.Empty(t, resp.NextCursor)
			return ids
		}
		require.NotEmpty(t, resp.NextCursor)
		req.Cursor = resp.NextCursor
	}
}

func TestGetPlatformTransactions_PagesBothTablesWithoutGapsOrRepeats(t *testing.T) {
	client, _ := statementFixture(t)

	newestFirst := []string{"pt-6", "pt-5", "d-5", "d-4", "pt-3", "d-2", "pt-1"}

	for _, pageSize := range []int{1, 2, 3, 7, 50} {
		ids := collectStatement(t, client, PlatformTransactionsRequest{PageSize: pageSize})
		assert.Equal(t, newestFirst, ids, "page size %d", pageSize)
	}

	oldestFirst := []string{"pt-1", "d-2", "pt-3", "d-4", "d-5", "pt-5", "pt-6"}
	ids := collectStatement(t, client, PlatformTransactionsRequest{PageSize: 2, SortOrder: "ASC"})
	assert.Equal(t, oldestFirst, ids)
}

func TestGetPlatformTransactions_FiltersByType(t *testing.T) {
	client, _ := statementFixture(t)

	incomes := collectStatement(t, client, PlatformTransactionsRequest{Type: PlatformTransactionIncome, PageSize: 2})
	assert.Equal(t, []string{"pt-6", "pt-5", "pt-3", "pt-1"}, incomes)

	payouts := collectStatement(t, client, PlatformTransactionsRequest{Type: PlatformTransactionPayout, PageSize: 2})
	assert.Equal(t, []string{"d-5", "d-4", "d-2"}, payouts)
}

func TestGetPlatformTransactions_IncomeIsWhatThePlatformWasCredited(t *testing.T) {
	client, _ := statementFixture(t)

	resp, err := client.GetPlatformTransactions(context.Background(), PlatformTransactionsRequest{PageSize: 50})
	require.NoError(t, err)

	lines := map[string]*PlatformTransaction{}
	for _, line := range resp.Transactions {
		lines[line.ID] = line
	}

	// The settled figure from the entries, not the 1.000 that was priced.
	settled := lines["pt-1"]
	require.NotNil(t, settled.Income)
	assert.Equal(t, PlatformTransactionIncome, settled.Type)
	assert.Equal(t, int64(998), settled.Amount)
	assert.Equal(t, PlatformIncomeSourceFee, settled.Income.Source)
	assert.Equal(t, domain.TransactionStatusSettled, settled.Income.Transaction.Status)
	assert.Equal(t, at(1), settled.OccurredAt, "an income is dated when it was paid")
	assert.Nil(t, settled.Payout)

	own := lines["pt-3"]
	assert.Equal(t, int64(50_000), own.Amount)
	assert.Equal(t, PlatformIncomeSourceSale, own.Income.Source)

	failed := lines["d-4"]
	require.NotNil(t, failed.Payout)
	assert.Equal(t, PlatformTransactionPayout, failed.Type)
	assert.Equal(t, int64(5_000), failed.Amount)
	assert.Equal(t, domain.DisbursementStatusFailed, failed.Payout.Status)
	assert.Nil(t, failed.Income)
}

func TestGetPlatformTransactions_RefusesABadRequest(t *testing.T) {
	client, _ := statementFixture(t)

	_, err := client.GetPlatformTransactions(context.Background(), PlatformTransactionsRequest{Cursor: "not-a-cursor!"})
	require.Error(t, err)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeInvalidRequest, err))

	_, err = client.GetPlatformTransactions(context.Background(), PlatformTransactionsRequest{Type: "REFUND"})
	require.Error(t, err)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeInvalidRequest, err))
}

func TestPlatformCursor_RoundTripsItsPosition(t *testing.T) {
	// Microseconds, like the timestamp columns the cursor is compared against.
	position := domain.KeysetCursor{At: time.Date(2026, 9, 25, 3, 4, 5, 123456000, time.UTC), ID: "7c9e6679-7425-40de-944b-e07fc1f90ae7"}

	decoded, err := decodePlatformCursor(encodePlatformCursor(position))

	require.NoError(t, err)
	require.NotNil(t, decoded)
	assert.True(t, position.At.Equal(decoded.At))
	assert.Equal(t, position.ID, decoded.ID)

	first, err := decodePlatformCursor("")
	require.NoError(t, err)
	assert.Nil(t, first, "an empty cursor is the first page")
}
