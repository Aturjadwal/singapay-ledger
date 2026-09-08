package singapay

import (
	"errors"
	"fmt"
	"net/http"
)

// ResponseCode is a Singapay SP-code. It describes what happened to the *request*, not to
// the payment. SP000 on a disbursement means Singapay accepted the instruction; the money
// may not have moved yet, and may never.
type ResponseCode string

const (
	CodeSuccess ResponseCode = "SP000"

	CodeTransactionFailure ResponseCode = "SP001"
	CodeGeneralFailure     ResponseCode = "SP002"
	CodeInsufficientFunds  ResponseCode = "SP003"
	CodeDuplicateReference ResponseCode = "SP004"
	CodeTimeout            ResponseCode = "SP005"
	CodeBeneficiaryLimit   ResponseCode = "SP006"
	CodeAccountLimit       ResponseCode = "SP007"

	CodeInvalidReference    ResponseCode = "SP008"
	CodeTransactionNotFound ResponseCode = "SP009"
	CodeBeneficiaryNotFound ResponseCode = "SP010"
	CodeVendorNotActive     ResponseCode = "SP011"
	CodeMerchantNotFound    ResponseCode = "SP020"

	CodeBadRequest       ResponseCode = "SP012"
	CodeUnauthorized     ResponseCode = "SP013"
	CodeNotFound         ResponseCode = "SP014"
	CodeForbidden        ResponseCode = "SP015"
	CodeSignatureInvalid ResponseCode = "SP016"
	CodeUnauthorizedIP   ResponseCode = "SP017"
	CodeValidationError  ResponseCode = "SP018"
	CodeGeneralError     ResponseCode = "SP019"
)

// Outcome says what a failed money-out call implies about the money itself.
//
// This is the distinction that decides whether a reserved balance may be released. The
// obvious approach is an HTTP heuristic — 4xx means a definite refusal, 5xx means unknown
// — and that heuristic is wrong here. Singapay answers HTTP 400 for SP001, SP002,
// SP004 and SP005, and its own documentation says to call inquiry-status for every one of
// them because the transfer may still settle. Releasing the reservation on those is
// exactly how a payout gets made twice.
type Outcome int

const (
	// OutcomeUnknown means the transfer may still have happened. Keep the funds
	// reserved and resolve with an inquiry. It is the zero value on purpose: an
	// unrecognised code, a timeout, a dropped connection and a 502 all land here.
	OutcomeUnknown Outcome = iota

	// OutcomeRefused means Singapay declined before moving anything. Safe to release.
	OutcomeRefused

	// OutcomeDuplicate means a transaction with this reference already exists. The
	// original may well have succeeded — inquire on the reference, never re-send.
	OutcomeDuplicate
)

func (o Outcome) String() string {
	switch o {
	case OutcomeRefused:
		return "refused"
	case OutcomeDuplicate:
		return "duplicate"
	default:
		return "unknown"
	}
}

// outcomes maps each documented SP-code to what it implies about the money.
//
// Anything absent is OutcomeUnknown by omission, which is the conservative answer and the
// one that must survive Singapay adding a code we have never seen.
var outcomes = map[ResponseCode]Outcome{
	// "Call the inquiry-status endpoint to confirm the final state." — the transfer
	// may still settle, so these must not release anything.
	CodeTransactionFailure: OutcomeUnknown,
	CodeGeneralFailure:     OutcomeUnknown,
	CodeTimeout:            OutcomeUnknown,
	CodeGeneralError:       OutcomeUnknown,

	CodeDuplicateReference: OutcomeDuplicate,

	// Declined before anything moved.
	CodeInsufficientFunds:   OutcomeRefused,
	CodeBeneficiaryLimit:    OutcomeRefused,
	CodeAccountLimit:        OutcomeRefused,
	CodeInvalidReference:    OutcomeRefused,
	CodeTransactionNotFound: OutcomeRefused,
	CodeBeneficiaryNotFound: OutcomeRefused,
	CodeVendorNotActive:     OutcomeRefused,
	CodeMerchantNotFound:    OutcomeRefused,
	CodeBadRequest:          OutcomeRefused,
	CodeUnauthorized:        OutcomeRefused,
	CodeNotFound:            OutcomeRefused,
	CodeForbidden:           OutcomeRefused,
	CodeSignatureInvalid:    OutcomeRefused,
	CodeUnauthorizedIP:      OutcomeRefused,
	CodeValidationError:     OutcomeRefused,
}

