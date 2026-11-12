package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// ─────────────────────────────────────────────────────────────────────────────
// Card platform-fee reconciliation — before the sweep
// ─────────────────────────────────────────────────────────────────────────────
//
// Singapay reports no fee and no net per transaction for a payment link, and a card payment
// is one, so bookSettlement books a card on the fee quoted at checkout (FeeReported = false,
// settled_fee_source ESTIMATE). That quote is whole rupiah; Singapay takes its 3% + Rp2.000 to
// the sen, and the two can differ by a few dozen sen. When Singapay took more, the seller's
// sub-account holds less than the ledger thinks, and the platform-fee transfer is refused
// with 422 once the seller has withdrawn everything else (SWIMLQ: 4041.00 booked, 4041.24
// taken, 3999.76 left for a 4000.00 transfer).
//
// The sub-account balance is the one figure that does contain Singapay's real cut. For one
// seller, in sen:
//
//	card platform share = sub-account available
//	                    − seller AVAILABLE in the ledger × 100
//	                    − platform fee already final but not transferred yet
//
// Everything else in the sub-account is accounted for: the seller's own money is their
// ledger balance (a seller is paid exactly what they were priced, docs/104), and a final fee
// is one Singapay reported, one recomputed here in an earlier round, or one corrected by
// hand. What remains is what the cards left the platform, and it is split over them.
//
// The sweep leaves ESTIMATE rows alone (GetSettledWithoutPlatformFeeTransfer), so a card is
// only swept once this has made it BALANCE. Hence the order the consuming service runs:
// settlement pass, then this, then the sweep.

// defaultCardFeeMaxSellers bounds one round: each seller costs one balance inquiry. A seller
// not reached keeps their cards ESTIMATE, unswept, until the next round.
const defaultCardFeeMaxSellers = 100

// The outcome of one seller's round. APPLIED and HELD are recorded in
// platform_fee_reconciliations; SKIPPED and FAILED are not, and the next round simply tries
// again.
const (
	CardFeeOutcomeApplied = string(domain.PlatformFeeReconciliationApplied)
	CardFeeOutcomeHeld    = string(domain.PlatformFeeReconciliationHeld)
	// CardFeeOutcomeSkipped: something else moved first — the ledger changed while the
	// balance was read, or another round recomputed these cards. Nothing was written.
	CardFeeOutcomeSkipped = "SKIPPED"
	// CardFeeOutcomeFailed: the round could not be completed or recorded (a read or a write
	// failed). Nothing was written.
	CardFeeOutcomeFailed = "FAILED"
)

// CardPlatformFeeReconciliationOptions tunes one round. The zero value is the intended
// configuration.
type CardPlatformFeeReconciliationOptions struct {
	// TolerancePerCardMinor is how far, in sen per card, the computed platform share may be
	// from the priced one before the seller's round is held. 0 uses
	// domain.DefaultCardFeeTolerancePerCardMinor (Rp100).
	TolerancePerCardMinor int64

	// MaxSellers caps the sellers one round reads a balance for. 0 uses 100.
	MaxSellers int
}

// CardPlatformFeeReconciliationResult is one round, seller by seller.
type CardPlatformFeeReconciliationResult struct {
	Applied int                           `json:"applied"`
	Held    int                           `json:"held"`
	Skipped int                           `json:"skipped"`
	Failed  int                           `json:"failed"`
	Sellers []CardPlatformFeeSellerResult `json:"sellers"`
}

