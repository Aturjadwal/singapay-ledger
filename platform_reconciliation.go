package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// ─────────────────────────────────────────────────────────────────────────────
// Platform balance reconciliation (v0.13.0)
// ─────────────────────────────────────────────────────────────────────────────
//
// The platform's ledger account against its Singapay sub-account, in sen:
//
//	unexplained = S − L − K + B
//
// (domain/platform_reconciliation.go names the terms; docs/106 is the reasoning). The consuming
// service runs, on every settlement tick and after a manual sweep:
//
//	settlement pass → card fees → sweep → BookPlatformResidual → CheckPlatformBalance → alert
//
// Only the residual pool K is ever booked by itself, a whole rupiah at a time. A check that is
// not exactly 0 is an alert; an admin books it with BookPlatformDifference and a reason. The
// DOKU era, which the ledger holds and the sub-account never did, moves to a RESERVE archive
// account once (ArchiveToReserve). Every booking writes a platform_adjustments row, a journal
// and its entries in one transaction, under the platform account's row lock.

// PlatformDifferenceMaxCheckAge is how old a check may be when its difference is booked. Older
// than this, the admin checks again: the figure booked must be one somebody just looked at.
const PlatformDifferenceMaxCheckAge = 10 * time.Minute

// platformBalanceReadAttempts bounds how often one check reads the balance when the ledger
// moves while it does. Each attempt is one Singapay call.
const platformBalanceReadAttempts = 3

// platformReconciliationClock is the reconciliation's clock: the time a check records and the
// time a booking measures its age against. A variable so a test can move it.
var platformReconciliationClock = time.Now

// platformAccount returns the platform account, ErrLedgerNotFound when it is not provisioned.
func (c *LedgerClient) platformAccount(ctx context.Context, provider interface {
	Account() domain.AccountRepository
}) (*domain.Account, error) {
	account, err := provider.Account().GetPlatformAccount(ctx)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get platform account", err)
	}
	return account, nil
}

// ── The residual pool ────────────────────────────────────────────────────────

// PlatformResidualBooking is what BookPlatformResidual did.
type PlatformResidualBooking struct {
	// Booked is true when a ROUNDING was written.
	Booked bool `json:"booked"`
	// Adjustment is the ROUNDING written; nil when nothing was booked.
	Adjustment *domain.PlatformAdjustment `json:"adjustment,omitempty"`
	// PoolBeforeMinor and PoolAfterMinor are the residual pool before and after, in sen.
	PoolBeforeMinor int64 `json:"pool_before_minor"`
	PoolAfterMinor  int64 `json:"pool_after_minor"`
	// Sources is how many swept transactions' sen the rounding names.
	Sources int `json:"sources"`
}

