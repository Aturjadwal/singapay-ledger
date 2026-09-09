package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
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
// test that walks a whole sale through it without a gateway in sight: payment credits
// PENDING, settlement moves PENDING to AVAILABLE, and the two must net to zero on the
// pending side.
func TestLedgerEntries_PaymentThroughSettlement(t *testing.T) {
	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	platformAcc := createTestAccount(domain.OwnerTypePlatform, domain.OWNER_TYPE_PLATFORM, "01PLATFORMACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, platformAcc))
	sellerAcc := createTestAccount(domain.OwnerTypeSeller, "seller-1", "01SELLERACCOUNTULID")
	require.NoError(t, fakes.Account().Save(ctx, sellerAcc))

	fee, err := domain.NewFeeBreakdown(50000, 1000, 4995, domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)
	require.NoError(t, err)
	tx := createTestProductTransaction("INV-001", sellerAcc.UUID, fee)
	require.NoError(t, fakes.ProductTransaction().Save(ctx, tx))

	// Payment: the seller's share lands in PENDING.
	journal := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, tx.UUID, nil)
	require.NoError(t, fakes.Journal().Save(ctx, journal))

	pendingEntry := &domain.LedgerEntry{
		JournalUUID:   journal.UUID,
		AccountUUID:   sellerAcc.UUID,
		Amount:        46005,
		BalanceBucket: domain.BalanceBucketPending,
		EntryType:     domain.EntryTypeProductPayment,
		SourceType:    domain.SourceTypeProductTransaction,
		SourceID:      tx.UUID,
	}
	redifu.InitRecord(pendingEntry)
	require.NoError(t, fakes.LedgerEntry().Save(ctx, pendingEntry))

	// Settlement: a batch covering a window, and the item that matched inside it.
	batch, err := domain.NewSettlementBatch(
		platformAcc.UUID,
		"SP-SETTLE-1",
		"SETTLE/2026/001",
		time.Now().Add(-24*time.Hour),
		time.Now(),
		time.Now(),
		"settlement-webhook",
		domain.CurrencyIDR,
	)
	require.NoError(t, err)
	require.NoError(t, fakes.SettlementBatch().Save(ctx, batch))

	// Net credited to the sub-account is total_charged - gateway_fee = 51000 - 4995,
	// which is exactly seller_net + platform_fee.
	item, err := domain.NewSettlementItem(batch.UUID, domain.SettledTransaction{
		MerchantReference:    "INV-001",
		GatewayTransactionID: "SP-TX-1",
		GatewayAccountID:     "01SELLERACCOUNTULID",
		PaymentChannel:       "VA_BCA",
		GrossAmount:          51000,
		NetAmount:            46005,
		Fee:                  4995,
		FeeReported:          true,
	})
	require.NoError(t, err)
	require.NoError(t, item.MatchToTransaction(tx))
	require.NoError(t, fakes.SettlementItem().Save(ctx, item))

	settlementJournal := domain.NewJournal(domain.EventTypeSettlement, domain.SourceTypeSettlementBatch, batch.UUID, nil)
	require.NoError(t, fakes.Journal().Save(ctx, settlementJournal))
	require.NoError(t, fakes.LedgerEntry().SaveBatch(ctx,
		domain.NewSettlementEntriesForAccount(settlementJournal.UUID, tx.UUID, sellerAcc.UUID, 46005)))

	pending, available, err := fakes.LedgerEntry().GetAllBalances(ctx, sellerAcc.UUID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pending, "settlement must clear what payment put in PENDING")
	assert.Equal(t, int64(46005), available, "the settled net is seller_price + platform_fee")

	retrievedItems, err := fakes.SettlementItem().GetByProductTransactionID(ctx, tx.UUID)
	require.NoError(t, err)
	require.Len(t, retrievedItems, 1)
	assert.False(t, retrievedItems[0].HasAmountDiscrepancy())
	assert.True(t, retrievedItems[0].FeeReported,
		"a VA row reports its own fee, so the delta against the expected fee is meaningful")
}

// A payment-link row carries no fee anywhere in Singapay's API, so its AllocatedFee is a
// fallback rather than a fact. FeeReported is what stops that being mistaken for a perfect
// reconciliation.
func TestSettlementItem_PaymentLinkFeeIsNotReported(t *testing.T) {
	item, err := domain.NewSettlementItem("batch-1", domain.SettledTransaction{
		MerchantReference: "INV-002",
		PaymentChannel:    ChannelPaymentLink,
		GrossAmount:       51000,
		NetAmount:         46005,
		Fee:               4995, // copied from what was expected, not observed
		FeeReported:       false,
	})
	require.NoError(t, err)
	assert.False(t, item.FeeReported)
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

// A batch with no window can never be reconciled or audited later — Singapay sends no list
// of the transactions it covered, so the window is the only handle on them.
func TestNewSettlementBatch_RequiresAWindow(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour)
	to := time.Now()

	_, err := domain.NewSettlementBatch("ledger-1", "SP-1", "ref", time.Time{}, to, to, "webhook", domain.CurrencyIDR)
	assert.Error(t, err, "a batch without settle_from must be refused")

	_, err = domain.NewSettlementBatch("ledger-1", "SP-1", "ref", from, time.Time{}, to, "webhook", domain.CurrencyIDR)
	assert.Error(t, err, "a batch without settle_to must be refused")

	_, err = domain.NewSettlementBatch("ledger-1", "SP-1", "ref", to, from, to, "webhook", domain.CurrencyIDR)
	assert.Error(t, err, "an inverted window must be refused")

	_, err = domain.NewSettlementBatch("ledger-1", "", "ref", from, to, to, "webhook", domain.CurrencyIDR)
	assert.Error(t, err, "a batch without a gateway settlement id has no idempotency key")

	batch, err := domain.NewSettlementBatch("ledger-1", "SP-1", "ref", from, to, to, "webhook", domain.CurrencyIDR)
	require.NoError(t, err)
	assert.Equal(t, "SP-1", batch.BatchID)
	assert.Equal(t, domain.SettlementBatchStatusPending, batch.ProcessingStatus)
}

// Reconciliation is deliberately unimplemented. It must say so loudly rather than quietly
// booking nothing, because a settlement that silently moves no balances is indistinguishable
// from one with nothing in it.
func TestProcessReconciliation_RefusesUntilImplemented(t *testing.T) {
	gw := &fakeGateway{}
	client, _, _ := newPayoutTestClient(t, gw, 0)

	resp, err := client.ProcessReconciliation(context.Background(), &ReconciliationRequest{
		SettlementID: "SP-SETTLE-1",
		SettleFrom:   time.Now().Add(-24 * time.Hour),
		SettleTo:     time.Now(),
		InitiatedBy:  "settlement-webhook",
	})

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ErrReconciliationNotImplemented),
		"expected the not-implemented answer, got: %v", err)
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

func TestValidateSubAccountEmail(t *testing.T) {
	assert.NoError(t, validateSubAccountEmail("seller@example.com"))
	assert.Error(t, validateSubAccountEmail(""), "an account nobody can open in the dashboard is unsupportable")
	assert.Error(t, validateSubAccountEmail("not-an-email"))
}
