package domain

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func minor(v int64) *int64 { return &v }

// swimlq is INV-20260925161538-SWIMLQ as settlement booked it: a card payment priced
// 60.000 + 4.000 + 4.041 = 68.041, settled on the estimate.
func swimlq() CardFeeCandidate {
	return CardFeeCandidate{
		ProductTransactionUUID:  "1fceb3f6-swimlq",
		InvoiceNumber:           "INV-20260925161538-SWIMLQ",
		PlatformFee:             4000,
		GatewayFee:              4041,
		TotalCharged:            68041,
		SellerNetAmount:         60000,
		SettledPlatformFeeMinor: minor(400000),
		SettledGatewayFeeMinor:  minor(404100),
		PlatformResidualMinor:   minor(0),
	}
}

func TestSettledFeeSourceFor(t *testing.T) {
	assert.Equal(t, SettledFeeSourceGateway, SettledFeeSourceFor(true))
	assert.Equal(t, SettledFeeSourceEstimate, SettledFeeSourceFor(false))
}

// The checkout formula, in sen: 3% of a whole-rupiah charge is the charge × 3 sen, exactly.
func TestExpectedCardPlatformShareMinor(t *testing.T) {
	// 6.804.100 − 6.000.000 − (204.123 + 200.000) = 399.977. Singapay took one sen more on
	// SWIMLQ (4041.24), which is why the balance, not this formula, decides the total.
	assert.Equal(t, int64(399977), ExpectedCardPlatformShareMinor(68041, 60000))
	// 109.278: 10.927.800 − 10.000.000 − (327.834 + 200.000) = 399.966.
	assert.Equal(t, int64(399966), ExpectedCardPlatformShareMinor(109278, 100000))
}

func TestSplitPlatformFeeMinor(t *testing.T) {
	cases := []struct {
		share, rupiah, residual int64
	}{
		{399976, 3999, 76},
		{400042, 4000, 42},
		{400000, 4000, 0},
		{99, 0, 99},
		{0, 0, 0},
	}
	for _, c := range cases {
		rupiah, residual := SplitPlatformFeeMinor(c.share)
		assert.Equal(t, c.rupiah, rupiah, "rupiah of %d", c.share)
		assert.Equal(t, c.residual, residual, "residual of %d", c.share)
		assert.Equal(t, c.share, RupiahToMinor(rupiah)+residual)
	}
}

func TestAllocateProportionally(t *testing.T) {
	t.Run("one weight takes everything", func(t *testing.T) {
		parts, err := AllocateProportionally(399976, []int64{399977})
		require.NoError(t, err)
		assert.Equal(t, []int64{399976}, parts)
	})

	t.Run("equal weights: the leftover sen go to the earlier positions", func(t *testing.T) {
		parts, err := AllocateProportionally(10, []int64{1, 1, 1})
		require.NoError(t, err)
		assert.Equal(t, []int64{4, 3, 3}, parts)
	})

	t.Run("the largest remainder gets the sen, not the first position", func(t *testing.T) {
		// 100 × 1/6 = 16.67, 100 × 2/6 = 33.33, 100 × 3/6 = 50: floor 16 + 33 + 50 = 99, and
		// the one sen left goes to the 0.67 remainder.
		parts, err := AllocateProportionally(100, []int64{1, 2, 3})
		require.NoError(t, err)
		assert.Equal(t, []int64{17, 33, 50}, parts)
	})

	t.Run("zero is split into zeros", func(t *testing.T) {
		parts, err := AllocateProportionally(0, []int64{5, 7})
		require.NoError(t, err)
		assert.Equal(t, []int64{0, 0}, parts)
	})

	t.Run("the parts always sum to the total", func(t *testing.T) {
		weights := []int64{399977, 400012, 399943, 400100, 399500, 401234, 398765, 400001, 399999, 400000, 400003, 399990, 400010, 399980, 400020}
		for _, total := range []int64{0, 1, 14, 15, 16, 5999999, 6000000, 6000001, 6000017} {
			parts, err := AllocateProportionally(total, weights)
			require.NoError(t, err)
			var sum int64
			for _, p := range parts {
				assert.GreaterOrEqual(t, p, int64(0))
				sum += p
			}
			assert.Equal(t, total, sum, "total %d", total)
		}
	})

	t.Run("products beyond int64 do not overflow", func(t *testing.T) {
		parts, err := AllocateProportionally(math.MaxInt64/2, []int64{math.MaxInt64 / 4, math.MaxInt64 / 4})
		require.NoError(t, err)
		assert.Equal(t, int64(math.MaxInt64/2), parts[0]+parts[1])
	})

	t.Run("refused inputs", func(t *testing.T) {
		_, err := AllocateProportionally(10, nil)
		assert.Error(t, err)
		_, err = AllocateProportionally(-1, []int64{1})
		assert.Error(t, err)
		_, err = AllocateProportionally(10, []int64{1, 0})
		assert.Error(t, err)
		_, err = AllocateProportionally(10, []int64{1, -1})
		assert.Error(t, err)
	})
}

