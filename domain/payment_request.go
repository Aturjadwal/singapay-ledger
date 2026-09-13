package domain

import (
	"github.com/21strive/redifu"

	"context"
	"time"
)

// PaymentRequest records the gateway payment instrument issued for a transaction.
//
// It carries no status of its own, deliberately. A payment request is created 1:1 with its
// ProductTransaction, in the same database transaction, and never independently — so
// "has this been paid?" is a question about the transaction, and product_transactions.status
// is where it is both asked and answered under a compare-and-set. A second copy here could
// only ever agree with that one or be wrong about it.
//
// What this row is for is identity: which instrument was issued, and which keys read the
// payment back from Singapay later.
type PaymentRequest struct {
	*redifu.Record         `json:",inline" bson:",inline" db:"-"`
	ProductTransactionUUID string
	RequestID              string // Singapay's own id for the payment instrument (VA ULID, QRIS/link id, e-wallet id)
	PaymentCode            string // VA number, QRIS code, etc.

	// GatewayTransactionID and GatewayTransactionRef identify the PAYMENT, not the
	// instrument, and are filled in from the money-in webhook rather than at creation.
	//
	// RequestID above is the instrument's id, which for VA and payment link is a
	// different entity from the transaction: a VA is a container and the payment that
	// arrives in it has its own business id; a payment link can carry several attempts,
	// each with its own. So neither can be used to read a settled transaction back.
	//
	// Two fields because the four detail endpoints disagree about which identifier they
	// take — the numeric id for QRIS, e-wallet and payment link, the business id for VA.
	// Both arrive in the same webhook, so storing both removes a per-channel guess from
	// the settlement read path. Empty on rows that predate the columns, which the reader
	// treats as "fall back to a per-channel lookup", not as an error.
	GatewayTransactionID  string // MoneyInTransaction.ID — numeric primary key
	GatewayTransactionRef string // MoneyInTransaction.TransactionID — business id
	PaymentChannel        string // Payment method (QRIS, VA_BCA, etc.)
	PaymentURL            string // URL for user to complete payment
	Amount                int64  // Total charged to buyer

	Currency Currency

	// ExpiresAt is the expiry the instrument was issued with. It is a record of what was
	// asked of Singapay, not a lifecycle this ledger drives: an instrument that lapses
	// lapses at the gateway, and the transaction it belongs to simply never leaves
	// PENDING. Nothing here sweeps on it.
	ExpiresAt time.Time
}

// PaymentRequestRepository defines data access for payment requests.
//
// The reads are keyed the way the settling pass needs them: by transaction, which is the
// only lookup the ledger performs today. GetByID, GetByRequestID and GetByPaymentCode are
// kept for operators tracing a single instrument from an identifier Singapay quoted.
type PaymentRequestRepository interface {
	GetByID(ctx context.Context, id string) (*PaymentRequest, error)
	GetByRequestID(ctx context.Context, requestID string) (*PaymentRequest, error)
	GetByPaymentCode(ctx context.Context, paymentCode string) (*PaymentRequest, error)
	GetByProductTransactionID(ctx context.Context, productTransactionID string) (*PaymentRequest, error)
	Save(ctx context.Context, pr *PaymentRequest) error
	Update(ctx context.Context, pr *PaymentRequest) error
}

// NewPaymentRequest creates a payment request for a freshly issued instrument.
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
		ExpiresAt:              expiresAt,
	}
	redifu.InitRecord(pr)

	return pr
}

// SetPaymentCode sets the payment code (VA number, QRIS code, etc.)
func (pr *PaymentRequest) SetPaymentCode(code string) {
	pr.PaymentCode = code
	pr.UpdatedAt = time.Now()
}

// SetGatewayTransaction records the gateway's identifiers for the payment itself.
//
// Called when the money-in webhook is booked, which is the first moment the transaction
// exists at Singapay for every channel. Both values are stored as sent; neither is
// validated, because an identifier we do not recognise is still the identifier Singapay
// will quote back in a dispute.
func (pr *PaymentRequest) SetGatewayTransaction(id, ref string) {
	if id != "" {
		pr.GatewayTransactionID = id
	}
	if ref != "" {
		pr.GatewayTransactionRef = ref
	}
	pr.UpdatedAt = time.Now()
}

// SetPaymentURL sets the URL for user to complete payment
func (pr *PaymentRequest) SetPaymentURL(url string) {
	pr.PaymentURL = url
	pr.UpdatedAt = time.Now()
}

// HasExpired reports whether the instrument's expiry has passed. It is a read over
// ExpiresAt for callers that want to explain why a transaction is still PENDING; it
// changes nothing.
func (pr *PaymentRequest) HasExpired() bool {
	return time.Now().After(pr.ExpiresAt)
}
