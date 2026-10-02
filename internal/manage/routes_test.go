package manage

// Route management's own part, against an in-memory store: which principal owns which webhook, whose
// authority a target is checked and granted on, and that every refused target looks the same. The
// real-store coverage lives with each adapter (internal/mcp webhook_routes_test.go,
// friend_authority_test.go; internal/server api_routes_test.go).
//
// Governing: SPEC-0006 REQ "Webhook Route Fan-Out Under Ownership and Friendship"; SPEC-0035 REQ
// "Shared Implementation With MCP", REQ "Route Management", REQ "Reach on Every Route"; ADR-0038,
// SPEC-0033 REQ "Closing the Audited Surfaces" (F19).
//
// @joestump-agent 10/02/2026 - Added with routes.go (#555).

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	humanStranger = "human-s"
	humanFriend   = "human-f"
	epStranger    = "11111111-1111-4111-8111-111111111111" // human-s's endpoint
	epFriend      = "22222222-2222-4222-8222-222222222222" // human-f's endpoint; H may deliver to F
	epRevoked     = "33333333-3333-4333-8333-333333333333" // H's own, revoked
	epUnknown     = "44444444-4444-4444-8444-444444444444"
	epOwnTarget   = "55555555-5555-4555-8555-555555555555" // H's own, active
)

// memRoutes models the route store: webhook ownership by endpoint, endpoint owners (active only),
// one approved edge H→F, and the route rows.
type memRoutes struct {
	owners  map[string]string // webhook → owning endpoint
	humans  map[string]string // active endpoint → human
	edges   map[[2]string]bool
	rows    map[string][]store.WebhookRoute
	removes int
}

func newMemRoutes() *memRoutes {
	return &memRoutes{
		owners: map[string]string{hookW1: epE1, hookW2: epE2},
		humans: map[string]string{epE1: humanH, epE2: humanH, epOwnTarget: humanH, epStranger: humanStranger, epFriend: humanFriend},
		edges:  map[[2]string]bool{{humanH, humanFriend}: true, {humanStranger, humanH}: true},
		rows:   map[string][]store.WebhookRoute{},
	}
}

func (m *memRoutes) WebhookOwnerEndpointFor(_ context.Context, webhookID, callerEndpointID string) (string, error) {
	if owner, ok := m.owners[webhookID]; ok && owner == callerEndpointID {
		return owner, nil
	}
	return "", store.ErrNotFound
}

func (m *memRoutes) EndpointOwnerHuman(_ context.Context, endpointID string) (string, error) {
	if h, ok := m.humans[endpointID]; ok {
		return h, nil
	}
	return "", store.ErrNotFound
}

func (m *memRoutes) FriendEdgeAuthorizesDelivery(_ context.Context, from, to string) (bool, error) {
	return m.edges[[2]string{from, to}], nil
}

func (m *memRoutes) AddWebhookRoute(_ context.Context, webhookID, target, grantedBy string) error {
	if slices.ContainsFunc(m.rows[webhookID], func(r store.WebhookRoute) bool { return r.TargetEndpointID == target }) {
		return nil
	}
	m.rows[webhookID] = append(m.rows[webhookID], store.WebhookRoute{WebhookID: webhookID, TargetEndpointID: target,
		GrantedByHumanID: grantedBy, GrantedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)})
	return nil
}

func (m *memRoutes) RemoveWebhookRoute(_ context.Context, webhookID, target string) error {
	m.removes++
	m.rows[webhookID] = slices.DeleteFunc(m.rows[webhookID], func(r store.WebhookRoute) bool { return r.TargetEndpointID == target })
	return nil
}

func (m *memRoutes) ListWebhookRoutes(_ context.Context, webhookID string) ([]store.WebhookRoute, error) {
	return m.rows[webhookID], nil
}

func routesFixture(e2State string) (Routes, *memRoutes, *memReach) {
	_, _, reach := fixture(e2State)
	st := newMemRoutes()
	return Routes{Store: st, Humans: reach}, st, reach
}

// An endpoint principal owns only its own endpoint's webhooks (F19): a sibling's webhook of the same
// human is not_found on every verb, and the human reach resolver is never asked. On its own webhook
// it routes on its own human's authority, which is what the grant records.
func TestRoutesEndpointPrincipalOwnsOnlyItsOwnWebhook(t *testing.T) {
	svc, st, reach := routesFixture("active")
	ctx := t.Context()
	e1 := EndpointPrincipalOf(epE1, humanH)

	_, err := svc.List(ctx, e1, hookW2)
	wantKind(t, "list sibling", err, ErrNotFound)
	_, err = svc.Add(ctx, e1, hookW2, epOwnTarget)
	wantKind(t, "add sibling", err, ErrNotFound)
	_, err = svc.Remove(ctx, e1, hookW2, epOwnTarget)
	wantKind(t, "remove sibling", err, ErrNotFound)
	if reach.calls != 0 || len(st.rows[hookW2]) != 0 || st.removes != 0 {
		t.Fatalf("endpoint principal reached the sibling: reach calls %d, rows %v, removes %d", reach.calls, st.rows[hookW2], st.removes)
	}

	out, err := svc.Add(ctx, e1, hookW1, epOwnTarget)
	if err != nil || out != (RouteAddOut{WebhookID: hookW1, TargetEndpointID: epOwnTarget, Routed: true}) {
		t.Fatalf("add own = %+v, %v", out, err)
	}
	if rows := st.rows[hookW1]; len(rows) != 1 || rows[0].GrantedByHumanID != humanH {
		t.Fatalf("rows = %+v, want one granted by %s", rows, humanH)
	}
	list, err := svc.List(ctx, e1, hookW1)
	if err != nil || list.OwnerEndpointID != epE1 || len(list.Routes) != 1 || list.Routes[0].GrantedAt != "2026-10-02T12:00:00Z" {
		t.Fatalf("list own = %+v, %v", list, err)
	}

	// Without its human, an endpoint principal has no authority to route anywhere.
	_, err = svc.Add(ctx, EndpointPrincipal(epE1), hookW1, epOwnTarget)
	wantKind(t, "add without a human", err, ErrForbidden)
}

