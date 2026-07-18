package web

// parseLifetime unit contract: the vend form's optional credential lifetime accepts Go duration
// strings plus the wizard's whole-day/week shorthand, and rejects anything malformed, zero, or
// negative so an endpoint can never be vended already expired.
// Governing: SPEC-0016 REQ "Credential Lifetime" (lifetime chosen at vend time), ADR-0019.

import (
	"testing"
	"time"
)

func TestParseLifetimeAcceptsDurationsAndDayWeekShorthand(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"90m":   90 * time.Minute,
		"1h":    time.Hour,
		"24h":   24 * time.Hour,
		"1d":    24 * time.Hour,
		"7d":    7 * 24 * time.Hour,
		"30d":   30 * 24 * time.Hour,
		"1w":    7 * 24 * time.Hour,
		"4w":    4 * 7 * 24 * time.Hour,
		"1h30m": 90 * time.Minute,
	} {
		got, err := parseLifetime(in)
		if err != nil {
			t.Errorf("parseLifetime(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseLifetime(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseLifetimeRejectsMalformedZeroAndNegative(t *testing.T) {
	for _, in := range []string{"", "soon", "-1h", "0", "0h", "0d", "-2d", "1.5d", "d", "w", "1x"} {
		if d, err := parseLifetime(in); err == nil {
			t.Errorf("parseLifetime(%q) = %v, want error", in, d)
		}
	}
}
