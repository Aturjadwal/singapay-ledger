package ledger

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

// The invariant this whole fee-balancing mechanism exists for:
//
//	a seller's Singapay sub-account balance == a seller's ledger balance
//
// Everything else — sen arithmetic, the residual column, decimal transfers — is machinery
// in service of that one equality. These tests compute the two sides INDEPENDENTLY and
// compare them, rather than asserting on the adjustment's own fields. An adjustment that
// agrees with itself proves nothing; two balances computed down different paths that land
// on the same number proves the thing that matters.
//
// Run with -v to read the walkthrough.

// simulate replays one transaction end to end and reports both balances, in sen.
//
// The Singapay side is modelled the way the money actually moves, not the way the ledger
// records it:
//
//  1. the customer pays TotalCharged into the seller's sub-account
//  2. Singapay deducts its own fee before crediting — this is the decimal figure
//  3. ProcessPlatformFeeTransfer sweeps the settled platform fee out to the platform
//
// The ledger side is whatever bookSettlement would write.
type simulation struct {
	sellerPrice    int64 // rupiah, as priced at checkout
	platformFee    int64 // rupiah, as priced at checkout
	estimatedFee   int64 // rupiah, from fee_configs
	actualFeeMinor int64 // sen, what Singapay really took

	// Outcomes, all in sen.
	sellerSingapayMinor   int64
	sellerLedgerMinor     int64
	platformSingapayMinor int64
	platformLedgerMinor   int64
	residualMinor         int64
	blocked               string
}

func simulate(sellerPrice, platformFee, estimatedFee, actualFeeMinor int64) simulation {
	s := simulation{
		sellerPrice:    sellerPrice,
		platformFee:    platformFee,
		estimatedFee:   estimatedFee,
		actualFeeMinor: actualFeeMinor,
	}

	// GATEWAY_ON_CUSTOMER: the customer pays every fee on top of the seller's price.
	totalCharged := sellerPrice + platformFee + estimatedFee

	tx := &domain.ProductTransaction{
		SellerAccountID: "seller-account",
		Fee: domain.FeeBreakdown{
			SellerPrice:     sellerPrice,
			SellerNetAmount: sellerPrice,
			PlatformFee:     platformFee,
			GatewayFee:      estimatedFee,
			TotalCharged:    totalCharged,
			FeeModel:        domain.FeeModelGatewayOnCustomer,
		},
	}

	adj, blocked := resolveFeeAdjustment(tx, domain.SettledTransaction{
		FeeMinor:    actualFeeMinor,
		FeeReported: true,
	})
	s.blocked = blocked
	if blocked != "" {
		return s
	}

	// --- The Singapay side: real money, tracked in sen. ---

	// The payer's money lands in the seller's sub-account with Singapay's fee already off.
	sellerSubAccount := domain.RupiahToMinor(totalCharged) - actualFeeMinor

	// The sweep moves the settled platform fee across. The transfer endpoint takes a
	// decimal, so this moves exactly — fraction included.
	sweep := adj.PlatformFeeMinor
	sellerSubAccount -= sweep

	s.sellerSingapayMinor = sellerSubAccount
	s.platformSingapayMinor = sweep

	// --- The ledger side: what bookSettlement writes, in whole rupiah. ---

	s.sellerLedgerMinor = domain.RupiahToMinor(adj.SellerNet)
	s.platformLedgerMinor = domain.RupiahToMinor(adj.PlatformFeeRupiah())
	s.residualMinor = adj.PlatformResidualMinor()

	return s
}

func (s simulation) report() string {
	if s.blocked != "" {
		return fmt.Sprintf(
			"  actual fee %-9s BLOCKED: %s",
			formatMinor(s.actualFeeMinor), s.blocked)
	}
	return fmt.Sprintf(
		"  actual fee %-9s | sweep %-10s | seller: singapay %-10s ledger %-10s | platform: singapay %-10s ledger %-10s residual %s",
		formatMinor(s.actualFeeMinor),
		formatMinor(s.platformSingapayMinor),
		formatMinor(s.sellerSingapayMinor),
		formatMinor(s.sellerLedgerMinor),
		formatMinor(s.platformSingapayMinor),
		formatMinor(s.platformLedgerMinor),
		formatMinor(s.residualMinor),
	)
}

