package server

// The human API's webhook and rule routes (SPEC-0035), through the real router, the real OAuth
// guard and PostgreSQL. The fixture endpoints are vended WITHOUT any rule verb, which is the case
// these routes exist for: a webhook whose owning endpoint predates the rule verbs is still managed
// by its human. Skipped without SWITCHBOARD_TEST_DATABASE_URL like every DB-backed suite.
//
// Governing: SPEC-0035 REQ "Reach on Every Route", REQ "Rule Management", REQ "Human API Surface",
// REQ "Error Handling Standards"; ADR-0022; ADR-0024.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// ruleAPICall sends one request as the given bearer and returns the status, the decoded body and the
// raw body. A string body is sent verbatim; nil sends none; anything else is JSON-encoded.
func ruleAPICall(t *testing.T, r http.Handler, bearer, method, path string, body any) (int, map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc, rec.Body.String()
}

// ruleFixture is two humans: A owns endpoints epA (webhook whA) and epA2 (webhook whA2), B owns epB
// (webhook whB). No endpoint carries a rule verb.
type ruleFixture struct {
	r                  http.Handler
	st                 *store.Store
	ctx                context.Context
	humanA, humanB     store.Human
	bearerA, bearerB   string
	epA, epA2, epB     store.Endpoint
	whA, whA2, whB     string
	ingestA, ingestA2  string
	ingestB, secretTag string
}

func newRuleFixture(t *testing.T) *ruleFixture {
	t.Helper()
	r, st, ctx := newDBRouter(t)
	f := &ruleFixture{r: r, st: st, ctx: ctx, ingestA: "ingest-tok-a-7f3c", ingestA2: "ingest-tok-a2-9d1e",
		ingestB: "ingest-tok-b-44aa", secretTag: "whsec_do_not_leak"}
	f.humanA, _ = mintSession(t, st, ctx, "test|rules-a", "Human A", "a@example.com")
	f.humanB, _ = mintSession(t, st, ctx, "test|rules-b", "Human B", "b@example.com")
	f.bearerA = mintOperatorBearer(t, st, ctx, f.humanA.ID, "cid-rules-a")
	f.bearerB = mintOperatorBearer(t, st, ctx, f.humanB.ID, "cid-rules-b")
	f.epA = consentFixture(t, st, ctx, f.humanA.ID, "router-a", "router-a-11111111", "cid-fixture-rules-a")
	f.epA2 = consentFixture(t, st, ctx, f.humanA.ID, "router-a2", "router-a2-22222222", "cid-fixture-rules-a2")
	f.epB = consentFixture(t, st, ctx, f.humanB.ID, "router-b", "router-b-33333333", "cid-fixture-rules-b")
	for _, ep := range []store.Endpoint{f.epA, f.epA2, f.epB} {
		if slices.ContainsFunc(ep.ScopeVerbs, func(v string) bool { return strings.Contains(v, "webhook_rule") }) {
			t.Fatalf("fixture endpoint %s carries a rule verb (%v); these tests need one that does not", ep.Slug, ep.ScopeVerbs)
		}
	}
	f.whA = mustAPIWebhook(t, st, ctx, f.epA.ID, f.ingestA, f.secretTag)
	f.whA2 = mustAPIWebhook(t, st, ctx, f.epA2.ID, f.ingestA2, f.secretTag)
	f.whB = mustAPIWebhook(t, st, ctx, f.epB.ID, f.ingestB, f.secretTag)
	return f
}

func mustAPIWebhook(t *testing.T, st *store.Store, ctx context.Context, endpointID, ingestToken, secret string) string {
	t.Helper()
	w, err := st.CreateWebhook(ctx, endpointID, "github", "github", "signed", ingestToken, secret, 10)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return w.ID
}