// CardPlatformFeeSellerResult is what one seller's round read and decided. Amounts ending in
// Minor are sen; SellerLedgerAvailable is whole rupiah, as the ledger keeps it.
type CardPlatformFeeSellerResult struct {
	SellerAccountID   string `json:"seller_account_id"`
	SingapayAccountID string `json:"singapay_account_id,omitempty"`

	// Outcome is one of the CardFeeOutcome values. HoldReason is set when HELD.
	Outcome    string `json:"outcome"`
	HoldReason string `json:"hold_reason,omitempty"`
	// Detail says why a round was SKIPPED or FAILED, or what the balance inquiry answered when
	// it failed.
	Detail string `json:"detail,omitempty"`

	// ReconciliationUUID is the platform_fee_reconciliations row, for APPLIED and HELD.
	ReconciliationUUID string `json:"reconciliation_uuid,omitempty"`

	CardCount                      int    `json:"card_count"`
	SingapayAvailableMinor         *int64 `json:"singapay_available_minor,omitempty"`
	SellerLedgerAvailable          int64  `json:"seller_ledger_available"`
	CertainUnsweptPlatformFeeMinor int64  `json:"certain_unswept_platform_fee_minor"`
	ComputedPlatformFeeMinor       *int64 `json:"computed_platform_fee_minor,omitempty"`
	NominalPlatformFeeMinor        int64  `json:"nominal_platform_fee_minor"`
	ToleranceMinor                 int64  `json:"tolerance_minor"`

	// Invoices are the cards, with their new figures when APPLIED.
	Invoices []CardPlatformFeeInvoiceResult `json:"invoices,omitempty"`
}

// CardPlatformFeeInvoiceResult is one card of a seller's round.
type CardPlatformFeeInvoiceResult struct {
	TransactionID string `json:"transaction_id"`
	InvoiceNumber string `json:"invoice_number"`

	PricedPlatformFeeMinor   int64 `json:"priced_platform_fee_minor"`
	PreviousPlatformFeeMinor int64 `json:"previous_platform_fee_minor"`
	PreviousGatewayFeeMinor  int64 `json:"previous_gateway_fee_minor"`

	// The recomputed figures, set when APPLIED. PlatformFeeMinor is what the sweep moves.
	PlatformFeeMinor      *int64 `json:"platform_fee_minor,omitempty"`
	GatewayFeeMinor       *int64 `json:"gateway_fee_minor,omitempty"`
	PlatformResidualMinor *int64 `json:"platform_residual_minor,omitempty"`

	// PlatformEntryAdjustment is the platform's FEE_ADJUSTMENT entry, whole rupiah; 0 when
	// the whole-rupiah part did not change and no entry was written.
	PlatformEntryAdjustment int64 `json:"platform_entry_adjustment"`
}

// errCardFeeSuperseded marks a round that lost a race: a card's row no longer matched what
// was read when the round went to write. It never leaves this file.
var errCardFeeSuperseded = errors.New("a card was recomputed or changed by someone else first")

