package singapay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testClientID = "client-abc"
	testSecret   = "secret-xyz"
	testPartner  = "partner-123"
	testToken    = "the-access-token"
)

var testNow = time.Date(2026, 9, 8, 5, 0, 0, 0, time.UTC)

// testClient builds a client pinned to a fixed clock. Pass "" for tests that never
// perform a request.
func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Config{
		ClientID:     testClientID,
		ClientSecret: testSecret,
		PartnerID:    testPartner,
		BaseURL:      baseURL,
		Now:          func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// capture records what the fake server received, so a test can assert on the wire form.
type capture struct {
	Method  string
	Path    string
	RawPath string
	Headers http.Header
	Body    []byte
}

// newServer serves the access-token endpoint and hands everything else to handler.
// tokenHits counts how often a token was minted, which is how token caching is observed.
func newServer(t *testing.T, tokenHits *int32, handler func(w http.ResponseWriter, r *http.Request, got *capture)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.URL.Path == "/api/v1.1/access-token/b2b" {
			if tokenHits != nil {
				atomic.AddInt32(tokenHits, 1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":200,"success":true,"data":{"access_token":"` + testToken + `","token_type":"Bearer","expires_in":"216000"}}`))
			return
		}

		got := &capture{
			Method:  r.Method,
			Path:    r.URL.Path,
			RawPath: r.URL.RequestURI(),
			Headers: r.Header.Clone(),
			Body:    body,
		}
		handler(w, r, got)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestNewRequiresCredentials(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no client id", Config{ClientSecret: "s", PartnerID: "p"}},
		{"no client secret", Config{ClientID: "c", PartnerID: "p"}},
		{"no partner id", Config{ClientID: "c", ClientSecret: "s"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestNewSelectsHost(t *testing.T) {
	sandbox, _ := New(Config{ClientID: "c", ClientSecret: "s", PartnerID: "p"})
	if sandbox.baseURL != SandboxBaseURL {
		t.Errorf("default host is %s, want sandbox", sandbox.baseURL)
	}
	prod, _ := New(Config{ClientID: "c", ClientSecret: "s", PartnerID: "p", IsProduction: true})
	if prod.baseURL != ProductionBaseURL {
		t.Errorf("got %s, want production", prod.baseURL)
	}
}

func TestAccessTokenIsCached(t *testing.T) {
	var hits int32
	srv := newServer(t, &hits, func(w http.ResponseWriter, r *http.Request, got *capture) {
		writeJSON(w, 200, `{"status":200,"success":true,"data":{"held_balance":{"value":"0.00","currency":"IDR"}}}`)
	})
	c := testClient(t, srv.URL)
	ctx := context.Background()

	for range 3 {
		if _, err := c.GetMerchantBalance(ctx); err != nil {
			t.Fatalf("GetMerchantBalance: %v", err)
		}
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("minted %d tokens across three calls, want 1", got)
	}
}

func TestAccessTokenSendsSignatureHeaders(t *testing.T) {
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		writeJSON(w, 200, `{"status":200,"success":true,"data":{"access_token":"t","expires_in":"216000"}}`)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	if _, err := c.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	if got := captured.Get("X-PARTNER-ID"); got != testPartner {
		t.Errorf("X-PARTNER-ID = %q", got)
	}
	if got := captured.Get("X-CLIENT-ID"); got != testClientID {
		t.Errorf("X-CLIENT-ID = %q", got)
	}
	want := accessTokenSignature(testClientID, testSecret, testNow)
	if got := captured.Get("X-Signature"); got != want {
		t.Errorf("X-Signature = %q, want %q", got, want)
	}
	if captured.Get("Authorization") != "" {
		t.Error("the token request must not send an Authorization header")
	}
}

func TestSignedRequestHeaders(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"response_code":"SP000","response_message":"Successfully","data":{"transaction_id":"1","transaction_status":{"code":"00","desc":"Success"}}}`)
	})
	c := testClient(t, srv.URL)

	_, err := c.Disburse(context.Background(), DisburseRequest{
		AccountID:         "01K9",
		ReferenceNumber:   "REF-1",
		BankCode:          "BRINIDJA",
		BankAccountNumber: "1234567890",
		Amount:            50000,
	})
	if err != nil {
		t.Fatalf("Disburse: %v", err)
	}

	if got.Headers.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got.Headers.Get("Authorization"))
	}
	if got.Headers.Get("X-PARTNER-ID") != testPartner {
		t.Errorf("X-PARTNER-ID = %q", got.Headers.Get("X-PARTNER-ID"))
	}

	wantTimestamp := UnixSecondsTimestamp(testNow)
	if got.Headers.Get("X-Timestamp") != wantTimestamp {
		t.Errorf("X-Timestamp = %q, want %q", got.Headers.Get("X-Timestamp"), wantTimestamp)
	}

	// The signature must be computed over the body actually sent and the path
	// actually requested — that is the pairing SP016 exists to punish.
	wantSig, err := requestSignature(http.MethodPost, got.RawPath, testToken, got.Body, wantTimestamp, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got.Headers.Get("X-Signature") != wantSig {
		t.Errorf("X-Signature does not match the transmitted body and path")
	}
}

func TestUnsignedRequestOmitsSignature(t *testing.T) {
	var got *capture
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		got = c
		writeJSON(w, 200, `{"status":200,"success":true,"data":{"id":"01K9","name":"Budi","status":"active"}}`)
	})
	c := testClient(t, srv.URL)

	if _, err := c.GetAccount(context.Background(), "01K9"); err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got.Headers.Get("X-Signature") != "" {
		t.Error("an unsigned endpoint must not send X-Signature")
	}
}

func TestClassifyV1Failure(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 422, `{"status":422,"success":false,"error":{"code":422,"message":"The name field is required."}}`)
	})
	c := testClient(t, srv.URL)

	_, err := c.CreateAccount(context.Background(), CreateAccountRequest{Type: AccountTypeOwned})
	if err == nil {
		t.Fatal("want an error")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("want a *Error, got %T", err)
	}
	if e.StatusCode != 422 {
		t.Errorf("StatusCode = %d", e.StatusCode)
	}
	if !strings.Contains(e.Message, "name field is required") {
		t.Errorf("Message = %q", e.Message)
	}
}

func TestClassifyV2FailureOnHTTP200(t *testing.T) {
	// A v2 endpoint can answer 200 with a failing SP-code. Trusting the HTTP status
	// alone would read this as a successful payout.
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, `{"response_code":"SP003","response_message":"Insufficient Balance","data":{}}`)
	})
	c := testClient(t, srv.URL)

	_, err := c.Disburse(context.Background(), DisburseRequest{AccountID: "01K9", ReferenceNumber: "R"})
	if err == nil {
		t.Fatal("want an error despite HTTP 200")
	}
	e, _ := AsError(err)
	if e.Code != CodeInsufficientFunds {
		t.Errorf("Code = %q, want SP003", e.Code)
	}
	if e.Outcome() != OutcomeRefused {
		t.Errorf("Outcome = %v, want refused", e.Outcome())
	}
}

func TestClassifyExtractsFieldErrors(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 400, `{"response_code":"SP018","response_message":"Validation Error","data":{"errors":{"amount":["must be at least 10000"]}}}`)
	})
	c := testClient(t, srv.URL)

	_, err := c.Disburse(context.Background(), DisburseRequest{AccountID: "01K9", ReferenceNumber: "R"})
	e, _ := AsError(err)
	if e == nil {
		t.Fatal("want a *Error")
	}
	if _, ok := e.Fields["amount"]; !ok {
		t.Errorf("Fields = %v, want the amount detail", e.Fields)
	}
}

func TestTransportFailureIsUnknownOutcome(t *testing.T) {
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {})
	c := testClient(t, srv.URL)
	// Cancelling mid-flight is the shape of a call that may still have been acted on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Disburse(ctx, DisburseRequest{AccountID: "01K9", ReferenceNumber: "R"})
	if err == nil {
		t.Fatal("want an error")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("want a *Error, got %T", err)
	}
	if e.Outcome() != OutcomeUnknown {
		t.Errorf("Outcome = %v, want unknown — a call with no answer may still have moved money", e.Outcome())
	}
}

func TestPaginationAcceptsBothSpellings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Pagination
	}{
		{
			name: "fractal snake_case",
			in:   `{"count":6,"total":42,"per_page":25,"current_page":2,"total_pages":2}`,
			want: Pagination{Count: 6, Total: 42, PerPage: 25, CurrentPage: 2, TotalPages: 2},
		},
		{
			name: "statements camelCase",
			in:   `{"count":6,"total":42,"perPage":25,"currentPage":2,"totalPages":2}`,
			want: Pagination{Count: 6, Total: 42, PerPage: 25, CurrentPage: 2, TotalPages: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Pagination
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