// Error is a failed Singapay call.
type Error struct {
	// StatusCode is the HTTP status. Present even when Singapay sent no SP-code.
	StatusCode int
	// Code is the SP-code, empty for v1.0 endpoints and for transport failures.
	Code ResponseCode
	// Message is Singapay's own text.
	Message string
	// Fields carries per-field validation detail, sent with SP018.
	Fields map[string]any
	// Body is the raw response, kept because a payout Singapay refuses is argued over
	// what it actually sent back.
	Body []byte
	// Err is the underlying transport error, if the call never got an answer.
	Err error
}

func (e *Error) Error() string {
	switch {
	case e.Err != nil && e.Code == "":
		return fmt.Sprintf("singapay: %v", e.Err)
	case e.Code == "":
		return fmt.Sprintf("singapay: http %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("singapay: %s (%s): %s", e.Code, e.Message, http.StatusText(e.StatusCode))
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Outcome reports what this failure implies about the money. See [Outcome].
//
// A transport failure — no answer at all — is OutcomeUnknown, which is the whole point:
// the request may have been received and acted on.
func (e *Error) Outcome() Outcome {
	if e == nil {
		return OutcomeRefused
	}
	if e.Code == "" {
		// No SP-code. A 4xx without one is a request Singapay rejected at the edge
		// (bad JSON, missing header) and never routed; anything else is unknown.
		if e.StatusCode >= 400 && e.StatusCode < 500 && e.Err == nil {
			return OutcomeRefused
		}
		return OutcomeUnknown
	}
	return outcomes[e.Code]
}

// IsDuplicate reports whether the call failed because the reference was already used.
func (e *Error) IsDuplicate() bool { return e.Outcome() == OutcomeDuplicate }

// AsError extracts a *Error from an error chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// TransactionStatus is the state of the payment itself, distinct from the SP-code that
// describes the request. Values are Singapay's two-digit codes.
type TransactionStatus string

const (
	StatusSuccess   TransactionStatus = "00"
	StatusInitiated TransactionStatus = "01"
	StatusPaying    TransactionStatus = "02"
	StatusPending   TransactionStatus = "03"
	StatusRefunded  TransactionStatus = "04"
	StatusCanceled  TransactionStatus = "05"
	StatusFailed    TransactionStatus = "06"
	StatusNotFound  TransactionStatus = "07"
)

// Terminal reports whether the status can still change.
//
// 04, 05 and 07 are terminal failures, and that matters: treating them as still-in-flight
// leaves a seller's balance reserved against a transfer that will never complete.
func (s TransactionStatus) Terminal() bool {
	switch s {
	case StatusSuccess, StatusRefunded, StatusCanceled, StatusFailed, StatusNotFound:
		return true
	default:
		return false
	}
}

// Succeeded reports whether funds have moved.
func (s TransactionStatus) Succeeded() bool { return s == StatusSuccess }

// Failed reports whether the status is terminal and unsuccessful.
func (s TransactionStatus) Failed() bool { return s.Terminal() && s != StatusSuccess }

func (s TransactionStatus) String() string {
	switch s {
	case StatusSuccess:
		return "Success"
	case StatusInitiated:
		return "Initiated"
	case StatusPaying:
		return "Paying"
	case StatusPending:
		return "Pending"
	case StatusRefunded:
		return "Refunded"
	case StatusCanceled:
		return "Canceled"
	case StatusFailed:
		return "Failed"
	case StatusNotFound:
		return "Not Found"
	default:
		return "Unknown(" + string(s) + ")"
	}
}
