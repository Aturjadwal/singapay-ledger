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
//
// # Units
//
// Every amount here is in MINOR UNITS — sen, rupiah × 100 — and the Minor suffix on each
// field says so at every use site. That is not decoration. Singapay reports a money-in fee
// with two decimals (a QRIS fee of "119.84" is a real figure, not a rounding artefact), and
// [github.com/Aturjadwal/singapay-ledger/singapay.Amount] keeps it exactly by storing sen.
// FeeBreakdown, by contrast, is whole rupiah, because every figure quoted at checkout is.
//
// The two meet in bookSettlement, and mixing them is a silent ×100 error on the path that
// decides what a seller is paid — it once shipped, because the field names carried no unit
// and the unit test built this struct by hand in rupiah. Convert with [RupiahToMinor] at
// the boundary; never assign a rupiah figure to a Minor field.
type SettledTransaction struct {
	MerchantReference    string
	GatewayTransactionID string
	GatewayAccountID     string
	PaymentChannel       string

	// GrossMinor is what the payer was charged; NetMinor is what reached the
	// sub-account. Where the channel reports a fee, Gross - Fee == Net. All in sen.
	GrossMinor  int64
	NetMinor    int64
	FeeMinor    int64
	FeeReported bool

	Raw map[string]string
}

// MinorPerRupiah is how many sen make a rupiah. Every conversion between the whole-rupiah
// figures quoted at checkout and the two-decimal figures Singapay reports goes through it.
const MinorPerRupiah = 100

// RupiahToMinor converts whole rupiah to sen. Always exact.
func RupiahToMinor(rupiah int64) int64 { return rupiah * MinorPerRupiah }

// MinorToRupiah converts sen to whole rupiah, reporting whether the conversion was exact.
//
// It truncates toward zero when it is not, and the ok result is what callers must branch
// on rather than the value: a figure that will not divide evenly is a fraction of a rupiah
// somebody has to account for, and dropping it is how a balance stops explaining itself.
func MinorToRupiah(minor int64) (rupiah int64, ok bool) {
	return minor / MinorPerRupiah, minor%MinorPerRupiah == 0
}
