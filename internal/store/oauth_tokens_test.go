package store

// OAuth token storage tests (SPEC-0016 REQ "Token Issuance And Refresh", REQ "Resource-Server
// Token Validation", REQ "Revocation Cascade"): issuance clamps to the endpoint's own expiry,
// rotation is atomic and single-winner, resolution fails closed on token AND endpoint state, and
// endpoint revocation/expiry kills every OAuth credential in the same transaction. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0019, SPEC-0007 ("revoke is instant and total").

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestOAuthTokenExpiryClampedToEndpoint: an endpoint carrying its own expires_at clamps every
// issued token to it; an endpoint without expiry issues at the requested policy expiry.
func TestOAuthTokenExpiryClampedToEndpoint(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|tok1", "clamp-bot", "hash-tok-1", "clamp-bot-aa01", "cid-tok-1")

	// No endpoint expiry: the policy expiry sticks. (Truncated to microseconds — timestamptz
	// resolution — so the round-trip compares exactly.)
	policy := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	tok, err := s.CreateOAuthToken(ctx, "th-clamp-a", "rh-clamp-a", "cid-tok-1", ep.ID, policy)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if got := tok.ExpiresAt.UTC(); !got.Equal(policy) {
		t.Fatalf("expiry = %v, want policy %v", got, policy)
	}
	if tok.EndpointID != ep.ID || tok.ClientID != "cid-tok-1" {
		t.Fatalf("token binding = %+v", tok)
	}

	// Endpoint expiry nearer than policy: the endpoint wins (SPEC-0016: "an expiry no later than
	// the endpoint's own expiry").
	epExpiry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Microsecond)
	if _, err := s.pool.Exec(ctx,
		`UPDATE endpoints SET expires_at = $1 WHERE id = $2`, epExpiry, ep.ID); err != nil {
		t.Fatalf("set endpoint expiry: %v", err)
	}
	tok, err = s.CreateOAuthToken(ctx, "th-clamp-b", "rh-clamp-b", "cid-tok-1", ep.ID, policy)
	if err != nil {
		t.Fatalf("create clamped token: %v", err)
	}
	if got := tok.ExpiresAt.UTC(); !got.Equal(epExpiry) {
		t.Fatalf("clamped expiry = %v, want endpoint expiry %v", got, epExpiry)
	}
}

// TestOAuthTokenIssuanceRefusesDeadEndpoint: a revoked or expired endpoint mints nothing —
// ErrNotFound, no row.
func TestOAuthTokenIssuanceRefusesDeadEndpoint(t *testing.T) {
	s, ctx := testStore(t)
	h, ep := oauthCodeFixture(t, s, ctx, "pocket|tok2", "dead-bot", "hash-tok-2", "dead-bot-aa02", "cid-tok-2")

	if err := s.RevokeEndpoint(ctx, ep.ID, h.ID); err != nil {
		t.Fatalf("revoke endpoint: %v", err)
	}
	if _, err := s.CreateOAuthToken(ctx, "th-dead", "rh-dead", "cid-tok-2", ep.ID,
		time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issuance on revoked endpoint: err = %v, want ErrNotFound", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_tokens WHERE endpoint_id = $1`, ep.ID).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 0 {
		t.Fatalf("token rows = %d, want 0", n)
	}
}

