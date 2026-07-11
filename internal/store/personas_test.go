package store

import (
	"context"
	"errors"
	"testing"
)

// mustEndpointScoped vends an endpoint for an agent with an explicit queue/verb scope, so persona
// tests can build up an agent's vended grant. credHash must be unique per call.
func mustEndpointScoped(t *testing.T, s *Store, ctx context.Context, agentID, credHash string, queues, verbs []string) Endpoint {
	t.Helper()
	slug, err := MintSlug("persona-agent")
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, agentID, credHash, "sbk_test12", slug, queues, verbs)
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	return ep
}

// A persona's verb_subset and queues are accepted only when each is a subset of the backing agent's
// vended grant; any verb or queue the agent was never vended is rejected with ErrScopeExceeded and
// the persona is not persisted.
// Governing: ADR-0009, SPEC-0009 REQ "Persona Record"
// (scenario "Persona scope may not exceed the agent's vended grant").
func TestPersonaSubsetValidation(t *testing.T) {
	s, ctx := testStore(t)
	h := mustHuman(t, s, ctx, "pocket|persona-owner", "Owner")
	ag := mustAgent(t, s, ctx, h.ID, "reviewer-bot")
	mustEndpointScoped(t, s, ctx, ag.ID, "grant-1", []string{"reviews"}, []string{"list_todos", "claim", "complete"})

	// A subset of the vended grant is accepted.
	p, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "Reviewer",
		SystemPrompt: "Review PRs carefully.",
		VerbSubset:   []string{"list_todos", "claim"},
		Queues:       []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("in-scope persona should be accepted: %v", err)
	}
	if p.AgentID != ag.ID || p.SystemPrompt != "Review PRs carefully." {
		t.Fatalf("persona stored wrong: %+v", p)
	}

	// A verb the agent was never vended is rejected, and nothing is persisted.
	if _, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "Overreach",
		SystemPrompt: "…", VerbSubset: []string{"list_todos", "create_for"}, Queues: []string{"reviews"},
	}); !errors.Is(err, ErrScopeExceeded) {
		t.Fatalf("out-of-grant verb must be ErrScopeExceeded, got %v", err)
	}

	// A queue the agent was never vended is likewise rejected.
	if _, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "WrongQueue",
		SystemPrompt: "…", VerbSubset: []string{"list_todos"}, Queues: []string{"deploys"},
	}); !errors.Is(err, ErrScopeExceeded) {
		t.Fatalf("out-of-grant queue must be ErrScopeExceeded, got %v", err)
	}

	// Only the one accepted persona exists — the rejected ones left no rows.
	list, err := s.ListPersonas(ctx, h.ID)
	if err != nil || len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("only the accepted persona should persist: %+v, %v", list, err)
	}
}

