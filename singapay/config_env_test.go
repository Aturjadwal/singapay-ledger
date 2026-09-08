package singapay

import (
	"strings"
	"testing"
	"time"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, name := range []string{EnvClientID, EnvClientSecret, EnvPartnerID, EnvProduction, EnvBaseURL, EnvTimestamp} {
		t.Setenv(name, kv[name])
	}
}

func TestConfigFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		EnvClientID:     "client-abc",
		EnvClientSecret: "secret-xyz",
		EnvPartnerID:    "partner-123",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.ClientID != "client-abc" || cfg.ClientSecret != "secret-xyz" || cfg.PartnerID != "partner-123" {
		t.Errorf("got %+v", cfg)
	}
	// Sandbox unless production is asked for explicitly: an unset or misspelled
	// variable must never move real money.
	if cfg.IsProduction {
		t.Error("must default to sandbox")
	}
}

func TestConfigFromEnvReportsEveryMissingVariable(t *testing.T) {
	setEnv(t, map[string]string{EnvClientID: "client-abc"})

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("want an error")
	}
	// One run should name everything that is missing, not just the first.
	for _, want := range []string{EnvClientSecret, EnvPartnerID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if strings.Contains(err.Error(), EnvClientID) {
		t.Error("named a variable that was present")
	}
}

func TestConfigFromEnvTrimsWhitespace(t *testing.T) {
	// A trailing newline from a secrets mount would otherwise land inside the HMAC key
	// and fail every signature with no clue why.
	setEnv(t, map[string]string{
		EnvClientID:     "  client-abc\n",
		EnvClientSecret: "secret-xyz  ",
		EnvPartnerID:    "\tpartner-123",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "client-abc" || cfg.ClientSecret != "secret-xyz" || cfg.PartnerID != "partner-123" {
		t.Errorf("got %+v", cfg)
	}
}

func TestConfigFromEnvProduction(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run("SINGAPAY_PRODUCTION="+tc.value, func(t *testing.T) {
			setEnv(t, map[string]string{
				EnvClientID: "c", EnvClientSecret: "s", EnvPartnerID: "p",
				EnvProduction: tc.value,
			})
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.IsProduction != tc.want {
				t.Errorf("IsProduction = %v, want %v", cfg.IsProduction, tc.want)
			}
		})
	}
}

func TestConfigFromEnvRejectsNonBooleanProduction(t *testing.T) {
	// "yes" silently parsing as false would put a production deploy on sandbox.
	setEnv(t, map[string]string{
		EnvClientID: "c", EnvClientSecret: "s", EnvPartnerID: "p",
		EnvProduction: "yes please",
	})
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("want an error rather than a silent default")
	}
}

func TestConfigFromEnvTimestampFormat(t *testing.T) {
	now := time.Date(2026, 9, 8, 6, 0, 0, 0, time.UTC)

	tests := []struct {
		value string
		want  string
	}{
		{"", UnixSecondsTimestamp(now)},
		{"unix", UnixSecondsTimestamp(now)},
		{"iso", ISO8601Timestamp(now)},
		{"ISO8601", ISO8601Timestamp(now)},
	}
	for _, tc := range tests {
		t.Run("format="+tc.value, func(t *testing.T) {
			setEnv(t, map[string]string{
				EnvClientID: "c", EnvClientSecret: "s", EnvPartnerID: "p",
				EnvTimestamp: tc.value,
			})
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Timestamp(now); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigFromEnvRejectsUnknownTimestampFormat(t *testing.T) {
	setEnv(t, map[string]string{
		EnvClientID: "c", EnvClientSecret: "s", EnvPartnerID: "p",
		EnvTimestamp: "rfc3339",
	})
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("want an error for an unrecognised format")
	}
}

func TestNewFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		EnvClientID: "c", EnvClientSecret: "s", EnvPartnerID: "p",
	})
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != SandboxBaseURL {
		t.Errorf("baseURL = %s, want sandbox", c.baseURL)
	}
}
