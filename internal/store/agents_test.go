package store

import (
	"context"
	"errors"
	"testing"
)

// mustHuman seeds a human principal for ownership tests.
func mustHuman(t *testing.T, s *Store, ctx context.Context, subject, name string) Human {
	t.Helper()
	h, err := s.UpsertHuman(ctx, subject, name, "")
	if err != nil {
		t.Fatalf("upsert human %s: %v", subject, err)
	}
	return h
}

// mustAgent seeds an agent owned by the given human.
func mustAgent(t *testing.T, s *Store, ctx context.Context, ownerID, name string) Agent {
	t.Helper()
	ag, err := s.CreateAgent(ctx, ownerID, name, "")
	if err != nil {
		t.Fatalf("create agent %s: %v", name, err)
	}
	return ag
}

// mustEndpoint vends an endpoint for the given agent with a fixed scope.
func mustEndpoint(t *testing.T, s *Store, ctx context.Context, agentID, credHash string) Endpoint {
	t.Helper()
	slug, err := MintSlug("owned-agent")
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, agentID, credHash, "sbk_test12", slug, []string{"q"}, []string{"list_todos"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	return ep
}

// Registering an agent creates an owned record and nothing else: no endpoint row exists, so no
// credential can ever authenticate on its behalf until the human explicitly vends one.
// Governing: ADR-0008, SPEC-0007 REQ "Human as Accountable Principal"
// (scenario "Registration grants nothing").
func TestRegistrationGrantsNothing(t *testing.T) {
	s, ctx := testStore(t)
	h := mustHuman(t, s, ctx, "pocket|owner", "Owner")

	ag, err := s.CreateAgent(ctx, h.ID, "fresh-bot", "just registered")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if ag.OwnerHumanID != h.ID {
		t.Fatalf("agent owner=%s want %s", ag.OwnerHumanID, h.ID)
	}

	// The owned record exists...
	got, err := s.GetAgentOwned(ctx, ag.ID, h.ID)
	if err != nil || got.ID != ag.ID {
		t.Fatalf("owner should see the agent: %+v, %v", got, err)
	}

	// ...but carries no endpoints and therefore no credential.
	eps, err := s.ListEndpoints(ctx, ag.ID)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("registration must vend nothing, got %d endpoints", len(eps))
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM endpoints WHERE agent_id = $1`, ag.ID).Scan(&n); err != nil {
		t.Fatalf("count endpoints: %v", err)
	}
	if n != 0 {
		t.Fatalf("registration must persist zero endpoint rows, got %d", n)
	}
}

// A human cannot read another human's agent, and a cross-tenant miss is byte-for-byte the same
// sentinel as a genuinely missing id — ownership failures must not leak existence.
// Governing: ADR-0008, SPEC-0007 REQ "Human as Accountable Principal"
// (scenario "Ownership guards management").
func TestAgentOwnershipIsolation(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|alice", "Alice")
	other := mustHuman(t, s, ctx, "pocket|mallory", "Mallory")
	ag := mustAgent(t, s, ctx, owner.ID, "alices-bot")

	// Cross-tenant read is not-found, indistinguishable from a missing id.
	if _, err := s.GetAgentOwned(ctx, ag.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read must be ErrNotFound, got %v", err)
	}
	if _, err := s.GetAgentOwned(ctx, "00000000-0000-0000-0000-000000000000", owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id must be ErrNotFound, got %v", err)
	}

	// Listings are tenant-scoped: each human sees exactly their own agents.
	mine, err := s.ListAgents(ctx, owner.ID)
	if err != nil || len(mine) != 1 || mine[0].ID != ag.ID {
		t.Fatalf("owner list wrong: %+v, %v", mine, err)
	}
	theirs, err := s.ListAgents(ctx, other.ID)
	if err != nil || len(theirs) != 0 {
		t.Fatalf("other human must see no agents, got %d (%v)", len(theirs), err)
	}
}

// Revocation binds the ownership predicate into the UPDATE itself: a non-owner's revoke reports
// not-found and mutates nothing — the credential keeps resolving until the true owner revokes.
// Governing: ADR-0008, SPEC-0007 REQ "Human as Accountable Principal",
// REQ "Database Operation Standards" (scenario "Revocation binds ownership in the write").
func TestCrossHumanRevokeRejectedWithoutStateChange(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|alice2", "Alice")
	other := mustHuman(t, s, ctx, "pocket|mallory2", "Mallory")
	ag := mustAgent(t, s, ctx, owner.ID, "alices-bot")
	ep := mustEndpoint(t, s, ctx, ag.ID, "crosshash-1")

	// Non-owner revoke: not-found, and the endpoint must remain fully live.
	if err := s.RevokeEndpoint(ctx, ep.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant revoke must be ErrNotFound, got %v", err)
	}
	auth, err := s.EndpointByCredHash(ctx, "crosshash-1")
	if err != nil {
		t.Fatalf("credential must still resolve after a rejected revoke: %v", err)
	}
	if auth.OwnerHumanID != owner.ID {
		t.Fatalf("auth owner=%s want %s", auth.OwnerHumanID, owner.ID)
	}
	var state string
	if err := s.pool.QueryRow(ctx,
		`SELECT state FROM endpoints WHERE id = $1`, ep.ID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "active" {
		t.Fatalf("rejected revoke must not change state, got %q", state)
	}

	// The true owner's revoke lands, and the credential stops resolving.
	if err := s.RevokeEndpoint(ctx, ep.ID, owner.ID); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
	if _, err := s.EndpointByCredHash(ctx, "crosshash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked credential must not resolve, got %v", err)
	}
}

// Ownership is a database invariant, not just an application convention: every agent references
// exactly one human (NOT NULL FK) and every endpoint references exactly one agent (NOT NULL FK),
// so an orphaned or unowned record cannot exist.
// Governing: ADR-0008, SPEC-0007 REQ "Human as Accountable Principal".
func TestOwnershipSchemaConstraints(t *testing.T) {
	s, ctx := testStore(t)

	// Column-level: owner FKs are NOT NULL.
	notNullable := func(table, column string) {
		t.Helper()
		var isNullable string
		err := s.pool.QueryRow(ctx,
			`SELECT is_nullable FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
			table, column).Scan(&isNullable)
		if err != nil {
			t.Fatalf("%s.%s: %v", table, column, err)
		}
		if isNullable != "NO" {
			t.Fatalf("%s.%s must be NOT NULL, is_nullable=%s", table, column, isNullable)
		}
	}
	notNullable("agents", "owner_human_id")
	notNullable("endpoints", "agent_id")

	// Referential: a foreign key actually binds each column to its parent table.
	fkTarget := func(table, column string) string {
		t.Helper()
		var target string
		err := s.pool.QueryRow(ctx, `
			SELECT ccu.table_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_name = tc.constraint_name
			JOIN information_schema.constraint_column_usage ccu
			  ON ccu.constraint_name = tc.constraint_name
			WHERE tc.constraint_type = 'FOREIGN KEY'
			  AND tc.table_name = $1 AND kcu.column_name = $2`,
			table, column).Scan(&target)
		if err != nil {
			t.Fatalf("%s.%s has no foreign key: %v", table, column, err)
		}
		return target
	}
	if got := fkTarget("agents", "owner_human_id"); got != "humans" {
		t.Fatalf("agents.owner_human_id must reference humans, got %q", got)
	}
	if got := fkTarget("endpoints", "agent_id"); got != "agents" {
		t.Fatalf("endpoints.agent_id must reference agents, got %q", got)
	}

	// Behavioral: the database rejects an agent owned by nobody real.
	if _, err := s.CreateAgent(ctx, "00000000-0000-0000-0000-000000000000", "orphan-bot", ""); err == nil {
		t.Fatal("creating an agent for a nonexistent human must fail (FK violation)")
	}
}