// BookPlatformResidual books the whole rupiah of the residual pool to the platform's AVAILABLE
// balance: one ROUNDING adjustment, one RECONCILIATION journal, one entry. Under Rp1 nothing is
// written and the sen wait for the next sweep.
//
// The pool is recomputed inside the transaction, after the platform account's row lock: two
// processes running this at once serialise on the lock, and the second reads a pool the
// first's rounding already emptied, so it books nothing.
//
// The rounding names the swept transactions whose sen entered the pool since the previous one
// (Pembulatan sen dari N transaksi); sen a DIFFERENCE put in the pool are part of the amount
// without being a transaction.
func (c *LedgerClient) BookPlatformResidual(ctx context.Context) (*PlatformResidualBooking, error) {
	platform, err := c.platformAccount(ctx, c.repoProvider)
	if err != nil {
		return nil, err
	}

	var booking *PlatformResidualBooking
	err = c.txProvider.TransactReadCommitted(ctx, func(dbtx repo.Tx) error {
		if _, err := dbtx.Account().GetByIDForUpdate(ctx, platform.UUID); err != nil {
			return fmt.Errorf("lock the platform account: %w", err)
		}

		snapshot, err := dbtx.PlatformReconciliation().ReadBalanceSnapshot(ctx, platform.UUID)
		if err != nil {
			return fmt.Errorf("read the residual pool: %w", err)
		}
		pool := snapshot.PoolMinor()
		booking = &PlatformResidualBooking{PoolBeforeMinor: pool, PoolAfterMinor: pool}
		if pool < domain.MinorPerRupiah {
			return nil
		}

		rupiah, carried := domain.FloorSplitMinor(pool)
		sources, err := dbtx.PlatformReconciliation().ListUnroundedResidualSources(ctx, platform.UUID)
		if err != nil {
			return fmt.Errorf("list the rounding's transactions: %w", err)
		}

		adjustment := domain.NewPlatformAdjustment(platform.UUID, domain.PlatformAdjustmentRounding, rupiah)
		poolBefore := pool
		adjustment.PoolBeforeMinor = &poolBefore
		adjustment.SourceTransactionUUIDs = sources

		journal := domain.NewJournal(domain.EventTypeReconciliation, domain.SourceTypeManualAdjustment, adjustment.UUID, map[string]any{
			"reconciliation":            "platform-residual-rounding",
			"adjustment_uuid":           adjustment.UUID,
			"reason":                    "Whole rupiah of the residual pool: sen the platform-fee sweep moved at Singapay that the whole-rupiah ledger entries could not record.",
			"pool_before_minor":         pool,
			"swept_residual_minor":      snapshot.SweptResidualMinor,
			"adjustment_residual_minor": snapshot.AdjustmentResidualMinor,
			"rounded_before_minor":      snapshot.RoundedMinor,
			"rupiah":                    rupiah,
			"carried_minor":             carried,
			"source_transactions":       len(sources),
		})
		adjustment.JournalUUID = journal.UUID

		if err := dbtx.Journal().Save(ctx, journal); err != nil {
			return err
		}
		// SaveBatch keeps balance_after and ledger_accounts.available_balance right.
		if err := dbtx.LedgerEntry().SaveBatch(ctx, []*domain.LedgerEntry{
			domain.NewPlatformReconciliationEntry(journal.UUID, adjustment.UUID, platform.UUID, rupiah),
		}); err != nil {
			return err
		}
		if err := dbtx.PlatformReconciliation().SaveAdjustment(ctx, adjustment); err != nil {
			return err
		}

		booking.Booked = true
		booking.Adjustment = adjustment
		booking.PoolAfterMinor = carried
		booking.Sources = len(sources)
		return nil
	})
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to book the platform's residual sen", err)
	}

	if booking.Booked {
		c.logger.InfoContext(ctx, "Platform residual sen booked",
			"adjustment_uuid", booking.Adjustment.UUID,
			"rupiah", booking.Adjustment.Amount,
			"pool_before", formatMinor(booking.PoolBeforeMinor),
			"carried", formatMinor(booking.PoolAfterMinor),
			"source_transactions", booking.Sources,
		)
	}
	return booking, nil
}

// ── The check ────────────────────────────────────────────────────────────────