// ReconcileCardPlatformFees recomputes the platform fee of every card payment still settled
// on the checkout estimate, seller by seller, from the seller's Singapay sub-account balance.
// Run it after the settlement pass and before ProcessPlatformFeeTransfer: the sweep moves a
// card's fee only once this has made it final.
//
// For each seller holding such cards (oldest first, at most opts.MaxSellers):
//
//  1. One statement reads the seller's AVAILABLE balance, the final but unswept platform fee,
//     the payouts in flight and the cards (domain.CardFeeSnapshot).
//  2. A payout in PENDING or PROCESSING holds the round (DISBURSEMENT_IN_FLIGHT): the ledger
//     has debited it and Singapay may not have.
//  3. Singapay's balance inquiry is asked for the sub-account's available balance. A failure
//     holds the round (BALANCE_READ_FAILED).
//  4. The statement is read again. A payout that started holds the round; any other change
//     skips it — the balance may predate the change — and nothing is recorded.
//  5. The share is computed and checked against the priced fee (domain.ComputeCardPlatformFees).
//     Too far off holds the round (DEVIATION_ABOVE_TOLERANCE).
//  6. Otherwise, in one database transaction: each card's conditional ESTIMATE→BALANCE update
//     first (SaveBalancedFeesIf; a card that no longer matches rolls everything back), then
//     the round's row, a RECONCILIATION journal per card, the platform's FEE_ADJUSTMENT
//     entries where the whole-rupiah part changed, and the items.
//
// A held round records its row and one item per card with no result, and writes nothing
// else: the cards stay ESTIMATE, unswept, and the next round tries again.
//
// The error is non-nil only when the round could not start. One seller's failure does not
// stop the others; it is reported in the result.
func (c *LedgerClient) ReconcileCardPlatformFees(ctx context.Context, opts CardPlatformFeeReconciliationOptions) (*CardPlatformFeeReconciliationResult, error) {
	maxSellers := opts.MaxSellers
	if maxSellers <= 0 {
		maxSellers = defaultCardFeeMaxSellers
	}

	result := &CardPlatformFeeReconciliationResult{Sellers: []CardPlatformFeeSellerResult{}}

	sellers, err := c.repoProvider.PlatformFeeReconciliation().SellersWithEstimatedCardFees(ctx, maxSellers)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to list sellers with card fees still on the estimate", err)
	}
	if len(sellers) == 0 {
		c.logger.InfoContext(ctx, "No card platform fee to reconcile")
		return result, nil
	}

	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get platform account", err)
	}

	for _, sellerAccountID := range sellers {
		// Between sellers, never inside one: each seller's write is a single transaction.
		if ctx.Err() != nil {
			break
		}

		seller := c.reconcileSellerCardFees(ctx, platformAccount, sellerAccountID, opts.TolerancePerCardMinor)
		switch seller.Outcome {
		case CardFeeOutcomeApplied:
			result.Applied++
		case CardFeeOutcomeHeld:
			result.Held++
		case CardFeeOutcomeSkipped:
			result.Skipped++
		default:
			result.Failed++
		}
		result.Sellers = append(result.Sellers, seller)
	}

	c.logger.InfoContext(ctx, "Card platform fee reconciliation finished",
		"sellers", len(result.Sellers),
		"applied", result.Applied,
		"held", result.Held,
		"skipped", result.Skipped,
		"failed", result.Failed,
	)

	return result, nil
}

