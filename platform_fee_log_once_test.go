package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// A failure the ledger returns to its caller is logged once, by the caller (the consuming
// service's settlement worker), with everything the ledger knows about it: the ledger's own
// line is DEBUG. Before, the sweep logged each failed row at ERROR and the worker logged it
// again: two error-tracker groups for one Singapay refusal (264 lines in 30 days).

func captureLogs(f *cardFeeFixture) *bytes.Buffer {
	var logs bytes.Buffer
	f.client.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &logs
}

func levelsOf(t *testing.T, logs *bytes.Buffer) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		record := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		level, _ := record["level"].(string)
		msg, _ := record["msg"].(string)
		out[level] = append(out[level], msg)
	}
	return out
}

func TestPlatformFeeSweep_ARefusedTransferIsReturnedWithEverythingTheLedgerKnew(t *testing.T) {
	f := newCardFeeFixture(t)
	fee := f.fees.GetFeeBreakdown(50000, "QRIS", domain.CurrencyIDR)
	qris := f.sale("INV-20261102074327-37ZJ7J", "QRIS", 50000, domain.RupiahToMinor(fee.GatewayFee))
	f.transferErr = &singapay.Error{StatusCode: http.StatusUnauthorized, Message: "Unauthorized. Invalid X-SIGNATURE"}
	logs := captureLogs(f)

	result := f.sweep()

	require.Equal(t, 1, result.Failed)
	require.Len(t, result.Errors, 1)
	got := result.Errors[0]
	assert.Equal(t, qris.UUID, got.TransactionID)
	assert.Equal(t, "INV-20261102074327-37ZJ7J", got.InvoiceNumber)
	assert.Equal(t, *qris.SettledPlatformFeeMinor, got.PlatformFeeMinor)
	assert.Equal(t, PlatformFeeStepSingapayTransfer, got.Step)
	assert.Equal(t, f.transferErr.Error(), got.Cause, "Singapay's answer, as the ledger logged it")
	assert.Equal(t, "Singapay account transfer failed: "+f.transferErr.Error(), got.ErrorMessage, "unchanged")
	assert.Equal(t, f.seller.UUID, got.SellerAccountID)
	assert.Equal(t, cardFeeSellerULID, got.FromAccount)
	assert.Equal(t, "000000000001", got.ToAccountNumber)
	assert.Equal(t, platformFeeTransferReference("INV-20261102074327-37ZJ7J"), got.MerchantRefNo)
	assert.Equal(t, fee.PlatformFee, got.PricedPlatformFee)
	assert.True(t, got.SettledFeeRecorded)

	levels := levelsOf(t, logs)
	assert.Empty(t, levels["ERROR"], "the caller logs it, once")
	assert.Contains(t, levels["DEBUG"], "Platform fee transfer failed - Singapay API error")
}

func TestPlatformFeeSweep_ASellerWithoutASubAccountIsReturnedWithItsStep(t *testing.T) {
	f := newCardFeeFixture(t)
	fee := f.fees.GetFeeBreakdown(50000, "QRIS", domain.CurrencyIDR)
	f.sale("INV-NOSUB", "QRIS", 50000, domain.RupiahToMinor(fee.GatewayFee))
	f.seller.SingapayAccountID = ""
	require.NoError(t, f.fakes.Account().Save(f.ctx, f.seller))
	logs := captureLogs(f)

	result := f.sweep()

	require.Len(t, result.Errors, 1)
	assert.Equal(t, PlatformFeeStepSubAccount, result.Errors[0].Step)
	assert.Equal(t, "", result.Errors[0].Cause, "a rule of the ledger's own: no cause")
	assert.Equal(t, f.seller.UUID, result.Errors[0].SellerAccountID)
	assert.Empty(t, levelsOf(t, logs)["ERROR"])
}

func TestCardPlatformFees_AFailedSellerIsReturnedWithItsCause(t *testing.T) {
	f := newCardFeeFixture(t)
	f.card("INV-CARD-1", 100000, 0)
	f.fakes.accountRepo.getByIDErr = errors.New("pq: canceling statement due to statement timeout")
	t.Cleanup(func() { f.fakes.accountRepo.getByIDErr = nil })
	logs := captureLogs(f)

	result, err := f.client.ReconcileCardPlatformFees(f.ctx, CardPlatformFeeReconciliationOptions{})
	require.NoError(t, err)
	require.Len(t, result.Sellers, 1)
	assert.Equal(t, CardFeeOutcomeFailed, result.Sellers[0].Outcome)
	assert.Equal(t, "pq: canceling statement due to statement timeout", result.Sellers[0].Cause)
	assert.Equal(t, "failed to read the seller account: pq: canceling statement due to statement timeout", result.Sellers[0].Detail)
	assert.Empty(t, levelsOf(t, logs)["ERROR"], "the caller logs it, once")
}