// CheckPlatformBalance reads the platform sub-account's balance at Singapay (a read, nothing
// moves), computes the equation against one snapshot of the ledger and records the check.
//
//  1. One statement reads L, K, B and the platform payouts in flight.
//  2. A payout in PENDING or PROCESSING skips the check (PAYOUT_IN_FLIGHT): the ledger has
//     reserved it and Singapay may not have debited it.
//  3. Singapay's balance inquiry. A failure skips the check (BALANCE_READ_FAILED).
//  4. The statement again. A payout that started skips the check; any other change means the
//     balance may be from before it or after it, so the read is made again, up to three times,
//     and then the check is skipped (LEDGER_MOVED).
//  5. Otherwise the figure is decided: MATCHED at exactly 0, MISMATCH anything else.
//
// The check is recorded whatever the outcome, and returned. The error is non-nil only when
// nothing could be recorded: no platform account, no sub-account on it, or the database.
//
// Run it after the sweep and BookPlatformResidual, never beside them in another process if it
// can be helped: a sweep that has moved a fee at Singapay and not yet marked its row is one
// the check sees as money in the sub-account the ledger cannot explain (until the row is
// marked, a moment later).
func (c *LedgerClient) CheckPlatformBalance(ctx context.Context) (*domain.PlatformReconciliation, error) {
	platform, err := c.platformAccount(ctx, c.repoProvider)
	if err != nil {
		return nil, err
	}
	if platform.SingapayAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInternal,
			"platform account has no Singapay sub-account; its balance cannot be read", nil)
	}

	reconciliations := c.repoProvider.PlatformReconciliation()
	before, err := reconciliations.ReadBalanceSnapshot(ctx, platform.UUID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the platform's ledger", err)
	}

	if before.PayoutsInFlight > 0 {
		rec := domain.NewPlatformReconciliation(platform.UUID, platform.SingapayAccountID, before, platformReconciliationClock())
		rec.Skip(domain.SkipPayoutInFlight, fmt.Sprintf("%d platform payout(s) in PENDING or PROCESSING", before.PayoutsInFlight))
		return c.recordPlatformReconciliation(ctx, rec)
	}

	for attempt := 1; ; attempt++ {
		balance, err := c.gateway.GetAccountBalance(ctx, platform.SingapayAccountID)
		checkedAt := platformReconciliationClock()
		if err != nil {
			rec := domain.NewPlatformReconciliation(platform.UUID, platform.SingapayAccountID, before, checkedAt)
			rec.Skip(domain.SkipBalanceReadFailed, err.Error())
			return c.recordPlatformReconciliation(ctx, rec)
		}

		after, err := reconciliations.ReadBalanceSnapshot(ctx, platform.UUID)
		if err != nil {
			return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the platform's ledger again after the balance inquiry", err)
		}

		rec := domain.NewPlatformReconciliation(platform.UUID, platform.SingapayAccountID, after, checkedAt)
		recordPlatformBalance(rec, balance)

		switch {
		case after.PayoutsInFlight > 0:
			rec.Skip(domain.SkipPayoutInFlight, fmt.Sprintf("%d platform payout(s) started while the balance was read", after.PayoutsInFlight))
			return c.recordPlatformReconciliation(ctx, rec)
		case before.SameAs(after):
			rec.Decide(balance.Available.Minor())
			return c.recordPlatformReconciliation(ctx, rec)
		case attempt >= platformBalanceReadAttempts:
			rec.Skip(domain.SkipLedgerMoved, fmt.Sprintf("the platform's ledger changed during each of %d balance reads", attempt))
			return c.recordPlatformReconciliation(ctx, rec)
		}
		before = after
	}
}

// recordPlatformBalance copies the balance inquiry's answer onto the check.
func recordPlatformBalance(rec *domain.PlatformReconciliation, balance *singapay.Balance) {
	available := balance.Available.Minor()
	pending := balance.Pending.Minor()
	held := balance.Held.Minor()
	rec.SingapayAvailableMinor = &available
	rec.SingapayPendingMinor = &pending
	rec.SingapayHeldMinor = &held
	if raw, err := json.Marshal(balance); err == nil {
		rec.RawBalance = raw
	}
}

func (c *LedgerClient) recordPlatformReconciliation(ctx context.Context, rec *domain.PlatformReconciliation) (*domain.PlatformReconciliation, error) {
	if err := c.repoProvider.PlatformReconciliation().SaveReconciliation(ctx, rec); err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to record the platform balance check", err)
	}
	return rec, nil
}

// ── Booking a difference ─────────────────────────────────────────────────────

// PlatformDifferenceRequest books the difference of one check.
type PlatformDifferenceRequest struct {
	// ReconciliationUUID is the check whose figure is booked: the latest one, at most
	// PlatformDifferenceMaxCheckAge old. The amount is that check's, never the caller's.
	ReconciliationUUID string

	// Reason is required; Note is required with OTHER, at most 500 characters.
	Reason domain.PlatformAdjustmentReason
	Note   string

	// AdminID and AdminName are who books it. AdminID is required.
	AdminID   string
	AdminName string
}

// PlatformDifferenceBooking is a booked difference.
type PlatformDifferenceBooking struct {
	Adjustment     *domain.PlatformAdjustment     `json:"adjustment"`
	Reconciliation *domain.PlatformReconciliation `json:"reconciliation"`
}

