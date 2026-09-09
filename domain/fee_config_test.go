package domain_test

import (
	"testing"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/stretchr/testify/assert"
)

func TestFeeConfig_CalculateFee(t *testing.T) {
	t.Run("fixed fee returns fixed amount", func(t *testing.T) {
		fc := &domain.FeeConfig{
			FeeType:     domain.FeeTypeFixed,
			FixedAmount: 4500,
			IsActive:    true,
		}

		assert.Equal(t, int64(4500), fc.CalculateFee(10000))
		assert.Equal(t, int64(4500), fc.CalculateFee(100000))
		assert.Equal(t, int64(4500), fc.CalculateFee(1))
	})

	t.Run("percentage fee calculates correctly", func(t *testing.T) {
		fc := &domain.FeeConfig{
			FeeType:    domain.FeeTypePercentage,
			Percentage: 2.2, // 2.2%
			IsActive:   true,
		}

		// 10000 * 2.2% = 220
		assert.Equal(t, int64(220), fc.CalculateFee(10000))
		// 100000 * 2.2% = 2200
		assert.Equal(t, int64(2200), fc.CalculateFee(100000))
	})

	t.Run("percentage fee rounds correctly", func(t *testing.T) {
		fc := &domain.FeeConfig{
			FeeType:    domain.FeeTypePercentage,
			Percentage: 2.2, // 2.2%
			IsActive:   true,
		}

		// Test rounding - 173.4 = 173
		// 7882 * 2.2% = 173.404 -> 173
		assert.Equal(t, int64(173), fc.CalculateFee(7882))

		// Test rounding - 173.5 = 174
		// 7886 * 2.2% = 173.492 -> 173 (still rounds down)
		// To get 173.5 exactly: amount = 173.5 / 0.022 = 7886.36...
		// Let's use a different setup for exact half
		fc2 := &domain.FeeConfig{
			FeeType:    domain.FeeTypePercentage,
			Percentage: 10, // 10%
			IsActive:   true,
		}
		// 1735 * 10% = 173.5 -> 174
		assert.Equal(t, int64(174), fc2.CalculateFee(1735))

		// Test rounding - 173.6 = 174
		// 1736 * 10% = 173.6 -> 174
		assert.Equal(t, int64(174), fc2.CalculateFee(1736))

		// Test rounding - 173.4 = 173
		// 1734 * 10% = 173.4 -> 173
		assert.Equal(t, int64(173), fc2.CalculateFee(1734))
	})

	t.Run("inactive fee returns zero", func(t *testing.T) {
		fc := &domain.FeeConfig{
			FeeType:     domain.FeeTypeFixed,
			FixedAmount: 4500,
			IsActive:    false,
		}

		assert.Equal(t, int64(0), fc.CalculateFee(10000))
	})

	t.Run("unknown fee type returns zero", func(t *testing.T) {
		fc := &domain.FeeConfig{
			FeeType:  "UNKNOWN",
			IsActive: true,
		}

		assert.Equal(t, int64(0), fc.CalculateFee(10000))
	})

	t.Run("zero amount returns zero for percentage", func(t *testing.T) {
		fc := &domain.FeeConfig{
			FeeType:    domain.FeeTypePercentage,
			Percentage: 2.2,
			IsActive:   true,
		}

		assert.Equal(t, int64(0), fc.CalculateFee(0))
	})
}

