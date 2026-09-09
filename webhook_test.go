package ledger

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/ledgererr"
	"github.com/21strive/ledger/singapay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The money-in webhook is the one path where a mistake credits a seller money that never
// arrived, and Singapay retries — sometimes concurrently — so "it worked once" proves
// nothing. These tests hold the four properties that matter: a paid delivery books once, a
// second delivery books nothing, two at once book once between them, and a delivery that
// underpays books nothing at all.

const (
	webhookSellerNet   = int64(100_000)
	webhookPlatformFee = int64(10_000)
	webhookGatewayFee  = int64(4_000)
	webhookTotal       = webhookSellerNet + webhookPlatformFee + webhookGatewayFee
	webhookInvoice     = "INV-20260909120000-ABC123"
)

// webhookFixture is a ledger with one PENDING transaction priced at webhookTotal, its
// payment request, and the three accounts a payment credits.
type webhookFixture struct {
	client    *LedgerClient
	fakes     *FakeRepositoryProvider
	gateway   *fakeGateway
	seller    *domain.Account
	platform  *domain.Account
	gwAccount *domain.Account
	productTx *domain.ProductTransaction
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()

	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	seller := domain.NewSellerAccount("SP-SELLER-ULID", "seller-1", domain.CurrencyIDR)
	platform := domain.NewPlatformAccount("SP-PLATFORM-ULID", "platform", domain.CurrencyIDR)
	gwAccount := domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
	for _, acc := range []*domain.Account{&seller, &platform, &gwAccount} {
		require.NoError(t, fakes.accountRepo.Save(ctx, acc))
	}

	fee := domain.FeeBreakdown{
		SellerPrice:     webhookSellerNet,
		PlatformFee:     webhookPlatformFee,
		GatewayFee:      webhookGatewayFee,
		TotalCharged:    webhookTotal,
		SellerNetAmount: webhookSellerNet,
		FeeModel:        domain.FeeModelGatewayOnCustomer,
		Currency:        domain.CurrencyIDR,
	}
	productTx := domain.NewProductTransaction("buyer-1", seller.UUID, "service-1", "SERVICE", webhookInvoice, fee, nil)
	require.NoError(t, fakes.productTransactionRepo.Save(ctx, productTx))

	paymentReq := domain.NewPaymentRequest(productTx.UUID, "SP-VA-1", "VA_BCA", webhookTotal, domain.CurrencyIDR, productTx.CreatedAt)
	require.NoError(t, fakes.paymentRequestRepo.Save(ctx, paymentReq))

	// Verification is exercised on its own in the singapay package, against the real HMAC.
	// Here it is scripted so each test can say whether the delivery was authentic without
	// also re-testing the signature scheme.
	gateway := &fakeGateway{verifyWebhook: func(singapay.WebhookRequest) error { return nil }}

	return &webhookFixture{
		client: &LedgerClient{
			txProvider:   NewFakeTransactionProvider(fakes),
			repoProvider: fakes,
			logger:       testLogger(),
			gateway:      gateway,
		},
		fakes:     fakes,
		gateway:   gateway,
		seller:    &seller,
		platform:  &platform,
		gwAccount: &gwAccount,
		productTx: productTx,
	}
}

// vaPaidBody is a virtual-account payment confirmation. VA is the channel that carries its
// own fee, and it puts the merchant reference in reff_no rather than merchant_reff_no —
// both of which the parser has to get right for any of this to match an invoice.
func vaPaidBody(charged int64) []byte {
	return []byte(fmt.Sprintf(`{
		"status": 200,
		"success": true,
		"event": "va-transaction",
		"data": {
			"transaction": {
				"id": 991,
				"reff_no": %q,
				"transaction_id": "SP-TX-991",
				"type": "va",
				"status": "paid",
				"amount": %d,
				"total_amount": %d
			},
			"payment": {
				"method": "va",
				"vendor": "BCA",
				"additional_info": {
					"va_number": "8888000012345",
					"fees": {"name": "VA Fee", "amount": %d, "currency": "IDR"}
				}
			}
		}
	}`, webhookInvoice, charged, charged, webhookGatewayFee))
}

func webhookRequest(body []byte) singapay.WebhookRequest {
	return singapay.WebhookRequest{
		Endpoint:  "/singapay/notification",
		Body:      body,
		Signature: "scripted",
	}
}

// balances sums the entries actually written, which is the only number that can prove a
// double credit did not happen.
func (f *webhookFixture) balances(t *testing.T, accountUUID string) int64 {
	t.Helper()
	pending, _, err := f.fakes.ledgerEntryRepo.GetAllBalances(context.Background(), accountUUID)
	require.NoError(t, err)
	return pending
}

