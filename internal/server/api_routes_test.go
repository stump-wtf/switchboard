package server

// The human API's webhook route routes (SPEC-0035 REQ "Route Management"), through the real router,
// the real OAuth guard and PostgreSQL, on the rule suite's two-human fixture. None of the fixture
// endpoints carries a routing verb, and none of their credentials is used: this is the operator who
// owns a webhook managing its routes without the owning endpoint's MCP key (#555). Skipped without
// SWITCHBOARD_TEST_DATABASE_URL like every DB-backed suite.
//
// Governing: SPEC-0035 REQ "Route Management", REQ "Reach on Every Route", REQ "Shared Implementation
// With MCP", REQ "Human API Surface"; SPEC-0006 REQ "Webhook Route Fan-Out Under Ownership and
// Friendship"; ADR-0022; ADR-0010.
//
// @joestump-agent 10/02/2026 - Added with the route routes (#555).

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

func webhookRoutesPath(webhookID string) string { return "/api/v1/webhooks/" + webhookID + "/routes" }

func webhookRoutePath(webhookID, endpointID string) string {
	return webhookRoutesPath(webhookID) + "/" + endpointID
}

// targets is where a delivery to webhookID lands right now: the receiver's own read, so an assertion
// on it is about delivery and not about a row that might be ignored.
func (f *ruleFixture) targets(t *testing.T, webhookID, ownerEndpointID string) []string {
	t.Helper()
	got, err := f.st.ResolveWebhookTargets(f.ctx, webhookID, ownerEndpointID)
	if err != nil {
		t.Fatalf("resolve targets of %s: %v", webhookID, err)
	}
	return got
}

// routeRows reads a webhook's explicit route rows straight from the store.
func (f *ruleFixture) routeRows(t *testing.T, webhookID string) []store.WebhookRoute {
	t.Helper()
	rows, err := f.st.ListWebhookRoutes(f.ctx, webhookID)
	if err != nil {
		t.Fatalf("list routes of %s: %v", webhookID, err)
	}
	return rows
}

