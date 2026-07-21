package store

import (
	"context"
	"errors"
	"testing"
	"time"
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

// TestFriendApprovalRejectsOwnOutgoingEdge pins the both-operators-approve doctrine (SPEC-0010):
// a locally sent (direction=outgoing) pending edge awaits the REMOTE operator, so its own owner
// cannot approve it — the transition is refused in the store (not just hidden in the web confirm
// page) and nothing is minted; the edge stays pending/withdrawable.
func TestFriendApprovalRejectsOwnOutgoingEdge(t *testing.T) {
	s, ctx := testStore(t)
	sender := mustHuman(t, s, ctx, "pocket|selfappr", "Sender")
	agent := mustAgent(t, s, ctx, sender.ID, "sender-agent")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "sender-agent", ToPersona: "zed@far.example", Direction: "outgoing",
		ToHuman: sender.ID, RequestedVerbs: []string{"create_for"},
	})

	slug, _ := MintSlug(agent.Name)
	if _, _, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: sender.ID, AgentID: agent.ID,
		CredentialHash: "selfapprhash-1", CredentialPrefix: "sbk_sa1", Slug: slug,
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("self-approving an outgoing edge must be ErrInvalidTransition, got %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM endpoints`).Scan(&n); err != nil {
		t.Fatalf("count endpoints: %v", err)
	}
	if n != 0 {
		t.Fatalf("refused self-approval must mint nothing, got %d endpoints", n)
	}
	edges, err := s.ListFriendEdges(ctx, sender.ID, "pending")
	if err != nil || len(edges) != 1 || edges[0].ID != e.ID {
		t.Fatalf("edge must stay pending: %+v, %v", edges, err)
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

// CountLiveFriendRequestsFrom counts only a requester's LIVE (pending/approved) edges — the
// per-requester anti-flood quota counter the A2A intake checks. Denied/revoked edges are terminal
// and drop out of the count, so a requester whose past asks were resolved is never blocked.
// Governing: SPEC-0010 REQ "Anti-Spam — Bounded Discovery and Quotas".
func TestCountLiveFriendRequestsFrom(t *testing.T) {
	s, ctx := testStore(t)
	requester := mustHuman(t, s, ctx, "pocket|quota-req", "Requester")
	target := mustHuman(t, s, ctx, "pocket|quota-tgt", "Target")

	if n, err := s.CountLiveFriendRequestsFrom(ctx, requester.ID); err != nil || n != 0 {
		t.Fatalf("fresh requester count = %d (%v), want 0", n, err)
	}

	// Two pending edges from this requester to distinct target personas.
	mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "req@r", ToPersona: "one@t", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedVerbs: []string{"create_for"}, ProvenanceVerified: true,
	})
	denied := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "req@r", ToPersona: "two@t", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedVerbs: []string{"create_for"}, ProvenanceVerified: true,
	})
	if n, err := s.CountLiveFriendRequestsFrom(ctx, requester.ID); err != nil || n != 2 {
		t.Fatalf("two live edges count = %d (%v), want 2", n, err)
	}

	// Denying one drops it from the live count (terminal state).
	if _, err := s.DenyFriendRequest(ctx, denied.ID, target.ID); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if n, err := s.CountLiveFriendRequestsFrom(ctx, requester.ID); err != nil || n != 1 {
		t.Fatalf("after deny count = %d (%v), want 1", n, err)
	}

	// A different requester with no edges counts zero (per-requester scoping).
	if n, err := s.CountLiveFriendRequestsFrom(ctx, target.ID); err != nil || n != 0 {
		t.Fatalf("unrelated requester count = %d (%v), want 0", n, err)
	}
}

// mustApprovedFriend seeds a pending edge and approves it, returning the edge and its vended
// endpoint. The requesting persona is "a@a"; the grant is the given queues/verbs (no narrowing).
func mustApprovedFriend(t *testing.T, s *Store, ctx context.Context, target Human, vendAgent Agent, queues, verbs []string) (FriendEdge, Endpoint) {
	t.Helper()
	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", ToHuman: target.ID,
		RequestedQueues: queues, RequestedVerbs: verbs,
	})
	slug, _ := MintSlug(vendAgent.Name)
	edge, ep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: "cff-" + vendAgent.ID, CredentialPrefix: "sbk_cff", Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve friend: %v", err)
	}
	return edge, ep
}