func TestNewFeeCalculator(t *testing.T) {
	t.Run("creates calculator with platform and gateway fees", func(t *testing.T) {
		configs := []*domain.FeeConfig{
			{
				ConfigType:     domain.FeeConfigTypePlatform,
				PaymentChannel: "PLATFORM",
				FeeType:        domain.FeeTypeFixed,
				FixedAmount:    1000,
				IsActive:       true,
			},
			{
				ConfigType:     domain.FeeConfigTypeGateway,
				PaymentChannel: "QRIS",
				FeeType:        domain.FeeTypePercentage,
				Percentage:     2.2,
				IsActive:       true,
			},
			{
				ConfigType:     domain.FeeConfigTypeGateway,
				PaymentChannel: "VIRTUAL_ACCOUNT_MANDIRI",
				FeeType:        domain.FeeTypeFixed,
				FixedAmount:    4500,
				IsActive:       true,
			},
		}

		calc := domain.NewFeeCalculator(configs)
		assert.NotNil(t, calc)

		// Test QRIS calculation
		// base_amount = 10000 + 1000 = 11000
		// total_charged = 11000 / (1 - 0.022) = 11247
		// gateway_fee = 11247 - 11000 = 247
		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "QRIS")
		assert.Equal(t, int64(1000), platformFee)
		assert.Equal(t, int64(247), gatewayFee)
		assert.Equal(t, int64(11247), total)
	})

	t.Run("ignores inactive configs", func(t *testing.T) {
		configs := []*domain.FeeConfig{
			{
				ConfigType:     domain.FeeConfigTypePlatform,
				PaymentChannel: "PLATFORM",
				FeeType:        domain.FeeTypeFixed,
				FixedAmount:    1000,
				IsActive:       false, // Inactive
			},
			{
				ConfigType:     domain.FeeConfigTypeGateway,
				PaymentChannel: "QRIS",
				FeeType:        domain.FeeTypePercentage,
				Percentage:     2.2,
				IsActive:       true,
			},
		}

		calc := domain.NewFeeCalculator(configs)

		// base_amount = 10000 + 0 = 10000
		// total_charged = 10000 / (1 - 0.022) = 10225
		// gateway_fee = 10225 - 10000 = 225
		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "QRIS")
		assert.Equal(t, int64(0), platformFee) // No platform fee (inactive)
		assert.Equal(t, int64(225), gatewayFee)
		assert.Equal(t, int64(10225), total)
	})

	t.Run("handles empty configs", func(t *testing.T) {
		calc := domain.NewFeeCalculator([]*domain.FeeConfig{})

		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "QRIS")
		assert.Equal(t, int64(0), platformFee)
		assert.Equal(t, int64(0), gatewayFee)
		assert.Equal(t, int64(10000), total)
	})

	t.Run("handles nil configs", func(t *testing.T) {
		calc := domain.NewFeeCalculator(nil)

		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "QRIS")
		assert.Equal(t, int64(0), platformFee)
		assert.Equal(t, int64(0), gatewayFee)
		assert.Equal(t, int64(10000), total)
	})
}

func TestFeeCalculator_CalculateTotalFees(t *testing.T) {
	platformConfig := &domain.FeeConfig{
		ConfigType:     domain.FeeConfigTypePlatform,
		PaymentChannel: "PLATFORM",
		FeeType:        domain.FeeTypeFixed,
		FixedAmount:    1000,
		IsActive:       true,
	}
	qrisConfig := &domain.FeeConfig{
		ConfigType:     domain.FeeConfigTypeGateway,
		PaymentChannel: "QRIS",
		FeeType:        domain.FeeTypePercentage,
		Percentage:     2.2,
		IsActive:       true,
	}
	vaConfig := &domain.FeeConfig{
		ConfigType:     domain.FeeConfigTypeGateway,
		PaymentChannel: "VIRTUAL_ACCOUNT_MANDIRI",
		FeeType:        domain.FeeTypeFixed,
		FixedAmount:    4500,
		IsActive:       true,
	}

	calc := domain.NewFeeCalculator([]*domain.FeeConfig{platformConfig, qrisConfig, vaConfig})

	t.Run("QRIS percentage fee", func(t *testing.T) {
		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "QRIS")

		assert.Equal(t, int64(1000), platformFee)
		// base_amount = 11000, total = 11000 / 0.978 = 11247, gateway_fee = 247
		assert.Equal(t, int64(247), gatewayFee)
		assert.Equal(t, int64(11247), total)
	})

	t.Run("VA fixed fee", func(t *testing.T) {
		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "VIRTUAL_ACCOUNT_MANDIRI")

		assert.Equal(t, int64(1000), platformFee)
		assert.Equal(t, int64(4500), gatewayFee) // Fixed fee regardless of amount
		assert.Equal(t, int64(15500), total)
	})

	t.Run("unknown payment channel returns only platform fee", func(t *testing.T) {
		platformFee, gatewayFee, total := calc.CalculateTotalFees(10000, "UNKNOWN_CHANNEL")

		assert.Equal(t, int64(1000), platformFee)
		assert.Equal(t, int64(0), gatewayFee) // No gateway config for unknown channel
		assert.Equal(t, int64(11000), total)
	})

	t.Run("large amounts", func(t *testing.T) {
		// 90,000,000 IDR (90M)
		// base_amount = 90000000 + 1000 = 90001000
		// total_charged = 90001000 / 0.978 = 92025562 (rounded)
		// gateway_fee = 92025562 - 90001000 = 2024562
		platformFee, gatewayFee, total := calc.CalculateTotalFees(90000000, "QRIS")

		assert.Equal(t, int64(1000), platformFee)
		assert.Equal(t, int64(2024562), gatewayFee)
		assert.Equal(t, int64(92025562), total)
	})
}

