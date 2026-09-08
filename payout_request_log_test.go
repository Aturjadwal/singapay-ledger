package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/singapay"
)

func TestMaskAccountNumber(t *testing.T) {
	tests := []struct {
		name          string
		accountNumber string
		want          string
	}{
		{"typical account number keeps the last four", "712739123020001", "***********0001"},
		{"exactly four digits is masked whole", "1234", "****"},
		{"shorter than four is masked whole", "12", "**"},
		{"five digits reveals four", "12345", "*2345"},
		{"empty stays empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, maskAccountNumber(tt.accountNumber))
		})
	}
}

// The rendered body must be the body, not a summary of it: same field names, same types
// Singapay is given.
func TestPayoutRequestLogBody_RendersTheWireShape(t *testing.T) {
	req := singapay.DisburseRequest{
		AccountID:         "01SELLERACCOUNTULID",
		ReferenceNumber:   "ref-1",
		BankCode:          "BNINIDJA",
		BankAccountNumber: "712739123020001",
		Amount:            50000,
		Notes:             "DSB-1",
	}

	body := payoutRequestLogBody(req)

	assert.JSONEq(t, `{
		"account_id": "01SELLERACCOUNTULID",
		"reference_number": "ref-1",
		"bank_code": "BNINIDJA",
		"bank_account_number": "***********0001",
		"amount": 50000,
		"notes": "DSB-1"
	}`, body)

	assert.Equal(t, "712739123020001", req.BankAccountNumber,
		"masking for the log must not reach the request being sent")
}

// An empty account_id is the state that makes Singapay answer with a not-found rather than
// a clear validation error, so the body has to show it rather than omit the field.
func TestPayoutRequestLogBody_KeepsAnEmptyAccountID(t *testing.T) {
	req := singapay.DisburseRequest{Amount: 50000}

	assert.Contains(t, payoutRequestLogBody(req), `"account_id":""`)
}

// The point of the line is that it can be trusted as a record of the call: what it prints
// must be what the client was handed.
func TestExecutePayout_LogsTheBodyItSends(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, _, account := newPayoutTestClient(t, gw, 100000)

	logs := &bytes.Buffer{}
	client.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	resp, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.NoError(t, err)
	require.Len(t, gw.bodies, 1)

	record := findLogRecord(t, logs, "Sending Singapay disbursement")

	assert.Equal(t, payoutRequestLogBody(gw.bodies[0]), record["request_body"])
	assert.Equal(t, "/api/v2.0/disbursement/transfer", record["request_target"])
	assert.Equal(t, account.SingapayAccountID, record["singapay_account_id"])
	assert.Equal(t, false, record["singapay_account_id_empty"])
	assert.Equal(t, resp.DisbursementID, record["disbursement_id"])

	assert.NotContains(t, logs.String(), "712739123020001",
		"the full beneficiary account number must not reach the logs")
}

// A retry replays the same payout, and it is the one most likely to be read after the fact —
// it must leave the same record.
func TestRetryDisbursement_LogsTheBodyItSends(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, fakes, _ := newPayoutTestClient(t, gw, 100000)

	gw.err = &singapay.Error{Err: context.DeadlineExceeded}
	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err, "a timeout must leave the disbursement in flight for the retry")

	require.Len(t, fakes.disbursementRepo.disbursements, 1)
	var pending *domain.Disbursement
	for _, d := range fakes.disbursementRepo.disbursements {
		pending = d
	}

	logs := &bytes.Buffer{}
	client.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gw.err = nil

	_, err = client.RetryDisbursement(context.Background(), pending.UUID)
	require.NoError(t, err)
	require.Len(t, gw.bodies, 2)

	record := findLogRecord(t, logs, "Sending Singapay disbursement")
	assert.Equal(t, payoutRequestLogBody(gw.bodies[1]), record["request_body"])
	assert.Equal(t, pending.PayoutRequestID, record["payout_reference"])
}

// findLogRecord returns the first JSON log record carrying the given message.
func findLogRecord(t *testing.T, logs *bytes.Buffer, message string) map[string]any {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		record := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(line), &record), "log line is not JSON: %s", line)
		if record["msg"] == message {
			return record
		}
	}

	t.Fatalf("no log record with msg %q in:\n%s", message, logs.String())
	return nil
}
