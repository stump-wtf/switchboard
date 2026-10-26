package ingest

// Intake-time payload projection (ADR-0054): a rule action's payload_fields decides what the todo
// stores, while the event record keeps the delivery body whole. The two cases that matter are the
// one that shrinks (the fields are there) and the one that must NOT shrink (they are not, so the
// todo keeps the body it always carried and the trace says why).
//
// Governing: ADR-0054; SPEC-0020 REQ "Routing Trace".

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// giteaIssueProjectionBody is a Gitea issue delivery shaped like the one the report measured: a few
// hundred bytes of fields a consumer reads, wrapped in owner, avatar and sender objects it never
// opens.
const giteaIssueProjectionBody = `{"action":"opened","number":4812,` +
	`"repository":{"id":11,"full_name":"acme/widget","owner":{"login":"acme","id":7,"avatar_url":"https://gitea.example/acme.png"}},` +
	`"issue":{"number":4812,"title":"slim todo responses","labels":[{"id":1,"name":"bug"},{"id":2,"name":"size/l"}],` +
	`"user":{"login":"squidney-j","id":99,"avatar_url":"https://gitea.example/sq.png"}},"sender":{"login":"acme-bot","id":5}}`

func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("stored payload does not parse: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want payload does not parse: %v", err)
	}
	return reflect.DeepEqual(a, b)
}

func postGiteaIssue(ing *Ingest, token, secret, delivery string) *httptest.ResponseRecorder {
	return postSelfManaged(ing, token, giteaIssueProjectionBody, map[string]string{
		"X-Gitea-Delivery":  delivery,
		"X-Gitea-Event":     "issues",
		"X-Gitea-Signature": giteaSig(secret, giteaIssueProjectionBody),
	})
}

func TestSelfManagedRuleProjectionShrinksTheStoredTodo(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	const secret = "whsec_gitea_projection"
	_, _, wh := seedWebhook(t, st, ctx, "gitea", "signed", "forge", "gitea-projection", secret)
	fields := []string{"action", "issue.number", "issue.labels[].name", "repository.full_name"}
	setRules(t, ctx, st, wh.ID, routing.Config{Rules: []routing.Rule{
		{ID: "issues", Expr: `.headers["x-gitea-event"] == "issues"`,
			Action: routing.Action{Queue: "forge", PayloadFields: fields}},
	}})

	rec := postGiteaIssue(ing, "gitea-projection", secret, "guid-proj-1")
	if r := routed(t, rec); r.Created != 1 || len(r.Todos) != 1 {
		t.Fatalf("routed = %+v, want one new todo", r)
	}

	var stored, traceRaw []byte
	if err := pool.QueryRow(ctx, `SELECT payload, routing_trace FROM todos WHERE source = 'gitea'`).Scan(&stored, &traceRaw); err != nil {
		t.Fatalf("read todo: %v", err)
	}
	var eventBody []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM events WHERE external_id = $1`, wh.ID+":guid-proj-1").Scan(&eventBody); err != nil {
		t.Fatalf("read event: %v", err)
	}
	want := `{"action":"opened","issue":{"labels":[{"name":"bug"},{"name":"size/l"}],"number":4812},"repository":{"full_name":"acme/widget"}}`
	if !sameJSON(t, string(stored), want) {
		t.Fatalf("stored todo payload =\n%s\nwant\n%s", stored, want)
	}
	// The event keeps the body: replay, the Board and debugging still see everything.
	if string(eventBody) != giteaIssueProjectionBody {
		t.Fatalf("event payload was rewritten: %d bytes stored, body is %d", len(eventBody), len(giteaIssueProjectionBody))
	}
	var tr routing.Trace
	if err := json.Unmarshal(traceRaw, &tr); err != nil {
		t.Fatalf("decode todo trace: %v", err)
	}
	if !reflect.DeepEqual(tr.PayloadFields, fields) || tr.ProjectionCause != "" {
		t.Fatalf("trace = %+v, want the applied fields and no cause", tr)
	}
}

func TestSelfManagedRuleProjectionThatMatchesNothingStoresTheBodyWhole(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	const secret = "whsec_gitea_projection_miss"
	_, _, wh := seedWebhook(t, st, ctx, "gitea", "signed", "forge", "gitea-projection-miss", secret)
	setRules(t, ctx, st, wh.ID, routing.Config{Rules: []routing.Rule{
		{ID: "issues", Expr: `.headers["x-gitea-event"] == "issues"`,
			Action: routing.Action{Queue: "forge", PayloadFields: []string{"pull_request.number"}}},
	}})

	rec := postGiteaIssue(ing, "gitea-projection-miss", secret, "guid-proj-2")
	if r := routed(t, rec); r.Created != 1 {
		t.Fatalf("routed = %+v, want one new todo", r)
	}
	var stored, traceRaw []byte
	if err := pool.QueryRow(ctx, `SELECT payload, routing_trace FROM todos WHERE source = 'gitea'`).Scan(&stored, &traceRaw); err != nil {
		t.Fatalf("read todo: %v", err)
	}
	if !sameJSON(t, string(stored), giteaIssueProjectionBody) {
		t.Fatalf("stored todo payload = %s, want the delivery body whole", stored)
	}
	var tr routing.Trace
	if err := json.Unmarshal(traceRaw, &tr); err != nil {
		t.Fatalf("decode todo trace: %v", err)
	}
	if len(tr.PayloadFields) != 0 || tr.ProjectionCause == "" {
		t.Fatalf("trace = %+v, want no applied fields and a cause naming why", tr)
	}
}
