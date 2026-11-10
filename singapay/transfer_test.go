package singapay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reencodeLikeSingapay is what Singapay does to a request body before it hashes it for the
// signature check. It decodes the body, every number becoming a float, and encodes it again
// with the keys sorted.
//
// PHP's json_encode prints a float in the fewest digits that read back as the same float,
// and so does encoding/json, so this reproduces Singapay for every amount the client sends.
// It was checked against PHP 8.3 json_encode(json_decode($body, true)) on the production
// bodies of 2026-11-10: 4000.00 comes back as 4000, 4000.10 as 4000.1, and 4000.42
// unchanged.
//
// The two part ways on integers past 2^53, where PHP keeps an int and a float64 cannot. No
// body here carries one.
func reencodeLikeSingapay(body []byte) (string, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// singapaySignature is the X-Signature Singapay expects. The scheme is the client's own,
// computed over Singapay's re-encoding of the body rather than the bytes it received.
func singapaySignature(method, endpoint string, body []byte, timestamp string) (string, error) {
	reencoded, err := reencodeLikeSingapay(body)
	if err != nil {
		return "", err
	}
	return requestSignature(method, endpoint, testToken, []byte(reencoded), timestamp, testSecret)
}

// The stand-in is checked against production before anything is tested against it. On
// 2026-11-09 one seller's sub-account sent two platform-fee transfers in the same pass,
// with the same token, secret and destination. Singapay refused the 4000.00 one with 401
// "Invalid X-SIGNATURE" and moved the 4000.42 one. These are those two bodies as v0.10.2
// wrote them, identifiers replaced.
func TestSingapayStandInRefusesWhatProductionRefused(t *testing.T) {
	const (
		endpoint  = "/api/v1.0/account-transfer/01K9SELLER/transfer"
		timestamp = "1793253604"
	)
	tests := []struct {
		name     string
		body     string
		accepted bool
	}{
		{
			name:     "a whole rupiah in two decimals is refused",
			body:     `{"amount":4000.00,"beneficiary_account_number":"123456789012","merchant_ref_no":"PF-INV-20261107222853-AAAAAA"}`,
			accepted: false,
		},
		{
			name:     "two sen digits are accepted",
			body:     `{"amount":4000.42,"beneficiary_account_number":"123456789012","merchant_ref_no":"PF-INV-20261108122148-BBBBBB"}`,
			accepted: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ours, err := requestSignature(http.MethodPost, endpoint, testToken, []byte(tc.body), timestamp, testSecret)
			if err != nil {
				t.Fatal(err)
			}
			theirs, err := singapaySignature(http.MethodPost, endpoint, []byte(tc.body), timestamp)
			if err != nil {
				t.Fatal(err)
			}
			if got := ours == theirs; got != tc.accepted {
				t.Errorf("signature accepted = %v, want %v", got, tc.accepted)
			}
		})
	}
}

// newTransferStandIn serves the account-transfer endpoint as Singapay does, signature check
// included. It recomputes X-Signature over its own re-encoding of the body and answers 401
// "Invalid X-SIGNATURE" when the two differ, the answer production gave.
func newTransferStandIn(t *testing.T, got **capture) *httptest.Server {
	t.Helper()
	return newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		*got = c
		want, err := singapaySignature(r.Method, c.RawPath, c.Body, c.Headers.Get("X-Timestamp"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, `{"status":400,"success":false,"message":"body is not JSON"}`)
			return
		}
		if c.Headers.Get("X-Signature") != want {
			writeJSON(w, http.StatusUnauthorized, `{"status":401,"success":false,"message":"Unauthorized. Invalid X-SIGNATURE"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"status":200,"success":true,"data":{"transaction_id":"T-1","merchant_ref_no":"PF-INV-1","status":"success","amount":"4000"}}`)
	})
}

func TestTransferBetweenAccounts_PassesSingapaysSignatureCheck(t *testing.T) {
	tests := []struct {
		name       string
		minor      int64
		wantAmount string
	}{
		{"a whole rupiah, refused before v0.10.3", 400000, `"amount":4000,`},
		{"a last sen digit of zero, refused before v0.10.3", 400010, `"amount":4000.1,`},
		{"two sen digits", 400042, `"amount":4000.42,`},
		{"below the quoted fee", 399958, `"amount":3999.58,`},
		{"the Rp1 minimum", 100, `"amount":1,`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got *capture
			srv := newTransferStandIn(t, &got)
			c := testClient(t, srv.URL)

			transfer, err := c.TransferBetweenAccounts(context.Background(), "01K9SELLER", TransferRequest{
				Amount:                   NewAmountFromMinor(tc.minor, "IDR"),
				BeneficiaryAccountNumber: "123456789012",
				MerchantRefNo:            "PF-INV-20261109072340-CCCCCC",
			})
			if err != nil {
				t.Fatalf("TransferBetweenAccounts: %v", err)
			}
			if transfer.Status != "success" {
				t.Errorf("status = %q, want success", transfer.Status)
			}
			if got.Path != "/api/v1.0/account-transfer/01K9SELLER/transfer" {
				t.Errorf("path = %s", got.Path)
			}
			if !strings.Contains(string(got.Body), tc.wantAmount) {
				t.Errorf("body %s, want it to carry %s", got.Body, tc.wantAmount)
			}
		})
	}
}