// TestListEndpointCards: the Endpoints-view projection is scoped strictly by owner, orders active
// endpoints before revoked ones, carries the credential DISPLAY PREFIX (never a reversible
// credential), and stamps revoked_at on a killed endpoint. Governing: SPEC-0013 REQ "Endpoints View
// and Vend Modal", SPEC-0007 REQ "Human as Accountable Principal".
func TestListEndpointCards(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "cards|owner", "Owner")
	other := mustHuman(t, s, ctx, "cards|other", "Other")

	agOwner := mustAgent(t, s, ctx, owner.ID, "reviewer-bot")
	slugA, _ := MintSlug("reviewer-bot")
	active, err := s.CreateEndpoint(ctx, agOwner.ID, "hash-active", "sbk_active0", slugA, []string{"reviews"}, []string{"claim"})
	if err != nil {
		t.Fatalf("vend active: %v", err)
	}
	agOwner2 := mustAgent(t, s, ctx, owner.ID, "old-bot")
	slugB, _ := MintSlug("old-bot")
	revoked, err := s.CreateEndpoint(ctx, agOwner2.ID, "hash-revoked", "sbk_revoke0", slugB, []string{"deploys"}, []string{"complete"})
	if err != nil {
		t.Fatalf("vend revoked: %v", err)
	}
	if err := s.RevokeEndpoint(ctx, revoked.ID, owner.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Another human's endpoint must never appear in the owner's cards.
	agOther := mustAgent(t, s, ctx, other.ID, "intruder-bot")
	slugC, _ := MintSlug("intruder-bot")
	if _, err := s.CreateEndpoint(ctx, agOther.ID, "hash-other", "sbk_other00", slugC, []string{"x"}, []string{"claim"}); err != nil {
		t.Fatalf("vend other: %v", err)
	}

	cards, err := s.ListEndpointCards(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("owner has %d cards, want 2 (no cross-owner leak)", len(cards))
	}
	// Active sorts before revoked.
	if cards[0].State != "active" || cards[1].State != "revoked" {
		t.Errorf("ordering = [%s, %s], want [active, revoked]", cards[0].State, cards[1].State)
	}
	a := cards[0]
	if a.ID != active.ID || a.AgentName != "reviewer-bot" {
		t.Errorf("active card = {%s, %s}, want {%s, reviewer-bot}", a.ID, a.AgentName, active.ID)
	}
	if a.CredentialPrefix != "sbk_active0" {
		t.Errorf("card prefix = %q, want the display prefix sbk_active0", a.CredentialPrefix)
	}
	if a.RevokedAt != nil {
		t.Errorf("active card carries a revoked_at stamp: %v", a.RevokedAt)
	}
	r := cards[1]
	if r.RevokedAt == nil {
		t.Error("revoked card must carry a revoked_at stamp (killed · when)")
	}
}