// A weight from the formula that is not positive — a rate far from 3% + Rp2.000 on a large
// charge — sends every card to the priced fee instead.
func TestCardPlatformShareWeights_FallsBackToThePricedFee(t *testing.T) {
	normal := swimlq()
	assert.Equal(t, []int64{399977}, CardPlatformShareWeights([]CardFeeCandidate{normal}))

	odd := swimlq()
	odd.ProductTransactionUUID = "odd"
	odd.PlatformFee = 10
	odd.TotalCharged = 100_000_000 // its formula share is far below zero
	odd.SellerNetAmount = 99_000_000
	assert.Equal(t, []int64{400000, 1000}, CardPlatformShareWeights([]CardFeeCandidate{normal, odd}))
}

func TestComputeCardPlatformFees(t *testing.T) {
	// SWIMLQ before the manual correction: the seller has withdrawn everything, nothing else
	// is owed, and the sub-account holds 3999.76.
	t.Run("SWIMLQ", func(t *testing.T) {
		snapshot := &CardFeeSnapshot{Cards: []CardFeeCandidate{swimlq()}}
		result, err := ComputeCardPlatformFees(399976, snapshot, 0)
		require.NoError(t, err)
		assert.Empty(t, result.HoldReason)
		assert.Equal(t, int64(399976), result.ComputedMinor)
		assert.Equal(t, int64(400000), result.NominalMinor)
		assert.Equal(t, DefaultCardFeeTolerancePerCardMinor, result.ToleranceMinor)
		assert.Equal(t, []int64{399976}, result.Shares)
		assert.Equal(t, []int64{399977}, result.Weights)
	})

	t.Run("subtracts the seller's balance and the final unswept fees", func(t *testing.T) {
		snapshot := &CardFeeSnapshot{
			SellerLedgerAvailable:          100000,
			CertainUnsweptPlatformFeeMinor: 399976,
			Cards:                          []CardFeeCandidate{swimlq()},
		}
		result, err := ComputeCardPlatformFees(10000000+399976+399966, snapshot, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(399966), result.ComputedMinor)
		assert.Equal(t, []int64{399966}, result.Shares)
	})

	t.Run("a deviation exactly at the tolerance is applied, one sen more is held", func(t *testing.T) {
		snapshot := &CardFeeSnapshot{Cards: []CardFeeCandidate{swimlq(), swimlq()}}
		snapshot.Cards[1].ProductTransactionUUID = "second"

		atLimit, err := ComputeCardPlatformFees(800000-20000, snapshot, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(20000), atLimit.ToleranceMinor, "Rp100 per card, two cards")
		assert.Empty(t, atLimit.HoldReason)
		assert.Equal(t, int64(780000), atLimit.Shares[0]+atLimit.Shares[1])

		over, err := ComputeCardPlatformFees(800000+20001, snapshot, 0)
		require.NoError(t, err)
		assert.Equal(t, HoldDeviationAboveTolerance, over.HoldReason)
		assert.Nil(t, over.Shares)
	})

	t.Run("a configured tolerance replaces the default", func(t *testing.T) {
		snapshot := &CardFeeSnapshot{Cards: []CardFeeCandidate{swimlq()}}
		held, err := ComputeCardPlatformFees(399976, snapshot, 20)
		require.NoError(t, err)
		assert.Equal(t, int64(20), held.ToleranceMinor)
		assert.Equal(t, HoldDeviationAboveTolerance, held.HoldReason, "24 sen off, 20 allowed")
	})

	t.Run("a negative share is held whatever the tolerance", func(t *testing.T) {
		snapshot := &CardFeeSnapshot{SellerLedgerAvailable: 60000, Cards: []CardFeeCandidate{swimlq()}}
		result, err := ComputeCardPlatformFees(0, snapshot, 1_000_000_000)
		require.NoError(t, err)
		assert.Equal(t, int64(-6000000), result.ComputedMinor)
		assert.Equal(t, HoldDeviationAboveTolerance, result.HoldReason)
	})

	t.Run("a share above the payer's fees is held", func(t *testing.T) {
		// The tolerance lets 9.000.000 through; it would leave the gateway fee negative.
		snapshot := &CardFeeSnapshot{Cards: []CardFeeCandidate{swimlq()}}
		result, err := ComputeCardPlatformFees(9000000, snapshot, 1_000_000_000)
		require.NoError(t, err)
		assert.Equal(t, HoldDeviationAboveTolerance, result.HoldReason)
	})

	t.Run("no card is an error, not a round", func(t *testing.T) {
		_, err := ComputeCardPlatformFees(0, &CardFeeSnapshot{}, 0)
		assert.Error(t, err)
	})
}

