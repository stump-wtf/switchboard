package mcp

// tools/call integration tests for the ADR-0024 routing-rule verbs, over the real SDK client and
// server against the REAL store and PostgreSQL — the authorization under test is made of joined rows
// (webhook → endpoint → agent → owner human, and the webhook's live delivery targets), which a fake
// could only echo back. test_webhook_rules runs through the production out-of-process sandbox: the
// TestMain hook below lets it re-execute this test binary as its child.
//
// Skips cleanly without SWITCHBOARD_TEST_DATABASE_URL, in the house style.
//
// Governing: ADR-0024, SPEC-0020 REQ "Rule Validation at Save Time", REQ "Routing Trace", REQ
// "Isolation and Tenant Safety"; SPEC-0006 REQ "Structured Output and Stable Error Shape"; ADR-0022.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

func TestMain(m *testing.M) {
	routing.RunChildIfRequested()
	os.Exit(m.Run())
}

// --- fakeStore routing-rule stubs ------------------------------------------------------------------
//
// Like the route stubs (webhook_routes_test.go), these model nothing: rule authorization is joins,
// and every rule assertion below runs against the real store.

func (f *fakeStore) WebhookRoutingForHuman(_ context.Context, _, _ string) (store.WebhookRouting, error) {
	return store.WebhookRouting{}, store.ErrNotFound
}

func (f *fakeStore) UpdateWebhookRouting(_ context.Context, _, _ string, _ func(store.WebhookRouting) (routing.Config, error)) (store.WebhookRouting, error) {
	return store.WebhookRouting{}, store.ErrNotFound
}

func (f *fakeStore) ResolveWebhookTargets(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeStore) EventForWebhook(_ context.Context, _ int64, _ string) (store.EventHistoryDetail, error) {
	return store.EventHistoryDetail{}, store.ErrNotFound
}

func (f *fakeStore) WebhookEventsBefore(context.Context, string, time.Time, int64, int) ([]store.EventHistoryDetail, error) {
	return nil, nil
}

var allRuleVerbs = []string{
	"list_webhook_rules", "set_webhook_rules", "add_webhook_rule", "update_webhook_rule",
	"move_webhook_rule", "remove_webhook_rule", "test_webhook_rules",
}

