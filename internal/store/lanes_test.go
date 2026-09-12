package store

// DB-backed tests for the ADR-0025 persistence: at-most-once work orders (routing_once), the work
// order column, deterministic delivery-target order (which exclusive routing picks the executing
// identity from), per-target scope reads, and routing params.
//
// Governing: ADR-0025, SPEC-0020 REQ "At-Most-Once Work Orders", REQ "Work Orders", REQ "Exclusive
// Delivery".

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/routing"
)

func TestRoutingOnceMintsAWorkOrderAtMostOnce(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "once", "lane-m")
	wh, err := s.CreateWebhook(ctx, ep, "gitea", "lane-m", "signed", "tok-once", "whsec_once", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	trace := []byte(`{"stage":"rule","rule_id":"size-m","action":{"queue":"lane-m","exclusive":true,"once":true,"work_order":true}}`)
	workOrder := []byte(`{"version":1,"lane":"lane-m","subject":{"type":"issue","url":"https://gitea.example/o/r/issues/1"}}`)
	deliver := func(ext, onceKey string) (int64, []CreatedTodo) {
		t.Helper()
		key := wh.ID + ":" + ext
		// The ingest receiver stamps the claimed once key into the trace it stores (routing.Trace
		// OnceKey); the store reads it back on a redelivery instead of re-deciding the key.
		storedTrace := trace
		if onceKey != "" {
			storedTrace = []byte(fmt.Sprintf(`{"stage":"rule","rule_id":"size-m","action":{"queue":"lane-m","exclusive":true,"once":true,"work_order":true},"once_key":%q}`, onceKey))
		}
		evID, todos, dropped, err := s.CreateRoutedEventTodos(ctx,
			EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed",
				Verified: true, Payload: []byte(`{"action":"label_updated"}`), WebhookID: wh.ID, RoutingTrace: storedTrace},
			false, []string{ep},
			CreateTodoParams{Queue: "lane-m", Source: "gitea", Kind: "webhook", Title: "issue #1", IdempotencyKey: key,
				RoutingTrace: storedTrace, OnceKey: onceKey, WorkOrder: workOrder})
		if err != nil || dropped {
			t.Fatalf("deliver %s: dropped %v, %v", ext, dropped, err)
		}
		return evID, todos
	}
	countTodos := func() int {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, ep).Scan(&n); err != nil {
			t.Fatalf("count todos: %v", err)
		}
		return n
	}

	ev1, first := deliver("d1", "once:issue-1-zai-flash")
	if len(first) != 1 || !first[0].New || !sameJSON(t, first[0].Todo.WorkOrder, workOrder) {
		t.Fatalf("first delivery = %+v, want one new todo carrying the work order", first)
	}
	if got, err := s.GetTodo(ctx, ep, first[0].Todo.ID); err != nil || !sameJSON(t, got.WorkOrder, workOrder) {
		t.Fatalf("GetTodo work order = %s (%v)", got.WorkOrder, err)
	}

	// A different delivery about the same subject and queue (Gitea's next label_updated) is recorded
	// and mints nothing; its event trace says why.
	ev2, repeat := deliver("d2", "once:issue-1-zai-flash")
	if ev2 == ev1 || len(repeat) != 0 || countTodos() != 1 {
		t.Fatalf("repeat = (event %d, %d todos, %d total), want a new event and no todo", ev2, len(repeat), countTodos())
	}
	if ev, err := s.EventHistoryByID(ctx, ev2); err != nil || !strings.Contains(string(ev.RoutingTrace), `"once": "repeat"`) {
		t.Fatalf("repeat event trace = %s (%v), want once repeat marked", ev.RoutingTrace, err)
	}

	// A redelivery of the claiming delivery reports its todo — and does not re-mint it even once done.
	for _, state := range []string{"pending", "done"} {
		if _, err := s.pool.Exec(ctx, `UPDATE todos SET state = $2 WHERE id = $1`, first[0].Todo.ID, state); err != nil {
			t.Fatalf("set state: %v", err)
		}
		evRe, again := deliver("d1", "once:issue-1-zai-flash")
		if evRe != ev1 || len(again) != 1 || again[0].New || again[0].Todo.ID != first[0].Todo.ID || countTodos() != 1 {
			t.Fatalf("redelivery with todo %s = (event %d, %+v, %d total), want the original todo reported", state, evRe, again, countTodos())
		}
	}

	// Another queue is another work order (a re-size re-routes).
	if _, other := deliver("d3", "once:issue-1-zai"); len(other) != 1 || !other[0].New {
		t.Fatalf("other queue = %+v, want a new todo", other)
	}

	// Retention deleting the claiming event does not release the key.
	if _, err := s.pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, ev1); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	before := countTodos()
	if _, late := deliver("d4", "once:issue-1-zai-flash"); len(late) != 0 || countTodos() != before {
		t.Fatalf("after event retention = %d todos (total %d, was %d), want the key still claimed", len(late), countTodos(), before)
	}

	// A once key without the delivery's webhook is a programming error, not a silent unclaimed key.
	_, _, _, err = s.CreateRoutedEventTodos(ctx, EventInput{Source: "gitea", Family: "webhook", ExternalID: "x:nowebhook", TrustMode: "signed"},
		false, []string{ep}, CreateTodoParams{Queue: "lane-m", Title: "t", IdempotencyKey: "x:nowebhook", OnceKey: "once:k"})
	if err == nil {
		t.Fatalf("a once key with no webhook id was accepted")
	}
}

