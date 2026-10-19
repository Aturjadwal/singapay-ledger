package ledger

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Platform statistics
//
// Two daily series for a dashboard: what the platform earned, and how many payments the
// gateway processed. Both are pure reads of this package's tables — no gateway call — and
// both date a transaction by when the payer paid, the key the platform statement is ordered
// by. One day of the two series is the same set of payments.
// ─────────────────────────────────────────────────────────────────────────────

// PlatformDailyRequest is a [From, To) window of instants, counted per calendar date of
// TimeZone, an IANA zone name such as "Asia/Jakarta". The library assumes no zone: the
// caller says which calendar its days are, and From and To are the instants its first day
// begins and the day after its last day begins, in that zone.
//
// The zone is checked against Go's zone database and applied by PostgreSQL's, so it must be
// known to both: a consumer binary without system zoneinfo needs `import _ "time/tzdata"`
// (aturjadwal-monoservice has it), or every request is refused.
//
// An empty or unknown TimeZone, a zero From or To, and a To before From are
// CodeInvalidRequest. From equal to To is an empty window.
type PlatformDailyRequest struct {
	From, To time.Time
	TimeZone string
}

// PlatformEarningsDayRow is what the platform account was credited by the payments of one
// date, from one source.
type PlatformEarningsDayRow struct {
	// Date is YYYY-MM-DD in the request's TimeZone.
	Date string

	// Source is PlatformIncomeSourceFee (the platform fee on a seller's sale) or
	// PlatformIncomeSourceSale (a sale the platform made itself), told apart as the
	// statement tells its income lines apart.
	Source PlatformIncomeSource

	// Amount is the sum of the platform account's ledger entries for those payments, whole
	// rupiah: the sum of the Amount of the statement's INCOME lines paid on that date. A
	// payment that has settled counts its settled figure, one that has not its priced fee.
	Amount int64

	// AwaitingSettlement is the part of Amount still in the PENDING bucket, the priced fee
	// of the payments not settled yet. It can move by a few rupiah when they settle and the
	// gateway's real fee replaces the estimate.
	AwaitingSettlement int64
}

// PaidTransactionsDayRow counts the payments of one product type paid on one date.
type PaidTransactionsDayRow struct {
	// Date is YYYY-MM-DD in the request's TimeZone.
	Date string

	// ProductType is the transaction's product_type as the consumer issued it.
	ProductType string

	// Count is the number of paid transactions (COMPLETED or SETTLED).
	Count int64

	// Amount is the sum of their TotalCharged: what the payers were charged, whole rupiah —
	// the seller's price plus the fees the payer bears under the transaction's fee model
	// (under GATEWAY_ON_SELLER, a web subscription's, the gateway fee is not in it). A QRIS
	// tip is not in it either: the ledger books a payment as priced.
	Amount int64
}

// PlatformEarningsByDay returns what the platform account was credited per date of the
// payment time and per source, over the window. Dates and sources with nothing are left out;
// the rows are ordered by Date, then Source.
//
// The figures are the platform statement's: a transaction counts when GetPlatformTransactions
// lists it as an INCOME line, with that line's Amount, on the date its OccurredAt falls on
// in TimeZone. So the rows of a window add up to the INCOME lines whose OccurredAt is in it.
//
// ErrLedgerNotFound means the platform account has not been provisioned.
func (c *LedgerClient) PlatformEarningsByDay(ctx context.Context, req PlatformDailyRequest) ([]PlatformEarningsDayRow, error) {
	window, err := req.window()
	if err != nil {
		return nil, err
	}

	account, err := c.GetPlatformAccount(ctx)
	if err != nil {
		return nil, err
	}

	days, err := c.repoProvider.ProductTransaction().PlatformEarningsByDay(ctx, account.UUID, window)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to sum the platform's earnings per day", err)
	}

	rows := make([]PlatformEarningsDayRow, 0, len(days))
	for _, day := range days {
		rows = append(rows, PlatformEarningsDayRow{
			Date:               day.Date,
			Source:             platformIncomeSource(day.PlatformSale),
			Amount:             day.Amount,
			AwaitingSettlement: day.AwaitingSettlement,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Date != rows[j].Date {
			return rows[i].Date < rows[j].Date
		}
		return rows[i].Source < rows[j].Source
	})

	return rows, nil
}

// PaidTransactionsByDay returns how many transactions were paid, and what their payers were
// charged, per date of the payment time and product type, over the window. Every paid
// transaction counts, whatever it credited the platform. Dates and types with nothing are
// left out; the rows are ordered by Date, then ProductType.
//
// It needs no platform account: the payments are there whether or not one is provisioned.
func (c *LedgerClient) PaidTransactionsByDay(ctx context.Context, req PlatformDailyRequest) ([]PaidTransactionsDayRow, error) {
	window, err := req.window()
	if err != nil {
		return nil, err
	}

	days, err := c.repoProvider.ProductTransaction().PaidTransactionsByDay(ctx, window)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to count the paid transactions per day", err)
	}

	rows := make([]PaidTransactionsDayRow, 0, len(days))
	for _, day := range days {
		rows = append(rows, PaidTransactionsDayRow{
			Date:        day.Date,
			ProductType: day.ProductType,
			Count:       day.Count,
			Amount:      day.Amount,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Date != rows[j].Date {
			return rows[i].Date < rows[j].Date
		}
		return rows[i].ProductType < rows[j].ProductType
	})

	return rows, nil
}

// window checks the request and turns it into the repository's window.
//
// The zone is checked here, against Go's zone database, so that a typo is the caller's 400
// rather than a failed query. "Local" is refused: it names the process's zone, which
// PostgreSQL has never heard of, and a day that moves with the server's setting is not a day
// a dashboard can show.
func (req PlatformDailyRequest) window() (domain.DailyWindow, error) {
	if req.TimeZone == "" || req.TimeZone == "Local" {
		return domain.DailyWindow{}, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"a time zone is required to say which calendar date a payment falls on", nil)
	}
	if _, err := time.LoadLocation(req.TimeZone); err != nil {
		return domain.DailyWindow{}, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			fmt.Sprintf("unknown time zone %q", req.TimeZone), err)
	}
	if req.From.IsZero() || req.To.IsZero() {
		return domain.DailyWindow{}, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"the window needs both a start and an end", nil)
	}
	if req.To.Before(req.From) {
		return domain.DailyWindow{}, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"the window ends before it starts", nil)
	}

	return domain.DailyWindow{From: req.From, To: req.To, TimeZone: req.TimeZone}, nil
}

// platformIncomeSource is why a paid transaction credited the platform: a sale the platform
// made itself, or the fee on a seller's. The statement's lines and the daily earnings both
// ask it, so the two cannot file one transaction under different sources.
func platformIncomeSource(platformIsTheSeller bool) PlatformIncomeSource {
	if platformIsTheSeller {
		return PlatformIncomeSourceSale
	}
	return PlatformIncomeSourceFee
}