// seedDelivery records one routed delivery on webhookID, as the receiver would, and returns its id.
func seedDelivery(t *testing.T, st *store.Store, ctx context.Context, webhookID, endpointID, key, payload string) int64 {
	t.Helper()
	trace := []byte(`{"stage":"default","cause":"no_match_default","action":{"queue":"github"}}`)
	id, _, _, err := st.CreateIntakeEventTodos(ctx, store.EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: webhookID + ":" + key,
		TrustMode: "signed", Verified: true, Headers: []byte(`{"X-Github-Event":"issues"}`),
		Payload: []byte(payload), WebhookID: webhookID, RoutingTrace: trace,
	}, []string{endpointID}, store.CreateTodoParams{Queue: "github", Title: key, IdempotencyKey: webhookID + ":" + key, RoutingTrace: trace})
	if err != nil {
		t.Fatalf("seed delivery %s: %v", key, err)
	}
	return id
}

// storedConfig reads a webhook's routing straight from the store, bypassing every route under test.
func (f *ruleFixture) storedConfig(t *testing.T, webhookID string) routing.Config {
	t.Helper()
	wr, err := f.st.WebhookRoutingByID(f.ctx, webhookID)
	if err != nil {
		t.Fatalf("read routing of %s: %v", webhookID, err)
	}
	return wr.Config
}

func rulesPath(id string) string { return "/api/v1/webhooks/" + id + "/rules" }

func TestAPIListWebhooksIsReachScopedAndCarriesNoCredential(t *testing.T) {
	f := newRuleFixture(t)
	if _, err := f.st.UpdateWebhookRouting(f.ctx, f.whA, f.epA.ID, func(store.WebhookRouting) (routing.Config, error) {
		return routing.Config{Rules: []routing.Rule{{ID: "r1", Expr: "true", Action: routing.Action{Drop: true}}},
			Default: &routing.Action{Drop: true}, Params: map[string]any{"actors": []any{"joe"}}}, nil
	}); err != nil {
		t.Fatalf("seed rules: %v", err)
	}

	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, "/api/v1/webhooks", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, raw)
	}
	rows, _ := doc["webhooks"].([]any)
	byID := map[string]map[string]any{}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		byID[m["webhook_id"].(string)] = m
	}
	if len(byID) != 2 || byID[f.whA] == nil || byID[f.whA2] == nil {
		t.Fatalf("A's list = %v, want exactly %s and %s", byID, f.whA, f.whA2)
	}
	a := byID[f.whA]
	if a["endpoint_id"] != f.epA.ID || a["endpoint_slug"] != f.epA.Slug || a["endpoint_state"] != "active" ||
		a["source_type"] != "github" || a["target_queue"] != "github" || a["rule_count"] != float64(1) ||
		a["has_default_action"] != true || a["has_params"] != true {
		t.Fatalf("A's row for %s = %v", f.whA, a)
	}
	if b := byID[f.whA2]; b["rule_count"] != float64(0) || b["has_default_action"] != false || b["has_params"] != false {
		t.Fatalf("A's row for %s = %v, want no routing", f.whA2, b)
	}
	// The ingest URL is a credential for a token webhook and the secret is one for a signed one;
	// neither ever appears. Asserted on the actual values, not on a field name.
	for _, leak := range []string{f.ingestA, f.ingestA2, f.secretTag, "ingest_url", "signing_secret"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("list leaks %q: %s", leak, raw)
		}
	}

	_, docB, rawB := ruleAPICall(t, f.r, f.bearerB, http.MethodGet, "/api/v1/webhooks", nil)
	rowsB, _ := docB["webhooks"].([]any)
	if len(rowsB) != 1 || rowsB[0].(map[string]any)["webhook_id"] != f.whB || strings.Contains(rawB, f.whA) {
		t.Fatalf("B's list = %s, want only %s", rawB, f.whB)
	}

	// An endpoint credential is not a human: refused before any store read (SPEC-0035 scenario
	// "Endpoint credential on a management route").
	if code, _, _ := ruleAPICall(t, f.r, "sbk_"+strings.Repeat("x", 40), http.MethodGet, "/api/v1/webhooks", nil); code != http.StatusUnauthorized {
		t.Fatalf("endpoint credential: %d, want 401", code)
	}
}

