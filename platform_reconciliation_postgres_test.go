package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/21strive/redifu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// The platform balance reconciliation against PostgreSQL: the snapshot statement, migration
// 034's constraints and unique indexes, the platform account's row lock under real concurrency
// (two processes rounding, two admins booking, two archives at once), and the statement's third
// source. It runs only when LEDGER_TEST_DATABASE_URL names a database (README_TESTING.md),
// each test in a schema of its own built from database/schemas/schema.sql.

type pgPlatformReconFixture struct {
	t      *testing.T
	ctx    context.Context
	db     *sql.DB
	client *LedgerClient
	repos  repo.RepositoryProvider
	fees   *domain.FeeCalculator

	platform, gateway, seller *domain.Account

	mu            sync.Mutex
	singapay      map[string]int64 // available per sub-account, sen
	failTransfers map[string]bool  // merchant refs whose transfer Singapay refuses
	sales         int
}

func newPgPlatformReconFixture(t *testing.T) *pgPlatformReconFixture {
	t.Helper()
	db := ledgerTestDB(t)
	ctx := context.Background()
	repos := repo.NewRepositoryProvider(db)

	platform := domain.NewPlatformAccount(reconPlatformULID, "platform", domain.CurrencyIDR)
	platform.SetSingapayAccount(reconPlatformULID, reconPlatformNumber)
	require.NoError(t, repos.Account().Save(ctx, &platform))
	gatewayAccount := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
	require.NoError(t, repos.Account().Save(ctx, &gatewayAccount))
	seller := domain.NewSellerAccount(reconSellerULID, "seller-1", domain.CurrencyIDR)
	seller.SetSingapayAccount(reconSellerULID, "000000000777")
	require.NoError(t, repos.Account().Save(ctx, &seller))

	f := &pgPlatformReconFixture{
		t: t, ctx: ctx, db: db, repos: repos,
		platform: &platform, gateway: &gatewayAccount, seller: &seller,
		singapay:      map[string]int64{},
		failTransfers: map[string]bool{},
		fees: domain.NewFeeCalculator([]*domain.FeeConfig{
			{ConfigType: domain.FeeConfigTypePlatform, PaymentChannel: "PLATFORM", FeeType: domain.FeeTypeFixed, FixedAmount: 4000, IsActive: true},
			{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "QRIS", FeeType: domain.FeeTypePercentage, Percentage: 0.7, IsActive: true},
		}),
	}
	gw := &fakeGateway{
		balance: func(accountID string) (*singapay.Balance, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			available := f.singapay[accountID]
			return &singapay.Balance{
				Available: singapay.NewAmountFromMinor(available, "IDR"),
				Total:     singapay.NewAmountFromMinor(available, "IDR"),
				AccountID: accountID,
			}, nil
		},
		transfer: func(from string, req singapay.TransferRequest) (*singapay.AccountTransfer, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.failTransfers[req.MerchantRefNo] {
				return nil, &singapay.Error{StatusCode: http.StatusInternalServerError, Message: "fixture: transfer refused"}
			}
			if req.Amount.Minor() > f.singapay[from] {
				return nil, &singapay.Error{StatusCode: http.StatusUnprocessableEntity, Message: "Insufficient remitter account balance"}
			}
			f.singapay[from] -= req.Amount.Minor()
			f.singapay[reconPlatformULID] += req.Amount.Minor()
			return &singapay.AccountTransfer{Status: "success", MerchantRefNo: req.MerchantRefNo, Amount: req.Amount}, nil
		},
	}
	f.client = NewLedgerClient(db, gw, testLogger())
	return f
}