// After a grant, cross-agent work lands as a DURABLE TODO in the granted queue via the create_for
// verb backend — not as an ephemeral A2A task. The todo is attributed to the requesting persona,
// carries the invoked intent as its kind, and dedups on the idempotency key. Governing: SPEC-0010
// REQ "Work Flows as Todos, Not A2A Tasks", ADR-0010, ADR-0007.
func TestCreateForFriendLandsAsTodo(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|cff-tgt", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")
	_, ep := mustApprovedFriend(t, s, ctx, target, vendAgent,
		[]string{"reviews", "triage"}, []string{"create_for"})

	td, created, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: ep.ID, Queue: "reviews", Intent: "create_for",
		Title: "Review PR #42", Payload: []byte(`{"pr":42}`), IdempotencyKey: "handoff-1",
	})
	if err != nil {
		t.Fatalf("create_for friend: %v", err)
	}
	if !created {
		t.Fatal("first handoff must create a new todo")
	}
	if td.Queue != "reviews" {
		t.Fatalf("todo queue=%q want the granted queue reviews", td.Queue)
	}
	if td.Source != "a@a" {
		t.Fatalf("todo must be attributed to the requesting persona, got source=%q", td.Source)
	}
	if td.Kind != "create_for" {
		t.Fatalf("todo kind=%q want the invoked intent", td.Kind)
	}
	if td.State != "pending" {
		t.Fatalf("handoff todo must be claimable/pending, got %q", td.State)
	}

	// Same idempotency key → the existing todo, not a duplicate (the handoff is dedup'd).
	td2, created2, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: ep.ID, Queue: "reviews", Intent: "create_for",
		Title: "Review PR #42", IdempotencyKey: "handoff-1",
	})
	if err != nil || created2 {
		t.Fatalf("duplicate handoff must dedup: created=%v err=%v", created2, err)
	}
	if td2.ID != td.ID {
		t.Fatalf("dedup must return the same todo, got %q want %q", td2.ID, td.ID)
	}
}

// The negotiated scope is enforced at the boundary: an intent (verb) outside the granted set, or a
// queue outside the granted set, is rejected as ErrIntentNotNegotiated and mints no todo — a friend
// may do exactly what was approved. Governing: SPEC-0010 REQ "Work Flows as Todos, Not A2A Tasks".
func TestCreateForFriendRejectsUnnegotiatedIntent(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|cff-scope", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")
	_, ep := mustApprovedFriend(t, s, ctx, target, vendAgent,
		[]string{"reviews"}, []string{"create_for"})

	// Intent (verb) not in the granted set → rejected.
	if _, _, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: ep.ID, Queue: "reviews", Intent: "delete_todo", IdempotencyKey: "bad-verb",
	}); !errors.Is(err, ErrIntentNotNegotiated) {
		t.Fatalf("un-negotiated intent must be ErrIntentNotNegotiated, got %v", err)
	}
	// Queue not in the granted set → rejected (queue scope is enforced too).
	if _, _, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: ep.ID, Queue: "secrets", Intent: "create_for", IdempotencyKey: "bad-queue",
	}); !errors.Is(err, ErrIntentNotNegotiated) {
		t.Fatalf("out-of-scope queue must be ErrIntentNotNegotiated, got %v", err)
	}
	// Nothing was minted for either rejected handoff.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos`).Scan(&n); err != nil {
		t.Fatalf("count todos: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected handoffs must mint no todo, got %d", n)
	}
}

