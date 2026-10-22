package vend

// The scope rules both vend surfaces share, checked where they live: a lifetime is read the way the
// wizard's lifetime step reads one, and a ceiling is refused for the reasons the wizard refuses it.
// The point of these tests is the parity claim, not the arithmetic: an operator who vends from a script
// gets the scope the wizard would have handed them, and no wider.
//
// Governing: SPEC-0016 REQ "Credential Lifetime", SPEC-0006 REQ "Webhook Self-Management Within a
// Vended Ceiling", SPEC-0026 REQ-6 (the reserved queue name).
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584.

import (
	"errors"
	"testing"
	"time"
)

func TestParseLifetimeAcceptsTheWizardsVocabulary(t *testing.T) {
	for _, tcase := range []struct {
		in   string
		want time.Duration
	}{
		{"90m", 90 * time.Minute},
		{"24h", 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"7d", 7 * 24 * time.Hour},
		{" 4w ", 4 * 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
	} {
		got, err := ParseLifetime(tcase.in)
		if err != nil {
			t.Errorf("ParseLifetime(%q): %v", tcase.in, err)
			continue
		}
		if got != tcase.want {
			t.Errorf("ParseLifetime(%q) = %v, want %v", tcase.in, got, tcase.want)
		}
	}
}

func TestParseLifetimeRefusesWhatWouldMintADeadCredential(t *testing.T) {
	// An empty lifetime is refused rather than read as "no expiry": omitting the field is how a vend
	// says valid until revoked, and an empty string reaching the store would expire the credential now.
	for _, in := range []string{"", "   ", "0", "-2d", "0d", "1.5d", "d", "w", "tomorrow", "7d2h"} {
		got, err := ParseLifetime(in)
		if err == nil {
			t.Errorf("ParseLifetime(%q) = %v, want a refusal", in, got)
		}
		if !errors.Is(err, ErrInvalidScope) {
			t.Errorf("ParseLifetime(%q) error %v does not match ErrInvalidScope", in, err)
		}
	}
}

func TestCheckQueues(t *testing.T) {
	if err := CheckQueues(nil); err == nil {
		t.Error("no queues accepted, want at least one required")
	}
	if err := CheckQueues([]string{"inbox", "reviews"}); err != nil {
		t.Errorf("two ordinary queues refused: %v", err)
	}
	if err := CheckQueues([]string{"inbox", " "}); err == nil {
		t.Error("a blank queue name accepted")
	}
	err := CheckQueues([]string{"quarantine"})
	if err == nil {
		t.Fatal("the reserved queue name accepted")
	}
	if !errors.Is(err, ErrInvalidScope) {
		t.Errorf("reserved queue error %v does not match ErrInvalidScope", err)
	}
	if !errors.Is(err, ErrInvalidScope) || err.Error() == "" {
		t.Errorf("the refusal must name the queue, got %q", err)
	}
}

func TestCheckCeilingRefusesWhatTheWizardWouldRefuse(t *testing.T) {
	for _, tcase := range []struct {
		name          string
		sourceTypes   []string
		webhookQueues []string
		webhookMax    int
	}{
		{"an unknown source type", []string{"carrier-pigeon"}, nil, 1},
		{"a source type switchboard cannot verify", []string{"linear"}, nil, 1},
		{"a blank source type", []string{""}, nil, 1},
		{"the reserved queue", nil, []string{"quarantine"}, 1},
		{"a negative count", nil, nil, -1},
	} {
		err := CheckCeiling(tcase.sourceTypes, tcase.webhookQueues, tcase.webhookMax)
		if err == nil {
			t.Errorf("%s accepted, want a refusal", tcase.name)
			continue
		}
		if !errors.Is(err, ErrInvalidScope) {
			t.Errorf("%s: error %v does not match ErrInvalidScope", tcase.name, err)
		}
	}

	// What the wizard hands out when the operator fills the steps in must stay legal here, or the
	// scripted path would refuse a scope the UI mints.
	for _, tcase := range []struct {
		name    string
		sources []string
		queues  []string
		max     int
	}{
		{"the basics ceiling", []string{"generic"}, []string{"inbox"}, 1},
		{"a router endpoint", []string{"github", "gitea", "cairn", "stripe", "slack", "telemetry"}, []string{"inbox", "reviews"}, 5},
		{"no self-managed webhooks", nil, nil, 0},
		{"a count with no source types", nil, []string{"inbox"}, 2},
	} {
		if err := CheckCeiling(tcase.sources, tcase.queues, tcase.max); err != nil {
			t.Errorf("%s refused: %v", tcase.name, err)
		}
	}
}
