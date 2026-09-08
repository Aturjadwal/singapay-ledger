package singapay

import (
	"encoding/json"
	"testing"
)

func TestAmountUnmarshal(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantMinor int64
		wantCur   string
		wantSet   bool
	}{
		// The three shapes Singapay actually sends, all for the same kind of field.
		{"quoted decimal, balance inquiry", `"1234.56"`, 123456, "", true},
		{"quoted integer, account transfer", `"500000"`, 50000000, "", true},
		{"bare number, VA webhook", `100000`, 10000000, "", true},

		{"money object", `{"value":"12504.00","currency":"IDR"}`, 1250400, "IDR", true},
		{"money object, value first", `{"currency":"IDR","value":829988}`, 82998800, "IDR", true},
		{"bare decimal number", `12504.00`, 1250400, "", true},

		{"zero", `"0.00"`, 0, "", true},
		{"negative", `"-2500.50"`, -250050, "", true},
		{"one decimal place is padded", `"10.5"`, 1050, "", true},
		{"trailing zeros beyond two are fine", `"10.5000"`, 1050, "", true},

		// Absent, which is not the same as zero: a failed disbursement carries
		// balance_after "" and processed_timestamp "".
		{"null", `null`, 0, "", false},
		{"empty string", `""`, 0, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var a Amount
			if err := json.Unmarshal([]byte(tc.in), &a); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.in, err)
			}
			if a.Minor() != tc.wantMinor {
				t.Errorf("Minor() = %d, want %d", a.Minor(), tc.wantMinor)
			}
			if a.Currency != tc.wantCur {
				t.Errorf("Currency = %q, want %q", a.Currency, tc.wantCur)
			}
			if a.Set != tc.wantSet {
				t.Errorf("Set = %v, want %v", a.Set, tc.wantSet)
			}
		})
	}
}

func TestAmountRejectsMalformed(t *testing.T) {
	for _, in := range []string{`"abc"`, `"12.ab"`, `"1.234"`, `"12."`, `"1,234.00"`} {
		t.Run(in, func(t *testing.T) {
			var a Amount
			if err := json.Unmarshal([]byte(in), &a); err == nil {
				t.Errorf("want an error, got %s", a)
			}
		})
	}
}

func TestAmountRupiah(t *testing.T) {
	t.Run("whole amounts convert", func(t *testing.T) {
		var a Amount
		if err := json.Unmarshal([]byte(`"12504.00"`), &a); err != nil {
			t.Fatal(err)
		}
		got, err := a.Rupiah()
		if err != nil {
			t.Fatalf("Rupiah: %v", err)
		}
		if got != 12504 {
			t.Errorf("got %d, want 12504", got)
		}
	})

	t.Run("fractional amounts refuse rather than truncate", func(t *testing.T) {
		// This is the bug the type exists to prevent: fmt.Sscanf("%d") reads
		// "1234.56" as 1234 and reports no error at all, and a balance comparison
		// built on that is silently wrong.
		var a Amount
		if err := json.Unmarshal([]byte(`"1234.56"`), &a); err != nil {
			t.Fatal(err)
		}
		if got, err := a.Rupiah(); err == nil {
			t.Errorf("got %d with no error; want a refusal", got)
		}
	})
}

func TestAmountString(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{123456, "1234.56"},
		{1250400, "12504.00"},
		{0, "0.00"},
		{-250050, "-2500.50"},
		{5, "0.05"},
	}
	for _, tc := range tests {
		a := Amount{minor: tc.minor, Set: true}
		if got := a.String(); got != tc.want {
			t.Errorf("minor %d: got %q, want %q", tc.minor, got, tc.want)
		}
	}
}

func TestNewAmount(t *testing.T) {
	a := NewAmount(50000, "IDR")
	if a.Minor() != 5000000 {
		t.Errorf("Minor() = %d, want 5000000", a.Minor())
	}
	got, err := a.Rupiah()
	if err != nil || got != 50000 {
		t.Errorf("Rupiah() = %d, %v", got, err)
	}
}

func TestMillisTimeUnmarshal(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantUnix int64
		wantSet  bool
	}{
		{"quoted millis", `"1766978961000"`, 1766978961, true},
		{"bare millis", `1766978961000`, 1766978961, true},
		{"empty string means absent", `""`, 0, false},
		{"null means absent", `null`, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m MillisTime
			if err := json.Unmarshal([]byte(tc.in), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m.Set != tc.wantSet {
				t.Fatalf("Set = %v, want %v", m.Set, tc.wantSet)
			}
			if m.Set && m.Unix() != tc.wantUnix {
				t.Errorf("Unix() = %d, want %d", m.Unix(), tc.wantUnix)
			}
		})
	}
}
