package domain

import (
	"github.com/21strive/redifu"

	"context"
	"strconv"
	"strings"
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
	// Two fields because the channels disagree about which identifier their webhook
	// carries and which one reads the payment back. What each channel stores is decided
	// by singapay.MoneyInNotification.GatewayTransactionIdentifiers:
	//
	//	QRIS, e-wallet      ID  = transaction.id, the numeric id their detail endpoints take
	//	virtual account     Ref = transaction.transaction_id, the business id its detail
	//	                    endpoint takes
	//	payment link, card  Ref = transaction.reff_no, the reference of the attempt that
	//	                    paid: payment_link_histories.reff_no
	//
	// VA and payment-link webhooks carry no numeric id, so the webhook leaves ID empty for
	// them. A payment link's is filled in later, by settlement: once it has found the
	// attempt that paid, it stores the attempt's payment_link_histories.id here (and its
	// reff_no in Ref, if that was empty) through RecordGatewayTransaction. ID is never "0"
	// (see SetGatewayTransaction), but VA and payment-link rows booked before that rule
	// may hold "0" there, which the settlement reader treats as absent. Empty on rows that
	// predate the columns, which the reader treats as "fall back to a per-channel lookup",
	// not as an error.
	GatewayTransactionID  string // QRIS/e-wallet: MoneyInTransaction.ID; payment link/card: the attempt's history id, once settlement finds it; else empty
	GatewayTransactionRef string // VA: MoneyInTransaction.TransactionID; payment link / card: MoneyInTransaction.ReffNo
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

	// RecordGatewayTransaction fills in gateway identifiers the ledger found for itself —
	// settlement locating a payment-link attempt by searching for it — and reports whether
	// the row changed. It only fills what is missing: an id is written over NULL, an empty
	// string or a value that is not a positive integer (the "0" a payment-link webhook
	// used to leave), a ref only over NULL or an empty string, and an empty argument
	// writes nothing. A valid identifier is never replaced, and an unknown id changes
	// nothing and is not an error.
	RecordGatewayTransaction(ctx context.Context, paymentRequestID, id, ref string) (bool, error)
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
// exists at Singapay for every channel. An empty value leaves its field as it was.
//
// So does an id that reads as a whole number of zero or less. No Singapay payment has one,
// and storing it is how a payment link came to be read back as history 0: its webhook
// carries no numeric id, the caller formatted the missing value as "0", and every
// settlement lookup asked for history 0 and got a 404. Refusing it here keeps it out
// whichever caller supplies it. Anything else is stored as given, apart from surrounding
// spaces, and is not otherwise validated — an identifier we do not recognise is still the
// identifier Singapay will quote back in a dispute.
func (pr *PaymentRequest) SetGatewayTransaction(id, ref string) {
	if id = strings.TrimSpace(id); id != "" && !isNonPositiveInteger(id) {
		pr.GatewayTransactionID = id
	}
	if ref = strings.TrimSpace(ref); ref != "" {
		pr.GatewayTransactionRef = ref
	}
	pr.UpdatedAt = time.Now()
}

// isNonPositiveInteger reports whether s reads as a whole number of zero or less. A value
// that is not a number at all — a business id — is not one.
func isNonPositiveInteger(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n <= 0
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
