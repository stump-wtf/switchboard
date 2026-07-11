package store

import (
	"context"
	"errors"
	"testing"
)

// mustFriendRequest seeds a pending friend edge targeting toHuman.
func mustFriendRequest(t *testing.T, s *Store, ctx context.Context, p CreateFriendRequestParams) FriendEdge {
	t.Helper()
	e, err := s.CreateFriendRequest(ctx, p)
	if err != nil {
		t.Fatalf("create friend request: %v", err)
	}
	return e
}

// A friend request creates a PENDING edge that grants nothing: no endpoint is minted and no
// credential exists while the edge is pending. Approval is the sole vend.
// Governing: SPEC-0010 REQ "Friend-Request Lifecycle" (scenario "Pending edge grants nothing").
func TestFriendRequestPendingGrantsNothing(t *testing.T) {
	s, ctx := testStore(t)
	requester := mustHuman(t, s, ctx, "pocket|req", "Requester")
	target := mustHuman(t, s, ctx, "pocket|tgt", "Target")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedQueues: []string{"reviews"}, RequestedVerbs: []string{"create_for"},
		Reason: "hand you PR reviews", ProvenanceVerified: true,
	})
	if e.State != "pending" {
		t.Fatalf("new edge state=%s want pending", e.State)
	}
	if e.Direction != "outbound" {
		t.Fatalf("default direction=%q want outbound", e.Direction)
	}
	if e.EndpointID != "" {
		t.Fatalf("pending edge must mint no endpoint, got endpoint_id=%q", e.EndpointID)
	}
	if !e.ProvenanceVerified || e.FromHuman != requester.ID {
		t.Fatalf("edge lost provenance context: %+v", e)
	}
	// No endpoint row exists anywhere for this pending edge.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM endpoints`).Scan(&n); err != nil {
		t.Fatalf("count endpoints: %v", err)
	}
	if n != 0 {
		t.Fatalf("pending edge must persist zero endpoints, got %d", n)
	}

	// A second LIVE request for the same directional pair collides (anti-flood invariant).
	if _, err := s.CreateFriendRequest(ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", ToHuman: target.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate live request must be ErrConflict, got %v", err)
	}
}

// Approval is the vend: it transitions pending → approved and mints a scoped endpoint in one step.
// Denial is terminal and mints nothing. With no narrowing the minted scope equals the requested.
// Governing: SPEC-0010 REQ "Friend-Request Lifecycle", REQ "Approval Is the Vend, Narrow-Only"
// (scenarios "Approval mints the endpoint", "Denial is terminal", "no narrowing grants requested").
func TestFriendApprovalMintsEndpoint(t *testing.T) {
	s, ctx := testStore(t)
	requester := mustHuman(t, s, ctx, "pocket|req2", "Requester")
	target := mustHuman(t, s, ctx, "pocket|tgt2", "Target")
	// The vended agent is a local record owned by the target human (B vends into its own surface).
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedQueues: []string{"reviews", "triage"}, RequestedVerbs: []string{"create_for"},
	})

	slug, _ := MintSlug(vendAgent.Name)
	approved, ep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: "friendhash-1", CredentialPrefix: "sbk_fr1", Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.State != "approved" || approved.EndpointID != ep.ID || approved.DecidedAt == nil {
		t.Fatalf("approved edge wrong: %+v", approved)
	}
	if approved.FromAgentID != vendAgent.ID {
		t.Fatalf("approval must bind the vended agent, got %q", approved.FromAgentID)
	}
	// No narrowing → minted scope equals the requested scope.
	if !isSubset(ep.ScopeQueues, []string{"reviews", "triage"}) || len(ep.ScopeQueues) != 2 {
		t.Fatalf("minted queues=%v want the requested two", ep.ScopeQueues)
	}
	// The minted credential resolves to the vending human's surface.
	auth, err := s.EndpointByCredHash(ctx, "friendhash-1")
	if err != nil || auth.OwnerHumanID != target.ID {
		t.Fatalf("minted credential must resolve to target: %+v, %v", auth, err)
	}

	// Re-approving an already-approved edge is not a legal transition.
	if _, _, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: "friendhash-x", CredentialPrefix: "sbk_frx", Slug: slug + "-x",
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("re-approve must be ErrInvalidTransition, got %v", err)
	}

	// Denial is terminal on a fresh pending edge (its own directional pair) and mints nothing.
	deny := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "c@c", ToPersona: "b@b", ToHuman: target.ID, RequestedVerbs: []string{"create_for"},
	})
	denied, err := s.DenyFriendRequest(ctx, deny.ID, target.ID)
	if err != nil || denied.State != "denied" || denied.EndpointID != "" || denied.DecidedAt == nil {
		t.Fatalf("deny wrong: %+v, %v", denied, err)
	}
	if _, err := s.DenyFriendRequest(ctx, deny.ID, target.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("re-deny must be ErrInvalidTransition, got %v", err)
	}
}