func TestFeeCalculator_GetFeeBreakdown(t *testing.T) {
	configs := []*domain.FeeConfig{
		{
			ConfigType:     domain.FeeConfigTypePlatform,
			PaymentChannel: "PLATFORM",
			FeeType:        domain.FeeTypeFixed,
			FixedAmount:    1000,
			IsActive:       true,
		},
		{
			ConfigType:     domain.FeeConfigTypeGateway,
			PaymentChannel: "QRIS",
			FeeType:        domain.FeeTypePercentage,
			Percentage:     2.2,
			IsActive:       true,
		},
	}

	calc := domain.NewFeeCalculator(configs)

	t.Run("returns complete fee breakdown", func(t *testing.T) {
		breakdown := calc.GetFeeBreakdown(10000, "QRIS", domain.CurrencyIDR)

		assert.Equal(t, int64(10000), breakdown.SellerPrice)
		assert.Equal(t, int64(1000), breakdown.PlatformFee)
		// base_amount = 11000, total = 11247, gateway_fee = 247
		assert.Equal(t, int64(247), breakdown.GatewayFee)
		assert.Equal(t, int64(11247), breakdown.TotalCharged)
		assert.Equal(t, int64(10000), breakdown.SellerNetAmount) // Seller gets 100% (default GATEWAY_ON_CUSTOMER)
		assert.Equal(t, domain.FeeModelGatewayOnCustomer, breakdown.FeeModel)
		assert.Equal(t, domain.CurrencyIDR, breakdown.Currency)
	})

	t.Run("fee breakdown validates correctly", func(t *testing.T) {
		breakdown := calc.GetFeeBreakdown(10000, "QRIS", domain.CurrencyIDR)

		// TotalCharged should equal SellerPrice + PlatformFee + GatewayFee for GATEWAY_ON_CUSTOMER
		expectedTotal := breakdown.SellerPrice + breakdown.PlatformFee + breakdown.GatewayFee
		assert.Equal(t, expectedTotal, breakdown.TotalCharged)

		// SellerNetAmount should equal SellerPrice for GATEWAY_ON_CUSTOMER
		assert.Equal(t, breakdown.SellerPrice, breakdown.SellerNetAmount)
	})
}