// vendParams builds a VendAgentEndpoint input with a fixed scope, distinct credential material, and
// the given slug — the harness for the atomic-vend and persona-binding tests.
func vendParams(ownerHumanID, name, slug string) VendParams {
	return VendParams{
		OwnerHumanID: ownerHumanID, Name: name,
		CredHash: "hash-" + slug, CredPrefix: "sbk_" + slug[:min(len(slug), 6)],
		Slug: slug, Queues: []string{"reviews"}, Verbs: []string{"list_todos", "claim"},
	}
}

// A vend is atomic: if the endpoint insert fails after the agent insert (here forced with a duplicate
// slug), the whole transaction rolls back and no orphan agent is left behind. Governing: SPEC-0013 REQ
// "Endpoints View and Vend Modal" (agent+endpoint minted together), ADR-0008.
func TestVendAgentEndpointRollsBackOrphanAgent(t *testing.T) {
	s, ctx := testStore(t)
	h := mustHuman(t, s, ctx, "pocket|vend-atomic", "Owner")

	// First vend succeeds and claims the slug.
	if _, err := s.VendAgentEndpoint(ctx, vendParams(h.ID, "reviewer-bot", "dup-slug-abc")); err != nil {
		t.Fatalf("first vend should succeed: %v", err)
	}

	// Second vend reuses the same slug: the agent insert succeeds inside the tx, then the endpoint
	// insert violates the unique slug index — the tx must roll back, leaving no second agent.
	if _, err := s.VendAgentEndpoint(ctx, vendParams(h.ID, "orphan-bot", "dup-slug-abc")); err == nil {
		t.Fatal("second vend with a duplicate slug must fail")
	}

	agents, err := s.ListAgents(ctx, h.ID)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agent count = %d, want 1 — the failed vend left an orphan agent", len(agents))
	}
	if agents[0].Name != "reviewer-bot" {
		t.Fatalf("surviving agent = %q, want the first vend's reviewer-bot", agents[0].Name)
	}
}

