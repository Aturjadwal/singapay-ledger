package ledger

import (
	"context"
	"testing"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/singapay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestAccount(ownerType domain.OwnerType, ownerID, singapayAccountID string) *domain.Account {
	acc := domain.NewAccount(ownerType, singapayAccountID, ownerID, domain.CurrencyIDR)
	return &acc
}

func createTestProductTransaction(invoiceNumber, sellerAccountID string, fee *domain.FeeBreakdown) *domain.ProductTransaction {
	tx := domain.NewProductTransaction(
		"buyer-123",
		sellerAccountID,
		"product-456",
		"PHOTO",
		invoiceNumber,
		*fee,
		nil,
	)
	tx.MarkCompleted()
	return tx
}

// The double-entry model is what survives the gateway change untouched, so it is worth a
// test that walks a whole sale through it: payment credits PENDING, settlement moves
// PENDING to AVAILABLE, and the two must net to zero on the pending side.
//
// The settlement half runs through bookSettlement rather than hand-built entries, so the
// invariant is asserted against the code that actually writes them — including the
// COMPLETED -> SETTLED compare-and-set and the settled fee figures it records.
func TestLedgerEntries_PaymentThroughSettlement(t *testing.T) {
	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	platformAcc := createTestAccount(domain.OwnerTypePlatform, "platform", "01PLATFORMACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, platformAcc))
	gatewayAcc := createTestAccount(domain.OwnerTypePaymentGateway, "SINGAPAY", "01GATEWAYACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, gatewayAcc))
	sellerAcc := createTestAccount(domain.OwnerTypeSeller, "seller-1", "01SELLERACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, sellerAcc))

	fee, err := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
	require.NoError(t, err)
	tx := createTestProductTransaction("INV-001", sellerAcc.UUID, fee)
	require.NoError(t, fakes.ProductTransaction().Save(ctx, tx))

	// Payment: every party's share lands in PENDING, exactly as the money-in webhook
	// books it.
	journal := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(t, fakes.Journal().Save(ctx, journal))
	require.NoError(t, fakes.LedgerEntry().SaveBatch(ctx, domain.NewPaymentEntries(
		journal.UUID,
		tx.UUID,
		sellerAcc.UUID, fee.SellerNetAmount,
		platformAcc.UUID, fee.PlatformFee,
		gatewayAcc.UUID, fee.GatewayFee,
	)))

	client := &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		logger:       testLogger(),
	}

	// Settlement: Singapay reports the transaction settled, and the fee it really took
	// matches the one priced, so there is no adjustment to absorb. Net credited to the
	// sub-account is total_charged - gateway_fee = 51000 - 4995 = seller_net + platform_fee.
	outcome, err := client.bookSettlement(ctx, tx, domain.SettledTransaction{
		MerchantReference:    "INV-001",
		GatewayTransactionID: "SP-TX-1",
		GatewayAccountID:     "01SELLERACCOUNTULID",
		PaymentChannel:       "VA_BCA",
		GrossMinor:           5100000,
		NetMinor:             4600500,
		FeeMinor:             499500,
		FeeReported:          true,
	})
	require.NoError(t, err)
	assert.Equal(t, settleOutcomeSettled, outcome)

	sellerPending, sellerAvailable, err := fakes.LedgerEntry().GetAllBalances(ctx, sellerAcc.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), sellerPending, "settlement must clear what payment put in PENDING")
	assert.Equal(t, int64(45005), sellerAvailable, "the seller's share is price minus the gateway fee they absorb")

	platformPending, platformAvailable, err := fakes.LedgerEntry().GetAllBalances(ctx, platformAcc.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), platformPending)
	assert.Equal(t, int64(1000), platformAvailable, "the platform fee settles alongside the seller's share")

	// The gateway expense account is cleared, not credited: that money left for Singapay.
	gatewayPending, gatewayAvailable, err := fakes.LedgerEntry().GetAllBalances(ctx, gatewayAcc.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), gatewayPending, "the gateway fee booked at payment time must be cleared")
	assert.Equal(t, int64(0), gatewayAvailable)

	settled, err := fakes.ProductTransaction().GetByID(ctx, tx.UUID)
	require.NoError(t, err)
	assert.Equal(t, domain.TransactionStatusSettled, settled.Status)
	require.NotNil(t, settled.SettledGatewayFeeMinor)
	assert.Equal(t, int64(499500), *settled.SettledGatewayFeeMinor,
		"the fee Singapay actually took is what the transfer step must read, in sen")
	require.NotNil(t, settled.PlatformResidualMinor)
	assert.Equal(t, int64(0), *settled.PlatformResidualMinor,
		"a fee that matches the estimate to the rupiah strands no fraction")
}

