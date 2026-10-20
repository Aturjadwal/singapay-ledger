package singapay

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestListSettlementTransactions(t *testing.T) {
	// Two rows the way the documentation shapes them: a VA payment with every field, and
	// a QRIS payment with no processed time, a fee in sen-precise decimals, and no account.
	body := `{"status":200,"success":true,"data":[
		{"transaction_id":"VA-20251024-0001H9X8ZK","merchant_reff_no":"INV-20260916070421-ETVYRH",
		 "account":{"id":"01H9X8ZK3M7QF4N2VTB6RYJWPC","name":"Toko Sumber Rejeki"},
		 "fees":{"name":"BRI Virtual Account","amount":4500,"currency":"IDR"},
		 "amount":{"value":"100000.00","currency":"IDR"},
		 "post_timestamp":"1729748679474","processed_timestamp":"1729748707000",
		 "has_settle":true,"settle_at":1729753200000},
		{"transaction_id":"647179096636882876","merchant_reff_no":"INV-20261002183927-YF2GYY",
		 "account":null,
		 "fees":{"name":"QRIS","amount":119.84,"currency":"IDR"},
		 "amount":{"value":"39275.00","currency":"IDR"},
		 "post_timestamp":"1729748679474","processed_timestamp":null,
		 "has_settle":true,"settle_at":1729753200000}],
		"pagination":{"count":2,"total":52,"per_page":50,"current_page":2,"total_pages":2}}`

	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, body)
	})
	c := testClient(t, srv.URL)

	refunded := false
	rows, page, err := c.ListSettlementTransactions(context.Background(), "3937", ListSettlementTransactionsOptions{
		Page:            2,
		PerPage:         50,
		TransactionType: SettlementTransactionTypeQRIS,
		Refunded:        &refunded,
	})
	if err != nil {
		t.Fatalf("ListSettlementTransactions: %v", err)
	}

	if got.Method != http.MethodGet || got.Path != "/api/v1.0/settlement-transactions/3937" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	query, err := url.ParseQuery(got.RawPath[len(got.Path)+1:])
	if err != nil {
		t.Fatalf("query %q: %v", got.RawPath, err)
	}
	for key, want := range map[string]string{"page": "2", "per_page": "50", "transaction_type": "QRIS", "is_refunded": "false"} {
		if query.Get(key) != want {
			t.Errorf("query %s = %q, want %q (sent %q)", key, query.Get(key), want, got.RawPath)
		}
	}
	if got.Headers.Get("Authorization") != "Bearer "+testToken || got.Headers.Get("X-PARTNER-ID") != testPartner {
		t.Errorf("auth headers = %q / %q", got.Headers.Get("Authorization"), got.Headers.Get("X-PARTNER-ID"))
	}
	// A read: no request signature, which only money-out endpoints expect.
	if got.Headers.Get("X-Signature") != "" {
		t.Error("a settlement listing is not a signed call")
	}

	if page != (Pagination{Count: 2, Total: 52, PerPage: 50, CurrentPage: 2, TotalPages: 2}) {
		t.Errorf("page = %+v", page)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}

	va := rows[0]
	if va.TransactionID != "VA-20251024-0001H9X8ZK" || va.MerchantReffNo != "INV-20260916070421-ETVYRH" {
		t.Errorf("ids = %q / %q", va.TransactionID, va.MerchantReffNo)
	}
	if va.Account == nil || va.Account.ID != "01H9X8ZK3M7QF4N2VTB6RYJWPC" || va.Account.Name != "Toko Sumber Rejeki" {
		t.Errorf("account = %+v", va.Account)
	}
	if va.Amount.Minor() != 10000000 || va.Fees.Amount.Minor() != 450000 || va.Fees.Name != "BRI Virtual Account" {
		t.Errorf("amount = %d, fee = %d %q", va.Amount.Minor(), va.Fees.Amount.Minor(), va.Fees.Name)
	}
	if !va.HasSettle || !va.SettleAt.Set || !va.SettleAt.Equal(time.UnixMilli(1729753200000)) {
		t.Errorf("settle = %v %+v", va.HasSettle, va.SettleAt)
	}
	if !va.PostedAt.Set || !va.ProcessedAt.Set || !va.ProcessedAt.Equal(time.UnixMilli(1729748707000)) {
		t.Errorf("posted %+v, processed %+v", va.PostedAt, va.ProcessedAt)
	}

	qris := rows[1]
	// account: null is "Singapay did not say", not an account with an empty id.
	if qris.Account != nil {
		t.Errorf("account = %+v, want nil", qris.Account)
	}
	// 119.84 is a real QRIS fee; the sen survive.
	if !qris.Fees.Amount.Set || qris.Fees.Amount.Minor() != 11984 {
		t.Errorf("fee = %+v, want 11984 sen", qris.Fees.Amount)
	}
	if qris.ProcessedAt.Set {
		t.Errorf("processed = %+v, want unset: QRIS reports no processed time", qris.ProcessedAt)
	}
}

