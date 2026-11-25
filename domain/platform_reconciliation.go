package domain

import (
	"context"
	"strings"
	"time"

	"github.com/21strive/redifu"
)

// ─────────────────────────────────────────────────────────────────────────────
// Platform balance reconciliation (migration 034, v0.13.0)
// ─────────────────────────────────────────────────────────────────────────────
//
// The platform's ledger account and its Singapay sub-account hold the same money in two
// places, and the two can only agree through an equation, never by being equal:
//
//	unexplained = S − L − K + B                                   (all in sen)
//
//	S  the sub-account's available balance at Singapay
//	L  the platform's AVAILABLE balance in the ledger (Σ of its entries, × 100)
//	K  the residual pool: sen the sweep moved that no whole-rupiah entry recorded
//	   = Σ platform_residual of swept rows + Σ residual_minor of adjustments − Σ roundings × 100
//	B  platform fee settled but not swept yet, at the whole rupiah the ledger holds for it
//	   (its SETTLEMENT_NET and FEE_ADJUSTMENT entries), × 100
//
// Every term is recomputed from rows that never change once written — entries, swept rows,
// adjustments — never from a running counter, so the figure for a given moment can always be
// produced again. Normal operation keeps it at exactly 0: a settlement moves L and B by the
// same amount, a sweep moves S by the fee in sen and B and K by its two parts, a rounding moves
// L and K by the same rupiah. Anything else is unexplained and is a person's to look at; it is
// never booked by itself (docs/106-platform-balance-reconciliation.md).

// OwnerTypeReserve is an archive account: money the ledger keeps a record of but that no
// sub-account holds and no total counts. The CHECK on ledger_accounts.owner_type has allowed it
// from the start; ArchiveToReserve creates the first one (the DOKU era).
const OwnerTypeReserve OwnerType = "RESERVE"

// ReserveArchiveOwnerID is the owner_id of the RESERVE account that holds the platform's DOKU
// era.
const ReserveArchiveOwnerID = "PLATFORM_DOKU_ARCHIVE"

// PlatformAdjustmentKind is why an adjustment moved the platform's AVAILABLE balance.
type PlatformAdjustmentKind string

const (
	// PlatformAdjustmentRounding books whole rupiah out of the residual pool: the sen the
	// sweeps moved and the ledger could not record, once they add up to at least Rp1.
	PlatformAdjustmentRounding PlatformAdjustmentKind = "ROUNDING"

	// PlatformAdjustmentDifference books the unexplained figure of one check, by an admin,
	// with a reason. Its whole rupiah go to the ledger and its sen to the pool.
	PlatformAdjustmentDifference PlatformAdjustmentKind = "DIFFERENCE"

	// PlatformAdjustmentReserveArchive moves the DOKU era out of the platform account into a
	// RESERVE archive account, once.
	PlatformAdjustmentReserveArchive PlatformAdjustmentKind = "RESERVE_ARCHIVE"
)

// PlatformAdjustmentReason is why an admin booked a difference.
type PlatformAdjustmentReason string

const (
	// PlatformAdjustmentReasonTestFunds: money a test moved into the sub-account (CSV7MW, a
	// staging run on production credentials).
	PlatformAdjustmentReasonTestFunds PlatformAdjustmentReason = "TEST_FUNDS"
	// PlatformAdjustmentReasonOffSystemWithdrawal: money taken out of the sub-account outside
	// this system, from Singapay's own dashboard for example.
	PlatformAdjustmentReasonOffSystemWithdrawal PlatformAdjustmentReason = "OFF_SYSTEM_WITHDRAWAL"
	// PlatformAdjustmentReasonSingapayCorrection: Singapay corrected the balance.
	PlatformAdjustmentReasonSingapayCorrection PlatformAdjustmentReason = "SINGAPAY_CORRECTION"
	// PlatformAdjustmentReasonOther: anything else; the note is required.
	PlatformAdjustmentReasonOther PlatformAdjustmentReason = "OTHER"
)

