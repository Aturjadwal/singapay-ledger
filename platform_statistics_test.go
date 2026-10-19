package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// The platform statistics are the statement added up per day. These tests walk sales through
// the real money path — the money-in webhook, then settlement — and hold what a dashboard
// built on the two series relies on:
//
//   - a day is a calendar date of the caller's zone: 16:59:59Z and 17:00:00Z are two dates in
//     Asia/Jakarta and one in UTC;
//   - only paid transactions count, dated by completed_at, or created_at where a row has none;
//   - a sale awaiting settlement counts its priced fee, all of it awaiting settlement; once it
//     settles it counts the settled fee and nothing awaits;
//   - the platform's own sales and the fees on sellers' sales are told apart as the statement
//     tells them apart, and a day's earnings are exactly that day's INCOME lines;
//   - every paid transaction is counted, whatever it credited the platform, under its own
//     product type.
//
// The scenario runs against the in-memory fakes, and against PostgreSQL when
// LEDGER_TEST_DATABASE_URL names a database to work in (README_TESTING.md): the fakes prove
// the method's own logic, PostgreSQL that the SQL says the same.

const statisticsZone = "Asia/Jakarta"

// statisticsD is the scenario's first date, D. Times below are UTC; D's WIB day is
// [D-1 17:00Z, D 17:00Z).
var statisticsD = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func statisticsAt(day, hour, minute, second int) time.Time {
	return statisticsD.AddDate(0, 0, day).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute + time.Duration(second)*time.Second)
}

func statisticsDate(day int) string {
	return statisticsD.AddDate(0, 0, day).Format("2006-01-02")
}

// wibWindow is the WIB dates D+first .. D+last, both included, as instants.
func wibWindow(first, last int) PlatformDailyRequest {
	wib, _ := time.LoadLocation(statisticsZone)
	start := statisticsD.AddDate(0, 0, first)
	end := statisticsD.AddDate(0, 0, last+1)
	return PlatformDailyRequest{
		From:     time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, wib),
		To:       time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, wib),
		TimeZone: statisticsZone,
	}
}

// statisticsWorld is a ledger to run the scenario in: the client, and how to say when a
// transaction was paid. The money-in webhook stamps completed_at with the clock, so a
// scenario that needs a payment at 16:59:59Z re-dates it afterwards — in the fakes' map, or
// with an UPDATE.
type statisticsWorld struct {
	client      *LedgerClient
	datePayment func(t *testing.T, transactionUUID string, completedAt *time.Time, createdAt time.Time)
}

func fakeStatisticsWorld(t *testing.T) *statisticsWorld {
	t.Helper()
	fakes := NewFakeRepositoryProvider()
	return &statisticsWorld{
		client: &LedgerClient{
			txProvider:   NewFakeTransactionProvider(fakes),
			repoProvider: fakes,
			logger:       testLogger(),
			gateway:      &fakeGateway{verifyWebhook: func(singapay.WebhookRequest) error { return nil }},
		},
		datePayment: func(t *testing.T, transactionUUID string, completedAt *time.Time, createdAt time.Time) {
			tx, ok := fakes.productTransactionRepo.transactions[transactionUUID]
			require.True(t, ok, "transaction %s", transactionUUID)
			tx.CompletedAt = completedAt
			tx.CreatedAt = createdAt
		},
	}
}

// postgresStatisticsWorld builds the schema in a schema of its own inside
// LEDGER_TEST_DATABASE_URL, and drops it afterwards. Skipped without the variable.
func postgresStatisticsWorld(t *testing.T) *statisticsWorld {
	t.Helper()
	dsn := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set LEDGER_TEST_DATABASE_URL to run against PostgreSQL (README_TESTING.md)")
	}

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })

	schemaName := fmt.Sprintf("ledger_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schemaName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(schemaName) + ` CASCADE`)
	})

	// lib/pq passes search_path through as a run-time parameter of every connection the
	// pool opens, so the whole client works inside the test's schema.
	db, err := sql.Open("postgres", dsn+" search_path="+schemaName)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	schema, err := os.ReadFile("database/schemas/schema.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(schema))
	require.NoError(t, err, "database/schemas/schema.sql")

	return &statisticsWorld{
		client: NewLedgerClient(db, &fakeGateway{verifyWebhook: func(singapay.WebhookRequest) error { return nil }}, testLogger()),
		datePayment: func(t *testing.T, transactionUUID string, completedAt *time.Time, createdAt time.Time) {
			var completed any
			if completedAt != nil {
				completed = completedAt.UTC()
			}
			res, err := db.Exec(`UPDATE product_transactions SET completed_at = $1, created_at = $2 WHERE uuid = $3`,
				completed, createdAt.UTC(), transactionUUID)
			require.NoError(t, err)
			n, err := res.RowsAffected()
			require.NoError(t, err)
			require.Equal(t, int64(1), n, "transaction %s", transactionUUID)
		},
	}
}

