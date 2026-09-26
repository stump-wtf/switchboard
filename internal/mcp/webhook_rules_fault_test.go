package mcp

// Fail-closed routing at the MCP surface (ADR-0031, SPEC-0026): the save-time dry-run against the
// webhook's recent deliveries, param typing, test_webhook_rules reporting faulted as blocking, and
// the list_webhook_events disposition filter. Real store, real sandbox (see webhook_rules_test.go).
//
// Governing: SPEC-0026 REQ-1 "Faults Stop Evaluation", REQ-3 "Save-Time Fault Refusal and Param
// Typing".

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// seedStoredEvent records one delivery on a webhook, as the receiver would, and returns its id.
func seedStoredEvent(t *testing.T, f *routeFixture, key, payload, disposition string) int64 {
	t.Helper()
	trace := []byte(`{"stage":"default","cause":"no_match_default","action":{"queue":"reviews"}}`)
	targets := []string{f.epA1}
	if disposition != "" {
		targets = nil
	}
	id, _, _, err := f.st.CreateIntakeEventTodos(t.Context(), store.EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: f.webhookA + ":" + key,
		TrustMode: "signed", Verified: true, Headers: []byte(`{"X-Github-Event":"issues"}`),
		Payload: []byte(payload), WebhookID: f.webhookA, RoutingTrace: trace, Disposition: disposition,
	}, targets, store.CreateTodoParams{Queue: "reviews", Title: key, IdempotencyKey: f.webhookA + ":" + key, RoutingTrace: trace})
	if err != nil {
		t.Fatalf("seed event %s: %v", key, err)
	}
	return id
}

// SPEC-0026 REQ-3 scenario "A rule that faults on real traffic is refused": the save fails with
// invalid_argument naming the rule, every event it faulted on and the cause, and the stored rules
// are unchanged. A webhook with no stored events skips the dry-run.
func TestWebhookRuleSaveRefusesFaultsOnRecentDeliveries(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	var out webhookRulesOut

	// A number plus a string faults. With no stored deliveries there is nothing to dry-run against,
	// so the save goes through.
	faulting := map[string]any{"webhook_id": f.webhookA, "id": "adder", "expr": `.payload.n + 1 > 1`,
		"action": map[string]any{"queue": "forge"}}
	callOK(t, ctx, cs, "add_webhook_rule", faulting, &out)
	callOK(t, ctx, cs, "remove_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "adder"}, &out)
	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "keep",
		"expr": `.kind == "push"`, "action": map[string]any{"queue": "reviews"}}, &out)

	ok := seedStoredEvent(t, f, "ok", `{"n":1}`, "")
	bad1 := seedStoredEvent(t, f, "bad1", `{"n":"a"}`, "")
	bad2 := seedStoredEvent(t, f, "bad2", `{"n":"b"}`, "")

	msg := callErr(t, ctx, cs, "add_webhook_rule", faulting, codeInvalidArgument)
	for _, want := range []string{"adder", routing.FaultError, strconv.FormatInt(bad1, 10), strconv.FormatInt(bad2, 10)} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, "events "+strconv.FormatInt(ok, 10)) || strings.Contains(msg, ", "+strconv.FormatInt(ok, 10)) {
		t.Fatalf("refusal %q names event %d, which does not fault", msg, ok)
	}
	callErr(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA,
		"rules": []any{map[string]any{"id": "adder", "expr": `.payload.n + 1 > 1`, "action": map[string]any{"queue": "forge"}}}}, codeInvalidArgument)
	callErr(t, ctx, cs, "update_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "keep", "expr": `.payload.n + 1 > 1`}, codeInvalidArgument)

	callOK(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, &out)
	if len(out.Rules) != 1 || out.Rules[0].ID != "keep" || out.Rules[0].Expr != `.kind == "push"` {
		t.Fatalf("rules after refused saves = %+v, want only keep, unchanged", out.Rules)
	}

	// The same intent written defensively saves: it evaluates on every recent delivery.
	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "safe",
		"expr": `(.payload.n | numbers) + 1 > 1`, "action": map[string]any{"queue": "forge"}}, &out)
	if len(out.Rules) != 2 {
		t.Fatalf("rules after a clean save = %+v, want keep and safe", out.Rules)
	}
	callOK(t, ctx, cs, "move_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "safe", "position": 0}, &out)
}

