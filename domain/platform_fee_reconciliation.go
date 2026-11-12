package domain

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/21strive/redifu"
)

// ─────────────────────────────────────────────────────────────────────────────
// Where a settled fee came from
// ─────────────────────────────────────────────────────────────────────────────

// SettledFeeSource says where a settled transaction's fee figures came from
// (product_transactions.settled_fee_source, migration 032).
//
// It exists because two kinds of settled figure look the same in the columns. Singapay
// reports the fee it took on a virtual account, QRIS or e-wallet payment, so those figures
// are facts. It reports none on a payment link, and a card payment is one, so settlement
// copies the fee quoted at checkout. That copy is whole rupiah, while Singapay takes the
// real fee to the sen, and the platform-fee sweep would move the copy out of a sub-account
// that holds a few sen less (SWIMLQ, 2026-09-25: 4041.00 booked, 4041.24 taken).
//
// NULL in the column is a row settled before migration 032, or by code older than it, and
// is read as GATEWAY.
type SettledFeeSource string

const (
	// SettledFeeSourceGateway: Singapay reported this transaction's fee. Final at settlement.
	SettledFeeSourceGateway SettledFeeSource = "GATEWAY"

	// SettledFeeSourceEstimate: Singapay reports no fee for the channel, so settlement copied
	// the checkout estimate. Not final: the sweep leaves the row alone, and a card's figures
	// are replaced by ReconcileCardPlatformFees.
	SettledFeeSourceEstimate SettledFeeSource = "ESTIMATE"

	// SettledFeeSourceBalance: recomputed from the seller's Singapay sub-account balance by
	// ReconcileCardPlatformFees. Final.
	SettledFeeSourceBalance SettledFeeSource = "BALANCE"

	// SettledFeeSourceManual: corrected by hand, outside this package (SWIMLQ, 2026-11-11).
	// Final.
	SettledFeeSourceManual SettledFeeSource = "MANUAL"
)

// SettledFeeSourceFor is the source a settlement records for the fee figures it books.
func SettledFeeSourceFor(feeReported bool) SettledFeeSource {
	if feeReported {
		return SettledFeeSourceGateway
	}
	return SettledFeeSourceEstimate
}

// ─────────────────────────────────────────────────────────────────────────────
// The card platform-fee reconciliation
// ─────────────────────────────────────────────────────────────────────────────

// PlatformFeeReconciliationOutcome is what one seller's round decided.
type PlatformFeeReconciliationOutcome string

const (
	// PlatformFeeReconciliationApplied: the cards' figures were replaced, in one database
	// transaction with this row.
	PlatformFeeReconciliationApplied PlatformFeeReconciliationOutcome = "APPLIED"

	// PlatformFeeReconciliationHeld: nothing was written but this row and its items. The
	// cards stay ESTIMATE, the sweep keeps leaving them alone, and the next round tries again.
	PlatformFeeReconciliationHeld PlatformFeeReconciliationOutcome = "HELD"
)

// PlatformFeeReconciliationHoldReason is why a round was held.
type PlatformFeeReconciliationHoldReason string

const (
	// HoldDeviationAboveTolerance: the computed platform share is further from the priced one
	// than the tolerance allows, or below zero. Money the ledger has not booked is in the
	// sub-account, or money the ledger has booked is not there yet.
	HoldDeviationAboveTolerance PlatformFeeReconciliationHoldReason = "DEVIATION_ABOVE_TOLERANCE"

	// HoldDisbursementInFlight: a payout of the seller is PENDING or PROCESSING. The ledger
	// has debited it and Singapay may or may not have, so the balance proves nothing.
	HoldDisbursementInFlight PlatformFeeReconciliationHoldReason = "DISBURSEMENT_IN_FLIGHT"

	// HoldBalanceReadFailed: Singapay's balance inquiry failed.
	HoldBalanceReadFailed PlatformFeeReconciliationHoldReason = "BALANCE_READ_FAILED"
)

// DefaultCardFeeTolerancePerCardMinor is how far, in sen per card, the computed platform
// share may be from the priced one: Rp100.
//
// The real difference is a few dozen sen a card — the rounding of a percentage fee that
// Singapay takes to the sen and checkout quotes to the rupiah. Anything larger is not a
// rounding: it is money the ledger and the sub-account disagree about, and booking it as a
// fee would hide it.
const DefaultCardFeeTolerancePerCardMinor int64 = 10000