// An inactive friendship grants no transport: a revoked edge (endpoint killed) and a pending edge
// (no endpoint yet) both reject the handoff as ErrFriendshipInactive, minting nothing. Governing:
// SPEC-0010 REQ "Work Flows as Todos, Not A2A Tasks", REQ "Per-Direction, Revocable" (revoke kills it).
func TestCreateForFriendRejectsInactiveFriendship(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|cff-inact", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")
	edge, ep := mustApprovedFriend(t, s, ctx, target, vendAgent,
		[]string{"reviews"}, []string{"create_for"})

	// Revoke the friendship (kills the vended endpoint), then a handoff is refused.
	if _, err := s.RevokeFriendEdge(ctx, edge.ID, target.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: ep.ID, Queue: "reviews", Intent: "create_for", IdempotencyKey: "post-revoke",
	}); !errors.Is(err, ErrFriendshipInactive) {
		t.Fatalf("handoff on a revoked friendship must be ErrFriendshipInactive, got %v", err)
	}

	// A pending edge has no endpoint at all; an unknown/unbacked endpoint id is likewise inactive.
	if _, _, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: "00000000-0000-0000-0000-000000000000", Queue: "reviews", Intent: "create_for",
	}); !errors.Is(err, ErrFriendshipInactive) {
		t.Fatalf("handoff on an unbacked endpoint must be ErrFriendshipInactive, got %v", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos`).Scan(&n); err != nil {
		t.Fatalf("count todos: %v", err)
	}
	if n != 0 {
		t.Fatalf("inactive-friendship handoffs must mint no todo, got %d", n)
	}
}

// approvedFriendFrom seeds and approves a friendship with caller-chosen personas so a single target
// human can hold several distinct friendships at once (the live partial unique index forbids two
// live edges sharing (from_persona, to_persona, direction), so each friend needs its own personas).
func approvedFriendFrom(t *testing.T, s *Store, ctx context.Context, target Human, vendAgent Agent,
	fromPersona, toPersona, credHash string, queues, verbs []string) Endpoint {
	t.Helper()
	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: fromPersona, ToPersona: toPersona, ToHuman: target.ID,
		RequestedQueues: queues, RequestedVerbs: verbs,
	})
	slug, _ := MintSlug(vendAgent.Name)
	_, ep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: credHash, CredentialPrefix: "sbk_iso", Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve friend %s: %v", fromPersona, err)
	}
	return ep
}

// Two friends granted the SAME queue must not share one idempotency-key dedup namespace. The
// (queue, idempotency_key) uniqueness is global, so an un-namespaced key would let friend B replay
// friend A's key on the shared queue and get created=false with A's existing Todo — Payload and
// Source included — handed back. CreateForFriend namespaces the key by the friend-edge id so B can
// neither suppress nor read A's handoff: B's colliding key mints B's OWN todo, and A's payload never
// leaks. A friend's own legitimate retry still dedups (same endpoint → same edge → same namespace).
// Governing: SPEC-0010 REQ "Work Flows as Todos, Not A2A Tasks" (cross-friend isolation, payload-disclosure guard).
func TestCreateForFriendIdempotencyKeyIsNamespacedPerFriend(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|iso-tgt", "Target")
	agentA := mustAgent(t, s, ctx, target.ID, "friend-a-agent")
	agentB := mustAgent(t, s, ctx, target.ID, "friend-b-agent")

	// Two DISTINCT friendships, both granted the same shared queue + verb.
	epA := approvedFriendFrom(t, s, ctx, target, agentA, "alice@a", "hostA@t", "iso-a",
		[]string{"shared"}, []string{"create_for"})
	epB := approvedFriendFrom(t, s, ctx, target, agentB, "bob@b", "hostB@t", "iso-b",
		[]string{"shared"}, []string{"create_for"})

	const sharedKey = "collide-1"
	secretPayload := []byte(`{"secret":"friend-A-only"}`)

	// Friend A hands off work carrying a private payload under the shared key.
	tdA, createdA, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: epA.ID, Queue: "shared", Intent: "create_for",
		Title: "A's work", Payload: secretPayload, IdempotencyKey: sharedKey,
	})
	if err != nil || !createdA {
		t.Fatalf("friend A handoff: created=%v err=%v", createdA, err)
	}

	// Friend B replays the SAME key on the SAME queue. It must mint B's OWN todo (created=true),
	// never dedup against A, and never return A's row/payload/source.
	tdB, createdB, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: epB.ID, Queue: "shared", Intent: "create_for",
		Title: "B's work", Payload: []byte(`{"benign":true}`), IdempotencyKey: sharedKey,
	})
	if err != nil {
		t.Fatalf("friend B colliding handoff: %v", err)
	}
	if !createdB {
		t.Fatal("friend B must mint its OWN todo, not silently dedup against friend A's key")
	}
	if tdB.ID == tdA.ID {
		t.Fatal("cross-friend key collision must not return friend A's todo to friend B")
	}
	if tdB.Source == tdA.Source {
		t.Fatalf("friend B's todo must be attributed to B (%q), not A (%q)", "bob@b", tdB.Source)
	}
	if string(tdB.Payload) == string(secretPayload) {
		t.Fatal("payload-disclosure: friend B must never receive friend A's payload")
	}

	// Friend A's own retry under the same key STILL dedups to A's original todo (idempotency for the
	// rightful sender is preserved by same-endpoint → same-edge → same namespace).
	tdARetry, createdARetry, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: epA.ID, Queue: "shared", Intent: "create_for",
		Title: "A's work retry", IdempotencyKey: sharedKey,
	})
	if err != nil {
		t.Fatalf("friend A retry: %v", err)
	}
	if createdARetry || tdARetry.ID != tdA.ID {
		t.Fatalf("friend A's own retry must dedup to its original todo: created=%v id=%s want %s",
			createdARetry, tdARetry.ID, tdA.ID)
	}

	// Exactly two todos exist on the shared queue: one per friend, fully isolated.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos WHERE queue = 'shared'`).Scan(&n); err != nil {
		t.Fatalf("count todos: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 isolated todos on the shared queue, got %d", n)
	}
}