// Valid reports whether the reason is one of the four.
func (r PlatformAdjustmentReason) Valid() bool {
	switch r {
	case PlatformAdjustmentReasonTestFunds, PlatformAdjustmentReasonOffSystemWithdrawal,
		PlatformAdjustmentReasonSingapayCorrection, PlatformAdjustmentReasonOther:
		return true
	}
	return false
}

// MaxPlatformAdjustmentNoteLength bounds an admin's note, in characters.
const MaxPlatformAdjustmentNoteLength = 500

// PlatformAdjustment is one row of platform_adjustments: one movement of the platform's
// AVAILABLE balance that no sale and no payout explains. Insert-only.
type PlatformAdjustment struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`

	PlatformAccountUUID string
	Kind                PlatformAdjustmentKind

	// Amount is the whole rupiah the platform's AVAILABLE moved by, signed. It is the amount of
	// the adjustment's ledger entry on the platform account, and 0 only for a difference under
	// Rp1, which writes no entry at all.
	Amount int64

	// ResidualMinor is the sen a DIFFERENCE puts into the residual pool, 0..99: the part of the
	// booked figure a whole-rupiah entry cannot hold. 0 for the other kinds.
	ResidualMinor int64

	// PoolBeforeMinor is, for a ROUNDING, the pool it booked from; nil otherwise.
	PoolBeforeMinor *int64

	// Reason and Note: a DIFFERENCE's, given by the admin. Empty for the other kinds, except
	// that the archive may carry a note.
	Reason PlatformAdjustmentReason
	Note   string

	// AdminID and AdminName are who booked it: the admin for a DIFFERENCE, the operator for the
	// archive, empty for a ROUNDING (the worker).
	AdminID   string
	AdminName string

	// JournalUUID is the journal of the adjustment's entries; empty when none was written.
	JournalUUID string

	// ReconciliationUUID is the check a DIFFERENCE booked. One booking per check.
	ReconciliationUUID string

	// ReserveAccountUUID is the RESERVE account a RESERVE_ARCHIVE credited.
	ReserveAccountUUID string

	// SourceTransactionUUIDs are the product transactions the amount comes from: for a
	// ROUNDING the swept transactions whose sen entered the pool since the previous rounding,
	// for the archive the DOKU-era transactions. Empty for a DIFFERENCE.
	SourceTransactionUUIDs []string
}

// NewPlatformAdjustment builds an adjustment of kind on the platform account.
func NewPlatformAdjustment(platformAccountUUID string, kind PlatformAdjustmentKind, amount int64) *PlatformAdjustment {
	a := &PlatformAdjustment{
		PlatformAccountUUID:    platformAccountUUID,
		Kind:                   kind,
		Amount:                 amount,
		SourceTransactionUUIDs: []string{},
	}
	redifu.InitRecord(a)
	// InitRecord allocates every nil pointer, and a pointer to 0 would read as a pool of zero.
	// It is NULL until a rounding sets it.
	a.PoolBeforeMinor = nil
	return a
}

// PlatformReconciliationStatus is the outcome of one check.
type PlatformReconciliationStatus string

const (
	// PlatformReconciliationMatched: the unexplained figure is exactly 0.
	PlatformReconciliationMatched PlatformReconciliationStatus = "MATCHED"
	// PlatformReconciliationMismatch: it is not. An alert, never a booking.
	PlatformReconciliationMismatch PlatformReconciliationStatus = "MISMATCH"
	// PlatformReconciliationSkipped: the figure could not be computed honestly this time; see
	// the skip reason. The next check computes it again.
	PlatformReconciliationSkipped PlatformReconciliationStatus = "SKIPPED"
)

// PlatformReconciliationSkipReason says why a check was skipped.
type PlatformReconciliationSkipReason string

const (
	// SkipPayoutInFlight: a platform payout is PENDING or PROCESSING. The ledger has reserved
	// it and Singapay may not have debited it, so the two balances disagree by it for a while.
	SkipPayoutInFlight PlatformReconciliationSkipReason = "PAYOUT_IN_FLIGHT"
	// SkipBalanceReadFailed: Singapay's balance inquiry failed.
	SkipBalanceReadFailed PlatformReconciliationSkipReason = "BALANCE_READ_FAILED"
	// SkipLedgerMoved: the platform's ledger changed while the balance was read, on every try.
	// The balance may be from before the change or after it.
	SkipLedgerMoved PlatformReconciliationSkipReason = "LEDGER_MOVED"
)

// PlatformReconciliation is one row of platform_reconciliations: one check of the equation.
// Every amount is in sen. Insert-only, except EmailedAt.
type PlatformReconciliation struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`

	PlatformAccountUUID string
	SingapayAccountID   string

	// S, as Singapay answered; nil when the balance was not read.
	SingapayAvailableMinor *int64
	SingapayPendingMinor   *int64
	SingapayHeldMinor      *int64
	RawBalance             []byte

	// L, K and B, from one snapshot of the ledger.
	LedgerAvailableMinor    int64
	ResidualPoolMinor       int64
	UnsweptPlatformFeeMinor int64
	UnsweptCount            int
	PayoutsInFlight         int

	// UnexplainedMinor is S − L − K + B; nil when S was not read.
	UnexplainedMinor *int64

	Status     PlatformReconciliationStatus
	SkipReason PlatformReconciliationSkipReason
	SkipDetail string

	// CheckedAt is when Singapay's balance was read (when the check ran, for one skipped
	// before reading it).
	CheckedAt time.Time

	// EmailedAt is when the admins were sent the alert for this check; nil when they were not.
	EmailedAt *time.Time
}

