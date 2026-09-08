package domain

import (
	"context"
	"math"

	"github.com/21strive/redifu"
)

// FeeConfigType represents the type of fee configuration
type FeeConfigType string

const (
	FeeConfigTypePlatform FeeConfigType = "PLATFORM"

	// FeeConfigTypeGateway holds the per-channel rate Singapay charges. Singapay does
	// not quote money-in rates through the API — ListPaymentMethods returns the channel
	// catalogue with no prices, because they are commercial terms — so this table is
	// the only place the expected fee exists before a transaction settles.
	FeeConfigTypeGateway FeeConfigType = "GATEWAY"
)

// FeeType represents whether the fee is fixed or percentage-based
type FeeType string

const (
	FeeTypeFixed      FeeType = "FIXED"
	FeeTypePercentage FeeType = "PERCENTAGE"
	FeeTypeHybrid     FeeType = "HYBRID" // fixed_amount + percentage of total_charged
)

// FeeConfig represents a fee configuration for a payment channel
type FeeConfig struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`
	ConfigType     FeeConfigType // PLATFORM or GATEWAY
	PaymentChannel string        // Singapay channel code: QRIS, VA_BCA, EWALLET_DANA, …
	Name           string        // Human-readable name (e.g., "VA BCA", "DANA", "QRIS")
	FeeType        FeeType       // FIXED or PERCENTAGE
	FixedAmount    int64         // Fixed fee amount in smallest currency unit
	Percentage     float64       // Percentage fee (e.g., 2.2 means 2.2%)
	IsActive       bool
}

// FeeConfigRepository defines data access for fee configurations
type FeeConfigRepository interface {
	GetByID(ctx context.Context, id string) (*FeeConfig, error)
	GetByConfigTypeAndChannel(ctx context.Context, configType FeeConfigType, paymentChannel string) (*FeeConfig, error)
	GetActiveByPaymentChannel(ctx context.Context, paymentChannel string) ([]*FeeConfig, error)
	GetPlatformFee(ctx context.Context) (*FeeConfig, error)
	GetAllActive(ctx context.Context) ([]*FeeConfig, error)
	GetAllExcludingPlatform(ctx context.Context) ([]*FeeConfig, error)
	Save(ctx context.Context, fc *FeeConfig) error
	Update(ctx context.Context, fc *FeeConfig) error
}

// CalculateFee calculates the fee based on the configuration
func (fc *FeeConfig) CalculateFee(amount int64) int64 {
	if !fc.IsActive {
		return 0
	}

	switch fc.FeeType {
	case FeeTypeFixed:
		return fc.FixedAmount
	case FeeTypePercentage:
		// Percentage is stored as whole number (e.g., 2.2 = 2.2%)
		// Use math.Round for standard rounding (half up)
		return int64(math.Round(float64(amount) * fc.Percentage / 100))
	case FeeTypeHybrid:
		percentageFee := int64(math.Round(float64(amount) * fc.Percentage / 100))
		return fc.FixedAmount + percentageFee
	default:
		return 0
	}
}

// FeeCalculator provides methods to calculate fees for transactions
type FeeCalculator struct {
	platformFee *FeeConfig
	gatewayFees map[string]*FeeConfig // keyed by payment channel
}

// NewFeeCalculator creates a new fee calculator with provided configurations
func NewFeeCalculator(configs []*FeeConfig) *FeeCalculator {
	calc := &FeeCalculator{
		gatewayFees: make(map[string]*FeeConfig),
	}

	for _, cfg := range configs {
		if !cfg.IsActive {
			continue
		}
		if calc.platformFee == nil && cfg.ConfigType == FeeConfigTypePlatform && cfg.PaymentChannel == "PLATFORM" {
			calc.platformFee = cfg
		} else if cfg.ConfigType == FeeConfigTypeGateway {
			calc.gatewayFees[cfg.PaymentChannel] = cfg
		}
	}

	return calc
}

// calculateFees is the core fee calculation logic.
// skipPlatformFee=true omits platform fee from base amount and result.
// platformFeeMultiplier multiplies the platform fee (e.g. installment with 2 due terms → multiplier=2).
// The gateway fee is never multiplied regardless of the multiplier value.
func (fc *FeeCalculator) calculateFees(sellerPrice int64, paymentChannel string, skipPlatformFee bool, platformFeeMultiplier int) (platformFee, gatewayFee, totalCharged int64) {
	if !skipPlatformFee && fc.platformFee != nil {
		platformFee = fc.platformFee.CalculateFee(sellerPrice)
		if platformFeeMultiplier > 1 {
			platformFee *= int64(platformFeeMultiplier)
		}
	}

	baseAmount := sellerPrice + platformFee

	// Calculate the gateway fee based on payment channel
	if gatewayConfig, ok := fc.gatewayFees[paymentChannel]; ok {
		if gatewayConfig.IsActive {
			switch gatewayConfig.FeeType {
			case FeeTypeFixed:
				// Fixed fee is straightforward
				gatewayFee = gatewayConfig.FixedAmount
				totalCharged = baseAmount + gatewayFee
			case FeeTypePercentage:
				// The gateway charges X% on total_charged, so:
				// total_charged - (total_charged * X%) = base_amount
				// total_charged * (1 - X%) = base_amount
				// total_charged = base_amount / (1 - X%)
				//
				// Example: base_amount = 51000, gateway = 2.2%
				// total_charged = 51000 / (1 - 0.022) = 51000 / 0.978 = 52147
				// gateway_fee = 52147 - 51000 = 1147
				// The gateway receives 52147, takes 2.2% = 1147, leaves 51000 ✓
				percentage := gatewayConfig.Percentage / 100
				totalCharged = int64(math.Round(float64(baseAmount) / (1 - percentage)))
				gatewayFee = totalCharged - baseAmount
			case FeeTypeHybrid:
				// The gateway charges fixed_amount + X% on total_charged, so:
				// total_charged - fixed_amount - (total_charged * X%) = base_amount
				// total_charged * (1 - X%) = base_amount + fixed_amount
				// total_charged = (base_amount + fixed_amount) / (1 - X%)
				//
				// Example: base_amount = 51000, gateway = 1500 + 1%
				// total_charged = (51000 + 1500) / (1 - 0.01) = 52500 / 0.99 = 53030
				// gateway_fee = 53030 - 51000 = 2030
				// The gateway receives 53030, takes 1500 + 1% of 53030 (530) = 2030, leaves 51000 ✓
				percentage := gatewayConfig.Percentage / 100
				totalCharged = int64(math.Round(float64(baseAmount+gatewayConfig.FixedAmount) / (1 - percentage)))
				gatewayFee = totalCharged - baseAmount
			}
		} else {
			totalCharged = baseAmount
		}
	} else {
		totalCharged = baseAmount
	}

	return
}

// CalculateTotalFees calculates the platform fee and the gateway fee for a given seller price and payment channel.
// IMPORTANT: the gateway charges its fee on the TOTAL amount it receives, not on (seller_price + platform_fee),
// so this reverse-calculates to ensure seller and platform get the right amounts.
func (fc *FeeCalculator) CalculateTotalFees(sellerPrice int64, paymentChannel string) (platformFee, gatewayFee, totalCharged int64) {
	return fc.calculateFees(sellerPrice, paymentChannel, false, 0)
}

// FeeBreakdownOptions controls optional behaviour during fee calculation.
type FeeBreakdownOptions struct {
	FeeModel              FeeModel
	SkipPlatformFee       bool // When true, platform fee is not charged (e.g. partner / promo transactions)
	PlatformFeeMultiplier int  // When > 1, platform fee is multiplied (e.g. installment: 2 due terms → multiplier=2). The gateway fee is never multiplied.
}

// GetFeeBreakdown returns a complete fee breakdown for a transaction
// Defaults to FeeModelGatewayOnCustomer for backward compatibility
func (fc *FeeCalculator) GetFeeBreakdown(sellerPrice int64, paymentChannel string, currency Currency) FeeBreakdown {
	return fc.GetFeeBreakdownWithModel(sellerPrice, paymentChannel, currency, FeeModelGatewayOnCustomer)
}

// GetFeeBreakdownWithModel returns a complete fee breakdown with specified fee model
func (fc *FeeCalculator) GetFeeBreakdownWithModel(sellerPrice int64, paymentChannel string, currency Currency, feeModel FeeModel) FeeBreakdown {
	return fc.GetFeeBreakdownWithOptions(sellerPrice, paymentChannel, currency, FeeBreakdownOptions{FeeModel: feeModel})
}

// GetFeeBreakdownWithOptions returns a complete fee breakdown with full control over fee behaviour.
func (fc *FeeCalculator) GetFeeBreakdownWithOptions(sellerPrice int64, paymentChannel string, currency Currency, opts FeeBreakdownOptions) FeeBreakdown {
	platformFee, gatewayFee, _ := fc.calculateFees(sellerPrice, paymentChannel, opts.SkipPlatformFee, opts.PlatformFeeMultiplier)

	var totalCharged, sellerNetAmount int64

	switch opts.FeeModel {
	case FeeModelGatewayOnCustomer:
		// Customer pays everything: seller_price + platform_fee + gateway_fee
		totalCharged = sellerPrice + platformFee + gatewayFee
		sellerNetAmount = sellerPrice // Seller gets 100% of their price

	case FeeModelGatewayOnSeller:
		// Customer pays: seller_price + platform_fee (no gateway fee)
		totalCharged = sellerPrice + platformFee
		sellerNetAmount = sellerPrice - gatewayFee // Seller bears the gateway fee; platform fee tracked separately

	default:
		// Default to customer pays all (backward compatibility)
		totalCharged = sellerPrice + platformFee + gatewayFee
		sellerNetAmount = sellerPrice
	}

	return FeeBreakdown{
		SellerPrice:     sellerPrice,
		PlatformFee:     platformFee,
		GatewayFee:      gatewayFee,
		TotalCharged:    totalCharged,
		SellerNetAmount: sellerNetAmount,
		FeeModel:        opts.FeeModel,
		Currency:        currency,
	}
}

// HasPaymentChannel checks if the fee calculator has a gateway fee config for the given payment channel
func (fc *FeeCalculator) HasPaymentChannel(paymentChannel string) bool {
	_, ok := fc.gatewayFees[paymentChannel]
	return ok
}

// SupportedPaymentChannels returns all payment channels that have fee configurations
func (fc *FeeCalculator) SupportedPaymentChannels() []string {
	channels := make([]string, 0, len(fc.gatewayFees))
	for channel := range fc.gatewayFees {
		channels = append(channels, channel)
	}
	return channels
}

// GetCheapestChannel returns the payment channel with the lowest gateway fee for the given amount.
// Returns empty string if no channels are configured.
func (fc *FeeCalculator) GetCheapestChannel(sellerPrice int64, currency Currency, opts FeeBreakdownOptions) (channel string, breakdown FeeBreakdown) {
	var minFee int64 = math.MaxInt64

	for ch := range fc.gatewayFees {
		chOpts := opts
		chOpts.FeeModel = opts.FeeModel
		b := fc.GetFeeBreakdownWithOptions(sellerPrice, ch, currency, chOpts)
		if b.GatewayFee < minFee {
			minFee = b.GatewayFee
			channel = ch
			breakdown = b
		}
	}

	return
}
