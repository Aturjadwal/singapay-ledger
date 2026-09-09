package domain

import (
	"github.com/21strive/redifu"

	"context"
	"time"

	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// PaymentStatus represents the lifecycle state of a payment request
type PaymentStatus string

const (
	PaymentStatusPending   PaymentStatus = "PENDING"
	PaymentStatusCompleted PaymentStatus = "COMPLETED"
	PaymentStatusFailed    PaymentStatus = "FAILED"
	PaymentStatusExpired   PaymentStatus = "EXPIRED"
)

// PaymentRequest tracks the gateway payment lifecycle for a transaction
type PaymentRequest struct {
	*redifu.Record         `json:",inline" bson:",inline" db:"-"`
	ProductTransactionUUID string
	RequestID              string // Singapay's own id for the payment instrument (VA ULID, QRIS/link id, e-wallet id)
	PaymentCode            string // VA number, QRIS code, etc.
	PaymentChannel         string // Payment method (QRIS, VA_BCA, etc.)
	PaymentURL             string // URL for user to complete payment
	Amount                 int64  // Total charged to buyer
	Currency               Currency
	Status                 PaymentStatus
	FailureReason          string
	CompletedAt            *time.Time // When the money-in webhook confirmed payment
	ExpiresAt              time.Time  // Payment link expiration
}

// PaymentRequestRepository defines data access for payment requests
type PaymentRequestRepository interface {
	GetByID(ctx context.Context, id string) (*PaymentRequest, error)
	GetByRequestID(ctx context.Context, requestID string) (*PaymentRequest, error)
	GetByPaymentCode(ctx context.Context, paymentCode string) (*PaymentRequest, error)
	GetByProductTransactionID(ctx context.Context, productTransactionID string) (*PaymentRequest, error)
	GetPendingExpired(ctx context.Context, before time.Time) ([]*PaymentRequest, error)
	Save(ctx context.Context, pr *PaymentRequest) error
	Update(ctx context.Context, pr *PaymentRequest) error
}

// NewPaymentRequest creates a new payment request in PENDING status
func NewPaymentRequest(
	productTransactionID string,
	requestID string,
	paymentChannel string,
	amount int64,
	currency Currency,
	expiresAt time.Time,
) *PaymentRequest {
	pr := &PaymentRequest{
		ProductTransactionUUID: productTransactionID,
		RequestID:              requestID,
		PaymentChannel:         paymentChannel,
		Amount:                 amount,
		Currency:               currency,
		Status:                 PaymentStatusPending,
		ExpiresAt:              expiresAt,
	}
	redifu.InitRecord(pr)

	// CRITICAL FIX: redifu.InitRecord initializes pointer fields to zero time instead of nil
	pr.CompletedAt = nil

	return pr
}

// SetPaymentCode sets the payment code (VA number, QRIS code, etc.)
func (pr *PaymentRequest) SetPaymentCode(code string) {
	pr.PaymentCode = code
	pr.UpdatedAt = time.Now()
}

// SetPaymentURL sets the URL for user to complete payment
func (pr *PaymentRequest) SetPaymentURL(url string) {
	pr.PaymentURL = url
	pr.UpdatedAt = time.Now()
}

// IsPending checks if payment is waiting for user to pay
func (pr *PaymentRequest) IsPending() bool {
	return pr.Status == PaymentStatusPending
}

// IsCompleted checks if payment has been received
func (pr *PaymentRequest) IsCompleted() bool {
	return pr.Status == PaymentStatusCompleted
}

// IsFailed checks if payment has failed
func (pr *PaymentRequest) IsFailed() bool {
	return pr.Status == PaymentStatusFailed
}

// IsExpired checks if payment link has expired
func (pr *PaymentRequest) IsExpired() bool {
	return pr.Status == PaymentStatusExpired
}

// IsTerminal checks if payment is in a terminal state (no more transitions)
func (pr *PaymentRequest) IsTerminal() bool {
	return pr.IsCompleted() || pr.IsFailed() || pr.IsExpired()
}

// HasExpired checks if the payment link has passed its expiration time
func (pr *PaymentRequest) HasExpired() bool {
	return time.Now().After(pr.ExpiresAt)
}

// CanTransitionTo validates if status transition is allowed
func (pr *PaymentRequest) CanTransitionTo(newStatus PaymentStatus) bool {
	switch pr.Status {
	case PaymentStatusPending:
		// PENDING can transition to COMPLETED, FAILED, or EXPIRED
		return newStatus == PaymentStatusCompleted ||
			newStatus == PaymentStatusFailed ||
			newStatus == PaymentStatusExpired
	case PaymentStatusCompleted, PaymentStatusFailed, PaymentStatusExpired:
		// Terminal states - no transitions allowed
		return false
	default:
		return false
	}
}

// MarkCompleted transitions from PENDING to COMPLETED (when the money-in webhook is booked)
func (pr *PaymentRequest) MarkCompleted() error {
	if !pr.CanTransitionTo(PaymentStatusCompleted) {
		return ledgererr.ErrInvalidPaymentStatus
	}
	now := time.Now()
	pr.Status = PaymentStatusCompleted
	pr.CompletedAt = &now
	pr.UpdatedAt = now
	return nil
}

// MarkFailed transitions from PENDING to FAILED
func (pr *PaymentRequest) MarkFailed(reason string) error {
	if !pr.CanTransitionTo(PaymentStatusFailed) {
		return ledgererr.ErrInvalidPaymentStatus
	}
	pr.Status = PaymentStatusFailed
	pr.FailureReason = reason
	pr.UpdatedAt = time.Now()
	return nil
}

// MarkExpired transitions from PENDING to EXPIRED
func (pr *PaymentRequest) MarkExpired() error {
	if !pr.CanTransitionTo(PaymentStatusExpired) {
		return ledgererr.ErrInvalidPaymentStatus
	}
	pr.Status = PaymentStatusExpired
	pr.UpdatedAt = time.Now()
	return nil
}

// CheckAndMarkExpired checks if payment has expired and marks it if so
func (pr *PaymentRequest) CheckAndMarkExpired() (bool, error) {
	if !pr.IsPending() {
		return false, nil
	}
	if !pr.HasExpired() {
		return false, nil
	}
	if err := pr.MarkExpired(); err != nil {
		return false, err
	}
	return true, nil
}

// GetRemainingTime returns duration until payment expires (negative if expired)
func (pr *PaymentRequest) GetRemainingTime() time.Duration {
	return time.Until(pr.ExpiresAt)
}
