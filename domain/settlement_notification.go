package domain

import (
	"context"
	"time"

	"github.com/21strive/redifu"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// SettlementNotificationStatus is where a stored settlement webhook is in its life.
type SettlementNotificationStatus string

const (
	// SettlementNotificationPending is accepted and not yet acted on: the worker's input.
	SettlementNotificationPending SettlementNotificationStatus = "PENDING"
	// SettlementNotificationProcessing is claimed by a worker. It stops a second worker
	// starting the same pass; it is not a claim that the pass will succeed.
	SettlementNotificationProcessing SettlementNotificationStatus = "PROCESSING"
	// SettlementNotificationProcessed means a settling pass ran after seeing this
	// notification. It does NOT mean every transaction in the window settled — that fact
	// lives on each transaction. Conflating the two is how a partial pass gets recorded
	// as a complete one.
	SettlementNotificationProcessed SettlementNotificationStatus = "PROCESSED"
	// SettlementNotificationNeedsReview is for deliveries a person has to decide on.
	// Refund events land here: they can pull back funds that are already AVAILABLE and
	// may already have been withdrawn, and this ledger has no negative-balance policy.
	SettlementNotificationNeedsReview SettlementNotificationStatus = "NEEDS_REVIEW"
	// SettlementNotificationFailed is a pass that errored. Retried on a later tick.
	SettlementNotificationFailed SettlementNotificationStatus = "FAILED"
)

// SettlementNotification is one settlement webhook, stored as it arrived.
//
// It is an inbox entry, not a reconciliation record. Singapay's settlement webhook carries
// totals and a date window and no list of the transactions the batch covered, so nothing
// here is sufficient to book a ledger entry. What it does is tell the settling pass that
// something changed and is worth looking at — and preserve, verbatim, what Singapay
// actually said, which is the only useful artefact when a settled amount is disputed later.
//
// The window (StartDate/EndDate) is stored but deliberately not used to select rows.
// Singapay writes those timestamps as text with no offset, so their true boundaries are an
// assumption; the per-transaction settling path never needs them.
type SettlementNotification struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`

	// SettlementID is Singapay's own id for the batch, and half of this row's identity.
	SettlementID string
	// SettlementReference is reference_no — the label a support conversation will quote.
	SettlementReference string

	// Event is settlement.completed, settlement.refunded or settlement.refund_cancelled.
	// It is the other half of the identity: one settlement legitimately produces more
	// than one event, and all of them must be stored.
	Event string

	// Method is balance, auto-balance, bank-account or e-wallet. Only the first two turn
	// PENDING into AVAILABLE; bank-account pays out to a nominated bank account instead.
	Method string
	// Type scopes the batch by product (ALL, VA, QRIS, EWALLET) — not by account.
	Type string

	// StartDate and EndDate are the announced window. Audit only; see the type comment.
	StartDate *time.Time
	EndDate   *time.Time

	// Totals as announced. Reported, never trusted as the basis for an entry.
	TotalTransactions int
	Amount            int64
	TotalFee          int64
	Currency          string

	// RawPayload is the verified delivery exactly as it arrived.
	RawPayload []byte

	Status        SettlementNotificationStatus
	FailureReason string

	ReceivedAt  time.Time
	ProcessedAt *time.Time
}

// SettlementNotificationRepository is data access for the settlement inbox.
type SettlementNotificationRepository interface {
	// Save stores a delivery. A repeat delivery of the same (SettlementID, Event) is a
	// success that stores nothing and reports stored=false — webhook redelivery is
	// ordinary traffic, and the caller must still answer 200.
	Save(ctx context.Context, n *SettlementNotification) (stored bool, err error)

	GetByID(ctx context.Context, id string) (*SettlementNotification, error)
	GetByIdentity(ctx context.Context, settlementID, event string) (*SettlementNotification, error)

	// GetActionable returns notifications waiting for a settling pass, oldest first:
	// PENDING and FAILED, never NEEDS_REVIEW.
	GetActionable(ctx context.Context, limit int) ([]*SettlementNotification, error)

	// CountActionable answers "is there anything to do?" without loading rows. It is the
	// first thing the worker asks on every tick.
	CountActionable(ctx context.Context) (int, error)

	// ClaimIfActionable moves a row to PROCESSING only if it is still actionable, and
	// reports whether this caller got it. Conditional for the same reason
	// UpdateStatusIf is: two workers on one row is a race, not a rarity.
	ClaimIfActionable(ctx context.Context, id string) (bool, error)

	MarkProcessed(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id string, reason string) error
	MarkNeedsReview(ctx context.Context, id string, reason string) error
}

// NewSettlementNotification builds an inbox entry from a verified delivery.
//
// It requires the identity fields and the raw payload, and nothing else. Every other field
// is whatever Singapay happened to send: a delivery that is missing totals is still a
// delivery, and refusing it here would lose the record of what arrived.
func NewSettlementNotification(settlementID, reference, event string, raw []byte) (*SettlementNotification, error) {
	if settlementID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "settlement_id is required", nil)
	}
	if event == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "event is required", nil)
	}
	if len(raw) == 0 {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "raw payload is required", nil)
	}

	n := &SettlementNotification{
		SettlementID:        settlementID,
		SettlementReference: reference,
		Event:               event,
		RawPayload:          raw,
		Status:              SettlementNotificationPending,
		ReceivedAt:          time.Now(),
	}
	redifu.InitRecord(n)

	// redifu.InitRecord fills pointer fields with a zero time rather than leaving them
	// nil. Same correction as NewPaymentRequest makes.
	n.ProcessedAt = nil
	n.StartDate = nil
	n.EndDate = nil

	return n, nil
}

// IsActionable reports whether a settling pass should pick this up.
func (n *SettlementNotification) IsActionable() bool {
	return n.Status == SettlementNotificationPending || n.Status == SettlementNotificationFailed
}

// MovesPendingToAvailable reports whether this batch converted pending balance into
// available balance, as opposed to paying out to a bank account. Only those batches have
// anything to do with this ledger's PENDING -> AVAILABLE step.
func (n *SettlementNotification) MovesPendingToAvailable() bool {
	return n.Method == "balance" || n.Method == "auto-balance"
}