// Two personas of one agent are independently bounded by their own verb_subset/queues: the reviewer
// carries neither the deployer's verbs nor its queue, and vice versa. Scope follows the persona, not
// the shared runtime. Governing: ADR-0009, SPEC-0009 REQ "Persona Record"
// (scenario "Two personas of one agent carry different access").
func TestTwoPersonasIndependentScope(t *testing.T) {
	s, ctx := testStore(t)
	h := mustHuman(t, s, ctx, "pocket|multi", "Owner")
	ag := mustAgent(t, s, ctx, h.ID, "multi-bot")
	// The agent's total vended grant is the union of two endpoints.
	mustEndpointScoped(t, s, ctx, ag.ID, "union-a", []string{"reviews"}, []string{"list_todos", "claim", "complete"})
	mustEndpointScoped(t, s, ctx, ag.ID, "union-b", []string{"deploys"}, []string{"webhook_create"})

	reviewer, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "Reviewer", SystemPrompt: "review",
		VerbSubset: []string{"list_todos", "claim", "complete"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}
	deployer, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "Deployer", SystemPrompt: "deploy",
		VerbSubset: []string{"list_todos", "claim", "complete", "webhook_create"}, Queues: []string{"deploys"},
	})
	if err != nil {
		t.Fatalf("create deployer: %v", err)
	}

	// Each persona's stored scope is exactly its own — bounded independently.
	if contains(reviewer.VerbSubset, "webhook_create") {
		t.Fatalf("reviewer must not carry the deployer's webhook verb: %v", reviewer.VerbSubset)
	}
	if contains(reviewer.Queues, "deploys") {
		t.Fatalf("reviewer must not carry the deploys queue: %v", reviewer.Queues)
	}
	if !contains(deployer.VerbSubset, "webhook_create") || !contains(deployer.Queues, "deploys") {
		t.Fatalf("deployer must carry its own webhook verb + deploys queue: %+v", deployer)
	}

	// A persona cannot be widened beyond the agent's grant even though a *sibling* persona holds the
	// verb — the bound is the agent's grant, but each persona's own subset is what is enforced.
	if _, err := s.UpdatePersona(ctx, UpdatePersonaParams{
		ID: reviewer.ID, OwnerHumanID: h.ID, Name: "Reviewer", SystemPrompt: "review",
		VerbSubset: []string{"list_todos", "deploy_now"}, Queues: []string{"reviews"},
	}); !errors.Is(err, ErrScopeExceeded) {
		t.Fatalf("widening a persona past the agent grant must be ErrScopeExceeded, got %v", err)
	}

	// But widening within the agent's grant is allowed (webhook_create is vended to the agent).
	up, err := s.UpdatePersona(ctx, UpdatePersonaParams{
		ID: reviewer.ID, OwnerHumanID: h.ID, Name: "Reviewer+", SystemPrompt: "review",
		VerbSubset: []string{"list_todos", "webhook_create"}, Queues: []string{"reviews", "deploys"},
	})
	if err != nil {
		t.Fatalf("in-grant update should succeed: %v", err)
	}
	if !contains(up.VerbSubset, "webhook_create") || up.Name != "Reviewer+" {
		t.Fatalf("update did not apply: %+v", up)
	}
}

