package singapay

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// Straight from Singapay's own worked example.
			name: "keys are sorted recursively",
			in:   `{"status":200,"success":true,"data":{"transaction":{"reff_no":"123"}}}`,
			want: `{"data":{"transaction":{"reff_no":"123"}},"status":200,"success":true}`,
		},
		{
			name: "13-digit timestamps survive verbatim",
			in:   `{"post_timestamp":1766978961000}`,
			want: `{"post_timestamp":1766978961000}`,
		},
		{
			// Past 2^53 a float64 round-trip corrupts the value outright:
			// 9007199254740993 comes back as ...992.
			name: "integers beyond float64 precision survive",
			in:   `{"id":9007199254740993}`,
			want: `{"id":9007199254740993}`,
		},
		{
			// The likelier failure, and the quieter one: a float64 round-trip
			// rewrites 10000.00 as 10000 and 0.070 as 0.07.
			name: "decimals keep the form they were sent in",
			in:   `{"a":10000.00,"b":0.070}`,
			want: `{"a":10000.00,"b":0.070}`,
		},
		{
			// Go escapes these by default; PHP's JSON_UNESCAPED_* flags do not.
			name: "HTML characters are not escaped",
			in:   `{"note":"a<b>c&d"}`,
			want: `{"note":"a<b>c&d"}`,
		},
		{
			name: "slashes are not escaped",
			in:   `{"url":"https://example.com/a/b"}`,
			want: `{"url":"https://example.com/a/b"}`,
		},
		{
			name: "unicode is not escaped",
			in:   `{"name":"Budi Söhne"}`,
			want: `{"name":"Budi Söhne"}`,
		},
		{
			name: "array order is preserved",
			in:   `{"items":[{"b":2,"a":1},{"d":4,"c":3}]}`,
			want: `{"items":[{"a":1,"b":2},{"c":3,"d":4}]}`,
		},
		{
			name: "whitespace is removed",
			in:   "{\n  \"a\" : 1\n}",
			want: `{"a":1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("canonicalJSON: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestCanonicalJSONEmptyBody(t *testing.T) {
	got, err := canonicalJSON(nil)
	if err != nil {
		t.Fatalf("canonicalJSON(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got %q, want nil", got)
	}
}

func TestCanonicalJSONRejectsGarbage(t *testing.T) {
	if _, err := canonicalJSON([]byte("not json")); err == nil {
		t.Error("want an error for a non-JSON body")
	}
}

func TestAccessTokenSignature(t *testing.T) {
	const (
		clientID = "client-abc"
		secret   = "secret-xyz"
	)
	// 18:00 UTC is already the next calendar day in Jakarta. Taking the date locally
	// would sign with 20260908 and be rejected for the first seven hours of every day.
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)

	got := accessTokenSignature(clientID, secret, now)

	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte("client-abc_secret-xyz_20260909"))
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Errorf("signature does not use the Jakarta date\n got %s\nwant %s", got, want)
	}
	if len(got) != 128 {
		t.Errorf("HMAC-SHA512 hex is 128 chars, got %d", len(got))
	}
	if strings.ToLower(got) != got {
		t.Error("digest must be lowercase hex")
	}
}

func TestRequestSignature(t *testing.T) {
	const (
		token  = "the-access-token"
		secret = "secret-xyz"
	)
	body := []byte(`{"amount":50000,"account_id":"01K9"}`)

	got, err := requestSignature("post", "/api/v2.0/disbursement/transfer", token, body, "1695711945", secret)
	if err != nil {
		t.Fatalf("requestSignature: %v", err)
	}

	// Rebuild the documented string independently: sorted body, SHA-256 hex, then
	// METHOD:ENDPOINT:TOKEN:HASH:TIMESTAMP under HMAC-SHA512.
	sum := sha256.Sum256([]byte(`{"account_id":"01K9","amount":50000}`))
	stringToSign := "POST:/api/v2.0/disbursement/transfer:" + token + ":" + hex.EncodeToString(sum[:]) + ":1695711945"
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRequestSignatureUppercasesMethod(t *testing.T) {
	body := []byte(`{"a":1}`)
	lower, err := requestSignature("post", "/api/x", "tok", body, "1", "s")
	if err != nil {
		t.Fatal(err)
	}
	upper, err := requestSignature("POST", "/api/x", "tok", body, "1", "s")
	if err != nil {
		t.Fatal(err)
	}
	if lower != upper {
		t.Error("method casing must not change the signature")
	}
}

func TestRequestSignatureIsSensitiveToEveryComponent(t *testing.T) {
	base := func() (string, error) {
		return requestSignature("POST", "/api/x?a=1", "tok", []byte(`{"a":1}`), "100", "secret")
	}
	original, err := base()
	if err != nil {
		t.Fatal(err)
	}

	variants := map[string]func() (string, error){
		"different path": func() (string, error) {
			return requestSignature("POST", "/api/x", "tok", []byte(`{"a":1}`), "100", "secret")
		},
		"different token": func() (string, error) {
			return requestSignature("POST", "/api/x?a=1", "other", []byte(`{"a":1}`), "100", "secret")
		},
		"different body": func() (string, error) {
			return requestSignature("POST", "/api/x?a=1", "tok", []byte(`{"a":2}`), "100", "secret")
		},
		"different timestamp": func() (string, error) {
			return requestSignature("POST", "/api/x?a=1", "tok", []byte(`{"a":1}`), "101", "secret")
		},
		"different secret": func() (string, error) {
			return requestSignature("POST", "/api/x?a=1", "tok", []byte(`{"a":1}`), "100", "other")
		},
	}
	for name, fn := range variants {
		got, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == original {
			t.Errorf("%s produced the same signature", name)
		}
	}
}

func TestVerifyWebhook(t *testing.T) {
	c := testClient(t, "")
	const endpoint = "/webhook/callback"
	body := []byte(`{"status":200,"success":true,"event":"va-transaction","data":{"transaction":{"reff_no":"INV-1"}}}`)

	sig, err := requestSignature("POST", endpoint, "random-token", body, "1695711945", c.cfg.ClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	req := WebhookRequest{
		Endpoint:      endpoint,
		Body:          body,
		Signature:     sig,
		Timestamp:     "1695711945",
		Authorization: "Bearer random-token",
	}

	if err := c.VerifyWebhook(req); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}

	t.Run("tampered body is rejected", func(t *testing.T) {
		bad := req
		bad.Body = []byte(`{"status":200,"success":true,"event":"va-transaction","data":{"transaction":{"reff_no":"INV-2"}}}`)
		if err := c.VerifyWebhook(bad); err == nil {
			t.Error("want rejection")
		}
	})

	t.Run("reordered keys still verify", func(t *testing.T) {
		// The signature is over the canonical form, so a proxy that re-serialises
		// the JSON must not break verification.
		reordered := req
		reordered.Body = []byte(`{"data":{"transaction":{"reff_no":"INV-1"}},"event":"va-transaction","success":true,"status":200}`)
		if err := c.VerifyWebhook(reordered); err != nil {
			t.Errorf("reordering must not matter: %v", err)
		}
	})

	t.Run("wrong endpoint is rejected", func(t *testing.T) {
		bad := req
		bad.Endpoint = "/webhook/other"
		if err := c.VerifyWebhook(bad); err == nil {
			t.Error("want rejection")
		}
	})

	t.Run("missing signature is rejected", func(t *testing.T) {
		bad := req
		bad.Signature = ""
		if err := c.VerifyWebhook(bad); err == nil {
			t.Error("want rejection")
		}
	})

	t.Run("bare token without Bearer prefix verifies", func(t *testing.T) {
		bare := req
		bare.Authorization = "random-token"
		if err := c.VerifyWebhook(bare); err != nil {
			t.Errorf("want acceptance: %v", err)
		}
	})
}

func TestWebhookFresh(t *testing.T) {
	now := time.Unix(1695711945, 0)
	tests := []struct {
		name      string
		timestamp string
		want      bool
	}{
		{"same second", "1695711945", true},
		{"four minutes old", "1695711705", true},
		{"six minutes old", "1695711585", false},
		{"six minutes in the future", "1695712305", false},
		{"ISO-8601 within tolerance", "2023-09-26T14:05:45+07:00", true},
		{"ISO-8601 outside tolerance", "2026-09-08T13:00:00+07:00", false},
		{"garbage", "not-a-time", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebhookFresh(tc.timestamp, now, 5*time.Minute); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The webhook key is separate from the client secret because it may genuinely be a
// separate secret — the dashboard issues an HMAC validation key, and whether that or the
// client secret signs an inbound callback is not settled. These two tests pin both
// readings so that whichever turns out to be right is a configuration change and not a
// code change.
func TestVerifyWebhook_WebhookKeyDefaultsToClientSecret(t *testing.T) {
	c := testClient(t, "")

	if c.cfg.WebhookKey != testSecret {
		t.Fatalf("unset WebhookKey should fall back to ClientSecret, got %q", c.cfg.WebhookKey)
	}

	// A delivery signed with the client secret still verifies, which is the behaviour
	// every deployment has today.
	const endpoint = "/singapay/notification"
	body := []byte(`{"event":"va-transaction","data":{"transaction":{"reff_no":"INV-1"}}}`)
	sig, err := requestSignature("POST", endpoint, "tok", body, "1695711945", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.VerifyWebhook(WebhookRequest{
		Endpoint: endpoint, Body: body, Signature: sig,
		Timestamp: "1695711945", Authorization: "Bearer tok",
	}); err != nil {
		t.Fatalf("delivery signed with the client secret must verify by default: %v", err)
	}
}

func TestVerifyWebhook_UsesTheWebhookKeyWhenSet(t *testing.T) {
	const webhookKey = "an-entirely-different-hmac-validation-key"
	c, err := New(Config{
		ClientID:     testClientID,
		ClientSecret: testSecret,
		PartnerID:    testPartner,
		WebhookKey:   webhookKey,
		Now:          func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const endpoint = "/singapay/notification"
	body := []byte(`{"event":"va-transaction","data":{"transaction":{"reff_no":"INV-1"}}}`)

	signedWithWebhookKey, err := requestSignature("POST", endpoint, "tok", body, "1695711945", webhookKey)
	if err != nil {
		t.Fatal(err)
	}
	req := WebhookRequest{
		Endpoint: endpoint, Body: body, Signature: signedWithWebhookKey,
		Timestamp: "1695711945", Authorization: "Bearer tok",
	}
	if err := c.VerifyWebhook(req); err != nil {
		t.Fatalf("delivery signed with the webhook key must verify: %v", err)
	}

	// And the client secret must now be refused. Accepting both would defeat the point:
	// the whole reason to separate the keys is that only one of them is the real one.
	signedWithClientSecret, err := requestSignature("POST", endpoint, "tok", body, "1695711945", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	req.Signature = signedWithClientSecret
	if err := c.VerifyWebhook(req); err == nil {
		t.Error("with a distinct webhook key set, a delivery signed with the client secret must be refused")
	}
}

// Outbound signing must keep using the client secret. A webhook key that leaked into the
// request signature would fail every API call, and the failure would point at the wrong
// credential entirely.
func TestVerifyWebhook_WebhookKeyDoesNotAffectOutboundSigning(t *testing.T) {
	c, err := New(Config{
		ClientID:     testClientID,
		ClientSecret: testSecret,
		PartnerID:    testPartner,
		WebhookKey:   "a-different-key",
		Now:          func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want, err := requestSignature("POST", "/api/v2.0/disbursement/transfer", "tok", []byte(`{"a":1}`), "1695711945", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := requestSignature("POST", "/api/v2.0/disbursement/transfer", "tok", []byte(`{"a":1}`), "1695711945", c.cfg.ClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("outbound request signing must use the client secret, not the webhook key")
	}
}
