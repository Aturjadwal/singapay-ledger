package singapay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SandboxBaseURL    = "https://sandbox-payment-b2b.singapay.id"
	ProductionBaseURL = "https://payment-b2b.singapay.id"

	// tokenRefreshMargin is how long before expiry a cached token is discarded. The
	// token lives about 60 hours, so this is generous by design.
	tokenRefreshMargin = 5 * time.Minute

	defaultTimeout = 30 * time.Second
)

// Config holds the credentials and knobs for a [Client].
type Config struct {
	// ClientID and ClientSecret are issued during onboarding. ClientSecret is the HMAC
	// key for every signature scheme — it is never sent on the wire.
	ClientID     string
	ClientSecret string
	// PartnerID is the merchant API key, sent as X-PARTNER-ID on every call.
	PartnerID string

	// IsProduction selects the production host. Ignored when BaseURL is set.
	IsProduction bool
	// BaseURL overrides the host entirely. Useful for tests.
	BaseURL string

	// HTTPClient defaults to one with a 30s timeout.
	HTTPClient *http.Client

	// Timestamp renders X-Timestamp. Defaults to [UnixSecondsTimestamp]; see
	// [TimestampFunc] for why this is configurable at all.
	Timestamp TimestampFunc

	// Now defaults to time.Now. Injectable so signatures are testable.
	Now func() time.Time
}

// Client talks to the Singapay merchant API.
//
// It is safe for concurrent use; the cached access token is guarded internally.
type Client struct {
	cfg     Config
	http    *http.Client
	baseURL string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// New builds a Client. It fails only on missing credentials — no network call is made.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.ClientID == "":
		return nil, fmt.Errorf("singapay: ClientID is required")
	case cfg.ClientSecret == "":
		return nil, fmt.Errorf("singapay: ClientSecret is required")
	case cfg.PartnerID == "":
		return nil, fmt.Errorf("singapay: PartnerID is required")
	}

	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	if cfg.Timestamp == nil {
		cfg.Timestamp = UnixSecondsTimestamp
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = SandboxBaseURL
		if cfg.IsProduction {
			baseURL = ProductionBaseURL
		}
	}

	return &Client{cfg: cfg, http: cfg.HTTPClient, baseURL: strings.TrimSuffix(baseURL, "/")}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Access token
// ─────────────────────────────────────────────────────────────────────────────

type accessTokenData struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	// ExpiresIn is seconds, delivered as a string ("216000").
	ExpiresIn string `json:"expires_in"`
}

