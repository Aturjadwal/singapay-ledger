package repo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// The daily statistics are the statement added up per day, so what these tests hold is that
// the two read the same transactions with the same amounts — the shared fragments, verbatim —
// and that the window reaches PostgreSQL as UTC instants, the only form a timestamp column
// without a zone compares correctly against.

// wib is a fixed +07:00 zone, so a window built in it is visibly not UTC.
var wib = time.FixedZone("WIB", 7*60*60)

// statisticsWindow is 1–19 October 2026 in WIB: [1 Oct 00:00 WIB, 20 Oct 00:00 WIB).
func statisticsWindow() domain.DailyWindow {
	return domain.DailyWindow{
		From:     time.Date(2026, 10, 1, 0, 0, 0, 0, wib),
		To:       time.Date(2026, 10, 20, 0, 0, 0, 0, wib),
		TimeZone: "Asia/Jakarta",
	}
}

// utcInstant matches a time argument that is the instant want AND is expressed in UTC. The
// zone matters, not only the instant: lib/pq sends the wall clock with its offset and
// PostgreSQL drops the offset when it compares with a timestamp column, so the same instant
// sent in +07:00 would be read seven hours late.
type utcInstant struct{ want time.Time }

func (a utcInstant) Match(v driver.Value) bool {
	t, ok := v.(time.Time)
	return ok && t.Location() == time.UTC && t.Equal(a.want)
}

// inOrder is a pattern that requires each fragment, verbatim, in this order.
func inOrder(fragments ...string) string {
	pattern := `(?s)`
	for i, fragment := range fragments {
		if i > 0 {
			pattern += `.*`
		}
		pattern += regexp.QuoteMeta(fragment)
	}
	return pattern
}

func TestPlatformEarningsByDay_SumsTheStatementsTransactionsPerDayAndSource(t *testing.T) {
	r, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	window := statisticsWindow()

	mock.ExpectQuery(inOrder(
		`SELECT to_char((COALESCE(pt.completed_at, pt.created_at) AT TIME ZONE 'UTC' AT TIME ZONE $4::text)::date, 'YYYY-MM-DD') AS day`,
		`pt.seller_account_id = $1 AS platform_sale`,
		`COALESCE(SUM(le.amount), 0)::bigint AS amount`,
		`COALESCE(SUM(le.amount) FILTER (WHERE le.balance_bucket = 'PENDING'), 0)::bigint AS awaiting_settlement`,
		`FROM product_transactions pt`,
		`JOIN ledger_entries le`,
		`ON `+platformEntriesSQL,
		`WHERE `+paidSQL,
		`AND `+creditsPlatformSQL,
		`AND COALESCE(pt.completed_at, pt.created_at) >= $2`,
		`AND COALESCE(pt.completed_at, pt.created_at) < $3`,
		`GROUP BY 1, 2`,
	)).
		WithArgs("platform-acc",
			utcInstant{time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)},
			utcInstant{time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)},
			"Asia/Jakarta").
		WillReturnRows(sqlmock.NewRows([]string{"day", "platform_sale", "amount", "awaiting_settlement"}).
			AddRow("2026-10-02", false, int64(9_749), int64(0)).
			AddRow("2026-10-02", true, int64(100_000), int64(100_000)).
			AddRow("2026-10-19", false, int64(10_000), int64(10_000)))

	days, err := r.PlatformEarningsByDay(context.Background(), "platform-acc", window)

	require.NoError(t, err)
	assert.Equal(t, []*domain.PlatformEarningsDay{
		{Date: "2026-10-02", PlatformSale: false, Amount: 9_749, AwaitingSettlement: 0},
		{Date: "2026-10-02", PlatformSale: true, Amount: 100_000, AwaitingSettlement: 100_000},
		{Date: "2026-10-19", PlatformSale: false, Amount: 10_000, AwaitingSettlement: 10_000},
	}, days)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPaidTransactionsByDay_CountsEveryPaidTransactionPerDayAndType(t *testing.T) {
	r, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	window := statisticsWindow()

	mock.ExpectQuery(inOrder(
		`SELECT to_char((COALESCE(pt.completed_at, pt.created_at) AT TIME ZONE 'UTC' AT TIME ZONE $3::text)::date, 'YYYY-MM-DD') AS day`,
		`pt.product_type`,
		`COUNT(*) AS n`,
		`COALESCE(SUM(pt.total_charged), 0)::bigint AS amount`,
		`FROM product_transactions pt`,
		`WHERE `+paidSQL,
		`AND COALESCE(pt.completed_at, pt.created_at) >= $1`,
		`AND COALESCE(pt.completed_at, pt.created_at) < $2`,
		`GROUP BY 1, 2`,
	)).
		WithArgs(
			utcInstant{time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)},
			utcInstant{time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)},
			"Asia/Jakarta").
		WillReturnRows(sqlmock.NewRows([]string{"day", "product_type", "n", "amount"}).
			AddRow("2026-10-02", "SERVICE", int64(3), int64(330_000)).
			AddRow("2026-10-02", "PHOTO", int64(1), int64(5_000)))

	days, err := r.PaidTransactionsByDay(context.Background(), window)

	require.NoError(t, err)
	assert.Equal(t, []*domain.PaidTransactionsDay{
		{Date: "2026-10-02", ProductType: "SERVICE", Count: 3, Amount: 330_000},
		{Date: "2026-10-02", ProductType: "PHOTO", Count: 1, Amount: 5_000},
	}, days)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// The transaction counts must not depend on the platform: they are every paid transaction,