// sale books a paid, settled QRIS sale through the real repositories and bookSettlement; its
// platform fee settles at 4000 rupiah plus residualMinor sen.
func (f *pgPlatformReconFixture) sale(invoice string, residualMinor int64) *domain.ProductTransaction {
	f.t.Helper()
	f.sales++
	fee := f.fees.GetFeeBreakdown(100000, "QRIS", domain.CurrencyIDR)
	tx := domain.NewProductTransaction("buyer", f.seller.UUID, "product-"+invoice, "BOOKING", invoice, fee, nil)
	require.NoError(f.t, f.repos.ProductTransaction().Save(f.ctx, tx))
	payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(f.t, f.repos.Journal().Save(f.ctx, payment))
	require.NoError(f.t, f.repos.LedgerEntry().SaveBatch(f.ctx, domain.NewPaymentEntries(
		payment.UUID, tx.UUID, f.seller.UUID, fee.SellerNetAmount, f.platform.UUID, fee.PlatformFee, f.gateway.UUID, fee.GatewayFee)))
	moved, err := f.repos.ProductTransaction().UpdateStatusIf(f.ctx, tx.UUID,
		domain.TransactionStatusPending, domain.TransactionStatusCompleted, time.Now().Add(time.Duration(f.sales)*time.Second))
	require.NoError(f.t, err)
	require.True(f.t, moved)
	tx.Status = domain.TransactionStatusCompleted

	gross := domain.RupiahToMinor(fee.TotalCharged)
	actual := domain.RupiahToMinor(fee.GatewayFee) - residualMinor
	outcome, err := f.client.bookSettlement(f.ctx, tx, domain.SettledTransaction{
		PaymentChannel: "QRIS", GrossMinor: gross, FeeMinor: actual, NetMinor: gross - actual, FeeReported: true})
	require.NoError(f.t, err)
	require.Equal(f.t, settleOutcomeSettled, outcome)

	f.mu.Lock()
	f.singapay[reconSellerULID] += gross - actual
	f.mu.Unlock()
	return tx
}

func (f *pgPlatformReconFixture) sweep() *PlatformFeeTransferResult {
	f.t.Helper()
	result, err := f.client.ProcessPlatformFeeTransfer(f.ctx, 50)
	require.NoError(f.t, err)
	return result
}

func (f *pgPlatformReconFixture) available(accountUUID string) int64 {
	f.t.Helper()
	_, available, err := f.repos.LedgerEntry().GetAllBalances(f.ctx, accountUUID)
	require.NoError(f.t, err)
	return available
}

func (f *pgPlatformReconFixture) cachedAvailable(accountUUID string) int64 {
	f.t.Helper()
	account, err := f.repos.Account().GetByID(f.ctx, accountUUID)
	require.NoError(f.t, err)
	return account.AvailableBalance
}

func (f *pgPlatformReconFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(query, args...).Scan(&n))
	return n
}