func TestListSettlementTransactionsByReference(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"status":200,"success":true,"data":[]}`)
	})
	c := testClient(t, srv.URL)

	rows, page, err := c.ListSettlementTransactions(context.Background(), " SETTLEMENT-647-6AC4EDE197FB9 ", ListSettlementTransactionsOptions{})
	if err != nil {
		t.Fatalf("ListSettlementTransactions: %v", err)
	}
	// The reference works in place of the id, trimmed; the zero options send no query.
	if got.RawPath != "/api/v1.0/settlement-transactions/SETTLEMENT-647-6AC4EDE197FB9" {
		t.Errorf("request = %s", got.RawPath)
	}
	if len(rows) != 0 || page != (Pagination{}) {
		t.Errorf("rows = %v, page = %+v", rows, page)
	}
}

func TestListSettlementTransactionsRequiresIdentifier(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		t.Errorf("no request should be made without a settlement, got %s", c.RawPath)
		writeJSON(w, 200, `{"status":200,"success":true,"data":[]}`)
	})
	c := testClient(t, srv.URL)

	for _, identifier := range []string{"", "   "} {
		if _, _, err := c.ListSettlementTransactions(context.Background(), identifier, ListSettlementTransactionsOptions{}); err == nil {
			t.Errorf("identifier %q: want an error", identifier)
		}
	}
}

func TestListSettlementTransactionsNotFound(t *testing.T) {
	// What Singapay answers for a settlement that is not completed, or not this merchant's.
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 404, `{"status":404,"success":false,"error":{"code":404,"message":"Account not found."}}`)
	})
	c := testClient(t, srv.URL)

	_, _, err := c.ListSettlementTransactions(context.Background(), "3937", ListSettlementTransactionsOptions{})
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("want a *Error, got %T %v", err, err)
	}
	if e.StatusCode != http.StatusNotFound || e.Message != "Account not found." {
		t.Errorf("error = %d %q", e.StatusCode, e.Message)
	}
}

// A row is read leniently: the listing is shown to a person, and one odd field must not cost
// the whole page. Each value below fails the strict types this package uses for money paths.
func TestSettlementTransactionDecodesOddRowsLeniently(t *testing.T) {
	body := `{"status":200,"success":true,"data":[
		{"transaction_id":647179096636882876,"merchant_reff_no":null,"account":[],
		 "fees":{"name":"QRIS","amount":1.1300000000000001,"currency":"IDR"},
		 "amount":{"value":"39275.004","currency":"IDR"},
		 "post_timestamp":1759395328000,"processed_timestamp":"","has_settle":1,
		 "settle_at":"2026-10-07T07:00:00+07:00"},
		{"transaction_id":"VA-1","merchant_reff_no":"INV-1","account":{"id":"01SUB","name":null},
		 "fees":[],"amount":"not a number","post_timestamp":"yesterday","has_settle":"false","settle_at":null}],
		"pagination":{"count":2,"total":2,"per_page":25,"current_page":1,"total_pages":1}}`
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, body)
	})
	c := testClient(t, srv.URL)

	rows, page, err := c.ListSettlementTransactions(context.Background(), "3937", ListSettlementTransactionsOptions{})
	if err != nil {
		t.Fatalf("ListSettlementTransactions: %v", err)
	}
	if page.Total != 2 || len(rows) != 2 {
		t.Fatalf("got %d rows, page %+v", len(rows), page)
	}

	odd := rows[0]
	if odd.TransactionID != "647179096636882876" {
		t.Errorf("a numeric id reads as its digits, got %q", odd.TransactionID)
	}
	if odd.MerchantReffNo != "" || odd.Account != nil {
		t.Errorf("null reference and [] account read as absent, got %q / %+v", odd.MerchantReffNo, odd.Account)
	}
	if !odd.Fees.Amount.Set || odd.Fees.Amount.Minor() != 113 {
		t.Errorf("float noise is rounded to the sen: fee = %+v", odd.Fees.Amount)
	}
	if !odd.Amount.Set || odd.Amount.Minor() != 3927500 || odd.Amount.Currency != "IDR" {
		t.Errorf("a third decimal is rounded to the sen: amount = %+v", odd.Amount)
	}
	if !odd.HasSettle {
		t.Error("has_settle 1 is true")
	}
	if !odd.PostedAt.Set || !odd.PostedAt.Equal(time.UnixMilli(1759395328000)) || odd.ProcessedAt.Set {
		t.Errorf("posted %+v, processed %+v", odd.PostedAt, odd.ProcessedAt)
	}
	if want := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC); !odd.SettleAt.Set || !odd.SettleAt.Equal(want) {
		t.Errorf("an ISO settle_at is read too: %+v", odd.SettleAt)
	}

	bare := rows[1]
	if bare.Account == nil || bare.Account.ID != "01SUB" || bare.Account.Name != "" {
		t.Errorf("account = %+v", bare.Account)
	}
	if bare.Fees.Amount.Set || bare.Amount.Set {
		t.Errorf("unreadable figures are unset: fee %+v, amount %+v", bare.Fees.Amount, bare.Amount)
	}
	if bare.PostedAt.Set || bare.SettleAt.Set || bare.HasSettle {
		t.Errorf("unreadable timestamps are unset, \"false\" is false: %+v %+v %v", bare.PostedAt, bare.SettleAt, bare.HasSettle)
	}
}

// Only a row that is not an object fails, and it fails the call: that is a different shape
// altogether, not an odd field.
func TestSettlementTransactionThatIsNotAnObjectFails(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, `{"status":200,"success":true,"data":["INV-1"]}`)
	})
	c := testClient(t, srv.URL)

	if _, _, err := c.ListSettlementTransactions(context.Background(), "3937", ListSettlementTransactionsOptions{}); err == nil {
		t.Error("want an error for a row that is a string")
	}
}