// The headline case, walked through so the numbers can be read off rather than trusted.
func TestSimulation_SellerBalancesMatchWhateverSingapayCharges(t *testing.T) {
	// Priced: seller 13,000 + platform 4,000 + estimated gateway 120 = 17,120 charged.
	const (
		sellerPrice  = 13000
		platformFee  = 4000
		estimatedFee = 120
	)

	cases := []struct {
		name           string
		actualFeeMinor int64
	}{
		{"estimate exact", 12000},                // 120.00
		{"Singapay charged 0.16 less", 11984},    // 119.84 — the case from the spec
		{"Singapay charged 0.30 more", 12030},    // 120.30
		{"Singapay charged 0.01 less", 11999},    // 119.99
		{"Singapay charged 0.99 less", 11901},    // 119.01
		{"Singapay charged a whole 5 more", 500}, // 5.00, far below estimate
		{"Singapay charged 50 more", 17000},      // 170.00
	}

	t.Logf("priced: seller %d + platform %d + estimated gateway %d = %d charged",
		sellerPrice, platformFee, estimatedFee, sellerPrice+platformFee+estimatedFee)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := simulate(sellerPrice, platformFee, estimatedFee, tc.actualFeeMinor)
			t.Log(s.report())

			require.Empty(t, s.blocked)

			// The invariant.
			assert.Equal(t, s.sellerSingapayMinor, s.sellerLedgerMinor,
				"the seller's Singapay balance and ledger balance must be the same number")

			// And the seller always ends at the priced net — not merely consistent, but
			// consistent at the figure the checkout promised.
			assert.Equal(t, domain.RupiahToMinor(sellerPrice), s.sellerLedgerMinor,
				"the seller is paid what they were priced, whatever Singapay charged")

			// The platform carries the whole difference, and the part its whole-rupiah
			// ledger entry cannot express is recorded rather than lost.
			assert.Equal(t, s.platformSingapayMinor, s.platformLedgerMinor+s.residualMinor,
				"platform ledger + residual must reconstruct what actually moved")
		})
	}
}

// One case proves nothing about the ones nobody thought to write down. This sweeps every
// sen the fee could plausibly land on and asserts the invariant across all of them.
func TestSimulation_InvariantHoldsAcrossEverySenValue(t *testing.T) {
	const (
		sellerPrice  = 13000
		platformFee  = 4000
		estimatedFee = 120
	)

	checked, blocked := 0, 0

	// 0.00 through 5,000.00, every single sen.
	for actual := int64(0); actual <= 500000; actual++ {
		s := simulate(sellerPrice, platformFee, estimatedFee, actual)

		if s.blocked != "" {
			blocked++
			// A block writes nothing at all, so there is no balance to diverge.
			// It may only happen once the overcharge exceeds the whole platform fee.
			require.Greater(t, actual, domain.RupiahToMinor(estimatedFee+platformFee),
				"blocked at %s, which the platform fee could still have absorbed", formatMinor(actual))
			continue
		}

		checked++

		require.Equal(t, domain.RupiahToMinor(sellerPrice), s.sellerSingapayMinor,
			"seller's Singapay balance drifted at actual fee %s", formatMinor(actual))
		require.Equal(t, s.sellerSingapayMinor, s.sellerLedgerMinor,
			"balances diverged at actual fee %s", formatMinor(actual))
		require.Equal(t, s.platformSingapayMinor, s.platformLedgerMinor+s.residualMinor,
			"platform residual did not reconcile at actual fee %s", formatMinor(actual))
		require.GreaterOrEqual(t, s.residualMinor, int64(0))
		require.Less(t, s.residualMinor, int64(domain.MinorPerRupiah),
			"a residual is a fraction of a rupiah by definition")
	}

	t.Logf("invariant held for %d fee values; %d blocked without writing anything", checked, blocked)
}

// What the ×100 bug did, stated as a test so the size of it is on the record rather than in
// a commit message. This does not exercise current code — it reproduces the old arithmetic
// to show what the fix was worth.
func TestSimulation_TheOldUnitBug(t *testing.T) {
	const (
		platformFee  = 4000
		estimatedFee = 120
	)

	actualFeeMinor := int64(11984) // 119.84, as Singapay reports it

	// The old code assigned .Minor() into a field it then compared against whole rupiah.
	oldDelta := actualFeeMinor - estimatedFee // 11984 - 120, sen minus rupiah
	oldPlatformFee := platformFee - oldDelta

	correctDelta := actualFeeMinor - domain.RupiahToMinor(estimatedFee)

	t.Logf("real fee                     : %s", formatMinor(actualFeeMinor))
	t.Logf("correct delta                : %s", formatMinor(correctDelta))
	t.Logf("delta the old code computed  : %d (treated as rupiah)", oldDelta)
	t.Logf("platform fee it would book   : %d  -> negative, so settlement BLOCKED", oldPlatformFee)

	assert.Equal(t, int64(-16), correctDelta, "Singapay charged 0.16 less than estimated")
	assert.Negative(t, oldPlatformFee,
		"the old arithmetic drove the platform fee negative, which is why these settlements blocked")

	// The blocking is why the damage was contained: a blocked transaction writes nothing
	// and stays COMPLETED. It is also why the backlog of unsettled transactions grew.
	adj, blocked := resolveFeeAdjustment(
		&domain.ProductTransaction{
			Fee: domain.FeeBreakdown{
				SellerNetAmount: 13000,
				PlatformFee:     platformFee,
				GatewayFee:      estimatedFee,
				FeeModel:        domain.FeeModelGatewayOnCustomer,
			},
		},
		domain.SettledTransaction{FeeMinor: actualFeeMinor, FeeReported: true},
	)

	assert.Empty(t, blocked, "the same transaction settles cleanly now")
	assert.Equal(t, int64(400016), adj.PlatformFeeMinor, "platform collects 4000.16")
	assert.Equal(t, int64(13000), adj.SellerNet, "seller is unaffected either way")
}
