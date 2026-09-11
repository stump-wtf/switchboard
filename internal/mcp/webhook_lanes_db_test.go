package mcp

// tools/call tests for the ADR-0025 additions to the rule verbs, over the real store: installing the
// fleet pack body as-is with set_webhook_rules, exclusive actions refusing to save until a lane pool
// is routed, params round-tripping, and a dry-run that previews the work order and once key.
//
// Governing: ADR-0025, SPEC-0020 REQ "Rule Parameters", REQ "Exclusive Delivery", REQ "Work Orders".

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/cred"
)

func TestFleetPackInstallsAndDryRuns(t *testing.T) {
	pool, ctx := routeTestPool(t)
	f := newRouteFixture(t, ctx, pool)
	verbs := append(append([]string{}, allRuleVerbs...), allRouteVerbs...)

	_, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	router, err := f.st.CreateEndpoint(ctx, f.agentA1, hash, prefix, "lanes-router-77777777", []string{"router"}, verbs)
	if err != nil {
		t.Fatalf("router endpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE endpoints SET webhook_queues = $2 WHERE id = $1`, router.ID,
		[]string{"triage", "lane-s", "lane-m", "lane-l", "lane-vision", "hold"}); err != nil {
		t.Fatalf("ceiling: %v", err)
	}
	token, hash2, prefix2, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	session, err := f.st.CreateEndpoint(ctx, f.agentA1, hash2, prefix2, "lanes-admin-88888888", []string{"router"}, verbs)
	if err != nil {
		t.Fatalf("session endpoint: %v", err)
	}
	wh, err := f.st.CreateWebhook(ctx, router.ID, "gitea", "triage", "signed", "tok-lanes-mcp", "whsec", 5)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	cs := ruleSession(t, ctx, f.st, session.Slug, token)

	raw, err := os.ReadFile("../../docs/routing/rule-packs/fleet.json")
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	body["webhook_id"] = wh.ID

	// No lane pool is routed yet: every exclusive rule is unreachable, so the save is refused.
	if msg := callErr(t, ctx, cs, "set_webhook_rules", body, codeForbidden); !strings.Contains(msg, "exclusive") {
		t.Fatalf("pre-routing save error %q does not explain exclusivity", msg)
	}

	lanes := map[string]string{}
	for _, q := range []string{"triage", "lane-s", "lane-m", "lane-l", "lane-vision", "hold"} {
		_, h, p, err := cred.Mint()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		ep, err := f.st.CreateEndpoint(ctx, f.agentA2, h, p, "pool-"+q+"-99999999", []string{q}, []string{"list_todos"})
		if err != nil {
			t.Fatalf("pool %s: %v", q, err)
		}
		lanes[q] = ep.ID
		var routed webhookRouteOut
		callOK(t, ctx, cs, "add_webhook_route", map[string]any{"webhook_id": wh.ID, "target_endpoint_id": ep.ID}, &routed)
	}

	var saved webhookRulesOut
	callOK(t, ctx, cs, "set_webhook_rules", body, &saved)
	if len(saved.Rules) != 28 || saved.DefaultAction == nil || !saved.DefaultAction.Drop {
		t.Fatalf("installed pack = %d rules, default %+v", len(saved.Rules), saved.DefaultAction)
	}
	if actors, _ := saved.Params["cairn_actors"].([]any); len(actors) != 2 {
		t.Fatalf("params did not round-trip: %v", saved.Params)
	}
	if first := saved.Rules[len(saved.Rules)-1].Action; !first.Exclusive || !first.Once || !first.WorkOrder {
		t.Fatalf("action flags did not round-trip: %+v", first)
	}

	sample, err := os.ReadFile("../routing/testdata/fleet/samples/gitea-issue-opened.json")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	var s struct {
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	}
	if err := json.Unmarshal(sample, &s); err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	var res testWebhookRulesOut
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": wh.ID, "payload": s.Body, "headers": s.Headers, "omit_envelope": true,
	}, &res)
	if res.Decision.Queue != "triage" || !slices.Equal(res.Decision.Endpoints, []string{lanes["triage"]}) ||
		!strings.HasPrefix(res.OnceKey, "once:") || res.WorkOrder == nil || res.WorkOrder.Subject == nil ||
		res.WorkOrder.Subject.URL != "https://gitea.stump.rocks/stump.wtf/switchboard/issues/212" {
		t.Fatalf("dry-run = %+v (work order %+v)", res, res.WorkOrder)
	}

	// Candidate params are tried, not saved: an empty allowlist turns the same issue into a drop.
	res = testWebhookRulesOut{}
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": wh.ID, "payload": s.Body, "headers": s.Headers, "omit_envelope": true,
		"params": map[string]any{"require_verified": true, "trusted_humans": []string{}, "trusted_agents": []string{}, "repo_prefixes": []string{"stump.wtf/"}},
	}, &res)
	if !res.Decision.Drop || res.Trace.RuleID != "untrusted-author" || res.WorkOrder != nil {
		t.Fatalf("candidate-params dry-run = %+v, want untrusted-author drop", res)
	}
	var listed webhookRulesOut
	callOK(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": wh.ID}, &listed)
	if humans, _ := listed.Params["trusted_humans"].([]any); len(humans) != 1 {
		t.Fatalf("candidate params were saved: %v", listed.Params)
	}
}
