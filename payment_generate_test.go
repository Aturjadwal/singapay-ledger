package ledger

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// moneyInGateway records the instruments GeneratePayment and GenerateSubscriptionPayment
// ask Singapay to issue. Everything else on the gateway stays unscripted.
type moneyInGateway struct {
	*fakeGateway

	virtualAccounts []singapay.CreateVirtualAccountRequest
	qris            []singapay.GenerateQRISRequest
	ewalletOrders   []singapay.CreateEwalletOrderRequest
	paymentLinks    []singapay.CreatePaymentLinkRequest
}

func (g *moneyInGateway) CreateVirtualAccount(_ context.Context, _ string, req singapay.CreateVirtualAccountRequest) (*singapay.VirtualAccount, error) {
	g.virtualAccounts = append(g.virtualAccounts, req)
	return &singapay.VirtualAccount{ID: "SP-VA-1", Number: "8808123456789"}, nil
}

func (g *moneyInGateway) GenerateQRIS(_ context.Context, _ string, req singapay.GenerateQRISRequest) (*singapay.QRISTransaction, error) {
	g.qris = append(g.qris, req)
	return &singapay.QRISTransaction{ID: 1, QRData: "00020101021226660015ID.SINGAPAY"}, nil
}

func (g *moneyInGateway) CreateEwalletOrder(_ context.Context, req singapay.CreateEwalletOrderRequest) (*singapay.EwalletTransaction, error) {
	g.ewalletOrders = append(g.ewalletOrders, req)
	return &singapay.EwalletTransaction{ID: 2, CheckoutURL: "https://checkout.example/dana"}, nil
}

func (g *moneyInGateway) CreatePaymentLink(_ context.Context, _ string, req singapay.CreatePaymentLinkRequest) (*singapay.PaymentLink, error) {
	g.paymentLinks = append(g.paymentLinks, req)
	return &singapay.PaymentLink{ID: 3, PaymentURL: "https://pay.example/link"}, nil
}

func (g *moneyInGateway) issued() int {
	return len(g.virtualAccounts) + len(g.qris) + len(g.ewalletOrders) + len(g.paymentLinks)
}

// activeFeeConfigs is the fee table the payment paths price against. Only the one read
// they make is implemented.
type activeFeeConfigs struct {
	domain.FeeConfigRepository
	configs []*domain.FeeConfig
}

func (f activeFeeConfigs) GetAllActive(context.Context) ([]*domain.FeeConfig, error) {
	return f.configs, nil
}

// withFeeConfigs gives the in-memory repositories the fee table the shared fakes leave out.
type withFeeConfigs struct {
	*FakeRepositoryProvider
	fees activeFeeConfigs
}

func (p withFeeConfigs) FeeConfig() domain.FeeConfigRepository {
	return p.fees
}

// newPaymentTestClient is a ledger with a seller and the platform both holding a Singapay
// sub-account, and a fee for every channel these tests issue on.
func newPaymentTestClient(t *testing.T) (*LedgerClient, *moneyInGateway) {
	t.Helper()

	ctx := context.Background()
	fakes := NewFakeRepositoryProvider()

	seller := domain.NewSellerAccount("01SELLERACCOUNTULID", "seller-1", domain.CurrencyIDR)
	require.NoError(t, fakes.Account().Save(ctx, &seller))
	platform := domain.NewPlatformAccount("01PLATFORMACCOUNTULID", "platform", domain.CurrencyIDR)
	require.NoError(t, fakes.Account().Save(ctx, &platform))

	gateway := &moneyInGateway{fakeGateway: &fakeGateway{}}

	return &LedgerClient{
		txProvider: NewFakeTransactionProvider(fakes),
		repoProvider: withFeeConfigs{
			FakeRepositoryProvider: fakes,
			fees: activeFeeConfigs{configs: []*domain.FeeConfig{
				{ConfigType: domain.FeeConfigTypePlatform, PaymentChannel: "PLATFORM", FeeType: domain.FeeTypeFixed, FixedAmount: 1000, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "VA_BCA", FeeType: domain.FeeTypeFixed, FixedAmount: 4000, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "QRIS", FeeType: domain.FeeTypePercentage, Percentage: 0.7, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "EWALLET_DANA", FeeType: domain.FeeTypePercentage, Percentage: 1.5, IsActive: true},
			}},
		},
		logger:  testLogger(),
		gateway: gateway,
	}, gateway
}