// BookPlatformDifference books the unexplained figure of the latest check: its whole rupiah,
// rounded down, to the platform's AVAILABLE balance (one MANUAL_ADJUSTMENT journal, one
// entry), and its sen, 0..99, to the residual pool — 4.000,16 is Rp4.000 and 16 sen, −41,30 is
// −Rp42 and 70 sen, so the ledger never claims more than Singapay holds. Either direction.
//
// Refused, with nothing written: a reason or note that does not hold
// (ErrInvalidPlatformAdjustment); no check yet (ErrPlatformReconciliationNotFound); a newer
// check has run (ErrPlatformReconciliationNotLatest); the check found nothing or was skipped
// (ErrPlatformReconciliationNothingToBook); it is older than PlatformDifferenceMaxCheckAge
// (ErrPlatformReconciliationStale); it is already booked
// (ErrPlatformReconciliationAlreadyBooked); a platform payout is in flight
// (ErrPlatformReconciliationPayoutInFlight).
//
// Everything is decided after the platform account's row lock, and one check can be booked
// once (a unique index on platform_adjustments.reconciliation_uuid), so two admins pressing
// the button at once book it once. Check again afterwards: the next check is 0.
func (c *LedgerClient) BookPlatformDifference(ctx context.Context, req PlatformDifferenceRequest) (*PlatformDifferenceBooking, error) {
	if !req.Reason.Valid() {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidPlatformAdjustment, fmt.Sprintf("unknown reason %q", req.Reason), nil)
	}
	note, ok := domain.CleanPlatformAdjustmentNote(req.Note)
	if !ok {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidPlatformAdjustment,
			fmt.Sprintf("the note is longer than %d characters", domain.MaxPlatformAdjustmentNoteLength), nil)
	}
	if req.Reason == domain.PlatformAdjustmentReasonOther && note == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidPlatformAdjustment, "the reason OTHER needs a note", nil)
	}
	if req.AdminID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidPlatformAdjustment, "the admin who books the difference is required", nil)
	}
	if req.ReconciliationUUID == "" {
		return nil, ledgererr.ErrPlatformReconciliationNotFound
	}

	platform, err := c.platformAccount(ctx, c.repoProvider)
	if err != nil {
		return nil, err
	}

	var booking *PlatformDifferenceBooking
	err = c.txProvider.TransactReadCommitted(ctx, func(dbtx repo.Tx) error {
		if _, err := dbtx.Account().GetByIDForUpdate(ctx, platform.UUID); err != nil {
			return fmt.Errorf("lock the platform account: %w", err)
		}
		reconciliations := dbtx.PlatformReconciliation()

		latest, err := reconciliations.LatestReconciliation(ctx, platform.UUID)
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return ledgererr.ErrPlatformReconciliationNotFound
		}
		if err != nil {
			return err
		}
		if latest.UUID != req.ReconciliationUUID {
			return ledgererr.ErrPlatformReconciliationNotLatest
		}
		if latest.Status != domain.PlatformReconciliationMismatch || latest.Unexplained() == 0 {
			return ledgererr.ErrPlatformReconciliationNothingToBook
		}
		if platformReconciliationClock().Sub(latest.CheckedAt) > PlatformDifferenceMaxCheckAge {
			return ledgererr.ErrPlatformReconciliationStale
		}
		if _, err := reconciliations.GetAdjustmentByReconciliation(ctx, latest.UUID); err == nil {
			return ledgererr.ErrPlatformReconciliationAlreadyBooked
		} else if !ledgererr.IsAppError(err, repo.ErrNotFound) {
			return err
		}
		snapshot, err := reconciliations.ReadBalanceSnapshot(ctx, platform.UUID)
		if err != nil {
			return err
		}
		if snapshot.PayoutsInFlight > 0 {
			return ledgererr.ErrPlatformReconciliationPayoutInFlight
		}

		unexplained := latest.Unexplained()
		rupiah, sen := domain.FloorSplitMinor(unexplained)

		adjustment := domain.NewPlatformAdjustment(platform.UUID, domain.PlatformAdjustmentDifference, rupiah)
		adjustment.ResidualMinor = sen
		adjustment.Reason = req.Reason
		adjustment.Note = note
		adjustment.AdminID = req.AdminID
		adjustment.AdminName = req.AdminName
		adjustment.ReconciliationUUID = latest.UUID

		if rupiah != 0 {
			journal := domain.NewJournal(domain.EventTypeManualAdjustment, domain.SourceTypeManualAdjustment, adjustment.UUID, map[string]any{
				"reconciliation":      "platform-balance-difference",
				"adjustment_uuid":     adjustment.UUID,
				"reconciliation_uuid": latest.UUID,
				"reason":              string(req.Reason),
				"note":                note,
				"admin_id":            req.AdminID,
				"admin_name":          req.AdminName,
				"unexplained_minor":   unexplained,
				"rupiah":              rupiah,
				"residual_minor":      sen,
				"singapay_available":  derefInt64(latest.SingapayAvailableMinor),
				"ledger_available":    latest.LedgerAvailableMinor,
				"residual_pool":       latest.ResidualPoolMinor,
				"unswept":             latest.UnsweptPlatformFeeMinor,
				"checked_at":          latest.CheckedAt.UTC().Format(time.RFC3339),
			})
			adjustment.JournalUUID = journal.UUID
			if err := dbtx.Journal().Save(ctx, journal); err != nil {
				return err
			}
			if err := dbtx.LedgerEntry().SaveBatch(ctx, []*domain.LedgerEntry{
				domain.NewPlatformReconciliationEntry(journal.UUID, adjustment.UUID, platform.UUID, rupiah),
			}); err != nil {
				return err
			}
		}

		if err := reconciliations.SaveAdjustment(ctx, adjustment); err != nil {
			if repo.IsUniqueViolation(err, repo.ConstraintUniqueReconciliationBooking) {
				return ledgererr.ErrPlatformReconciliationAlreadyBooked
			}
			return err
		}

		booking = &PlatformDifferenceBooking{Adjustment: adjustment, Reconciliation: latest}
		return nil
	})
	if err != nil {
		if isPlatformReconciliationRefusal(err) {
			return nil, err
		}
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to book the platform balance difference", err)
	}

	c.logger.InfoContext(ctx, "Platform balance difference booked",
		"adjustment_uuid", booking.Adjustment.UUID,
		"reconciliation_uuid", booking.Reconciliation.UUID,
		"unexplained", formatMinor(booking.Reconciliation.Unexplained()),
		"rupiah", booking.Adjustment.Amount,
		"residual_minor", booking.Adjustment.ResidualMinor,
		"reason", string(booking.Adjustment.Reason),
		"admin_id", booking.Adjustment.AdminID,
	)
	return booking, nil
}