func TestAPIWebhookRulesOwnerReplacesAndReads(t *testing.T) {
	f := newRuleFixture(t)
	body := map[string]any{
		"rules": []any{
			map[string]any{"id": "prs", "name": "pull requests", "expr": `.kind == "pull_request"`, "action": map[string]any{"queue": "ci"}},
			map[string]any{"id": "lane", "expr": `.kind == "issues"`, "action": map[string]any{"queue": "github", "work_order": true}},
		},
		"default_action": map[string]any{"drop": true},
		"params":         map[string]any{"actors": []any{"joestump"}},
	}
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), body)
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, raw)
	}
	if doc["webhook_id"] != f.whA || doc["target_queue"] != "github" {
		t.Fatalf("put response = %s", raw)
	}
	grant, _ := doc["grant"].(map[string]any)
	if queues, _ := grant["queues"].([]any); !slices.Contains(queues, any("github")) || !slices.Contains(queues, any("ci")) {
		t.Fatalf("grant = %v, want the owner's queues github and ci", grant)
	}
	cfg := f.storedConfig(t, f.whA)
	if len(cfg.Rules) != 2 || cfg.Rules[0].ID != "prs" || cfg.Rules[1].ID != "lane" || !cfg.Rules[1].Action.WorkOrder ||
		cfg.Default == nil || !cfg.Default.Drop || !reflect.DeepEqual(cfg.Params, map[string]any{"actors": []any{"joestump"}}) {
		t.Fatalf("stored config = %+v", cfg)
	}

	// GET returns the same document the save did, in list_webhook_rules' shape.
	code, got, rawGet := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, rulesPath(f.whA), nil)
	if code != http.StatusOK || !reflect.DeepEqual(got, doc) {
		t.Fatalf("get = %d %s, want the saved document %s", code, rawGet, raw)
	}

	// The GET document is itself a valid PUT body (the CLI's get --json | set round trip), and
	// replaying it changes nothing.
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), rawGet); code != http.StatusOK {
		t.Fatalf("round-trip put: %d %s", code, raw)
	}
	if after := f.storedConfig(t, f.whA); !reflect.DeepEqual(after, cfg) {
		t.Fatalf("round trip changed the config: %+v, was %+v", after, cfg)
	}

	// A's second endpoint's webhook is in A's reach too: reach is the human, not one endpoint.
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, rulesPath(f.whA2), nil); code != http.StatusOK {
		t.Fatalf("get A2: %d %s", code, raw)
	}
}

// SPEC-0035 scenario "Probing another human's webhook": another human's webhook, an unknown id and
// a malformed id are the same 404 on every rule route, and nothing of the other human's changes.
func TestAPIWebhookRulesAnotherHumansWebhookIsNotFound(t *testing.T) {
	f := newRuleFixture(t)
	seed := routing.Rule{ID: "b-seed", Expr: "true", Action: routing.Action{Drop: true}}
	if _, err := f.st.UpdateWebhookRouting(f.ctx, f.whB, f.epB.ID, func(store.WebhookRouting) (routing.Config, error) {
		return routing.Config{Rules: []routing.Rule{seed}}, nil
	}); err != nil {
		t.Fatalf("seed B's rules: %v", err)
	}
	eventB := seedDelivery(t, f.st, f.ctx, f.whB, f.epB.ID, "b-1", `{"secret":"only-B-may-read"}`)

	for _, c := range []struct {
		method, suffix string
		body           any
	}{
		{http.MethodGet, "", nil},
		{http.MethodPut, "", map[string]any{"rules": []any{}}},
		{http.MethodPost, "/test", map[string]any{"event_id": eventB}},
		{http.MethodPost, "/test", map[string]any{"payload": map[string]any{}}},
	} {
		var bodies []string
		for _, id := range []string{f.whB, "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "not-a-uuid"} {
			code, doc, raw := ruleAPICall(t, f.r, f.bearerA, c.method, rulesPath(id)+c.suffix, c.body)
			if code != http.StatusNotFound || doc["code"] != "not_found" {
				t.Fatalf("%s %s on %s: %d %s, want 404 not_found", c.method, c.suffix, id, code, raw)
			}
			bodies = append(bodies, raw)
		}
		if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
			t.Fatalf("%s %s: another human's webhook is distinguishable: %q", c.method, c.suffix, bodies)
		}
		if strings.Contains(bodies[0], "only-B-may-read") {
			t.Fatalf("%s %s leaked B's payload: %s", c.method, c.suffix, bodies[0])
		}
	}
	if cfg := f.storedConfig(t, f.whB); len(cfg.Rules) != 1 || cfg.Rules[0].ID != seed.ID {
		t.Fatalf("B's rules after A's calls = %+v, want only the seed", cfg)
	}
}

