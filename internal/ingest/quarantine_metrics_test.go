package ingest

// Quarantine metrics end to end (SPEC-0026 REQ-11): the real receiver, the real store and the real
// *metrics.Metrics, wired as internal/server wires them. Each reason the routing layer produces
// (untrusted_actor, rule_action, rule_fault) must reach switchboard_quarantine_items_total as
// itself, and a release and a discard through the ingest service must reach
// switchboard_quarantine_resolved_total under the resolver's kind. The store and metrics packages
// each test their own half against a recorder or a direct call; only this test proves the two halves
// agree on the label values, so a reason renamed on one side fails here instead of folding into
// __other__ in production.
//
// @joestump-agent 09/27/2026 - Added in review of #509.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

func TestQuarantineMetricsEndToEnd(t *testing.T) {
	ing, pool, ctx, _ := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	m := metrics.New(metrics.Options{})
	ing.SetMetrics(m)
	ing.store.SetMetrics(m) // internal/server does both (server.go)
	t.Cleanup(func() { ing.store.SetMetrics(nil) })

	st := store.New(pool)
	const secret = "whsec_q_metrics"
	h, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "tok-q-metrics", secret)
	setTrust(t, ctx, st, wh, `{"logins":["joestump"]}`)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "hold-edits", Expr: `.payload.action == "edited"`, Action: routing.Action{Quarantine: true}},
		{ID: "broken", Expr: `.payload.action == "closed" and error("boom")`, Action: routing.Action{Queue: "reviews"}},
	}, Default: &routing.Action{Queue: "reviews"}})

	// untrusted_actor, and its redelivery collapsing onto the same held item (not counted again).
	for range 2 {
		if code := postGitHubIssue(ing, "tok-q-metrics", secret, "qm-untrusted", githubIssueBody("opened", "mallory", "mallory")); code != 202 {
			t.Fatalf("untrusted delivery = %d", code)
		}
	}
	untrusted := heldTodo(t, ctx, pool, st, owner.ID)
	// rule_action
	if code := postGitHubIssue(ing, "tok-q-metrics", secret, "qm-rule", githubIssueBody("edited", "joestump", "joestump")); code != 202 {
		t.Fatalf("rule-held delivery = %d", code)
	}
	ruleHeld := heldTodo(t, ctx, pool, st, owner.ID)
	// rule_fault
	if code := postGitHubIssue(ing, "tok-q-metrics", secret, "qm-fault", githubIssueBody("closed", "joestump", "joestump")); code != 202 {
		t.Fatalf("faulting delivery = %d", code)
	}

	if _, err := ing.ReleaseQuarantined(ctx, h.ID, ruleHeld.ID, "classifier:triage-x", "reviews"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := ing.DiscardQuarantined(ctx, h.ID, untrusted.ID, "human:"+h.ID, "spam"); err != nil {
		t.Fatalf("discard: %v", err)
	}

	got := gatherCounters(t, m)
	want := map[string]float64{
		`switchboard_quarantine_items_total{reason="untrusted_actor"}`:              1,
		`switchboard_quarantine_items_total{reason="rule_action"}`:                  1,
		`switchboard_quarantine_items_total{reason="rule_fault"}`:                   1,
		`switchboard_quarantine_resolved_total{by="classifier",outcome="released"}`: 1,
		`switchboard_quarantine_resolved_total{by="human",outcome="discarded"}`:     1,
		`switchboard_routing_faults_total{cause="` + metrics.FaultCauseError + `"}`: 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok && v != 0 && strings.HasPrefix(k, "switchboard_quarantine") {
			t.Errorf("unexpected series %s = %v", k, v)
		}
	}
}
