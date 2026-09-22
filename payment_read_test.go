package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// A payment read back is what lets a payer resume one they walked away from — the link in
// a WhatsApp message reopens the instrument instead of issuing a second one. So the read
// has to return the instrument itself, not just a status: the virtual account number is
// the whole point of reopening the page.
func TestGetPaymentByInvoiceNumber_ReturnsTheInstrumentToResume(t *testing.T) {
	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()
	expiry := time.Now().Add(30 * time.Minute)

	fee, err := domain.NewFeeBreakdown(50000, 1000, 4000, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
	require.NoError(t, err)

	tx := domain.NewProductTransaction("buyer-1", "seller-1", "service-1", "SERVICE", "INV-RESUME", *fee,
		map[string]any{"payment_numbers": []any{float64(1)}})
	require.NoError(t, fakes.ProductTransaction().Save(ctx, tx))

	paymentRequest := domain.NewPaymentRequest(tx.UUID, "SP-VA-1", "VA_BCA", fee.TotalCharged, domain.CurrencyIDR, expiry)
	paymentRequest.SetPaymentCode("8808123456789")
	require.NoError(t, fakes.PaymentRequest().Save(ctx, paymentRequest))

	client := &LedgerClient{repoProvider: fakes, logger: testLogger()}

	payment, err := client.GetPaymentByInvoiceNumber(ctx, "INV-RESUME")
	require.NoError(t, err)

	assert.Equal(t, tx.UUID, payment.TransactionID)
	assert.Equal(t, "INV-RESUME", payment.InvoiceNumber)
	assert.Equal(t, string(domain.TransactionStatusPending), payment.Status,
		"a freshly issued instrument is still waiting to be paid")
	assert.Equal(t, "VA_BCA", payment.PaymentChannel)
	assert.Equal(t, "8808123456789", payment.PaymentCode,
		"the account number is what the payer needs back")
	assert.Equal(t, expiry.Unix(), payment.ExpiresAt)
	assert.False(t, payment.IsExpired)
	assert.Equal(t, fee.TotalCharged, payment.TotalCharged,
		"a closed virtual account accepts the gross, not the seller's price")
	assert.Equal(t, "service-1", payment.ProductID)
	assert.Equal(t, map[string]any{"payment_numbers": []any{float64(1)}}, payment.Metadata,
		"the metadata says which terms this invoice covers")
	assert.Nil(t, payment.CompletedAt)
}

// The expiry is the one signal that says the number on the page is dead. The ledger never
// sweeps on it — a lapsed instrument leaves its transaction PENDING forever — so a caller
// that only looked at Status would show a payer a virtual account no bank will accept.
func TestGetPaymentByInvoiceNumber_FlagsALapsedInstrument(t *testing.T) {
	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	fee, err := domain.NewFeeBreakdown(50000, 1000, 4000, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
	require.NoError(t, err)

	tx := domain.NewProductTransaction("buyer-1", "seller-1", "service-1", "SERVICE", "INV-LAPSED", *fee, nil)
	require.NoError(t, fakes.ProductTransaction().Save(ctx, tx))
	require.NoError(t, fakes.PaymentRequest().Save(ctx,
		domain.NewPaymentRequest(tx.UUID, "SP-VA-2", "VA_BCA", fee.TotalCharged, domain.CurrencyIDR,
			time.Now().Add(-time.Minute))))

	client := &LedgerClient{repoProvider: fakes, logger: testLogger()}

	payment, err := client.GetPaymentByInvoiceNumber(ctx, "INV-LAPSED")
	require.NoError(t, err)

	assert.Equal(t, string(domain.TransactionStatusPending), payment.Status,
		"an instrument that lapses at the gateway leaves its transaction where it was")
	assert.True(t, payment.IsExpired, "so expiry is the only thing that says to issue a new one")
}

// A paid transaction must read back as paid. This is the case that stops a payer being
// handed payment instructions for a booking they already settled.
func TestGetPaymentByInvoiceNumber_ReportsAPaidTransaction(t *testing.T) {
	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	fee, err := domain.NewFeeBreakdown(50000, 1000, 4000, domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
	require.NoError(t, err)

	tx := domain.NewProductTransaction("buyer-1", "seller-1", "service-1", "SERVICE", "INV-PAID", *fee, nil)
	tx.MarkCompleted()
	require.NoError(t, fakes.ProductTransaction().Save(ctx, tx))
	require.NoError(t, fakes.PaymentRequest().Save(ctx,
		domain.NewPaymentRequest(tx.UUID, "SP-QR-1", "QRIS", fee.TotalCharged, domain.CurrencyIDR,
			time.Now().Add(time.Hour))))

	client := &LedgerClient{repoProvider: fakes, logger: testLogger()}

	payment, err := client.GetPaymentByInvoiceNumber(ctx, "INV-PAID")
	require.NoError(t, err)

	assert.Equal(t, string(domain.TransactionStatusCompleted), payment.Status)
	require.NotNil(t, payment.CompletedAt, "a completed transaction records when the payer paid")
}

// The consumer decides between 404 and 500 on the error's own Code — ErrCode() unwraps to
// the repository's generic not-found — so the code carried by the returned error is part of
// the contract and is asserted the way a caller reads it.
func TestGetPaymentByInvoiceNumber_UnknownInvoiceIsNotFound(t *testing.T) {
	client := &LedgerClient{repoProvider: NewFakeRepositoryProvider(), logger: testLogger()}

	_, err := client.GetPaymentByInvoiceNumber(context.Background(), "INV-NOBODY")
	require.Error(t, err)

	var appErr ledgererr.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, ledgererr.CodeProductTransactionNotFound, appErr.Code)
}

func TestGetPaymentByInvoiceNumber_RefusesAnEmptyInvoiceNumber(t *testing.T) {
	client := &LedgerClient{repoProvider: NewFakeRepositoryProvider(), logger: testLogger()}

	_, err := client.GetPaymentByInvoiceNumber(context.Background(), "")
	require.Error(t, err)

	var appErr ledgererr.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, ledgererr.CodeInvalidRequest, appErr.Code,
		"an empty key would otherwise reach the database as a lookup for nothing")
}