// reconcileSellerCardFees runs one seller's round. See ReconcileCardPlatformFees.
func (c *LedgerClient) reconcileSellerCardFees(
	ctx context.Context,
	platformAccount *domain.Account,
	sellerAccountID string,
	tolerancePerCardMinor int64,
) CardPlatformFeeSellerResult {
	out := CardPlatformFeeSellerResult{SellerAccountID: sellerAccountID}

	failed := func(detail string, err error) CardPlatformFeeSellerResult {
		out.Outcome = CardFeeOutcomeFailed
		out.Detail = detail
		if err != nil {
			out.Detail = fmt.Sprintf("%s: %v", detail, err)
		}
		c.logger.ErrorContext(ctx, "Card platform fee reconciliation failed for a seller; nothing was written",
			"seller_account_id", sellerAccountID,
			"singapay_account_id", out.SingapayAccountID,
			"detail", out.Detail,
		)
		return out
	}

	seller, err := c.repoProvider.Account().GetByID(ctx, sellerAccountID)
	if err != nil {
		return failed("failed to read the seller account", err)
	}
	if seller.SingapayAccountID == "" {
		return failed("seller account has no Singapay sub-account id", nil)
	}
	out.SingapayAccountID = seller.SingapayAccountID

	reconciliations := c.repoProvider.PlatformFeeReconciliation()

	before, err := reconciliations.ReadCardFeeSnapshot(ctx, sellerAccountID)
	if err != nil {
		return failed("failed to read the seller's ledger", err)
	}
	if len(before.Cards) == 0 {
		// Listed a moment ago and gone now: another round, or a person, got here first.
		out.Outcome = CardFeeOutcomeSkipped
		out.Detail = "no card on the estimate is left for this seller"
		return out
	}
	out.describe(before, tolerancePerCardMinor)

	if before.DisbursementsInFlight > 0 {
		rec := domain.NewPlatformFeeReconciliation(before, seller.SingapayAccountID, tolerancePerCardMinor)
		return c.holdCardFees(ctx, out, rec, before, domain.HoldDisbursementInFlight,
			fmt.Sprintf("%d payout(s) in PENDING or PROCESSING", before.DisbursementsInFlight))
	}

	balance, err := c.gateway.GetAccountBalance(ctx, seller.SingapayAccountID)
	if err != nil {
		rec := domain.NewPlatformFeeReconciliation(before, seller.SingapayAccountID, tolerancePerCardMinor)
		return c.holdCardFees(ctx, out, rec, before, domain.HoldBalanceReadFailed, err.Error())
	}

	after, err := reconciliations.ReadCardFeeSnapshot(ctx, sellerAccountID)
	if err != nil {
		return failed("failed to read the seller's ledger again after the balance inquiry", err)
	}

	rec := domain.NewPlatformFeeReconciliation(after, seller.SingapayAccountID, tolerancePerCardMinor)
	recordBalance(rec, balance)
	out.SingapayAvailableMinor = rec.SingapayAvailableMinor

	if after.DisbursementsInFlight > 0 && len(after.Cards) > 0 {
		out.describe(after, tolerancePerCardMinor)
		return c.holdCardFees(ctx, out, rec, after, domain.HoldDisbursementInFlight,
			fmt.Sprintf("%d payout(s) started while the balance was read", after.DisbursementsInFlight))
	}
	if !before.SameAs(after) {
		// The balance may be from before the change or after it; there is no telling which.
		// Nothing is recorded, and the next round reads both again.
		out.Outcome = CardFeeOutcomeSkipped
		out.Detail = "the seller's ledger changed while the Singapay balance was read"
		c.logger.WarnContext(ctx, "Card platform fee reconciliation skipped a seller: the ledger moved during the balance read",
			"seller_account_id", sellerAccountID,
			"singapay_account_id", seller.SingapayAccountID,
		)
		return out
	}

	computation, err := domain.ComputeCardPlatformFees(balance.Available.Minor(), after, tolerancePerCardMinor)
	if err != nil {
		return failed("failed to compute the card platform share", err)
	}
	computed := computation.ComputedMinor
	rec.ComputedPlatformFeeMinor = &computed
	out.ComputedPlatformFeeMinor = &computed

	if computation.HoldReason != "" {
		return c.holdCardFees(ctx, out, rec, after, computation.HoldReason, fmt.Sprintf(
			"computed %s against %s priced, tolerance %s",
			formatMinor(computed), formatMinor(computation.NominalMinor), formatMinor(computation.ToleranceMinor)))
	}

	invoices, err := c.applyCardFees(ctx, platformAccount, rec, after, computation)
	if errors.Is(err, errCardFeeSuperseded) {
		out.Outcome = CardFeeOutcomeSkipped
		out.Detail = err.Error()
		c.logger.WarnContext(ctx, "Card platform fee reconciliation skipped a seller: a card changed before the write",
			"seller_account_id", sellerAccountID,
			"singapay_account_id", seller.SingapayAccountID,
		)
		return out
	}
	if err != nil {
		return failed("failed to write the recomputed card fees", err)
	}

	out.Outcome = CardFeeOutcomeApplied
	out.ReconciliationUUID = rec.UUID
	out.Invoices = invoices

	for _, invoice := range invoices {
		c.logger.InfoContext(ctx, "Card platform fee recomputed from the seller's Singapay balance",
			"reconciliation_uuid", rec.UUID,
			"transaction_id", invoice.TransactionID,
			"invoice_number", invoice.InvoiceNumber,
			"previous_platform_fee", formatMinor(invoice.PreviousPlatformFeeMinor),
			"platform_fee", formatMinor(*invoice.PlatformFeeMinor),
			"previous_gateway_fee", formatMinor(invoice.PreviousGatewayFeeMinor),
			"gateway_fee", formatMinor(*invoice.GatewayFeeMinor),
			"platform_residual_minor", *invoice.PlatformResidualMinor,
			"platform_entry_adjustment", invoice.PlatformEntryAdjustment,
		)
	}
	c.logger.InfoContext(ctx, "Card platform fees applied for a seller",
		"reconciliation_uuid", rec.UUID,
		"seller_account_id", sellerAccountID,
		"singapay_account_id", seller.SingapayAccountID,
		"cards", len(invoices),
		"singapay_available", formatMinor(*rec.SingapayAvailableMinor),
		"seller_ledger_available", after.SellerLedgerAvailable,
		"certain_unswept_platform_fee", formatMinor(after.CertainUnsweptPlatformFeeMinor),
		"computed_platform_fee", formatMinor(computed),
		"priced_platform_fee", formatMinor(computation.NominalMinor),
	)

	return out
}