func TestCardFeeSnapshot_SameAs(t *testing.T) {
	base := func() *CardFeeSnapshot {
		return &CardFeeSnapshot{
			SellerAccountID:                "seller",
			SellerLedgerAvailable:          100,
			CertainUnsweptPlatformFeeMinor: 200,
			Cards:                          []CardFeeCandidate{swimlq()},
		}
	}
	assert.True(t, base().SameAs(base()))

	changes := map[string]func(*CardFeeSnapshot){
		"balance":     func(s *CardFeeSnapshot) { s.SellerLedgerAvailable++ },
		"certain fee": func(s *CardFeeSnapshot) { s.CertainUnsweptPlatformFeeMinor++ },
		"payout":      func(s *CardFeeSnapshot) { s.DisbursementsInFlight = 1 },
		"a new card":  func(s *CardFeeSnapshot) { s.Cards = append(s.Cards, swimlq()) },
		"a card gone": func(s *CardFeeSnapshot) { s.Cards = nil },
		"a booked fee": func(s *CardFeeSnapshot) {
			s.Cards[0].SettledPlatformFeeMinor = minor(399976)
		},
		"a fee becoming NULL": func(s *CardFeeSnapshot) {
			s.Cards[0].SettledGatewayFeeMinor = nil
			s.Cards[0].GatewayFee = 4041 // the fallback would read the same figure
		},
	}
	for name, change := range changes {
		changed := base()
		change(changed)
		assert.False(t, base().SameAs(changed), name)
	}
}

func TestNewPlatformFeeReconciliation_NullableFiguresStartNull(t *testing.T) {
	snapshot := &CardFeeSnapshot{
		SellerAccountID:                "seller",
		SellerLedgerAvailable:          600,
		CertainUnsweptPlatformFeeMinor: 77,
		Cards:                          []CardFeeCandidate{swimlq()},
	}
	rec := NewPlatformFeeReconciliation(snapshot, "01SUBACCOUNT", 0)

	assert.NotEmpty(t, rec.UUID)
	assert.NotEmpty(t, rec.RandId)
	assert.Nil(t, rec.SingapayAvailableMinor, "InitRecord allocates every nil pointer; NULL must stay NULL")
	assert.Nil(t, rec.SingapayPendingMinor)
	assert.Nil(t, rec.SingapayHeldMinor)
	assert.Nil(t, rec.ComputedPlatformFeeMinor)
	assert.Equal(t, int64(60000), rec.SellerLedgerAvailableMinor, "sen, like every figure on the row")
	assert.Equal(t, int64(400000), rec.NominalPlatformFeeMinor)
	assert.Equal(t, DefaultCardFeeTolerancePerCardMinor, rec.ToleranceMinor)
	assert.Equal(t, 1, rec.CardCount)

	item := NewPlatformFeeReconciliationItem(rec.UUID, swimlq())
	assert.Nil(t, item.AllocatedPlatformFeeMinor)
	assert.Nil(t, item.SettledGatewayFeeMinor)
	assert.Nil(t, item.PlatformResidualMinor)
	assert.Equal(t, int64(400000), item.PreviousSettledPlatformFeeMinor)
	assert.Equal(t, int64(404100), item.PreviousSettledGatewayFeeMinor)
}
