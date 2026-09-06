package mcp

import (
	"slices"
	"strings"
	"testing"
)

// TestAllVerbsComposesFamilies: AllVerbs must be exactly the concatenation of the three verb
// families, with no duplicates. It cannot see family additions (want recomputes from the same
// families) or hand-composed call sites — it guards AllVerbs itself against drifting from the
// composition. Governing: SPEC-0006 REQ "Todo Drain Verbs", REQ "Webhook Self-Management";
// SPEC-0014 REQ "Agent Tool Surface over MCP".
func TestAllVerbsComposesFamilies(t *testing.T) {
	want := append(append(append([]string{}, DrainVerbs()...), WebhookVerbs()...), EventVerbs()...)
	got := AllVerbs()

	if !slices.Equal(got, want) {
		t.Fatalf("AllVerbs() = %q, want %q", strings.Join(got, " "), strings.Join(want, " "))
	}
	seen := map[string]bool{}
	for _, v := range got {
		if seen[v] {
			t.Errorf("AllVerbs() contains duplicate %q", v)
		}
		seen[v] = true
	}
}