// A save that fails validation, reach or the dry run names why and leaves the previous config.
func TestAPIWebhookRulesFailedSaveKeepsThePreviousConfig(t *testing.T) {
	f := newRuleFixture(t)
	good := map[string]any{"rules": []any{map[string]any{"id": "keep", "expr": `.kind == "push"`, "action": map[string]any{"queue": "ci"}}}}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), good); code != http.StatusOK {
		t.Fatalf("seed put: %d %s", code, raw)
	}
	before := f.storedConfig(t, f.whA)

	rule := func(expr string, action map[string]any) map[string]any {
		return map[string]any{"rules": []any{map[string]any{"id": "bad", "expr": expr, "action": action}}}
	}
	for _, c := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"typo", rule(`.kind ==`, map[string]any{"queue": "ci"}), http.StatusBadRequest, routing.CodeInvalidExpression},
		{"queue outside the grant", rule(`true`, map[string]any{"queue": "elsewhere"}), http.StatusForbidden, "forbidden"},
		{"queue and drop", rule(`true`, map[string]any{"queue": "ci", "drop": true}), http.StatusBadRequest, routing.CodeInvalidRule},
		{"work order contents as input", rule(`true`, map[string]any{"queue": "ci", "work_order": map[string]any{"verified": true}}), http.StatusBadRequest, "invalid_argument"},
		{"no rules key", map[string]any{"default_action": map[string]any{"drop": true}}, http.StatusBadRequest, "invalid_argument"},
		{"params not an object", map[string]any{"rules": []any{}, "params": []any{"x"}}, http.StatusBadRequest, "invalid_argument"},
		{"malformed JSON", `{"rules": [`, http.StatusBadRequest, "invalid_argument"},
	} {
		code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), c.body)
		if code != c.status || doc["code"] != c.code || doc["error"] == "" {
			t.Fatalf("%s: %d %s, want %d with code %s", c.name, code, raw, c.status, c.code)
		}
		if after := f.storedConfig(t, f.whA); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s changed the config: %+v, was %+v", c.name, after, before)
		}
	}

	// The save-time dry run applies to the API (SPEC-0035 scenario "Dry run applies to the API"): a
	// rule that faults on a stored delivery is refused naming the rule and the event.
	ev := seedDelivery(t, f.st, f.ctx, f.whA, f.epA.ID, "n-is-a-string", `{"n":"a"}`)
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), rule(`.payload.n + 1 > 1`, map[string]any{"queue": "ci"}))
	msg, _ := doc["error"].(string)
	if code != http.StatusBadRequest || doc["code"] != "invalid_argument" || !strings.Contains(msg, "bad") ||
		!strings.Contains(msg, jsonNumber(ev)) {
		t.Fatalf("faulting save: %d %s, want 400 naming rule bad and event %d", code, raw, ev)
	}
	if after := f.storedConfig(t, f.whA); !reflect.DeepEqual(after, before) {
		t.Fatalf("faulting save changed the config: %+v", after)
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }

