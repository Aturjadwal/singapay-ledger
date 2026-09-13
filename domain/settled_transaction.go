package domain

// SettledTransaction is one settled row as a gateway product endpoint describes it,
// normalised across the four channels that can produce one.
//
// It exists so the settling pass works in one shape. The four Singapay read endpoints
// return four different structs, and only three of them carry a fee; flattening them here
// keeps that difference in one place — the FeeReported flag — instead of spreading four
// channel-specific branches through the settlement logic.
//
// Nothing persists it. It has no Record, no table and no repository: it is built from a
// gateway response in readSettledTransaction, consumed by bookSettlement, and discarded.
// What survives is the journal metadata written from it.
type SettledTransaction struct {
	MerchantReference    string
	GatewayTransactionID string
	GatewayAccountID     string
	PaymentChannel       string

	// GrossAmount is what the payer was charged; NetAmount is what reached the
	// sub-account. Where the channel reports a fee, Gross - Fee == Net.
	GrossAmount int64
	NetAmount   int64
	Fee         int64
	FeeReported bool

	Raw map[string]string
}
