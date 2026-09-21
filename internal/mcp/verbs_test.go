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

// TestBasicWebhookCeiling pins the basics-vend webhook ceiling both ways (issue #293): a grant that
// carries create_webhook gets a scope the verb can succeed within — a non-zero max, the generic
// source type, and exactly the vended queues — and a grant without it gets no webhook scope at all.
// Governing: ADR-0023, SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling".
func TestBasicWebhookCeiling(t *testing.T) {
	queues := []string{"reviews", "deploys"}
	maxWebhooks, sources, whQueues := BasicWebhookCeiling([]string{"claim", "create_webhook"}, queues)
	if maxWebhooks != BasicWebhookMax || maxWebhooks < 1 {
		t.Errorf("with create_webhook: max = %d, want BasicWebhookMax (%d) and at least 1", maxWebhooks, BasicWebhookMax)
	}
	if !slices.Equal(sources, []string{"generic"}) {
		t.Errorf("with create_webhook: source types = %q, want [generic]", sources)
	}
	if !slices.Equal(whQueues, queues) {
		t.Errorf("with create_webhook: webhook queues = %q, want the vended queues %q", whQueues, queues)
	}
	// The returned queues are a copy: mutating them must not reach the caller's scope queues.
	whQueues[0] = "mutated"
	if queues[0] != "reviews" {
		t.Error("BasicWebhookCeiling aliased the caller's queue slice")
	}

	maxWebhooks, sources, whQueues = BasicWebhookCeiling(DrainVerbs(), queues)
	if maxWebhooks != 0 || sources != nil || whQueues != nil {
		t.Errorf("without create_webhook: got max=%d sources=%q queues=%q, want no webhook scope", maxWebhooks, sources, whQueues)
	}
}
