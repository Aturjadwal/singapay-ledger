package domain

import "time"

// KeysetCursor is a position in a list ordered by (At, ID): the last row a page ended on.
//
// It exists for a list that spans more than one table. The RandId cursors elsewhere in
// this package look their row up again to learn its timestamp, which only works while every
// row comes from the table the lookup reads. A cursor that carries its own sort key can
// continue a list merged from two tables — the platform statement, which interleaves money
// in from product_transactions with money out from disbursements.
//
// ID breaks ties between rows stamped in the same microsecond. It is the row's UUID, unique
// across both tables, so (At, ID) is a total order over the merged list. Repositories
// compare it byte-wise (COLLATE "C"), the same way Go compares strings, so the order the
// database pages in and the order the merge interleaves in cannot disagree.
type KeysetCursor struct {
	At time.Time
	ID string
}

// PlatformIncome is one paid product transaction that credited the platform account, with
// what it credited. It has no table of its own — a value type, like SettledTransaction.
//
// Two kinds of transaction credit the platform: a seller's sale, which pays the platform its
// fee, and a sale the platform makes itself (a subscription), where the platform account is
// the seller and receives the whole net.
//
// PlatformAmount is the sum of the platform account's ledger entries for the transaction,
// not a figure recomputed from the pricing columns. While the transaction awaits settlement
// that is the PENDING credit written at payment time; once it settles, the pending legs
// cancel out and what remains is the AVAILABLE credit the settlement booked — the priced fee
// adjusted by whatever the gateway really took. Reading it off the entries is what makes a
// statement add up to the balance it explains.
type PlatformIncome struct {
	Transaction    *ProductTransaction
	PlatformAmount int64
}

// DailyWindow is a [From, To) window of instants to be counted per calendar date of
// TimeZone, an IANA zone name such as "Asia/Jakarta". From and To are compared with the
// payment time as UTC instants; TimeZone only decides which date an instant falls on.
type DailyWindow struct {
	From     time.Time
	To       time.Time
	TimeZone string
}

// PlatformEarningsDay is what the platform account was credited by the transactions paid on
// one date, from one kind of sale. It has no table of its own — a value type, like
// PlatformIncome, and the same figures: Amount is the sum, over those transactions, of the
// PlatformAmount a statement line shows.
type PlatformEarningsDay struct {
	// Date is the calendar date of the payment time in the window's zone, YYYY-MM-DD.
	Date string

	// PlatformSale is true for the platform's own sales (the platform is the seller) and false
	// for the platform fee on a seller's sale.
	PlatformSale bool

	// Amount is the sum of the platform account's ledger entries for those transactions,
	// whole rupiah.
	Amount int64

	// AwaitingSettlement is the part of Amount still in the PENDING bucket: the priced credit
	// of transactions that have not settled. Settlement clears it and books the settled
	// figure as AVAILABLE, so it is zero for a settled transaction.
	AwaitingSettlement int64
}

// PaidTransactionsDay counts the transactions of one product type paid on one date.
type PaidTransactionsDay struct {
	// Date is the calendar date of the payment time in the window's zone, YYYY-MM-DD.
	Date        string
	ProductType string
	Count       int64

	// Amount is the sum of total_charged: what the payers were charged, whole rupiah.
	Amount int64
}
