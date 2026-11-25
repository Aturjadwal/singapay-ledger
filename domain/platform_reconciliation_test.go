package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFloorSplitMinor(t *testing.T) {
	for _, c := range []struct {
		minor, rupiah, sen int64
	}{
		{400016, 4000, 16},     // CSV7MW: Rp4.000 to the ledger, 16 sen to the pool
		{5028, 50, 28},         // the residual pool on 25 Nov 09.06 WIB
		{99, 0, 99},            // under a rupiah: nothing to book, everything carried
		{100, 1, 0},            // exactly a rupiah
		{0, 0, 0},              //
		{-4130, -42, 70},       // −41,30 rounds DOWN: −Rp42, and 70 sen to the pool
		{-100, -1, 0},          //
		{-1, -1, 99},           // one sen short is a rupiah down and 99 sen carried
		{-4172684, -41727, 16}, // the figure before the DOKU archive
	} {
		rupiah, sen := FloorSplitMinor(c.minor)
		assert.Equal(t, c.rupiah, rupiah, "rupiah of %d", c.minor)
		assert.Equal(t, c.sen, sen, "sen of %d", c.minor)
		assert.Equal(t, c.minor, RupiahToMinor(rupiah)+sen, "the parts add up to %d", c.minor)
		assert.True(t, sen >= 0 && sen < MinorPerRupiah, "sen of %d in 0..99", c.minor)
	}
}

func TestPlatformBalanceSnapshot_Equation(t *testing.T) {
	// 25 Nov 2026 14.10 WIB, after the 13.00 sweep: 164 sweeps, the DOKU era still in the ledger.
	s := &PlatformBalanceSnapshot{
		LedgerAvailable:    701679,
		SweptResidualMinor: 5190,
	}
	assert.Equal(t, int64(5190), s.PoolMinor())
	assert.Equal(t, int64(-4172684), s.UnexplainedMinor(66000406), "−45.727 + 4.000,16")

	// A rounding moves L and K by the same rupiah: the figure stays.
	s.LedgerAvailable += 51
	s.RoundedMinor += 5100
	assert.Equal(t, int64(90), s.PoolMinor())
	assert.Equal(t, int64(-4172684), s.UnexplainedMinor(66000406))

	// A fee settled and not swept: L and B move together.
	s.LedgerAvailable += 4000
	s.UnsweptPlatformFee += 4000
	assert.Equal(t, int64(-4172684), s.UnexplainedMinor(66000406))

	// A difference booked: rupiah to L, sen to K.
	s.LedgerAvailable += -41727
	s.AdjustmentResidualMinor += 16
	assert.Equal(t, int64(0), s.UnexplainedMinor(66000406))

	assert.True(t, s.SameAs(&PlatformBalanceSnapshot{LedgerAvailable: s.LedgerAvailable, SweptResidualMinor: 5190,
		RoundedMinor: 5100, UnsweptPlatformFee: 4000, AdjustmentResidualMinor: 16}))
	assert.False(t, s.SameAs(&PlatformBalanceSnapshot{}))
	assert.False(t, s.SameAs(nil))
}

func TestPlatformReconciliation_DecideAndSkip(t *testing.T) {
	snapshot := &PlatformBalanceSnapshot{LedgerAvailable: 619953, SweptResidualMinor: 5028}
	now := time.Date(2026, 11, 25, 2, 6, 0, 0, time.UTC)

	rec := NewPlatformReconciliation("platform", "01M2DCNBH0Y3CS41H0MM2SQGND", snapshot, now)
	assert.Nil(t, rec.SingapayAvailableMinor, "NULL until read, never a pointer to 0")
	assert.Nil(t, rec.UnexplainedMinor)
	assert.Nil(t, rec.EmailedAt)
	assert.Equal(t, int64(61995300), rec.LedgerAvailableMinor)
	assert.Equal(t, int64(5028), rec.ResidualPoolMinor)

	rec.Decide(62400344)
	assert.Equal(t, PlatformReconciliationMismatch, rec.Status)
	assert.Equal(t, int64(400016), rec.Unexplained())

	rec.Decide(61995300 + 5028)
	assert.Equal(t, PlatformReconciliationMatched, rec.Status)
	assert.Equal(t, int64(0), rec.Unexplained())

	rec.Skip(SkipPayoutInFlight, "1 platform payout(s)")
	assert.Equal(t, PlatformReconciliationSkipped, rec.Status)
	assert.Nil(t, rec.UnexplainedMinor)
	assert.Equal(t, int64(0), rec.Unexplained())
}

func TestPlatformAdjustment_ReasonAndNote(t *testing.T) {
	for _, r := range []PlatformAdjustmentReason{PlatformAdjustmentReasonTestFunds, PlatformAdjustmentReasonOffSystemWithdrawal,
		PlatformAdjustmentReasonSingapayCorrection, PlatformAdjustmentReasonOther} {
		assert.True(t, r.Valid(), r)
	}
	assert.False(t, PlatformAdjustmentReason("").Valid())
	assert.False(t, PlatformAdjustmentReason("test_funds").Valid())

	note, ok := CleanPlatformAdjustmentNote("  Dana uji coba  ")
	assert.True(t, ok)
	assert.Equal(t, "Dana uji coba", note)
	_, ok = CleanPlatformAdjustmentNote(strings.Repeat("é", MaxPlatformAdjustmentNoteLength))
	assert.True(t, ok, "characters, not bytes")
	_, ok = CleanPlatformAdjustmentNote(strings.Repeat("x", MaxPlatformAdjustmentNoteLength+1))
	assert.False(t, ok)

	a := NewPlatformAdjustment("platform", PlatformAdjustmentRounding, 50)
	assert.Nil(t, a.PoolBeforeMinor, "NULL until a rounding sets it")
	assert.NotNil(t, a.SourceTransactionUUIDs)
}