// Vending with a persona binds endpoints.persona_id and vends on the persona's OWN backing agent, so
// the endpoint and persona always share an agent. The bound persona then surfaces on the endpoint
// card. Governing: SPEC-0013 REQ "Endpoints View and Vend Modal" (optional persona), ADR-0009.
func TestVendAgentEndpointBindsPersona(t *testing.T) {
	s, ctx := testStore(t)
	h := mustHuman(t, s, ctx, "pocket|vend-persona", "Owner")

	// A backing agent with a vended grant the persona can be a subset of.
	base, err := s.VendAgentEndpoint(ctx, vendParams(h.ID, "reviewer-bot", "base-slug-1"))
	if err != nil {
		t.Fatalf("base vend: %v", err)
	}
	agentID := base.Endpoint.AgentID

	p, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: agentID, Name: "Reviewer",
		SystemPrompt: "Review carefully.", VerbSubset: []string{"list_todos", "claim"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}

	// Vend with the persona: no new agent, endpoint bound to the persona's agent, persona_id set.
	pp := vendParams(h.ID, "ignored-name", "persona-slug-1")
	pp.PersonaID = p.ID
	res, err := s.VendAgentEndpoint(ctx, pp)
	if err != nil {
		t.Fatalf("persona vend: %v", err)
	}
	if res.Endpoint.AgentID != agentID {
		t.Fatalf("persona endpoint agent = %s, want the persona's backing agent %s", res.Endpoint.AgentID, agentID)
	}
	if res.AgentName != "reviewer-bot" {
		t.Fatalf("reveal agent name = %q, want the persona's backing agent name", res.AgentName)
	}
	// Only one agent exists — the persona path reused it rather than minting a second.
	if agents, _ := s.ListAgents(ctx, h.ID); len(agents) != 1 {
		t.Fatalf("agent count = %d, want 1 (persona vend must reuse the backing agent)", len(agents))
	}

	// The bound persona surfaces on the endpoint card (LEFT JOIN on persona_id).
	cards, err := s.ListEndpointCards(ctx, h.ID)
	if err != nil {
		t.Fatalf("list cards: %v", err)
	}
	var bound *EndpointCard
	for i := range cards {
		if cards[i].ID == res.Endpoint.ID {
			bound = &cards[i]
		}
	}
	if bound == nil {
		t.Fatalf("persona-bound endpoint %s missing from cards", res.Endpoint.ID)
	}
	if bound.PersonaName != "Reviewer" {
		t.Fatalf("card persona = %q, want Reviewer — persona_id was not bound", bound.PersonaName)
	}
}

// A vend can never bind a persona owned by another human: the owner-scoped lookup returns no row, so
// the persona resolves to ErrNotFound and nothing is minted for the vending human. Governing:
// SPEC-0007 REQ "Human as Accountable Principal", ADR-0009.
func TestVendAgentEndpointRejectsForeignPersona(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|persona-real-owner", "Owner")
	intruder := mustHuman(t, s, ctx, "pocket|persona-intruder", "Intruder")

	base, err := s.VendAgentEndpoint(ctx, vendParams(owner.ID, "reviewer-bot", "owner-slug-1"))
	if err != nil {
		t.Fatalf("owner base vend: %v", err)
	}
	p, err := s.CreatePersona(ctx, CreatePersonaParams{
		OwnerHumanID: owner.ID, AgentID: base.Endpoint.AgentID, Name: "Reviewer",
		SystemPrompt: "…", VerbSubset: []string{"list_todos"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}

	// The intruder tries to vend against the owner's persona.
	pp := vendParams(intruder.ID, "intruder-bot", "intruder-slug-1")
	pp.PersonaID = p.ID
	if _, err := s.VendAgentEndpoint(ctx, pp); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign persona vend = %v, want ErrNotFound", err)
	}
	// Nothing was minted for the intruder — no agent, no endpoint.
	if agents, _ := s.ListAgents(ctx, intruder.ID); len(agents) != 0 {
		t.Fatalf("intruder agent count = %d, want 0", len(agents))
	}
	if cards, _ := s.ListEndpointCards(ctx, intruder.ID); len(cards) != 0 {
		t.Fatalf("intruder endpoint count = %d, want 0", len(cards))
	}
}
