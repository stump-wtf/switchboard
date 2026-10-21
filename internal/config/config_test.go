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

// TestDocsBaseURL pins #563: unset lands on the hosted docs site so a self-hosted landing page
// never emits relative /docs links; an override points the links at the operator's own docs
// deployment, with the trailing slash trimmed like BaseURL.
func TestDocsBaseURL(t *testing.T) {
	t.Setenv("SWITCHBOARD_DOCS_BASE_URL", "")
	if got := FromEnv().DocsBaseURL; got != DefaultDocsBaseURL {
		t.Fatalf("default DocsBaseURL: got %q, want %q", got, DefaultDocsBaseURL)
	}
	t.Setenv("SWITCHBOARD_DOCS_BASE_URL", "https://docs.example.com/docs/")
	if got := FromEnv().DocsBaseURL; got != "https://docs.example.com/docs" {
		t.Fatalf("override DocsBaseURL: got %q, want trailing slash trimmed", got)
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

// TestAttemptSummaryFromResult pins SPEC-0034 REQ-5's operator option: off when unset, on for the
// documented "true" (and any other strconv.ParseBool truth), and off for anything unparseable, so a
// typo never turns a risky option on.
func TestAttemptSummaryFromResult(t *testing.T) {
	for value, want := range map[string]bool{
		"":      false,
		"false": false,
		"0":     false,
		"yes":   false,
		"ture":  false,
		"true":  true,
		"TRUE":  true,
		"1":     true,
		" true": true,
	} {
		t.Setenv("SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT", value)
		if got := FromEnv().AttemptSummaryFromResult; got != want {
			t.Errorf("SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT=%q: AttemptSummaryFromResult = %v, want %v", value, got, want)
		}
	}
	_ = os.Unsetenv("SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT")
	if FromEnv().AttemptSummaryFromResult {
		t.Fatal("unset SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT turned the option on; it defaults to off")
	}
}

// TestWebhookAllowSuppliedSecret pins SPEC-0032 REQ-1's opt-in: unset or unparseable is off, so
// create_webhook refuses a caller-supplied secret for a source switchboard mints for unless the
// operator asked otherwise.
func TestWebhookAllowSuppliedSecret(t *testing.T) {
	for value, want := range map[string]bool{
		"":      false,
		"false": false,
		"0":     false,
		"maybe": false,
		"true":  true,
		"1":     true,
	} {
		t.Setenv("SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET", value)
		if got := FromEnv().WebhookAllowSuppliedSecret; got != want {
			t.Errorf("SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET=%q: %v, want %v", value, got, want)
		}
	}
	_ = os.Unsetenv("SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET")
	if FromEnv().WebhookAllowSuppliedSecret {
		t.Fatal("unset SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET turned the option on; it defaults to off")
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