// describe fills the ledger side of the result from a snapshot.
func (out *CardPlatformFeeSellerResult) describe(snapshot *domain.CardFeeSnapshot, tolerancePerCardMinor int64) {
	if tolerancePerCardMinor <= 0 {
		tolerancePerCardMinor = domain.DefaultCardFeeTolerancePerCardMinor
	}
	out.CardCount = len(snapshot.Cards)
	out.SellerLedgerAvailable = snapshot.SellerLedgerAvailable
	out.CertainUnsweptPlatformFeeMinor = snapshot.CertainUnsweptPlatformFeeMinor
	out.NominalPlatformFeeMinor = snapshot.NominalPlatformFeeMinor()
	out.ToleranceMinor = tolerancePerCardMinor * int64(len(snapshot.Cards))
	out.Invoices = make([]CardPlatformFeeInvoiceResult, 0, len(snapshot.Cards))
	for _, card := range snapshot.Cards {
		out.Invoices = append(out.Invoices, CardPlatformFeeInvoiceResult{
			TransactionID:            card.ProductTransactionUUID,
			InvoiceNumber:            card.InvoiceNumber,
			PricedPlatformFeeMinor:   card.PricedPlatformFeeMinor(),
			PreviousPlatformFeeMinor: card.PreviousPlatformFeeMinor(),
			PreviousGatewayFeeMinor:  card.PreviousGatewayFeeMinor(),
		})
	}
}

