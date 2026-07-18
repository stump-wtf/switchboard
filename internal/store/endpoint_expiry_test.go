package store

// Credential-lifetime store semantics: `endpoints.expires_at` is chosen at vend time, an expired
// credential fails auth resolution identically to a revoked one (ErrNotFound — before any reaper
// tick), and ExpireEndpoints flips passed-expiry endpoints into the SPEC-0007 revoked lifecycle
// (revoked_at stamped, ids returned for session teardown) without touching no-expiry or
// future-expiry endpoints.
// Governing: SPEC-0016 REQ "Credential Lifetime" (scenario "Expiry enforcement"), SPEC-0007
// revocation semantics reused, ADR-0019.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// expiryFixture vends an agent-level endpoint with the given optional expiry and returns it.
func expiryFixture(t *testing.T, s *Store, ctx context.Context, humanID, name, hash string, expiresAt *time.Time) Endpoint {
	t.Helper()
	slug, err := MintSlug(name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	res, err := s.VendAgentEndpoint(ctx, VendParams{
		OwnerHumanID: humanID, Name: name,
		CredHash: hash, CredPrefix: "sbk_" + name[:min(6, len(name))], Slug: slug,
		Queues: []string{"reviews"}, Verbs: []string{"claim"},
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("vend %s: %v", name, err)
	}
	return res.Endpoint
}

// TestVendPersistsExpiry: a lifetime chosen at vend time round-trips through the vend transaction,
// ListEndpoints, and the Endpoints-view card projection; a vend without a lifetime persists NULL
// (valid until revoked).
func TestVendPersistsExpiry(t *testing.T) {
	s, ctx := testStore(t)
	h, err := s.UpsertHuman(ctx, "pocket|expiry", "Eve", "eve@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}

	exp := time.Now().Add(24 * time.Hour).UTC()
	withExp := expiryFixture(t, s, ctx, h.ID, "expiring-bot", "hash-exp-1", &exp)
	if withExp.ExpiresAt == nil {
		t.Fatal("vend with lifetime: ExpiresAt not persisted")
	}
	if d := withExp.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
		t.Errorf("persisted expiry %v drifted from requested %v", withExp.ExpiresAt, exp)
	}
	forever := expiryFixture(t, s, ctx, h.ID, "forever-bot", "hash-exp-2", nil)
	if forever.ExpiresAt != nil {
		t.Errorf("vend without lifetime persisted an expiry: %v", forever.ExpiresAt)
	}

	// The Endpoints-view projection carries the countdown data.
	cards, err := s.ListEndpointCards(ctx, h.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	byID := map[string]EndpointCard{}
	for _, c := range cards {
		byID[c.ID] = c
	}
	if c := byID[withExp.ID]; c.ExpiresAt == nil {
		t.Error("endpoint card missing ExpiresAt for the expiring endpoint")
	}
	if c := byID[forever.ID]; c.ExpiresAt != nil {
		t.Error("endpoint card carries an ExpiresAt for the no-expiry endpoint")
	}

	// ListEndpoints (per-agent) carries it too.
	eps, err := s.ListEndpoints(ctx, withExp.AgentID)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}
	if len(eps) != 1 || eps[0].ExpiresAt == nil {
		t.Error("ListEndpoints dropped the endpoint expiry")
	}
}

// TestEndpointByCredHashRejectsExpired: the auth resolution refuses an expired credential with
// ErrNotFound — the SAME answer as unknown or revoked — even before any reaper tick has flipped
// the row, while unexpired and no-expiry credentials keep resolving. SPEC-0016 scenario "Expiry
// enforcement": subsequent bearer access fails identically to revocation.
func TestEndpointByCredHashRejectsExpired(t *testing.T) {
	s, ctx := testStore(t)
	h, err := s.UpsertHuman(ctx, "pocket|expiry", "Eve", "eve@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}

	future := time.Now().Add(time.Hour).UTC()
	live := expiryFixture(t, s, ctx, h.ID, "live-bot", "hash-live", &future)
	forever := expiryFixture(t, s, ctx, h.ID, "forever-bot", "hash-forever", nil)

	if _, err := s.EndpointByCredHash(ctx, "hash-live"); err != nil {
		t.Fatalf("unexpired credential must resolve: %v", err)
	}
	if _, err := s.EndpointByCredHash(ctx, "hash-forever"); err != nil {
		t.Fatalf("no-expiry credential must resolve: %v", err)
	}

	// Push the expiry into the past (still state='active' — no reaper has run) and auth must refuse.
	if _, err := s.pool.Exec(ctx,
		`UPDATE endpoints SET expires_at = now() - interval '1 second' WHERE id = $1`, live.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if _, err := s.EndpointByCredHash(ctx, "hash-live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired credential resolved (err=%v); want ErrNotFound identical to revocation", err)
	}
	if _, err := s.EndpointByCredHash(ctx, "hash-forever"); err != nil {
		t.Fatalf("no-expiry credential must keep resolving after another endpoint expires: %v", err)
	}
	_ = forever
}

// TestExpireEndpointsFlipsOnlyPassedExpiry: the reaper query revokes exactly the active endpoints
// whose expiry has passed — stamping revoked_at and returning their ids for session teardown —
// and is idempotent: a second sweep returns nothing because the rows are no longer active.
func TestExpireEndpointsFlipsOnlyPassedExpiry(t *testing.T) {
	s, ctx := testStore(t)
	h, err := s.UpsertHuman(ctx, "pocket|expiry", "Eve", "eve@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}

	past := time.Now().Add(time.Hour).UTC() // vended live, backdated below
	expired := expiryFixture(t, s, ctx, h.ID, "expired-bot", "hash-a", &past)
	future := time.Now().Add(time.Hour).UTC()
	pending := expiryFixture(t, s, ctx, h.ID, "pending-bot", "hash-b", &future)
	forever := expiryFixture(t, s, ctx, h.ID, "forever-bot", "hash-c", nil)
	if _, err := s.pool.Exec(ctx,
		`UPDATE endpoints SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	ids, err := s.ExpireEndpoints(ctx)
	if err != nil {
		t.Fatalf("expire endpoints: %v", err)
	}
	if len(ids) != 1 || ids[0] != expired.ID {
		t.Fatalf("expire endpoints returned %v; want exactly [%s]", ids, expired.ID)
	}

	cards, err := s.ListEndpointCards(ctx, h.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	for _, c := range cards {
		switch c.ID {
		case expired.ID:
			if c.State != "revoked" {
				t.Errorf("expired endpoint state = %q; want revoked (expiry IS revocation)", c.State)
			}
			if c.RevokedAt == nil {
				t.Error("expired endpoint missing revoked_at stamp")
			}
		case pending.ID, forever.ID:
			if c.State != "active" {
				t.Errorf("endpoint %s state = %q; want active (untouched by expiry sweep)", c.AgentName, c.State)
			}
		}
	}

	// Idempotent: the flipped row is no longer active, so a second sweep finds nothing.
	again, err := s.ExpireEndpoints(ctx)
	if err != nil {
		t.Fatalf("second expire sweep: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second expire sweep returned %v; want none", again)
	}
}
