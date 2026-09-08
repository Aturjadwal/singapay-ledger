package singapay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Amount is a monetary value as Singapay writes it.
//
// The same value arrives in three shapes depending on which endpoint answered:
// "1234.56" (balance inquiry), "500000" (account transfer), 100000 (VA webhook). Amount
// accepts all of them and stores minor units — sen — so nothing is lost on the way in.
//
// The ledger works in whole rupiah, so reading one out goes through [Amount.Rupiah],
// which returns an error rather than rounding. That is deliberate: reading a balance with
// fmt.Sscanf("%d", …) turns "1234.56" into 1234 and reports success, and a balance
// comparison that is quietly wrong is worse than one that fails.
type Amount struct {
	minor    int64 // value × 100
	Currency string
	// Set distinguishes a real zero from an absent field. Singapay omits or nulls
	// balance_after on a failed disbursement, and "absent" must not read as "zero
	// balance".
	Set bool
}

// NewAmount builds an Amount from whole rupiah.
func NewAmount(rupiah int64, currency string) Amount {
	return Amount{minor: rupiah * 100, Currency: currency, Set: true}
}

// Minor returns the value in sen. Always exact.
func (a Amount) Minor() int64 { return a.minor }

// Rupiah returns the value in whole rupiah, erroring if there is a fractional remainder
// that rounding would discard.
func (a Amount) Rupiah() (int64, error) {
	if a.minor%100 != 0 {
		return 0, fmt.Errorf("singapay: %s is not a whole rupiah amount", a.String())
	}
	return a.minor / 100, nil
}

// IsZero reports whether the amount is zero. An unset amount is zero.
func (a Amount) IsZero() bool { return a.minor == 0 }

// String renders the amount the way Singapay writes it, with two decimals.
func (a Amount) String() string {
	sign := ""
	m := a.minor
	if m < 0 {
		sign, m = "-", -m
	}
	return fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)
}

// moneyObject is the {value, currency} wrapper Singapay puts around most amounts.
type moneyObject struct {
	Value    json.RawMessage `json:"value"`
	Currency string          `json:"currency"`
}

// UnmarshalJSON accepts a bare number, a quoted decimal, or a {value, currency} object.
func (a *Amount) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*a = Amount{}
		return nil
	}

	if trimmed[0] == '{' {
		var obj moneyObject
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		if err := a.UnmarshalJSON(obj.Value); err != nil {
			return err
		}
		a.Currency = obj.Currency
		return nil
	}

	// A quoted decimal and a bare number are the same token once the quotes are gone.
	// Parsing the text rather than going through float64 is what keeps this exact —
	// 1766978961000 and 0.07 both survive.
	raw := trimmed
	if raw[0] == '"' {
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			return fmt.Errorf("singapay: malformed amount %s: %w", trimmed, err)
		}
		raw = strings.TrimSpace(unquoted)
	}
	if raw == "" {
		*a = Amount{}
		return nil
	}

	minor, err := parseDecimalToMinor(raw)
	if err != nil {
		return err
	}
	*a = Amount{minor: minor, Currency: a.Currency, Set: true}
	return nil
}

// MarshalJSON writes the amount as a JSON number with two decimals.
func (a Amount) MarshalJSON() ([]byte, error) {
	if !a.Set {
		return []byte("null"), nil
	}
	return []byte(a.String()), nil
}

var errMalformedAmount = errors.New("singapay: malformed amount")

// parseDecimalToMinor converts a decimal string to sen without touching floating point.
//
// Fractional digits beyond the second are accepted only when they are zeros. Singapay
// documents two decimals everywhere; a third significant digit means the contract moved,
// and silently dropping it is how money goes missing one sen at a time.
func parseDecimalToMinor(s string) (int64, error) {
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("%w: empty", errMalformedAmount)
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", errMalformedAmount, s, err)
	}

	var cents int64
	if hasFrac {
		if fracPart == "" {
			return 0, fmt.Errorf("%w %q: trailing decimal point", errMalformedAmount, s)
		}
		for _, r := range fracPart {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("%w %q: non-digit in fraction", errMalformedAmount, s)
			}
		}
		padded := fracPart
		if len(padded) < 2 {
			padded += strings.Repeat("0", 2-len(padded))
		}
		cents, err = strconv.ParseInt(padded[:2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w %q: %w", errMalformedAmount, s, err)
		}
		if strings.Trim(padded[2:], "0") != "" {
			return 0, fmt.Errorf("%w %q: more precision than IDR carries", errMalformedAmount, s)
		}
	}

	minor := whole*100 + cents
	if neg {
		minor = -minor
	}
	return minor, nil
}