// The card rate the checkout prices with: 3,0% of what the payer is charged plus Rp2.000
// (fee_configs CREDIT_CARD, HYBRID, loaded by migration 020 and activated by 029). Used only
// to weigh one seller's computed share across their cards; the total itself always comes
// from the balance.
const (
	cardRatePercent        int64 = 3
	cardFixedFeeMinor      int64 = 200000
	cardRatePercentDivisor int64 = 100
)

// ExpectedCardPlatformShareMinor is the platform's share of a card payment as the checkout
// formula expects it, in sen:
//
//	total_charged×100 − seller_net_amount×100 − (3% × total_charged×100 + 200000)
//
// 3% of the charge in sen is the charge in rupiah × 3, so the figure is exact.
func ExpectedCardPlatformShareMinor(totalCharged, sellerNetAmount int64) int64 {
	totalMinor := RupiahToMinor(totalCharged)
	cardFeeMinor := totalMinor*cardRatePercent/cardRatePercentDivisor + cardFixedFeeMinor
	return totalMinor - RupiahToMinor(sellerNetAmount) - cardFeeMinor
}

// CardFeeCandidate is one card payment whose settled platform fee is still the checkout
// estimate: SETTLED, not transferred, settled_fee_source ESTIMATE, channel CREDIT_CARD,
// GATEWAY_ON_CUSTOMER, with a platform fee, sold by a seller account. A value read for the
// reconciliation, not a row of its own.
type CardFeeCandidate struct {
	ProductTransactionUUID string
	InvoiceNumber          string

	// The checkout's figures, whole rupiah.
	PlatformFee     int64
	GatewayFee      int64
	TotalCharged    int64
	SellerNetAmount int64

	// What settlement booked, in sen. nil is NULL: never written by bookSettlement, which
	// always sets them, but read as the priced figures (see PreviousPlatformFeeMinor).
	SettledPlatformFeeMinor *int64
	SettledGatewayFeeMinor  *int64
	PlatformResidualMinor   *int64

	SettledAt *time.Time
}

// PricedPlatformFeeMinor is the platform fee quoted at checkout, in sen.
func (c CardFeeCandidate) PricedPlatformFeeMinor() int64 { return RupiahToMinor(c.PlatformFee) }

// PreviousPlatformFeeMinor is the settled platform fee the reconciliation replaces, in sen:
// the booked figure, or the priced one where none was booked (the sweep's own fallback).
func (c CardFeeCandidate) PreviousPlatformFeeMinor() int64 {
	if c.SettledPlatformFeeMinor != nil {
		return *c.SettledPlatformFeeMinor
	}
	return RupiahToMinor(c.PlatformFee)
}

// PreviousGatewayFeeMinor is the settled gateway fee the reconciliation replaces, in sen.
func (c CardFeeCandidate) PreviousGatewayFeeMinor() int64 {
	if c.SettledGatewayFeeMinor != nil {
		return *c.SettledGatewayFeeMinor
	}
	return RupiahToMinor(c.GatewayFee)
}

// FeesMinor is what the payer paid above the seller's net, in sen: the gateway fee and the
// platform fee together. Recomputing moves sen between the two and never changes this.
func (c CardFeeCandidate) FeesMinor() int64 {
	return RupiahToMinor(c.TotalCharged) - RupiahToMinor(c.SellerNetAmount)
}

// CardFeeSnapshot is the ledger side of one seller's round, read in ONE statement so that
// every figure in it describes the same moment — a settlement pass or a payout committing
// between two reads cannot pair a balance from before it with fees from after it.
type CardFeeSnapshot struct {
	SellerAccountID string

	// SellerLedgerAvailable is the seller's AVAILABLE balance, whole rupiah: the sum of the
	// bucket's ledger entries, as a withdrawal derives it.
	SellerLedgerAvailable int64

	// CertainUnsweptPlatformFeeMinor is the platform fee, in sen, that is already final but
	// still in the seller's sub-account: SETTLED, not transferred, settled_fee_source not
	// ESTIMATE, at COALESCE(settled_platform_fee, platform_fee×100). It includes a card made
	// BALANCE in an earlier round whose transfer has not succeeded yet, so its share is never
	// counted twice.
	CertainUnsweptPlatformFeeMinor int64

	// DisbursementsInFlight counts the seller's payouts in PENDING or PROCESSING.
	DisbursementsInFlight int

	// Cards are the seller's candidates, oldest settlement first.
	Cards []CardFeeCandidate
}