// A payment-link row carries no fee anywhere in Singapay's API, so its Fee is a fallback
// copied from what was expected rather than a fact. That makes the delta zero by
// construction — a perfect reconciliation that reconciled nothing. FeeReported is the only
// thing that tells the two apart, so it has to survive into the journal the settlement
// writes.
func TestResolveFeeAdjustment_PaymentLinkFeeIsNotReported(t *testing.T) {
	tx := &domain.ProductTransaction{
		SellerAccountID: "seller-account",
		Fee: domain.FeeBreakdown{
			SellerPrice:     50000,
			SellerNetAmount: 45005,
			PlatformFee:     1000,
			GatewayFee:      4995,
			FeeModel:        domain.FeeModelGatewayOnSeller,
		},
	}

	settled := domain.SettledTransaction{
		MerchantReference: "INV-002",
		PaymentChannel:    ChannelPaymentLink,
		GrossMinor:        5100000,
		NetMinor:          4600500,
		FeeMinor:          499500, // copied from what was expected, not observed
		FeeReported:       false,
	}

	adj, blocked := resolveFeeAdjustment(tx, settled)

	assert.Empty(t, blocked)
	assert.Equal(t, int64(0), adj.FeeDeltaMinor,
		"a copied fee can only ever produce a zero delta")
	assert.Equal(t, tx.Fee.SellerNetAmount, adj.SellerNet)
	assert.Equal(t, domain.RupiahToMinor(tx.Fee.PlatformFee), adj.PlatformFeeMinor)
	assert.False(t, settled.FeeReported,
		"the zero delta above must stay distinguishable from a real reconciliation")
}

func TestFeeBreakdown_FeeModels(t *testing.T) {
	tests := []struct {
		name                 string
		sellerPrice          int64
		platformFee          int64
		gatewayFee           int64
		feeModel             domain.FeeModel
		expectedTotalCharged int64
		expectedSellerNet    int64
	}{
		{
			name:                 "GATEWAY_ON_CUSTOMER - customer pays all",
			sellerPrice:          50000,
			platformFee:          1000,
			gatewayFee:           4995,
			feeModel:             domain.FeeModelGatewayOnCustomer,
			expectedTotalCharged: 55995, // 50000 + 1000 + 4995
			expectedSellerNet:    50000, // Seller gets full price
		},
		{
			name:                 "GATEWAY_ON_SELLER - seller absorbs gateway fee",
			sellerPrice:          50000,
			platformFee:          1000,
			gatewayFee:           4995,
			feeModel:             domain.FeeModelGatewayOnSeller,
			expectedTotalCharged: 51000, // 50000 + 1000 (no gateway fee shown to customer)
			expectedSellerNet:    45005, // 50000 - 4995 (platform fee tracked separately)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fee, err := domain.NewFeeBreakdown(tt.sellerPrice, tt.platformFee, tt.gatewayFee, domain.CurrencyIDR, tt.feeModel)
			require.NoError(t, err)

			assert.Equal(t, tt.expectedTotalCharged, fee.TotalCharged)
			assert.Equal(t, tt.expectedSellerNet, fee.SellerNetAmount)
			assert.Equal(t, tt.platformFee, fee.PlatformFee)
			assert.Equal(t, tt.gatewayFee, fee.GatewayFee)
		})
	}
}

// The channel code decides which Singapay product issues the payment, and getting it wrong
// is not a cosmetic error: only VA, QRIS and e-wallet report a per-transaction fee, so a
// mis-routed payment loses fee reconciliation permanently.
func TestPaymentChannelKind(t *testing.T) {
	tests := []struct {
		channel string
		want    channelKind
	}{
		{"QRIS", channelQRIS},
		{"VA_BCA", channelVirtualAccount},
		{"VA_MANDIRI", channelVirtualAccount},
		{"EWALLET_DANA", channelEwallet},
		{"", channelPaymentLink},
		{ChannelPaymentLink, channelPaymentLink},
		{ChannelCreditCard, channelCard},
		{"VIRTUAL_ACCOUNT_MANDIRI", channelUnknown},
		{"GOPAY", channelUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.channel, func(t *testing.T) {
			assert.Equal(t, tt.want, paymentChannelKind(tt.channel))
		})
	}
}

// Singapay's catalogue spells the channel "VA_BCA" but the create-VA body wants the bank
// alone. A channel naming a bank Singapay does not issue for has to be refused here, on the
// booking path, rather than becoming an opaque validation error from the API.
func TestVABankFromChannel(t *testing.T) {
	bank, err := vaBankFromChannel("VA_BCA")
	require.NoError(t, err)
	assert.Equal(t, singapay.BankBCA, bank)

	bank, err = vaBankFromChannel("VA_MANDIRI")
	require.NoError(t, err)
	assert.Equal(t, singapay.BankMandiri, bank)

	_, err = vaBankFromChannel("VA_NOTABANK")
	assert.Error(t, err)
}

// The merchant reference for a platform-fee transfer must be derived, never generated.
// Singapay's merchant_ref_no is unique per merchant across every account movement and
// re-sending a used one returns the original transfer without moving anything — which is
// what makes a retry safe. A random value per attempt would forfeit that silently.
func TestPlatformFeeTransferReference_IsDeterministic(t *testing.T) {
	first := platformFeeTransferReference("INV-20260101120000-ABC123")
	second := platformFeeTransferReference("INV-20260101120000-ABC123")

	assert.Equal(t, first, second)
	assert.NotEqual(t, "INV-20260101120000-ABC123", first,
		"the transfer reference must not collide with the payment it derives from")
	assert.Equal(t, "PF-INV-20260101120000-ABC123", first)
}

func TestSanitizeSubAccountName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Singapay accepts digits and punctuation, so a real business name survives intact.
		{"business name with punctuation and digits is kept", "PT. Maju 123", "PT. Maju 123"},
		{"whitespace is collapsed", "  Ria   Florensi  ", "Ria Florensi"},
		{"empty falls back to a default", "   ", defaultSubAccountName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeSubAccountName(tt.input))
		})
	}

	long := sanitizeSubAccountName(string(make([]byte, 0, 200)) + repeatString("a", 200))
	assert.LessOrEqual(t, len(long), maxSubAccountNameLength)
}

func repeatString(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
