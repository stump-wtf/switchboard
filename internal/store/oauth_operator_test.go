package store

// Operator-grant (human-bound) OAuth token tests (ADR-0023): a token minted with humanID binds to
// the human, resolves via HumanByOAuthToken, rotates within its grant, and never resolves through
// the endpoint path. Skipped without SWITCHBOARD_TEST_DATABASE_URL, like every DB-backed suite.

import (
	"errors"
	"testing"
	"time"
)

// TestOperatorGrantRoundTrip: the human-bound mint → HumanByOAuthToken resolution round-trip, and
// the shape separation — an operator token resolves to a human but never to an endpoint, and an
// endpoint token never resolves to a human.
func TestOperatorGrantRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	human, ep := oauthCodeFixture(t, s, ctx, "pocket|opgrant", "op-bot", "hash-op-1", "op-bot-aa01", "cid-op-1")

	policy := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)

	// Human-bound token: resolves to the human, never to an endpoint.
	if _, err := s.CreateOAuthToken(ctx, "th-op-h", "rh-op-h", "cid-op-1", "", human.ID, policy); err != nil {
		t.Fatalf("create human-bound token: %v", err)
	}
	got, err := s.HumanByOAuthToken(ctx, "th-op-h")
	if err != nil {
		t.Fatalf("resolve human by operator token: %v", err)
	}
	if got.ID != human.ID {
		t.Fatalf("operator token resolved to human %q, want %q", got.ID, human.ID)
	}
	if _, err := s.EndpointByOAuthToken(ctx, "th-op-h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("operator token resolved through the endpoint path: err = %v, want ErrNotFound", err)
	}

	// Endpoint-bound token: resolves to the endpoint, never to a human.
	if _, err := s.CreateOAuthToken(ctx, "th-op-e", "rh-op-e", "cid-op-1", ep.ID, "", policy); err != nil {
		t.Fatalf("create endpoint-bound token: %v", err)
	}
	if _, err := s.EndpointByOAuthToken(ctx, "th-op-e"); err != nil {
		t.Fatalf("endpoint token must resolve: %v", err)
	}
	if _, err := s.HumanByOAuthToken(ctx, "th-op-e"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("endpoint token resolved through the human path: err = %v, want ErrNotFound", err)
	}
}

// TestOperatorGrantRotationPreservesHuman: refreshing an operator grant re-mints against the same
// human — rotation can never migrate a grant between principals.
func TestOperatorGrantRotationPreservesHuman(t *testing.T) {
	s, ctx := testStore(t)
	human, _ := oauthCodeFixture(t, s, ctx, "pocket|oprot", "op-rot", "hash-op-2", "op-rot-aa01", "cid-op-2")

	policy := time.Now().Add(time.Hour).UTC()
	if _, err := s.CreateOAuthToken(ctx, "th-op-rot", "rh-op-rot", "cid-op-2", "", human.ID, policy); err != nil {
		t.Fatalf("create human-bound token: %v", err)
	}
	tok, err := s.RotateOAuthToken(ctx, "rh-op-rot", "cid-op-2", "th-op-rot-2", "rh-op-rot-2", policy)
	if err != nil {
		t.Fatalf("rotate human-bound token: %v", err)
	}
	if tok.HumanID != human.ID || tok.EndpointID != "" {
		t.Fatalf("rotation changed the principal: endpoint=%q human=%q", tok.EndpointID, tok.HumanID)
	}
	if _, err := s.HumanByOAuthToken(ctx, "th-op-rot-2"); err != nil {
		t.Fatalf("rotated operator token does not resolve: %v", err)
	}
}
