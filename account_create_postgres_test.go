package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// CreateAccount against PostgreSQL, with callers that really run at once. Like the platform
// statistics, it runs only when LEDGER_TEST_DATABASE_URL names a database (README_TESTING.md):
// each test builds database/schemas/schema.sql in a schema of its own and drops it afterwards.
// LEDGER_TEST_MAX_OPEN_CONNS sizes the pool (default 2, the consuming service's ledger pool in
// production).
//
// Point the URL at a transaction-mode pooler (lib/pq with binary_parameters=yes, as the consuming
// service connects; the pooler must pass search_path through, as Supavisor does) to prove the
// same through it: the creation lock lives and dies with one transaction, so a pooler that hands
// each transaction a different server connection changes nothing.

func ledgerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return ledgerTestDBWith(t, "", true)
}

// ledgerTestDBWith is ledgerTestDB with more connection parameters (extra) and, when
// withSellerIndex is false, without migration 031's unique index — the database the creation
// lock must hold on alone, before 031 is applied.
func ledgerTestDBWith(t *testing.T, extra string, withSellerIndex bool) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set LEDGER_TEST_DATABASE_URL to run against PostgreSQL (README_TESTING.md)")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	schemaName := fmt.Sprintf("ledger_race_%d", time.Now().UnixNano())
	_, err = admin.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schemaName))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(schemaName) + ` CASCADE`) })

	db, err := sql.Open("postgres", dsn+" search_path="+schemaName+" "+extra)
	require.NoError(t, err)
	conns := 2
	if raw := os.Getenv("LEDGER_TEST_MAX_OPEN_CONNS"); raw != "" {
		conns, err = strconv.Atoi(raw)
		require.NoError(t, err)
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	t.Cleanup(func() { db.Close() })

	schema, err := os.ReadFile("database/schemas/schema.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(schema))
	require.NoError(t, err, "database/schemas/schema.sql")
	if !withSellerIndex {
		_, err = db.Exec(`DROP INDEX idx_accounts_unique_seller`)
		require.NoError(t, err)
	}
	return db
}

// raceOwner is an owner id of this test's own (the advisory lock is database-wide).
func raceOwner(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("ledger-race-test-%d-%d", time.Now().UnixNano(), rand.Intn(1e6))
}

func ownerRows(t *testing.T, db *sql.DB, owner string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ledger_accounts WHERE owner_type = 'SELLER' AND owner_id = $1`, owner).Scan(&n))
	return n
}

// lockIsFree reports whether a new transaction can take the owner's creation lock: the
// lock of every finished creation must be gone, through a pooler as much as directly.
func lockIsFree(t *testing.T, db *sql.DB, owner string) bool {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	var free bool
	require.NoError(t, tx.QueryRow(`SELECT pg_try_advisory_xact_lock(hashtextextended('ledger_accounts:SELLER:' || $1::text, 0))`, owner).Scan(&free))
	return free
}

// slowGateway opens sub-accounts after a delay, counting the calls and how many ran at once.
// fail decides, per call (1-based), whether it is refused; hang makes a call wait for its
// caller to give up.
type slowGateway struct {
	*fakeGateway
	delay  time.Duration
	fail   func(call int) bool
	hang   bool
	inCall chan struct{} // receives once, when the first call starts

	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
}

func newSlowGateway(delay time.Duration) *slowGateway {
	return &slowGateway{fakeGateway: &fakeGateway{}, delay: delay, inCall: make(chan struct{}, 1)}
}

func (g *slowGateway) CreateAccount(ctx context.Context, req singapay.CreateAccountRequest) (*singapay.Account, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.inFlight++
	g.maxInFlight = max(g.maxInFlight, g.inFlight)
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.inFlight--; g.mu.Unlock() }()
	select {
	case g.inCall <- struct{}{}:
	default:
	}

	wait := g.delay
	if g.hang {
		wait = time.Hour
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if g.fail != nil && g.fail(call) {
		return nil, &singapay.Error{StatusCode: 503, Message: "service unavailable"}
	}
	id := fmt.Sprintf("01RACE%d%06d", time.Now().UnixNano(), call)
	return &singapay.Account{ID: id, Number: fmt.Sprintf("88%010d", rand.Int63n(1e10)), Status: singapay.AccountActive}, nil
}

func (g *slowGateway) stats() (calls, maxInFlight int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.maxInFlight
}

type createOutcome struct {
	account *domain.Account
	err     error
}

// createTogether runs n CreateAccount calls for owner at the same instant, each with its own
// deadline.
func createTogether(client *LedgerClient, owner string, n int, deadline time.Duration) []createOutcome {
	outcomes := make([]createOutcome, n)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			account, err := client.CreateAccount(ctx, owner, "Race Seller", domain.CurrencyIDR)
			outcomes[i] = createOutcome{account, err}
		}(i)
	}
	close(gate)
	wg.Wait()
	return outcomes
}

// tally counts the outcomes: created, told the account exists, refused by the gateway, gave up
// waiting, anything else.
func tally(outcomes []createOutcome) (created, exists, gateway, waited int, other []error) {
	for _, o := range outcomes {
		switch {
		case o.err == nil && o.account != nil:
			created++
		case ledgererr.IsAppError(o.err, ledgererr.ErrLedgerAlreadyExists):
			exists++
		case ledgererr.IsErrorCode(ledgererr.CodeGatewayAPIError, o.err):
			gateway++
		case ledgererr.IsErrorCode(ledgererr.CodeAccountCreationInProgress, o.err):
			waited++
		default:
			other = append(other, o.err)
		}
	}
	return
}

func TestCreateAccountPostgres_CallersAtOnceOpenOneSubAccount(t *testing.T) {
	t.Run("with migration 031", func(t *testing.T) { callersAtOnce(t, ledgerTestDBWith(t, "", true)) })
	t.Run("before migration 031: the lock alone", func(t *testing.T) { callersAtOnce(t, ledgerTestDBWith(t, "", false)) })
}

func callersAtOnce(t *testing.T, db *sql.DB) {
	owner := raceOwner(t)
	gateway := newSlowGateway(400 * time.Millisecond)
	client := NewLedgerClient(db, gateway, testLogger())
	const n = 24

	// While the holder waits on Singapay, the pool still serves other work: the waiters hold
	// no connection.
	probe := make(chan time.Duration, 1)
	go func() {
		<-gateway.inCall
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		var one int
		if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			probe <- -1
			return
		}
		probe <- time.Since(start)
	}()

	outcomes := createTogether(client, owner, n, 15*time.Second)

	created, exists, refused, waited, other := tally(outcomes)
	assert.Equal(t, 1, created, "one caller created the account")
	assert.Equal(t, n-1, exists, "every other caller was told it exists")
	assert.Zero(t, refused+waited)
	assert.Empty(t, other)
	calls, maxInFlight := gateway.stats()
	assert.Equal(t, 1, calls, "Singapay was asked for one sub-account")
	assert.Equal(t, 1, maxInFlight)
	assert.Equal(t, 1, ownerRows(t, db, owner), "one ledger row for the seller")
	assert.True(t, lockIsFree(t, db, owner), "the creation lock went with its transaction")
	select {
	case took := <-probe:
		require.GreaterOrEqual(t, took, time.Duration(0), "the probe query failed")
		assert.Less(t, took, 300*time.Millisecond, "a query during the Singapay call waited %s for a connection", took)
	case <-time.After(5 * time.Second):
		t.Error("the probe never ran")
	}
}

// The first holder's Singapay call is refused: it leaves no row, and one of the waiting
// callers asks again and creates the account.
func TestCreateAccountPostgres_AFailedHolderLeavesNothingAndAWaiterCreates(t *testing.T) {
	db := ledgerTestDB(t)
	owner := raceOwner(t)
	gateway := newSlowGateway(200 * time.Millisecond)
	gateway.fail = func(call int) bool { return call == 1 }
	client := NewLedgerClient(db, gateway, testLogger())
	const n = 12

	outcomes := createTogether(client, owner, n, 15*time.Second)

	created, exists, refused, waited, other := tally(outcomes)
	assert.Equal(t, 1, refused, "the holder whose call was refused")
	assert.Equal(t, 1, created, "the waiter that took over")
	assert.Equal(t, n-2, exists)
	assert.Zero(t, waited)
	assert.Empty(t, other)
	calls, maxInFlight := gateway.stats()
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, maxInFlight, "never two Singapay calls for one seller at once")
	assert.Equal(t, 1, ownerRows(t, db, owner))
	assert.True(t, lockIsFree(t, db, owner))
}

// Singapay does not answer before the callers' deadlines: every caller fails, at most one asks
// Singapay at a time, nothing is written — and a later call creates the account.
func TestCreateAccountPostgres_ATimeoutLeavesNothingAndALaterCallCreates(t *testing.T) {
	db := ledgerTestDB(t)
	owner := raceOwner(t)
	gateway := newSlowGateway(0)
	gateway.hang = true
	client := NewLedgerClient(db, gateway, testLogger())
	const n = 8

	outcomes := createTogether(client, owner, n, 500*time.Millisecond)

	created, exists, refused, waited, other := tally(outcomes)
	assert.Zero(t, created+exists)
	assert.GreaterOrEqual(t, refused, 1, "the holder's call was cut by its deadline")
	assert.Equal(t, n, refused+waited, "the others gave up waiting: %v", other)
	_, maxInFlight := gateway.stats()
	assert.Equal(t, 1, maxInFlight, "never two Singapay calls for one seller at once")
	assert.Zero(t, ownerRows(t, db, owner), "no half-made account")
	assert.True(t, lockIsFree(t, db, owner))

	gateway.hang = false
	gateway.delay = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	account, err := client.CreateAccount(ctx, owner, "Race Seller", domain.CurrencyIDR)
	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Equal(t, 1, ownerRows(t, db, owner))
}

// The server would end an idle transaction after 200 ms; the creating transaction waits on a
// 500 ms Singapay call with nothing running, and still commits: it sets its own bound past
// the caller's deadline, so the lock is never let go mid-call.
func TestCreateAccountPostgres_AShortServerIdleLimitDoesNotEndTheCreation(t *testing.T) {
	db := ledgerTestDBWith(t, "idle_in_transaction_session_timeout=200", true)
	var limit string
	require.NoError(t, db.QueryRow(`SHOW idle_in_transaction_session_timeout`).Scan(&limit))
	if limit != "200ms" {
		t.Skipf("the connection's idle_in_transaction_session_timeout is %q: this pooler does not pass run-time parameters", limit)
	}
	owner := raceOwner(t)
	gateway := newSlowGateway(500 * time.Millisecond)
	client := NewLedgerClient(db, gateway, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	account, err := client.CreateAccount(ctx, owner, "Race Seller", domain.CurrencyIDR)

	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Equal(t, 1, ownerRows(t, db, owner))
}
