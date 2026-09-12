package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

// resolveFeeAdjustment decides how much of a seller's and the platform's money becomes
// withdrawable, so every case here is a money case. The rules are
// docs/104-fee-mismatch-reconciliation.md.
func TestResolveFeeAdjustment(t *testing.T) {
	// A transaction priced at seller 10,000 + platform 500 + gateway 1,000.
	newTx := func(model domain.FeeModel, sellerNet, platformFee, gatewayFee int64) *domain.ProductTransaction {
		return &domain.ProductTransaction{
			SellerAccountID: "seller-account",
			Fee: domain.FeeBreakdown{
				SellerPrice:     sellerNet,
				SellerNetAmount: sellerNet,
				PlatformFee:     platformFee,
				GatewayFee:      gatewayFee,
				FeeModel:        model,
			},
		}
	}

	settledWithFee := func(fee int64) domain.SettledTransaction {
		return domain.SettledTransaction{Fee: fee, FeeReported: true}
	}

	t.Run("fee matches the price: nothing moves", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(1000))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(0), adj.FeeDelta)
		assert.Equal(t, int64(10000), adj.SellerNet)
		assert.Equal(t, int64(500), adj.PlatformFee)
	})

	t.Run("gateway overcharged: the platform absorbs it under GATEWAY_ON_CUSTOMER", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(1200))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(200), adj.FeeDelta)
		// The seller is untouched: they were promised their full price.
		assert.Equal(t, int64(10000), adj.SellerNet)
		assert.Equal(t, int64(300), adj.PlatformFee)
	})

	t.Run("gateway overcharged: the seller absorbs it under GATEWAY_ON_SELLER", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnSeller, 9000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(1200))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(200), adj.FeeDelta)
		assert.Equal(t, int64(8800), adj.SellerNet)
		// The platform keeps its whole fee under this model.
		assert.Equal(t, int64(500), adj.PlatformFee)
	})

	t.Run("gateway undercharged: the surplus does not inflate either settled amount", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(800))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(-200), adj.FeeDelta)
		// Both parties settle at the priced amounts; the 200 surplus is credited to the
		// seller as its own terminal entry, not folded in here.
		assert.Equal(t, int64(10000), adj.SellerNet)
		assert.Equal(t, int64(500), adj.PlatformFee)
	})

	t.Run("overcharge exactly consumes the platform fee: still bookable", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(1500))

		assert.Empty(t, blocked, "a platform fee of exactly zero is absorbable, not a block")
		assert.Equal(t, int64(0), adj.PlatformFee)
	})

	t.Run("overcharge exceeds the platform fee: blocked, not clamped", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(1600))

		assert.NotEmpty(t, blocked, "the platform would owe money it never charged")
		assert.Negative(t, adj.PlatformFee, "the caller must not be handed a clamped value it could book")
	})

	t.Run("overcharge exceeds the seller's whole share: blocked", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnSeller, 9500, 500, 500)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(11000))

		assert.NotEmpty(t, blocked, "the seller would receive negative proceeds")
		assert.Negative(t, adj.SellerNet)
	})

	t.Run("an unknown fee model absorbs on the platform, never on the seller", func(t *testing.T) {
		tx := newTx(domain.FeeModel("SOMETHING_NEW"), 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFee(1200))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(10000), adj.SellerNet, "a seller must never absorb a fee because of an unrecognised model")
		assert.Equal(t, int64(300), adj.PlatformFee)
	})
}

// The absorbing account decides whose PENDING balance carries a write-off, so naming the
// wrong one takes money from the wrong party.
func TestFeeAdjustmentAbsorbingAccount(t *testing.T) {
	const platformUUID = "platform-account"

	t.Run("GATEWAY_ON_CUSTOMER absorbs on the platform", func(t *testing.T) {
		tx := &domain.ProductTransaction{
			SellerAccountID: "seller-account",
			Fee:             domain.FeeBreakdown{FeeModel: domain.FeeModelGatewayOnCustomer},
		}

		assert.Equal(t, platformUUID, feeAdjustment{}.AbsorbingAccountUUID(tx, platformUUID))
	})

	t.Run("GATEWAY_ON_SELLER absorbs on the seller", func(t *testing.T) {
		tx := &domain.ProductTransaction{
			SellerAccountID: "seller-account",
			Fee:             domain.FeeBreakdown{FeeModel: domain.FeeModelGatewayOnSeller},
		}

		assert.Equal(t, "seller-account", feeAdjustment{}.AbsorbingAccountUUID(tx, platformUUID))
	})
}