// PUT without a params key keeps the stored params; an explicit null clears them.
func TestAPIWebhookRulesOmittedParamsArePreserved(t *testing.T) {
	f := newRuleFixture(t)
	params := map[string]any{"repo_prefixes": []any{"stump.wtf/"}}
	withParams := map[string]any{
		"rules":  []any{map[string]any{"id": "one", "expr": `any(($params.repo_prefixes // [])[]; . == "x")`, "action": map[string]any{"drop": true}}},
		"params": params,
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), withParams); code != http.StatusOK {
		t.Fatalf("put with params: %d %s", code, raw)
	}

	// SPEC-0035 scenario "Replacing rules without params": the new rules land, the params stay.
	noParams := map[string]any{"rules": []any{map[string]any{"id": "two", "expr": "true", "action": map[string]any{"queue": "ci"}}}}
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), noParams)
	if code != http.StatusOK || !reflect.DeepEqual(doc["params"], params) {
		t.Fatalf("put without params: %d %s, want params kept", code, raw)
	}
	if cfg := f.storedConfig(t, f.whA); len(cfg.Rules) != 1 || cfg.Rules[0].ID != "two" || !reflect.DeepEqual(cfg.Params, params) {
		t.Fatalf("stored after put without params = %+v, want rule two with the params kept", cfg)
	}

	// An explicit clear is the only way to drop them.
	clearParams := map[string]any{"rules": []any{}, "params": nil}
	if code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), clearParams); code != http.StatusOK || doc["params"] != nil {
		t.Fatalf("put params:null: %d %s, want params cleared", code, raw)
	}
	if cfg := f.storedConfig(t, f.whA); len(cfg.Params) != 0 || len(cfg.Rules) != 0 {
		t.Fatalf("stored after clear = %+v, want no rules and no params", cfg)
	}

	// An empty object clears too, SPEC-0026 REQ-4's explicit clear.
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), withParams); code != http.StatusOK {
		t.Fatalf("re-add params: %d %s", code, raw)
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), map[string]any{"rules": []any{}, "params": map[string]any{}}); code != http.StatusOK {
		t.Fatalf("put params:{}: %d %s", code, raw)
	}
	if cfg := f.storedConfig(t, f.whA); len(cfg.Params) != 0 {
		t.Fatalf("stored after params:{} = %+v, want no params", cfg)
	}
}

// The dry run evaluates candidates against a stored delivery or a sample payload and saves nothing.
func TestAPIWebhookRulesTestSavesNothing(t *testing.T) {
	f := newRuleFixture(t)
	saved := map[string]any{"rules": []any{map[string]any{"id": "saved", "expr": "true", "action": map[string]any{"drop": true}}}}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), saved); code != http.StatusOK {
		t.Fatalf("seed put: %d %s", code, raw)
	}
	before := f.storedConfig(t, f.whA)
	ev := seedDelivery(t, f.st, f.ctx, f.whA, f.epA.ID, "issue-7", `{"action":"opened","issue":{"number":7}}`)
	candidate := []any{map[string]any{"id": "issues-to-ci", "expr": `.kind == "issues"`, "action": map[string]any{"queue": "ci"}}}

	// SPEC-0035 scenario "Test against a stored delivery".
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPost, rulesPath(f.whA)+"/test",
		map[string]any{"event_id": ev, "rules": candidate})
	decision, _ := doc["decision"].(map[string]any)
	if code != http.StatusOK || decision["queue"] != "ci" || decision["drop"] != false || decision["disposition"] != "routed" {
		t.Fatalf("test by event: %d %s, want routed to ci", code, raw)
	}
	if trace, _ := doc["trace"].(map[string]any); trace["rule_id"] != "issues-to-ci" {
		t.Fatalf("trace = %v, want the candidate rule", doc["trace"])
	}
	if _, ok := doc["envelope"].(map[string]any); !ok {
		t.Fatalf("no envelope in %s", raw)
	}

	// The saved rules, against a sample payload: they drop it.
	code, doc, raw = ruleAPICall(t, f.r, f.bearerA, http.MethodPost, rulesPath(f.whA)+"/test",
		map[string]any{"payload": map[string]any{"zen": "hi"}, "headers": map[string]string{"X-GitHub-Event": "ping"}, "omit_envelope": true})
	if decision, _ := doc["decision"].(map[string]any); code != http.StatusOK || decision["drop"] != true || doc["envelope"] != nil {
		t.Fatalf("test by payload: %d %s, want a drop without the envelope", code, raw)
	}

	if after := f.storedConfig(t, f.whA); !reflect.DeepEqual(after, before) {
		t.Fatalf("a dry run changed the config: %+v, was %+v", after, before)
	}

	// A delivery of another webhook — even the same human's — is not this webhook's to replay.
	evA2 := seedDelivery(t, f.st, f.ctx, f.whA2, f.epA2.ID, "a2-1", `{}`)
	if code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPost, rulesPath(f.whA)+"/test", map[string]any{"event_id": evA2}); code != http.StatusNotFound || doc["code"] != "not_found" {
		t.Fatalf("test with another webhook's event: %d %s, want 404", code, raw)
	}
	for _, body := range []map[string]any{{}, {"event_id": ev, "payload": map[string]any{}}} {
		if code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPost, rulesPath(f.whA)+"/test", body); code != http.StatusBadRequest || doc["code"] != "invalid_argument" {
			t.Fatalf("test with %v: %d %s, want 400 invalid_argument", body, code, raw)
		}
	}
}

