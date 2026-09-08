package singapay

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// The payloads below are Singapay's own documented samples, kept verbatim so a change in
// their contract shows up here rather than in production.

const disburseSuccessBody = `{
    "response_code": "SP000",
    "response_message": "Successfully",
    "data": {
        "transaction_id": "101222025122910292195055674",
        "reference_number": "11111111118",
        "transaction_status": {"code": "00", "desc": "Success"},
        "post_timestamp": "1766978961000",
        "processed_timestamp": "1766978962000",
        "bank": {"code": "002", "name": "BRI", "account_name": "Dummy Test Account Internal", "account_number": "11111111118"},
        "gross_amount": {"currency": "IDR", "value": "12504.00"},
        "fee": {"currency": "IDR", "value": "2500"},
        "net_amount": {"currency": "IDR", "value": "10004.00"},
        "balance_after": {"currency": "IDR", "value": "829988"},
        "notes": "test transfer"
    }
}`

const disburseFailedBody = `{
    "response_code": "SP001",
    "response_message": "Transaction Failure",
    "data": {
        "transaction_id": "121222025122617513896515436",
        "reference_number": "333",
        "transaction_status": {"code": "06", "desc": "Failed"},
        "post_timestamp": "1766746298000",
        "processed_timestamp": "",
        "bank": {"code": "002", "name": "BRI", "account_name": "", "account_number": "091701064838533"},
        "gross_amount": {"currency": "IDR", "value": "12501.00"},
        "fee": {"currency": "IDR", "value": "2500"},
        "net_amount": {"currency": "IDR", "value": "10001.00"},
        "balance_after": {"currency": "IDR", "value": "0"},
        "notes": "test transfer",
        "failed_reason": "Transaction Failure : Invalid beneficiary account: Account inactive",
        "failed_code": "SP001"
    }
}`

func TestDisburseSuccess(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		if c.Path != "/api/v2.0/disbursement/transfer" {
			t.Errorf("path = %s", c.Path)
		}
		var body map[string]any
		if err := json.Unmarshal(c.Body, &body); err != nil {
			t.Fatal(err)
		}
		// account_id belongs in the body on v2, not the path. Putting it in the path
		// is the v1 contract and 404s here.
		if body["account_id"] != "01K946KF851RK7FX075GJHBVKF" {
			t.Errorf("account_id = %v", body["account_id"])
		}
		writeJSON(w, 200, disburseSuccessBody)
	})
	c := testClient(t, srv.URL)

	got, err := c.Disburse(context.Background(), DisburseRequest{
		AccountID:         "01K946KF851RK7FX075GJHBVKF",
		ReferenceNumber:   "11111111118",
		BankCode:          "002",
		BankAccountNumber: "11111111118",
		Amount:            10004,
		Notes:             "test transfer",
	})
	if err != nil {
		t.Fatalf("Disburse: %v", err)
	}

	if got.TransactionStatus() != StatusSuccess {
		t.Errorf("status = %s, want 00", got.TransactionStatus())
	}
	if !got.TransactionStatus().Succeeded() {
		t.Error("00 must report as succeeded")
	}

	// gross = net + fee, and the account is debited the gross. A caller that reserves
	// only the net drifts from Singapay's balance by the fee on every payout.
	if got.GrossAmount.Minor() != got.NetAmount.Minor()+got.Fee.Minor() {
		t.Errorf("gross %s != net %s + fee %s", got.GrossAmount, got.NetAmount, got.Fee)
	}

	fee, err := got.Fee.Rupiah()
	if err != nil || fee != 2500 {
		t.Errorf("Fee = %d, %v; want 2500", fee, err)
	}
	if !got.ProcessedAt.Set {
		t.Error("processed_timestamp should be set on a success")
	}
}

func TestDisburseFailure(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 400, disburseFailedBody)
	})
	c := testClient(t, srv.URL)

	_, err := c.Disburse(context.Background(), DisburseRequest{AccountID: "01K9", ReferenceNumber: "333"})
	if err == nil {
		t.Fatal("want an error")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("want a *Error, got %T", err)
	}
	if e.Code != CodeTransactionFailure {
		t.Errorf("Code = %q, want SP001", e.Code)
	}
	// SP001 arrives as HTTP 400, and the DOKU heuristic would call that a definite
	// refusal and release the reservation. Singapay says to inquire instead.
	if e.Outcome() != OutcomeUnknown {
		t.Errorf("Outcome = %v, want unknown", e.Outcome())
	}
	if len(e.Body) == 0 {
		t.Error("the raw body must be kept — a refused payout is argued over it")
	}
}

