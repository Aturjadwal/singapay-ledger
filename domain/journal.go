package domain

import (
	"context"

	"github.com/21strive/redifu"
)

// EventType represents the type of financial event a journal records.
// Each event type groups related ledger entries into a coherent business transaction.
type EventType string

const (
	EventTypePaymentSuccess   EventType = "PAYMENT_SUCCESS"   // Payment webhook received
	EventTypeSettlement       EventType = "SETTLEMENT"        // Funds settled: PENDING becomes AVAILABLE
	EventTypeDisbursement     EventType = "DISBURSEMENT"      // Withdrawal to bank account
	EventTypeReconciliation   EventType = "RECONCILIATION"    // Balance adjustment/correction
	EventTypeManualAdjustment EventType = "MANUAL_ADJUSTMENT" // Manual admin correction
)

// Journal represents an atomic accounting event.
// It groups one or more ledger_entries that together describe a complete financial transaction.
// Examples:
// - Payment Success: Groups entries for seller pending, platform commission, etc.
// - Settlement: Groups entries that clear pending and add to available balance
// - Disbursement: Groups entries for withdrawal debit
// - Reconciliation: Groups adjustment entries
type Journal struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`
	EventType      EventType
	SourceType     SourceType // Reuse from ledger_entry (what business entity triggered this)
	SourceID       string     // UUID of the triggering entity
	Metadata       map[string]any
}

// JournalRepository defines data access for journals.
type JournalRepository interface {
	// Save inserts a new journal. Returns an error if the ID already exists.
	Save(ctx context.Context, journal *Journal) error

	// GetByID returns a journal by its UUID.
	GetByID(ctx context.Context, id string) (*Journal, error)

	// GetBySourceID returns all journals for a given source entity.
	GetBySourceID(ctx context.Context, sourceType SourceType, sourceID string) ([]*Journal, error)

	// GetByEventType returns journals of a specific event type (paginated).
	GetByEventType(ctx context.Context, eventType EventType, limit, offset int) ([]*Journal, error)
}

// NewJournal creates a new journal for an accounting event.
func NewJournal(eventType EventType, sourceType SourceType, sourceID string, metadata map[string]any) *Journal {
	journal := &Journal{
		EventType:  eventType,
		SourceType: sourceType,
		SourceID:   sourceID,
		Metadata:   metadata,
	}
	redifu.InitRecord(journal)
	return journal
}