// A locally SENT friend request persists immediately as a pending direction=outgoing edge owned by
// the sending human (to_human = sender is the ownership key, so it lists locally and Withdraw is
// reachable), granting nothing until the remote operator approves. It never collides with an inbound
// edge for the reverse pair, and a duplicate live outgoing ask for the same pair is refused.
// Governing: SPEC-0010 REQ "Friend-Request Lifecycle", REQ "Per-Direction, Revocable, Non-Transitive
// Edges"; story #174 (persist outgoing friend requests).
func TestFriendRequestOutgoingDirectionPersists(t *testing.T) {
	s, ctx := testStore(t)
	sender := mustHuman(t, s, ctx, "pocket|out-snd", "Sender")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "local-agent", ToPersona: "zed@far.example", Direction: "outgoing",
		FromHuman: sender.ID, ToHuman: sender.ID,
		RequestedVerbs: []string{"create_for"}, Reason: "hand you deploy checks",
	})
	if e.State != "pending" || e.Direction != "outgoing" {
		t.Fatalf("outgoing edge state=%s direction=%s, want pending/outgoing", e.State, e.Direction)
	}
	if e.EndpointID != "" {
		t.Fatalf("pending outgoing edge must mint no endpoint, got %q", e.EndpointID)
	}

	// The sender's own listing surfaces the outgoing edge (ownership key = sender).
	edges, err := s.ListFriendEdges(ctx, sender.ID, "pending")
	if err != nil || len(edges) != 1 || edges[0].ID != e.ID || edges[0].Direction != "outgoing" {
		t.Fatalf("sender must list their outgoing pending edge: %+v, %v", edges, err)
	}

	// A duplicate LIVE outgoing request for the same pair collides (anti-flood unique index).
	if _, err := s.CreateFriendRequest(ctx, CreateFriendRequestParams{
		FromPersona: "local-agent", ToPersona: "zed@far.example", Direction: "outgoing",
		ToHuman: sender.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate live outgoing request must be ErrConflict, got %v", err)
	}
}