func TestFeeCalculator_GetFeeBreakdownWithModel(t *testing.T) {
	configs := []*domain.FeeConfig{
		{
			ConfigType:     domain.FeeConfigTypePlatform,
			PaymentChannel: "PLATFORM",
			FeeType:        domain.FeeTypeFixed,
			FixedAmount:    1000,
			IsActive:       true,
		},
		{
			ConfigType:     domain.FeeConfigTypeGateway,
			PaymentChannel: "QRIS",
			FeeType:        domain.FeeTypePercentage,
			Percentage:     2.2,
			IsActive:       true,
		},
	}

	calc := domain.NewFeeCalculator(configs)

	t.Run("GATEWAY_ON_CUSTOMER model - customer pays all fees", func(t *testing.T) {
		breakdown := calc.GetFeeBreakdownWithModel(10000, "QRIS", domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)

		assert.Equal(t, int64(10000), breakdown.SellerPrice)
		assert.Equal(t, int64(1000), breakdown.PlatformFee)
		assert.Equal(t, int64(247), breakdown.GatewayFee)
		assert.Equal(t, int64(11247), breakdown.TotalCharged)    // Customer pays: 10000 + 1000 + 247
		assert.Equal(t, int64(10000), breakdown.SellerNetAmount) // Seller gets 100% of price
		assert.Equal(t, domain.FeeModelGatewayOnCustomer, breakdown.FeeModel)
		assert.Equal(t, domain.CurrencyIDR, breakdown.Currency)
	})

	t.Run("GATEWAY_ON_SELLER model - seller absorbs gateway fee", func(t *testing.T) {
		breakdown := calc.GetFeeBreakdownWithModel(10000, "QRIS", domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)

		assert.Equal(t, int64(10000), breakdown.SellerPrice)
		assert.Equal(t, int64(1000), breakdown.PlatformFee)
		assert.Equal(t, int64(247), breakdown.GatewayFee)
		assert.Equal(t, int64(11000), breakdown.TotalCharged)   // Customer pays: 10000 + 1000 (no gateway fee)
		assert.Equal(t, int64(9753), breakdown.SellerNetAmount) // Seller bears the gateway fee: 10000 - 247
		assert.Equal(t, domain.FeeModelGatewayOnSeller, breakdown.FeeModel)
		assert.Equal(t, domain.CurrencyIDR, breakdown.Currency)

		// Verify CSV reconciliation would work:
		// Net credited by the gateway = totalCharged - gatewayFee = 11000 - 247 = 10753
		// This should equal SellerNetAmount + PlatformFee
		payToMerchant := breakdown.TotalCharged - breakdown.GatewayFee
		assert.Equal(t, breakdown.SellerNetAmount+breakdown.PlatformFee, payToMerchant, "SellerNetAmount + PlatformFee should equal PAY TO MERCHANT from CSV")

		// Distributions:
		// - Seller gets: SellerNetAmount = 9753
		// - Platform gets: PlatformFee = 1000
		// - Gateway keeps: GatewayFee = 247
		assert.Equal(t, int64(9753), breakdown.SellerNetAmount)
	})

	t.Run("validates seller net amount calculation", func(t *testing.T) {
		breakdownCustomer := calc.GetFeeBreakdownWithModel(10000, "QRIS", domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
		breakdownSeller := calc.GetFeeBreakdownWithModel(10000, "QRIS", domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)

		// GATEWAY_ON_CUSTOMER: SellerNetAmount = SellerPrice (seller gets 100% of their price)
		assert.Equal(t, breakdownCustomer.SellerPrice, breakdownCustomer.SellerNetAmount)

		// GATEWAY_ON_SELLER: SellerNetAmount = SellerPrice - GatewayFee (seller bears the gateway fee, platform tracked separately)
		expectedNet := breakdownSeller.SellerPrice - breakdownSeller.GatewayFee
		assert.Equal(t, expectedNet, breakdownSeller.SellerNetAmount)
	})

	t.Run("validates total charged calculation", func(t *testing.T) {
		breakdownCustomer := calc.GetFeeBreakdownWithModel(10000, "QRIS", domain.CurrencyIDR, domain.FeeModelGatewayOnCustomer)
		breakdownSeller := calc.GetFeeBreakdownWithModel(10000, "QRIS", domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)

		// GATEWAY_ON_CUSTOMER: TotalCharged = SellerPrice + PlatformFee + GatewayFee
		expectedCustomer := breakdownCustomer.SellerPrice + breakdownCustomer.PlatformFee + breakdownCustomer.GatewayFee
		assert.Equal(t, expectedCustomer, breakdownCustomer.TotalCharged)

		// GATEWAY_ON_SELLER: TotalCharged = SellerPrice + PlatformFee (no GatewayFee)
		expectedSeller := breakdownSeller.SellerPrice + breakdownSeller.PlatformFee
		assert.Equal(t, expectedSeller, breakdownSeller.TotalCharged)
	})
}

// Test a real-world settled transaction
func TestFeeCalculator_RealWorldScenario(t *testing.T) {
	// Setup: Platform fee = 1000, gateway fee = 4995 (fixed for Virtual Account)
	configs := []*domain.FeeConfig{
		{
			ConfigType:     domain.FeeConfigTypePlatform,
			PaymentChannel: "PLATFORM",
			FeeType:        domain.FeeTypeFixed,
			FixedAmount:    1000,
			IsActive:       true,
		},
		{
			ConfigType:     domain.FeeConfigTypeGateway,
			PaymentChannel: "VIRTUAL_ACCOUNT_MANDIRI",
			FeeType:        domain.FeeTypeFixed,
			FixedAmount:    4995,
			IsActive:       true,
		},
	}

	calc := domain.NewFeeCalculator(configs)

	t.Run("GATEWAY_ON_SELLER with seller_price=50000 matches CSV data", func(t *testing.T) {
		// User's scenario: SellerPrice = 50000, PlatformFee = 1000, GatewayFee = 4995
		// CSV shows: AMOUNT = 51000, FEE = 4995, PAY TO MERCHANT = 46005
		breakdown := calc.GetFeeBreakdownWithModel(50000, "VIRTUAL_ACCOUNT_MANDIRI", domain.CurrencyIDR, domain.FeeModelGatewayOnSeller)

		assert.Equal(t, int64(50000), breakdown.SellerPrice)
		assert.Equal(t, int64(1000), breakdown.PlatformFee)
		assert.Equal(t, int64(4995), breakdown.GatewayFee)
		assert.Equal(t, int64(51000), breakdown.TotalCharged, "Should match CSV AMOUNT")
		assert.Equal(t, int64(45005), breakdown.SellerNetAmount, "Seller bears the gateway fee: 50000 - 4995")

		// Verify reconciliation matching logic
		// CSV PAY TO MERCHANT = TotalCharged - GatewayFee = 51000 - 4995 = 46005
		// This equals SellerNetAmount + PlatformFee = 45005 + 1000 = 46005
		csvPayToMerchant := int64(46005)
		assert.Equal(t, csvPayToMerchant, breakdown.SellerNetAmount+breakdown.PlatformFee, "SellerNetAmount + PlatformFee should equal CSV PAY TO MERCHANT")

		// Seller's actual payout is SellerNetAmount directly
		assert.Equal(t, int64(45005), breakdown.SellerNetAmount, "Seller receives 45005 (price minus the gateway fee)")

		// Total check: seller + platform + gateway = total charged
		total := breakdown.SellerNetAmount + breakdown.PlatformFee + breakdown.GatewayFee
		assert.Equal(t, breakdown.TotalCharged, total, "All amounts should sum to total charged")
	})
}

func TestFeeConfigType_Constants(t *testing.T) {
	assert.Equal(t, domain.FeeConfigType("PLATFORM"), domain.FeeConfigTypePlatform)
	assert.Equal(t, domain.FeeConfigType("GATEWAY"), domain.FeeConfigTypeGateway)
}

func TestFeeType_Constants(t *testing.T) {
	assert.Equal(t, domain.FeeType("FIXED"), domain.FeeTypeFixed)
	assert.Equal(t, domain.FeeType("PERCENTAGE"), domain.FeeTypePercentage)
}
