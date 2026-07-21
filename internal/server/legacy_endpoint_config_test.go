package server

// Boot-time validation of SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID. The operator-configured
// receivers have no vended endpoint of their own, so this one env var decides tenant ownership for
// every todo they mint (and for every queue adapter that states no EndpointID). A wrong value is
// not caught by legacyEndpoint(), which guards only the EMPTY case — it reaches the INSERT and
// becomes a permanent 500 per delivery. These tests pin that a bad value stops the process at boot.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL, matching the store test pattern.
// Governing: ADR-0022, SPEC-0001 REQ "Error Handling Standards".

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/store"
)

func TestValidateLegacyEndpointID(t *testing.T) {
	_, st, ctx, ep := newReceiverDBRouter(t)

	// Positive control FIRST: the real, active endpoint the fixture vended must validate. Without
	// this, a validator that rejected everything would pass every negative case below.
	if err := validateLegacyEndpointID(ctx, st, ep.ID); err != nil {
		t.Fatalf("an active endpoint id must validate, got %v", err)
	}
	// Unset stays supported — the receivers answer 503 until PR 2 retires them.
	if err := validateLegacyEndpointID(ctx, st, ""); err != nil {
		t.Fatalf("unset must remain valid, got %v", err)
	}

	t.Run("non-uuid is refused at boot", func(t *testing.T) {
		// The likely operator error: pasting an endpoint's slug instead of its uuid. Reaching the
		// INSERT this would be a 22P02 on every delivery.
		err := validateLegacyEndpointID(ctx, st, ep.Slug)
		if err == nil {
			t.Fatal("a non-uuid legacy endpoint id must fail startup")
		}
		if !strings.Contains(err.Error(), "not a uuid") {
			t.Fatalf("error must name the actual problem, got %v", err)
		}
	})

	t.Run("well-formed but unknown uuid is refused at boot", func(t *testing.T) {
		// Reaching the INSERT this would be a 23503 FK violation on every delivery.
		const absent = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		err := validateLegacyEndpointID(ctx, st, absent)
		if err == nil {
			t.Fatal("a uuid naming no endpoint must fail startup")
		}
		if !strings.Contains(err.Error(), "no active endpoint") {
			t.Fatalf("error must name the actual problem, got %v", err)
		}
	})

	t.Run("revoked endpoint is refused at boot", func(t *testing.T) {
		// Well-formed and real, so the INSERT would SUCCEED — and every todo minted onto it would be
		// undrainable, because a revoked endpoint's credential no longer resolves. Silent
		// accumulation of unreachable work is worse than a refused boot.
		owner, err := st.EndpointOwnerHuman(ctx, ep.ID)
		if err != nil {
			t.Fatalf("resolve owner: %v", err)
		}
		if err := st.RevokeEndpoint(ctx, ep.ID, owner); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		err = validateLegacyEndpointID(ctx, st, ep.ID)
		if err == nil {
			t.Fatal("a revoked endpoint must fail startup — its todos would be undrainable")
		}
		if !strings.Contains(err.Error(), "no active endpoint") {
			t.Fatalf("error must name the actual problem, got %v", err)
		}
	})
}

// A store error that is NOT ErrNotFound must propagate rather than masquerade as a config error.
func TestValidateLegacyEndpointIDPropagatesStoreErrors(t *testing.T) {
	_, st, _, ep := newReceiverDBRouter(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err := validateLegacyEndpointID(cancelled, st, ep.ID)
	if err == nil {
		t.Fatal("a store failure must not validate as success")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a transport failure must not be reported as not-found: %v", err)
	}
}