// Withdraw removes a pending edge outright (RemoveFriendEdge with fromStates=pending): ownership
// isolation holds (cross-owner id is ErrNotFound, leaking nothing), a non-pending edge is
// ErrInvalidTransition and left untouched, and a withdrawn pair can be re-requested (the live unique
// index frees up). Governing: SPEC-0013 Endpoints table POST /friends/{id}/withdraw, SPEC-0010 REQ
// "Per-Direction, Revocable, Non-Transitive Edges"; story #174 (Withdraw + direction semantics).
func TestRemoveFriendEdgeWithdraw(t *testing.T) {
	s, ctx := testStore(t)
	sender := mustHuman(t, s, ctx, "pocket|wd-snd", "Sender")
	other := mustHuman(t, s, ctx, "pocket|wd-oth", "Other")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "local-agent", ToPersona: "zed@far.example", Direction: "outgoing",
		FromHuman: sender.ID, ToHuman: sender.ID, RequestedVerbs: []string{"create_for"},
	})

	// Cross-owner withdraw is ErrNotFound (indistinguishable from a missing id) and removes nothing.
	if _, err := s.RemoveFriendEdge(ctx, e.ID, other.ID, "pending"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner withdraw must be ErrNotFound, got %v", err)
	}

	// Withdraw by the owner deletes the pending edge and returns it as it was.
	gone, err := s.RemoveFriendEdge(ctx, e.ID, sender.ID, "pending")
	if err != nil || gone.ID != e.ID {
		t.Fatalf("withdraw: %+v, %v", gone, err)
	}
	if edges, err := s.ListFriendEdges(ctx, sender.ID); err != nil || len(edges) != 0 {
		t.Fatalf("withdrawn edge must be gone: %+v, %v", edges, err)
	}

	// The pair is free to re-request after withdrawal (the live unique index no longer blocks it).
	e2 := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "local-agent", ToPersona: "zed@far.example", Direction: "outgoing",
		FromHuman: sender.ID, ToHuman: sender.ID,
	})

	// A withdrawn-state mismatch: the fresh pending edge cannot be removed via the unblock states.
	if _, err := s.RemoveFriendEdge(ctx, e2.ID, sender.ID, "denied", "revoked"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("removing a pending edge under unblock states must be ErrInvalidTransition, got %v", err)
	}
	if edges, _ := s.ListFriendEdges(ctx, sender.ID, "pending"); len(edges) != 1 {
		t.Fatalf("refused removal must leave the edge, got %d", len(edges))
	}
}

// ListFriendEdges surfaces the vended endpoint's last-seen stamp on each edge (a scalar subquery,
// #175): nil while pending / before the credential ever authenticates, and populated after a
// TouchEndpoint — the Friends view's "last A2A call" meta line. Governing: SPEC-0007 (last-seen
// stamped on successful authentication); DESIGN (Friends card meta).
func TestListFriendEdgesCarriesEndpointLastSeen(t *testing.T) {
	s, ctx := testStore(t)
	requester := mustHuman(t, s, ctx, "pocket|seen-req", "Requester")
	target := mustHuman(t, s, ctx, "pocket|seen-tgt", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedQueues: []string{"reviews"}, RequestedVerbs: []string{"create_for"},
	})
	// Pending: no endpoint exists, so no last-seen.
	if edges, err := s.ListFriendEdges(ctx, target.ID); err != nil || len(edges) != 1 || edges[0].EndpointLastSeenAt != nil {
		t.Fatalf("pending edge must carry no last-seen: %+v, %v", edges, err)
	}

	slug, _ := MintSlug(vendAgent.Name)
	_, ep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: "seen-hash", CredentialPrefix: "sbk_seen", Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Approved but never authenticated: still no last-seen.
	if edges, _ := s.ListFriendEdges(ctx, target.ID); len(edges) != 1 || edges[0].EndpointLastSeenAt != nil {
		t.Fatalf("never-seen endpoint must carry no last-seen: %+v", edges)
	}

	// An authenticated call touches the endpoint; the listing now carries the stamp.
	if err := s.TouchEndpoint(ctx, ep.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	edges, err := s.ListFriendEdges(ctx, target.ID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("list after touch: %+v, %v", edges, err)
	}
	if edges[0].EndpointLastSeenAt == nil || time.Since(*edges[0].EndpointLastSeenAt) > time.Minute {
		t.Fatalf("touched endpoint's last-seen must surface on the edge: %+v", edges[0].EndpointLastSeenAt)
	}
}

// pendingApprovals is the read the Friends view's pending lane performs: the target human's own
// pending friend edges, straight out of friend_edges. It exists so this test asserts the SAME query
// path the Board uses, not a bespoke one.
func pendingApprovals(t *testing.T, s *Store, ctx context.Context, humanID string) []FriendEdge {
	t.Helper()
	edges, err := s.ListFriendEdges(ctx, humanID, "pending")
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}
	return edges
}