// SameAs reports whether two snapshots describe the same ledger state: the same balance,
// the same certain fees, the same payouts in flight and the same cards with the same
// booked figures. A round re-reads after asking Singapay and acts only when nothing moved
// in between.
func (s *CardFeeSnapshot) SameAs(other *CardFeeSnapshot) bool {
	if s == nil || other == nil {
		return s == other
	}
	if s.SellerAccountID != other.SellerAccountID ||
		s.SellerLedgerAvailable != other.SellerLedgerAvailable ||
		s.CertainUnsweptPlatformFeeMinor != other.CertainUnsweptPlatformFeeMinor ||
		s.DisbursementsInFlight != other.DisbursementsInFlight ||
		len(s.Cards) != len(other.Cards) {
		return false
	}
	for i := range s.Cards {
		a, b := s.Cards[i], other.Cards[i]
		if a.ProductTransactionUUID != b.ProductTransactionUUID ||
			a.PreviousPlatformFeeMinor() != b.PreviousPlatformFeeMinor() ||
			a.PreviousGatewayFeeMinor() != b.PreviousGatewayFeeMinor() ||
			!sameNullableMinor(a.SettledPlatformFeeMinor, b.SettledPlatformFeeMinor) ||
			!sameNullableMinor(a.SettledGatewayFeeMinor, b.SettledGatewayFeeMinor) {
			return false
		}
	}
	return true
}

func sameNullableMinor(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// NominalPlatformFeeMinor is the cards' platform fee as priced, in sen.
func (s *CardFeeSnapshot) NominalPlatformFeeMinor() int64 {
	var total int64
	for _, card := range s.Cards {
		total += card.PricedPlatformFeeMinor()
	}
	return total
}

// CardFeeComputation is the arithmetic of one seller's round, before anything is written.
type CardFeeComputation struct {
	// ComputedMinor is the cards' platform share together, in sen:
	//
	//	sub-account available − seller AVAILABLE × 100 − certain unswept platform fee
	ComputedMinor int64

	// NominalMinor is the cards' platform fee as priced, in sen.
	NominalMinor int64

	// ToleranceMinor is the per-card tolerance times the number of cards, in sen.
	ToleranceMinor int64

	// HoldReason is empty when the round may be applied.
	HoldReason PlatformFeeReconciliationHoldReason

	// Shares is ComputedMinor split over the cards, in the order of the snapshot, summing to
	// it exactly. nil when held.
	Shares []int64

	// Weights are what the split was proportional to, in sen, in the same order.
	Weights []int64
}

// ComputeCardPlatformFees solves one seller's round:
//
//	computed = singapayAvailableMinor − SellerLedgerAvailable×100 − CertainUnsweptPlatformFeeMinor
//
// and holds it when the result is negative, further than the tolerance from the priced fee,
// or would leave a card's gateway fee below zero. Otherwise it splits the result over the
// cards in proportion to ExpectedCardPlatformShareMinor.
//
// tolerancePerCardMinor ≤ 0 means DefaultCardFeeTolerancePerCardMinor.
func ComputeCardPlatformFees(singapayAvailableMinor int64, snapshot *CardFeeSnapshot, tolerancePerCardMinor int64) (CardFeeComputation, error) {
	if snapshot == nil || len(snapshot.Cards) == 0 {
		return CardFeeComputation{}, errors.New("no card to reconcile")
	}
	if tolerancePerCardMinor <= 0 {
		tolerancePerCardMinor = DefaultCardFeeTolerancePerCardMinor
	}

	result := CardFeeComputation{
		ComputedMinor: singapayAvailableMinor -
			RupiahToMinor(snapshot.SellerLedgerAvailable) -
			snapshot.CertainUnsweptPlatformFeeMinor,
		NominalMinor:   snapshot.NominalPlatformFeeMinor(),
		ToleranceMinor: tolerancePerCardMinor * int64(len(snapshot.Cards)),
	}

	deviation := result.ComputedMinor - result.NominalMinor
	if deviation < 0 {
		deviation = -deviation
	}
	if result.ComputedMinor < 0 || deviation > result.ToleranceMinor {
		result.HoldReason = HoldDeviationAboveTolerance
		return result, nil
	}

	weights := CardPlatformShareWeights(snapshot.Cards)
	shares, err := AllocateProportionally(result.ComputedMinor, weights)
	if err != nil {
		return CardFeeComputation{}, err
	}

	// A share larger than everything the payer paid above the seller's net would book a
	// negative gateway fee. Within any sane tolerance it cannot happen; if a tolerance is
	// configured wide enough for it, the round is a deviation all the same.
	for i, card := range snapshot.Cards {
		if shares[i] > card.FeesMinor() {
			result.HoldReason = HoldDeviationAboveTolerance
			return result, nil
		}
	}

	result.Shares = shares
	result.Weights = weights
	return result, nil
}

// CardPlatformShareWeights are the weights a seller's computed share is split by: each card's
// ExpectedCardPlatformShareMinor.
//
// Should any of them not be positive — a card rate far from the one the formula assumes, on
// a very large charge — the priced platform fees are used for every card instead. Those are
// positive for every candidate, which carries a platform fee by definition.
func CardPlatformShareWeights(cards []CardFeeCandidate) []int64 {
	weights := make([]int64, len(cards))
	formula := true
	for i, card := range cards {
		weights[i] = ExpectedCardPlatformShareMinor(card.TotalCharged, card.SellerNetAmount)
		if weights[i] <= 0 {
			formula = false
		}
	}
	if formula {
		return weights
	}
	for i, card := range cards {
		weights[i] = card.PricedPlatformFeeMinor()
	}
	return weights
}

// AllocateProportionally splits total over weights in proportion, to the sen.
//
// Each part is first floor(total × weight / Σweights). The sen that floor leaves — fewer
// than there are parts — go one each to the parts with the largest remainder, ties to the
// earlier position, so the order is fixed and the parts always sum to total exactly. The
// products are computed without overflow.
func AllocateProportionally(total int64, weights []int64) ([]int64, error) {
	if len(weights) == 0 {
		return nil, errors.New("nothing to allocate to")
	}
	if total < 0 {
		return nil, errors.New("cannot allocate a negative amount")
	}

	sum := new(big.Int)
	for _, w := range weights {
		if w <= 0 {
			return nil, errors.New("every weight must be positive")
		}
		sum.Add(sum, big.NewInt(w))
	}

	parts := make([]int64, len(weights))
	remainders := make([]*big.Int, len(weights))
	var allocated int64
	for i, w := range weights {
		quotient, remainder := new(big.Int).QuoRem(
			new(big.Int).Mul(big.NewInt(total), big.NewInt(w)), sum, new(big.Int))
		parts[i] = quotient.Int64()
		remainders[i] = remainder
		allocated += parts[i]
	}

	order := make([]int, len(weights))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return remainders[order[a]].Cmp(remainders[order[b]]) > 0
	})
	for k := int64(0); k < total-allocated; k++ {
		parts[order[k]]++
	}

	return parts, nil
}

