package store

// Friend edges act with their own authority (#420): a friend grant is bounded to create_for and the
// drain verbs however it was requested or approved (F3), and live-edge uniqueness is per pair of
// humans, not instance-wide (F13). Governing: ADR-0038, SPEC-0033 REQ "Closing the Audited
// Surfaces".

import (
	"errors"
	"slices"
	"testing"
)

// F3 at the store: the stored request drops non-grantable verbs; a pending edge recorded before the
// fix (still requesting set_webhook_rules) approves by default to its grantable subset only, and an
// explicit grant naming set_webhook_rules is refused and mints nothing.
func TestFriendGrantIsBoundedToCreateForAndDrain(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|f3-tgt", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "f3-persona")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "x@x", ToPersona: "t@t", ToHuman: target.ID, RequestedQueues: []string{"q"},
		RequestedVerbs: []string{"set_webhook_rules", "create_for", "replay_webhook_event", "complete"},
	})
	if !slices.Equal(e.RequestedVerbs, []string{"create_for", "complete"}) {
		t.Fatalf("stored requested verbs = %v, want [create_for complete]", e.RequestedVerbs)
	}

	// A legacy pending edge, as written before this fix.
	var legacy string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO friend_edges (from_persona, to_persona, to_human, requested_queues, requested_verbs)
		VALUES ('legacy@x', 't@t', $1, '{q}', '{create_for,set_webhook_rules,list_webhook_events}')
		RETURNING id::text`, target.ID).Scan(&legacy); err != nil {
		t.Fatalf("seed legacy edge: %v", err)
	}
	slug, _ := MintSlug(vendAgent.Name)
	if _, _, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: legacy, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		GrantedQueues: []string{"q"}, GrantedVerbs: []string{"create_for", "set_webhook_rules"},
		CredentialHash: "f3-hash-1", CredentialPrefix: "sbk_f31", Slug: slug,
	}); !errors.Is(err, ErrScopeExceedsRequest) {
		t.Fatalf("explicit grant of set_webhook_rules = %v, want ErrScopeExceedsRequest", err)
	}
	_, ep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: legacy, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: "f3-hash-2", CredentialPrefix: "sbk_f32", Slug: slug,
	})
	if err != nil {
		t.Fatalf("default approve of legacy edge: %v", err)
	}
	if !slices.Equal(ep.ScopeVerbs, []string{"create_for"}) {
		t.Fatalf("legacy edge minted verbs %v, want only [create_for]", ep.ScopeVerbs)
	}
}

// Scenario "Friend requests from different tenants do not collide (F13)": two humans each send from
// a persona named "reviewer" to the same target persona; both requests exist. The same human's
// second live request still collides (the anti-flood invariant).
func TestFriendRequestsFromDifferentTenantsDoNotCollide(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|f13-tgt", "Target")
	one := mustHuman(t, s, ctx, "pocket|f13-one", "One")
	two := mustHuman(t, s, ctx, "pocket|f13-two", "Two")
	req := func(from string) CreateFriendRequestParams {
		return CreateFriendRequestParams{FromPersona: "reviewer", ToPersona: "maintainer",
			FromHuman: from, ToHuman: target.ID, RequestedVerbs: []string{"create_for"}}
	}
	a, err := s.CreateFriendRequest(ctx, req(one.ID))
	if err != nil {
		t.Fatalf("first tenant: %v", err)
	}
	b, err := s.CreateFriendRequest(ctx, req(two.ID))
	if err != nil {
		t.Fatalf("second tenant's same-named persona collided with the first: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two tenants share one edge %s", a.ID)
	}
	if _, err := s.CreateFriendRequest(ctx, req(one.ID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same human's second live request = %v, want ErrConflict", err)
	}
}