// statisticsScenario is the ledger the scenario builds: who sold, and the transactions.
type statisticsScenario struct {
	world    *statisticsWorld
	platform *domain.Account
	sellerA  *domain.Account
	sellerB  *domain.Account
	invoices int
}

func newStatisticsScenario(t *testing.T, world *statisticsWorld) *statisticsScenario {
	t.Helper()
	ctx := context.Background()
	accounts := world.client.repoProvider.Account()

	platform := domain.NewPlatformAccount("SP-PLATFORM-ULID", "platform", domain.CurrencyIDR)
	platform.SetSingapayAccount("SP-PLATFORM-ULID", "000000000999")
	gateway := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
	sellerA := domain.NewSellerAccount("SP-SELLER-A", "seller-a", domain.CurrencyIDR)
	sellerA.SetSingapayAccount("SP-SELLER-A", "000000000101")
	sellerB := domain.NewSellerAccount("SP-SELLER-B", "seller-b", domain.CurrencyIDR)
	sellerB.SetSingapayAccount("SP-SELLER-B", "000000000102")
	for _, account := range []*domain.Account{&platform, &gateway, &sellerA, &sellerB} {
		require.NoError(t, accounts.Save(ctx, account))
	}

	return &statisticsScenario{world: world, platform: &platform, sellerA: &sellerA, sellerB: &sellerB}
}

// issue writes a PENDING transaction and its payment request, as GeneratePayment would.
func (s *statisticsScenario) issue(t *testing.T, seller *domain.Account, productType string, fee domain.FeeBreakdown) *domain.ProductTransaction {
	t.Helper()
	ctx := context.Background()
	s.invoices++
	invoice := fmt.Sprintf("INV-STAT-%03d-%d", s.invoices, time.Now().UnixNano()%1e9)

	tx := domain.NewProductTransaction("buyer-1", seller.UUID, "product-1", productType, invoice, fee, nil)
	require.NoError(t, s.world.client.repoProvider.ProductTransaction().Save(ctx, tx))

	request := domain.NewPaymentRequest(tx.UUID, "SP-QR-"+invoice, "QRIS", fee.TotalCharged, domain.CurrencyIDR, tx.CreatedAt.Add(time.Hour))
	require.NoError(t, s.world.client.repoProvider.PaymentRequest().Save(ctx, request))
	return tx
}

// pay books the money-in webhook for the transaction, then dates the payment.
func (s *statisticsScenario) pay(t *testing.T, tx *domain.ProductTransaction, paidAt time.Time) *domain.ProductTransaction {
	t.Helper()
	ctx := context.Background()
	s.invoices++
	body := []byte(fmt.Sprintf(`{
		"status": 200, "success": true, "event": "qris-acquirer-transaction",
		"data": {"transaction": {"id": %d, "reff_no": "REF-%d", "merchant_reff_no": %q, "type": "qris", "status": "paid",
		                         "amount": %d, "total_amount": %d},
		         "payment": {"method": "qris"}}
	}`, 1000+s.invoices, s.invoices, tx.InvoiceNumber, tx.Fee.TotalCharged, tx.Fee.TotalCharged))
	require.NoError(t, s.world.client.HandlePaymentSuccess(ctx, webhookRequest(body)))

	paid := paidAt
	s.world.datePayment(t, tx.UUID, &paid, paidAt.Add(-time.Minute))
	return s.reload(t, tx)
}

