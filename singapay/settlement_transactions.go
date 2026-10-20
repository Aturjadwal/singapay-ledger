package singapay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SettlementTransaction is one money-in transaction inside a settlement, as
// GET /api/v1.0/settlement-transactions/{identifier} lists it.
//
// It is what the settlement webhook leaves out. A settlement.completed delivery names the
// batch and its totals and nothing the batch covered; this listing is the batch's contents,
// read back from Singapay when someone asks. Each row mirrors the money-in transaction
// shape the VA list uses, less what only one product has: there is no status, and no
// product type — the type exists only as a filter
// ([ListSettlementTransactionsOptions.TransactionType]).
//
// Unlike the money-moving reads, a row is decoded leniently (see UnmarshalJSON): the listing
// is only ever shown to a person, and one odd field must not cost the whole page.
type SettlementTransaction struct {
	// TransactionID is Singapay's business id for the payment: a VA payment's
	// transaction_id ("VA-20251024-0001H9X8ZK"), every other product's reff_no.
	TransactionID string `json:"transaction_id"`
	// MerchantReffNo is the reference the merchant created the payment with. This ledger
	// sends the invoice number as merchant_reff_no on VA, QRIS and e-wallet; a payment
	// link is created with it as reff_no instead.
	MerchantReffNo string `json:"merchant_reff_no"`

	// Account is the sub-account the payment was made to, nil when Singapay sent null.
	Account *SettlementTransactionAccount `json:"account"`

	Fees struct {
		// Name is the payment method as Singapay names it ("BRI Virtual Account").
		Name string `json:"name"`
		// Amount is the merchant fee; for QRIS, vendor_fee + our_margin. Unset (Set
		// false) when Singapay sent null, which is not the same as a zero fee.
		Amount   Amount `json:"amount"`
		Currency string `json:"currency"`
	} `json:"fees"`

	// Amount is what the payment was for, two decimals ({"value":"1234.56",...}).
	Amount Amount `json:"amount"`

	PostedAt MillisTime `json:"post_timestamp"`
	// ProcessedAt is when the payment was paid; Singapay sends none for QRIS.
	ProcessedAt MillisTime `json:"processed_timestamp"`

	HasSettle bool       `json:"has_settle"`
	SettleAt  MillisTime `json:"settle_at"`
}

