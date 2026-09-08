package singapay

import (
	"errors"
	"testing"
)

// TestOutcomeClassification pins the decision that keeps a payout from being made twice.
//
// The DOKU path infers this from the HTTP status: 4xx is a definite refusal and releases
// the reserved balance, 5xx is unknown and holds it. Singapay answers HTTP 400 for SP001,
// SP002, SP004 and SP005 while telling merchants to call inquiry-status for all four,
// because the transfer may still settle. Those rows are the whole point of this table.
func TestOutcomeClassification(t *testing.T) {
	tests := []struct {
		code ResponseCode
		want Outcome
	}{
		{CodeTransactionFailure, OutcomeUnknown},
		{CodeGeneralFailure, OutcomeUnknown},
		{CodeTimeout, OutcomeUnknown},
		{CodeGeneralError, OutcomeUnknown},

		{CodeDuplicateReference, OutcomeDuplicate},

		{CodeInsufficientFunds, OutcomeRefused},
		{CodeBeneficiaryLimit, OutcomeRefused},
		{CodeAccountLimit, OutcomeRefused},
		{CodeInvalidReference, OutcomeRefused},
		{CodeTransactionNotFound, OutcomeRefused},
		{CodeBeneficiaryNotFound, OutcomeRefused},
		{CodeVendorNotActive, OutcomeRefused},
		{CodeMerchantNotFound, OutcomeRefused},
		{CodeBadRequest, OutcomeRefused},
		{CodeUnauthorized, OutcomeRefused},
		{CodeNotFound, OutcomeRefused},
		{CodeForbidden, OutcomeRefused},
		{CodeSignatureInvalid, OutcomeRefused},
		{CodeUnauthorizedIP, OutcomeRefused},
		{CodeValidationError, OutcomeRefused},
	}

	for _, tc := range tests {
		t.Run(string(tc.code), func(t *testing.T) {
			// Every one of these arrives as HTTP 400, so the status carries no
			// information and only the SP-code may be consulted.
			e := &Error{StatusCode: 400, Code: tc.code}
			if got := e.Outcome(); got != tc.want {
				t.Errorf("SP-code %s: got %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestOutcomeDefaultsToUnknown(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want Outcome
	}{
		{
			// A code Singapay adds later must not be read as a safe refusal.
			name: "unrecognised SP-code",
			err:  &Error{StatusCode: 400, Code: "SP099"},
			want: OutcomeUnknown,
		},
		{
			name: "transport failure, no answer at all",
			err:  &Error{Err: errors.New("connection reset")},
			want: OutcomeUnknown,
		},
		{
			name: "5xx without an SP-code",
			err:  &Error{StatusCode: 502},
			want: OutcomeUnknown,
		},
		{
			name: "4xx without an SP-code was rejected at the edge",
			err:  &Error{StatusCode: 422},
			want: OutcomeRefused,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Outcome(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestZeroOutcomeIsUnknown(t *testing.T) {
	// The zero value must be the conservative one, so a forgotten assignment holds
	// funds rather than releasing them.
	var o Outcome
	if o != OutcomeUnknown {
		t.Errorf("zero Outcome is %v, want unknown", o)
	}
}

func TestIsDuplicate(t *testing.T) {
	dup := &Error{StatusCode: 400, Code: CodeDuplicateReference}
	if !dup.IsDuplicate() {
		t.Error("SP004 must report as duplicate")
	}
	if (&Error{StatusCode: 400, Code: CodeInsufficientFunds}).IsDuplicate() {
		t.Error("SP003 is not a duplicate")
	}
}

func TestAsError(t *testing.T) {
	inner := &Error{StatusCode: 400, Code: CodeTimeout}
	wrapped := errors.Join(errors.New("context"), inner)

	got, ok := AsError(wrapped)
	if !ok {
		t.Fatal("want the *Error to be found through the chain")
	}
	if got.Code != CodeTimeout {
		t.Errorf("Code = %q", got.Code)
	}
}

func TestTransactionStatusTerminal(t *testing.T) {
	tests := []struct {
		status       TransactionStatus
		wantTerminal bool
		wantFailed   bool
	}{
		{StatusSuccess, true, false},
		{StatusInitiated, false, false},
		{StatusPaying, false, false},
		{StatusPending, false, false},
		// The three the DOKU switch has no case for. Reading them as still-in-flight
		// leaves a seller's balance reserved against a transfer that will never land.
		{StatusRefunded, true, true},
		{StatusCanceled, true, true},
		{StatusNotFound, true, true},
		{StatusFailed, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.status.String(), func(t *testing.T) {
			if got := tc.status.Terminal(); got != tc.wantTerminal {
				t.Errorf("Terminal() = %v, want %v", got, tc.wantTerminal)
			}
			if got := tc.status.Failed(); got != tc.wantFailed {
				t.Errorf("Failed() = %v, want %v", got, tc.wantFailed)
			}
		})
	}
}

func TestTransactionStatusUnknownCode(t *testing.T) {
	var s TransactionStatus = "99"
	if s.Terminal() {
		t.Error("an unknown status must not be treated as terminal")
	}
	if s.Succeeded() {
		t.Error("an unknown status must not be treated as success")
	}
}