// Narrow-only: the human may hand back LESS than requested (subset), but never MORE. A granted
// scope that is not a subset of the requested scope is rejected before anything is minted.
// Governing: SPEC-0010 REQ "Approval Is the Vend, Narrow-Only" (scenario "Granted cannot exceed requested").
func TestFriendApprovalNarrowOnly(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|tgt3", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", ToHuman: target.ID,
		RequestedQueues: []string{"reviews", "triage"}, RequestedVerbs: []string{"create_for", "list_todos"},
	})

	// Widening (a queue that was never requested) is rejected; nothing is minted.
	slug, _ := MintSlug(vendAgent.Name)
	if _, _, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		GrantedQueues: []string{"reviews", "secrets"}, GrantedVerbs: []string{"create_for"},
		CredentialHash: "narrowhash-1", CredentialPrefix: "sbk_nr1", Slug: slug,
	}); !errors.Is(err, ErrScopeExceedsRequest) {
		t.Fatalf("widening must be ErrScopeExceedsRequest, got %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM endpoints`).Scan(&n); err != nil {
		t.Fatalf("count endpoints: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected approval must mint nothing, got %d endpoints", n)
	}
	// The edge is still pending — a rejected approval is not a transition.
	edges, _ := s.ListFriendEdges(ctx, target.ID, "pending")
	if len(edges) != 1 {
		t.Fatalf("edge must remain pending after rejected approval, got %d pending", len(edges))
	}

	// A true subset narrows the grant: the minted endpoint carries only the narrowed scope.
	approved, ep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		GrantedQueues: []string{"reviews"}, GrantedVerbs: []string{"create_for"},
		CredentialHash: "narrowhash-2", CredentialPrefix: "sbk_nr2", Slug: slug,
	})
	if err != nil {
		t.Fatalf("subset approve: %v", err)
	}
	if len(approved.GrantedQueues) != 1 || approved.GrantedQueues[0] != "reviews" {
		t.Fatalf("granted queues=%v want [reviews]", approved.GrantedQueues)
	}
	if len(ep.ScopeQueues) != 1 || ep.ScopeQueues[0] != "reviews" || len(ep.ScopeVerbs) != 1 {
		t.Fatalf("minted endpoint carried un-narrowed scope: %v / %v", ep.ScopeQueues, ep.ScopeVerbs)
	}
}