// A human sees and mutates only their own personas; cross-owner reads/updates/deletes and creates
// referencing another human's agent are all ErrNotFound — indistinguishable from a missing id, so
// ownership failures never leak existence. Governing: ADR-0009, SPEC-0009 (owner-controlled personas).
func TestPersonaOwnershipIsolation(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|p-alice", "Alice")
	other := mustHuman(t, s, ctx, "pocket|p-mallory", "Mallory")
	ag := mustAgent(t, s, ctx, owner.ID, "alices-bot")
	mustEndpointScoped(t, s, ctx, ag.ID, "iso-1", []string{"reviews"}, []string{"list_todos"})

	p, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: owner.ID, AgentID: ag.ID, Name: "Face", SystemPrompt: "x",
		VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}

	// Cross-owner read is not-found, indistinguishable from a missing id.
	if _, err := s.GetPersona(ctx, p.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner read must be ErrNotFound, got %v", err)
	}
	if _, err := s.GetPersona(ctx, "00000000-0000-0000-0000-000000000000", owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id must be ErrNotFound, got %v", err)
	}

	// A human cannot create a persona against another human's agent (agent not owned → ErrNotFound).
	if _, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: other.ID, AgentID: ag.ID, Name: "Steal", SystemPrompt: "x",
		VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create against another's agent must be ErrNotFound, got %v", err)
	}

	// Cross-owner mutations all fail without changing anything.
	if _, err := s.UpdatePersona(ctx, UpdatePersonaParams{
		ID: p.ID, OwnerHumanID: other.ID, Name: "Hijack", SystemPrompt: "x",
		VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner update must be ErrNotFound, got %v", err)
	}
	if _, err := s.SetPersonaDiscoverable(ctx, p.ID, other.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner set-discoverable must be ErrNotFound, got %v", err)
	}
	if err := s.DeletePersona(ctx, p.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner delete must be ErrNotFound, got %v", err)
	}

	// Listings are tenant-scoped.
	mine, err := s.ListPersonas(ctx, owner.ID)
	if err != nil || len(mine) != 1 || mine[0].ID != p.ID {
		t.Fatalf("owner list wrong: %+v, %v", mine, err)
	}
	theirs, err := s.ListPersonas(ctx, other.ID)
	if err != nil || len(theirs) != 0 {
		t.Fatalf("other human must see no personas, got %d (%v)", len(theirs), err)
	}

	// The true owner can mark discoverable and then delete.
	pub, err := s.SetPersonaDiscoverable(ctx, p.ID, owner.ID, true)
	if err != nil || !pub.Discoverable {
		t.Fatalf("owner set-discoverable should succeed and set the flag: %+v, %v", pub, err)
	}
	if err := s.DeletePersona(ctx, p.ID, owner.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := s.GetPersona(ctx, p.ID, owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted persona must be gone, got %v", err)
	}
}

// PublishedPersonaByID resolves a PUBLISHED persona owner-agnostically (the public discovery card
// the friend-request intake targets) but treats an unpublished or unknown id as ErrNotFound — so an
// unpublished persona cannot be friend-requested and a malformed id never surfaces a SQL type error.
// Governing: ADR-0010, SPEC-0010 REQ "Anti-Spam — Bounded Discovery and Quotas", SPEC-0009.
func TestPublishedPersonaByID(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|pub-owner", "Owner")
	ag := mustAgent(t, s, ctx, owner.ID, "card-bot")
	mustEndpointScoped(t, s, ctx, ag.ID, "pub-grant", []string{"reviews"}, []string{"create_for"})
	p, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: owner.ID, AgentID: ag.ID, Name: "Card", SystemPrompt: "x",
		VerbSubset: []string{"create_for"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}

	// Unpublished → ErrNotFound (not discoverable, so not friend-requestable).
	if _, err := s.PublishedPersonaByID(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpublished persona must be ErrNotFound, got %v", err)
	}
	// After marking discoverable → resolvable owner-agnostically, carrying the owning human.
	if _, err := s.SetPersonaDiscoverable(ctx, p.ID, owner.ID, true); err != nil {
		t.Fatalf("set discoverable: %v", err)
	}
	got, err := s.PublishedPersonaByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("published persona must resolve: %v", err)
	}
	if got.OwnerHumanID != owner.ID {
		t.Fatalf("resolved owner = %q, want %q", got.OwnerHumanID, owner.ID)
	}
	// A malformed (non-uuid) id is ErrNotFound, never a SQL error.
	if _, err := s.PublishedPersonaByID(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id must be ErrNotFound, got %v", err)
	}
	// A random well-formed uuid that matches nothing is ErrNotFound too.
	if _, err := s.PublishedPersonaByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id must be ErrNotFound, got %v", err)
	}
}

// A persona slug is unique per owner: two personas of one human that derive the same slug collide
// (ErrConflict), while two different humans may each hold a persona with the same slug.
// Governing: SPEC-0009 REQ "Well-Known Card Endpoint" (per-persona base path resolves to one persona).
func TestPersonaSlugUniquePerOwner(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|slug-a", "Alice")
	other := mustHuman(t, s, ctx, "pocket|slug-b", "Bob")
	agA := mustAgent(t, s, ctx, owner.ID, "a-bot")
	agB := mustAgent(t, s, ctx, other.ID, "b-bot")
	mustEndpointScoped(t, s, ctx, agA.ID, "slug-grant-a", []string{"reviews"}, []string{"list_todos"})
	mustEndpointScoped(t, s, ctx, agB.ID, "slug-grant-b", []string{"reviews"}, []string{"list_todos"})

	first, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: owner.ID, AgentID: agA.ID, Name: "Code Reviewer", SystemPrompt: "x",
		VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("first persona: %v", err)
	}
	if first.Slug != "code-reviewer" {
		t.Fatalf("slug should derive from name, got %q", first.Slug)
	}

	// Same owner, same derived slug → conflict.
	if _, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: owner.ID, AgentID: agA.ID, Name: "Code Reviewer", SystemPrompt: "y",
		VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate slug for one owner must be ErrConflict, got %v", err)
	}

	// A different human may hold the same slug.
	if _, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: other.ID, AgentID: agB.ID, Name: "Code Reviewer", SystemPrompt: "z",
		VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	}); err != nil {
		t.Fatalf("different owner sharing a slug should be allowed: %v", err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