// A redelivery claims with the key its ORIGINAL delivery claimed, recorded in the event's routing
// trace — never the freshly-evaluated one. Between the two arrivals the owner may have edited the
// rules, so replaying the claiming delivery after a re-route must not claim and burn the new
// (subject, queue) key with zero todos minted there. Regression for the review finding on
// internal/store/todos.go: a redelivery's once key comes from the event's stored trace.
func TestRoutingOnceRedeliveryKeepsItsOriginalKey(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "once-key", "lane-a")
	wh, err := s.CreateWebhook(ctx, ep, "gitea", "router", "signed", "tok-once-key", "whsec_once_key", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	// traceWith mimics the ingest layer: the receiver stamps the claimed key into the trace it
	// stores with the event. An empty key means the delivery routed without a once action.
	traceWith := func(key string) []byte {
		return []byte(fmt.Sprintf(`{"stage":"rule","action":{"queue":"lane-a","once":true},"once_key":%q}`, key))
	}
	deliver := func(ext, queue, key string, trace []byte) (int64, []CreatedTodo) {
		t.Helper()
		idem := wh.ID + ":" + ext
		evID, todos, dropped, err := s.CreateRoutedEventTodos(ctx,
			EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: idem, TrustMode: "signed",
				Verified: true, Payload: []byte(`{"action":"label_updated"}`), WebhookID: wh.ID, RoutingTrace: trace},
			false, []string{ep},
			CreateTodoParams{Queue: queue, Source: "gitea", Kind: "webhook", Title: "issue #1", IdempotencyKey: idem,
				RoutingTrace: trace, OnceKey: key})
		if err != nil || dropped {
			t.Fatalf("deliver %s: dropped %v, %v", ext, dropped, err)
		}
		return evID, todos
	}

	// D1 claims (issue, lane-a) and mints there.
	if _, first := deliver("d1", "lane-a", "once:issue-1-lane-a", traceWith("once:issue-1-lane-a")); len(first) != 1 || !first[0].New {
		t.Fatalf("first delivery = %+v, want one new todo on lane-a", first)
	}

	// The owner edits the rules; Gitea's redeliver button replays D1, now evaluating to lane-b.
	if _, again := deliver("d1", "lane-b", "once:issue-1-lane-b", traceWith("once:issue-1-lane-b")); len(again) != 1 || again[0].New {
		t.Fatalf("redelivery after a re-route = %+v, want the original lane-a todo reported", again)
	}

	// The lane-b key is NOT burned: a later, legitimate delivery for the same subject routed to
	// lane-b mints there.
	if _, laneB := deliver("d2", "lane-b", "once:issue-1-lane-b", traceWith("once:issue-1-lane-b")); len(laneB) != 1 || !laneB[0].New {
		t.Fatalf("post-reroute delivery on lane-b = %+v, want one new todo", laneB)
	}

	// A redelivery of a delivery that never claimed (once was off at the time) does not claim the
	// key it would evaluate to today, either: the replay re-reports its todos, and the key stays
	// free for the next real delivery.
	noOnce := []byte(`{"stage":"target","action":{}}`)
	if _, unclaimed := deliver("d3", "lane-b", "", noOnce); len(unclaimed) != 1 || !unclaimed[0].New {
		t.Fatalf("never-claiming delivery = %+v, want one new todo", unclaimed)
	}
	if _, replay := deliver("d3", "lane-b", "once:issue-1-lane-b", traceWith("once:issue-1-lane-b")); len(replay) != 1 || replay[0].New {
		t.Fatalf("redelivery of the never-claiming delivery = %+v, want its todo re-reported", replay)
	}
	if _, fresh := deliver("d4", "lane-b", "once:issue-2-lane-b", traceWith("once:issue-2-lane-b")); len(fresh) != 1 || !fresh[0].New {
		t.Fatalf("next subject on lane-b = %+v, want one new todo", fresh)
	}
}

