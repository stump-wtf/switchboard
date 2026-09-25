package config

import (
	"os"
	"strings"
	"testing"
)

func TestFromEnvDefaults(t *testing.T) {
	_ = os.Unsetenv("SWITCHBOARD_ADDR")
	_ = os.Unsetenv("SWITCHBOARD_DATABASE_URL")
	cfg := FromEnv()
	if cfg.Addr != "127.0.0.1:8080" {
		t.Fatalf("default Addr: got %q, want 127.0.0.1:8080", cfg.Addr)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("default DatabaseURL: got %q, want empty", cfg.DatabaseURL)
	}
}

func TestFromEnvOverride(t *testing.T) {
	t.Setenv("SWITCHBOARD_ADDR", "0.0.0.0:9000")
	if got := FromEnv().Addr; got != "0.0.0.0:9000" {
		t.Fatalf("override Addr: got %q, want 0.0.0.0:9000", got)
	}
}

// TestMetricsTokenValidation pins SPEC-0023 REQ-1's startup contract for the scrape credential:
// unset is valid (the endpoint is then closed), a set token shorter than 32 bytes or carrying
// whitespace / non-ASCII fails startup, and the error never echoes the token.
func TestMetricsTokenValidation(t *testing.T) {
	const good = "0123456789abcdef0123456789abcdef" // exactly 32 bytes; a fixed test value
	for name, tc := range map[string]struct {
		token string
		ok    bool
	}{
		"unset":            {"", true},
		"exactly 32 bytes": {good, true},
		"longer":           {good + good, true},
		"31 bytes":         {good[:31], false},
		"inner space":      {good[:16] + " " + good[16:], false},
		"control char":     {good + "\x01", false},
		"non-ascii":        {good + "é", false},
	} {
		err := Config{MetricsToken: tc.token}.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: Validate() = %v, want nil", name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: Validate() = nil, want an error", name)
			} else if tc.token != "" && strings.Contains(err.Error(), tc.token) {
				t.Errorf("%s: validation error echoes the token", name)
			}
		}
	}
}

// TestMetricsTokenFromEnvTrimsWhitespace: a token read from a secret file often carries a trailing
// newline; it must still match the credential a scraper presents.
func TestMetricsTokenFromEnvTrimsWhitespace(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	t.Setenv("SWITCHBOARD_METRICS_TOKEN", " "+tok+"\n")
	cfg := FromEnv()
	if cfg.MetricsToken != tok {
		t.Fatalf("MetricsToken not trimmed (len %d, want %d)", len(cfg.MetricsToken), len(tok))
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	t.Setenv("SWITCHBOARD_METRICS_TOKEN", "")
	if FromEnv().MetricsToken != "" {
		t.Fatal("unset SWITCHBOARD_METRICS_TOKEN should leave MetricsToken empty")
	}
}

// TestNotifyHookMax pins SPEC-0024 REQ-1's operator bound: unset is 5, 0 is a valid kill switch,
// and a malformed or negative value fails startup rather than silently becoming the default.
func TestNotifyHookMax(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		want int
		ok   bool
	}{
		"unset":    {"", DefaultNotifyHookMax, true},
		"zero":     {"0", 0, true},
		"raised":   {"12", 12, true},
		"spaces":   {" 3 ", 3, true},
		"negative": {"-1", -1, false},
		"garbage":  {"five", -1, false},
	} {
		t.Setenv("SWITCHBOARD_NOTIFY_HOOK_MAX", tc.env)
		cfg := FromEnv()
		if cfg.NotifyHookMax != tc.want {
			t.Errorf("%s: NotifyHookMax = %d, want %d", name, cfg.NotifyHookMax, tc.want)
		}
		if err := cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", name, err, tc.ok)
		}
	}
}

// TestNotifyHookAllowCIDRs: SPEC-0024 REQ-3's allowlist is empty by default, accepts a CIDR list,
// and a malformed entry fails startup naming it.
func TestNotifyHookAllowCIDRs(t *testing.T) {
	t.Setenv("SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS", "")
	if cfg := FromEnv(); cfg.NotifyHookAllowCIDRs != "" || cfg.Validate() != nil {
		t.Fatalf("default allowlist = %q, %v", cfg.NotifyHookAllowCIDRs, cfg.Validate())
	}
	t.Setenv("SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS", " 127.0.0.1/32, 192.168.1.0/24 ")
	if err := FromEnv().Validate(); err != nil {
		t.Fatalf("valid allowlist: %v", err)
	}
	t.Setenv("SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS", "10.0.0.0/8, lan")
	if err := FromEnv().Validate(); err == nil || !strings.Contains(err.Error(), "lan") {
		t.Fatalf("malformed allowlist: %v", err)
	}
}