// concurrently runs fn from n goroutines released together, and returns their errors.
func concurrently(n int, fn func() error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

func TestPlatformReconciliationPostgres_CheckAndRoundingUnderConcurrency(t *testing.T) {
	f := newPgPlatformReconFixture(t)
	f.sale("INV-PG-A", 72)
	f.sale("INV-PG-B", 90)
	unswept := f.sale("INV-PG-C", 16)
	f.failTransfers[platformFeeTransferReference(unswept.InvoiceNumber)] = true
	sweep := f.sweep()
	require.Equal(t, 2, sweep.Succeeded)
	require.Equal(t, 1, sweep.Failed)

	snapshot, err := f.repos.PlatformReconciliation().ReadBalanceSnapshot(f.ctx, f.platform.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(4000+4000+4000), snapshot.LedgerAvailable)
	assert.Equal(t, int64(72+90), snapshot.SweptResidualMinor)
	assert.Equal(t, int64(4000), snapshot.UnsweptPlatformFee, "B = the whole rupiah the ledger holds for the unswept 4000.16")
	assert.Equal(t, 1, snapshot.UnsweptCount)
	assert.Equal(t, 0, snapshot.PayoutsInFlight)

	rec, err := f.client.CheckPlatformBalance(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status, "unexplained %d", rec.Unexplained())

	// Eight processes round at once: one books the rupiah, the others read an emptied pool.
	before := f.available(f.platform.UUID)
	errs := concurrently(8, func() error {
		_, err := f.client.BookPlatformResidual(f.ctx)
		return err
	})
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_adjustments WHERE kind = 'ROUNDING'`))
	assert.Equal(t, before+1, f.available(f.platform.UUID))
	assert.Equal(t, before+1, f.cachedAvailable(f.platform.UUID), "SaveBatch kept the cached balance in step")
	var sources int
	require.NoError(t, f.db.QueryRow(`SELECT cardinality(source_transaction_uuids) FROM platform_adjustments WHERE kind = 'ROUNDING'`).Scan(&sources))
	assert.Equal(t, 2, sources, "the two swept rows, not the unswept one")

	// The unswept row's 16 sen join the pool once it is swept: 62 + 16 = 78, under a rupiah.
	delete(f.failTransfers, platformFeeTransferReference(unswept.InvoiceNumber))
	require.Equal(t, 1, f.sweep().Succeeded)
	rec, err = f.client.CheckPlatformBalance(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.PlatformReconciliationMatched, rec.Status, "unexplained %d", rec.Unexplained())
	assert.Equal(t, int64(78), rec.ResidualPoolMinor)

	latest, err := f.repos.PlatformReconciliation().LatestReconciliation(f.ctx, f.platform.UUID)
	require.NoError(t, err)
	assert.Equal(t, rec.UUID, latest.UUID)
	assert.Equal(t, *rec.SingapayAvailableMinor, *latest.SingapayAvailableMinor)
	assert.NotEmpty(t, latest.RawBalance)
}

func TestPlatformReconciliationPostgres_BookDifferenceOnceUnderConcurrency(t *testing.T) {
	f := newPgPlatformReconFixture(t)
	f.sale("INV-PG-A", 28)
	require.Equal(t, 1, f.sweep().Succeeded)
	f.mu.Lock()
	f.singapay[reconPlatformULID] += 400016 // CSV7MW
	f.mu.Unlock()

	rec, err := f.client.CheckPlatformBalance(f.ctx)
	require.NoError(t, err)
	require.Equal(t, int64(400016), rec.Unexplained())
	before := f.available(f.platform.UUID)

	errs := concurrently(8, func() error {
		_, err := f.client.BookPlatformDifference(f.ctx, PlatformDifferenceRequest{
			ReconciliationUUID: rec.UUID, Reason: domain.PlatformAdjustmentReasonTestFunds,
			Note: "Dana uji coba staging, PF-INV-20260911084626-CSV7MW", AdminID: "admin-1", AdminName: "Admin Satu"})
		return err
	})
	booked, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			booked++
		case ledgererr.IsAppError(err, ledgererr.ErrPlatformReconciliationAlreadyBooked):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, booked)
	assert.Equal(t, 7, refused)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_adjustments WHERE kind = 'DIFFERENCE'`))
	assert.Equal(t, before+4000, f.available(f.platform.UUID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journals WHERE event_type = 'MANUAL_ADJUSTMENT'`))

	again, err := f.client.CheckPlatformBalance(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.PlatformReconciliationMatched, again.Status, "unexplained %d", again.Unexplained())
	assert.Equal(t, int64(28+16), again.ResidualPoolMinor)

	state, err := f.client.GetPlatformReconciliation(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, again.UUID, state.Latest.UUID)
	assert.False(t, state.Bookable)

	at := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, f.client.MarkPlatformReconciliationEmailed(f.ctx, again.UUID, at))
	state, err = f.client.GetPlatformReconciliation(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, state.LastEmailedAt)
	assert.True(t, at.Equal(*state.LastEmailedAt))
}

func TestPlatformReconciliationPostgres_ArchiveOnceAndTheStatement(t *testing.T) {
	f := newPgPlatformReconFixture(t)

	// The DOKU era: two fees and the platform's own sale, settled before settled fees were
	// recorded (settled_platform_fee NULL).
	doku := func(invoice, sellerUUID string, amount int64) {
		fee := domain.FeeBreakdown{SellerPrice: 50000, PlatformFee: 1000, TotalCharged: 51000, SellerNetAmount: 50000,
			FeeModel: domain.FeeModelGatewayOnCustomer, Currency: domain.CurrencyIDR}
		tx := domain.NewProductTransaction("buyer", sellerUUID, "p-"+invoice, "BOOKING", invoice, fee, nil)
		tx.Status = domain.TransactionStatusSettled
		paid := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		tx.CompletedAt, tx.SettledAt = &paid, &paid
		require.NoError(t, f.repos.ProductTransaction().Save(f.ctx, tx))
		payment := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
		require.NoError(t, f.repos.Journal().Save(f.ctx, payment))
		credit := &domain.LedgerEntry{JournalUUID: payment.UUID, AccountUUID: f.platform.UUID, Amount: amount,
			BalanceBucket: domain.BalanceBucketPending, EntryType: domain.EntryTypePlatformCommission,
			SourceType: domain.SourceTypeProductTransaction, SourceID: tx.UUID}
		redifu.InitRecord(credit)
		require.NoError(t, f.repos.LedgerEntry().SaveBatch(f.ctx, []*domain.LedgerEntry{credit}))
		settlement := domain.NewJournal(domain.EventTypeSettlement, domain.SourceTypeProductTransaction, tx.UUID, nil)
		require.NoError(t, f.repos.Journal().Save(f.ctx, settlement))
		require.NoError(t, f.repos.LedgerEntry().SaveBatch(f.ctx, domain.NewSettlementEntriesForAccount(settlement.UUID, tx.UUID, f.platform.UUID, amount)))
		if sellerUUID != f.platform.UUID {
			require.NoError(t, f.repos.ProductTransaction().MarkPlatformFeeTransferred(f.ctx, tx.UUID))
		}
	}
	doku("INV-DOKU-1", f.seller.UUID, 1000)
	doku("INV-DOKU-2", f.seller.UUID, 1000)
	doku("INV-DOKU-YTWGBR", f.platform.UUID, 38727)
	f.sale("INV-PG-A", 50)
	require.Equal(t, 1, f.sweep().Succeeded)

	incomesBefore, err := f.repos.ProductTransaction().GetPlatformIncomes(f.ctx, f.platform.UUID, nil, 100, true)
	require.NoError(t, err)
	before := f.available(f.platform.UUID)

	plan, err := f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 40727})
	require.NoError(t, err)
	require.True(t, plan.AmountMatches, "computed %d", plan.Amount)
	assert.Len(t, plan.Sources, 3)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM ledger_accounts WHERE owner_type = 'RESERVE'`), "a dry run writes nothing")

	// Two archives at once: one is written, the other refused.
	results := make(chan error, 2)
	for _, err := range concurrently(2, func() error {
		_, err := f.client.ArchiveToReserve(f.ctx, ReserveArchiveRequest{ExpectedAmount: 40727, Apply: true, AdminName: "operator"})
		return err
	}) {
		results <- err
	}
	close(results)
	applied, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			applied++
		case ledgererr.IsAppError(err, ledgererr.ErrReserveArchiveAlreadyDone):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, applied)
	assert.Equal(t, 1, refused)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM ledger_accounts WHERE owner_type = 'RESERVE'`))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM platform_adjustments WHERE kind = 'RESERVE_ARCHIVE'`))
	assert.Equal(t, before-40727, f.available(f.platform.UUID))
	assert.Equal(t, before-40727, f.cachedAvailable(f.platform.UUID))
	var reserveUUID string
	require.NoError(t, f.db.QueryRow(`SELECT uuid FROM ledger_accounts WHERE owner_type = 'RESERVE'`).Scan(&reserveUUID))
	assert.Equal(t, int64(40727), f.available(reserveUUID))

	// The DOKU-era income lines are what they were.
	incomesAfter, err := f.repos.ProductTransaction().GetPlatformIncomes(f.ctx, f.platform.UUID, nil, 100, true)
	require.NoError(t, err)
	require.Len(t, incomesAfter, len(incomesBefore))
	for i := range incomesBefore {
		assert.Equal(t, incomesBefore[i].PlatformAmount, incomesAfter[i].PlatformAmount, incomesBefore[i].Transaction.InvoiceNumber)
	}

	// The statement's ADJUSTMENT line, paged with the others under COLLATE "C".
	page, err := f.client.GetPlatformTransactions(f.ctx, PlatformTransactionsRequest{Type: PlatformTransactionAdjustment})
	require.NoError(t, err)
	require.Len(t, page.Transactions, 1)
	assert.Equal(t, int64(-40727), page.Transactions[0].Amount)
	assert.Len(t, page.Transactions[0].Adjustment.SourceTransactionUUIDs, 3)
	all, err := f.client.GetPlatformTransactions(f.ctx, PlatformTransactionsRequest{PageSize: 2, SortOrder: "ASC"})
	require.NoError(t, err)
	seen := len(all.Transactions)
	for all.HasMore {
		all, err = f.client.GetPlatformTransactions(f.ctx, PlatformTransactionsRequest{PageSize: 2, SortOrder: "ASC", Cursor: all.NextCursor})
		require.NoError(t, err)
		seen += len(all.Transactions)
	}
	assert.Equal(t, 5, seen, "four incomes and the archive")
}