func paymentRequest(channel, buyerEmail string) *GeneratePaymentRequest {
	return &GeneratePaymentRequest{
		SellerAccountID: "seller-1",
		BuyerAccountID:  "customer-1",
		BuyerName:       "Andi",
		BuyerEmail:      buyerEmail,
		ProductID:       "service-1",
		ProductType:     "SERVICE",
		SellerPrice:     100000,
		Currency:        string(domain.CurrencyIDR),
		PaymentChannel:  channel,
	}
}

// Singapay does not ask for the payer's email: a virtual account and a QRIS charge never
// receive it, and an e-wallet order takes it as optional. So a payer who gave none is
// charged on every channel, rather than refused before the gateway is even called.
func TestGeneratePayment_IssuesWithoutABuyerEmail(t *testing.T) {
	for _, channel := range []string{"VA_BCA", "QRIS", "EWALLET_DANA"} {
		t.Run(channel, func(t *testing.T) {
			client, gateway := newPaymentTestClient(t)

			response, err := client.GeneratePayment(context.Background(), paymentRequest(channel, ""))

			require.NoError(t, err)
			assert.NotEmpty(t, response.InvoiceNumber)
			assert.Equal(t, channel, response.PaymentChannel)
			assert.Equal(t, 1, gateway.issued())
		})
	}
}

// The one channel here that carries the email leaves the field out when there is none,
// instead of sending Singapay an empty address.
func TestGeneratePayment_AnEwalletOrderLeavesOutAMissingEmail(t *testing.T) {
	client, gateway := newPaymentTestClient(t)

	_, err := client.GeneratePayment(context.Background(), paymentRequest("EWALLET_DANA", ""))
	require.NoError(t, err)

	require.Len(t, gateway.ewalletOrders, 1)
	body, err := json.Marshal(gateway.ewalletOrders[0])
	require.NoError(t, err)
	assert.NotContains(t, string(body), "customer_email")
}

func TestGeneratePayment_AGivenEmailStillReachesTheEwalletOrder(t *testing.T) {
	client, gateway := newPaymentTestClient(t)

	_, err := client.GeneratePayment(context.Background(), paymentRequest("EWALLET_DANA", "andi@example.com"))
	require.NoError(t, err)

	require.Len(t, gateway.ewalletOrders, 1)
	assert.Equal(t, "andi@example.com", gateway.ewalletOrders[0].CustomerEmail)
}

// A subscription is paid through a payment link, where the email is only a pre-fill too.
func TestGenerateSubscriptionPayment_IssuesWithoutABuyerEmail(t *testing.T) {
	client, gateway := newPaymentTestClient(t)

	response, err := client.GenerateSubscriptionPayment(context.Background(), &GenerateSubscriptionPaymentRequest{
		BuyerAccountID:    "user-1",
		BuyerName:         "Monika",
		ProductID:         "package-1",
		SubscriptionPrice: 99000,
		Currency:          string(domain.CurrencyIDR),
	})

	require.NoError(t, err)
	assert.Equal(t, "https://pay.example/link", response.PaymentURL)

	require.Len(t, gateway.paymentLinks, 1)
	body, err := json.Marshal(gateway.paymentLinks[0])
	require.NoError(t, err)
	assert.NotContains(t, string(body), "customer_email")
}

// Only the email became optional. Everything else a payment cannot be issued without is
// still refused before the gateway is called.
func TestValidateGeneratePaymentRequest_StillRefusesWhatAPaymentNeeds(t *testing.T) {
	client := &LedgerClient{}
	require.NoError(t, client.validateGeneratePaymentRequest(paymentRequest("QRIS", "")))

	for field, clear := range map[string]func(*GeneratePaymentRequest){
		"seller_account_id": func(r *GeneratePaymentRequest) { r.SellerAccountID = "" },
		"buyer_account_id":  func(r *GeneratePaymentRequest) { r.BuyerAccountID = "" },
		"buyer_name":        func(r *GeneratePaymentRequest) { r.BuyerName = "" },
		"product_id":        func(r *GeneratePaymentRequest) { r.ProductID = "" },
		"seller_price":      func(r *GeneratePaymentRequest) { r.SellerPrice = 0 },
		"currency":          func(r *GeneratePaymentRequest) { r.Currency = "" },
	} {
		t.Run(field, func(t *testing.T) {
			request := paymentRequest("QRIS", "")
			clear(request)

			err := client.validateGeneratePaymentRequest(request)

			require.Error(t, err)
			assert.Contains(t, err.Error(), field)
		})
	}
}