// A human adds, lists and removes routes on webhooks owned by any of their endpoints. Add is
// idempotent in both spellings; remove succeeds whether or not the route exists, and on the owner
// endpoint changes nothing.
func TestAPIWebhookRoutesOwnerAddsListsAndRemoves(t *testing.T) {
	f := newRuleFixture(t)

	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, webhookRoutesPath(f.whA), nil)
	if routes, ok := doc["routes"].([]any); code != http.StatusOK || doc["webhook_id"] != f.whA ||
		doc["owner_endpoint_id"] != f.epA.ID || !ok || len(routes) != 0 {
		t.Fatalf("list before any route: %d %s, want the owner and an empty routes array", code, raw)
	}

	// PUT twice: one row, one more delivery target, and the same answer both times.
	code, first, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, webhookRoutePath(f.whA, f.epA2.ID), nil)
	if code != http.StatusOK || first["webhook_id"] != f.whA || first["target_endpoint_id"] != f.epA2.ID || first["routed"] != true {
		t.Fatalf("add: %d %s, want routed", code, raw)
	}
	if code, _, again := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, webhookRoutePath(f.whA, f.epA2.ID), nil); code != http.StatusOK || again != raw {
		t.Fatalf("repeat add: %d %s, want the same answer %s", code, again, raw)
	}
	rows := f.routeRows(t, f.whA)
	if len(rows) != 1 || rows[0].TargetEndpointID != f.epA2.ID || rows[0].GrantedByHumanID != f.humanA.ID {
		t.Fatalf("route rows after two adds = %+v, want one to %s granted by %s", rows, f.epA2.ID, f.humanA.ID)
	}
	if got := f.targets(t, f.whA, f.epA.ID); !slices.Equal(got, []string{f.epA.ID, f.epA2.ID}) {
		t.Fatalf("targets = %v, want [%s %s]", got, f.epA.ID, f.epA2.ID)
	}

	code, doc, raw = ruleAPICall(t, f.r, f.bearerA, http.MethodGet, webhookRoutesPath(f.whA), nil)
	routes, _ := doc["routes"].([]any)
	if code != http.StatusOK || len(routes) != 1 {
		t.Fatalf("list after add: %d %s, want one route", code, raw)
	}
	route, _ := routes[0].(map[string]any)
	granted, _ := route["granted_at"].(string)
	if route["target_endpoint_id"] != f.epA2.ID {
		t.Fatalf("listed route = %v, want %s", route, f.epA2.ID)
	}
	if _, err := time.Parse(time.RFC3339, granted); err != nil {
		t.Fatalf("granted_at %q is not RFC 3339: %v", granted, err)
	}
	if _, leaks := route["granted_by_human_id"]; leaks {
		t.Fatalf("listed route carries granted_by_human_id: %s", raw)
	}

	// The owner endpoint is an implicit, unremovable target: removing it answers removed and the
	// webhook still delivers to it.
	if code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodDelete, webhookRoutePath(f.whA, f.epA.ID), nil); code != http.StatusOK || doc["removed"] != true {
		t.Fatalf("remove the owner: %d %s, want removed", code, raw)
	}
	if got := f.targets(t, f.whA, f.epA.ID); !slices.Equal(got, []string{f.epA.ID, f.epA2.ID}) {
		t.Fatalf("targets after removing the owner = %v, want both still", got)
	}

	// Remove twice, and a malformed id: every one states an end condition that holds.
	for _, target := range []string{f.epA2.ID, f.epA2.ID, "not-a-uuid"} {
		code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodDelete, webhookRoutePath(f.whA, target), nil)
		if code != http.StatusOK || doc["removed"] != true || doc["target_endpoint_id"] != target {
			t.Fatalf("remove %s: %d %s, want removed", target, code, raw)
		}
	}
	if rows := f.routeRows(t, f.whA); len(rows) != 0 {
		t.Fatalf("route rows after remove = %+v, want none", rows)
	}
	if got := f.targets(t, f.whA, f.epA.ID); !slices.Equal(got, []string{f.epA.ID}) {
		t.Fatalf("targets after remove = %v, want only the owner", got)
	}

	// POST with a body is the same add (#555's spelling), and is just as idempotent.
	for range 2 {
		code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPost, webhookRoutesPath(f.whA), map[string]any{"target_endpoint_id": f.epA2.ID})
		if code != http.StatusOK || doc["routed"] != true || doc["target_endpoint_id"] != f.epA2.ID {
			t.Fatalf("POST add: %d %s, want routed", code, raw)
		}
	}
	if rows := f.routeRows(t, f.whA); len(rows) != 1 {
		t.Fatalf("route rows after two POSTs = %+v, want one", rows)
	}
	for _, body := range []any{map[string]any{}, `{"target_endpoint_id": `} {
		if code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPost, webhookRoutesPath(f.whA), body); code != http.StatusBadRequest || doc["code"] != "invalid_argument" {
			t.Fatalf("POST %v: %d %s, want 400 invalid_argument", body, code, raw)
		}
	}

	// Reach is the human, not one endpoint: A's second endpoint's webhook is A's to route too, which
	// is exactly what the MCP verb cannot do from epA (SPEC-0033 F19 keeps that per endpoint).
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, webhookRoutePath(f.whA2, f.epA.ID), nil); code != http.StatusOK {
		t.Fatalf("add on A's other webhook: %d %s", code, raw)
	}
	if got := f.targets(t, f.whA2, f.epA2.ID); !slices.Equal(got, []string{f.epA2.ID, f.epA.ID}) {
		t.Fatalf("targets of %s = %v, want [%s %s]", f.whA2, got, f.epA2.ID, f.epA.ID)
	}

	// An endpoint credential is not a human, here as on every /api/v1 route.
	if code, _, _ := ruleAPICall(t, f.r, "sbk_"+strings.Repeat("x", 40), http.MethodPut, webhookRoutePath(f.whA, f.epA2.ID), nil); code != http.StatusUnauthorized {
		t.Fatalf("endpoint credential: %d, want 401", code)
	}
}

// SPEC-0035 REQ "Reach on Every Route": another human's webhook, an unknown id and a malformed id are
// the same 404 on every route verb, and the other human's routes do not change.
func TestAPIWebhookRoutesAnotherHumansWebhookIsNotFound(t *testing.T) {
	f := newRuleFixture(t)
	epB2 := consentFixture(t, f.st, f.ctx, f.humanB.ID, "router-b2", "router-b2-44444444", "cid-fixture-routes-b2")
	if err := f.st.AddWebhookRoute(f.ctx, f.whB, epB2.ID, f.humanB.ID); err != nil {
		t.Fatalf("seed B's route: %v", err)
	}

	for _, c := range []struct {
		method, target string
		body           any
		post           bool
	}{
		{method: http.MethodGet},
		{method: http.MethodPut, target: f.epA.ID},
		{method: http.MethodPost, body: map[string]any{"target_endpoint_id": f.epA.ID}, post: true},
		{method: http.MethodDelete, target: epB2.ID},
	} {
		var bodies []string
		for _, id := range []string{f.whB, "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "not-a-uuid"} {
			path := webhookRoutesPath(id)
			if c.target != "" {
				path = webhookRoutePath(id, c.target)
			}
			code, doc, raw := ruleAPICall(t, f.r, f.bearerA, c.method, path, c.body)
			if code != http.StatusNotFound || doc["code"] != "not_found" {
				t.Fatalf("%s on %s: %d %s, want 404 not_found", c.method, id, code, raw)
			}
			bodies = append(bodies, raw)
		}
		if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
			t.Fatalf("%s: another human's webhook is distinguishable: %q", c.method, bodies)
		}
		if strings.Contains(bodies[0], epB2.ID) {
			t.Fatalf("%s leaked B's route: %s", c.method, bodies[0])
		}
	}
	if rows := f.routeRows(t, f.whB); len(rows) != 1 || rows[0].TargetEndpointID != epB2.ID {
		t.Fatalf("B's routes after A's calls = %+v, want only the seed", rows)
	}
}