// SplitPlatformFeeMinor splits a settled platform fee into what a whole-rupiah ledger entry
// can carry and the residual sen: share = rupiah×100 + residual.
func SplitPlatformFeeMinor(shareMinor int64) (rupiah, residualMinor int64) {
	rupiah = shareMinor / MinorPerRupiah
	return rupiah, shareMinor - RupiahToMinor(rupiah)
}

// PlatformFeeReconciliation is one seller in one round (platform_fee_reconciliations). Rows
// are insert-only; every amount is sen.
type PlatformFeeReconciliation struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`

	SellerAccountID   string
	SingapayAccountID string

	// What the balance inquiry answered. nil when it was not read: it failed, or the round
	// was held before asking.
	SingapayAvailableMinor *int64
	SingapayPendingMinor   *int64
	SingapayHeldMinor      *int64
	// RawBalance is the answer as the client decoded it, JSON. nil when not read.
	RawBalance []byte

	// SellerLedgerAvailableMinor is the seller's AVAILABLE balance × 100.
	SellerLedgerAvailableMinor     int64
	CertainUnsweptPlatformFeeMinor int64

	// ComputedPlatformFeeMinor is available − seller − certain. nil when not computed.
	ComputedPlatformFeeMinor *int64
	NominalPlatformFeeMinor  int64
	ToleranceMinor           int64
	CardCount                int

	Outcome    PlatformFeeReconciliationOutcome
	HoldReason PlatformFeeReconciliationHoldReason // empty when APPLIED
}

// NewPlatformFeeReconciliation starts the row for one seller's round.
func NewPlatformFeeReconciliation(snapshot *CardFeeSnapshot, singapayAccountID string, tolerancePerCardMinor int64) *PlatformFeeReconciliation {
	if tolerancePerCardMinor <= 0 {
		tolerancePerCardMinor = DefaultCardFeeTolerancePerCardMinor
	}
	r := &PlatformFeeReconciliation{
		SellerAccountID:                snapshot.SellerAccountID,
		SingapayAccountID:              singapayAccountID,
		SellerLedgerAvailableMinor:     RupiahToMinor(snapshot.SellerLedgerAvailable),
		CertainUnsweptPlatformFeeMinor: snapshot.CertainUnsweptPlatformFeeMinor,
		NominalPlatformFeeMinor:        snapshot.NominalPlatformFeeMinor(),
		ToleranceMinor:                 tolerancePerCardMinor * int64(len(snapshot.Cards)),
		CardCount:                      len(snapshot.Cards),
	}
	redifu.InitRecord(r)

	// InitRecord allocates every nil pointer, and a pointer to 0 would read as a balance of
	// zero. These are NULL until set.
	r.SingapayAvailableMinor = nil
	r.SingapayPendingMinor = nil
	r.SingapayHeldMinor = nil
	r.ComputedPlatformFeeMinor = nil
	return r
}

// Hold marks the round HELD for a reason.
func (r *PlatformFeeReconciliation) Hold(reason PlatformFeeReconciliationHoldReason) {
	r.Outcome = PlatformFeeReconciliationHeld
	r.HoldReason = reason
}

// Apply marks the round APPLIED.
func (r *PlatformFeeReconciliation) Apply() {
	r.Outcome = PlatformFeeReconciliationApplied
	r.HoldReason = ""
}

// PlatformFeeReconciliationItem is one card in one round (platform_fee_reconciliation_items).
// On a HELD round only the card and its previous figures are recorded; every amount is sen.
type PlatformFeeReconciliationItem struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`

	ReconciliationUUID     string
	ProductTransactionUUID string

	PricedPlatformFeeMinor          int64
	PreviousSettledGatewayFeeMinor  int64
	PreviousSettledPlatformFeeMinor int64

	// The new figures, nil on a HELD round.
	AllocatedPlatformFeeMinor *int64
	SettledGatewayFeeMinor    *int64
	PlatformResidualMinor     *int64

	// JournalUUID is the RECONCILIATION journal, empty on a HELD round. LedgerEntryUUID is
	// the platform's FEE_ADJUSTMENT entry, empty when the whole-rupiah part did not change.
	JournalUUID     string
	LedgerEntryUUID string
}