func TestDisburseDuplicateReference(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 400, `{"response_code":"SP004","response_message":"Duplicate Reference Number","data":{}}`)
	})
	c := testClient(t, srv.URL)

	_, err := c.Disburse(context.Background(), DisburseRequest{AccountID: "01K9", ReferenceNumber: "REF-1"})
	e, _ := AsError(err)
	if e == nil || !e.IsDuplicate() {
		t.Fatalf("want a duplicate, got %v", err)
	}
	// The original may have succeeded, so this must never release the reservation.
	if e.Outcome() == OutcomeRefused {
		t.Error("a duplicate reference must not read as a refusal")
	}
}

func TestDisbursementFailedPayloadFieldsDecode(t *testing.T) {
	var env struct {
		Data Disbursement `json:"data"`
	}
	if err := json.Unmarshal([]byte(disburseFailedBody), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := env.Data

	if d.TransactionStatus() != StatusFailed {
		t.Errorf("status = %s, want 06", d.TransactionStatus())
	}
	if d.FailedCode != "SP001" || d.FailedReason == "" {
		t.Errorf("failure detail missing: %q / %q", d.FailedCode, d.FailedReason)
	}
	// An empty-string timestamp must decode as absent, not as the epoch.
	if d.ProcessedAt.Set {
		t.Error(`processed_timestamp "" must decode as unset`)
	}
	if !d.PostedAt.Set {
		t.Error("post_timestamp should be set")
	}
}

func TestInquiryDisbursement(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		if c.Path != "/api/v2.0/disbursement/01K9/inquiry-status" {
			t.Errorf("path = %s", c.Path)
		}
		if c.Headers.Get("X-Signature") != "" {
			t.Error("inquiry-status is not a signed endpoint")
		}
		writeJSON(w, 200, disburseSuccessBody)
	})
	c := testClient(t, srv.URL)

	got, err := c.InquiryDisbursement(context.Background(), "01K9", "11111111118")
	if err != nil {
		t.Fatalf("InquiryDisbursement: %v", err)
	}
	if got.ReferenceNumber != "11111111118" {
		t.Errorf("ReferenceNumber = %q", got.ReferenceNumber)
	}
}

func TestCheckBeneficiaryInvalidIsNotAnError(t *testing.T) {
	// Singapay answers 200 + SP000 with status "invalid" for an account that does not
	// exist. Reading validity from err == nil would call it valid.
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, `{"response_code":"SP000","response_message":"Successfully","data":{"bank_code":"002","bank_account_number":"12345678","status":"invalid","bank_account_name":"","bank_name":"BRI","message":"Invalid account:Account inactive"}}`)
	})
	c := testClient(t, srv.URL)

	got, err := c.CheckBeneficiary(context.Background(), "002", "12345678")
	if err != nil {
		t.Fatalf("an invalid account is not a transport error: %v", err)
	}
	if got.IsValid() {
		t.Error("IsValid() must be false for status invalid")
	}
	if got.Message == "" {
		t.Error("the bank's reason should be carried through")
	}
}

func TestCheckBeneficiaryValid(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, `{"response_code":"SP000","response_message":"Successfully","data":{"bank_code":"002","bank_account_number":"1234567890","status":"valid","bank_account_name":"BUDI SANTOSO","bank_name":"BRI","message":null}}`)
	})
	c := testClient(t, srv.URL)

	got, err := c.CheckBeneficiary(context.Background(), "002", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsValid() || got.AccountName != "BUDI SANTOSO" {
		t.Errorf("got %+v", got)
	}
}

func TestCheckFee(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		if c.Path != "/api/v1.0/disbursement/01K9/check-fee" {
			t.Errorf("path = %s", c.Path)
		}
		var body map[string]any
		_ = json.Unmarshal(c.Body, &body)
		// check-fee is a v1 endpoint and takes SWIFT only — a three-digit code here
		// is rejected, which is why callers should store SWIFT.
		if body["bank_swift_code"] != "BRINIDJA" {
			t.Errorf("bank_swift_code = %v", body["bank_swift_code"])
		}
		writeJSON(w, 200, `{"status":200,"success":true,"data":{"gross_amount":"51000.00","transfer_fee":"1000.00","net_amount":"50000.00","currency":"IDR","beneficiary":{"full_name":"Bank Rakyat Indonesia","short_name":"BRI"}}}`)
	})
	c := testClient(t, srv.URL)

	got, err := c.CheckFee(context.Background(), "01K9", "BRINIDJA", 50000)
	if err != nil {
		t.Fatalf("CheckFee: %v", err)
	}

	gross, err := got.GrossAmount.Rupiah()
	if err != nil {
		t.Fatal(err)
	}
	net, _ := got.NetAmount.Rupiah()
	fee, _ := got.TransferFee.Rupiah()
	if gross != net+fee {
		t.Errorf("gross %d != net %d + fee %d", gross, net, fee)
	}
	// This is the number a caller must reserve, not the 50000 it asked for.
	if gross != 51000 {
		t.Errorf("gross = %d, want 51000", gross)
	}
}