// recordBalance copies the balance inquiry's answer onto the round's row.
func recordBalance(rec *domain.PlatformFeeReconciliation, balance *singapay.Balance) {
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

// holdCardFees records a held round: its row and one item per card with no result, in one
// database transaction. Nothing else is written.
func (c *LedgerClient) holdCardFees(
	ctx context.Context,
	out CardPlatformFeeSellerResult,
	rec *domain.PlatformFeeReconciliation,
	snapshot *domain.CardFeeSnapshot,
	reason domain.PlatformFeeReconciliationHoldReason,
	detail string,
) CardPlatformFeeSellerResult {
	rec.Hold(reason)

	items := make([]*domain.PlatformFeeReconciliationItem, 0, len(snapshot.Cards))
	for _, card := range snapshot.Cards {
		items = append(items, domain.NewPlatformFeeReconciliationItem(rec.UUID, card))
	}

	err := c.txProvider.Transact(ctx, func(dbtx repo.Tx) error {
		if err := dbtx.PlatformFeeReconciliation().Save(ctx, rec); err != nil {
			return err
		}
		return dbtx.PlatformFeeReconciliation().SaveItems(ctx, items)
	})

	out.HoldReason = string(reason)
	out.Detail = detail
	if err != nil {
		out.Outcome = CardFeeOutcomeFailed
		out.Detail = fmt.Sprintf("held (%s: %s), and the hold could not be recorded: %v", reason, detail, err)
		c.logger.ErrorContext(ctx, "Card platform fee reconciliation could not record a hold",
			"seller_account_id", rec.SellerAccountID,
			"hold_reason", string(reason),
			"detail", detail,
			"error", err,
		)
		return out
	}

	out.Outcome = CardFeeOutcomeHeld
	out.ReconciliationUUID = rec.UUID
	c.logger.WarnContext(ctx, "Card platform fees held for a seller; the cards stay unswept until a later round applies them",
		"reconciliation_uuid", rec.UUID,
		"seller_account_id", rec.SellerAccountID,
		"singapay_account_id", rec.SingapayAccountID,
		"hold_reason", string(reason),
		"detail", detail,
		"cards", rec.CardCount,
	)
	return out
}

// applyCardFees writes an applied round in one database transaction and returns the cards'
// new figures. errCardFeeSuperseded means a card no longer matched and nothing was written.
func (c *LedgerClient) applyCardFees(
	ctx context.Context,
	platformAccount *domain.Account,
	rec *domain.PlatformFeeReconciliation,
	snapshot *domain.CardFeeSnapshot,
	computation domain.CardFeeComputation,
) ([]CardPlatformFeeInvoiceResult, error) {
	rec.Apply()

	type cardWrite struct {
		card        domain.CardFeeCandidate
		shareMinor  int64
		gatewayFee  int64
		residual    int64
		entryAmount int64
	}

	writes := make([]cardWrite, len(snapshot.Cards))
	journals := make([]*domain.Journal, 0, len(snapshot.Cards))
	items := make([]*domain.PlatformFeeReconciliationItem, 0, len(snapshot.Cards))
	var entries []*domain.LedgerEntry
	invoices := make([]CardPlatformFeeInvoiceResult, 0, len(snapshot.Cards))

	for i, card := range snapshot.Cards {
		share := computation.Shares[i]
		shareRupiah, residual := domain.SplitPlatformFeeMinor(share)
		previousRupiah, previousResidual := domain.SplitPlatformFeeMinor(card.PreviousPlatformFeeMinor())

		w := cardWrite{
			card:       card,
			shareMinor: share,
			// The payer's fees are fixed; recomputing only moves sen between the two parts.
			gatewayFee:  card.FeesMinor() - share,
			residual:    residual,
			entryAmount: shareRupiah - previousRupiah,
		}
		writes[i] = w

		journal := domain.NewJournal(
			domain.EventTypeReconciliation,
			domain.SourceTypeProductTransaction,
			card.ProductTransactionUUID,
			map[string]any{
				"reconciliation":      "card-platform-fee",
				"reconciliation_uuid": rec.UUID,
				"reason": "Card payment settled on the checkout's gateway fee: Singapay reports no fee for a payment link. " +
					"The platform share is recomputed from the seller's Singapay sub-account balance " +
					"(available - seller AVAILABLE x 100 - final unswept platform fee) and split over the seller's cards " +
					"(docs/104-fee-mismatch-reconciliation.md); the seller keeps the priced net.",
				"invoice_number":                      card.InvoiceNumber,
				"payment_channel":                     ChannelCreditCard,
				"settled_fee_source":                  string(domain.SettledFeeSourceBalance),
				"previous_settled_fee_source":         string(domain.SettledFeeSourceEstimate),
				"priced_platform_fee":                 card.PlatformFee,
				"estimated_gateway_fee":               card.GatewayFee,
				"settled_platform_fee_minor":          share,
				"previous_settled_platform_fee_minor": card.PreviousPlatformFeeMinor(),
				"settled_gateway_fee_minor":           w.gatewayFee,
				"previous_settled_gateway_fee_minor":  card.PreviousGatewayFeeMinor(),
				"platform_residual_minor":             residual,
				"previous_platform_residual_minor":    previousResidual,
				"platform_entry_adjustment":           w.entryAmount,
				"allocation_weight_minor":             computation.Weights[i],
				"seller_singapay_available_minor":     *rec.SingapayAvailableMinor,
				"seller_ledger_available":             snapshot.SellerLedgerAvailable,
				"certain_unswept_platform_fee_minor":  snapshot.CertainUnsweptPlatformFeeMinor,
				"computed_platform_fee_minor":         computation.ComputedMinor,
				"priced_platform_fee_minor":           computation.NominalMinor,
				"card_count":                          len(snapshot.Cards),
			},
		)
		journals = append(journals, journal)

		item := domain.NewPlatformFeeReconciliationItem(rec.UUID, card)
		shareCopy, gatewayCopy, residualCopy := share, w.gatewayFee, residual
		item.AllocatedPlatformFeeMinor = &shareCopy
		item.SettledGatewayFeeMinor = &gatewayCopy
		item.PlatformResidualMinor = &residualCopy
		item.JournalUUID = journal.UUID

		if w.entryAmount != 0 {
			entry := domain.NewPlatformFeeAdjustmentEntry(journal.UUID, card.ProductTransactionUUID, platformAccount.UUID, w.entryAmount)
			entry.Metadata = map[string]any{
				"reconciliation_uuid": rec.UUID,
				"reason": fmt.Sprintf("Card platform fee recomputed from the seller's Singapay balance: %s -> %s; whole-rupiah part %d -> %d, %d sen are platform_residual.",
					formatMinor(card.PreviousPlatformFeeMinor()), formatMinor(share), previousRupiah, shareRupiah, residual),
			}
			entries = append(entries, entry)
			item.LedgerEntryUUID = entry.UUID
		}
		items = append(items, item)

		invoices = append(invoices, CardPlatformFeeInvoiceResult{
			TransactionID:            card.ProductTransactionUUID,
			InvoiceNumber:            card.InvoiceNumber,
			PricedPlatformFeeMinor:   card.PricedPlatformFeeMinor(),
			PreviousPlatformFeeMinor: card.PreviousPlatformFeeMinor(),
			PreviousGatewayFeeMinor:  card.PreviousGatewayFeeMinor(),
			PlatformFeeMinor:         &shareCopy,
			GatewayFeeMinor:          &gatewayCopy,
			PlatformResidualMinor:    &residualCopy,
			PlatformEntryAdjustment:  w.entryAmount,
		})
	}

	err := c.txProvider.Transact(ctx, func(dbtx repo.Tx) error {
		// The compare-and-sets come first: they take the rows' locks, and a card that moved
		// since the snapshot means the whole seller is rolled back before anything else is
		// written.
		for _, w := range writes {
			moved, err := dbtx.ProductTransaction().SaveBalancedFeesIf(ctx, w.card.ProductTransactionUUID,
				w.card.SettledPlatformFeeMinor, w.card.SettledGatewayFeeMinor,
				w.shareMinor, w.gatewayFee, w.residual)
			if err != nil {
				return err
			}
			if !moved {
				return fmt.Errorf("%w: %s", errCardFeeSuperseded, w.card.InvoiceNumber)
			}
		}

		if err := dbtx.PlatformFeeReconciliation().Save(ctx, rec); err != nil {
			return err
		}
		for _, journal := range journals {
			if err := dbtx.Journal().Save(ctx, journal); err != nil {
				return err
			}
		}
		// SaveBatch keeps balance_after and ledger_accounts.available_balance right.
		if len(entries) > 0 {
			if err := dbtx.LedgerEntry().SaveBatch(ctx, entries); err != nil {
				return err
			}
		}
		if err := dbtx.PlatformFeeReconciliation().SaveItems(ctx, items); err != nil {
			if repo.IsUniqueViolation(err, repo.ConstraintUniqueAppliedReconciliationItem) {
				return fmt.Errorf("%w: %v", errCardFeeSuperseded, err)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return invoices, nil
}