// UnmarshalJSON reads a row leniently, field by field.
//
// The strict types this package uses elsewhere refuse what they cannot read exactly, and
// that is right where a figure decides a booking. Here nothing is booked, and the listing is
// a PHP service's JSON: a fee summed in floating point (vendor_fee + our_margin can print
// 1.1300000000000001), a boolean written 1, an empty object written []. So every field
// takes what it can — an amount with float noise is rounded to the sen, a figure or a
// timestamp it cannot read at all is left unset, an id written as a number becomes its
// digits — and only a row that is not a JSON object fails.
func (t *SettlementTransaction) UnmarshalJSON(b []byte) error {
	var raw struct {
		TransactionID      json.RawMessage `json:"transaction_id"`
		MerchantReffNo     json.RawMessage `json:"merchant_reff_no"`
		Account            json.RawMessage `json:"account"`
		Fees               json.RawMessage `json:"fees"`
		Amount             json.RawMessage `json:"amount"`
		PostTimestamp      json.RawMessage `json:"post_timestamp"`
		ProcessedTimestamp json.RawMessage `json:"processed_timestamp"`
		HasSettle          json.RawMessage `json:"has_settle"`
		SettleAt           json.RawMessage `json:"settle_at"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("singapay: a settlement transaction is not an object: %w", err)
	}

	*t = SettlementTransaction{
		TransactionID:  looseString(raw.TransactionID),
		MerchantReffNo: looseString(raw.MerchantReffNo),
		Amount:         looseAmount(raw.Amount),
		PostedAt:       looseMillisTime(raw.PostTimestamp),
		ProcessedAt:    looseMillisTime(raw.ProcessedTimestamp),
		HasSettle:      looseBool(raw.HasSettle),
		SettleAt:       looseMillisTime(raw.SettleAt),
	}
	if isObject(raw.Account) {
		var account struct {
			ID   json.RawMessage `json:"id"`
			Name json.RawMessage `json:"name"`
		}
		if json.Unmarshal(raw.Account, &account) == nil {
			if id, name := looseString(account.ID), looseString(account.Name); id != "" || name != "" {
				t.Account = &SettlementTransactionAccount{ID: id, Name: name}
			}
		}
	}
	if isObject(raw.Fees) {
		var fees struct {
			Name     json.RawMessage `json:"name"`
			Amount   json.RawMessage `json:"amount"`
			Currency json.RawMessage `json:"currency"`
		}
		if json.Unmarshal(raw.Fees, &fees) == nil {
			t.Fees.Name = looseString(fees.Name)
			t.Fees.Amount = looseAmount(fees.Amount)
			t.Fees.Currency = looseString(fees.Currency)
		}
	}
	return nil
}

func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// looseString is a string, or a number's digits, or "" for anything else (null included).
func looseString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return strings.TrimSpace(s)
		}
		return ""
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		if json.Unmarshal(trimmed, &n) == nil {
			return n.String()
		}
	}
	return ""
}

// looseAmount is an [Amount] read strictly when it can be, and otherwise rounded to the sen
// from whatever number it carries — a bare number, a quoted one, or {value, currency}.
// Unreadable is unset (Set false), never an error.
func looseAmount(raw json.RawMessage) Amount {
	var strict Amount
	if err := strict.UnmarshalJSON(raw); err == nil {
		return strict
	}

	value, currency := bytes.TrimSpace(raw), ""
	if isObject(value) {
		var obj moneyObject
		if json.Unmarshal(value, &obj) != nil {
			return Amount{}
		}
		value, currency = bytes.TrimSpace(obj.Value), obj.Currency
	}
	text := strings.TrimSpace(strings.Trim(string(value), `"`))
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > math.MaxInt64/100 {
		return Amount{}
	}
	return Amount{minor: int64(math.Round(f * 100)), Currency: currency, Set: true}
}

// looseMillisTime is a [MillisTime] read from 13-digit milliseconds (bare or quoted), or
// from an ISO 8601 or text timestamp if that is what arrived; unset when it is none of them.
func looseMillisTime(raw json.RawMessage) MillisTime {
	var millis MillisTime
	if err := millis.UnmarshalJSON(raw); err == nil {
		return millis
	}
	var iso ISOTime
	if err := iso.UnmarshalJSON(raw); err == nil && iso.Set {
		return MillisTime{Time: iso.Time, Set: true}
	}
	var text TextTime
	if err := text.UnmarshalJSON(raw); err == nil && text.Set {
		return MillisTime{Time: text.Time, Set: true}
	}
	return MillisTime{}
}

// looseBool is true for true and 1, quoted or not; false for anything else.
func looseBool(raw json.RawMessage) bool {
	switch strings.ToLower(strings.Trim(string(bytes.TrimSpace(raw)), `"`)) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// SettlementTransactionAccount names the sub-account a settled payment belongs to.
type SettlementTransactionAccount struct {
	// ID is the account ULID — the id the ledger stores as singapay_account_id.
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Settlement transaction types, the values [ListSettlementTransactionsOptions] filters on.
const (
	SettlementTransactionTypeVA          = "VA"
	SettlementTransactionTypeQRIS        = "QRIS"
	SettlementTransactionTypePaymentLink = "PAYMENT_LINK"
	SettlementTransactionTypeEwallet     = "EWALLET"
)

// ListSettlementTransactionsOptions pages and filters [Client.ListSettlementTransactions].
// The zero value asks for Singapay's first page at its default size (25), unfiltered.
type ListSettlementTransactionsOptions struct {
	// Page starts at 1; 0 leaves it to Singapay.
	Page int
	// PerPage is the page size; 0 leaves it to Singapay. No maximum is documented.
	PerPage int
	// TransactionType is VA, QRIS, PAYMENT_LINK or EWALLET; empty lists every type.
	TransactionType string
	// Refunded, when set, filters on the settlement-level refund state of each row.
	Refunded *bool
}

func (o ListSettlementTransactionsOptions) values() url.Values {
	q := url.Values{}
	if o.Page > 0 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	if o.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(o.PerPage))
	}
	if o.TransactionType != "" {
		q.Set("transaction_type", o.TransactionType)
	}
	if o.Refunded != nil {
		q.Set("is_refunded", strconv.FormatBool(*o.Refunded))
	}
	return q
}

// ListSettlementTransactions lists the transactions a settlement covered, one page at a
// time.
//
// identifier is the settlement's numeric id (the settlement webhook's settlement.id) or its
// reference_no; Singapay accepts either. It answers 404 — an [*Error] — for a settlement
// that is not this merchant's, and for one whose status is not completed.
//
// Nothing here is booked. The settling pass never selects on a batch's contents (see the
// ledger's settlement.go); this is for reading what a batch was, after the fact.
func (c *Client) ListSettlementTransactions(ctx context.Context, identifier string, opts ListSettlementTransactionsOptions) ([]SettlementTransaction, Pagination, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		// An empty identifier would request the collection itself, which is not this call.
		return nil, Pagination{}, fmt.Errorf("singapay: a settlement id or reference is required")
	}

	path := withQuery("/api/v1.0/settlement-transactions/"+url.PathEscape(identifier), opts.values())

	var out []SettlementTransaction
	var page Pagination
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &page); err != nil {
		return nil, Pagination{}, err
	}
	return out, page, nil
}
