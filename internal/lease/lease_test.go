package lease

import (
	"errors"
	"testing"
	"time"
)

func ptr(n int) *int { return &n }

// TestTTLPrecedence pins the one rule every claim path applies: an explicit per-call
// lease_ttl_seconds wins (clamped to the cap), else the endpoint's default, else the server
// default. Governing: ADR-0043, SPEC-0006 REQ "Lease Lifecycle and Crash Safety".
func TestTTLPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		perCall  int
		endpoint *int
		want     time.Duration
	}{
		{"nothing set: server default", 0, nil, DefaultTTL},
		{"endpoint default applies", 0, ptr(3600), time.Hour},
		{"per-call beats endpoint default", 120, ptr(3600), 2 * time.Minute},
		{"per-call beats server default", 45, nil, 45 * time.Second},
		{"per-call over the cap is clamped", MaxSeconds + 1, ptr(3600), MaxTTL},
		{"negative per-call falls back to the endpoint", -5, ptr(600), 10 * time.Minute},
		{"stored default over the cap is clamped, not trusted", 0, ptr(MaxSeconds * 2), MaxTTL},
	}
	for _, c := range cases {
		if got := TTL(c.perCall, c.endpoint); got != c.want {
			t.Errorf("%s: TTL(%d, %v) = %v, want %v", c.name, c.perCall, c.endpoint, got, c.want)
		}
	}
	if got := EffectiveSeconds(nil); got != DefaultSeconds {
		t.Errorf("EffectiveSeconds(nil) = %d, want %d", got, DefaultSeconds)
	}
	if got := EffectiveSeconds(ptr(900)); got != 900 {
		t.Errorf("EffectiveSeconds(900) = %d, want 900", got)
	}
}

// TestBoundsAreTheDocumentedNumbers: the seconds forms are what the specs and the UI copy state.
func TestBoundsAreTheDocumentedNumbers(t *testing.T) {
	if DefaultSeconds != 300 || MaxSeconds != 86400 || MinDefaultSeconds != 60 {
		t.Fatalf("bounds = default %d, max %d, min default %d; want 300, 86400, 60",
			DefaultSeconds, MaxSeconds, MinDefaultSeconds)
	}
}

// TestValidateDefaultBounds: 60 and 86400 are allowed, 59 and 86401 are refused with ErrOutOfRange.
func TestValidateDefaultBounds(t *testing.T) {
	for _, ok := range []int{MinDefaultSeconds, 3600, MaxSeconds} {
		if err := ValidateDefault(ok); err != nil {
			t.Errorf("ValidateDefault(%d) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []int{MinDefaultSeconds - 1, MaxSeconds + 1, 0, -60} {
		if err := ValidateDefault(bad); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("ValidateDefault(%d) = %v, want ErrOutOfRange", bad, err)
		}
	}
}

// TestParseSeconds: bare seconds and Go durations convert; fractional seconds, blanks and junk do
// not. Range is the server's call, so 0 and 999999 parse.
func TestParseSeconds(t *testing.T) {
	good := map[string]int{"3600": 3600, "45m": 2700, "1h": 3600, " 90s ": 90, "1h30m": 5400, "0": 0, "999999": 999999}
	for in, want := range good {
		got, err := ParseSeconds(in)
		if err != nil || got != want {
			t.Errorf("ParseSeconds(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "1.5s", "soon", "1d", "500ms"} {
		if got, err := ParseSeconds(in); err == nil {
			t.Errorf("ParseSeconds(%q) = %d, want an error", in, got)
		}
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[int]string{3600: "1h (3600s)", 2700: "45m (2700s)", 90: "90s", 300: "5m (300s)"} {
		if got := Label(in); got != want {
			t.Errorf("Label(%d) = %q, want %q", in, got, want)
		}
	}
}
