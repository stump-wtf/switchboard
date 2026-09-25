package store

// DB-backed tests for webhook routing persistence: rule storage under human ownership, the
// validate-inside-the-lock contract, drop semantics (the event and its dedup slot persist, no todo,
// and a dropped delivery STAYS dropped on redelivery), the trace on events and todos, and the
// webhook scoping that keeps a dry-run by event id inside its tenant.
//
// Governing: ADR-0024, SPEC-0020 REQ "Rule Validation at Save Time", REQ "Drop Action Semantics",
// REQ "Routing Trace", REQ "Isolation and Tenant Safety"; ADR-0022.

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/stump-wtf/switchboard/internal/routing"
)

func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func TestUpdateWebhookRoutingIsOwnedAndAtomic(t *testing.T) {
	s, ctx := testStore(t)
	epA := seedEndpoint(t, s, ctx, "routing-a", "q")
	epB := seedEndpoint(t, s, ctx, "routing-b", "q")
	humanA, humanB := ownerOf(t, s, ctx, epA), ownerOf(t, s, ctx, epB)
	wh, err := s.CreateWebhook(ctx, epA, "cairn", "q", "signed", "tok-routing-a", "whsec_a", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	// A new webhook routes exactly as before routing existed: no rules, no default.
	wr, err := s.WebhookRoutingByID(ctx, wh.ID)
	if err != nil {
		t.Fatalf("routing by id: %v", err)
	}
	if len(wr.Config.Rules) != 0 || wr.Config.Default != nil || wr.TargetQueue != "q" || wr.EndpointID != epA || wr.TrustMode != "signed" {
		t.Fatalf("fresh routing = %+v, want empty config on target queue q owned by %s", wr, epA)
	}

	cfg := routing.Config{
		Rules:   []routing.Rule{{ID: "r1", Name: "one", Expr: `.kind == "x"`, Action: routing.Action{Queue: "q", Endpoints: []string{epA}}}},
		Default: &routing.Action{Drop: true},
	}
	if _, err := s.UpdateWebhookRouting(ctx, wh.ID, humanA, func(WebhookRouting) (routing.Config, error) { return cfg, nil }); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	got, err := s.WebhookRoutingForHuman(ctx, wh.ID, humanA)
	if err != nil || !reflect.DeepEqual(got.Config, cfg) {
		t.Fatalf("reread = %+v (%v), want %+v", got.Config, err, cfg)
	}

	// Another human learns nothing and changes nothing — and its mutate callback never runs.
	if _, err := s.WebhookRoutingForHuman(ctx, wh.ID, humanB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other human read = %v, want ErrNotFound", err)
	}
	called := false
	_, err = s.UpdateWebhookRouting(ctx, wh.ID, humanB, func(WebhookRouting) (routing.Config, error) {
		called = true
		return routing.Config{}, nil
	})
	if !errors.Is(err, ErrNotFound) || called {
		t.Fatalf("other human update = %v (mutate called: %v), want ErrNotFound without calling mutate", err, called)
	}
	for _, id := range []string{"", "not-a-uuid"} {
		if _, err := s.WebhookRoutingByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("routing by malformed id %q = %v, want ErrNotFound", id, err)
		}
		if _, err := s.UpdateWebhookRouting(ctx, id, humanA, func(WebhookRouting) (routing.Config, error) { return cfg, nil }); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update malformed id %q = %v, want ErrNotFound", id, err)
		}
	}

	// A failed validation (any mutate error) leaves the previous configuration in force.
	invalid := errors.New("rule 0: expr does not parse")
	if _, err := s.UpdateWebhookRouting(ctx, wh.ID, humanA, func(WebhookRouting) (routing.Config, error) {
		return routing.Config{}, invalid
	}); !errors.Is(err, invalid) {
		t.Fatalf("failing mutate = %v, want its error unchanged", err)
	}
	if got, _ := s.WebhookRoutingByID(ctx, wh.ID); !reflect.DeepEqual(got.Config, cfg) {
		t.Fatalf("config after failed save = %+v, want the previous %+v", got.Config, cfg)
	}

	// Clearing persists an empty list and a NULL default: back to the target queue.
	if _, err := s.UpdateWebhookRouting(ctx, wh.ID, humanA, func(WebhookRouting) (routing.Config, error) { return routing.Config{}, nil }); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var rules string
	var defaultNull bool
	if err := s.pool.QueryRow(ctx, `SELECT routing_rules::text, default_action IS NULL FROM endpoint_webhooks WHERE id = $1`, wh.ID).Scan(&rules, &defaultNull); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if rules != "[]" || !defaultNull {
		t.Fatalf("cleared columns = (%s, default null %v), want ([], true)", rules, defaultNull)
	}
}

