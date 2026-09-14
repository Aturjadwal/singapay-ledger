package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

// resolveFeeAdjustment decides how much of a seller's and the platform's money becomes
// withdrawable, so every case here is a money case. The rules are
// docs/104-fee-mismatch-reconciliation.md.
//
// Two of them carry the weight:
//
//   - The seller is paid what they were priced. SellerNet is the priced figure in every
//     case below, including the ones where Singapay charged more or less than estimated.
//   - The platform sub-account balances the difference in both directions, and the
//     difference is usually a fraction of a rupiah — so the arithmetic is in sen.
//
// The fixtures build SettledTransaction through settledWithFeeMinor rather than by hand.
// An earlier version of this file constructed it inline in whole rupiah, which is how a
// ×100 unit mismatch between this struct and FeeBreakdown survived in readSettledTransaction
// with a green test suite: the bug lived at a boundary the test never crossed.
func TestResolveFeeAdjustment(t *testing.T) {
	// A transaction priced at seller 10,000 + platform 500 + gateway 1,000, in rupiah —
	// FeeBreakdown is whole rupiah, as every figure quoted at checkout is.
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

	// SettledTransaction is sen, always.
	settledWithFeeMinor := func(feeMinor int64) domain.SettledTransaction {
		return domain.SettledTransaction{FeeMinor: feeMinor, FeeReported: true}
	}

	t.Run("fee matches the estimate: nothing moves", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(100000))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(0), adj.FeeDeltaMinor)
		assert.Equal(t, int64(10000), adj.SellerNet)
		assert.Equal(t, int64(50000), adj.PlatformFeeMinor)
		assert.Equal(t, int64(0), adj.PlatformResidualMinor())
	})

	t.Run("gateway overcharged by whole rupiah: the platform absorbs it", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(120000))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(20000), adj.FeeDeltaMinor)
		assert.Equal(t, int64(10000), adj.SellerNet, "the seller was promised their full price")
		assert.Equal(t, int64(30000), adj.PlatformFeeMinor)
	})

	t.Run("gateway undercharged by whole rupiah: the residual is the platform's", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(80000))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(-20000), adj.FeeDeltaMinor)
		assert.Equal(t, int64(10000), adj.SellerNet,
			"a surplus is not the seller's either — their net is what they were priced, both ways")
		assert.Equal(t, int64(70000), adj.PlatformFeeMinor,
			"the platform collects the 200 Singapay did not take")
	})

	// The case the whole mechanism exists for. Singapay's money-in fee has two decimals,
	// so the delta against a whole-rupiah estimate is almost never whole.
	t.Run("gateway undercharged by sen: the fraction lands on the platform", func(t *testing.T) {
		// Estimated 120, actual 119.84 — the residual is 0.16.
		tx := newTx(domain.FeeModelGatewayOnCustomer, 13000, 4000, 120)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(11984))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(-16), adj.FeeDeltaMinor)
		assert.Equal(t, int64(13000), adj.SellerNet,
			"the seller's net must not move by a fraction of a rupiah")
		assert.Equal(t, int64(400016), adj.PlatformFeeMinor, "4000.16 in sen")
		assert.Equal(t, int64(4000), adj.PlatformFeeRupiah(),
			"the ledger entry can only carry the whole rupiah")
		assert.Equal(t, int64(16), adj.PlatformResidualMinor(),
			"and the 16 sen it cannot carry has to be recorded, not dropped")
	})

	t.Run("gateway overcharged by sen: the platform absorbs the fraction", func(t *testing.T) {
		// Estimated 120, actual 120.30 — the shortfall is 0.30.
		tx := newTx(domain.FeeModelGatewayOnCustomer, 13000, 4000, 120)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(12030))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(30), adj.FeeDeltaMinor)
		assert.Equal(t, int64(13000), adj.SellerNet)
		assert.Equal(t, int64(399970), adj.PlatformFeeMinor, "3999.70 in sen")
		assert.Equal(t, int64(3999), adj.PlatformFeeRupiah())
		assert.Equal(t, int64(70), adj.PlatformResidualMinor())
	})

	t.Run("the residual is the part the rupiah entry drops, never a rounding", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 13000, 4000, 120)

		// 119.99 -> delta -1 sen -> platform 4000.01
		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(11999))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(400001), adj.PlatformFeeMinor)
		assert.Equal(t, int64(4000), adj.PlatformFeeRupiah(),
			"0.01 must not round the entry up to 4001 — the sweep moves 4000.01, the entry books 4000")
		assert.Equal(t, int64(1), adj.PlatformResidualMinor())
		assert.Equal(t, adj.PlatformFeeMinor,
			domain.RupiahToMinor(adj.PlatformFeeRupiah())+adj.PlatformResidualMinor(),
			"the entry and the residual must reconstruct the settled fee exactly")
	})

	t.Run("overcharge exactly consumes the platform fee: still bookable", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(150000))

		assert.Empty(t, blocked, "a platform fee of exactly zero is absorbable, not a block")
		assert.Equal(t, int64(0), adj.PlatformFeeMinor)
	})

	t.Run("overcharge exceeds the platform fee: blocked, not clamped", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(160000))

		assert.NotEmpty(t, blocked, "the platform would owe money it never charged")
		assert.Negative(t, adj.PlatformFeeMinor, "the caller must not be handed a clamped value it could book")
	})

	t.Run("a blocked settlement still leaves the seller's net untouched", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnCustomer, 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(160000))

		assert.NotEmpty(t, blocked)
		assert.Equal(t, int64(10000), adj.SellerNet,
			"blocking is a decision about the platform's share; it never reprices the seller")
	})

	t.Run("GATEWAY_ON_SELLER: the seller's net is still the priced one", func(t *testing.T) {
		// This model is subscriptions, where the platform is itself the beneficiary and
		// the platform fee is skipped. Its delta is zero by construction — a payment link
		// reports no fee — so the estimate is copied to the actual and nothing to absorb
		// ever arises.
		tx := newTx(domain.FeeModelGatewayOnSeller, 9000, 0, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(100000))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(0), adj.FeeDeltaMinor)
		assert.Equal(t, int64(9000), adj.SellerNet)
		assert.Equal(t, int64(0), adj.PlatformFeeMinor)
	})

	t.Run("GATEWAY_ON_SELLER with no platform fee cannot absorb: blocked, not taken from the seller", func(t *testing.T) {
		tx := newTx(domain.FeeModelGatewayOnSeller, 9000, 0, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(120000))

		assert.NotEmpty(t, blocked, "there is no platform fee to absorb a delta out of")
		assert.Equal(t, int64(9000), adj.SellerNet,
			"a loud block beats quietly repricing a seller who was promised this net")
	})

	t.Run("an unknown fee model absorbs on the platform, never on the seller", func(t *testing.T) {
		tx := newTx(domain.FeeModel("SOMETHING_NEW"), 10000, 500, 1000)

		adj, blocked := resolveFeeAdjustment(tx, settledWithFeeMinor(120000))

		assert.Empty(t, blocked)
		assert.Equal(t, int64(10000), adj.SellerNet, "a seller must never absorb a fee because of an unrecognised model")
		assert.Equal(t, int64(30000), adj.PlatformFeeMinor)
	})
}

// formatMinor renders sen for humans, and it is read off log lines and block reasons during
// an incident. Getting the padding wrong turns 0.05 into 0.5 on the one line somebody is
// squinting at to decide whether money is missing.
func TestFormatMinor(t *testing.T) {
	cases := map[int64]string{
		0:      "0.00",
		1:      "0.01",
		5:      "0.05",
		16:     "0.16",
		100:    "1.00",
		11984:  "119.84",
		400016: "4000.16",
		-30:    "-0.30",
		-11984: "-119.84",
	}

	for minor, want := range cases {
		assert.Equal(t, want, formatMinor(minor), "formatMinor(%d)", minor)
	}
}
