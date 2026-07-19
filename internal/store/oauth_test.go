package store

// OAuth client storage tests (0010_oauth; ADR-0019, SPEC-0016 REQ "Dynamic Client Registration")
// plus schema assertions for the code/token tables the follow-on stories build on: hash lookups
// are unique-indexed and endpoint deletion cascades through every OAuth credential (the
// revocation-cascade substrate).

import (
	"errors"
	"strings"
	"testing"
)

func TestOAuthClientRoundTrip(t *testing.T) {
	s, ctx := testStore(t)

	uris := []string{"https://client.example.com/callback", "http://127.0.0.1:33418/cb"}
	c, err := s.CreateOAuthClient(ctx, "cid-abc123", "Claude Desktop", uris)
	if err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	if c.ClientID != "cid-abc123" || c.Name != "Claude Desktop" || c.ID == "" || c.CreatedAt.IsZero() {
		t.Fatalf("created client = %+v", c)
	}

	got, err := s.OAuthClientByClientID(ctx, "cid-abc123")
	if err != nil {
		t.Fatalf("client by client_id: %v", err)
	}
	// The registered redirect allowlist must round-trip byte-for-byte: authorize-time validation
	// is exact string matching against these values (SPEC-0016 "validated exactly").
	if len(got.RedirectURIs) != len(uris) {
		t.Fatalf("redirect_uris = %v, want %v", got.RedirectURIs, uris)
	}
	for i := range uris {
		if got.RedirectURIs[i] != uris[i] {
			t.Fatalf("redirect_uris[%d] = %q, want %q", i, got.RedirectURIs[i], uris[i])
		}
	}

	if _, err := s.OAuthClientByClientID(ctx, "cid-never-registered"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown client_id: err = %v, want ErrNotFound", err)
	}
}

// TestOAuthClientIDUnique: client_id is the public handle the whole flow keys on — the schema
// must refuse a duplicate rather than silently shadowing a registration.
func TestOAuthClientIDUnique(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateOAuthClient(ctx, "cid-dup", "one", []string{"https://a.example.com/cb"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateOAuthClient(ctx, "cid-dup", "two", []string{"https://b.example.com/cb"}); err == nil {
		t.Fatal("duplicate client_id must be rejected by the unique constraint")
	}
}

// TestOAuthSchemaHashLookupsUnique: the resource server resolves presented codes/tokens by hash on
// the hot path; those lookups must be unique-indexed (design.md "Storage (one migration)").
func TestOAuthSchemaHashLookupsUnique(t *testing.T) {
	s, ctx := testStore(t)
	for _, idx := range []string{"idx_oauth_codes_hash", "idx_oauth_tokens_hash", "idx_oauth_tokens_refresh"} {
		def := indexDef(t, s, ctx, idx)
		if !strings.Contains(strings.ToUpper(def), "UNIQUE") {
			t.Errorf("%s must be unique: %s", idx, def)
		}
	}
}

// TestOAuthEndpointCascade: deleting an endpoint row deletes its codes and tokens in the same
// stroke — the storage half of SPEC-0016 REQ "Revocation Cascade" ("revoke = instant and total",
// ADR-0008 doctrine).
func TestOAuthEndpointCascade(t *testing.T) {
	s, ctx := testStore(t)

	h, err := s.UpsertHuman(ctx, "pocket|oauth", "Joe", "")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	ag, err := s.CreateAgent(ctx, h.ID, "oauth-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, "hash-oauth-test", "sbk_test…", "oauth-bot-aa11bb22",
		[]string{"reviews"}, []string{"list_todos"})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if _, err := s.CreateOAuthClient(ctx, "cid-cascade", "c", []string{"https://c.example.com/cb"}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_codes (code_hash, client_id, endpoint_id, pkce_challenge, redirect_uri, expires_at)
		VALUES ('codehash', 'cid-cascade', $1, 'chal', 'https://c.example.com/cb', now() + interval '5 minutes')`,
		ep.ID); err != nil {
		t.Fatalf("insert code: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_tokens (token_hash, refresh_hash, client_id, endpoint_id, expires_at)
		VALUES ('tokhash', 'refhash', 'cid-cascade', $1, now() + interval '1 hour')`,
		ep.ID); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	if _, err := s.pool.Exec(ctx, `DELETE FROM endpoints WHERE id = $1`, ep.ID); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	var codes, tokens int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_codes`).Scan(&codes); err != nil {
		t.Fatalf("count codes: %v", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_tokens`).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if codes != 0 || tokens != 0 {
		t.Fatalf("endpoint deletion left %d codes and %d tokens; want 0/0 (revocation cascade)", codes, tokens)
	}
}