func TestCreateRoutedEventTodosDropSpendsTheSlot(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "routing-drop", "q")
	wh, err := s.CreateWebhook(ctx, ep, "cairn", "q", "signed", "tok-routing-drop", "whsec_d", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	dropTrace := []byte(`{"stage":"rule","rule_index":0,"rule_id":"noise","action":{"drop":true}}`)
	queueTrace := []byte(`{"stage":"default","cause":"no_match_default","action":{"queue":"q"}}`)
	in := EventInput{Source: "cairn", Family: "webhook", EventType: "artifact.created", ExternalID: wh.ID + ":evt-1",
		TrustMode: "signed", Verified: true, Payload: []byte(`{"kind":"artifact.created"}`), WebhookID: wh.ID, RoutingTrace: dropTrace}
	params := func(key string) CreateTodoParams {
		return CreateTodoParams{Queue: "q", Source: "cairn", Kind: "webhook", Title: "t", IdempotencyKey: key, RoutingTrace: queueTrace}
	}

	evID, todos, dropped, err := s.CreateRoutedEventTodos(ctx, in, true, nil, CreateTodoParams{})
	if err != nil || !dropped || len(todos) != 0 {
		t.Fatalf("drop = (%d todos, dropped %v, %v), want a dropped delivery with no todos", len(todos), dropped, err)
	}
	ev, err := s.EventHistoryByID(ctx, callerOf(t, s, ctx, ep), evID)
	if err != nil || ev.WebhookID != wh.ID || !sameJSON(t, ev.RoutingTrace, dropTrace) {
		t.Fatalf("dropped event = %+v (%v), want webhook %s and the drop trace", ev.EventHistoryItem, err, wh.ID)
	}

	// The owner has since changed the rules so this delivery would route — but a redelivery of a
	// dropped event stays dropped: the first decision owns the dedup slot.
	redelivery := in
	redelivery.RoutingTrace = queueTrace
	evID2, todos2, dropped2, err := s.CreateRoutedEventTodos(ctx, redelivery, false, []string{ep}, params(in.ExternalID))
	if err != nil || evID2 != evID || !dropped2 || len(todos2) != 0 {
		t.Fatalf("redelivery = (event %d, %d todos, dropped %v, %v), want event %d still dropped", evID2, len(todos2), dropped2, err, evID)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, ep).Scan(&n); err != nil || n != 0 {
		t.Fatalf("todos after drop + redelivery = %d (%v), want 0", n, err)
	}

	// A routed delivery stamps its trace on the event and on the todo, through every read path.
	routed := redelivery
	routed.ExternalID = wh.ID + ":evt-2"
	evID3, todos3, dropped3, err := s.CreateRoutedEventTodos(ctx, routed, false, []string{ep}, params(routed.ExternalID))
	if err != nil || dropped3 || len(todos3) != 1 || !sameJSON(t, todos3[0].Todo.RoutingTrace, queueTrace) {
		t.Fatalf("routed = (%+v, dropped %v, %v), want one todo carrying the trace", todos3, dropped3, err)
	}
	reread, err := s.GetTodo(ctx, ep, todos3[0].Todo.ID)
	if err != nil || !sameJSON(t, reread.RoutingTrace, queueTrace) || reread.EventID == nil || *reread.EventID != evID3 {
		t.Fatalf("GetTodo = %+v (%v), want the trace and event %d", reread, err, evID3)
	}

	// Only a drop may have no targets.
	noTargets := routed
	noTargets.ExternalID = wh.ID + ":evt-3"
	if _, _, _, err := s.CreateRoutedEventTodos(ctx, noTargets, false, nil, params(noTargets.ExternalID)); err == nil {
		t.Fatalf("a non-drop delivery with no targets was accepted")
	}
}

func TestEventForWebhookIsScopedToItsWebhook(t *testing.T) {
	s, ctx := testStore(t)
	epA := seedEndpoint(t, s, ctx, "routing-ev-a", "q")
	epB := seedEndpoint(t, s, ctx, "routing-ev-b", "q")
	whA, err := s.CreateWebhook(ctx, epA, "generic", "q", "token", "tok-routing-ev-a", "", 5)
	if err != nil {
		t.Fatalf("create webhook A: %v", err)
	}
	whB, err := s.CreateWebhook(ctx, epB, "generic", "q", "token", "tok-routing-ev-b", "", 5)
	if err != nil {
		t.Fatalf("create webhook B: %v", err)
	}
	trace := []byte(`{"stage":"default","cause":"no_match_default","action":{"queue":"q"}}`)
	evID, _, _, err := s.CreateRoutedEventTodos(ctx,
		EventInput{Source: "generic", Family: "webhook", ExternalID: whA.ID + ":a1", TrustMode: "token",
			Payload: []byte(`{"secret":"only-A"}`), WebhookID: whA.ID, RoutingTrace: trace},
		false, []string{epA}, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: whA.ID + ":a1", RoutingTrace: trace})
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}

	if ev, err := s.EventForWebhook(ctx, evID, whA.ID); err != nil || ev.ID != evID || string(ev.Payload) != `{"secret":"only-A"}` {
		t.Fatalf("own webhook read = %+v (%v), want event %d", ev.EventHistoryItem, err, evID)
	}
	for _, c := range []struct {
		id      int64
		webhook string
	}{{evID, whB.ID}, {evID, "not-a-uuid"}, {0, whA.ID}, {evID + 1000, whA.ID}} {
		if _, err := s.EventForWebhook(ctx, c.id, c.webhook); !errors.Is(err, ErrNotFound) {
			t.Fatalf("EventForWebhook(%d, %q) = %v, want ErrNotFound", c.id, c.webhook, err)
		}
	}

	items, err := s.ListEventHistory(ctx, callerOf(t, s, ctx, epA), EventHistoryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == evID {
			found = it.WebhookID == whA.ID && sameJSON(t, it.RoutingTrace, trace)
		}
	}
	if !found {
		t.Fatalf("history summary for %d lacks its webhook and trace: %+v", evID, items)
	}

	// Deleting the webhook keeps the delivery's history (the reference nulls, the row stays) rather
	// than blocking delete_webhook on its own event rows.
	if err := s.DeleteWebhook(ctx, whA.ID, epA); err != nil {
		t.Fatalf("delete webhook with recorded events: %v", err)
	}
	if ev, err := s.EventHistoryByID(ctx, callerOf(t, s, ctx, epA), evID); err != nil || ev.WebhookID != "" {
		t.Fatalf("event after webhook delete = %+v (%v), want it kept with no webhook", ev.EventHistoryItem, err)
	}
}
