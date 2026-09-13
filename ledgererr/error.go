package ledgererr

import (
	"errors"
	"fmt"
)

type ErrorCode int

type AppError struct {
	Code        ErrorCode
	Msg         string
	OriginError error
}

func NewError(code ErrorCode, msg string, origin error) AppError {
	return AppError{
		Code:        code,
		Msg:         msg,
		OriginError: origin,
	}
}

func (err *AppError) WithError(origin error) AppError {
	return AppError{
		Code:        err.Code,
		Msg:         err.Msg,
		OriginError: origin,
	}
}

func (err AppError) Error() string {
	if err.OriginError != nil {
		return fmt.Sprintf("%s: %s", err.Msg, err.OriginError.Error())
	}
	return err.Msg
}

func (err AppError) ErrCode() ErrorCode {
	var originErr AppError
	if errors.As(err.OriginError, &originErr) {
		return originErr.ErrCode()
	}
	return err.Code
}

func (err AppError) Unwrap() error {
	return err.OriginError
}

func IsAppError(target error, err AppError) bool {
	var appErr AppError
	if errors.As(target, &appErr) {
		return appErr.ErrCode() == err.ErrCode()
	}
	return false
}

func IsErrorCode(code ErrorCode, err error) bool {
	var appErr AppError
	if errors.As(err, &appErr) {
		return appErr.ErrCode() == code
	}
	return false
}

const (
	CodeInternal       ErrorCode = 500
	CodeNotFound       ErrorCode = 404
	CodeInvalidRequest ErrorCode = 400

	CodeDatabaseError ErrorCode = 500001

	// CodeGatewayAPIError covers every failed call to Singapay. It stays a 500-class
	// code even when Singapay answered 4xx: an SP001 or SP004 on a payout is not the
	// caller's fault and must not be surfaced to them as a bad request.
	CodeGatewayAPIError ErrorCode = 500002

	// CodeGatewayOutcomeUnknown is the one that decides money. It means a money-out
	// call failed without telling us whether the money moved — a timeout, a dropped
	// connection, or one of the SP-codes Singapay documents as "call inquiry-status".
	// A caller must not treat it as a failure and must not retry it blind; the reserved
	// balance stays held until an inquiry settles it.
	CodeGatewayOutcomeUnknown ErrorCode = 500004

	// CodeWebhookVerificationFailed means an inbound webhook's HMAC did not match, or
	// its timestamp was outside the replay window. Distinct from CodeInvalidRequest so
	// a handler can answer 401 rather than 400 and so forged deliveries are countable.
	CodeWebhookVerificationFailed ErrorCode = 401001

	// CodeWebhookAmountMismatch is a verified money-in webhook reporting that the payer
	// was charged less than the transaction was priced at. It is a refusal to book, not
	// a signature problem: the delivery is genuine and the money is real, but crediting
	// the full price against a partial payment would put money in a seller's balance
	// that never arrived.
	CodeWebhookAmountMismatch ErrorCode = 409008

	CodeSubaccountAlreadyExists ErrorCode = 409001

	// Ledger error codes
	CodeLedgerNotFound                 ErrorCode = 404001
	CodeLedgerAlreadyExists            ErrorCode = 409002
	CodeReconciliationDiscrepancyFound ErrorCode = 409003

	// CodeReconciliationNotImplemented reports that settlement reconciliation has no
	// Singapay implementation yet. It is a state of the system, not a transient fault:
	// retrying never helps, and a caller must not treat it as a reason to hold a
	// webhook unacknowledged.
	CodeReconciliationNotImplemented ErrorCode = 501001

	// ProductTransaction error codes
	CodeProductTransactionNotFound      ErrorCode = 404002
	CodeProductTransactionAlreadyExists ErrorCode = 409004
	CodeInvalidTransactionStatus        ErrorCode = 400001
	CodeInvalidFeeBreakdown             ErrorCode = 400002

	// PaymentRequest error codes
	CodePaymentRequestNotFound      ErrorCode = 404003
	CodePaymentRequestAlreadyExists ErrorCode = 409005
	CodePaymentExpired              ErrorCode = 400004

	// FeeConfig error codes
	CodeFeeConfigNotFound         ErrorCode = 404004
	CodeUnsupportedPaymentChannel ErrorCode = 400005

	// Disbursement error codes
	CodeDisbursementNotFound      ErrorCode = 404005
	CodeDisbursementAlreadyExists ErrorCode = 409006
	CodeInvalidDisbursementStatus ErrorCode = 400006
	CodeInvalidDisbursementAmount ErrorCode = 400007
	CodeInvalidBankAccount        ErrorCode = 400008
	CodeInsufficientBalance       ErrorCode = 400009

	// Settlement error codes
	CodeSettlementBatchNotFound      ErrorCode = 404006
	CodeSettlementBatchAlreadyExists ErrorCode = 409007
	CodeInvalidSettlementBatchStatus ErrorCode = 400010
	CodeInvalidSettlementItem        ErrorCode = 400011
	CodeInvalidSettlementWindow      ErrorCode = 400012
	CodeSettlementItemNotFound       ErrorCode = 404007

	// Analytics error codes
	CodeAnalyticsDataNotFound ErrorCode = 404008
	CodeAnalyticsQueryError   ErrorCode = 500003
	CodeInvalidLimit          ErrorCode = 400013
	CodeInvalidOffset         ErrorCode = 400014
	CodeInvalidYear           ErrorCode = 400015
	CodeInvalidMonth          ErrorCode = 400016
	CodeInvalidDateRange      ErrorCode = 400017
	CodeInvalidIntervalType   ErrorCode = 400018
	CodeInvalidAccountID      ErrorCode = 400019
	CodeStartDateRequired     ErrorCode = 400020
	CodeEndDateRequired       ErrorCode = 400021
)