func TestHandlePaymentSuccess_BooksThePaymentOnce(t *testing.T) {
	f := newWebhookFixture(t)

	require.NoError(t, f.client.HandlePaymentSuccess(context.Background(), webhookRequest(vaPaidBody(webhookTotal))))

	assert.Equal(t, webhookSellerNet, f.balances(t, f.seller.UUID), "seller credited its net amount")
	assert.Equal(t, webhookPlatformFee, f.balances(t, f.platform.UUID), "platform credited its fee")
	assert.Equal(t, webhookGatewayFee, f.balances(t, f.gwAccount.UUID), "gateway expense account carries the channel fee")

	tx, err := f.fakes.productTransactionRepo.GetByInvoiceNumber(context.Background(), webhookInvoice)
	require.NoError(t, err)
	assert.Equal(t, domain.TransactionStatusCompleted, tx.Status)
	assert.NotNil(t, tx.CompletedAt, "completed_at is stamped by the compare-and-set, not left to the caller")
}

// A retried delivery is ordinary traffic, not an incident: Singapay redelivers until it
// gets a success. It must be answered with success and must write nothing.
func TestHandlePaymentSuccess_DuplicateDeliveryBooksNothingMore(t *testing.T) {
	f := newWebhookFixture(t)
	ctx := context.Background()

	require.NoError(t, f.client.HandlePaymentSuccess(ctx, webhookRequest(vaPaidBody(webhookTotal))))
	entriesAfterFirst := len(f.fakes.ledgerEntryRepo.entries)
	updatesAfterFirst := f.fakes.paymentRequestRepo.updates

	require.NoError(t, f.client.HandlePaymentSuccess(ctx, webhookRequest(vaPaidBody(webhookTotal))),
		"a duplicate is acknowledged, not refused")

	assert.Equal(t, entriesAfterFirst, len(f.fakes.ledgerEntryRepo.entries), "no second set of ledger entries")
	assert.Equal(t, updatesAfterFirst, f.fakes.paymentRequestRepo.updates, "the payment request is not rewritten")
	assert.Equal(t, webhookSellerNet, f.balances(t, f.seller.UUID), "the seller is credited once, not twice")
}

// The duplicate above is caught by the IsPending read, which only works because the first
// delivery had already committed. Two deliveries in flight at once both read PENDING, and
// what stops the second one is the conditional UPDATE — so this drives the transition
// directly, which is the layer that guard lives at.
func TestUpdateStatusIf_OnlyOneCallerMovesTheTransaction(t *testing.T) {
	f := newWebhookFixture(t)
	ctx := context.Background()
	repository := f.fakes.productTransactionRepo

	const racers = 8
	var (
		mu    sync.Mutex
		moved int
	)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The fake's map is not goroutine-safe, so the lock stands in for the row
			// lock Postgres takes on the real conditional UPDATE. What is under test is
			// the compare-and-set answer, not the locking.
			mu.Lock()
			defer mu.Unlock()
			ok, err := repository.UpdateStatusIf(ctx, f.productTx.UUID,
				domain.TransactionStatusPending, domain.TransactionStatusCompleted, f.productTx.CreatedAt)
			require.NoError(t, err)
			if ok {
				moved++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, moved, "exactly one caller may move a transaction out of PENDING")
}

// The whole point of the compare-and-set: the loser of the race writes nothing, so the
// entries it had prepared never reach the ledger.
//
// The duplicate test above cannot show this. There the second delivery reads a COMPLETED
// row and stops at the IsPending check, never reaching the ledger writes. A genuinely
// concurrent delivery reads PENDING — the winner has not committed yet — and walks all the
// way to the compare-and-set with a full set of entries in hand. That is the case where
// getting it wrong credits the seller twice, and it is the case reproduced here: the row is
// moved after this delivery has read it.
func TestHandlePaymentSuccess_LosingTheRaceWritesNothing(t *testing.T) {
	f := newWebhookFixture(t)
	ctx := context.Background()

	// The winner commits between this delivery's read and its write. The read happens
	// inside HandlePaymentSuccess and returns a detached copy, so the copy still says
	// PENDING while the row no longer does — which is precisely the window.
	f.fakes.productTransactionRepo.beforeCAS = func() {
		moved, err := f.fakes.productTransactionRepo.updateStatusIf(f.productTx.UUID,
			domain.TransactionStatusPending, domain.TransactionStatusCompleted, f.productTx.CreatedAt)
		require.NoError(t, err)
		require.True(t, moved)
	}

	require.NoError(t, f.client.HandlePaymentSuccess(ctx, webhookRequest(vaPaidBody(webhookTotal))))

	assert.Empty(t, f.fakes.ledgerEntryRepo.entries, "the losing delivery books no entries")
	assert.Zero(t, f.balances(t, f.seller.UUID), "and credits nothing")
}

