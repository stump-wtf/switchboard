package mcp

// Friend-vended endpoints act with their own authority (#420), over the real SDK client and the REAL
// store and PostgreSQL. A friend endpoint is vended on the approver's agent, so before this fix it
// could carry webhook, rule and event verbs and exercise them with the approver's authority, and any
// endpoint of a human could configure every one of that human's webhooks. Skips cleanly without
// SWITCHBOARD_TEST_DATABASE_URL, in the house style.
//
// Governing: ADR-0038, SPEC-0033 REQ "Closing the Audited Surfaces" — scenarios "Friend grant cannot
// carry webhook verbs (F3)" and "Rule verbs stay on their own webhook (F3, F19)".

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Scenario "Friend grant cannot carry webhook verbs (F3)": a request asking for set_webhook_rules
// (and an event verb) is approved as-is; the minted endpoint carries only create_for and drain
// verbs, and set_webhook_rules on it is a scope violation.
func TestFriendGrantCannotCarryWebhookVerbs(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)

	edge, err := f.st.CreateFriendRequest(ctx, store.CreateFriendRequestParams{
		FromPersona: "b@remote", ToPersona: "a@local", FromHuman: f.humanB, ToHuman: f.humanA,
		RequestedQueues: []string{"reviews"},
		RequestedVerbs:  []string{"create_for", "set_webhook_rules", "test_webhook_rules", "list_webhook_events", "claim"},
	})
	if err != nil {
		t.Fatalf("friend request: %v", err)
	}
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	const slug = "friend-b-12121212"
	_, ep, err := f.st.ApproveFriendRequest(ctx, store.ApproveFriendRequestParams{
		EdgeID: edge.ID, OwnerHumanID: f.humanA, AgentID: f.agentA1,
		CredentialHash: hash, CredentialPrefix: prefix, Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !slices.Equal(ep.ScopeVerbs, []string{"create_for", "claim"}) {
		t.Fatalf("friend endpoint verbs = %v, want only [create_for claim]", ep.ScopeVerbs)
	}

	cs := routeSession(t, ctx, f.st, slug, token)
	callErr(t, ctx, cs, "set_webhook_rules", map[string]any{"webhook_id": f.webhookA, "rules": []any{}}, codeForbidden)
	callErr(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA, "payload": map[string]any{}}, codeForbidden)
	callErr(t, ctx, cs, "list_webhook_events", map[string]any{}, codeForbidden)
	wr, err := f.st.WebhookRoutingByID(ctx, f.webhookA)
	if err != nil || len(wr.Config.Rules) != 0 {
		t.Fatalf("approver's webhook after the friend's calls = %+v (%v), want untouched", wr.Config, err)
	}
}

// Scenario "Rule verbs stay on their own webhook (F3, F19)": human H owns E1 (rule verbs, webhook W1)
// and E2 (webhook W2); E1's rule and route verbs on W2 answer not_found, exactly like an unknown id.
func TestRuleAndRouteVerbsStayOnTheirOwnWebhook(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	csE1, _ := open("A") // epA1, the owner of webhookA
	w2 := mustWebhook(t, ctx, f.st, f.epA2, "tok-a2-own")

	for tool, args := range map[string]map[string]any{
		"list_webhook_rules":  {},
		"set_webhook_rules":   {"rules": []any{}},
		"add_webhook_rule":    {"expr": "true", "action": map[string]any{"drop": true}},
		"test_webhook_rules":  {"payload": map[string]any{}},
		"list_webhook_routes": {},
		"add_webhook_route":   {"target_endpoint_id": f.epA1},
	} {
		args["webhook_id"] = w2
		sibling := callErr(t, ctx, csE1, tool, args, codeNotFound)
		args["webhook_id"] = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		unknown := callErr(t, ctx, csE1, tool, args, codeNotFound)
		if sibling != unknown {
			t.Fatalf("%s leaks the sibling webhook: %q vs unknown %q", tool, sibling, unknown)
		}
	}
	wr, err := f.st.WebhookRoutingByID(ctx, w2)
	if err != nil || len(wr.Config.Rules) != 0 {
		t.Fatalf("W2 after E1's calls = %+v (%v), want untouched", wr.Config, err)
	}
	if routes, err := f.st.ListWebhookRoutes(ctx, w2); err != nil || len(routes) != 0 {
		t.Fatalf("W2 routes after E1's calls = %v (%v), want none", routes, err)
	}

	// E1 still drives its own webhook: the positive control.
	var out webhookRulesOut
	callOK(t, ctx, csE1, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, &out)
}

// The store's friend-grantable set is create_for plus exactly the drain verbs this package serves,
// so a drain verb added here cannot silently become ungrantable to friends, and nothing else can
// silently become grantable.
func TestFriendGrantableVerbsAreCreateForAndDrain(t *testing.T) {
	want := append([]string{"create_for"}, DrainVerbs()...)
	if got := store.FriendGrantableVerbs(); !slices.Equal(got, want) {
		t.Fatalf("store.FriendGrantableVerbs() = %v, want %v", got, want)
	}
}
