package manage

// Principal resolution and the save semantics the two surfaces rely on, against an in-memory store
// that enforces the same endpoint-scoped ownership the real one does. The real-store, real-surface
// coverage lives with each adapter (internal/mcp webhook_rules*_test.go, internal/server
// api_webhooks_test.go); this file pins the part that is manage's own: an endpoint principal never
// resolves through a human's reach, and a human principal resolves only to the owning endpoint.
//
// Governing: ADR-0038, SPEC-0033 REQ "Closing the Audited Surfaces" (F19); SPEC-0035 REQ "Shared
// Implementation With MCP", REQ "Reach on Every Route", REQ "Rule Management".

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// memStore models one rule: a webhook's routing is readable and writable through its owning
// endpoint only, everything else is store.ErrNotFound.
type memStore struct {
	routings map[string]store.WebhookRouting
	writes   int
}

func (m *memStore) WebhookRoutingForEndpoint(_ context.Context, webhookID, endpointID string) (store.WebhookRouting, error) {
	wr, ok := m.routings[webhookID]
	if !ok || wr.EndpointID != endpointID {
		return store.WebhookRouting{}, store.ErrNotFound
	}
	return wr, nil
}

func (m *memStore) UpdateWebhookRouting(ctx context.Context, webhookID, endpointID string, mutate func(store.WebhookRouting) (routing.Config, error)) (store.WebhookRouting, error) {
	wr, err := m.WebhookRoutingForEndpoint(ctx, webhookID, endpointID)
	if err != nil {
		return store.WebhookRouting{}, err
	}
	cfg, err := mutate(wr)
	if err != nil {
		return store.WebhookRouting{}, err
	}
	wr.Config = cfg
	m.routings[webhookID] = wr
	m.writes++
	return wr, nil
}

func (m *memStore) ResolveWebhookTargets(_ context.Context, _, ownerEndpointID string) ([]string, error) {
	return []string{ownerEndpointID}, nil
}

func (m *memStore) EndpointScopeQueues(_ context.Context, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, id := range ids {
		out[id] = []string{"q"}
	}
	return out, nil
}

func (m *memStore) EventForWebhook(context.Context, int64, string) (store.EventHistoryDetail, error) {
	return store.EventHistoryDetail{}, store.ErrNotFound
}

func (m *memStore) WebhookEventsBefore(context.Context, string, time.Time, int64, int) ([]store.EventHistoryDetail, error) {
	return nil, nil
}

// memReach answers WebhookOwnerForHuman from a fixed table and counts its calls.
type memReach struct {
	owners map[string]struct {
		human string
		owner store.WebhookOwner
	}
	calls int
}

func (m *memReach) WebhookOwnerForHuman(_ context.Context, webhookID, humanID string) (store.WebhookOwner, error) {
	m.calls++
	o, ok := m.owners[webhookID]
	if !ok || o.human != humanID {
		return store.WebhookOwner{}, store.ErrNotFound
	}
	return o.owner, nil
}

const (
	humanH = "human-h"
	epE1   = "endpoint-e1"
	epE2   = "endpoint-e2"
	hookW1 = "webhook-w1" // owned by E1
	hookW2 = "webhook-w2" // owned by E2, the same human's other endpoint
)

func fixture(e2State string) (Rules, *memStore, *memReach) {
	seed := routing.Config{Rules: []routing.Rule{{ID: "seed", Expr: "true", Action: routing.Action{Drop: true}}},
		Params: map[string]any{"actors": []any{"joe"}}}
	st := &memStore{routings: map[string]store.WebhookRouting{
		hookW1: {WebhookID: hookW1, EndpointID: epE1, SourceType: "github", TargetQueue: "q", OwnerHumanID: humanH},
		hookW2: {WebhookID: hookW2, EndpointID: epE2, SourceType: "github", TargetQueue: "q", OwnerHumanID: humanH, Config: seed},
	}}
	reach := &memReach{owners: map[string]struct {
		human string
		owner store.WebhookOwner
	}{
		hookW1: {humanH, store.WebhookOwner{EndpointID: epE1, EndpointSlug: "e1", EndpointState: "active"}},
		hookW2: {humanH, store.WebhookOwner{EndpointID: epE2, EndpointSlug: "e2", EndpointState: e2State}},
	}}
	return Rules{Store: st, Humans: reach, Router: func() routing.Router { return routing.InProcess{} }}, st, reach
}

func wantKind(t *testing.T, what string, err error, k Kind) {
	t.Helper()
	if !errors.Is(err, k) {
		t.Fatalf("%s: err = %v, want %s", what, err, k)
	}
}

// The MCP rule: an endpoint principal is only ever its own webhook's. Even with a human reach
// resolver wired in that WOULD place W2 in reach, E1 gets not_found on W2 from every operation and
// the resolver is never asked.
func TestEndpointPrincipalNeverResolvesThroughHumanReach(t *testing.T) {
	svc, st, reach := fixture("active")
	ctx := t.Context()
	e1 := EndpointPrincipal(epE1)
	before := st.routings[hookW2].Config

	_, err := svc.Get(ctx, e1, hookW2)
	wantKind(t, "get", err, ErrNotFound)
	_, _, err = svc.Replace(ctx, e1, hookW2, Replacement{})
	wantKind(t, "replace", err, ErrNotFound)
	_, _, err = svc.Mutate(ctx, e1, hookW2, false, Remove("seed"))
	wantKind(t, "remove", err, ErrNotFound)
	_, err = svc.Test(ctx, e1, hookW2, TestIn{Payload: map[string]any{}})
	wantKind(t, "test", err, ErrNotFound)

	if reach.calls != 0 {
		t.Fatalf("an endpoint principal consulted human reach %d times", reach.calls)
	}
	if st.writes != 0 || !reflect.DeepEqual(st.routings[hookW2].Config, before) {
		t.Fatalf("W2 changed under E1: %+v (writes %d)", st.routings[hookW2].Config, st.writes)
	}
	// The positive control: E2 drives its own webhook.
	if out, err := svc.Get(ctx, EndpointPrincipal(epE2), hookW2); err != nil || len(out.Rules) != 1 {
		t.Fatalf("E2 on its own webhook = %+v, %v", out, err)
	}
}

