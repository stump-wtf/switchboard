package store

// EndpointReplayTargets is the one read replay_webhook_event resolves an owned default through, so
// its contract is pinned against PostgreSQL here: the list comes back in vend order, an endpoint
// vended without targets owns none, and a revoked or unknown endpoint is ErrNotFound rather than a
// list a stale caller could still replay to.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets".

import (
	"errors"
	"slices"
	"testing"
)

func TestEndpointReplayTargetsOwnedActiveOnly(t *testing.T) {
	s, ctx := testStore(t)
	owner := mustHuman(t, s, ctx, "pocket|replay-owner", "Owner")
	targets := []string{"https://b.example/hook", "https://a.example/hook"}

	vendWith := func(name, hash string, rt []string) Endpoint {
		t.Helper()
		slug, err := MintSlug(name)
		if err != nil {
			t.Fatalf("mint slug: %v", err)
		}
		res, err := s.VendAgentEndpoint(ctx, VendParams{
			OwnerHumanID: owner.ID, Name: name, CredHash: hash, CredPrefix: "sbk_" + name[:6],
			Slug: slug, Queues: []string{"reviews"}, Verbs: []string{"claim"}, ReplayTargets: rt,
		})
		if err != nil {
			t.Fatalf("vend %s: %v", name, err)
		}
		return res.Endpoint
	}
	withTargets := vendWith("replayer", "rt-hash-1", targets)
	without := vendWith("plainer", "rt-hash-2", nil)

	if got, err := s.EndpointReplayTargets(ctx, withTargets.ID); err != nil || !slices.Equal(got, targets) {
		t.Fatalf("owned targets = %v, %v; want %v in vend order", got, err, targets)
	}
	if got, err := s.EndpointReplayTargets(ctx, without.ID); err != nil || len(got) != 0 {
		t.Fatalf("endpoint vended without targets owns %v, %v; want none", got, err)
	}

	if err := s.RevokeEndpoint(ctx, withTargets.ID, owner.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got, err := s.EndpointReplayTargets(ctx, withTargets.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked endpoint's targets = %v, %v; want ErrNotFound", got, err)
	}
	if _, err := s.EndpointReplayTargets(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown endpoint: want ErrNotFound, got %v", err)
	}
}