// NewPlatformReconciliation builds a check from a snapshot of the ledger. The caller records
// the balance (RecordBalance) and settles it (Decide or Skip).
func NewPlatformReconciliation(platformAccountUUID, singapayAccountID string, snapshot *PlatformBalanceSnapshot, checkedAt time.Time) *PlatformReconciliation {
	r := &PlatformReconciliation{
		PlatformAccountUUID: platformAccountUUID,
		SingapayAccountID:   singapayAccountID,
		CheckedAt:           checkedAt.UTC(),
	}
	redifu.InitRecord(r)
	// InitRecord allocates every nil pointer, and a pointer to 0 would read as a balance of
	// zero (or an email sent at year 1). These are NULL until set.
	r.SingapayAvailableMinor = nil
	r.SingapayPendingMinor = nil
	r.SingapayHeldMinor = nil
	r.UnexplainedMinor = nil
	r.EmailedAt = nil
	if snapshot != nil {
		r.LedgerAvailableMinor = RupiahToMinor(snapshot.LedgerAvailable)
		r.ResidualPoolMinor = snapshot.PoolMinor()
		r.UnsweptPlatformFeeMinor = RupiahToMinor(snapshot.UnsweptPlatformFee)
		r.UnsweptCount = snapshot.UnsweptCount
		r.PayoutsInFlight = snapshot.PayoutsInFlight
	}
	return r
}

// Decide computes the unexplained figure from S and sets the status: MATCHED at exactly 0,
// MISMATCH otherwise.
func (r *PlatformReconciliation) Decide(singapayAvailableMinor int64) {
	s := singapayAvailableMinor
	r.SingapayAvailableMinor = &s
	unexplained := s - r.LedgerAvailableMinor - r.ResidualPoolMinor + r.UnsweptPlatformFeeMinor
	r.UnexplainedMinor = &unexplained
	r.SkipReason = ""
	r.SkipDetail = ""
	if unexplained == 0 {
		r.Status = PlatformReconciliationMatched
	} else {
		r.Status = PlatformReconciliationMismatch
	}
}

