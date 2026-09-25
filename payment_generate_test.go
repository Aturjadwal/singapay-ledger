package ledger

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
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

	// catalogue is what ListPaymentMethods answers; catalogueReads counts the calls.
	catalogue      []singapay.PaymentMethod
	catalogueReads int
}

func (g *moneyInGateway) ListPaymentMethods(context.Context) ([]singapay.PaymentMethod, error) {
	g.catalogueReads++
	return g.catalogue, nil
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
// they make is implemented, and it filters on IsActive the way the real query does.
type activeFeeConfigs struct {
	domain.FeeConfigRepository
	configs []*domain.FeeConfig
}

func (f activeFeeConfigs) GetAllActive(context.Context) ([]*domain.FeeConfig, error) {
	var active []*domain.FeeConfig
	for _, cfg := range f.configs {
		if cfg.IsActive {
			active = append(active, cfg)
		}
	}
	return active, nil
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

	gateway := &moneyInGateway{
		fakeGateway: &fakeGateway{},
		// The shape of a real catalogue: card methods sit beside the rest, under their own
		// group, and more than one acquirer can be behind them.
		catalogue: []singapay.PaymentMethod{
			{Code: "VA_BCA", Name: "VA BCA", Group: "va"},
			{Code: "QRIS", Name: "QRIS", Group: "qris"},
			{Code: "CREDIT_CARD", Name: "Credit Card", Group: "card"},
			{Code: "NICEPAY_CARD", Name: "Kartu Kredit / Debit", Group: "Card"},
			{Code: "EWALLET_DANA", Name: "DANA", Group: "ewallet"},
		},
	}

	return &LedgerClient{
		txProvider: NewFakeTransactionProvider(fakes),
		repoProvider: withFeeConfigs{
			FakeRepositoryProvider: fakes,
			fees: activeFeeConfigs{configs: []*domain.FeeConfig{
				{ConfigType: domain.FeeConfigTypePlatform, PaymentChannel: "PLATFORM", FeeType: domain.FeeTypeFixed, FixedAmount: 1000, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "VA_BCA", FeeType: domain.FeeTypeFixed, FixedAmount: 4000, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "QRIS", FeeType: domain.FeeTypePercentage, Percentage: 0.7, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "EWALLET_DANA", FeeType: domain.FeeTypePercentage, Percentage: 1.5, IsActive: true},
				{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: ChannelCreditCard, FeeType: domain.FeeTypeHybrid, FixedAmount: 2000, Percentage: 3.0, IsActive: true},
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
	// Singapay takes the name and the email together or not at all, so without the email
	// the name has to stay out too.
	assert.NotContains(t, string(body), "customer_name")
}

// A card is paid through a payment link pinned to every method the catalogue files under
// "card". Singapay's hosted page takes the card number, so it never reaches the ledger.
func TestGeneratePayment_ACardIsAPaymentLinkPinnedToTheCardMethods(t *testing.T) {
	client, gateway := newPaymentTestClient(t)

	response, err := client.GeneratePayment(context.Background(), paymentRequest(ChannelCreditCard, "andi@example.com"))
	require.NoError(t, err)

	assert.Equal(t, ChannelCreditCard, response.PaymentChannel)
	assert.Equal(t, "https://pay.example/link", response.PaymentURL)
	assert.Empty(t, response.PaymentCode)
	// 100,000 + 1,000 platform, grossed up for 2,000 + 3%: (101,000 + 2,000) / 0.97.
	assert.Equal(t, int64(106186), response.TotalCharged)
	assert.Equal(t, int64(5186), response.GatewayFee)

	require.Len(t, gateway.paymentLinks, 1)
	link := gateway.paymentLinks[0]
	assert.Equal(t, []string{"CREDIT_CARD", "NICEPAY_CARD"}, link.WhitelistedPaymentMethod,
		"every card method, matched on the group whatever its case, and nothing else")
	assert.Equal(t, response.TotalCharged, link.TotalAmount)
	assert.Equal(t, 1, gateway.issued())
}

// An unpinned link stays unpinned: the whitelist is only for cards.
func TestGeneratePayment_APaymentLinkIsNotPinned(t *testing.T) {
	client, gateway := newPaymentTestClient(t)

	_, err := client.GenerateSubscriptionPayment(context.Background(), &GenerateSubscriptionPaymentRequest{
		BuyerAccountID:    "user-1",
		BuyerName:         "Monika",
		BuyerEmail:        "monika@example.com",
		ProductID:         "package-1",
		SubscriptionPrice: 99000,
		Currency:          string(domain.CurrencyIDR),
	})
	require.NoError(t, err)

	require.Len(t, gateway.paymentLinks, 1)
	assert.Empty(t, gateway.paymentLinks[0].WhitelistedPaymentMethod)
	assert.Zero(t, gateway.catalogueReads)
}

// Refused before Singapay is called. A card link under the minimum would be refused at
// creation with a validation error about the amount, which names nothing a payer can act on.
func TestGeneratePayment_ACardBelowTheMinimumIsRefusedBeforeTheGateway(t *testing.T) {
	client, gateway := newPaymentTestClient(t)
	request := paymentRequest(ChannelCreditCard, "andi@example.com")
	// 3,000 + 1,000 platform, grossed up for the card fee: 6,186.
	request.SellerPrice = 3000

	_, err := client.GeneratePayment(context.Background(), request)

	require.Error(t, err)
	assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeInvalidRequest, err))
	assert.Equal(t, "a card payment must be at least Rp10000; this one is Rp6186", err.Error())
	assert.Zero(t, gateway.issued())
	assert.Zero(t, gateway.catalogueReads)
}

// A merchant whose catalogue has no card group does not take cards, and has to be told so:
// an unpinned link would offer the payer every other channel at a card price.
func TestGeneratePayment_ACardIsRefusedWhenTheCatalogueHasNoCardMethod(t *testing.T) {
	client, gateway := newPaymentTestClient(t)
	gateway.catalogue = []singapay.PaymentMethod{
		{Code: "VA_BCA", Name: "VA BCA", Group: "va"},
		{Code: "QRIS", Name: "QRIS", Group: "qris"},
	}

	_, err := client.GeneratePayment(context.Background(), paymentRequest(ChannelCreditCard, "andi@example.com"))

	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrUnsupportedPaymentChannel))
	assert.Zero(t, gateway.issued())
}

// Singapay takes a payment link's customer fields as a set: once one is sent, the name and
// the email are both required. A payer with no email gets no pre-fill at all.
func TestGeneratePayment_APaymentLinkPrefillsTheCustomerOnlyAsASet(t *testing.T) {
	t.Run("without an email nothing is pre-filled", func(t *testing.T) {
		client, gateway := newPaymentTestClient(t)
		request := paymentRequest(ChannelCreditCard, "")
		request.CustomerPhone = "081234567890"

		_, err := client.GeneratePayment(context.Background(), request)
		require.NoError(t, err)

		require.Len(t, gateway.paymentLinks, 1)
		body, err := json.Marshal(gateway.paymentLinks[0])
		require.NoError(t, err)
		assert.NotContains(t, string(body), "customer_name")
		assert.NotContains(t, string(body), "customer_email")
		assert.NotContains(t, string(body), "customer_phone")
	})

	t.Run("with an email the name, email and phone go together", func(t *testing.T) {
		client, gateway := newPaymentTestClient(t)
		request := paymentRequest(ChannelCreditCard, "andi@example.com")
		request.CustomerPhone = "081234567890"

		_, err := client.GeneratePayment(context.Background(), request)
		require.NoError(t, err)

		require.Len(t, gateway.paymentLinks, 1)
		link := gateway.paymentLinks[0]
		assert.Equal(t, "Andi", link.CustomerName)
		assert.Equal(t, "andi@example.com", link.CustomerEmail)
		assert.Equal(t, "081234567890", link.CustomerPhone)
	})
}

// The estimate prices a card like any other channel, from its fee config.
func TestCalculateFeesForCustomer_PricesACard(t *testing.T) {
	client, _ := newPaymentTestClient(t)

	estimate, err := client.CalculateFeesForCustomer(context.Background(), 100000, ChannelCreditCard, string(domain.CurrencyIDR), 1)

	require.NoError(t, err)
	assert.Equal(t, int64(106186), estimate.TotalCharged)
	assert.Equal(t, int64(5186), estimate.GatewayFee)
}

// A channel with no active fee config cannot be paid, so it cannot be priced either.
// Before, it was quoted with a gateway fee of zero — cheaper than any channel that works.
func TestCalculateFeesForCustomer_RefusesAChannelWithNoActiveFeeConfig(t *testing.T) {
	client, _ := newPaymentTestClient(t)

	_, err := client.CalculateFeesForCustomer(context.Background(), 100000, "VA_TIDAKADA", string(domain.CurrencyIDR), 1)

	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrUnsupportedPaymentChannel))
}

// The list payers choose from is the list that can be paid: an inactive row is refused by
// the estimate and by GeneratePayment, so it must not be offered either.
func TestGetPaymentChannelFeeConfigs_ListsOnlyActiveChannels(t *testing.T) {
	client, _ := newPaymentTestClient(t)
	provider := client.repoProvider.(withFeeConfigs)
	provider.fees.configs = append(provider.fees.configs,
		&domain.FeeConfig{ConfigType: domain.FeeConfigTypeGateway, PaymentChannel: "VA_PERMATA", FeeType: domain.FeeTypeFixed, FixedAmount: 2500, IsActive: false})
	client.repoProvider = provider

	configs, err := client.GetPaymentChannelFeeConfigs(context.Background())

	require.NoError(t, err)
	var channels []string
	for _, cfg := range configs {
		channels = append(channels, cfg.PaymentChannel)
	}
	assert.ElementsMatch(t, []string{"VA_BCA", "QRIS", "EWALLET_DANA", ChannelCreditCard}, channels,
		"no PLATFORM row, and no inactive one")
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
