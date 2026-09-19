package ledger

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// The money-out webhook is where a payout stops being in flight, and it is the only
// place a caller can learn that without either parsing a payload it has not verified
// or querying a table this package owns. These tests hold what the returned outcome
// promises: it names the row and the seller it belongs to, it says whether this
// delivery is what settled it, and a delivery that settles nothing says so rather
// than inventing a row.

// moneyOutBody builds a disbursement notification for one reference and status code.
func moneyOutBody(reference, statusCode string) []byte {
	return []byte(fmt.Sprintf(`{
		"response_code": "SP000",
		"event": "disbursement",
		"data": {
			"transaction_id": "SP-TX-WEBHOOK",
			"reference_number": %q,
			"transaction_status": {"code": %q, "desc": "scripted"},
			"bank": {"code": "BNINIDJA", "account_number": "712739123020001", "account_name": "Ria Florensi"},
			"gross_amount": {"currency": "IDR", "value": "50000.00"},
			"fee": {"currency": "IDR", "value": "3000"},
			"net_amount": {"currency": "IDR", "value": "47000.00"}
		}
	}`, reference, statusCode))
}

// inFlightPayout runs a withdrawal Singapay accepts without settling (status 01),
// which leaves exactly one PROCESSING row for a webhook to finish. It returns the
// client and the reference the payout travels under — the only key the webhook
// carries.
func inFlightPayout(t *testing.T) (*LedgerClient, string) {
	t.Helper()

	gateway := &fakeGateway{
		disburse:      payoutWithStatus(t, "01"),
		fee:           3_000,
		verifyWebhook: func(singapay.WebhookRequest) error { return nil },
	}

	client, _, _ := newPayoutTestClient(t, gateway, 100_000)

	response, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.NoError(t, err)
	require.Equal(t, string(domain.DisbursementStatusProcessing), response.Status)
	require.Len(t, gateway.references, 1)

	return client, gateway.references[0]
}

func TestHandleDisbursementNotification_ReportsTheRowItSettledAndItsSeller(t *testing.T) {
	client, reference := inFlightPayout(t)

	outcome, err := client.HandleDisbursementNotification(context.Background(),
		webhookRequest(moneyOutBody(reference, "00")))

	require.NoError(t, err)
	require.NotNil(t, outcome)
	require.NotNil(t, outcome.Disbursement)

	// The status the delivery just wrote, not the one it arrived to. A caller reading
	// PROCESSING here would send the wrong message to a seller whose money has landed.
	assert.Equal(t, domain.DisbursementStatusCompleted, outcome.Disbursement.Status)
	assert.True(t, outcome.Booked, "this delivery is what settled the row")

	// The caller's own id for the seller, not this package's account uuid: a caller
	// that had to translate one into the other would need the accounts table too.
	assert.Equal(t, "seller-1", outcome.SellerID)
}

func TestHandleDisbursementNotification_ReportsAFailedPayoutAsFailed(t *testing.T) {
	client, reference := inFlightPayout(t)

	// 06 is terminal-failed: no money moved and the reservation goes back.
	outcome, err := client.HandleDisbursementNotification(context.Background(),
		webhookRequest(moneyOutBody(reference, "06")))

	require.NoError(t, err)
	require.NotNil(t, outcome.Disbursement)
	assert.Equal(t, domain.DisbursementStatusFailed, outcome.Disbursement.Status)
	assert.True(t, outcome.Booked)
	assert.Equal(t, "seller-1", outcome.SellerID)
}

// A redelivery must not book a second time, and must not pretend nothing is there
// either: the row is returned with Booked false, which is what lets a caller act
// exactly once per outcome without keeping its own record of what it has seen.
func TestHandleDisbursementNotification_ARedeliveryReturnsTheRowUnbooked(t *testing.T) {
	client, reference := inFlightPayout(t)
	ctx := context.Background()

	first, err := client.HandleDisbursementNotification(ctx, webhookRequest(moneyOutBody(reference, "00")))
	require.NoError(t, err)
	require.True(t, first.Booked)

	second, err := client.HandleDisbursementNotification(ctx, webhookRequest(moneyOutBody(reference, "00")))
	require.NoError(t, err)
	require.NotNil(t, second.Disbursement)
	assert.Equal(t, domain.DisbursementStatusCompleted, second.Disbursement.Status)
	assert.False(t, second.Booked, "the row was already terminal; this delivery booked nothing")
	assert.Equal(t, "seller-1", second.SellerID)
}

// The same URL carries e-wallet top-ups and QRIS issuer results. Neither is a payout
// this ledger made, so the delivery is acknowledged and the outcome is empty.
func TestHandleDisbursementNotification_AnotherProductSettlesNothing(t *testing.T) {
	client, _ := inFlightPayout(t)

	body := []byte(`{"response_code":"SP000","event":"ewallet-topup","data":{"reference_number":"whatever"}}`)

	outcome, err := client.HandleDisbursementNotification(context.Background(), webhookRequest(body))

	require.NoError(t, err)
	require.NotNil(t, outcome)
	assert.Nil(t, outcome.Disbursement, "nothing of ours settled")
	assert.Empty(t, outcome.SellerID)
	assert.False(t, outcome.Booked)
}

// A reference this ledger never issued is still an error rather than an empty
// outcome: it means a delivery meant for another environment, or a reference that
// was lost, and both deserve a redelivery rather than a silent 200.
func TestHandleDisbursementNotification_AnUnknownReferenceIsNotFound(t *testing.T) {
	client, _ := inFlightPayout(t)

	outcome, err := client.HandleDisbursementNotification(context.Background(),
		webhookRequest(moneyOutBody("a-reference-we-never-sent", "00")))

	require.Error(t, err)
	assert.Nil(t, outcome)
}