// TestEndpointByOAuthToken: a live token on a live endpoint resolves to the SAME AuthEndpoint
// shape the static bearer path returns (scope included) and stamps last_used_at; expired,
// revoked, and unknown tokens are uniformly ErrNotFound.
func TestEndpointByOAuthToken(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|tok3", "res-bot", "hash-tok-3", "res-bot-aa03", "cid-tok-3")

	if _, err := s.CreateOAuthToken(ctx, "th-res", "rh-res", "cid-tok-3", ep.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}

	a, err := s.EndpointByOAuthToken(ctx, "th-res")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if a.ID != ep.ID || a.Slug != ep.Slug {
		t.Fatalf("resolved endpoint = %+v, want %s/%s", a, ep.ID, ep.Slug)
	}
	// Identical downstream enforcement starts from identical scope columns.
	b, err := s.EndpointByCredHash(ctx, "hash-tok-3")
	if err != nil {
		t.Fatalf("resolve static bearer: %v", err)
	}
	if a.ID != b.ID || len(a.ScopeQueues) != len(b.ScopeQueues) || len(a.ScopeVerbs) != len(b.ScopeVerbs) {
		t.Fatalf("credential shapes disagree: oauth=%+v static=%+v", a, b)
	}

	var lastUsed *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT last_used_at FROM oauth_tokens WHERE token_hash = 'th-res'`).Scan(&lastUsed); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if lastUsed == nil {
		t.Fatal("resolution must stamp last_used_at")
	}

	// Expired token: dead even though the endpoint lives.
	if _, err := s.pool.Exec(ctx,
		`UPDATE oauth_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = 'th-res'`); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	if _, err := s.EndpointByOAuthToken(ctx, "th-res"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token: err = %v, want ErrNotFound", err)
	}
	if _, err := s.EndpointByOAuthToken(ctx, "th-never-issued"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: err = %v, want ErrNotFound", err)
	}
}

// TestOAuthTokenRotation: rotation revokes the presented pair and mints the replacement
// atomically; the old refresh (and the old access token) die, the new pair lives, and a
// second presentation of the spent refresh is refused.
func TestOAuthTokenRotation(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|tok4", "rot-bot", "hash-tok-4", "rot-bot-aa04", "cid-tok-4")

	if _, err := s.CreateOAuthToken(ctx, "th-rot-1", "rh-rot-1", "cid-tok-4", ep.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}

	tok, err := s.RotateOAuthToken(ctx, "rh-rot-1", "cid-tok-4", "th-rot-2", "rh-rot-2",
		time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if tok.EndpointID != ep.ID {
		t.Fatalf("rotated token endpoint = %s, want %s", tok.EndpointID, ep.ID)
	}

	// Old pair dead on both faces: the access hash no longer resolves, the refresh no longer rotates.
	if _, err := s.EndpointByOAuthToken(ctx, "th-rot-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old access after rotation: err = %v, want ErrNotFound", err)
	}
	if _, err := s.RotateOAuthToken(ctx, "rh-rot-1", "cid-tok-4", "th-x", "rh-x",
		time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("spent refresh re-presented: err = %v, want ErrNotFound", err)
	}
	// New pair lives; a foreign client cannot rotate it.
	if _, err := s.EndpointByOAuthToken(ctx, "th-rot-2"); err != nil {
		t.Fatalf("new access: %v", err)
	}
	if _, err := s.RotateOAuthToken(ctx, "rh-rot-2", "cid-other", "th-y", "rh-y",
		time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign-client rotation: err = %v, want ErrNotFound", err)
	}
}

// TestOAuthTokenRotationSingleWinner races concurrent rotations of ONE refresh token: exactly one
// wins; every loser reports ErrNotFound and mints nothing (the -race companion to the row-lock
// argument in RotateOAuthToken).
func TestOAuthTokenRotationSingleWinner(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|tok5", "race-bot", "hash-tok-5", "race-bot-aa05", "cid-tok-5")

	if _, err := s.CreateOAuthToken(ctx, "th-race", "rh-race", "cid-tok-5", ep.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	wins := make(chan string, racers)
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			suffix := string(rune('a' + i))
			tok, err := s.RotateOAuthToken(ctx, "rh-race", "cid-tok-5",
				"th-race-new-"+suffix, "rh-race-new-"+suffix, time.Now().Add(time.Hour))
			if err != nil {
				errs <- err
				return
			}
			wins <- tok.ID
		}()
	}
	wg.Wait()
	close(wins)
	close(errs)

	if n := len(wins); n != 1 {
		t.Fatalf("winners = %d, want exactly 1", n)
	}
	for err := range errs {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("loser error = %v, want ErrNotFound", err)
		}
	}
	var live int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM oauth_tokens WHERE endpoint_id = $1 AND revoked_at IS NULL`,
		ep.ID).Scan(&live); err != nil {
		t.Fatalf("count live tokens: %v", err)
	}
	if live != 1 {
		t.Fatalf("live tokens after race = %d, want 1", live)
	}
}