// A target is authorized exactly as add_webhook_route authorizes it: another human's endpoint only
// across an approved friend edge in the delivering direction, and every refusal (no edge, a pending
// or opposite edge, an unknown, malformed or revoked endpoint) is the same 403.
func TestAPIWebhookRoutesTargetAuthorization(t *testing.T) {
	f := newRuleFixture(t)
	add := func(target string) (int, map[string]any, string) {
		return ruleAPICall(t, f.r, f.bearerA, http.MethodPut, webhookRoutePath(f.whA, target), nil)
	}
	refused := func(what, target string) string {
		t.Helper()
		code, doc, raw := add(target)
		if code != http.StatusForbidden || doc["code"] != "forbidden" {
			t.Fatalf("%s: %d %s, want 403 forbidden", what, code, raw)
		}
		if rows := f.routeRows(t, f.whA); len(rows) != 0 {
			t.Fatalf("%s left a route row: %+v", what, rows)
		}
		return raw
	}

	// SPEC-0035 scenario "Routing to a stranger's endpoint".
	stranger := refused("a stranger's endpoint", f.epB.ID)
	for what, target := range map[string]string{
		"an unknown id":  "6ba7b811-9dad-11d1-80b4-00c04fd430c8",
		"a malformed id": "not-a-uuid",
	} {
		if raw := refused(what, target); raw != stranger {
			t.Fatalf("%s answers %s, the stranger %s: the route is an existence oracle", what, raw, stranger)
		}
	}

	// B asking to hand work to A is the opposite direction, and authorizes nothing for A.
	reverse, err := f.st.CreateFriendRequest(f.ctx, store.CreateFriendRequestParams{
		FromPersona: "b@remote", ToPersona: "a@local", FromHuman: f.humanB.ID, ToHuman: f.humanA.ID,
		RequestedQueues: []string{"github"}, RequestedVerbs: []string{"create_for"},
	})
	if err != nil {
		t.Fatalf("reverse friend request: %v", err)
	}
	approveEdge(t, f, reverse.ID, f.humanA.ID, f.epA.AgentID, "friend-b-55555555")
	if raw := refused("an opposite-direction edge", f.epB.ID); raw != stranger {
		t.Fatalf("opposite edge answers %s, want %s", raw, stranger)
	}

	// A asks B: pending grants nothing; approved does.
	edge, err := f.st.CreateFriendRequest(f.ctx, store.CreateFriendRequestParams{
		FromPersona: "a@local", ToPersona: "b@remote", FromHuman: f.humanA.ID, ToHuman: f.humanB.ID,
		RequestedQueues: []string{"github"}, RequestedVerbs: []string{"create_for"},
	})
	if err != nil {
		t.Fatalf("friend request: %v", err)
	}
	if raw := refused("a pending edge", f.epB.ID); raw != stranger {
		t.Fatalf("pending edge answers %s, want %s", raw, stranger)
	}
	approveEdge(t, f, edge.ID, f.humanB.ID, f.epB.AgentID, "friend-a-66666666")
	if code, doc, raw := add(f.epB.ID); code != http.StatusOK || doc["routed"] != true {
		t.Fatalf("approved edge: %d %s, want routed", code, raw)
	}
	if got := f.targets(t, f.whA, f.epA.ID); !slices.Equal(got, []string{f.epA.ID, f.epB.ID}) {
		t.Fatalf("targets = %v, want [%s %s]", got, f.epA.ID, f.epB.ID)
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodDelete, webhookRoutePath(f.whA, f.epB.ID), nil); code != http.StatusOK {
		t.Fatalf("remove the friend route: %d %s", code, raw)
	}

	// A revoked endpoint, even the human's own, is not a target, and says so in the same words.
	if err := f.st.RevokeEndpoint(f.ctx, f.epA2.ID, f.humanA.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if raw := refused("a revoked endpoint", f.epA2.ID); raw != stranger {
		t.Fatalf("revoked target answers %s, want %s", raw, stranger)
	}
}

// approveEdge approves a pending friend edge as its target human, vending the friend endpoint on
// agentID, one of that human's agents (approval is the vend, ADR-0008).
func approveEdge(t *testing.T, f *ruleFixture, edgeID, approverHumanID, agentID, slug string) {
	t.Helper()
	_, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, _, err := f.st.ApproveFriendRequest(f.ctx, store.ApproveFriendRequestParams{
		EdgeID: edgeID, OwnerHumanID: approverHumanID, AgentID: agentID,
		CredentialHash: hash, CredentialPrefix: prefix, Slug: slug,
	}); err != nil {
		t.Fatalf("approve friend edge: %v", err)
	}
}

// A webhook whose owning endpoint is revoked gains no route (409 naming the state), but its routes
// stay listable and removable: a revoked endpoint's routes still deliver, and withdrawing delivery is
// always safe.
func TestAPIWebhookRoutesRevokedOwnerEndpoint(t *testing.T) {
	f := newRuleFixture(t)
	if err := f.st.AddWebhookRoute(f.ctx, f.whA2, f.epA.ID, f.humanA.ID); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	if err := f.st.RevokeEndpoint(f.ctx, f.epA2.ID, f.humanA.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	epA3 := consentFixture(t, f.st, f.ctx, f.humanA.ID, "router-a3", "router-a3-77777777", "cid-fixture-routes-a3")

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, webhookRoutePath(f.whA2, epA3.ID), nil},
		{http.MethodPost, webhookRoutesPath(f.whA2), map[string]any{"target_endpoint_id": epA3.ID}},
	} {
		code, doc, raw := ruleAPICall(t, f.r, f.bearerA, c.method, c.path, c.body)
		if code != http.StatusConflict || doc["code"] != "conflict" || doc["state"] != "revoked" {
			t.Fatalf("%s on a revoked endpoint's webhook: %d %s, want 409 conflict with state revoked", c.method, code, raw)
		}
	}
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, webhookRoutesPath(f.whA2), nil)
	if routes, _ := doc["routes"].([]any); code != http.StatusOK || len(routes) != 1 {
		t.Fatalf("list on a revoked endpoint's webhook: %d %s, want the seeded route", code, raw)
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodDelete, webhookRoutePath(f.whA2, f.epA.ID), nil); code != http.StatusOK {
		t.Fatalf("remove on a revoked endpoint's webhook: %d %s", code, raw)
	}
	if rows := f.routeRows(t, f.whA2); len(rows) != 0 {
		t.Fatalf("routes after remove = %+v, want none", rows)
	}
}