// isPlatformReconciliationRefusal is a refusal the caller acts on, returned as it is.
func isPlatformReconciliationRefusal(err error) bool {
	for _, refusal := range []ledgererr.AppError{
		ledgererr.ErrPlatformReconciliationNotFound,
		ledgererr.ErrPlatformReconciliationNotLatest,
		ledgererr.ErrPlatformReconciliationStale,
		ledgererr.ErrPlatformReconciliationNothingToBook,
		ledgererr.ErrPlatformReconciliationAlreadyBooked,
		ledgererr.ErrPlatformReconciliationPayoutInFlight,
		ledgererr.ErrReserveArchiveAlreadyDone,
		ledgererr.ErrReserveArchiveAmountMismatch,
	} {
		if ledgererr.IsAppError(err, refusal) {
			return true
		}
	}
	return false
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// ── State for a dashboard ────────────────────────────────────────────────────

// PlatformReconciliationState is the latest check and what can be done with it.
type PlatformReconciliationState struct {
	// Latest is the most recent check; nil before the first.
	Latest *domain.PlatformReconciliation `json:"latest,omitempty"`
	// Booking is the DIFFERENCE booked against Latest; nil when none was.
	Booking *domain.PlatformAdjustment `json:"booking,omitempty"`
	// LastEmailedAt is when the admins were last emailed about any check; nil when never.
	LastEmailedAt *time.Time `json:"last_emailed_at,omitempty"`
	// Bookable reports whether Latest's difference can be booked now, and BookableUntil when
	// that stops: Latest is a MISMATCH, not booked, at most PlatformDifferenceMaxCheckAge old.
	// (A payout in flight still refuses the booking.)
	Bookable      bool       `json:"bookable"`
	BookableUntil *time.Time `json:"bookable_until,omitempty"`
}

// GetPlatformReconciliation reads the latest check, its booking and the last alert. A pure
// read of this package's tables — no gateway call.
func (c *LedgerClient) GetPlatformReconciliation(ctx context.Context) (*PlatformReconciliationState, error) {
	platform, err := c.platformAccount(ctx, c.repoProvider)
	if err != nil {
		return nil, err
	}
	reconciliations := c.repoProvider.PlatformReconciliation()

	state := &PlatformReconciliationState{}
	latest, err := reconciliations.LatestReconciliation(ctx, platform.UUID)
	switch {
	case ledgererr.IsAppError(err, repo.ErrNotFound):
		latest = nil
	case err != nil:
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the latest platform balance check", err)
	}
	state.Latest = latest

	if latest != nil {
		booking, err := reconciliations.GetAdjustmentByReconciliation(ctx, latest.UUID)
		switch {
		case err == nil:
			state.Booking = booking
		case !ledgererr.IsAppError(err, repo.ErrNotFound):
			return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the check's booking", err)
		}
		if latest.Status == domain.PlatformReconciliationMismatch && state.Booking == nil {
			until := latest.CheckedAt.Add(PlatformDifferenceMaxCheckAge)
			state.BookableUntil = &until
			state.Bookable = !platformReconciliationClock().After(until)
		}
	}

	lastEmailed, err := reconciliations.LastEmailedAt(ctx, platform.UUID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read when the admins were last emailed", err)
	}
	state.LastEmailedAt = lastEmailed
	return state, nil
}

// MarkPlatformReconciliationEmailed records that the admins were emailed about a check. The
// consuming service sends at most one alert a day (its outbox deduplicates by day); this is
// the record the next tick reads with GetPlatformReconciliation.
func (c *LedgerClient) MarkPlatformReconciliationEmailed(ctx context.Context, reconciliationUUID string, at time.Time) error {
	err := c.repoProvider.PlatformReconciliation().MarkEmailed(ctx, reconciliationUUID, at)
	if ledgererr.IsAppError(err, repo.ErrNotFound) {
		// Not wrapped: ErrCode reads the innermost AppError, and the caller asks for this one.
		return ledgererr.ErrPlatformReconciliationNotFound
	}
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to record the platform balance alert", err)
	}
	return nil
}