// A write to a webhook whose owning endpoint is revoked is a 409 naming the state; reads and dry
// runs still work, so history stays inspectable after a rotation (SPEC-0035 REQ "Reach on Every
// Route").
func TestAPIWebhookRulesRevokedEndpointIsReadOnly(t *testing.T) {
	f := newRuleFixture(t)
	if err := f.st.RevokeEndpoint(f.ctx, f.epA2.ID, f.humanA.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA2), map[string]any{"rules": []any{}})
	if code != http.StatusConflict || doc["code"] != "conflict" || doc["state"] != "revoked" {
		t.Fatalf("put on a revoked endpoint's webhook: %d %s, want 409 conflict with state revoked", code, raw)
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, rulesPath(f.whA2), nil); code != http.StatusOK {
		t.Fatalf("get on a revoked endpoint's webhook: %d %s", code, raw)
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPost, rulesPath(f.whA2)+"/test", map[string]any{"payload": map[string]any{}}); code != http.StatusOK {
		t.Fatalf("test on a revoked endpoint's webhook: %d %s", code, raw)
	}
	_, list, _ := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, "/api/v1/webhooks", nil)
	found := false
	for _, row := range list["webhooks"].([]any) {
		if m := row.(map[string]any); m["webhook_id"] == f.whA2 {
			found = m["endpoint_state"] == "revoked"
		}
	}
	if !found {
		t.Fatalf("list = %v, want %s listed with endpoint_state revoked", list, f.whA2)
	}
}

// SPEC-0035 scenario "Sandbox outage during a save": 503 unavailable, and the previous config stays.
func TestAPIWebhookRulesSaveRefusedWhenRoutingUnavailable(t *testing.T) {
	f := newRuleFixture(t)
	seedDelivery(t, f.st, f.ctx, f.whA, f.epA.ID, "one", `{}`)
	before := f.storedConfig(t, f.whA)

	api := newAPIHandler(f.st, "https://sb.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	api.rules.Router = func() routing.Router { return routing.Unavailable{} }
	r := api.Routes()
	code, doc, raw := ruleAPICall(t, r, f.bearerA, http.MethodPut, "/webhooks/"+f.whA+"/rules",
		map[string]any{"rules": []any{map[string]any{"expr": "true", "action": map[string]any{"queue": "ci"}}}})
	if code != http.StatusServiceUnavailable || doc["code"] != "unavailable" {
		t.Fatalf("save with no evaluator: %d %s, want 503 unavailable", code, raw)
	}
	if after := f.storedConfig(t, f.whA); !reflect.DeepEqual(after, before) {
		t.Fatalf("an unchecked save changed the config: %+v", after)
	}
}
