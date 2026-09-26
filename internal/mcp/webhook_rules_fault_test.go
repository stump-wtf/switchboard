package mcp

// Fail-closed routing at the MCP surface (ADR-0031, SPEC-0026): the save-time dry-run against the
// webhook's recent deliveries, param typing, test_webhook_rules reporting faulted as blocking, and
// the list_webhook_events disposition filter. Real store, real sandbox (see webhook_rules_test.go).
//
// Governing: SPEC-0026 REQ-1 "Faults Stop Evaluation", REQ-3 "Save-Time Fault Refusal and Param
// Typing".

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

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

// interleavingRouter runs before once, on the first evaluation, and then routes with next. It lets a
// test land a concurrent edit between the save-time dry-run and the locked write.
type interleavingRouter struct {
	once   sync.Once
	before func()
	next   routing.Router
}

func (r *interleavingRouter) Route(ctx context.Context, cfg routing.Config, g routing.Grant, in routing.EnvelopeInput) routing.Decision {
	r.once.Do(r.before)
	return r.next.Route(ctx, cfg, g, in)
}

// openRulesWith opens a rule session for human A whose dry-runs use router.
func openRulesWith(t *testing.T, ctx context.Context, f *routeFixture, slug string, router routing.Router) *sdk.ClientSession {
	t.Helper()
	verbs := append(append([]string{}, allRuleVerbs...), "list_webhook_events")
	_, token := mustEndpoint(t, ctx, f.st, f.agentA1, slug, verbs)
	return ruleSessionWithRouter(t, ctx, f.st, slug, token, router)
}

func storedRuleIDs(t *testing.T, ctx context.Context, f *routeFixture) []string {
	t.Helper()
	wr, err := f.st.WebhookRoutingByID(ctx, f.webhookA)
	if err != nil {
		t.Fatalf("read routing: %v", err)
	}
	ids := make([]string, 0, len(wr.Config.Rules))
	for _, r := range wr.Config.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}

// The dry-run runs before the routing row lock, so the locked write saves the checked candidate
// only if the stored rules are still the ones it was built from. An edit that lands in between
// answers conflict, and the concurrent edit is what stays stored: nothing that was never dry-run is
// saved (SPEC-0026 REQ-3, REQ-13).
func TestWebhookRuleSaveConflictsWithAConcurrentEdit(t *testing.T) {
	ctx, f, _ := ruleSessions(t)
	seedStoredEvent(t, f, "ev", `{"n":1}`, "")
	router := &interleavingRouter{next: routing.InProcess{}}
	router.before = func() {
		if _, err := f.st.UpdateWebhookRouting(ctx, f.webhookA, f.humanA, func(cur store.WebhookRouting) (routing.Config, error) {
			cfg := cur.Config
			cfg.Rules = append(slices.Clone(cfg.Rules), routing.Rule{ID: "concurrent", Expr: `false`, Action: routing.Action{Queue: "reviews"}})
			return cfg, nil
		}); err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}
	cs := openRulesWith(t, ctx, f, "rules-c-66666666", router)
	callErr(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "mine",
		"expr": `.kind == "push"`, "action": map[string]any{"queue": "forge"}}, codeConflict)
	if got := storedRuleIDs(t, ctx, f); !slices.Equal(got, []string{"concurrent"}) {
		t.Fatalf("stored rules = %v, want only the concurrent edit", got)
	}
}

// When the evaluator cannot run, a save that needs a dry-run is refused with unavailable (retry),
// not blamed on a rule, and nothing is saved.
func TestWebhookRuleSaveRefusedWhenRoutingUnavailable(t *testing.T) {
	ctx, f, _ := ruleSessions(t)
	seedStoredEvent(t, f, "ev", `{"n":1}`, "")
	cs := openRulesWith(t, ctx, f, "rules-u-77777777", routing.Unavailable{})
	callErr(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "mine",
		"expr": `.kind == "push"`, "action": map[string]any{"queue": "forge"}}, codeUnavailable)
	if got := storedRuleIDs(t, ctx, f); len(got) != 0 {
		t.Fatalf("stored rules = %v, want none after an unavailable dry-run", got)
	}
}

// move_webhook_rule is dry-run too: moving an unguarded rule ahead of the rule that guards it faults
// on recent traffic, so the move is refused and the order is unchanged.
func TestWebhookRuleMoveRefusedWhenItFaults(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	var out webhookRulesOut
	callOK(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA, "rules": []any{
		map[string]any{"id": "guard", "expr": `(.payload.n | type) != "number"`, "action": map[string]any{"drop": true}},
		map[string]any{"id": "adder", "expr": `.payload.n + 1 > 1`, "action": map[string]any{"queue": "forge"}},
	}}, &out)
	seedStoredEvent(t, f, "num", `{"n":1}`, "")
	bad := seedStoredEvent(t, f, "str", `{"n":"a"}`, "")

	msg := callErr(t, ctx, cs, "move_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "adder", "position": 0}, codeInvalidArgument)
	if !strings.Contains(msg, "adder") || !strings.Contains(msg, strconv.FormatInt(bad, 10)) {
		t.Fatalf("move refusal %q does not name adder and event %d", msg, bad)
	}
	if got := storedRuleIDs(t, ctx, f); !slices.Equal(got, []string{"guard", "adder"}) {
		t.Fatalf("stored order = %v, want guard then adder", got)
	}
}

// Params saved before shapes were checked do not block rule edits. Only a save that changes the
// params is held to the shape rule, so an owner can always edit, and remove, the rules that read
// them (SPEC-0026 REQ-3).
func TestWebhookRuleEditsKeepPreexistingParams(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	legacy := map[string]any{"trusted": map[string]any{"alice": true}} // an object: refused today
	if _, err := f.st.UpdateWebhookRouting(ctx, f.webhookA, f.humanA, func(store.WebhookRouting) (routing.Config, error) {
		return routing.Config{Params: legacy, Rules: []routing.Rule{
			{ID: "old", Expr: `$params.trusted | has("alice")`, Action: routing.Action{Queue: "reviews"}}}}, nil
	}); err != nil {
		t.Fatalf("seed legacy params: %v", err)
	}
	var out webhookRulesOut
	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "new",
		"expr": `.kind == "push"`, "action": map[string]any{"queue": "forge"}}, &out)
	callOK(t, ctx, cs, "move_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "new", "position": 0}, &out)
	callOK(t, ctx, cs, "remove_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "old"}, &out)
	if !slices.Equal(ruleIDs(out), []string{"new"}) || !reflect.DeepEqual(out.Params, legacy) {
		t.Fatalf("after edits: rules %v params %v, want [new] with the params kept", ruleIDs(out), out.Params)
	}
	// Changing the params is held to the shape rule.
	callErr(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA, "rules": []any{},
		"params": map[string]any{"trusted": map[string]any{"bob": true}}}, routing.CodeInvalidParams)
}
