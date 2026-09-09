package singapay

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Environment variable names read by [ConfigFromEnv].
const (
	EnvClientID     = "SINGAPAY_CLIENT_ID"
	EnvClientSecret = "SINGAPAY_CLIENT_SECRET"
	EnvPartnerID    = "SINGAPAY_PARTNER_ID"
	EnvWebhookKey   = "SINGAPAY_WEBHOOK_KEY"
	EnvProduction   = "SINGAPAY_PRODUCTION"
	EnvBaseURL      = "SINGAPAY_BASE_URL"
	EnvTimestamp    = "SINGAPAY_TIMESTAMP_FORMAT"
)

// ConfigFromEnv builds a [Config] from the environment.
//
//	SINGAPAY_CLIENT_ID          required
//	SINGAPAY_CLIENT_SECRET      required — HMAC key for every signature, and the key
//	                            that verifies inbound webhooks
//	SINGAPAY_PARTNER_ID         required — merchant API key, sent as X-PARTNER-ID. The
//	                            dashboard labels it as a merchant or API key rather than
//	                            a "partner id"
//	SINGAPAY_WEBHOOK_KEY        optional — HMAC key for verifying INBOUND webhooks.
//	                            Unset means SINGAPAY_CLIENT_SECRET. Set it only if the
//	                            dashboard issues a distinct HMAC validation key AND that
//	                            key is what signs callbacks; see [Config.WebhookKey]
//	SINGAPAY_PRODUCTION         optional — "true" targets production; anything else,
//	                            including unset, stays on sandbox
//	SINGAPAY_BASE_URL           optional — overrides the host entirely
//	SINGAPAY_TIMESTAMP_FORMAT   optional — "unix" (default) or "iso"
//
// Sandbox is the default on purpose: an unset or misspelled variable must not move real
// money.
//
// SINGAPAY_TIMESTAMP_FORMAT exists because Singapay's own documentation contradicts
// itself about X-Timestamp — see [TimestampFunc]. Until sandbox settles it, this is the
// switch, and it is an environment variable rather than a code constant so flipping it
// does not need a release.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		ClientID:     strings.TrimSpace(os.Getenv(EnvClientID)),
		ClientSecret: strings.TrimSpace(os.Getenv(EnvClientSecret)),
		PartnerID:    strings.TrimSpace(os.Getenv(EnvPartnerID)),
		WebhookKey:   strings.TrimSpace(os.Getenv(EnvWebhookKey)),
		BaseURL:      strings.TrimSpace(os.Getenv(EnvBaseURL)),
	}

	var missing []string
	for name, value := range map[string]string{
		EnvClientID:     cfg.ClientID,
		EnvClientSecret: cfg.ClientSecret,
		EnvPartnerID:    cfg.PartnerID,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// Sorted so the message is stable across runs; map iteration is not.
		slices.Sort(missing)
		return Config{}, fmt.Errorf("singapay: missing required environment: %s", strings.Join(missing, ", "))
	}

	if raw := strings.TrimSpace(os.Getenv(EnvProduction)); raw != "" {
		production, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("singapay: %s must be a boolean, got %q", EnvProduction, raw)
		}
		cfg.IsProduction = production
	}

	switch format := strings.ToLower(strings.TrimSpace(os.Getenv(EnvTimestamp))); format {
	case "", "unix":
		cfg.Timestamp = UnixSecondsTimestamp
	case "iso", "iso8601":
		cfg.Timestamp = ISO8601Timestamp
	default:
		return Config{}, fmt.Errorf("singapay: %s must be \"unix\" or \"iso\", got %q", EnvTimestamp, format)
	}

	return cfg, nil
}

// NewFromEnv builds a [Client] from [ConfigFromEnv].
func NewFromEnv() (*Client, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return New(cfg)
}