// vendUnder vends another endpoint for the human that owns ownerEP.
func vendUnder(t *testing.T, s *Store, ctx contextT, humanID, label string, queues ...string) string {
	t.Helper()
	ag, err := s.CreateAgent(ctx, humanID, label, "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	_, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, hash, prefix, fmt.Sprintf("%s-%d", label, time.Now().UnixNano()), queues, []string{"list_todos"})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return ep.ID
}

// Exclusive routing picks the first target scoped to the queue, so target order decides the
// executing identity. It must be owner first, then routes by grant time — not uuid or heap order.
func TestResolveWebhookTargetsOrderAndScopes(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "lanes-router", "router")
	human := ownerOf(t, s, ctx, owner)
	wh, err := s.CreateWebhook(ctx, owner, "gitea", "router", "signed", "tok-lanes-order", "whsec", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	// Mint so that grant order and uuid order disagree often enough to matter, then grant in order.
	var granted []string
	for _, q := range []string{"lane-l", "lane-s", "triage", "lane-m", "hold"} {
		ep := vendUnder(t, s, ctx, human, "pool-"+q, q)
		if err := s.AddWebhookRoute(ctx, wh.ID, ep, human); err != nil {
			t.Fatalf("add route: %v", err)
		}
		granted = append(granted, ep)
		time.Sleep(2 * time.Millisecond)
	}
	want := append([]string{owner}, granted...)
	for i := 0; i < 5; i++ {
		got, err := s.ResolveWebhookTargets(ctx, wh.ID, owner)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("targets = %v (%v), want owner then grant order %v", got, err, want)
		}
	}

	scopes, err := s.EndpointScopeQueues(ctx, []string{owner, granted[1], "not-a-uuid", "00000000-0000-0000-0000-000000000000"})
	if err != nil {
		t.Fatalf("scope queues: %v", err)
	}
	if len(scopes) != 2 || !slices.Equal(scopes[owner], []string{"router"}) || !slices.Equal(scopes[granted[1]], []string{"lane-s"}) {
		t.Fatalf("scopes = %v", scopes)
	}
	if empty, err := s.EndpointScopeQueues(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty scope read = %v (%v)", empty, err)
	}
}

func TestWebhookRoutingParamsRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "params", "q")
	human := ownerOf(t, s, ctx, ep)
	wh, err := s.CreateWebhook(ctx, ep, "cairn", "q", "signed", "tok-params", "whsec", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	params := map[string]any{"cairn_actors": []any{"joestump-agent"}, "require_verified": true}
	if _, err := s.UpdateWebhookRouting(ctx, wh.ID, human, func(WebhookRouting) (routing.Config, error) {
		return routing.Config{Params: params}, nil
	}); err != nil {
		t.Fatalf("set params: %v", err)
	}
	got, err := s.WebhookRoutingByID(ctx, wh.ID)
	if err != nil || !reflect.DeepEqual(got.Config.Params, params) {
		gotRaw, _ := json.Marshal(got.Config.Params)
		t.Fatalf("params = %s (%v), want %v", gotRaw, err, params)
	}
	if _, err := s.UpdateWebhookRouting(ctx, wh.ID, human, func(WebhookRouting) (routing.Config, error) {
		return routing.Config{}, nil
	}); err != nil {
		t.Fatalf("clear params: %v", err)
	}
	var isNull bool
	if err := s.pool.QueryRow(ctx, `SELECT routing_params IS NULL FROM endpoint_webhooks WHERE id = $1`, wh.ID).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("cleared params column null = %v (%v)", isNull, err)
	}
}