// A pending friend request is surfaced to the target human from its friend_edges row and NOT as a
// todo, and every terminal decision clears it from that view by the same write that decides the
// edge. This is the whole approval lane: it appears for the targeted human, is invisible to any
// other human, a duplicate ask collides instead of double-listing, and approve / deny / revoke each
// drain it. It also pins the negative half of the contract — the todos table stays empty throughout,
// because a human approval has no owning endpoint and todos.endpoint_id is NOT NULL.
// Governing: ADR-0022, SPEC-0010 REQ "Approval Surfaced from the Friend Edge" (scenarios "Pending
// request is visible to the targeted human", "A request is not visible to another human", "Deciding
// the edge clears the pending view", "A duplicate request collides rather than double-listing").
func TestPendingApprovalSurfacedFromFriendEdge(t *testing.T) {
	s, ctx := testStore(t)
	requester := mustHuman(t, s, ctx, "pocket|appr-req", "Requester")
	target := mustHuman(t, s, ctx, "pocket|appr-tgt", "Target")
	stranger := mustHuman(t, s, ctx, "pocket|appr-str", "Stranger")

	e := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedQueues: []string{"reviews"}, RequestedVerbs: []string{"create_for"},
		Reason: "hand you PR reviews", ProvenanceVerified: true,
	})

	// Happy path: the request appears for the TARGET human, carrying the legible who/why/scope the
	// human decides on — the edge row is the approval surface, so it must be self-sufficient.
	pending := pendingApprovals(t, s, ctx, target.ID)
	if len(pending) != 1 || pending[0].ID != e.ID {
		t.Fatalf("target must see exactly their one pending request, got %+v", pending)
	}
	got := pending[0]
	if got.FromHuman != requester.ID || got.FromPersona != "a@a" || got.ToPersona != "b@b" {
		t.Errorf("pending request lost its principals: %+v", got)
	}
	if got.Reason != "hand you PR reviews" || !got.ProvenanceVerified {
		t.Errorf("pending request lost its why/provenance: %+v", got)
	}
	if len(got.RequestedQueues) != 1 || got.RequestedQueues[0] != "reviews" ||
		len(got.RequestedVerbs) != 1 || got.RequestedVerbs[0] != "create_for" {
		t.Errorf("pending request lost its requested scope: %+v", got)
	}

	// Unhappy path — cross-tenant: another human must not see A's request to B. ListFriendEdges is
	// owner-scoped on to_human, which is the isolation key.
	if other := pendingApprovals(t, s, ctx, stranger.ID); len(other) != 0 {
		t.Fatalf("a request for another human must not be visible, got %+v", other)
	}

	// Unhappy path — duplicate: a second live ask for the same directional pair collides on
	// idx_friend_edges_live rather than producing a second listing. This is the dedup the removed
	// approval todo's idempotency key only pretended to provide.
	if _, err := s.CreateFriendRequest(ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", FromHuman: requester.ID, ToHuman: target.ID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate live request must be ErrConflict, got %v", err)
	}
	if again := pendingApprovals(t, s, ctx, target.ID); len(again) != 1 {
		t.Fatalf("a refused duplicate must not double-list, got %d pending", len(again))
	}

	// No todo was minted anywhere along the way. Asserted before the decisions so a leak cannot be
	// masked by a later "resolve" that marks it done rather than never creating it.
	assertNoTodos(t, s, ctx, "pending friend request")

	// Approving clears it: the edge moves to `approved`, and the pending lane reads state.
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-persona")
	slug, _ := MintSlug(vendAgent.Name)
	approved, _, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: e.ID, OwnerHumanID: target.ID, AgentID: vendAgent.ID,
		CredentialHash: "appr-hash-1", CredentialPrefix: "sbk_ap1", Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if left := pendingApprovals(t, s, ctx, target.ID); len(left) != 0 {
		t.Fatalf("approval must clear the pending view, got %+v", left)
	}

	// Revoking an approved edge also leaves the pending view empty — revocation is terminal, it does
	// not resurrect the request as work to redo.
	if _, err := s.RevokeFriendEdge(ctx, approved.ID, target.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if left := pendingApprovals(t, s, ctx, target.ID); len(left) != 0 {
		t.Fatalf("revocation must leave the pending view empty, got %+v", left)
	}

	// Denying clears it too. The revoked pair above is terminal, so a fresh request re-opens the live
	// index and gives us a pending edge to deny.
	d := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@a", ToPersona: "b@b", FromHuman: requester.ID, ToHuman: target.ID,
		RequestedVerbs: []string{"create_for"}, ProvenanceVerified: true,
	})
	if len(pendingApprovals(t, s, ctx, target.ID)) != 1 {
		t.Fatalf("re-request after a terminal edge must surface as pending")
	}
	if _, err := s.DenyFriendRequest(ctx, d.ID, target.ID); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if left := pendingApprovals(t, s, ctx, target.ID); len(left) != 0 {
		t.Fatalf("denial must clear the pending view, got %+v", left)
	}

	// Still no todos: the whole lifecycle ran without touching the todo queue.
	assertNoTodos(t, s, ctx, "full approve/revoke/deny lifecycle")
}

