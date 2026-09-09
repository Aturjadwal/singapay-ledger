package singapay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// jakarta is the timezone the access-token signature's date component is taken in.
var jakarta = time.FixedZone("WIB", 7*60*60)

// canonicalJSON normalises a JSON document the way Singapay's signature scheme expects:
// every object's keys sorted recursively and alphabetically, re-encoded compactly.
//
// Two details do the real work here. Numbers are decoded with UseNumber so they survive
// as written: routed through float64 instead, 10000.00 comes back out as 10000 and 0.070
// as 0.07, and any integer past 2^53 is corrupted outright. Either way the hash stops
// matching the one Singapay computed, and the only symptom is SP016. And HTML escaping is
// switched off, because Go escapes <, > and & by default while PHP's JSON_UNESCAPED_*
// flags do not.
//
// Key sorting comes free: encoding/json emits map keys in sorted order.
func canonicalJSON(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("singapay: body is not valid JSON: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// hashBody returns the SHA-256 hex digest of the canonical form of raw.
func hashBody(raw []byte) (string, error) {
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func hmacSHA512Hex(message, key string) string {
	mac := hmac.New(sha512.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// accessTokenSignature builds the X-Signature for POST /api/v1.1/access-token/b2b.
//
//	HMAC-SHA512("{client_id}_{client_secret}_{YYYYMMDD}", client_secret)
//
// The date is the current Asia/Jakarta calendar date and the signature is only valid for
// that day — a server running in UTC will fail for the first seven hours of every day if
// it takes the date locally.
func accessTokenSignature(clientID, clientSecret string, now time.Time) string {
	payload := fmt.Sprintf("%s_%s_%s", clientID, clientSecret, now.In(jakarta).Format("20060102"))
	return hmacSHA512Hex(payload, clientSecret)
}

// requestSignature builds the X-Signature for a signed money-out request.
//
//	HMAC-SHA512("{METHOD}:{ENDPOINT}:{ACCESS_TOKEN}:{SHA256(canonical body)}:{TIMESTAMP}",
//	            client_secret)
//
// endpoint is the request path including any query string and excluding the host.
// accessToken carries no "Bearer " prefix. timestamp must be the same string sent in the
// X-Timestamp header — see [TimestampFunc] for what that string should be.
func requestSignature(method, endpoint, accessToken string, body []byte, timestamp, clientSecret string) (string, error) {
	hashed, err := hashBody(body)
	if err != nil {
		return "", err
	}
	stringToSign := strings.Join([]string{
		strings.ToUpper(method), endpoint, accessToken, hashed, timestamp,
	}, ":")
	return hmacSHA512Hex(stringToSign, clientSecret), nil
}

// TimestampFunc renders the value sent as X-Timestamp and folded into the request
// signature.
//
// Singapay's documentation contradicts itself here. "Building the X-Signature Header"
// says Unix seconds; the OpenAPI parameter description for POST /api/v2.0/disbursement/
// transfer says ISO-8601. Both cannot be right, and a wrong guess surfaces only as SP016
// with no hint as to why.
//
// The default follows the signing guide, which is the document actually about signing.
// [UnixSecondsTimestamp] and [ISO8601Timestamp] are both provided so switching is one
// line in the config once sandbox settles it.
type TimestampFunc func(time.Time) string

// UnixSecondsTimestamp renders Unix seconds, e.g. "1695711945". The default.
func UnixSecondsTimestamp(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// ISO8601Timestamp renders RFC 3339 in Asia/Jakarta, e.g. "2026-09-08T13:00:00+07:00".
func ISO8601Timestamp(t time.Time) string { return t.In(jakarta).Format(time.RFC3339) }

// WebhookRequest is the part of an inbound webhook needed to verify it.
type WebhookRequest struct {
	// Endpoint is the path of your webhook URL, including query string. It must match
	// what is registered in the merchant dashboard exactly — Singapay signed that
	// string, not whatever your reverse proxy rewrote it to.
	Endpoint string
	// Body is the raw request body, byte for byte as received.
	Body []byte
	// Signature is the X-Signature header.
	Signature string
	// Timestamp is the X-Timestamp header.
	Timestamp string
	// Authorization is the Authorization header, with or without "Bearer ". For
	// system-triggered webhooks (VA payment, settlement) this is a random string
	// Singapay generated, not a token we issued — it is signed material either way.
	Authorization string
}

// WebhookRequestFromHTTP reads the verification inputs from an inbound request. body must
// be the raw bytes already read from r.Body.
func WebhookRequestFromHTTP(r *http.Request, endpoint string, body []byte) WebhookRequest {
	return WebhookRequest{
		Endpoint:      endpoint,
		Body:          body,
		Signature:     r.Header.Get("X-Signature"),
		Timestamp:     r.Header.Get("X-Timestamp"),
		Authorization: r.Header.Get("Authorization"),
	}
}

// VerifyWebhook checks an inbound webhook's HMAC-SHA512 signature.
//
// The scheme is the same as [requestSignature] with METHOD fixed to POST — the difference
// is only which side computes it. Comparison is constant time.
//
// The key is [Config.WebhookKey], which defaults to ClientSecret. Inbound deliveries may
// be signed with a different secret than the one this client signs its own requests with;
// see that field for why the question is open.
//
// Verifying does not make the payload trustworthy on its own: pair this with the IP
// allowlist, and treat a replayed-but-valid delivery as a duplicate rather than a second
// event. Singapay retries, so duplicates are expected traffic.
func (c *Client) VerifyWebhook(req WebhookRequest) error {
	if req.Signature == "" {
		return &Error{StatusCode: http.StatusUnauthorized, Message: "missing X-Signature"}
	}

	token := strings.TrimSpace(strings.TrimPrefix(req.Authorization, "Bearer "))
	want, err := requestSignature(http.MethodPost, req.Endpoint, token, req.Body, req.Timestamp, c.cfg.WebhookKey)
	if err != nil {
		return &Error{StatusCode: http.StatusBadRequest, Message: "webhook body is not valid JSON", Err: err}
	}

	if !hmac.Equal([]byte(want), []byte(req.Signature)) {
		return &Error{StatusCode: http.StatusUnauthorized, Code: CodeSignatureInvalid, Message: "webhook signature mismatch"}
	}
	return nil
}

// WebhookFresh reports whether a verified webhook's timestamp is within tolerance of now,
// which is what stops a captured delivery being replayed later. Singapay's own guidance
// is five minutes.
//
// It is separate from [Client.VerifyWebhook] because a stale-but-authentic delivery is a
// different decision from a forged one — during an outage you may well want to accept it.
func WebhookFresh(timestamp string, now time.Time, tolerance time.Duration) bool {
	secs, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(timestamp))
		if err != nil {
			return false
		}
		secs = t.Unix()
	}
	delta := now.Unix() - secs
	if delta < 0 {
		delta = -delta
	}
	return time.Duration(delta)*time.Second <= tolerance
}
