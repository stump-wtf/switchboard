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