// TestRefreshFailsAfterEndpointRevocation is the SPEC-0016 scenario "Refresh after endpoint
// death" against real storage: revoking the endpoint refuses the refresh grant, mints nothing,
// and eats the presented refresh token.
func TestRefreshFailsAfterEndpointRevocation(t *testing.T) {
	s, ctx := testStore(t)
	h, ep := oauthCodeFixture(t, s, ctx, "pocket|tok6", "rev-bot", "hash-tok-6", "rev-bot-aa06", "cid-tok-6")

	if _, err := s.CreateOAuthToken(ctx, "th-rev", "rh-rev", "cid-tok-6", ep.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := s.RevokeEndpoint(ctx, ep.ID, h.ID); err != nil {
		t.Fatalf("revoke endpoint: %v", err)
	}
	if _, err := s.RotateOAuthToken(ctx, "rh-rev", "cid-tok-6", "th-rev-2", "rh-rev-2",
		time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refresh on revoked endpoint: err = %v, want ErrNotFound", err)
	}
	var live int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM oauth_tokens WHERE endpoint_id = $1 AND revoked_at IS NULL`,
		ep.ID).Scan(&live); err != nil {
		t.Fatalf("count live tokens: %v", err)
	}
	if live != 0 {
		t.Fatalf("live tokens after endpoint revocation = %d, want 0", live)
	}
}

// TestRevokeEndpointCascadesOAuth: RevokeEndpoint atomically kills the endpoint, its tokens, and
// its unspent codes — after the one call, the static bearer, the access token, the refresh grant,
// and the pending code are ALL dead. Governing: SPEC-0016 REQ "Revocation Cascade".
func TestRevokeEndpointCascadesOAuth(t *testing.T) {
	s, ctx := testStore(t)
	h, ep := oauthCodeFixture(t, s, ctx, "pocket|tok7", "casc-bot", "hash-tok-7", "casc-bot-aa07", "cid-tok-7")

	if _, err := s.CreateOAuthToken(ctx, "th-casc", "rh-casc", "cid-tok-7", ep.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, err := s.CreateOAuthCode(ctx, "ch-casc", "cid-tok-7", ep.ID, "chal",
		"https://c.example.com/cb", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("create code: %v", err)
	}

	if err := s.RevokeEndpoint(ctx, ep.ID, h.ID); err != nil {
		t.Fatalf("revoke endpoint: %v", err)
	}

	if _, err := s.EndpointByCredHash(ctx, "hash-tok-7"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("static bearer after revoke: err = %v, want ErrNotFound", err)
	}
	if _, err := s.EndpointByOAuthToken(ctx, "th-casc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access token after revoke: err = %v, want ErrNotFound", err)
	}
	if _, err := s.RedeemOAuthCode(ctx, "ch-casc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending code after revoke: err = %v, want ErrNotFound", err)
	}
	var revoked *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT revoked_at FROM oauth_tokens WHERE token_hash = 'th-casc'`).Scan(&revoked); err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if revoked == nil {
		t.Fatal("cascade must stamp revoked_at on the token row itself, not rely on the join alone")
	}
}

// TestExpireEndpointsCascadesOAuth: the reaper's expiry flip carries the same OAuth cascade as an
// operator revoke — tokens revoked, codes dead — because expiry IS revocation (SPEC-0016 REQ
// "Credential Lifetime" scenario "Expiry enforcement").
func TestExpireEndpointsCascadesOAuth(t *testing.T) {
	s, ctx := testStore(t)
	_, ep := oauthCodeFixture(t, s, ctx, "pocket|tok8", "exp-bot", "hash-tok-8", "exp-bot-aa08", "cid-tok-8")

	if _, err := s.CreateOAuthToken(ctx, "th-exp", "rh-exp", "cid-tok-8", ep.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE endpoints SET expires_at = now() - interval '1 second' WHERE id = $1`, ep.ID); err != nil {
		t.Fatalf("backdate endpoint expiry: %v", err)
	}

	ids, err := s.ExpireEndpoints(ctx)
	if err != nil {
		t.Fatalf("expire endpoints: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == ep.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expired ids = %v, want to include %s", ids, ep.ID)
	}

	if _, err := s.EndpointByOAuthToken(ctx, "th-exp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access token after expiry: err = %v, want ErrNotFound", err)
	}
	if _, err := s.RotateOAuthToken(ctx, "rh-exp", "cid-tok-8", "th-exp-2", "rh-exp-2",
		time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refresh after expiry: err = %v, want ErrNotFound", err)
	}
	var revoked *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT revoked_at FROM oauth_tokens WHERE token_hash = 'th-exp'`).Scan(&revoked); err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if revoked == nil {
		t.Fatal("expiry cascade must stamp revoked_at on the token row")
	}
}
