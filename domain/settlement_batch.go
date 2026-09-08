package domain

import (
	"github.com/21strive/redifu"

	"context"
	"time"

	"github.com/21strive/ledger/ledgererr"
)

// SettlementBatchStatus represents the processing state of a settlement batch
type SettlementBatchStatus string

const (
	SettlementBatchStatusPending    SettlementBatchStatus = "PENDING"
	SettlementBatchStatusProcessing SettlementBatchStatus = "PROCESSING"
	SettlementBatchStatusCompleted  SettlementBatchStatus = "COMPLETED"
	SettlementBatchStatusFailed     SettlementBatchStatus = "FAILED"
)

// SettlementBatch is one Singapay settlement, and the record of what this ledger did
// about it.
//
// Singapay announces a settlement on settlement_notif_url with totals and a date window,
// and nothing else: there is no list of the transactions it covered. SettleFrom and
// SettleTo are therefore not decoration — they are the only handle on which rows the
// batch contained, and the reconciler replays them against the per-product transaction
// lists to find out. A batch with no window can never be reprocessed or audited, so both
// are required.
type SettlementBatch struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`
	LedgerUUID     string

	// BatchID is Singapay's own identifier for the settlement. It is the idempotency
	// key: a batch_id already present has been booked, and the ledger entries behind it
	// are immutable.
	BatchID string

	// SettlementReference is Singapay's reference_no — the human-facing label a support
	// conversation will quote. It is not unique enough to key on; BatchID is.
	SettlementReference string

	// SettleFrom and SettleTo bound the window Singapay says this batch covered. The
	// transaction lists are filtered on exactly this range to reconstruct the rows.
	SettleFrom time.Time
	SettleTo   time.Time

	SettlementDate time.Time
	GrossAmount    int64 // Total charged across all matched transactions
	NetAmount      int64 // Total credited after gateway fees
	GatewayFee     int64 // Total channel fees Singapay took
	Currency       Currency

	// InitiatedBy names who caused this reconciliation to run: the webhook that
	// delivered the settlement, or the operator who replayed it by hand.
	InitiatedBy string
	InitiatedAt time.Time

	ProcessedAt      *time.Time
	ProcessingStatus SettlementBatchStatus
	MatchedCount     int    // Number of settled rows matched to a transaction
	UnmatchedCount   int    // Number of settled rows that could not be booked
	FailureReason    string // Reason if processing failed
	Metadata         map[string]any
}

// SettlementBatchRepository defines data access for settlement batches
type SettlementBatchRepository interface {
	GetByID(ctx context.Context, id string) (*SettlementBatch, error)
	GetByLedgerID(ctx context.Context, ledgerID string, page, pageSize int) ([]*SettlementBatch, error)
	GetByLedgerIDAndDate(ctx context.Context, ledgerID string, settlementDate time.Time) (*SettlementBatch, error)

	// GetByBatchID looks up a batch by Singapay's own identifier for it, taken from the
	// settlement webhook. This is the idempotency lookup. Returns ErrNotFound when
	// nothing matches.
	GetByBatchID(ctx context.Context, batchID string) (*SettlementBatch, error)

	// FilterIngestedBatchIDs returns the subset of batchIDs that already have a batch
	// row. A caller replaying a range of settlements — after an outage that dropped
	// webhooks, say — diffs its list against this and processes the difference. An
	// empty input returns an empty set without querying.
	FilterIngestedBatchIDs(ctx context.Context, batchIDs []string) (map[string]struct{}, error)

	Save(ctx context.Context, batch *SettlementBatch) error
	UpdateStatus(ctx context.Context, id string, status SettlementBatchStatus, processedAt *time.Time, failureReason string) error
}

// NewSettlementBatch creates a new settlement batch in PENDING status.
func NewSettlementBatch(
	ledgerID string,
	batchID string,
	settlementReference string,
	settleFrom, settleTo time.Time,
	settlementDate time.Time,
	initiatedBy string,
	currency Currency,
) (*SettlementBatch, error) {
	if ledgerID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "ledger_id is required", nil)
	}
	if batchID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "batch_id is required", nil)
	}
	if initiatedBy == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "initiated_by is required", nil)
	}
	// Without a window there is nothing to query the gateway for, so a batch that
	// carries none is not a batch that can be reconciled later — it is a row that will
	// permanently claim a settlement was handled when it was not.
	if settleFrom.IsZero() || settleTo.IsZero() {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "settle_from and settle_to are required", nil)
	}
	if settleTo.Before(settleFrom) {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "settle_to must not precede settle_from", nil)
	}

	sb := &SettlementBatch{
		LedgerUUID:          ledgerID,
		BatchID:             batchID,
		SettlementReference: settlementReference,
		SettleFrom:          settleFrom,
		SettleTo:            settleTo,
		SettlementDate:      settlementDate,
		GrossAmount:         0,
		NetAmount:           0,
		GatewayFee:          0,
		Currency:            currency,
		InitiatedBy:         initiatedBy,
		InitiatedAt:         time.Now(),
		ProcessingStatus:    SettlementBatchStatusPending,
		MatchedCount:        0,
		UnmatchedCount:      0,
		Metadata:            make(map[string]any),
	}
	redifu.InitRecord(sb)

	// CRITICAL FIX: redifu.InitRecord initializes pointer fields to zero time instead of nil
	sb.ProcessedAt = nil

	return sb, nil
}

// IsPending checks if batch is waiting to be processed
func (sb *SettlementBatch) IsPending() bool {
	return sb.ProcessingStatus == SettlementBatchStatusPending
}

// IsProcessing checks if batch is currently being processed
func (sb *SettlementBatch) IsProcessing() bool {
	return sb.ProcessingStatus == SettlementBatchStatusProcessing
}

// IsCompleted checks if batch has been processed successfully
func (sb *SettlementBatch) IsCompleted() bool {
	return sb.ProcessingStatus == SettlementBatchStatusCompleted
}

// IsFailed checks if batch processing failed
func (sb *SettlementBatch) IsFailed() bool {
	return sb.ProcessingStatus == SettlementBatchStatusFailed
}

// MarkProcessing transitions to PROCESSING status
func (sb *SettlementBatch) MarkProcessing() error {
	if sb.ProcessingStatus != SettlementBatchStatusPending {
		return ledgererr.ErrInvalidSettlementBatchStatus
	}
	sb.ProcessingStatus = SettlementBatchStatusProcessing
	return nil
}

// MarkCompleted transitions to COMPLETED status with totals
func (sb *SettlementBatch) MarkCompleted(grossAmount, netAmount, gatewayFee int64, matchedCount, unmatchedCount int) error {
	if sb.ProcessingStatus != SettlementBatchStatusProcessing {
		return ledgererr.ErrInvalidSettlementBatchStatus
	}
	now := time.Now()
	sb.ProcessingStatus = SettlementBatchStatusCompleted
	sb.GrossAmount = grossAmount
	sb.NetAmount = netAmount
	sb.GatewayFee = gatewayFee
	sb.MatchedCount = matchedCount
	sb.UnmatchedCount = unmatchedCount
	sb.ProcessedAt = &now
	return nil
}

// MarkFailed transitions to FAILED status with a reason
func (sb *SettlementBatch) MarkFailed(reason string) error {
	if sb.ProcessingStatus != SettlementBatchStatusPending && sb.ProcessingStatus != SettlementBatchStatusProcessing {
		return ledgererr.ErrInvalidSettlementBatchStatus
	}
	now := time.Now()
	sb.ProcessingStatus = SettlementBatchStatusFailed
	sb.FailureReason = reason
	sb.ProcessedAt = &now
	return nil
}

// GetMatchRate returns the percentage of matched transactions
func (sb *SettlementBatch) GetMatchRate() float64 {
	total := sb.MatchedCount + sb.UnmatchedCount
	if total == 0 {
		return 0
	}
	return float64(sb.MatchedCount) / float64(total) * 100
}

// AddToTotals accumulates amounts from a settlement item
func (sb *SettlementBatch) AddToTotals(amount, fee int64) {
	sb.GrossAmount += amount
	sb.GatewayFee += fee
	sb.NetAmount += (amount - fee)
}

// IncrementMatched increments the matched count
func (sb *SettlementBatch) IncrementMatched() {
	sb.MatchedCount++
}

// IncrementUnmatched increments the unmatched count
func (sb *SettlementBatch) IncrementUnmatched() {
	sb.UnmatchedCount++
}