// assertNoTodos fails if any todo row exists. The approval lane must never mint one — under ADR-0022
// a todo is endpoint-owned, and a human friend approval has no owning endpoint.
func assertNoTodos(t *testing.T, s *Store, ctx context.Context, when string) {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos`).Scan(&n); err != nil {
		t.Fatalf("count todos: %v", err)
	}
	if n != 0 {
		t.Fatalf("%s minted %d todo(s); the approval lane must mint none", when, n)
	}
}

// TestCreateForFriendHandoffReachability pins WHO can actually reach a friend handoff after
// ADR-0022 made todo visibility endpoint-scoped rather than queue-scoped. Before ADR-0022 any of
// the target's agents listening on the granted queue would see the work; now visibility follows
// endpoint_id, which changes the answer. The change is easy to make silently and impossible to
// notice from the create path alone — TestCreateForFriendLandsAsTodo asserts queue/source/kind/
// dedup and would pass under either behaviour — so the reachability is asserted explicitly here.
//
// This test documents current behaviour rather than blessing it: whether a handoff SHOULD remain
// visible to the sender, and whether it SHOULD be drainable from the target's other endpoints, are
// SPEC-0010 design questions. Pinning them means any future answer has to change this test on
// purpose. Governing: ADR-0010, ADR-0022, SPEC-0010 REQ "Work Flows as Todos, Not A2A Tasks",
// SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func TestCreateForFriendHandoffReachability(t *testing.T) {
	s, ctx := testStore(t)
	target := mustHuman(t, s, ctx, "pocket|cff-reach", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "b-handler")
	_, friendEP := mustApprovedFriend(t, s, ctx, target, vendAgent,
		[]string{"reviews"}, []string{"create_for", "list_todos"})

	td, created, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: friendEP.ID, Queue: "reviews", Intent: "create_for",
		Title: "Review PR #7", IdempotencyKey: "reach-1",
	})
	if err != nil || !created {
		t.Fatalf("create_for friend: created=%v err=%v", created, err)
	}

	// The row is pinned to the friendship's vended endpoint — the tenancy anchor.
	if td.EndpointID != friendEP.ID {
		t.Fatalf("handoff todo endpoint_id=%s, want the friendship's vended endpoint %s",
			td.EndpointID, friendEP.ID)
	}

	// Reachable through the friendship endpoint. This is the positive control: it proves the
	// negative assertion below is about SCOPE, not about the todo failing to be written at all.
	viaFriendship, err := s.ListTodos(ctx, friendEP.ID, []string{"reviews"}, "", 50)
	if err != nil {
		t.Fatalf("list via friendship endpoint: %v", err)
	}
	if len(viaFriendship) != 1 || viaFriendship[0].ID != td.ID {
		t.Fatalf("the friendship endpoint must reach the handoff, got %d rows", len(viaFriendship))
	}

	// NOT reachable through another endpoint of the target's — even the SAME agent, even the same
	// granted queue. Queue name is no longer a visibility channel.
	otherEP := mustEndpointScoped(t, s, ctx, vendAgent.ID, "cff-reach-other",
		[]string{"reviews"}, []string{"list_todos"})
	viaOther, err := s.ListTodos(ctx, otherEP.ID, []string{"reviews"}, "", 50)
	if err != nil {
		t.Fatalf("list via the target's other endpoint: %v", err)
	}
	if len(viaOther) != 0 {
		t.Fatalf("a sibling endpoint on the same queue must not see the handoff, got %d rows", len(viaOther))
	}
}
