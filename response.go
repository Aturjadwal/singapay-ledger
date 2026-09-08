package ledger

import "github.com/21strive/ledger/domain"

// MoneyResponse represents a monetary amount with currency
type MoneyResponse struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// BalanceResponse represents the balance for an account.
// Values are cached in ledger_accounts table and updated automatically on ledger entry saves.
type BalanceResponse struct {
	PendingBalance        MoneyResponse `json:"pending_balance"`
	AvailableBalance      MoneyResponse `json:"available_balance"`
	TotalWithdrawalAmount MoneyResponse `json:"total_withdrawal_amount"`
	TotalDepositAmount    MoneyResponse `json:"total_deposit_amount"`
	Currency              string        `json:"currency"`
}

// CheapestChannelInfo contains summary info for the payment channel with the lowest gateway fee.
type CheapestChannelInfo struct {
	PaymentChannel string `json:"payment_channel"`
	GatewayFee     int64  `json:"gateway_fee"`
	TotalCharged   int64  `json:"total_charged"`
}

// FeeCalculationResponse wraps a FeeBreakdown and adds cheapest-channel guidance.
type FeeCalculationResponse struct {
	domain.FeeBreakdown
	// CheapestPaymentChannel is the channel with the lowest gateway fee for the same seller price.
	// It equals the requested channel when that channel is already the cheapest.
	CheapestPaymentChannel CheapestChannelInfo `json:"cheapest_payment_channel"`
}
