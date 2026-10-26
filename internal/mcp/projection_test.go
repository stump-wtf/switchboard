package mcp

// Payload projection on the read and claim verbs (ADR-0054): a caller that declares the paths it
// reads pays for those bytes and nothing else. The stored payload is never rewritten by a read, an
// unusable list is refused before the claim is taken, and a list that matches nothing returns the
// payload whole rather than an empty object that would read as an empty todo.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

const forgePayload = `{"action":"opened","repository":{"id":11,"full_name":"acme/widget","owner":{"login":"acme"}},` +
	`"issue":{"number":4812,"title":"slim responses","labels":[{"id":1,"name":"bug"},{"id":2,"name":"size/l"}]},` +
	`"sender":{"login":"acme-bot","html_url":"https://gitea.example/acme-bot"}}`

const projectedPayload = `{"action":"opened","issue":{"labels":[{"name":"bug"},{"name":"size/l"}],"number":4812},"repository":{"full_name":"acme/widget"}}`

var forgeFields = []string{"action", "issue.number", "issue.labels[].name", "repository.full_name"}

func projectionStore(t *testing.T) *fakeStore {
	t.Helper()
	f := newFakeStore()
	trace := []byte(`{"rule_id":"issues","queue":"reviews","payload_fields":["action"]}`)
	for _, id := range []string{"td_a", "td_b"} {
		f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: id, Queue: "reviews", Title: id,
			State: "pending", MaxAttempts: 3, Payload: []byte(forgePayload), RoutingTrace: trace})
	}
	return f
}

func jsonEqual(t *testing.T, got any, want string) bool {
	t.Helper()
	a, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &y); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	return reflect.DeepEqual(x, y)
}

func TestClaimProjectsOnlyDeclaredFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := projectionStore(t)
	cs := session(t, ctx, f, []string{"reviews"},
		[]string{"list_todos", "get_todo", "claim", "claim_next", "complete", "fail"})

	var got map[string]any
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_a", "payload_fields": forgeFields}, &got)
	if !jsonEqual(t, got["payload"], projectedPayload) {
		t.Fatalf("claim payload = %v, want %s", got["payload"], projectedPayload)
	}
	if got["routing"] == nil {
		t.Fatal("projection dropped the routing trace with the payload")
	}
	if got["payload_size"] != float64(len(forgePayload)) {
		t.Fatalf("payload_size = %v, want the stored size %d", got["payload_size"], len(forgePayload))
	}

	// The stored payload is a read's business only: get_todo without the argument still returns it
	// whole.
	got = nil
	callOK(t, ctx, cs, "get_todo", map[string]any{"id": "td_a"}, &got)
	if pl, _ := got["payload"].(map[string]any); pl["sender"] == nil {
		t.Fatalf("get_todo payload = %v, want the whole body", got["payload"])
	}
	got = nil
	callOK(t, ctx, cs, "get_todo", map[string]any{"id": "td_a", "payload_fields": forgeFields}, &got)
	if !jsonEqual(t, got["payload"], projectedPayload) {
		t.Fatalf("get_todo payload = %v, want %s", got["payload"], projectedPayload)
	}
}

func TestClaimNextProjectsOnlyDeclaredFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := projectionStore(t)
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim_next", "list_todos", "get_todo"})

	var next struct {
		Todo map[string]any `json:"todo"`
	}
	callOK(t, ctx, cs, "claim_next", map[string]any{"payload_fields": forgeFields}, &next)
	if next.Todo == nil {
		t.Fatal("claim_next answered no todo")
	}
	if !jsonEqual(t, next.Todo["payload"], projectedPayload) {
		t.Fatalf("claim_next payload = %v, want %s", next.Todo["payload"], projectedPayload)
	}
}

func TestListProjectsOnlyRowsAskedFor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := projectionStore(t)
	cs := session(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	var list struct {
		Todos []map[string]any `json:"todos"`
	}
	callOK(t, ctx, cs, "list_todos", map[string]any{"state": "pending", "limit": 200}, &list)
	for _, row := range list.Todos {
		if _, has := row["payload"]; has {
			t.Fatalf("row carries a payload nobody asked for: %v", row["payload"])
		}
	}
	list.Todos = nil
	callOK(t, ctx, cs, "list_todos", map[string]any{"state": "pending", "limit": 200, "payload_fields": []string{"issue.number"}}, &list)
	if len(list.Todos) != 2 {
		t.Fatalf("rows = %d, want 2", len(list.Todos))
	}
	for _, row := range list.Todos {
		pl, _ := row["payload"].(map[string]any)
		issue, _ := pl["issue"].(map[string]any)
		if issue == nil || issue["number"] != float64(4812) {
			t.Fatalf("row payload = %v, want the projected issue number", row["payload"])
		}
		if _, has := pl["sender"]; has {
			t.Fatalf("row payload carries undeclared fields: %v", pl)
		}
	}
}

func TestProjectionArgIsRefusedBeforeTheClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := projectionStore(t)
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "list_todos", "get_todo"})

	for _, bad := range [][]string{{"issue..number"}, {""}, {"[].number"}, {strings.Repeat("a", 300)}} {
		callErr(t, ctx, cs, "claim", map[string]any{"id": "td_a", "payload_fields": bad}, codeInvalidArgument)
	}
	var got map[string]any
	callOK(t, ctx, cs, "get_todo", map[string]any{"id": "td_a"}, &got)
	if got["state"] != "pending" {
		t.Fatalf("todo state = %v, want pending: a refused argument must not take a claim", got["state"])
	}
}

func TestProjectionThatMatchesNothingKeepsThePayloadWhole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := projectionStore(t)
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "get_todo"})

	var got map[string]any
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_a", "payload_fields": []string{"pull_request.number"}}, &got)
	pl, _ := got["payload"].(map[string]any)
	if pl == nil || pl["sender"] == nil {
		t.Fatalf("payload = %v, want the whole body when no declared path matches", got["payload"])
	}
}