// SPEC-0026 REQ-3 scenario "Nested params refused", and the other refused shapes.
func TestWebhookRuleParamsAreTyped(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	for key, val := range map[string]any{
		"trusted": map[string]any{"alice": true},
		"mixed":   []any{"alice", 1},
		"nested":  []any{[]any{"alice"}},
	} {
		msg := callErr(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA, "rules": []any{},
			"params": map[string]any{key: val}}, routing.CodeInvalidParams)
		if !strings.Contains(msg, key) {
			t.Fatalf("params refusal %q does not name %q", msg, key)
		}
	}
	var out webhookRulesOut
	callOK(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA, "rules": []any{},
		"params": map[string]any{"trusted": []any{"alice", "bob"}, "limit": 3, "on": true, "name": "x"}}, &out)
}

// test_webhook_rules reports a fault as a blocking faulted decision with no route, never as the
// default; list_webhook_events filters on disposition and rejects an unknown one.
func TestTestWebhookRulesReportsFaultedAndEventsFilter(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")

	var res testWebhookRulesOut
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "payload": map[string]any{"n": "a"}, "omit_envelope": true,
		"rules":          []any{map[string]any{"id": "adder", "expr": `.payload.n + 1 > 1`, "action": map[string]any{"drop": true}}},
		"default_action": map[string]any{"queue": "reviews"},
	}, &res)
	d := res.Decision
	if !d.Faulted || d.Disposition != "faulted" || d.Queue != "" || len(d.Endpoints) != 0 || d.Drop ||
		d.Fault == nil || d.Fault.RuleID != "adder" || d.Fault.Cause != routing.FaultError {
		t.Fatalf("faulting dry-run = %+v, want a blocking fault at adder and no route", d)
	}
	if res.Trace.Stage != routing.StageFault {
		t.Fatalf("trace = %+v, want the fault stage", res.Trace)
	}

	faulted := seedStoredEvent(t, f, "faulted-1", `{"n":"a"}`, store.DispositionFaulted)
	routed := seedStoredEvent(t, f, "routed-1", `{"n":1}`, "")
	var list listWebhookEventsOut
	callOK(t, ctx, cs, "list_webhook_events", map[string]any{"disposition": "faulted"}, &list)
	if len(list.Events) != 1 || list.Events[0].ID != faulted || list.Events[0].Disposition != "faulted" {
		t.Fatalf("faulted filter = %+v, want exactly event %d", list.Events, faulted)
	}
	list = listWebhookEventsOut{}
	callOK(t, ctx, cs, "list_webhook_events", map[string]any{"disposition": "routed"}, &list)
	found := false
	for _, e := range list.Events {
		if e.ID == faulted {
			t.Fatalf("routed filter returned the faulted event: %+v", list.Events)
		}
		found = found || (e.ID == routed && e.Disposition == "routed")
	}
	if !found {
		t.Fatalf("routed filter = %+v, want event %d", list.Events, routed)
	}
	callErr(t, ctx, cs, "list_webhook_events", map[string]any{"disposition": "sideways"}, codeInvalidArgument)
}