// so the query carries neither the platform-income predicate nor the entries.
func TestPaidTransactionsByDay_DoesNotFilterOnThePlatform(t *testing.T) {
	r, _, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	var query string
	r.db = queryRecorder{record: func(q string) { query = q }}

	_, _ = r.PaidTransactionsByDay(context.Background(), statisticsWindow())

	require.NotEmpty(t, query)
	assert.NotContains(t, query, "seller_account_id")
	assert.NotContains(t, query, "platform_fee")
	assert.NotContains(t, query, "ledger_entries")
}

// The statement and the daily earnings share their rule, so a day of the chart is the day's
// statement lines. Both queries are captured and checked for the same fragments, verbatim.
func TestPlatformEarningsByDay_UsesTheStatementsRule(t *testing.T) {
	var statement, earnings string

	r, _, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	r.db = queryRecorder{record: func(q string) { statement = q }}
	_, _ = r.GetPlatformIncomes(context.Background(), "platform-acc", nil, 10, false)
	r.db = queryRecorder{record: func(q string) { earnings = q }}
	_, _ = r.PlatformEarningsByDay(context.Background(), "platform-acc", statisticsWindow())

	for _, fragment := range []string{paidSQL, creditsPlatformSQL, platformEntriesSQL, paidAtSQL} {
		assert.Contains(t, statement, fragment, "the statement reads %q", fragment)
		assert.Contains(t, earnings, fragment, "the daily earnings read %q", fragment)
	}
}

func TestPlatformStatistics_QueryAndScanFailuresAreRepositoryErrors(t *testing.T) {
	t.Run("earnings query", func(t *testing.T) {
		r, mock, closeDB := newMockProductTransactionRepo(t)
		defer closeDB()
		mock.ExpectQuery(`(?s)FROM product_transactions pt`).WillReturnError(errors.New("connection reset"))

		_, err := r.PlatformEarningsByDay(context.Background(), "platform-acc", statisticsWindow())

		assert.True(t, ledgererr.IsAppError(err, ErrFailedQuerySQL), "got %v", err)
	})

	t.Run("earnings scan", func(t *testing.T) {
		r, mock, closeDB := newMockProductTransactionRepo(t)
		defer closeDB()
		mock.ExpectQuery(`(?s)FROM product_transactions pt`).
			WillReturnRows(sqlmock.NewRows([]string{"day", "platform_sale", "amount", "awaiting_settlement"}).
				AddRow("2026-10-02", false, "not a number", int64(0)))

		_, err := r.PlatformEarningsByDay(context.Background(), "platform-acc", statisticsWindow())

		assert.True(t, ledgererr.IsAppError(err, ErrFailedScanSQL), "got %v", err)
	})

	t.Run("transactions query", func(t *testing.T) {
		r, mock, closeDB := newMockProductTransactionRepo(t)
		defer closeDB()
		mock.ExpectQuery(`(?s)FROM product_transactions pt`).WillReturnError(errors.New("connection reset"))

		_, err := r.PaidTransactionsByDay(context.Background(), statisticsWindow())

		assert.True(t, ledgererr.IsAppError(err, ErrFailedQuerySQL), "got %v", err)
	})
}

// An empty window answers an empty list, not nil: a caller ranging over it, or encoding it,
// sees "no days" either way, but nil is the shape of a failure.
func TestPlatformStatistics_NothingInTheWindowIsAnEmptyList(t *testing.T) {
	r, mock, closeDB := newMockProductTransactionRepo(t)
	defer closeDB()

	mock.ExpectQuery(`(?s)FROM product_transactions pt`).
		WillReturnRows(sqlmock.NewRows([]string{"day", "platform_sale", "amount", "awaiting_settlement"}))
	mock.ExpectQuery(`(?s)FROM product_transactions pt`).
		WillReturnRows(sqlmock.NewRows([]string{"day", "product_type", "n", "amount"}))

	earnings, err := r.PlatformEarningsByDay(context.Background(), "platform-acc", statisticsWindow())
	require.NoError(t, err)
	assert.NotNil(t, earnings)
	assert.Empty(t, earnings)

	paid, err := r.PaidTransactionsByDay(context.Background(), statisticsWindow())
	require.NoError(t, err)
	assert.NotNil(t, paid)
	assert.Empty(t, paid)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// queryRecorder is a DBTX that remembers the SQL it was asked to run and fails it, for
// tests that only care what was asked.
type queryRecorder struct {
	DBTX
	record func(query string)
}

var errRecorded = errors.New("recorded, not run")

func (q queryRecorder) QueryContext(_ context.Context, query string, _ ...any) (*sql.Rows, error) {
	q.record(query)
	return nil, errRecorded
}