// ── The DOKU era ─────────────────────────────────────────────────────────────

// ReserveArchiveRequest moves the DOKU era out of the platform account.
type ReserveArchiveRequest struct {
	// ExpectedAmount is what the DOKU-era entries must add up to, whole rupiah (45.727 in
	// production). A different sum is refused: the archive moves exactly what was agreed.
	ExpectedAmount int64

	// Apply writes the archive; false is a dry run that reads only.
	Apply bool

	// AdminID, AdminName and Note are recorded on the adjustment. Optional.
	AdminID   string
	AdminName string
	Note      string
}

// ReserveArchiveResult is the archive as computed, and what applying it did or would do.
type ReserveArchiveResult struct {
	// Applied is true when this call wrote the archive.
	Applied bool `json:"applied"`
	// AlreadyArchived is true when an earlier call did; Adjustment is that archive then.
	AlreadyArchived bool `json:"already_archived"`

	// Amount is the sum of the DOKU-era entries, whole rupiah; AmountMatches whether it is
	// ExpectedAmount.
	Amount         int64                         `json:"amount"`
	ExpectedAmount int64                         `json:"expected_amount"`
	AmountMatches  bool                          `json:"amount_matches"`
	Sources        []domain.ReserveArchiveSource `json:"sources"`

	// The two accounts' AVAILABLE balances (Σ entries) before and after — after as it is (or
	// would be) once applied.
	PlatformAvailableBefore int64 `json:"platform_available_before"`
	PlatformAvailableAfter  int64 `json:"platform_available_after"`
	ReserveAvailableBefore  int64 `json:"reserve_available_before"`
	ReserveAvailableAfter   int64 `json:"reserve_available_after"`

	// ReserveAccount is the archive account; nil on a dry run before it exists.
	ReserveAccount *domain.Account `json:"reserve_account,omitempty"`
	// Adjustment is the RESERVE_ARCHIVE: written by this call, or found from an earlier one.
	Adjustment *domain.PlatformAdjustment `json:"adjustment,omitempty"`
}

