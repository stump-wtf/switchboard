package store

// The endpoint default lease against PostgreSQL: vend stores it and every endpoint read carries it,
// the bounds hold on both write paths and in the column's CHECK, nil resets, a revoked endpoint is
// refused, another human's endpoint is not found, and an edit leaves a lease already granted alone.
//
// Governing: ADR-0043, SPEC-0007 REQ "Endpoint Default Lease", SPEC-0006 REQ "Lease Lifecycle and
// Crash Safety".

import (
	"errors"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/lease"
)

func intPtr(n int) *int { return &n }

// vendLease vends an endpoint for owner with the given default lease (nil = server default).
func vendLease(t *testing.T, s *Store, ownerID, name, hash string, def *int) (VendResult, error) {
	t.Helper()
	slug, err := MintSlug(name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	return s.VendAgentEndpoint(t.Context(), VendParams{
		OwnerHumanID: ownerID, Name: name, CredHash: hash, CredPrefix: "sbk_" + name[:5],
		Slug: slug, Queues: []string{"reviews"}, Verbs: []string{"claim", "heartbeat"},
		DefaultLeaseTTLSeconds: def,
	})
}

func TestVendStoresDefaultLeaseAndEveryReadCarriesIt(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|lease-owner", "Owner")

	res, err := vendLease(t, s, owner.ID, "long-reviewer", "lease-hash-1", intPtr(3600))
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	if got := res.Endpoint.DefaultLeaseTTLSeconds; got == nil || *got != 3600 {
		t.Fatalf("vend result default = %v, want 3600", got)
	}
	plain, err := vendLease(t, s, owner.ID, "short-worker", "lease-hash-2", nil)
	if err != nil {
		t.Fatalf("vend plain: %v", err)
	}
	if plain.Endpoint.DefaultLeaseTTLSeconds != nil {
		t.Fatalf("plain vend default = %v, want nil (server default)", *plain.Endpoint.DefaultLeaseTTLSeconds)
	}

	auth, err := s.EndpointByCredHash(ctx, "lease-hash-1")
	if err != nil || auth.DefaultLeaseTTLSeconds == nil || *auth.DefaultLeaseTTLSeconds != 3600 {
		t.Fatalf("auth endpoint default = %v, %v; want 3600", auth.DefaultLeaseTTLSeconds, err)
	}
	if got, err := s.EndpointDefaultLease(ctx, res.Endpoint.ID); err != nil || got == nil || *got != 3600 {
		t.Fatalf("EndpointDefaultLease = %v, %v; want 3600", got, err)
	}
	if got, err := s.EndpointDefaultLease(ctx, plain.Endpoint.ID); err != nil || got != nil {
		t.Fatalf("EndpointDefaultLease(plain) = %v, %v; want nil", got, err)
	}
	cards, err := s.ListEndpointCards(ctx, owner.ID)
	if err != nil {
		t.Fatalf("cards: %v", err)
	}
	seen := 0
	for _, c := range cards {
		switch c.ID {
		case res.Endpoint.ID:
			seen++
			if c.DefaultLeaseTTLSeconds == nil || *c.DefaultLeaseTTLSeconds != 3600 {
				t.Errorf("card default = %v, want 3600", c.DefaultLeaseTTLSeconds)
			}
		case plain.Endpoint.ID:
			seen++
			if c.DefaultLeaseTTLSeconds != nil {
				t.Errorf("plain card default = %v, want nil", *c.DefaultLeaseTTLSeconds)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of 2 cards", seen)
	}
	eps, err := s.ListEndpoints(ctx, res.Endpoint.AgentID)
	if err != nil || len(eps) != 1 || eps[0].DefaultLeaseTTLSeconds == nil || *eps[0].DefaultLeaseTTLSeconds != 3600 {
		t.Fatalf("ListEndpoints = %+v, %v; want one endpoint with default 3600", eps, err)
	}
}

// TestVendRefusesOutOfRangeDefaultMintingNothing: 59 and 86401 are refused before the transaction,
// so no agent and no endpoint exist afterwards.
func TestVendRefusesOutOfRangeDefaultMintingNothing(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|lease-bad", "Owner")
	for i, bad := range []int{lease.MinDefaultSeconds - 1, lease.MaxSeconds + 1} {
		if _, err := vendLease(t, s, owner.ID, "bad-lease-agent", "bad-lease-"+string(rune('a'+i)), intPtr(bad)); !errors.Is(err, lease.ErrOutOfRange) {
			t.Fatalf("vend with default %d: err = %v, want lease.ErrOutOfRange", bad, err)
		}
	}
	var agents, endpoints int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agents WHERE owner_human_id = $1),
		(SELECT count(*) FROM endpoints e JOIN agents a ON a.id = e.agent_id WHERE a.owner_human_id = $1)`,
		owner.ID).Scan(&agents, &endpoints); err != nil {
		t.Fatalf("count: %v", err)
	}
	if agents != 0 || endpoints != 0 {
		t.Fatalf("a refused vend left %d agents and %d endpoints", agents, endpoints)
	}
}

// TestSetEndpointDefaultLeaseBounds walks both write paths across the bounds: 60 and 86400 are
// stored, 59 and 86401 are refused and leave the stored value as it was, and nil resets.
func TestSetEndpointDefaultLeaseBounds(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|lease-bounds", "Owner")
	res, err := vendLease(t, s, owner.ID, "bounded-agent", "lease-bounds-1", nil)
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	id := res.Endpoint.ID

	writers := map[string]func(*int) (*int, error){
		"self":  func(v *int) (*int, error) { return s.SetEndpointDefaultLease(ctx, id, v) },
		"human": func(v *int) (*int, error) { return s.SetEndpointDefaultLeaseForHuman(ctx, id, owner.ID, v) },
	}
	for name, set := range writers {
		for _, ok := range []int{lease.MinDefaultSeconds, lease.MaxSeconds} {
			got, err := set(intPtr(ok))
			if err != nil || got == nil || *got != ok {
				t.Fatalf("%s: set %d = %v, %v", name, ok, got, err)
			}
		}
		for _, bad := range []int{lease.MinDefaultSeconds - 1, lease.MaxSeconds + 1} {
			if _, err := set(intPtr(bad)); !errors.Is(err, lease.ErrOutOfRange) {
				t.Fatalf("%s: set %d: err = %v, want lease.ErrOutOfRange", name, bad, err)
			}
			if got, _ := s.EndpointDefaultLease(ctx, id); got == nil || *got != lease.MaxSeconds {
				t.Fatalf("%s: a refused %d changed the stored default to %v", name, bad, got)
			}
		}
		if got, err := set(nil); err != nil || got != nil {
			t.Fatalf("%s: reset = %v, %v; want nil", name, got, err)
		}
		if got, _ := s.EndpointDefaultLease(ctx, id); got != nil {
			t.Fatalf("%s: after reset the stored default is %d, want nil", name, *got)
		}
	}
}

// TestDefaultLeaseCheckConstraintMatchesTheBounds pins migration 0029's CHECK to internal/lease: a
// write that skips the Go validation still cannot store a value outside the bounds.
func TestDefaultLeaseCheckConstraintMatchesTheBounds(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|lease-check", "Owner")
	res, err := vendLease(t, s, owner.ID, "checked-agent", "lease-check-1", nil)
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	set := func(v int) error {
		_, err := s.pool.Exec(ctx, `UPDATE endpoints SET default_lease_ttl_seconds = $2 WHERE id = $1`, res.Endpoint.ID, v)
		return err
	}
	for _, ok := range []int{lease.MinDefaultSeconds, lease.MaxSeconds} {
		if err := set(ok); err != nil {
			t.Errorf("CHECK refused %d, inside the bounds: %v", ok, err)
		}
	}
	for _, bad := range []int{lease.MinDefaultSeconds - 1, lease.MaxSeconds + 1} {
		if err := set(bad); err == nil {
			t.Errorf("CHECK accepted %d, outside the bounds", bad)
		}
	}
}

// TestSetEndpointDefaultLeaseRefusesRevokedAndForeign: the self path answers not-found for a
// revoked endpoint; the human path answers conflict for the owner's revoked endpoint and not-found
// for another human's endpoint or an unknown id, changing nothing.
func TestSetEndpointDefaultLeaseRefusesRevokedAndForeign(t *testing.T) {
	s, ctx := testStore(t)
	alice := mustHuman(t, s, ctx, "pocket|lease-alice", "Alice")
	bob := mustHuman(t, s, ctx, "pocket|lease-bob", "Bob")
	res, err := vendLease(t, s, alice.ID, "alices-agent", "lease-foreign-1", intPtr(600))
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	id := res.Endpoint.ID

	if _, err := s.SetEndpointDefaultLeaseForHuman(ctx, id, bob.ID, intPtr(3600)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob setting alice's endpoint: err = %v, want ErrNotFound", err)
	}
	if _, err := s.SetEndpointDefaultLeaseForHuman(ctx, "00000000-0000-0000-0000-000000000000", alice.ID, intPtr(3600)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown endpoint: err = %v, want ErrNotFound", err)
	}
	if got, _ := s.EndpointDefaultLease(ctx, id); got == nil || *got != 600 {
		t.Fatalf("a refused write changed the default to %v", got)
	}

	if err := s.RevokeEndpoint(ctx, id, alice.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.SetEndpointDefaultLeaseForHuman(ctx, id, alice.ID, intPtr(3600)); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner setting a revoked endpoint: err = %v, want ErrConflict", err)
	}
	if _, err := s.SetEndpointDefaultLeaseForHuman(ctx, id, bob.ID, intPtr(3600)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob setting alice's revoked endpoint: err = %v, want ErrNotFound (revoked must not leak)", err)
	}
	if _, err := s.SetEndpointDefaultLease(ctx, id, intPtr(3600)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("self-set on a revoked endpoint: err = %v, want ErrNotFound", err)
	}
}

// TestDefaultLeaseEditLeavesGrantedLeasesAlone: changing the default changes no lease already
// granted; only the next claim or heartbeat reads it.
func TestDefaultLeaseEditLeavesGrantedLeasesAlone(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|lease-live", "Owner")
	res, err := vendLease(t, s, owner.ID, "live-agent", "lease-live-1", nil)
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	ep := res.Endpoint
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep.ID, Queue: "reviews", Title: "review #1"})
	if err != nil {
		t.Fatalf("create todo: %v", err)
	}
	claimed, err := s.ClaimTodo(ctx, ep.ID, td.ID, "agent:"+ep.AgentID, lease.TTL(0, nil))
	if err != nil || claimed.LeaseExpiresAt == nil {
		t.Fatalf("claim: %+v, %v", claimed, err)
	}
	before := *claimed.LeaseExpiresAt

	if _, err := s.SetEndpointDefaultLeaseForHuman(ctx, ep.ID, owner.ID, intPtr(lease.MaxSeconds)); err != nil {
		t.Fatalf("set default: %v", err)
	}
	after, err := s.GetTodo(ctx, ep.ID, td.ID)
	if err != nil || after.LeaseExpiresAt == nil {
		t.Fatalf("get todo: %+v, %v", after, err)
	}
	if !after.LeaseExpiresAt.Equal(before) {
		t.Fatalf("editing the default moved a granted lease from %s to %s", before, after.LeaseExpiresAt)
	}
	if time.Until(before) > lease.DefaultTTL {
		t.Fatalf("the claim before the edit got %s, more than the server default", time.Until(before))
	}
}