// A human principal owns every webhook of every endpoint of theirs. A write that adds a route to a
// non-active endpoint's webhook is conflict with the state; list and remove still work.
func TestRoutesHumanPrincipal(t *testing.T) {
	svc, st, _ := routesFixture("revoked")
	ctx := t.Context()
	h := HumanPrincipal(humanH)

	if _, err := svc.Add(ctx, h, hookW1, epOwnTarget); err != nil {
		t.Fatalf("add on W1: %v", err)
	}
	_, err := svc.Add(ctx, h, hookW2, epOwnTarget)
	var me *Error
	if !errors.As(err, &me) || me.Kind != ErrConflict || me.State != "revoked" {
		t.Fatalf("add on a revoked endpoint's webhook: %v, want conflict with state revoked", err)
	}
	st.rows[hookW2] = []store.WebhookRoute{{WebhookID: hookW2, TargetEndpointID: epOwnTarget}}
	if list, err := svc.List(ctx, h, hookW2); err != nil || list.OwnerEndpointID != epE2 || len(list.Routes) != 1 {
		t.Fatalf("list on the revoked endpoint's webhook = %+v, %v", list, err)
	}
	if _, err := svc.Remove(ctx, h, hookW2, epOwnTarget); err != nil || len(st.rows[hookW2]) != 0 {
		t.Fatalf("remove on the revoked endpoint's webhook: %v, rows %v", err, st.rows[hookW2])
	}

	for what, p := range map[string]Principal{
		"another human": HumanPrincipal(humanStranger),
		"no principal":  {},
		"both":          {EndpointID: epE1, HumanID: humanH},
	} {
		_, err := svc.List(ctx, p, hookW1)
		wantKind(t, what, err, ErrNotFound)
	}
	_, err = Routes{Store: st}.List(ctx, h, hookW1)
	wantKind(t, "a human with no reach resolver", err, ErrNotFound)
}

// Every refused target carries the same kind and message, whatever the reason; only Reason (for the
// log) tells them apart. An approved edge in the delivering direction is the one way across humans.
func TestRoutesRefusedTargetsAreIndistinguishable(t *testing.T) {
	svc, st, _ := routesFixture("active")
	ctx := t.Context()
	h := HumanPrincipal(humanH)

	reasons := map[string]string{}
	for what, target := range map[string]string{
		"a stranger (no edge from H)":   epStranger, // S→H exists; H→S does not
		"an unknown id":                 epUnknown,
		"a revoked endpoint of H's own": epRevoked,
		"a malformed id":                "not-a-uuid",
	} {
		_, err := svc.Add(ctx, h, hookW1, target)
		var me *Error
		if !errors.As(err, &me) || me.Kind != ErrForbidden || me.Msg != RouteRefused {
			t.Fatalf("%s: %v, want forbidden %q", what, err, RouteRefused)
		}
		reasons[what] = me.Reason
	}
	if reasons["a stranger (no edge from H)"] != "unfriended human" || reasons["an unknown id"] != "unresolvable target" {
		t.Fatalf("reasons = %v", reasons)
	}
	if len(st.rows[hookW1]) != 0 {
		t.Fatalf("a refused target left rows: %v", st.rows[hookW1])
	}
	if _, err := svc.Add(ctx, h, hookW1, epFriend); err != nil {
		t.Fatalf("approved edge H→F: %v", err)
	}
}

// Remove states an end condition: an absent route and a malformed id both succeed, and a malformed id
// never reaches the store.
func TestRoutesRemoveIsIdempotent(t *testing.T) {
	svc, st, _ := routesFixture("active")
	ctx := t.Context()
	h := HumanPrincipal(humanH)
	for _, target := range []string{epOwnTarget, epStranger, "not-a-uuid"} {
		out, err := svc.Remove(ctx, h, hookW1, target)
		if err != nil || !out.Removed || out.TargetEndpointID != target {
			t.Fatalf("remove %s = %+v, %v", target, out, err)
		}
	}
	if st.removes != 2 {
		t.Fatalf("store removes = %d, want 2 (the malformed id stays out of the store)", st.removes)
	}
	for _, c := range [][2]string{{"", epOwnTarget}, {hookW1, " "}} {
		_, err := svc.Remove(ctx, h, c[0], c[1])
		wantKind(t, "missing argument", err, ErrInvalidArgument)
		_, err = svc.Add(ctx, h, c[0], c[1])
		wantKind(t, "missing argument", err, ErrInvalidArgument)
	}
}