// A signature that does not verify is not a payment. Nothing about the body may be acted
// on, however convincing it looks.
func TestHandlePaymentSuccess_RejectsAnUnverifiedDelivery(t *testing.T) {
	f := newWebhookFixture(t)
	f.gateway.verifyWebhook = func(singapay.WebhookRequest) error {
		return &singapay.Error{Code: singapay.CodeSignatureInvalid, Message: "webhook signature mismatch"}
	}

	err := f.client.HandlePaymentSuccess(context.Background(), webhookRequest(vaPaidBody(webhookTotal)))

	require.Error(t, err)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeWebhookVerificationFailed, err))
	assert.Empty(t, f.fakes.ledgerEntryRepo.entries, "a forged delivery books nothing")

	tx, getErr := f.fakes.productTransactionRepo.GetByInvoiceNumber(context.Background(), webhookInvoice)
	require.NoError(t, getErr)
	assert.Equal(t, domain.TransactionStatusPending, tx.Status, "and moves nothing")
}

// A verified delivery reporting less than the transaction was priced at is refused. The
// entries are built from the transaction as priced, so booking it would credit the seller
// money that never arrived — and leaving the row PENDING keeps the situation recoverable.
func TestHandlePaymentSuccess_RefusesAnUnderpayment(t *testing.T) {
	f := newWebhookFixture(t)

	err := f.client.HandlePaymentSuccess(context.Background(), webhookRequest(vaPaidBody(webhookTotal-1)))

	require.Error(t, err)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeWebhookAmountMismatch, err))
	assert.Empty(t, f.fakes.ledgerEntryRepo.entries, "nothing is booked")

	tx, getErr := f.fakes.productTransactionRepo.GetByInvoiceNumber(context.Background(), webhookInvoice)
	require.NoError(t, getErr)
	assert.Equal(t, domain.TransactionStatusPending, tx.Status,
		"the transaction stays PENDING so a corrected delivery can still settle it")
}

// An overpayment is the opposite call. On QRIS the payer's tip lands in total_amount, so
// refusing these would reject ordinary traffic — and an overpayment cannot credit a
// balance that is not covered by money received.
func TestHandlePaymentSuccess_BooksAnOverpaymentAsPriced(t *testing.T) {
	f := newWebhookFixture(t)

	require.NoError(t, f.client.HandlePaymentSuccess(context.Background(), webhookRequest(vaPaidBody(webhookTotal+5_000))))

	assert.Equal(t, webhookSellerNet, f.balances(t, f.seller.UUID),
		"the seller is credited the transaction's price, not the webhook's amount")
}

// The same URL carries expiry and failure. Neither is a payment, and neither may be
// treated as one — but neither is an error either, so the delivery is acknowledged.
func TestHandlePaymentSuccess_IgnoresANonPaidNotification(t *testing.T) {
	f := newWebhookFixture(t)

	body := []byte(fmt.Sprintf(`{
		"status": 200, "success": true, "event": "va-transaction",
		"data": {
			"transaction": {"reff_no": %q, "type": "va", "status": "expired", "amount": %d},
			"payment": {"method": "va"}
		}
	}`, webhookInvoice, webhookTotal))

	require.NoError(t, f.client.HandlePaymentSuccess(context.Background(), webhookRequest(body)))

	assert.Empty(t, f.fakes.ledgerEntryRepo.entries, "an expiry books nothing")
	tx, err := f.fakes.productTransactionRepo.GetByInvoiceNumber(context.Background(), webhookInvoice)
	require.NoError(t, err)
	assert.Equal(t, domain.TransactionStatusPending, tx.Status)
}

// A payment link puts the merchant reference on the link, not on the transaction — its
// transaction.reff_no is the id of one attempt. Reading the transaction's field would
// never match a payment link to its invoice, so this fixes that resolution in a test.
func TestHandlePaymentSuccess_MatchesAPaymentLinkByItsLinkReference(t *testing.T) {
	f := newWebhookFixture(t)

	body := []byte(fmt.Sprintf(`{
		"status": 200, "success": true, "event": "payment-link-transaction",
		"data": {
			"transaction": {"reff_no": "attempt-77", "type": "pl", "status": "paid", "total_amount": %d},
			"payment": {
				"method": "payment_link",
				"additional_info": {"payment_link": {"id": 77, "reff_no": %q}}
			}
		}
	}`, webhookTotal, webhookInvoice))

	require.NoError(t, f.client.HandlePaymentSuccess(context.Background(), webhookRequest(body)))

	assert.Equal(t, webhookSellerNet, f.balances(t, f.seller.UUID))
}

// A verified delivery naming an invoice this ledger has never issued is a 404, not a
// credit. It is worth its own test because the lookup failing open would book against
// whatever the next read returned.
func TestHandlePaymentSuccess_RefusesAnUnknownInvoice(t *testing.T) {
	f := newWebhookFixture(t)

	body := []byte(fmt.Sprintf(`{
		"status": 200, "success": true, "event": "va-transaction",
		"data": {
			"transaction": {"reff_no": "INV-NEVER-ISSUED", "type": "va", "status": "paid", "total_amount": %d},
			"payment": {"method": "va"}
		}
	}`, webhookTotal))

	err := f.client.HandlePaymentSuccess(context.Background(), webhookRequest(body))

	require.Error(t, err)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeNotFound, err))
	assert.Empty(t, f.fakes.ledgerEntryRepo.entries)
}