// Ledger errors
var (
	ErrLedgerNotFound                 = NewError(CodeLedgerNotFound, "ledger not found", nil)
	ErrLedgerAlreadyExists            = NewError(CodeLedgerAlreadyExists, "ledger already exists", nil)
	ErrReconciliationDiscrepancyFound = NewError(CodeReconciliationDiscrepancyFound, "reconciliation discrepancy found", nil)
)

// ProductTransaction errors
var (
	ErrProductTransactionNotFound      = NewError(CodeProductTransactionNotFound, "product transaction not found", nil)
	ErrProductTransactionAlreadyExists = NewError(CodeProductTransactionAlreadyExists, "product transaction already exists", nil)
	ErrInvalidTransactionStatus        = NewError(CodeInvalidTransactionStatus, "invalid transaction status transition", nil)
	ErrInvalidFeeBreakdown             = NewError(CodeInvalidFeeBreakdown, "invalid fee breakdown", nil)
)

// PaymentRequest errors
var (
	ErrPaymentRequestNotFound      = NewError(CodePaymentRequestNotFound, "payment request not found", nil)
	ErrPaymentRequestAlreadyExists = NewError(CodePaymentRequestAlreadyExists, "payment request already exists", nil)
	ErrPaymentExpired              = NewError(CodePaymentExpired, "payment request has expired", nil)
)

// FeeConfig errors
var (
	ErrFeeConfigNotFound         = NewError(CodeFeeConfigNotFound, "fee config not found", nil)
	ErrUnsupportedPaymentChannel = NewError(CodeUnsupportedPaymentChannel, "unsupported payment channel", nil)
)

// Disbursement errors
var (
	ErrDisbursementNotFound      = NewError(CodeDisbursementNotFound, "disbursement not found", nil)
	ErrDisbursementAlreadyExists = NewError(CodeDisbursementAlreadyExists, "disbursement already exists", nil)
	ErrInvalidDisbursementStatus = NewError(CodeInvalidDisbursementStatus, "invalid disbursement status transition", nil)
	ErrInvalidDisbursementAmount = NewError(CodeInvalidDisbursementAmount, "disbursement amount must be positive", nil)
	ErrInvalidBankAccount        = NewError(CodeInvalidBankAccount, "invalid bank account information", nil)
	ErrInsufficientBalance       = NewError(CodeInsufficientBalance, "insufficient balance for disbursement", nil)
)

// Settlement errors
var (
	ErrSettlementBatchNotFound      = NewError(CodeSettlementBatchNotFound, "settlement batch not found", nil)
	ErrSettlementBatchAlreadyExists = NewError(CodeSettlementBatchAlreadyExists, "settlement batch already exists for this date", nil)
	ErrInvalidSettlementBatchStatus = NewError(CodeInvalidSettlementBatchStatus, "invalid settlement batch status transition", nil)
	ErrInvalidSettlementItem        = NewError(CodeInvalidSettlementItem, "invalid settlement item data", nil)
	ErrInvalidSettlementWindow      = NewError(CodeInvalidSettlementWindow, "invalid settlement window", nil)
	ErrSettlementItemNotFound       = NewError(CodeSettlementItemNotFound, "settlement item not found", nil)

	ErrInvalidRequest = NewError(CodeInvalidRequest, "invalid request", nil)
)

// Webhook errors
var (
	// ErrWebhookAmountMismatch refuses a verified money-in webhook whose charged amount
	// falls short of what the transaction was priced at. Nothing is booked and the
	// transaction stays PENDING, so a corrected delivery — or a human — can still settle
	// it. See HandlePaymentSuccess for why an overpayment is booked instead.
	ErrWebhookAmountMismatch = NewError(CodeWebhookAmountMismatch, "webhook amount does not match the transaction", nil)
)

// Gateway errors
var (
	// ErrGatewayOutcomeUnknown is returned when a money-out call gave no usable answer.
	// See CodeGatewayOutcomeUnknown: the reservation stays held and the payout is
	// resolved by inquiry, never by re-sending.
	ErrGatewayOutcomeUnknown = NewError(CodeGatewayOutcomeUnknown, "gateway outcome unknown; resolve by inquiry", nil)

	ErrWebhookVerificationFailed = NewError(CodeWebhookVerificationFailed, "webhook signature verification failed", nil)
)

// Analytics error
var (
	ErrAnalyticsDataNotFound = NewError(CodeAnalyticsDataNotFound, "analytics data not found", nil)
	ErrAnalyticsQueryError   = NewError(CodeAnalyticsQueryError, "analytics query error", nil)
	ErrInvalidLimit          = NewError(CodeInvalidLimit, "invalid limit", nil)
	ErrInvalidOffset         = NewError(CodeInvalidOffset, "invalid offset", nil)
	ErrInvalidYear           = NewError(CodeInvalidYear, "invalid year", nil)
	ErrInvalidMonth          = NewError(CodeInvalidMonth, "invalid month", nil)
	ErrInvalidDateRange      = NewError(CodeInvalidDateRange, "invalid date range", nil)
	ErrInvalidIntervalType   = NewError(CodeInvalidIntervalType, "invalid interval type", nil)
	ErrInvalidAccountID      = NewError(CodeInvalidAccountID, "invalid account id", nil)
	ErrStartDateRequired     = NewError(CodeStartDateRequired, "start date is required", nil)
	ErrEndDateRequired       = NewError(CodeEndDateRequired, "end date is required", nil)
)