// The save-time dry-run checks only deliveries the trust gate would pass today, because only those
// reach the rules on live traffic. An outsider's held deliveries, however many and however built to
// fault, never refuse the owner's save, and a flood of them does not push the owner's trusted traffic
// out of the checked window: a trusted delivery behind more than a window of held ones still refuses
// a rule that faults on it. Governing: SPEC-0026 REQ-3, REQ-5.
func TestWebhookRuleSaveDryRunSkipsHeldDeliveries(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	if _, err := f.st.SetWebhookTrustedActors(ctx, f.webhookA, f.epA1, []byte(`{"logins":["joestump"],"match":"sender"}`)); err != nil {
		t.Fatalf("set trust: %v", err)
	}
	// Oldest: one trusted delivery on which .m faults. Then more than a full window of mallory's
	// held deliveries, on which .n faults.
	trusted := seedStoredEvent(t, f, "trusted", `{"sender":{"login":"JoeStump"},"n":1,"m":"x"}`, "")
	held := make([]int64, 0, dryRunEvents+10)
	for i := range dryRunEvents + 10 {
		held = append(held, seedStoredEvent(t, f, "held-"+strconv.Itoa(i),
			`{"sender":{"login":"mallory"},"n":"a","m":1}`, store.DispositionFaulted))
	}

	var out webhookRulesOut
	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "on-n",
		"expr": `.payload.n + 1 > 1`, "action": map[string]any{"queue": "reviews"}}, &out)
	if len(out.Rules) != 1 || out.Rules[0].ID != "on-n" {
		t.Fatalf("rules = %+v, want on-n saved: it faults only on held deliveries", out.Rules)
	}
	// The scan reached the end of history inside its bound, so the report carries no warning.
	if out.DryRun == nil || out.DryRun.Checked != 1 || out.DryRun.SkippedHeld != len(held) || out.DryRun.Warning != "" {
		t.Fatalf("dry_run = %+v, want 1 checked, %d held skipped, no warning", out.DryRun, len(held))
	}
	out = webhookRulesOut{}
	callOK(t, ctx, cs, "remove_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "on-n"}, &out)
	if out.DryRun != nil {
		t.Fatalf("remove_webhook_rule reported a dry-run %+v; it is never dry-run", out.DryRun)
	}

	msg := callErr(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "on-m",
		"expr": `.payload.m + 1 > 1`, "action": map[string]any{"queue": "reviews"}}, codeInvalidArgument)
	if !strings.Contains(msg, "on-m") || !strings.Contains(msg, "events "+strconv.FormatInt(trusted, 10)) {
		t.Fatalf("refusal %q, want rule on-m on the trusted event %d", msg, trusted)
	}
	for _, id := range held {
		if strings.Contains(msg, strconv.FormatInt(id, 10)) {
			t.Fatalf("refusal %q names held event %d", msg, id)
		}
	}
}

// SPEC-0026 REQ-3: the dry-run's scan is bounded. A flood of held deliveries larger than the bound
// leaves nothing to check. The save goes through, because an outsider must not be able to block it,
// but the result says the rules went unchecked.
func TestWebhookRuleSaveDryRunReportsTheScanBound(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	if _, err := f.st.SetWebhookTrustedActors(ctx, f.webhookA, f.epA1, []byte(`{"logins":["joestump"],"match":"sender"}`)); err != nil {
		t.Fatalf("set trust: %v", err)
	}
	seedStoredEvent(t, f, "trusted", `{"sender":{"login":"joestump"},"m":"x"}`, "")
	flood := dryRunScanPages * dryRunEvents
	for i := range flood {
		seedStoredEvent(t, f, "held-"+strconv.Itoa(i), `{"sender":{"login":"mallory"},"m":1}`, store.DispositionFaulted)
	}

	var out webhookRulesOut
	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "on-m",
		"expr": `.payload.m + 1 > 1`, "action": map[string]any{"queue": "reviews"}}, &out)
	if out.DryRun == nil || out.DryRun.Checked != 0 || out.DryRun.SkippedHeld != flood ||
		!strings.Contains(out.DryRun.Warning, "only 0 of the 50") {
		t.Fatalf("dry_run = %+v, want 0 checked, %d held skipped, and a warning", out.DryRun, flood)
	}
}