func TestPlatformReconciliationPostgres_Constraints(t *testing.T) {
	f := newPgPlatformReconFixture(t)
	journal := domain.NewJournal(domain.EventTypeManualAdjustment, domain.SourceTypeManualAdjustment, "x", nil)
	require.NoError(t, f.repos.Journal().Save(f.ctx, journal))

	adj := func(mutate func(a *domain.PlatformAdjustment)) error {
		a := domain.NewPlatformAdjustment(f.platform.UUID, domain.PlatformAdjustmentRounding, 1)
		pool := int64(150)
		a.PoolBeforeMinor = &pool
		a.JournalUUID = journal.UUID
		mutate(a)
		return f.repos.PlatformReconciliation().SaveAdjustment(f.ctx, a)
	}
	assert.NoError(t, adj(func(a *domain.PlatformAdjustment) {}))
	assert.Error(t, adj(func(a *domain.PlatformAdjustment) { a.Amount = 2 }), "a rounding books at most its pool")
	assert.Error(t, adj(func(a *domain.PlatformAdjustment) { a.Amount = 0 }), "a rounding books at least a rupiah")
	assert.Error(t, adj(func(a *domain.PlatformAdjustment) {
		a.Kind = domain.PlatformAdjustmentDifference
		a.PoolBeforeMinor = nil
		a.AdminID = "admin-1"
	}), "a difference needs a reason and a check")
	assert.Error(t, adj(func(a *domain.PlatformAdjustment) { a.Kind = "BONUS" }))

	rec := domain.NewPlatformReconciliation(f.platform.UUID, reconPlatformULID, &domain.PlatformBalanceSnapshot{}, time.Now())
	rec.Status = domain.PlatformReconciliationMatched
	assert.Error(t, f.repos.PlatformReconciliation().SaveReconciliation(f.ctx, rec), "MATCHED needs S and a figure of 0")
	rec.Decide(0)
	assert.NoError(t, f.repos.PlatformReconciliation().SaveReconciliation(f.ctx, rec))
	assert.NoError(t, f.repos.PlatformReconciliation().MarkEmailed(f.ctx, rec.UUID, time.Now()))
	assert.True(t, ledgererr.IsAppError(f.repos.PlatformReconciliation().MarkEmailed(f.ctx, "nope", time.Now()), repo.ErrNotFound))

	_, err := f.repos.PlatformReconciliation().GetReserveArchive(f.ctx)
	assert.True(t, ledgererr.IsAppError(err, repo.ErrNotFound), fmt.Sprint(err))
}
