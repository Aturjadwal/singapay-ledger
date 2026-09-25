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
