package singapay

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MillisTime is a timestamp Singapay writes as 13-digit Unix milliseconds.
//
// It arrives quoted ("1766978961000") on most responses and bare on some, and is an empty
// string rather than null when absent — a failed disbursement carries
// "processed_timestamp": "". Set distinguishes absent from the epoch.
type MillisTime struct {
	time.Time
	Set bool
}

func (m *MillisTime) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" || raw == "null" {
		*m = MillisTime{}
		return nil
	}
	if raw[0] == '"' {
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			return fmt.Errorf("singapay: malformed timestamp %s: %w", raw, err)
		}
		raw = strings.TrimSpace(unquoted)
	}
	if raw == "" {
		*m = MillisTime{}
		return nil
	}

	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("singapay: malformed timestamp %q: %w", raw, err)
	}
	*m = MillisTime{Time: time.UnixMilli(millis), Set: true}
	return nil
}

func (m MillisTime) MarshalJSON() ([]byte, error) {
	if !m.Set {
		return []byte("null"), nil
	}
	return json.Marshal(strconv.FormatInt(m.UnixMilli(), 10))
}

// TextTime is a timestamp Singapay writes as human-readable text — "26 Dec 2025 13:35:45"
// — in money-in and settlement webhooks.
//
// Money-out webhooks use [MillisTime] for the same field names instead. There is no rule
// to infer which is which; it follows the endpoint, so the struct decides.
//
// No timezone is transmitted. Singapay operates in Asia/Jakarta, so that is assumed —
// which is a guess worth confirming, since reading these as UTC shifts every timestamp by
// seven hours.
type TextTime struct {
	time.Time
	Set bool
}

// textTimeLayouts covers the padded form Singapay documents and the unpadded one PHP's
// date() can emit for single-digit days.
var textTimeLayouts = []string{
	"02 Jan 2006 15:04:05",
	"2 Jan 2006 15:04:05",
}

func (t *TextTime) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" || raw == "null" {
		*t = TextTime{}
		return nil
	}
	if raw[0] == '"' {
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			return fmt.Errorf("singapay: malformed timestamp %s: %w", raw, err)
		}
		raw = strings.TrimSpace(unquoted)
	}
	if raw == "" {
		*t = TextTime{}
		return nil
	}

	for _, layout := range textTimeLayouts {
		if parsed, err := time.ParseInLocation(layout, raw, jakarta); err == nil {
			*t = TextTime{Time: parsed, Set: true}
			return nil
		}
	}
	return fmt.Errorf("singapay: unrecognised timestamp %q", raw)
}

func (t TextTime) MarshalJSON() ([]byte, error) {
	if !t.Set {
		return []byte("null"), nil
	}
	return json.Marshal(t.In(jakarta).Format(textTimeLayouts[0]))
}

// ISOTime is a timestamp Singapay writes as ISO 8601 — "2025-11-05T09:09:49.000000Z" or
// "2025-10-24T13:44:39+07:00" — on the money-in resource endpoints.
//
// That makes three timestamp encodings in one API: this, [MillisTime] on money-out, and
// [TextTime] on webhooks. Unlike the other two, this one carries its offset.
type ISOTime struct {
	time.Time
	Set bool
}

func (t *ISOTime) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" || raw == "null" {
		*t = ISOTime{}
		return nil
	}
	unquoted, err := strconv.Unquote(raw)
	if err != nil {
		return fmt.Errorf("singapay: malformed timestamp %s: %w", raw, err)
	}
	unquoted = strings.TrimSpace(unquoted)
	if unquoted == "" {
		*t = ISOTime{}
		return nil
	}

	parsed, err := time.Parse(time.RFC3339, unquoted)
	if err != nil {
		return fmt.Errorf("singapay: unrecognised timestamp %q: %w", unquoted, err)
	}
	*t = ISOTime{Time: parsed, Set: true}
	return nil
}

func (t ISOTime) MarshalJSON() ([]byte, error) {
	if !t.Set {
		return []byte("null"), nil
	}
	return json.Marshal(t.Format(time.RFC3339))
}
