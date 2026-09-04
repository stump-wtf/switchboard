package store

// Authorization-code storage tests (SPEC-0016 REQ "Authorization Code Flow With Consent"): codes
// are stored hashed and single-use; a replayed code is rejected AND revokes the tokens its grant
// issued; expiry makes a code unspendable; the consent-time endpoint binding is owner-scoped.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0019.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// oauthCodeFixture creates human + agent + endpoint + registered client and returns the pieces a
// code needs to hang off.
func oauthCodeFixture(t *testing.T, s *Store, ctx context.Context, subject, agentName, credHash, slug, clientID string) (Human, Endpoint) {
	t.Helper()
	h, err := s.UpsertHuman(ctx, subject, "Joe", "joe@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	ag, err := s.CreateAgent(ctx, h.ID, agentName, "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, credHash, "sbk_test…", slug,
		[]string{"github", "ci"}, []string{"list_todos", "claim", "complete"})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if _, err := s.CreateOAuthClient(ctx, clientID, "Claude Desktop", []string{"https://c.example.com/cb"}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	return h, ep
}

// TestOAuthCodeSingleUse: a live code redeems exactly once — the second redemption is the replay
// path, not a second grant.
func TestOAuthCodeSingleUse(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|code1", "code-bot", "hash-code-1", "code-bot-aa11", "cid-code-1")

	created, err := s.CreateOAuthCode(ctx, "codehash-1", "cid-code-1", ep.ID, "", "challenge-1",
		"https://c.example.com/cb", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	if created.UsedAt != nil {
		t.Fatal("freshly minted code must not be marked used")
	}

	got, err := s.RedeemOAuthCode(ctx, "codehash-1")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got.EndpointID != ep.ID || got.ClientID != "cid-code-1" ||
		got.PKCEChallenge != "challenge-1" || got.RedirectURI != "https://c.example.com/cb" {
		t.Fatalf("redeemed code = %+v", got)
	}
	if got.UsedAt == nil {
		t.Fatal("redemption must stamp used_at")
	}

	if _, err := s.RedeemOAuthCode(ctx, "codehash-1"); !errors.Is(err, ErrCodeReplayed) {
		t.Fatalf("second redemption: err = %v, want ErrCodeReplayed", err)
	}
}

// TestOAuthCodeReplayRevokesTokens pins the spec's replay teeth: replaying a spent code revokes
// the unrevoked tokens its grant (client × endpoint) issued — a replayed code means the plaintext
// leaked, so whatever it bought dies with it.
func TestOAuthCodeReplayRevokesTokens(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|code2", "replay-bot", "hash-code-2", "replay-bot-bb22", "cid-code-2")

	if _, err := s.CreateOAuthCode(ctx, "codehash-2", "cid-code-2", ep.ID, "", "chal",
		"https://c.example.com/cb", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("create code: %v", err)
	}
	if _, err := s.RedeemOAuthCode(ctx, "codehash-2"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	// The token the exchange minted from that code (issuance itself is the token story; the row
	// stands in for it here).
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_tokens (token_hash, refresh_hash, client_id, endpoint_id, expires_at)
		VALUES ('tok-replay', 'ref-replay', 'cid-code-2', $1, now() + interval '1 hour')`, ep.ID); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	if _, err := s.RedeemOAuthCode(ctx, "codehash-2"); !errors.Is(err, ErrCodeReplayed) {
		t.Fatalf("replay: err = %v, want ErrCodeReplayed", err)
	}
	var revoked *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT revoked_at FROM oauth_tokens WHERE token_hash = 'tok-replay'`).Scan(&revoked); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if revoked == nil {
		t.Fatal("replay must revoke the tokens issued from that grant")
	}
}

// TestOAuthCodeExpiryAndUnknown: an expired code is unspendable (ErrNotFound, not replay — it was
// never redeemed) and an unknown hash resolves to nothing.
func TestOAuthCodeExpiryAndUnknown(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|code3", "stale-bot", "hash-code-3", "stale-bot-cc33", "cid-code-3")

	if _, err := s.CreateOAuthCode(ctx, "codehash-3", "cid-code-3", ep.ID, "", "chal",
		"https://c.example.com/cb", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create code: %v", err)
	}
	if _, err := s.RedeemOAuthCode(ctx, "codehash-3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired code: err = %v, want ErrNotFound", err)
	}
	if _, err := s.RedeemOAuthCode(ctx, "codehash-never-minted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown code: err = %v, want ErrNotFound", err)
	}
}

// TestEndpointBySlugOwned: the consent-time binding read resolves only a LIVE endpoint within the
// asking human's ownership — foreign, revoked, and unknown slugs are uniformly ErrNotFound.
func TestEndpointBySlugOwned(t *testing.T) {
	s, ctx := testStore(t)
	owner, ep := oauthCodeFixture(t, s, ctx, "pocket|code4", "bind-bot", "hash-code-4", "bind-bot-dd44", "cid-code-4")

	got, err := s.EndpointBySlugOwned(ctx, "bind-bot-dd44", owner.ID)
	if err != nil {
		t.Fatalf("owned lookup: %v", err)
	}
	if got.ID != ep.ID || got.AgentName != "bind-bot" {
		t.Fatalf("bound endpoint = %+v", got)
	}
	// Scope columns round-trip — they are what the consent bullets derive from.
	if len(got.ScopeQueues) != 2 || len(got.ScopeVerbs) != 3 {
		t.Fatalf("scope = %v / %v", got.ScopeQueues, got.ScopeVerbs)
	}

	other, err := s.UpsertHuman(ctx, "pocket|other4", "Mallory", "")
	if err != nil {
		t.Fatalf("upsert other: %v", err)
	}
	if _, err := s.EndpointBySlugOwned(ctx, "bind-bot-dd44", other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign owner: err = %v, want ErrNotFound", err)
	}
	if _, err := s.EndpointBySlugOwned(ctx, "no-such-slug", owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug: err = %v, want ErrNotFound", err)
	}

	if err := s.RevokeEndpoint(ctx, ep.ID, owner.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.EndpointBySlugOwned(ctx, "bind-bot-dd44", owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked endpoint: err = %v, want ErrNotFound (consent cannot bind a dead line)", err)
	}
}