// Skip marks the check skipped. An unexplained figure computed before is dropped: it is not
// one to act on.
func (r *PlatformReconciliation) Skip(reason PlatformReconciliationSkipReason, detail string) {
	r.Status = PlatformReconciliationSkipped
	r.SkipReason = reason
	r.SkipDetail = detail
	r.UnexplainedMinor = nil
}

// Unexplained is the unexplained figure, 0 when there is none.
func (r *PlatformReconciliation) Unexplained() int64 {
	if r == nil || r.UnexplainedMinor == nil {
		return 0
	}
	return *r.UnexplainedMinor
}

// PlatformBalanceSnapshot is the ledger's side of the equation for the platform account, read
// in ONE statement so every figure is from the same moment.
type PlatformBalanceSnapshot struct {
	PlatformAccountUUID string

	// LedgerAvailable is L in whole rupiah: the sum of the platform's AVAILABLE entries, as a
	// payout derives the balance (not the cached column).
	LedgerAvailable int64

	// SweptResidualMinor is Σ platform_residual over the swept rows of sellers' sales, in sen.
	SweptResidualMinor int64

	// AdjustmentResidualMinor is Σ residual_minor over every adjustment, in sen.
	AdjustmentResidualMinor int64

	// RoundedMinor is Σ amount × 100 over the ROUNDING adjustments, in sen.
	RoundedMinor int64

	// UnsweptPlatformFee is B in whole rupiah: over the sellers' SETTLED rows whose fee is not
	// transferred yet, the platform's AVAILABLE entries for them — SETTLEMENT_NET and any
	// FEE_ADJUSTMENT. The whole rupiah the ledger holds, never the sen-exact figure the sweep
	// will move: the sen of an unswept row are not in K either, so using them here would turn
	// every unswept fee with sen into a false alert.
	UnsweptPlatformFee int64
	UnsweptCount       int

	// PayoutsInFlight counts the platform's payouts in PENDING or PROCESSING.
	PayoutsInFlight int
}

// PoolMinor is K, the residual pool, in sen: never negative in a ledger written by this
// package (residuals and adjustment sen are 0..99, and a rounding books at most the pool).
func (s *PlatformBalanceSnapshot) PoolMinor() int64 {
	return s.SweptResidualMinor + s.AdjustmentResidualMinor - s.RoundedMinor
}

// SameAs reports whether two snapshots read the same ledger.
func (s *PlatformBalanceSnapshot) SameAs(o *PlatformBalanceSnapshot) bool {
	return s != nil && o != nil && *s == *o
}

// UnexplainedMinor is S − L − K + B over this snapshot and Singapay's available balance, in sen.
func (s *PlatformBalanceSnapshot) UnexplainedMinor(singapayAvailableMinor int64) int64 {
	return singapayAvailableMinor - RupiahToMinor(s.LedgerAvailable) - s.PoolMinor() + RupiahToMinor(s.UnsweptPlatformFee)
}

// FloorSplitMinor splits a signed figure in sen into whole rupiah rounded toward minus
// infinity and the sen left over, 0..99: 400016 → (4000, 16), −4130 → (−42, 70).
//
// Rounded down, not toward zero, so the ledger never claims more than Singapay holds: booking
// −41,30 as −41 would leave the ledger 30 sen above the sub-account, and a payout of the whole
// ledger balance would then be refused. The sen go to the residual pool, where the next
// rounding books them once they make a rupiah.
func FloorSplitMinor(minor int64) (rupiah int64, sen int64) {
	rupiah = minor / MinorPerRupiah
	sen = minor % MinorPerRupiah
	if sen < 0 {
		rupiah--
		sen += MinorPerRupiah
	}
	return rupiah, sen
}

// CleanPlatformAdjustmentNote trims a note and reports whether it fits.
func CleanPlatformAdjustmentNote(note string) (string, bool) {
	note = strings.TrimSpace(note)
	return note, len([]rune(note)) <= MaxPlatformAdjustmentNoteLength
}

