package web

import (
	"strings"
	"testing"
)

// TestParseLeaseInput pins the operator vocabulary the vend forms and the card form share: blank and
// "default" are the server default, seconds and Go durations convert, and the bounds are the
// server's (60 and 86400 kept, 59 and 86401 refused with a message, never clamped).
// Governing: ADR-0043.
func TestParseLeaseInput(t *testing.T) {
	for in, want := range map[string]int{"60": 60, "86400": 86400, "1h": 3600, " 45m ": 2700, "3600": 3600} {
		got, msg := parseLeaseInput(in)
		if msg != "" || got == nil || *got != want {
			t.Errorf("parseLeaseInput(%q) = %v, %q; want %d", in, got, msg, want)
		}
	}
	for _, in := range []string{"", "  ", "default", "DEFAULT"} {
		if got, msg := parseLeaseInput(in); got != nil || msg != "" {
			t.Errorf("parseLeaseInput(%q) = %v, %q; want the server default", in, got, msg)
		}
	}
	for _, in := range []string{"59", "86401", "25h", "0", "-60", "1.5s", "soon"} {
		if got, msg := parseLeaseInput(in); got != nil || msg == "" {
			t.Errorf("parseLeaseInput(%q) = %v, %q; want a refusal", in, got, msg)
		}
	}
}

func TestLeaseCardLabel(t *testing.T) {
	if got := leaseCardLabel(nil); got != "5m (300s) · server default" {
		t.Errorf("leaseCardLabel(nil) = %q", got)
	}
	n := 3600
	if got := leaseCardLabel(&n); got != "1h (3600s) · set for this endpoint" {
		t.Errorf("leaseCardLabel(3600) = %q", got)
	}
	if !strings.Contains(leaseHelp, "300s") || !strings.Contains(leaseHelp, "60s to 86400s") {
		t.Errorf("leaseHelp does not state the bounds: %q", leaseHelp)
	}
}
