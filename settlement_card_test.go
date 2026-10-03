package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// paymentLinkHistories answers the payment-link history listing the settling pass falls
// back to when the webhook's attempt id was never stored.
type paymentLinkHistories struct {
	*fakeGateway
	rows []singapay.PaymentLinkHistory
}

func (g *paymentLinkHistories) ListPaymentLinkHistories(context.Context, string, singapay.SettlementWindow) ([]singapay.PaymentLinkHistory, singapay.Pagination, error) {
	return g.rows, singapay.Pagination{Count: len(g.rows), Total: len(g.rows)}, nil
}

func cardSettlementFixture(hasSettle bool) (*LedgerClient, *domain.ProductTransaction, *domain.PaymentRequest) {
	gateway := &paymentLinkHistories{
		fakeGateway: &fakeGateway{},
		rows: []singapay.PaymentLinkHistory{{
			ID:                77,
			ReffNo:            "18917720251110094037705",
			PaymentLinkReffNo: "INV-CARD-1",
			Amount:            singapay.NewAmount(106186, "IDR"),
			HasSettle:         hasSettle,
		}},
	}
	client := &LedgerClient{gateway: gateway, logger: testLogger()}
	tx := &domain.ProductTransaction{
		InvoiceNumber: "INV-CARD-1",
		Fee:           domain.FeeBreakdown{TotalCharged: 106186, GatewayFee: 5186},
	}
	paymentReq := &domain.PaymentRequest{PaymentChannel: ChannelCreditCard}
	return client, tx, paymentReq
}

// A card payment settles through the payment link it was issued as, and is booked under
// its own channel. The fee is the one it was priced at: the link reports none, so there is
// nothing to reconcile against, and FeeReported says so.
func TestReadSettledTransaction_ACardSettlesThroughItsPaymentLink(t *testing.T) {
	client, tx, paymentReq := cardSettlementFixture(true)

	settled, err := client.readSettledTransaction(context.Background(), "01SELLERACCOUNTULID", tx, paymentReq)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, ChannelCreditCard, settled.PaymentChannel)
	assert.Equal(t, "77", settled.GatewayTransactionID)
	assert.Equal(t, "INV-CARD-1", settled.MerchantReference)
	assert.Equal(t, int64(10618600), settled.GrossMinor)
	assert.Equal(t, int64(518600), settled.FeeMinor, "the priced fee, in sen")
	assert.Equal(t, int64(10100000), settled.NetMinor)
	assert.False(t, settled.FeeReported)
}

func TestReadSettledTransaction_AnUnsettledCardIsNotSettledYet(t *testing.T) {
	client, tx, paymentReq := cardSettlementFixture(false)

	settled, err := client.readSettledTransaction(context.Background(), "01SELLERACCOUNTULID", tx, paymentReq)

	require.NoError(t, err)
	assert.Nil(t, settled)
}

// The production case: the card payment's money-in webhook carried no numeric id, so "0"
// was stored, and every settling pass asked Singapay for history 0 and got a 404 — the
// transaction never settled. A "0" is no id: the lookup must take the fallback, the
// history listing matched on the invoice, and never ask for history 0.
func TestReadSettledTransaction_AStoredZeroIdFallsBackToTheInvoice(t *testing.T) {
	client, tx, paymentReq := cardSettlementFixture(true)
	paymentReq.GatewayTransactionID = "0"
	// paymentLinkHistories does not script GetPaymentLinkHistory: reaching it panics.

	settled, err := client.readSettledTransaction(context.Background(), "01SELLERACCOUNTULID", tx, paymentReq)

	require.NoError(t, err)
	require.NotNil(t, settled)
	assert.Equal(t, "77", settled.GatewayTransactionID)
	assert.Equal(t, ChannelCreditCard, settled.PaymentChannel)
}

func TestStoredGatewayTransactionID(t *testing.T) {
	for stored, want := range map[string]string{
		"":         "",
		"0":        "",
		" 0 ":      "",
		"-1":       "",
		"524493":   "524493",
		"VA-ABC-1": "VA-ABC-1", // not numeric: a business id, kept as given
	} {
		got := storedGatewayTransactionID(&domain.PaymentRequest{GatewayTransactionID: stored})
		assert.Equal(t, want, got, "stored %q", stored)
	}
}

// QRIS reads by the numeric id; a stored "0" must fall back to the instrument id, which
// for QRIS is the same entity.
func TestGatewayNumericID_AStoredZeroFallsBackToTheRequestID(t *testing.T) {
	id, err := gatewayNumericID(&domain.PaymentRequest{GatewayTransactionID: "0", RequestID: "361677"})

	require.NoError(t, err)
	assert.Equal(t, int64(361677), id)
}