// settle books the settlement with the fee the gateway really took, in sen.
func (s *statisticsScenario) settle(t *testing.T, tx *domain.ProductTransaction, actualFeeMinor int64, channel string) {
	t.Helper()
	gross := domain.RupiahToMinor(tx.Fee.TotalCharged)
	outcome, err := s.world.client.bookSettlement(context.Background(), s.reload(t, tx), domain.SettledTransaction{
		MerchantReference:    tx.InvoiceNumber,
		GatewayTransactionID: "SP-TX-" + tx.InvoiceNumber,
		GatewayAccountID:     "SP-ACCOUNT",
		PaymentChannel:       channel,
		GrossMinor:           gross,
		NetMinor:             gross - actualFeeMinor,
		FeeMinor:             actualFeeMinor,
		FeeReported:          channel != ChannelPaymentLink,
	})
	require.NoError(t, err)
	require.Equal(t, settleOutcomeSettled, outcome)
}

func (s *statisticsScenario) reload(t *testing.T, tx *domain.ProductTransaction) *domain.ProductTransaction {
	t.Helper()
	fresh, err := s.world.client.repoProvider.ProductTransaction().GetByID(context.Background(), tx.UUID)
	require.NoError(t, err)
	return fresh
}

func onCustomer(price, platformFee, gatewayFee int64) domain.FeeBreakdown {
	fee, _ := domain.NewFeeBreakdown(price, platformFee, gatewayFee, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
	return *fee
}

// statementByDay pages through the statement's INCOME lines and adds up, per date of
// OccurredAt in the zone and per source, those whose OccurredAt is in the window. It is the
// plan's invariant written from the other side: what the earnings must equal.
func statementByDay(t *testing.T, client *LedgerClient, req PlatformDailyRequest) map[string]int64 {
	t.Helper()
	location, err := time.LoadLocation(req.TimeZone)
	require.NoError(t, err)

	sums := map[string]int64{}
	cursor := ""
	for {
		page, err := client.GetPlatformTransactions(context.Background(), PlatformTransactionsRequest{
			Type: PlatformTransactionIncome, Cursor: cursor, PageSize: 3, SortOrder: "ASC",
		})
		require.NoError(t, err)
		for _, line := range page.Transactions {
			if line.OccurredAt.Before(req.From) || !line.OccurredAt.Before(req.To) {
				continue
			}
			sums[line.OccurredAt.In(location).Format("2006-01-02")+" "+string(line.Income.Source)] += line.Amount
		}
		if !page.HasMore {
			return sums
		}
		cursor = page.NextCursor
	}
}

func earningsByKey(rows []PlatformEarningsDayRow) map[string]int64 {
	sums := map[string]int64{}
	for _, row := range rows {
		sums[row.Date+" "+string(row.Source)] += row.Amount
	}
	return sums
}

func TestPlatformStatistics_AgainstTheFakes(t *testing.T) {
	runPlatformStatisticsScenario(t, fakeStatisticsWorld(t))
}

func TestPlatformStatistics_AgainstPostgres(t *testing.T) {
	runPlatformStatisticsScenario(t, postgresStatisticsWorld(t))
}

func runPlatformStatisticsScenario(t *testing.T, world *statisticsWorld) {
	ctx := context.Background()
	s := newStatisticsScenario(t, world)
	client := world.client

	// D, 23:59:59 WIB: a booking payment, paid and not settled.
	lateOnD := s.pay(t, s.issue(t, s.sellerA, "SERVICE", onCustomer(100_000, 10_000, 4_000)), statisticsAt(0, 16, 59, 59))
	// D+1, 00:00:00 WIB — one second later: a voucher, settled with a gateway fee 250.37
	// above the estimate, which the platform absorbs: 5,000 − 250.37 = 4,749.63, booked as
	// 4,749 (the 63 sen are the platform residual, outside the ledger).
	firstOnD1 := s.pay(t, s.issue(t, s.sellerA, "VOUCHER", onCustomer(50_000, 5_000, 2_000)), statisticsAt(0, 17, 0, 0))
	s.settle(t, firstOnD1, 225_037, "QRIS")
	// D+1: a web subscription, the platform's own sale: no fee, the platform is the seller
	// and is credited the net. Not settled.
	subscriptionFee, err := domain.NewFeeBreakdown(99_000, 0, 3_000, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
	require.NoError(t, err)
	subscription := s.pay(t, s.issue(t, s.platform, "SUBSCRIPTION", *subscriptionFee), statisticsAt(1, 3, 0, 0))
	// D+1: a cashier QRIS, settled at the estimate, from a row that predates completed_at:
	// it is dated by created_at.
	legacy := s.pay(t, s.issue(t, s.sellerB, "CASHIER_QRIS", onCustomer(40_000, 2_000, 280)), statisticsAt(1, 4, 0, 0))
	s.settle(t, legacy, domain.RupiahToMinor(280), "QRIS")
	world.datePayment(t, legacy.UUID, nil, statisticsAt(1, 4, 0, 0))
	// D+1: a sale with no platform fee: counted as a transaction, earns the platform nothing.
	noFee := s.pay(t, s.issue(t, s.sellerB, "SERVICE", onCustomer(20_000, 0, 1_000)), statisticsAt(1, 6, 0, 0))
	// D+1: a product type the consumer may add later: counted under its own name.
	photo := s.pay(t, s.issue(t, s.sellerA, "PHOTO", onCustomer(10_000, 1_000, 70)), statisticsAt(1, 7, 0, 0))
	// D+1: issued and never paid. PENDING forever: neither series sees it.
	unpaid := s.issue(t, s.sellerB, "SERVICE", onCustomer(70_000, 7_000, 490))
	world.datePayment(t, unpaid.UUID, nil, statisticsAt(1, 5, 0, 0))
	// Either side of the window D..D+2 (WIB): 16:59:59Z the day before D, and 17:00:00Z on
	// D+2, which is D+3 in WIB.
	s.pay(t, s.issue(t, s.sellerA, "SERVICE", onCustomer(30_000, 3_000, 210)), statisticsAt(-1, 16, 59, 59))
	s.pay(t, s.issue(t, s.sellerA, "SERVICE", onCustomer(30_000, 3_000, 210)), statisticsAt(2, 17, 0, 0))

	window := wibWindow(0, 2)

	t.Run("earnings per WIB date and source, the settled figure once settled", func(t *testing.T) {
		rows, err := client.PlatformEarningsByDay(ctx, window)
		require.NoError(t, err)
		assert.Equal(t, []PlatformEarningsDayRow{
			{Date: statisticsDate(0), Source: PlatformIncomeSourceFee, Amount: 10_000, AwaitingSettlement: 10_000},
			// 4,749 settled voucher + 2,000 settled cashier QRIS + 1,000 photo awaiting.
			{Date: statisticsDate(1), Source: PlatformIncomeSourceFee, Amount: 4_749 + 2_000 + 1_000, AwaitingSettlement: 1_000},
			{Date: statisticsDate(1), Source: PlatformIncomeSourceSale, Amount: 96_000, AwaitingSettlement: 96_000},
		}, rows)
	})

	t.Run("every paid transaction per WIB date and product type", func(t *testing.T) {
		rows, err := client.PaidTransactionsByDay(ctx, window)
		require.NoError(t, err)
		assert.Equal(t, []PaidTransactionsDayRow{
			{Date: statisticsDate(0), ProductType: "SERVICE", Count: 1, Amount: lateOnD.Fee.TotalCharged},
			{Date: statisticsDate(1), ProductType: "CASHIER_QRIS", Count: 1, Amount: legacy.Fee.TotalCharged},
			{Date: statisticsDate(1), ProductType: "PHOTO", Count: 1, Amount: photo.Fee.TotalCharged},
			{Date: statisticsDate(1), ProductType: "SERVICE", Count: 1, Amount: noFee.Fee.TotalCharged},
			{Date: statisticsDate(1), ProductType: "SUBSCRIPTION", Count: 1, Amount: subscription.Fee.TotalCharged},
			{Date: statisticsDate(1), ProductType: "VOUCHER", Count: 1, Amount: firstOnD1.Fee.TotalCharged},
		}, rows)
	})

	t.Run("a day's earnings are that day's INCOME lines of the statement", func(t *testing.T) {
		rows, err := client.PlatformEarningsByDay(ctx, window)
		require.NoError(t, err)
		assert.Equal(t, statementByDay(t, client, window), earningsByKey(rows))

		// And over a window wide enough to hold every sale, including the two outside D..D+2.
		wide := wibWindow(-3, 5)
		rows, err = client.PlatformEarningsByDay(ctx, wide)
		require.NoError(t, err)
		assert.Equal(t, statementByDay(t, client, wide), earningsByKey(rows))
		var total int64
		for _, row := range rows {
			total += row.Amount
		}
		assert.Equal(t, int64(10_000+4_749+96_000+2_000+1_000+3_000+3_000), total)
	})

	t.Run("the zone decides the date: in UTC 16:59:59Z and 17:00:00Z are one day", func(t *testing.T) {
		utc := PlatformDailyRequest{From: window.From, To: window.To, TimeZone: "UTC"}
		rows, err := client.PaidTransactionsByDay(ctx, utc)
		require.NoError(t, err)
		perDate := map[string]int64{}
		for _, row := range rows {
			perDate[row.Date] += row.Count
		}
		assert.Equal(t, map[string]int64{statisticsDate(0): 2, statisticsDate(1): 4}, perDate)

		earnings, err := client.PlatformEarningsByDay(ctx, utc)
		require.NoError(t, err)
		assert.Equal(t, statementByDay(t, client, utc), earningsByKey(earnings))
	})

	t.Run("settling moves a day from awaiting to the settled figure", func(t *testing.T) {
		// The gateway took 4,120 instead of the estimated 4,000: the platform's 10,000 fee
		// becomes 9,880, and nothing of D awaits settlement any more.
		s.settle(t, lateOnD, domain.RupiahToMinor(4_120), "QRIS")
		rows, err := client.PlatformEarningsByDay(ctx, wibWindow(0, 0))
		require.NoError(t, err)
		assert.Equal(t, []PlatformEarningsDayRow{
			{Date: statisticsDate(0), Source: PlatformIncomeSourceFee, Amount: 9_880, AwaitingSettlement: 0},
		}, rows)
		assert.Equal(t, statementByDay(t, client, wibWindow(0, 0)), earningsByKey(rows))
	})

	t.Run("an empty window answers no rows", func(t *testing.T) {
		empty := wibWindow(10, 12)
		earnings, err := client.PlatformEarningsByDay(ctx, empty)
		require.NoError(t, err)
		assert.Empty(t, earnings)
		assert.NotNil(t, earnings)

		paid, err := client.PaidTransactionsByDay(ctx, empty)
		require.NoError(t, err)
		assert.Empty(t, paid)
		assert.NotNil(t, paid)

		zeroLength := PlatformDailyRequest{From: window.From, To: window.From, TimeZone: statisticsZone}
		paid, err = client.PaidTransactionsByDay(ctx, zeroLength)
		require.NoError(t, err)
		assert.Empty(t, paid)
	})
}

func TestPlatformDailyRequest_RefusesWhatItCannotDate(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)

	cases := map[string]PlatformDailyRequest{
		"no zone":            {From: from, To: to},
		"the process's zone": {From: from, To: to, TimeZone: "Local"},
		"an unknown zone":    {From: from, To: to, TimeZone: "Asia/Atlantis"},
		"no start":           {To: to, TimeZone: statisticsZone},
		"no end":             {From: from, TimeZone: statisticsZone},
		"an end before":      {From: to, To: from, TimeZone: statisticsZone},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			client := fakeStatisticsWorld(t).client

			_, err := client.PlatformEarningsByDay(context.Background(), req)
			assert.Equal(t, ledgererr.CodeInvalidRequest, outerCode(t, err))

			_, err = client.PaidTransactionsByDay(context.Background(), req)
			assert.Equal(t, ledgererr.CodeInvalidRequest, outerCode(t, err))
		})
	}
}

// The earnings are the platform account's, so without one there is nothing to read them
// from. The payments are there all the same, and are counted.
func TestPlatformStatistics_WithoutAPlatformAccount(t *testing.T) {
	world := fakeStatisticsWorld(t)
	window := wibWindow(0, 2)

	_, err := world.client.PlatformEarningsByDay(context.Background(), window)
	assert.Equal(t, ledgererr.CodeLedgerNotFound, outerCode(t, err))

	rows, err := world.client.PaidTransactionsByDay(context.Background(), window)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// A failed read is the caller's 500, not a 400 or a not-found: the request was fine.
func TestPlatformStatistics_AFailedReadIsInternal(t *testing.T) {
	world := postgresStatisticsWorld(t)
	newStatisticsScenario(t, world)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := world.client.PaidTransactionsByDay(ctx, wibWindow(0, 2))
	require.Error(t, err)
	assert.Equal(t, ledgererr.CodeInternal, outerCode(t, err))
	assert.True(t, strings.Contains(err.Error(), "paid transactions"), err.Error())
}