// ArchiveToReserve moves the DOKU era out of the platform account into a RESERVE archive
// account (owner PLATFORM_DOKU_ARCHIVE), in one MANUAL_ADJUSTMENT journal: PLATFORM −amount,
// RESERVE +amount, AVAILABLE both. The DOKU era is the platform's AVAILABLE entries for the
// transactions settled before the ledger recorded settled fees — money the ledger holds and the
// Singapay sub-account never did.
//
// The amount is computed from those entries and must equal req.ExpectedAmount; otherwise it is
// refused (ErrReserveArchiveAmountMismatch). It runs once: a second run is refused
// (ErrReserveArchiveAlreadyDone), and a unique index holds that against a concurrent one.
//
// With Apply false it writes nothing and returns what it would do (also when it would be
// refused: read AlreadyArchived and AmountMatches). With Apply true it decides again inside the
// transaction, after the platform account's row lock, and returns the result with the error
// when refused.
//
// The DOKU-era lines of the platform statement and the Platform Earnings chart do not move:
// both sum the platform's PRODUCT_TRANSACTION entries, and the archive's are sourced on the
// adjustment. Nothing that totals balances reads RESERVE accounts.
func (c *LedgerClient) ArchiveToReserve(ctx context.Context, req ReserveArchiveRequest) (*ReserveArchiveResult, error) {
	if req.ExpectedAmount <= 0 {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "the expected DOKU-era amount must be positive", nil)
	}
	note, ok := domain.CleanPlatformAdjustmentNote(req.Note)
	if !ok {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidPlatformAdjustment,
			fmt.Sprintf("the note is longer than %d characters", domain.MaxPlatformAdjustmentNoteLength), nil)
	}

	platform, err := c.platformAccount(ctx, c.repoProvider)
	if err != nil {
		return nil, err
	}

	if !req.Apply {
		result, err := c.planReserveArchive(ctx, c.repoProvider, platform, req.ExpectedAmount)
		if err != nil {
			return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the DOKU era", err)
		}
		return result, nil
	}

	var result *ReserveArchiveResult
	err = c.txProvider.TransactReadCommitted(ctx, func(dbtx repo.Tx) error {
		if _, err := dbtx.Account().GetByIDForUpdate(ctx, platform.UUID); err != nil {
			return fmt.Errorf("lock the platform account: %w", err)
		}

		plan, err := c.planReserveArchive(ctx, dbtx, platform, req.ExpectedAmount)
		if err != nil {
			return err
		}
		result = plan
		if plan.AlreadyArchived {
			return ledgererr.ErrReserveArchiveAlreadyDone
		}
		if !plan.AmountMatches {
			return ledgererr.ErrReserveArchiveAmountMismatch
		}

		reserve := plan.ReserveAccount
		if reserve == nil {
			account := domain.NewAccount(domain.OwnerTypeReserve, "", domain.ReserveArchiveOwnerID, platform.Currency)
			if err := dbtx.Account().Save(ctx, &account); err != nil {
				return fmt.Errorf("create the RESERVE account: %w", err)
			}
			reserve = &account
		}

		adjustment := domain.NewPlatformAdjustment(platform.UUID, domain.PlatformAdjustmentReserveArchive, -plan.Amount)
		adjustment.ReserveAccountUUID = reserve.UUID
		adjustment.AdminID = req.AdminID
		adjustment.AdminName = req.AdminName
		adjustment.Note = note
		for _, source := range plan.Sources {
			adjustment.SourceTransactionUUIDs = append(adjustment.SourceTransactionUUIDs, source.ProductTransactionUUID)
		}

		invoices := make([]string, 0, len(plan.Sources))
		for _, source := range plan.Sources {
			invoices = append(invoices, source.InvoiceNumber)
		}
		journal := domain.NewJournal(domain.EventTypeManualAdjustment, domain.SourceTypeManualAdjustment, adjustment.UUID, map[string]any{
			"reconciliation":       "platform-doku-era-archive",
			"adjustment_uuid":      adjustment.UUID,
			"reason":               "The DOKU era (transactions settled before the ledger recorded settled fees) is in the platform's ledger and was never in its Singapay sub-account; it moves to the RESERVE archive account, which no total reads.",
			"amount":               plan.Amount,
			"expected_amount":      req.ExpectedAmount,
			"invoices":             invoices,
			"reserve_account_uuid": reserve.UUID,
			"admin_id":             req.AdminID,
			"admin_name":           req.AdminName,
			"note":                 note,
		})
		adjustment.JournalUUID = journal.UUID

		if err := dbtx.Journal().Save(ctx, journal); err != nil {
			return err
		}
		if err := dbtx.LedgerEntry().SaveBatch(ctx, []*domain.LedgerEntry{
			domain.NewPlatformReconciliationEntry(journal.UUID, adjustment.UUID, platform.UUID, -plan.Amount),
			domain.NewPlatformReconciliationEntry(journal.UUID, adjustment.UUID, reserve.UUID, plan.Amount),
		}); err != nil {
			return err
		}
		if err := dbtx.PlatformReconciliation().SaveAdjustment(ctx, adjustment); err != nil {
			if repo.IsUniqueViolation(err, repo.ConstraintUniqueReserveArchive) {
				return ledgererr.ErrReserveArchiveAlreadyDone
			}
			return err
		}

		result.Applied = true
		result.ReserveAccount = reserve
		result.Adjustment = adjustment
		return nil
	})
	if err != nil {
		if isPlatformReconciliationRefusal(err) {
			return result, err
		}
		return result, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to archive the DOKU era", err)
	}

	c.logger.InfoContext(ctx, "Platform DOKU era archived to the RESERVE account",
		"adjustment_uuid", result.Adjustment.UUID,
		"reserve_account_uuid", result.ReserveAccount.UUID,
		"amount", result.Amount,
		"transactions", len(result.Sources),
		"platform_available_after", result.PlatformAvailableAfter,
	)
	return result, nil
}