// ReserveArchiveSource is one DOKU-era transaction the archive moves: a transaction settled
// before the ledger recorded settled fees (settled_platform_fee NULL), with what the
// platform's AVAILABLE entries hold for it.
type ReserveArchiveSource struct {
	ProductTransactionUUID string
	InvoiceNumber          string
	ProductType            string
	// PlatformSale is true for the platform's own sale (a subscription), false for a fee on a
	// seller's sale.
	PlatformSale bool
	SettledAt    *time.Time
	// Amount is Σ of the platform's AVAILABLE entries for the transaction, whole rupiah.
	Amount int64
}

// PlatformReconciliationRepository is the reconciliation's reads and its two insert-only
// tables (migration 034). Every read of the equation is one statement.
type PlatformReconciliationRepository interface {
	// ReadBalanceSnapshot reads L, K's three parts, B and the payouts in flight in one
	// statement.
	ReadBalanceSnapshot(ctx context.Context, platformAccountUUID string) (*PlatformBalanceSnapshot, error)

	// ListUnroundedResidualSources returns the swept rows of sellers' sales with residual sen
	// that no ROUNDING names yet, oldest sweep first.
	ListUnroundedResidualSources(ctx context.Context, platformAccountUUID string) ([]string, error)

	// ListReserveArchiveSources returns the DOKU-era transactions and what the platform's
	// AVAILABLE entries hold for each, oldest settlement first.
	ListReserveArchiveSources(ctx context.Context, platformAccountUUID string) ([]ReserveArchiveSource, error)

	SaveAdjustment(ctx context.Context, adjustment *PlatformAdjustment) error
	// GetAdjustmentByReconciliation returns the DIFFERENCE booked against a check;
	// repo.ErrNotFound when there is none.
	GetAdjustmentByReconciliation(ctx context.Context, reconciliationUUID string) (*PlatformAdjustment, error)
	// GetReserveArchive returns the RESERVE_ARCHIVE adjustment; repo.ErrNotFound before it ran.
	GetReserveArchive(ctx context.Context) (*PlatformAdjustment, error)
	// ListAdjustments pages the adjustments for the platform statement, in (created_at, uuid)
	// keyset order, one past after.
	ListAdjustments(ctx context.Context, platformAccountUUID string, after *KeysetCursor, limit int, ascending bool) ([]*PlatformAdjustment, error)

	SaveReconciliation(ctx context.Context, reconciliation *PlatformReconciliation) error
	// LatestReconciliation returns the most recent check; repo.ErrNotFound before the first.
	LatestReconciliation(ctx context.Context, platformAccountUUID string) (*PlatformReconciliation, error)
	// LastEmailedAt returns when the admins were last emailed about a check; nil when never.
	LastEmailedAt(ctx context.Context, platformAccountUUID string) (*time.Time, error)
	// MarkEmailed records that the admins were emailed about a check.
	MarkEmailed(ctx context.Context, reconciliationUUID string, at time.Time) error
}

// NewPlatformReconciliationEntry is the ledger entry of a platform adjustment, on one account's
// AVAILABLE bucket:
//
//	account  ±amount  AVAILABLE  RECONCILIATION  (source MANUAL_ADJUSTMENT = the adjustment)
//
// Sourced on the adjustment, never on a product transaction, so the platform statement's line
// for a sale and the Platform Earnings chart — both the sum of the platform's
// PRODUCT_TRANSACTION entries — stay what they were. A RESERVE_ARCHIVE writes two: the
// platform's debit and the archive account's credit.
func NewPlatformReconciliationEntry(journalUUID, adjustmentUUID, accountUUID string, amount int64) *LedgerEntry {
	entry := &LedgerEntry{
		JournalUUID:   journalUUID,
		AccountUUID:   accountUUID,
		Amount:        amount,
		BalanceBucket: BalanceBucketAvailable,
		EntryType:     EntryTypeReconciliation,
		SourceType:    SourceTypeManualAdjustment,
		SourceID:      adjustmentUUID,
	}
	redifu.InitRecord(entry)
	return entry
}