// The case #555 exists for: a rule naming an endpoint outside the grant is refused until the route
// exists, and the same save passes once `route add` has put the endpoint in grant.endpoints.
func TestAPIWebhookRulesNamingAnEndpointNeedTheRouteFirst(t *testing.T) {
	f := newRuleFixture(t)
	body := map[string]any{"rules": []any{map[string]any{"id": "lane-s", "expr": `.kind == "issues"`,
		"action": map[string]any{"queue": "github", "endpoints": []any{f.epA2.ID}, "exclusive": true}}}}
	before := f.storedConfig(t, f.whA)

	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), body)
	if code != http.StatusForbidden || doc["code"] != "forbidden" {
		t.Fatalf("rules naming an unrouted endpoint: %d %s, want 403 forbidden", code, raw)
	}
	if after := f.storedConfig(t, f.whA); len(after.Rules) != len(before.Rules) {
		t.Fatalf("a refused save changed the rules: %+v", after)
	}

	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, webhookRoutePath(f.whA, f.epA2.ID), nil); code != http.StatusOK {
		t.Fatalf("route add: %d %s", code, raw)
	}
	_, got, _ := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, rulesPath(f.whA), nil)
	grant, _ := got["grant"].(map[string]any)
	if endpoints, _ := grant["endpoints"].([]any); !slices.Contains(endpoints, any(f.epA2.ID)) {
		t.Fatalf("grant after route add = %v, want %s in endpoints", grant, f.epA2.ID)
	}
	if code, _, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, rulesPath(f.whA), body); code != http.StatusOK {
		t.Fatalf("the same rules after route add: %d %s, want saved", code, raw)
	}
	if cfg := f.storedConfig(t, f.whA); len(cfg.Rules) != 1 || !slices.Equal(cfg.Rules[0].Action.Endpoints, []string{f.epA2.ID}) {
		t.Fatalf("stored rules = %+v, want lane-s narrowed to %s", cfg.Rules, f.epA2.ID)
	}
}