// AccessToken returns a valid bearer token, fetching one only when the cache is empty or
// close to expiry.
//
// Caching matters more here than it looks: the DOKU path asks for a fresh token on every
// bank-account validation, and a Singapay token is good for roughly sixty hours.
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && c.cfg.Now().Before(c.tokenExpiry.Add(-tokenRefreshMargin)) {
		return c.token, nil
	}

	now := c.cfg.Now()
	body := []byte(`{"grant_type":"client_credentials"}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1.1/access-token/b2b", bytes.NewReader(body))
	if err != nil {
		return "", &Error{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-PARTNER-ID", c.cfg.PartnerID)
	req.Header.Set("X-CLIENT-ID", c.cfg.ClientID)
	req.Header.Set("X-Signature", accessTokenSignature(c.cfg.ClientID, c.cfg.ClientSecret, now))

	var data accessTokenData
	if err := c.send(req, &data, nil); err != nil {
		return "", err
	}
	if data.AccessToken == "" {
		return "", &Error{StatusCode: http.StatusOK, Message: "access token missing from response"}
	}

	ttl := time.Duration(0)
	if secs, err := strconv.ParseInt(strings.TrimSpace(data.ExpiresIn), 10, 64); err == nil && secs > 0 {
		ttl = time.Duration(secs) * time.Second
	}
	if ttl <= tokenRefreshMargin {
		// An absent or nonsensical expires_in must not produce a token that is
		// discarded immediately, nor one cached forever.
		ttl = time.Hour
	}

	c.token = data.AccessToken
	c.tokenExpiry = now.Add(ttl)
	return c.token, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Request plumbing
// ─────────────────────────────────────────────────────────────────────────────

// call issues an authenticated request. path must start with "/api" and carry any query
// string, because that exact string is what a signature is computed over.
//
// signed adds X-Timestamp and X-Signature. Only money-out endpoints need it; sending it
// where it is not expected is harmless, but computing it wrong where it is expected
// surfaces as SP016 with no further detail.
func (c *Client) call(ctx context.Context, method, path string, in, out any, signed bool) error {
	return c.do(ctx, method, path, in, out, nil, signed)
}

// callPaged is call for list endpoints, additionally reading the page metadata.
func (c *Client) callPaged(ctx context.Context, method, path string, in, out any, page *Pagination) error {
	return c.do(ctx, method, path, in, out, page, false)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, page *Pagination, signed bool) error {
	var body []byte
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return &Error{Err: err}
		}
	}

	token, err := c.AccessToken(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return &Error{Err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-PARTNER-ID", c.cfg.PartnerID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if signed {
		timestamp := c.cfg.Timestamp(c.cfg.Now())
		sig, err := requestSignature(method, path, token, body, timestamp, c.cfg.ClientSecret)
		if err != nil {
			return &Error{Err: err}
		}
		req.Header.Set("X-Timestamp", timestamp)
		req.Header.Set("X-Signature", sig)
	}

	return c.send(req, out, page)
}

// envelope covers both response shapes. v1.0 endpoints fill status/success/data; v2.0
// endpoints fill response_code/response_message/data.
type envelope struct {
	Status          int             `json:"status"`
	Success         *bool           `json:"success"`
	ResponseCode    ResponseCode    `json:"response_code"`
	ResponseMessage string          `json:"response_message"`
	Data            json.RawMessage `json:"data"`
	Pagination      json.RawMessage `json:"pagination"`
	Meta            json.RawMessage `json:"meta"`
	Message         string          `json:"message"`
	Error           *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Errors map[string]any `json:"errors"`
}

func (c *Client) send(req *http.Request, out any, page *Pagination) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// No answer. Whether the server acted on this is unknowable from here, which
		// is exactly what Outcome() reports.
		return &Error{Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{StatusCode: resp.StatusCode, Err: err}
	}

	var env envelope
	// A body that will not parse is still a failure worth reporting with its status;
	// only a 2xx we cannot read is a genuine surprise.
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return &Error{
				StatusCode: resp.StatusCode,
				Message:    "response is not valid JSON",
				Body:       raw,
				Err:        err,
			}
		}
	}

	if apiErr := classify(resp.StatusCode, &env, raw); apiErr != nil {
		return apiErr
	}

	if page != nil && len(env.Pagination) > 0 {
		// A page we cannot read is not worth failing an otherwise good response over
		// — the rows are already in hand.
		_ = json.Unmarshal(env.Pagination, page)
	}

	if out == nil {
		return nil
	}
	payload := env.Data
	if len(payload) == 0 {
		// Some endpoints answer 200 with no data object at all.
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return &Error{StatusCode: resp.StatusCode, Message: "cannot decode data", Body: raw, Err: err}
	}
	return nil
}

// classify decides whether a response is a failure, and with which SP-code.
//
// The two envelopes disagree about where success lives, and they disagree in a way that
// matters: a v2 endpoint can answer HTTP 200 carrying SP001, which is a failed
// transaction, while a v1 endpoint signals failure only through the HTTP status.
func classify(status int, env *envelope, raw []byte) *Error {
	failed := status < 200 || status >= 300
	if env.ResponseCode != "" && env.ResponseCode != CodeSuccess {
		failed = true
	}
	if env.Success != nil && !*env.Success {
		failed = true
	}
	if !failed {
		return nil
	}

	message := env.ResponseMessage
	if message == "" {
		message = env.Message
	}
	if message == "" && env.Error != nil {
		message = env.Error.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}

	// SP018 puts per-field detail in data.errors.
	fields := env.Errors
	if len(env.Data) > 0 {
		var detail struct {
			Errors map[string]any `json:"errors"`
		}
		if err := json.Unmarshal(env.Data, &detail); err == nil && len(detail.Errors) > 0 {
			fields = detail.Errors
		}
	}

	return &Error{
		StatusCode: status,
		Code:       env.ResponseCode,
		Message:    message,
		Fields:     fields,
		Body:       raw,
	}
}

// Pagination is the page metadata on list endpoints.
//
// Singapay spells these two ways: fractal's snake_case on most collections, and camelCase
// on statements, where the app remaps them. Both are accepted.
type Pagination struct {
	Count       int
	Total       int
	PerPage     int
	CurrentPage int
	TotalPages  int
}

func (p *Pagination) UnmarshalJSON(b []byte) error {
	var raw struct {
		Count int `json:"count"`
		Total int `json:"total"`

		PerPage     int `json:"per_page"`
		CurrentPage int `json:"current_page"`
		TotalPages  int `json:"total_pages"`

		PerPageCamel     int `json:"perPage"`
		CurrentPageCamel int `json:"currentPage"`
		TotalPagesCamel  int `json:"totalPages"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = Pagination{
		Count:       raw.Count,
		Total:       raw.Total,
		PerPage:     cmpOr(raw.PerPage, raw.PerPageCamel),
		CurrentPage: cmpOr(raw.CurrentPage, raw.CurrentPageCamel),
		TotalPages:  cmpOr(raw.TotalPages, raw.TotalPagesCamel),
	}
	return nil
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
