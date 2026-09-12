package web

// The provider registry is INSTANCE-WIDE, not tenant-owned: `adapters` has no owner column because
// an operator-configured receiver is a property of the deployment, seeded from the server's own
// environment. Every other operator surface is scoped by owner_human_id; this one has no owner to
// scope by, so it is gated by identity against cfg.OperatorSubjects.
//
// Before that gate, /providers was open to every signed-in human. A newly invited user could read
// the configured receiver list and its ingest paths — and, far worse, POST
// /providers/{name}/remove and delete another tenant's provider outright. That was live in
// production, and a handler-level suite caught the destructive half that a read-focused audit
// (#176) had classed as disclosure only.
//
// Governing: SPEC-0017 REQ "Provider Lifecycle"; SPEC-0007 REQ "Human as Accountable Principal".

import (
	"testing"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/store"
)

func TestIsOperatorRequiresAnExplicitAllowlistMatch(t *testing.T) {
	cases := []struct {
		name     string
		subjects []string
		human    store.Human
		want     bool
	}{
		{
			// The default on any instance that has not opted in. It must match NOBODY: this gate's
			// whole job is to say no, so an unconfigured deployment is read-only rather than
			// administrable-by-everyone — which is the state that caused the incident.
			name:     "empty allowlist matches nobody",
			subjects: nil,
			human:    store.Human{ID: "h1", OIDCSubject: "pocket|joe"},
			want:     false,
		},
		{
			name:     "listed subject is an operator",
			subjects: []string{"pocket|joe"},
			human:    store.Human{ID: "h1", OIDCSubject: "pocket|joe"},
			want:     true,
		},
		{
			name:     "another signed-in human is not",
			subjects: []string{"pocket|joe"},
			human:    store.Human{ID: "h2", OIDCSubject: "pocket|eli"},
			want:     false,
		},
		{
			// A human whose subject failed to load must never match, and this is the case an empty
			// entry in the allowlist would silently break — hence splitSubjects drops empties.
			name:     "a human with no subject never matches",
			subjects: []string{"pocket|joe", ""},
			human:    store.Human{ID: "h3", OIDCSubject: ""},
			want:     false,
		},
		{
			// Subjects are compared whole. A prefix of a real subject is a different principal.
			name:     "prefix of a listed subject is not a match",
			subjects: []string{"pocket|joestump"},
			human:    store.Human{ID: "h4", OIDCSubject: "pocket|joe"},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: config.Config{OperatorSubjects: tc.subjects}}
			if got := h.isOperator(tc.human); got != tc.want {
				t.Fatalf("isOperator(%q) with allowlist %v = %v, want %v",
					tc.human.OIDCSubject, tc.subjects, got, tc.want)
			}
		})
	}
}

// splitSubjects is the parse half of the same gate: a trailing comma or a stray space must not mint
// an operator whose subject is the empty string, which would then match any human whose subject
// failed to load.
func TestSplitSubjectsDropsEmptiesAndTrims(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 0},
		{",", 0},
		{"   ", 0},
		{"pocket|joe", 1},
		{" pocket|joe , ", 1},
		{"pocket|joe,pocket|eli", 2},
		{"pocket|joe,,pocket|eli,", 2},
	} {
		if got := config.SplitSubjectsForTest(tc.raw); len(got) != tc.want {
			t.Fatalf("splitSubjects(%q) = %v (%d entries), want %d", tc.raw, got, len(got), tc.want)
		}
		for _, s := range config.SplitSubjectsForTest(tc.raw) {
			if s == "" {
				t.Fatalf("splitSubjects(%q) produced an empty subject, which would match a human with no subject", tc.raw)
			}
		}
	}
}