// planReserveArchive computes the archive from provider's reads: inside the archive's
// transaction when applying, outside any when dry-running.
func (c *LedgerClient) planReserveArchive(ctx context.Context, provider interface {
	Account() domain.AccountRepository
	LedgerEntry() domain.LedgerEntryRepository
	PlatformReconciliation() domain.PlatformReconciliationRepository
}, platform *domain.Account, expected int64) (*ReserveArchiveResult, error) {
	sources, err := provider.PlatformReconciliation().ListReserveArchiveSources(ctx, platform.UUID)
	if err != nil {
		return nil, err
	}
	var amount int64
	for _, source := range sources {
		amount += source.Amount
	}

	_, platformAvailable, err := provider.LedgerEntry().GetAllBalances(ctx, platform.UUID)
	if err != nil {
		return nil, err
	}

	result := &ReserveArchiveResult{
		Amount:                  amount,
		ExpectedAmount:          expected,
		AmountMatches:           amount == expected,
		Sources:                 sources,
		PlatformAvailableBefore: platformAvailable,
		PlatformAvailableAfter:  platformAvailable,
	}

	reserve, err := provider.Account().GetByOwner(ctx, domain.OwnerTypeReserve, domain.ReserveArchiveOwnerID)
	switch {
	case err == nil:
		result.ReserveAccount = reserve
		_, reserveAvailable, err := provider.LedgerEntry().GetAllBalances(ctx, reserve.UUID)
		if err != nil {
			return nil, err
		}
		result.ReserveAvailableBefore = reserveAvailable
		result.ReserveAvailableAfter = reserveAvailable
	case !ledgererr.IsAppError(err, repo.ErrNotFound):
		return nil, err
	}

	archive, err := provider.PlatformReconciliation().GetReserveArchive(ctx)
	switch {
	case err == nil:
		result.AlreadyArchived = true
		result.Adjustment = archive
		return result, nil
	case !ledgererr.IsAppError(err, repo.ErrNotFound):
		return nil, err
	}

	result.PlatformAvailableAfter = platformAvailable - amount
	result.ReserveAvailableAfter = result.ReserveAvailableBefore + amount
	return result, nil
}