// NewPlatformFeeReconciliationItem records one card of a round, with its figures before.
func NewPlatformFeeReconciliationItem(reconciliationUUID string, card CardFeeCandidate) *PlatformFeeReconciliationItem {
	item := &PlatformFeeReconciliationItem{
		ReconciliationUUID:              reconciliationUUID,
		ProductTransactionUUID:          card.ProductTransactionUUID,
		PricedPlatformFeeMinor:          card.PricedPlatformFeeMinor(),
		PreviousSettledGatewayFeeMinor:  card.PreviousGatewayFeeMinor(),
		PreviousSettledPlatformFeeMinor: card.PreviousPlatformFeeMinor(),
	}
	redifu.InitRecord(item)

	item.AllocatedPlatformFeeMinor = nil
	item.SettledGatewayFeeMinor = nil
	item.PlatformResidualMinor = nil
	return item
}

// PlatformFeeReconciliationRepository reads the reconciliation's inputs and stores its
// outcomes. The tables are insert-only.
type PlatformFeeReconciliationRepository interface {
	// SellersWithEstimatedCardFees returns the seller accounts that hold card candidates
	// (CardFeeCandidate), the one with the oldest such settlement first, at most limit.
	SellersWithEstimatedCardFees(ctx context.Context, limit int) ([]string, error)

	// ReadCardFeeSnapshot reads one seller's CardFeeSnapshot in a single statement.
	ReadCardFeeSnapshot(ctx context.Context, sellerAccountID string) (*CardFeeSnapshot, error)

	// Save inserts one round's row.
	Save(ctx context.Context, reconciliation *PlatformFeeReconciliation) error

	// SaveItems inserts the round's cards. A second APPLIED item for the same transaction
	// is refused by the database (uq_platform_fee_reconciliation_items_applied).
	SaveItems(ctx context.Context, items []*PlatformFeeReconciliationItem) error
}