// ruleSessions extends the route fixture with rule-capable sessions for human A (driving webhookA)
// and human B, and gives webhookA's owning endpoint a two-queue ceiling.
func ruleSessions(t *testing.T) (context.Context, *routeFixture, func(human string) (*sdk.ClientSession, string)) {
	t.Helper()
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(cancel)
	f := newRouteFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE endpoints SET webhook_queues = ARRAY['reviews','forge'] WHERE id = $1`, f.epA1); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}
	verbs := append(append(append([]string{}, allRuleVerbs...), allRouteVerbs...), "get_webhook_event", "list_webhook_events")
	open := func(human string) (*sdk.ClientSession, string) {
		agent, slug := f.agentA1, "rules-a-44444444"
		if human == "B" {
			agent, slug = f.agentB, "rules-b-55555555"
		}
		_, token := mustEndpoint(t, ctx, f.st, agent, slug, verbs)
		return ruleSession(t, ctx, f.st, slug, token), slug
	}
	return ctx, f, open
}

// ruleSession is routeSession with the dry-run sandbox given a generous deadline: the child is this
// race-instrumented test binary, which starts far slower than the production binary.
func ruleSession(t *testing.T, ctx context.Context, st *store.Store, slug, token string) *sdk.ClientSession {
	t.Helper()
	h := New(st, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	sb, err := routing.NewSandbox("", routing.WithDeadline(20*time.Second))
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	h.SetRouter(sb)
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	h.SetBaseURL("https://switchboard.example")
	client := sdk.NewClient(&sdk.Implementation{Name: "rule-test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp/" + slug,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func ruleIDs(out webhookRulesOut) []string {
	ids := make([]string, 0, len(out.Rules))
	for _, r := range out.Rules {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestWebhookRuleVerbsEditAndOrder(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")

	var out webhookRulesOut
	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{
		"webhook_id": f.webhookA, "name": "ci noise", "expr": `.kind == "workflow_run"`, "action": map[string]any{"drop": true},
	}, &out)
	if len(out.Rules) != 1 || !strings.HasPrefix(out.Rules[0].ID, "rule_") || out.TargetQueue != "reviews" {
		t.Fatalf("add = %+v, want one rule with a minted id", out)
	}
	if !slices.Equal(out.Grant.Queues, []string{"reviews", "forge"}) || !slices.Equal(out.Grant.Endpoints, []string{f.epA1}) {
		t.Fatalf("grant = %+v, want queues [reviews forge] and endpoints [%s]", out.Grant, f.epA1)
	}
	ci := out.Rules[0].ID

	callOK(t, ctx, cs, "add_webhook_rule", map[string]any{
		"webhook_id": f.webhookA, "id": "prs", "expr": `.kind == "pull_request"`, "action": map[string]any{"queue": "forge"}, "position": 0,
	}, &out)
	if !slices.Equal(ruleIDs(out), []string{"prs", ci}) {
		t.Fatalf("after positional add = %v, want [prs %s]", ruleIDs(out), ci)
	}
	callOK(t, ctx, cs, "move_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "prs", "position": 9}, &out)
	if !slices.Equal(ruleIDs(out), []string{ci, "prs"}) {
		t.Fatalf("after move = %v, want [%s prs]", ruleIDs(out), ci)
	}
	callOK(t, ctx, cs, "update_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": ci, "expr": `.kind == "workflow_job"`}, &out)
	if out.Rules[0].Expr != `.kind == "workflow_job"` || out.Rules[0].Name != "ci noise" || !out.Rules[0].Action.Drop {
		t.Fatalf("after update = %+v, want only the expression changed", out.Rules[0])
	}
	callErr(t, ctx, cs, "update_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "nope", "name": "x"}, codeRuleNotFound)

	callOK(t, ctx, cs, "remove_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": ci}, &out)
	callOK(t, ctx, cs, "remove_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": ci}, &out) // idempotent
	if !slices.Equal(ruleIDs(out), []string{"prs"}) {
		t.Fatalf("after remove = %v, want [prs]", ruleIDs(out))
	}

	callOK(t, ctx, cs, "set_webhook_rules", map[string]any{
		"webhook_id":     f.webhookA,
		"rules":          []any{map[string]any{"id": "only", "expr": "true", "action": map[string]any{"queue": "reviews"}}},
		"default_action": map[string]any{"drop": true},
	}, &out)
	var listed webhookRulesOut
	callOK(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, &listed)
	if !slices.Equal(ruleIDs(listed), []string{"only"}) || listed.DefaultAction == nil || !listed.DefaultAction.Drop {
		t.Fatalf("list after set = %+v, want [only] with a drop default", listed)
	}
}

// A save that fails validation names the rule and changes nothing; reach is bounded by the grant.
func TestWebhookRuleSaveValidation(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	var out webhookRulesOut
	callOK(t, ctx, cs, "set_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "rules": []any{map[string]any{"id": "keep", "expr": `.kind == "push"`, "action": map[string]any{"queue": "reviews"}}},
	}, &out)

	add := func(id, expr string, action map[string]any) map[string]any {
		return map[string]any{"webhook_id": f.webhookA, "id": id, "expr": expr, "action": action}
	}
	if msg := callErr(t, ctx, cs, "add_webhook_rule", add("typo", `.kind ==`, map[string]any{"drop": true}), routing.CodeInvalidExpression); !strings.Contains(msg, "typo") {
		t.Fatalf("compile error %q does not name the rule", msg)
	}
	callErr(t, ctx, cs, "add_webhook_rule", add("env", `env.DATABASE_URL != null`, map[string]any{"drop": true}), routing.CodeForbiddenFunction)
	callErr(t, ctx, cs, "add_webhook_rule", add("both", `true`, map[string]any{"queue": "forge", "drop": true}), routing.CodeInvalidRule)
	callErr(t, ctx, cs, "add_webhook_rule", add("q", `true`, map[string]any{"queue": "someone-elses"}), codeForbidden)
	callErr(t, ctx, cs, "add_webhook_rule", add("stranger", `true`, map[string]any{"queue": "forge", "endpoints": []string{f.epB}}), codeForbidden)
	callErr(t, ctx, cs, "add_webhook_rule", add("unrouted", `true`, map[string]any{"queue": "forge", "endpoints": []string{f.epA2}}), codeForbidden)

	callOK(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, &out)
	if !slices.Equal(ruleIDs(out), []string{"keep"}) {
		t.Fatalf("rules after rejected saves = %v, want [keep]", ruleIDs(out))
	}

	// Once add_webhook_route (with its own authorization) makes epA2 a target, a rule may narrow to it.
	var routed webhookRouteOut
	callOK(t, ctx, cs, "add_webhook_route", map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epA2}, &routed)
	callOK(t, ctx, cs, "add_webhook_rule", add("to-a2", `true`, map[string]any{"queue": "forge", "endpoints": []string{f.epA2}}), &out)
	if !slices.Equal(out.Grant.Endpoints, []string{f.epA1, f.epA2}) || !slices.Equal(ruleIDs(out), []string{"keep", "to-a2"}) {
		t.Fatalf("after routing to epA2 = %+v", out)
	}
}

// Human B can neither read nor change A's rules — every verb answers not_found — and cannot aim its
// own webhook's rules at A's endpoint.
func TestWebhookRuleVerbsCrossTenant(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	csA, _ := open("A")
	csB, _ := open("B")
	var out webhookRulesOut
	callOK(t, ctx, csA, "set_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "rules": []any{map[string]any{"id": "secret-rule", "expr": `.payload.token == "x"`, "action": map[string]any{"queue": "forge"}}},
	}, &out)

	for tool, args := range map[string]map[string]any{
		"list_webhook_rules":  {},
		"set_webhook_rules":   {"rules": []any{}},
		"add_webhook_rule":    {"expr": "true", "action": map[string]any{"drop": true}},
		"update_webhook_rule": {"rule_id": "secret-rule", "expr": "false"},
		"move_webhook_rule":   {"rule_id": "secret-rule", "position": 0},
		"remove_webhook_rule": {"rule_id": "secret-rule"},
		"test_webhook_rules":  {"payload": map[string]any{}},
	} {
		args["webhook_id"] = f.webhookA
		callErr(t, ctx, csB, tool, args, codeNotFound)
	}
	wr, err := f.st.WebhookRoutingByID(ctx, f.webhookA)
	if err != nil || len(wr.Config.Rules) != 1 || wr.Config.Rules[0].ID != "secret-rule" || wr.Config.Rules[0].Expr != `.payload.token == "x"` {
		t.Fatalf("A's rules after B's attempts = %+v (%v), want unchanged", wr.Config, err)
	}

	callErr(t, ctx, csB, "add_webhook_rule", map[string]any{
		"webhook_id": f.webhookB, "expr": "true", "action": map[string]any{"queue": "reviews", "endpoints": []string{f.epA1}},
	}, codeForbidden)
}

// Dry-run through the real sandbox: saved rules, candidate rules (not saved), a stored event of this
// webhook, and refusal of another tenant's stored event.
func TestTestWebhookRulesDryRun(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	var saved webhookRulesOut
	callOK(t, ctx, cs, "set_webhook_rules", map[string]any{
		"webhook_id":     f.webhookA,
		"rules":          []any{map[string]any{"id": "prs", "expr": `.kind == "pull_request" and .payload.action == "opened"`, "action": map[string]any{"queue": "forge"}}},
		"default_action": map[string]any{"drop": true},
	}, &saved)

	var res testWebhookRulesOut
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "payload": map[string]any{"action": "opened"}, "headers": map[string]string{"X-GitHub-Event": "pull_request"},
	}, &res)
	if res.Decision.Queue != "forge" || !slices.Equal(res.Decision.Endpoints, []string{f.epA1}) || res.Trace.RuleID != "prs" {
		t.Fatalf("opened PR dry-run = %+v, want forge via prs", res)
	}
	if res.Envelope["kind"] != "pull_request" || res.Envelope["verified"] != true || res.Envelope["source"] != "github" {
		t.Fatalf("envelope = %v, want kind/verified/source populated", res.Envelope)
	}

	res = testWebhookRulesOut{} // decode into a fresh value: omitempty fields would otherwise carry over
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "payload": map[string]any{"action": "closed"}, "headers": map[string]string{"X-GitHub-Event": "pull_request"}, "omit_envelope": true,
	}, &res)
	if !res.Decision.Drop || res.Trace.Cause != routing.CauseNoMatch || res.Envelope != nil {
		t.Fatalf("closed PR dry-run = %+v, want the drop default and no envelope", res)
	}

	res = testWebhookRulesOut{}
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "payload": map[string]any{"action": "closed"},
		"rules": []any{map[string]any{"id": "all", "expr": "true", "action": map[string]any{"queue": "reviews"}}},
	}, &res)
	if res.Decision.Queue != "reviews" || res.Trace.RuleID != "all" {
		t.Fatalf("candidate dry-run = %+v, want reviews via the candidate", res)
	}
	var listed webhookRulesOut
	callOK(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, &listed)
	if !slices.Equal(ruleIDs(listed), []string{"prs"}) {
		t.Fatalf("candidate rules were saved: %v", ruleIDs(listed))
	}
	callErr(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": f.webhookA, "payload": map[string]any{}, "rules": []any{map[string]any{"id": "bad", "expr": ".x ==", "action": map[string]any{"drop": true}}},
	}, routing.CodeInvalidExpression)

	// A stored delivery of THIS webhook replays through the rules, and its trace is on the event.
	trace := []byte(`{"stage":"rule","rule_index":0,"rule_id":"prs","action":{"queue":"forge"}}`)
	evA, _, _, err := f.st.CreateRoutedEventTodos(ctx, store.EventInput{
		Source: "github", Family: "webhook", EventType: "pull_request", ExternalID: f.webhookA + ":stored-1",
		TrustMode: "signed", Verified: true, Headers: []byte(`{"X-Github-Event":"pull_request"}`),
		Payload: []byte(`{"action":"opened"}`), WebhookID: f.webhookA, RoutingTrace: trace,
	}, false, []string{f.epA1}, store.CreateTodoParams{Queue: "forge", Title: "stored", IdempotencyKey: f.webhookA + ":stored-1", RoutingTrace: trace})
	if err != nil {
		t.Fatalf("seed stored event: %v", err)
	}
	res = testWebhookRulesOut{}
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA, "event_id": evA}, &res)
	if res.Decision.Queue != "forge" || res.Trace.RuleID != "prs" {
		t.Fatalf("stored event dry-run = %+v, want forge via prs", res)
	}
	var detail eventDetailOut
	callOK(t, ctx, cs, "get_webhook_event", map[string]any{"id": evA}, &detail)
	if detail.WebhookID != f.webhookA || detail.Routing == nil {
		t.Fatalf("event detail = %+v, want its webhook and routing trace", detail)
	}

	evB, _, _, err := f.st.CreateRoutedEventTodos(ctx, store.EventInput{
		Source: "github", Family: "webhook", ExternalID: f.webhookB + ":stored-b", TrustMode: "signed", Verified: true,
		Payload: []byte(`{"action":"opened","secret":"B-only"}`), WebhookID: f.webhookB,
	}, false, []string{f.epB}, store.CreateTodoParams{Queue: "reviews", Title: "b", IdempotencyKey: f.webhookB + ":stored-b"})
	if err != nil {
		t.Fatalf("seed B's event: %v", err)
	}
	callErr(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA, "event_id": evB}, codeNotFound)
	callErr(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA, "event_id": evA, "payload": map[string]any{}}, codeInvalidArgument)
	callErr(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA}, codeInvalidArgument)
}

// The rule verbs are scope-gated like every other verb family.
func TestWebhookRuleVerbsRequireScope(t *testing.T) {
	pool, ctx := routeTestPool(t)
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1) // route verbs only
	callErr(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, codeForbidden)
	callErr(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA, "rules": []any{}}, codeForbidden)
}

// A rule mutation must fit in ONE pooled connection. UpdateWebhookRouting holds a connection for its
// whole transaction; anything inside mutate that reaches for the pool again (resolving the grant's
// delivery targets did) blocks on itself here, and on a production-sized pool N concurrent rule
// edits each hold one connection while waiting on another — wedging ingest along with them.
func TestWebhookRuleMutationFitsOneConnection(t *testing.T) {
	pool, ctx := routeTestPool(t)
	f := newRouteFixture(t, ctx, pool)
	cfg := pool.Config()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	t.Cleanup(one.Close)
	st := store.New(one)
	_, token := mustEndpoint(t, ctx, st, f.agentA1, "rules-one-66666666", allRuleVerbs)
	cs := ruleSession(t, ctx, st, "rules-one-66666666", token)

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out webhookRulesOut
	callOK(t, callCtx, cs, "add_webhook_rule", map[string]any{
		"webhook_id": f.webhookA, "id": "one", "expr": "true", "action": map[string]any{"queue": "reviews"},
	}, &out)
	if !slices.Equal(ruleIDs(out), []string{"one"}) || !slices.Equal(out.Grant.Endpoints, []string{f.epA1}) {
		t.Fatalf("add on a one-connection pool = %+v", out)
	}
}

func TestTodoOutCarriesRoutingTrace(t *testing.T) {
	out := toOut(store.Todo{ID: "td_1", RoutingTrace: []byte(`{"stage":"rule","rule_id":"prs","action":{"queue":"forge"}}`)})
	m, ok := out.Routing.(map[string]any)
	if !ok || m["rule_id"] != "prs" {
		t.Fatalf("todo routing = %#v, want the decoded trace", out.Routing)
	}
	if toOut(store.Todo{ID: "td_2"}).Routing != nil || toOut(store.Todo{ID: "td_3", RoutingTrace: []byte("{")}).Routing != nil {
		t.Fatalf("absent or malformed traces must render as no routing")
	}
}