// A human principal resolves to the webhook's owning endpoint, and to nothing outside reach.
func TestHumanPrincipalResolvesToTheOwningEndpoint(t *testing.T) {
	svc, st, _ := fixture("active")
	ctx := t.Context()
	h := HumanPrincipal(humanH)

	for _, id := range []string{hookW1, hookW2} {
		if out, err := svc.Get(ctx, h, id); err != nil || out.WebhookID != id {
			t.Fatalf("human get %s = %+v, %v", id, out, err)
		}
	}
	out, _, err := svc.Replace(ctx, h, hookW2, Replacement{
		Rules: []RuleIO{{ID: "new", Expr: "true", Action: ActionIO{Queue: "q"}}}, KeepParams: true})
	if err != nil || len(out.Rules) != 1 || out.Rules[0].ID != "new" {
		t.Fatalf("human replace = %+v, %v", out, err)
	}
	if got := st.routings[hookW2].Config; got.Rules[0].ID != "new" {
		t.Fatalf("stored = %+v, want the human's save on W2's own row", got)
	}

	_, err = svc.Get(ctx, HumanPrincipal("someone-else"), hookW2)
	wantKind(t, "another human", err, ErrNotFound)
	_, err = svc.Get(ctx, h, "webhook-unknown")
	wantKind(t, "unknown webhook", err, ErrNotFound)

	// No resolver, no principal, or both fields set: nothing is in reach.
	for name, c := range map[string]struct {
		svc Rules
		p   Principal
	}{
		"no resolver":  {Rules{Store: st}, h},
		"empty":        {svc, Principal{}},
		"both at once": {svc, Principal{EndpointID: epE2, HumanID: humanH}},
	} {
		_, err := c.svc.Get(ctx, c.p, hookW2)
		wantKind(t, name, err, ErrNotFound)
	}
}

// A revoked owning endpoint makes its webhook read-only to the human: writes are conflict with the
// state, reads and dry runs still work.
func TestHumanWriteToARevokedEndpointsWebhookConflicts(t *testing.T) {
	svc, st, _ := fixture("revoked")
	ctx := t.Context()
	h := HumanPrincipal(humanH)

	_, _, err := svc.Replace(ctx, h, hookW2, Replacement{})
	var me *Error
	if !errors.As(err, &me) || me.Kind != ErrConflict || me.State != "revoked" {
		t.Fatalf("write = %v, want conflict with state revoked", err)
	}
	if st.writes != 0 {
		t.Fatalf("a refused write reached the store (%d writes)", st.writes)
	}
	if _, err := svc.Get(ctx, h, hookW2); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := svc.Test(ctx, h, hookW2, TestIn{Payload: map[string]any{}}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}

// KeepParams carries the stored params forward whatever Params says; without it Params replaces
// them, nil clearing (set_webhook_rules' semantics).
func TestReplaceKeepParams(t *testing.T) {
	svc, st, _ := fixture("active")
	ctx := t.Context()
	e2 := EndpointPrincipal(epE2)
	want := st.routings[hookW2].Config.Params

	if _, _, err := svc.Replace(ctx, e2, hookW2, Replacement{KeepParams: true, Params: map[string]any{"ignored": true}}); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if got := st.routings[hookW2].Config.Params; !reflect.DeepEqual(got, want) {
		t.Fatalf("params after KeepParams = %v, want %v", got, want)
	}
	if _, _, err := svc.Replace(ctx, e2, hookW2, Replacement{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := st.routings[hookW2].Config.Params; got != nil {
		t.Fatalf("params after a replace without them = %v, want cleared", got)
	}
}

func TestWorkOrderDelta(t *testing.T) {
	wo := func(id string, on bool) routing.Rule {
		return routing.Rule{ID: id, Expr: "true", Action: routing.Action{Queue: "q", WorkOrder: on}}
	}
	prev := routing.Config{Rules: []routing.Rule{wo("a", true), wo("b", true), wo("c", false)}}
	next := routing.Config{Rules: []routing.Rule{wo("a", true), wo("c", true), wo("d", false)},
		Default: &routing.Action{Queue: "q", WorkOrder: true}}
	added, removed, kept := WorkOrderDelta(prev, next)
	if !slices.Equal(added, []string{"(default)", "c"}) || !slices.Equal(removed, []string{"b"}) || !slices.Equal(kept, []string{"a"}) {
		t.Fatalf("delta = added %v removed %v kept %v", added, removed, kept)
	}
	if a, r, k := WorkOrderDelta(routing.Config{}, routing.Config{Rules: []routing.Rule{wo("x", false)}}); a != nil || r != nil || k != nil {
		t.Fatalf("no work orders on either side = %v %v %v, want nothing", a, r, k)
	}
}
