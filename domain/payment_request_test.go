package domain_test

import (
	"testing"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/stretchr/testify/assert"
)

func newCardPaymentRequest() *domain.PaymentRequest {
	return domain.NewPaymentRequest("ptx-1", "98465", "CREDIT_CARD", 68041, domain.CurrencyIDR, time.Now().Add(time.Hour))
}

// A payment link's webhook carries no numeric id, and the caller used to format the missing
// value as "0" — which settlement then asked Singapay for, as history 0, and got a 404. A
// zero, or any whole number below it, is no id, and is not stored whoever supplies it.
func TestSetGatewayTransaction_IgnoresAnIdThatIsNoId(t *testing.T) {
	for _, tc := range []struct {
		id, ref         string
		wantID, wantRef string
	}{
		{id: "0", ref: "x", wantID: "", wantRef: "x"},
		{id: "-5", wantID: ""},
		{id: " 0 ", wantID: ""},
		{id: "80937", wantID: "80937"},
		// Not a number at all: a business id, kept as given.
		{id: "VA-20251024-0001H9X8ZK", wantID: "VA-20251024-0001H9X8ZK"},
		{ref: " 18917720251110094037705 ", wantRef: "18917720251110094037705"},
	} {
		pr := newCardPaymentRequest()

		pr.SetGatewayTransaction(tc.id, tc.ref)

		assert.Equal(t, tc.wantID, pr.GatewayTransactionID, "id %q", tc.id)
		assert.Equal(t, tc.wantRef, pr.GatewayTransactionRef, "ref %q", tc.ref)
	}
}

// An empty or refused value leaves what is already there: a later call can fill in an
// identifier, never blank one out.
func TestSetGatewayTransaction_KeepsWhatItAlreadyHas(t *testing.T) {
	pr := newCardPaymentRequest()
	pr.SetGatewayTransaction("80937", "18917720251110094037705")

	pr.SetGatewayTransaction("0", "")

	assert.Equal(t, "80937", pr.GatewayTransactionID)
	assert.Equal(t, "18917720251110094037705", pr.GatewayTransactionRef)
}
