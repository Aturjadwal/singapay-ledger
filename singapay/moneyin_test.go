package singapay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCreatePaymentLink(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"status":200,"success":true,"data":{
			"id":103,"reff_no":"INV-2026-001","title":"INV-2026-001",
			"payment_url":"https://sandbox-paymentlink.singapay.id/b2b/INV-2026-001",
			"status":"open","max_usage":1,"current_usage":0,
			"total_amount":150000,"customer_pays_fee":false,
			"expired_at":"2026-09-08T13:00:00+07:00","created_at":"2026-09-08T12:00:00+07:00"}}`)
	})
	c := testClient(t, srv.URL)

	link, err := c.CreatePaymentLink(context.Background(), "01K9", CreatePaymentLinkRequest{
		ReffNo:                   "INV-2026-001",
		Type:                     PaymentLinkTotal,
		TotalAmount:              150000,
		ExpiredAt:                "2026-09-08T13:00:00+07:00",
		WhitelistedPaymentMethod: []string{"VA_BRI"},
	})
	if err != nil {
		t.Fatalf("CreatePaymentLink: %v", err)
	}

	if got.Path != "/api/v2.0/payment-link/01K9" {
		t.Errorf("path = %s", got.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	// expired_at is an absolute timestamp here rather than a lifetime in minutes.
	if body["expired_at"] != "2026-09-08T13:00:00+07:00" {
		t.Errorf("expired_at = %v", body["expired_at"])
	}
	if _, sent := body["customer_pays_fee"]; sent {
		t.Error("customer_pays_fee is not an input; sending it is meaningless")
	}

	if link.PaymentURL == "" {
		t.Error("want a payment_url to send the payer to")
	}
	// Always false on v2 — charging the fee to the customer has to be done by
	// grossing up total_amount instead.
	if link.CustomerPaysFee {
		t.Error("v2 links never charge the fee to the customer")
	}
}

func TestCreatePaymentLinkDefaultsToTotal(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"status":200,"success":true,"data":{"id":1,"reff_no":"X"}}`)
	})
	c := testClient(t, srv.URL)

	if _, err := c.CreatePaymentLink(context.Background(), "01K9", CreatePaymentLinkRequest{
		ReffNo: "X", TotalAmount: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(got.Body, &body)
	if body["payment_link_type"] != string(PaymentLinkTotal) {
		t.Errorf("payment_link_type = %v, want total", body["payment_link_type"])
	}
}

func TestCreateVirtualAccount(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"status":200,"success":true,"data":{
			"id":"01K946KF851RK7FX075GJHBVKF","number":"9090583126022726",
			"name":"Toko Budi","merchant_reff_no":"INV-2026-001","code":"VA_BNI",
			"bank":{"short_name":"BRI","number":"002","swift_code":"BRINIDJA"},
			"amount":{"value":"100000.00","currency":"IDR"},
			"amount_type":"closed","status":"active","kind":"temporary",
			"current_usage":0,"expired_at":1774000000000}}`)
	})
	c := testClient(t, srv.URL)

	va, err := c.CreateVirtualAccount(context.Background(), "01K9", CreateVirtualAccountRequest{
		BankCode:       BankBRI,
		Kind:           VATemporary,
		MerchantReffNo: "INV-2026-001",
		Amount:         100000,
		MaxUsage:       1,
		ExpiredAt:      "1774000000000",
	})
	if err != nil {
		t.Fatalf("CreateVirtualAccount: %v", err)
	}

	if got.Path != "/api/v1.0/virtual-accounts/01K9" {
		t.Errorf("path = %s", got.Path)
	}
	var body map[string]any
	_ = json.Unmarshal(got.Body, &body)
	// amount_type defaults to closed — an open VA would let the payer pay any amount.
	if body["amount_type"] != string(VAClosed) {
		t.Errorf("amount_type = %v, want closed", body["amount_type"])
	}
	// Milliseconds here, ISO 8601 on a payment link. Same concept, two encodings.
	if body["expired_at"] != "1774000000000" {
		t.Errorf("expired_at = %v", body["expired_at"])
	}

	// The number to show the payer arrives immediately, unlike a payment link where
	// the channel is only chosen later.
	if va.Number != "9090583126022726" {
		t.Errorf("Number = %q", va.Number)
	}
	if va.Code != "VA_BNI" {
		t.Errorf("Code = %q — this is the spelling a fee table must use", va.Code)
	}
}

func TestVATransactionNetAmount(t *testing.T) {
	body := `{"status":200,"success":true,"data":[{
		"transaction_id":"VA-20251024-0001","merchant_reff_no":"INV-2026-001",
		"va_number":"88810012345678","status":"paid",
		"amount":{"value":"100000.00","currency":"IDR"},
		"fees":{"name":"BRI Virtual Account","amount":4500,"currency":"IDR"},
		"has_settle":true,"settle_at":1729753200000}],
		"pagination":{"count":1,"total":1,"per_page":25,"current_page":1,"total_pages":1}}`

	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, body)
	})
	c := testClient(t, srv.URL)

	settled := true
	txs, page, err := c.ListVATransactions(context.Background(), "01K9", SettlementWindow{
		SettleFrom: "2026-06-01T00:00:00+07:00",
		SettleTo:   "2026-06-17T23:59:59+07:00",
		Settled:    &settled,
	})
	if err != nil {
		t.Fatalf("ListVATransactions: %v", err)
	}

	// The settlement window is how a batch's rows are reconstructed, since the
	// settlement webhook reports totals but never lists its transactions.
	for _, want := range []string{"settle_at_from=", "settle_at_to=", "has_settle=true"} {
		if !strings.Contains(got.RawPath, want) {
			t.Errorf("query %q missing %q", got.RawPath, want)
		}
	}

	if page.Total != 1 || len(txs) != 1 {
		t.Fatalf("got %d rows, page %+v", len(txs), page)
	}
	net, err := txs[0].NetAmount().Rupiah()
	if err != nil {
		t.Fatal(err)
	}
	// 100000 charged less a 4500 channel fee: this is what reaches the balance, and
	// the pair a fee-mismatch reconciliation compares against expectation.
	if net != 95500 {
		t.Errorf("NetAmount = %d, want 95500", net)
	}
}

func TestGenerateQRISCarriesTheRate(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		if c.Path != "/api/v1.0/qris-dynamic/01K9/generate-qr" {
			t.Errorf("path = %s", c.Path)
		}
		writeJSON(w, 200, `{"status":200,"success":true,"data":{
			"id":103,"reff_no":"QR-1","merchant_reff_no":"INV-2026-001","status":"open",
			"qr_data":"00020101021226620015ID.SINGAPAY.WWW","type":"mpm-dynamic",
			"amount":10000,"total_amount":10000,
			"mdr_percentage":1.5,"mdr_cost":150,"our_margin":50,"vendor_fee":150,
			"settled_to_merchant_amount":9750,
			"created_at":"2026-09-08T12:00:00+07:00"}}`)
	})
	c := testClient(t, srv.URL)

	q, err := c.GenerateQRIS(context.Background(), "01K9", GenerateQRISRequest{
		Amount:         10000,
		MerchantReffNo: "INV-2026-001",
	})
	if err != nil {
		t.Fatalf("GenerateQRIS: %v", err)
	}

	if q.QRData == "" {
		t.Error("want qr_data to render")
	}
	// QRIS is the only channel that returns the rate itself, not just the amount —
	// which makes it the one place a fee table can be verified rather than trusted.
	if q.MDRPercentage != 1.5 {
		t.Errorf("MDRPercentage = %v, want 1.5", q.MDRPercentage)
	}
	total, err := q.TotalFee().Rupiah()
	if err != nil {
		t.Fatal(err)
	}
	if total != 350 { // mdr 150 + vendor 150 + margin 50
		t.Errorf("TotalFee = %d, want 350", total)
	}
}

func TestCreateEwalletOrderPutsAccountInBody(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"response_code":"SP000","response_message":"Successfully","data":{
			"id":42,"account_id":"01K9","reff_no":"EW-1","merchant_reff_no":"INV-2026-001",
			"status":"pending","ewallet_vendor":"EWALLET_DANA",
			"amount":95000,"total_amount":100000,"merchant_fee":5000,"net_amount":95000,
			"checkout_url":"https://pay.example.com/x"}}`)
	})
	c := testClient(t, srv.URL)

	tx, err := c.CreateEwalletOrder(context.Background(), CreateEwalletOrderRequest{
		AccountID:      "01K9",
		Amount:         100000,
		Vendor:         "EWALLET_DANA",
		MerchantReffNo: "INV-2026-001",
	})
	if err != nil {
		t.Fatalf("CreateEwalletOrder: %v", err)
	}

	if got.Path != "/api/v2.0/ewallet-native/create-order" {
		t.Errorf("path = %s", got.Path)
	}
	var body map[string]any
	_ = json.Unmarshal(got.Body, &body)
	if body["account_id"] != "01K9" {
		t.Errorf("account_id must travel in the body on v2, got %v", body["account_id"])
	}
	if tx.CheckoutURL == "" {
		t.Error("want a checkout_url")
	}
	fee, _ := tx.MerchantFee.Rupiah()
	if fee != 5000 {
		t.Errorf("MerchantFee = %d, want 5000", fee)
	}
}

func TestListPaymentMethods(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, `{"status":200,"success":true,"data":{
			"payment_methods":[
				{"code":"VA_BRI","name":"VA BRI","group":"va","desc":"Virtual Account BRI"},
				{"code":"QRIS","name":"QRIS","group":"qris","desc":null}],
			"available_codes":["VA_BRI","QRIS"]}}`)
	})
	c := testClient(t, srv.URL)

	methods, err := c.ListPaymentMethods(context.Background())
	if err != nil {
		t.Fatalf("ListPaymentMethods: %v", err)
	}
	if len(methods) != 2 || methods[0].Code != "VA_BRI" {
		t.Fatalf("got %+v", methods)
	}
	// These codes are the only spellings the API accepts; a fee table has to key on them.
	for _, m := range methods {
		if m.Code == "" || m.Group == "" {
			t.Errorf("incomplete method %+v", m)
		}
	}
}