// Edges are per-direction and non-transitive; revocation is instant and one-sided. Revoking A→B
// kills that endpoint's credential while the separate B→A edge and its endpoint remain fully live.
// Governing: SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges"
// (scenarios "Approval is one-directional", "Revocation is one-sided").
func TestFriendRevokeIsOneSided(t *testing.T) {
	s, ctx := testStore(t)
	alice := mustHuman(t, s, ctx, "pocket|alice-f", "Alice")
	bob := mustHuman(t, s, ctx, "pocket|bob-f", "Bob")
	aAgent := mustAgent(t, s, ctx, alice.ID, "a-persona")
	bAgent := mustAgent(t, s, ctx, bob.ID, "b-persona")

	// A→B: Alice's agent requests to hand Bob work; Bob (target) approves and vends into his surface.
	ab := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", ToHuman: bob.ID, RequestedVerbs: []string{"create_for"},
	})
	slugB, _ := MintSlug(bAgent.Name)
	_, epAB, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: ab.ID, OwnerHumanID: bob.ID, AgentID: bAgent.ID,
		CredentialHash: "ab-hash", CredentialPrefix: "sbk_ab", Slug: slugB,
	})
	if err != nil {
		t.Fatalf("approve A->B: %v", err)
	}

	// B→A is a SEPARATE grant requiring its own request+approval — approving A→B granted nothing here.
	ba := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "b@b", ToPersona: "a@a", ToHuman: alice.ID, RequestedVerbs: []string{"create_for"},
	})
	slugA, _ := MintSlug(aAgent.Name)
	_, epBA, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: ba.ID, OwnerHumanID: alice.ID, AgentID: aAgent.ID,
		CredentialHash: "ba-hash", CredentialPrefix: "sbk_ba", Slug: slugA,
	})
	if err != nil {
		t.Fatalf("approve B->A: %v", err)
	}

	// Revoke only A→B: its credential stops resolving, the reverse edge + endpoint stay live.
	revoked, err := s.RevokeFriendEdge(ctx, ab.ID, bob.ID)
	if err != nil || revoked.State != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("revoke A->B wrong: %+v, %v", revoked, err)
	}
	if _, err := s.EndpointByCredHash(ctx, "ab-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked A->B credential must not resolve, got %v", err)
	}
	// The other direction is untouched.
	authBA, err := s.EndpointByCredHash(ctx, "ba-hash")
	if err != nil || authBA.OwnerHumanID != alice.ID {
		t.Fatalf("B->A endpoint must remain live after A->B revoke: %+v, %v", authBA, err)
	}
	baEdges, _ := s.ListFriendEdges(ctx, alice.ID, "approved")
	if len(baEdges) != 1 || baEdges[0].ID != ba.ID {
		t.Fatalf("B->A edge must remain approved, got %+v", baEdges)
	}

	// Revoking a non-approved edge is not a legal transition; revoking again is too.
	if _, err := s.RevokeFriendEdge(ctx, ab.ID, bob.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("re-revoke must be ErrInvalidTransition, got %v", err)
	}
	// Endpoint ids differ across directions (no shared grant).
	if epAB.ID == epBA.ID {
		t.Fatal("per-direction grants must mint distinct endpoints")
	}
}

// Ownership isolation: a human may only decide their OWN inbound edges. A cross-owner approve/deny/
// revoke is ErrNotFound — byte-for-byte the same as a missing id, so it never leaks existence — and
// mutates nothing. Listing is tenant-scoped and does not traverse to another owner's edges.
// Governing: SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges", ADR-0008 (ownership guard).
func TestFriendEdgeOwnershipIsolation(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|owner-f", "Owner")
	mallory := mustHuman(t, s, ctx, "pocket|mallory-f", "Mallory")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", ToHuman: target.ID, RequestedVerbs: []string{"create_for"},
	})

	slug, _ := MintSlug(vendAgent.Name)
	// A non-owner cannot approve, deny, or revoke — all ErrNotFound.
	if _, _, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: mallory.ID, AgentID: vendAgent.ID,
		CredentialHash: "iso-hash", CredentialPrefix: "sbk_iso", Slug: slug,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner approve must be ErrNotFound, got %v", err)
	}
	if _, err := s.DenyFriendRequest(ctx, e.ID, mallory.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner deny must be ErrNotFound, got %v", err)
	}
	if _, err := s.RevokeFriendEdge(ctx, e.ID, mallory.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner revoke must be ErrNotFound, got %v", err)
	}
	// A missing id is likewise ErrNotFound, indistinguishable from the cross-owner miss.
	if _, err := s.DenyFriendRequest(ctx, "00000000-0000-0000-0000-000000000000", target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id must be ErrNotFound, got %v", err)
	}
	// The rejected calls mutated nothing: the edge is still pending, no endpoint exists.
	if got, _ := s.ListFriendEdges(ctx, target.ID, "pending"); len(got) != 1 {
		t.Fatalf("rejected decisions must not change state, got %d pending", len(got))
	}
	if got, _ := s.ListFriendEdges(ctx, mallory.ID); len(got) != 0 {
		t.Fatalf("non-owner must see no edges, got %d", len(got))
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM endpoints`).Scan(&n); err != nil {
		t.Fatalf("count endpoints: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected approvals must mint nothing, got %d endpoints", n)
	}

	// The true owner's listing filters by state and returns their own edge.
	all, err := s.ListFriendEdges(ctx, target.ID)
	if err != nil || len(all) != 1 || all[0].ID != e.ID {
		t.Fatalf("owner list wrong: %+v, %v", all, err)
	}
}
