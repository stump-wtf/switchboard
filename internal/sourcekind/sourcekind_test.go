package sourcekind

// Table tests for SPEC-0032 REQ-1: one row per source type states the trust mode it verifies under and
// how its signing secret originates, and REQ-3: the shape rule a supplied secret must pass.
//
// Governing: SPEC-0032 REQ-1, REQ-3, REQ-10.

import (
	"errors"
	"strings"
	"testing"
)

// TestOriginPerSourceType is REQ-1's table read as assertions: switchboard mints the secret for the
// sources whose deliveries it signs with a value it chose, takes the producer's own secret for Stripe
// and Slack, and needs none where the ingest URL is the authentication.
func TestOriginPerSourceType(t *testing.T) {
	cases := []struct {
		sourceType string
		trustMode  string
		origin     string
	}{
		{"github", TrustSigned, OriginMinted},
		{"gitea", TrustSigned, OriginMinted},
		{"cairn", TrustSigned, OriginMinted},
		{"stripe", TrustSigned, OriginProvider},
		{"slack", TrustSigned, OriginProvider},
		{"linear", TrustSigned, OriginProvider},
		{"plain", TrustSigned, OriginProvider},
		{"generic", TrustToken, OriginNone},
		{"telemetry", TrustToken, OriginNone},
	}
	for _, c := range cases {
		t.Run(c.sourceType, func(t *testing.T) {
			k, ok := Lookup(c.sourceType)
			if !ok {
				t.Fatalf("%s has no row in the source table", c.sourceType)
			}
			if k.TrustMode != c.trustMode {
				t.Errorf("trust mode = %q, want %q", k.TrustMode, c.trustMode)
			}
			if k.Origin != c.origin {
				t.Errorf("secret origin = %q, want %q", k.Origin, c.origin)
			}
			if got := OriginOf(c.sourceType, c.trustMode); got != c.origin {
				t.Errorf("OriginOf = %q, want %q", got, c.origin)
			}
		})
	}
}

// TestSelectableIsWhatCreateAccepts: linear and plain are declared with a fixed provider origin but
// have no verifier yet (REQ-10 adds them), so they must not appear where create_webhook and the vend
// wizard read the list — a chip the server refuses is worse than no chip.
func TestSelectableIsWhatCreateAccepts(t *testing.T) {
	got := strings.Join(Selectable(), ",")
	if strings.Contains(got, "linear") || strings.Contains(got, "plain") {
		t.Fatalf("selectable types include a type with no verifier: %s", got)
	}
	for _, want := range []string{"cairn", "generic", "gitea", "github", "slack", "stripe", "telemetry"} {
		if !contains(Selectable(), want) {
			t.Errorf("selectable types missing %s: %s", want, got)
		}
	}
	names := Names()
	for _, want := range []string{"linear", "plain"} {
		if !contains(names, want) {
			t.Errorf("Names() must declare %s even though it is not selectable", want)
		}
	}
	if sel := Selectable(); !sortedAndUnique(sel) {
		t.Errorf("Selectable() must be sorted for a stable schema/wizard order: %v", sel)
	}
	for _, name := range Selectable() {
		if k, ok := Lookup(name); !ok || !k.Selectable {
			t.Errorf("Selectable() returned %s which the table does not mark selectable", name)
		}
	}
}

// TestOriginOfUnknownType: a source type with no row falls back to the trust mode's rule, so an
// unmapped signed type keeps behaving exactly as it did before this table existed.
func TestOriginOfUnknownType(t *testing.T) {
	if got := OriginOf("not-a-source", TrustSigned); got != OriginMinted {
		t.Errorf("unknown signed origin = %q, want minted", got)
	}
	if got := OriginOf("not-a-source", TrustToken); got != OriginNone {
		t.Errorf("unknown token origin = %q, want none", got)
	}
}

// TestCheckSecret is REQ-3's shape rule: 16-512 bytes of printable ASCII, plus the provider's own
// format. A stripe secret missing its whsec_ prefix is a paste error, and the message must name the
// field without quoting the value — an error that quotes a secret has already leaked it.
func TestCheckSecret(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		secret string
		ok     bool
	}{
		{"minted ok", "github", strings.Repeat("a", MinSecretBytes), true},
		{"too short", "github", strings.Repeat("a", MinSecretBytes-1), false},
		{"too long", "github", strings.Repeat("a", MaxSecretBytes+1), false},
		{"ceiling ok", "github", strings.Repeat("a", MaxSecretBytes), true},
		{"newline", "github", strings.Repeat("a", 15) + "\n", false},
		{"non ascii", "github", strings.Repeat("a", 15) + "é", false},
		{"stripe ok", "stripe", "whsec_" + strings.Repeat("S", 24), true},
		{"stripe no prefix", "stripe", strings.Repeat("S", 24), false},
		{"slack ok", "slack", strings.Repeat("k", 32), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckSecret(c.kind, c.secret)
			if c.ok {
				if err != nil {
					t.Fatalf("CheckSecret = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckSecret = nil, want a malformed-secret error")
			}
			if !errors.Is(err, ErrSecretMalformed) {
				t.Errorf("error %v does not wrap ErrSecretMalformed", err)
			}
			if strings.Contains(err.Error(), c.secret) {
				t.Errorf("error text echoes the secret value: %v", err)
			}
		})
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func sortedAndUnique(xs []string) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i-1] >= xs[i] {
			return false
		}
	}
	return true
}
