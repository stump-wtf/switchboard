package store

// OAuth authorization-server storage: dynamically registered clients (RFC 7591), single-use
// PKCE-bound authorization codes, and the consent-time endpoint binding the authorize surface
// reads. Tokens (the remaining 0010_oauth table) gain their issuance surface with the token story;
// code REDEMPTION lives here because its replay semantics — a spent code revokes what it issued —
// are part of the consent contract, not the exchange mechanics.
// Governing: ADR-0019, SPEC-0016 REQ "Dynamic Client Registration", REQ "Authorization Code Flow
// With Consent".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrCodeReplayed reports a redemption attempt on an authorization code that was already spent.
// The store has ALREADY revoked the tokens minted from that code by the time this returns — the
// caller's only job is to refuse the grant. Governing: SPEC-0016 ("replayed codes SHALL be
// rejected and SHALL revoke tokens issued from that code").
var ErrCodeReplayed = errors.New("store: authorization code replayed")

// OAuthClient is a dynamically registered OAuth client (RFC 7591). Clients are public (PKCE, no
// client secret), so the row holds no secret material: client_id is an identifier, not a
// credential, and RedirectURIs is the exact-match allowlist enforced at registration and again at
// authorize time. Governing: ADR-0019, SPEC-0016 REQ "Dynamic Client Registration".
type OAuthClient struct {
	ID           string
	ClientID     string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// CreateOAuthClient persists a dynamically registered client. The caller mints the client_id
// (high-entropy, unique) and has already validated every redirect URI; the store only records.
func (s *Store) CreateOAuthClient(ctx context.Context, clientID, name string, redirectURIs []string) (OAuthClient, error) {
	var c OAuthClient
	err := s.pool.QueryRow(ctx, `
		INSERT INTO oauth_clients (client_id, name, redirect_uris)
		VALUES ($1, $2, $3)
		RETURNING id::text, client_id, name, redirect_uris, created_at`,
		clientID, name, redirectURIs,
	).Scan(&c.ID, &c.ClientID, &c.Name, &c.RedirectURIs, &c.CreatedAt)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("store: create oauth client: %w", err)
	}
	return c, nil
}

// OAuthClientByClientID resolves a registered client by its public client_id, or ErrNotFound. The
// authorize endpoint reads here to re-validate the presented redirect_uri against the registered
// exact-match allowlist. Governing: SPEC-0016 REQ "Dynamic Client Registration" (validated "again
// at authorization time").
func (s *Store) OAuthClientByClientID(ctx context.Context, clientID string) (OAuthClient, error) {
	var c OAuthClient
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, client_id, name, redirect_uris, created_at
		FROM oauth_clients WHERE client_id = $1`,
		clientID,
	).Scan(&c.ID, &c.ClientID, &c.Name, &c.RedirectURIs, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, fmt.Errorf("store: oauth client by client_id: %w", err)
	}
	return c, nil
}

// OAuthCode is a single-use, expiring, PKCE-bound authorization code — the durable consent record:
// its row exists because a human approved this client onto this endpoint, and it carries only the
// code's hash (the plaintext lives solely in the redirect back to the client). Governing:
// SPEC-0016 REQ "Authorization Code Flow With Consent", ADR-0019.
type OAuthCode struct {
	ID            string
	ClientID      string
	EndpointID    string // set on an agent grant (the MCP shape); HumanID is empty
	HumanID       string // set on an operator grant (the CLI/API shape); EndpointID is empty
	PKCEChallenge string
	RedirectURI   string
	ExpiresAt     time.Time
	UsedAt        *time.Time
}

// EndpointBySlugOwned resolves a LIVE (active, unexpired) vended endpoint by its public slug,
// strictly within the given human's ownership — the consent screen's binding read. A slug that is
// unknown, revoked, expired, or owned by another human is uniformly ErrNotFound: consent can only
// ever be granted by the endpoint's own accountable principal, and the error shape leaks nothing
// about which of those it was. Governing: SPEC-0016 REQ "Authorization Code Flow With Consent"
// (bind the request to one vended endpoint), SPEC-0007 REQ "Human as Accountable Principal".
func (s *Store) EndpointBySlugOwned(ctx context.Context, slug, ownerHumanID string) (EndpointCard, error) {
	var c EndpointCard
	err := s.pool.QueryRow(ctx, `
		SELECT e.id::text, e.agent_id::text, ag.name, e.slug, e.scope_queues, e.scope_verbs,
		       e.state, e.created_at, e.expires_at
		FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE e.slug = $1 AND ag.owner_human_id = $2 AND e.state = 'active'
		  AND (e.expires_at IS NULL OR e.expires_at > now())`,
		slug, ownerHumanID,
	).Scan(&c.ID, &c.AgentID, &c.AgentName, &c.Slug, &c.ScopeQueues, &c.ScopeVerbs,
		&c.State, &c.CreatedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return EndpointCard{}, ErrNotFound
	}
	if err != nil {
		return EndpointCard{}, fmt.Errorf("store: endpoint by slug owned: %w", err)
	}
	return c, nil
}

// CreateOAuthCode persists an approved consent as a single-use authorization code row. The caller
// (the consent POST) mints the plaintext and hands over only its hash; expiry is fixed at write
// time so a code can never be extended after the human said yes. The grant binds to exactly one
// principal: endpointID for an agent grant (the MCP shape) or humanID for an operator grant (the
// CLI/API shape) — the caller sets exactly one, mirroring the schema's CHECK constraint.
func (s *Store) CreateOAuthCode(ctx context.Context, codeHash, clientID, endpointID, humanID, pkceChallenge, redirectURI string, expiresAt time.Time) (OAuthCode, error) {
	var c OAuthCode
	err := s.pool.QueryRow(ctx, `
		INSERT INTO oauth_codes (code_hash, client_id, endpoint_id, human_id, pkce_challenge, redirect_uri, expires_at)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7)
		RETURNING id::text, client_id, COALESCE(endpoint_id::text, ''), COALESCE(human_id::text, ''), pkce_challenge, redirect_uri, expires_at, used_at`,
		codeHash, clientID, endpointID, humanID, pkceChallenge, redirectURI, expiresAt,
	).Scan(&c.ID, &c.ClientID, &c.EndpointID, &c.HumanID, &c.PKCEChallenge, &c.RedirectURI, &c.ExpiresAt, &c.UsedAt)
	if err != nil {
		return OAuthCode{}, fmt.Errorf("store: create oauth code: %w", err)
	}
	return c, nil
}

// RedeemOAuthCode spends an authorization code by hash, atomically marking it used and returning
// the grant the token exchange must verify (PKCE challenge, bound endpoint, exact redirect URI).
// One code, one redemption:
//
//   - live and unspent → used_at is stamped and the code returned; a concurrent second redemption
//     loses the UPDATE race and falls through to the replay branch.
//   - already spent → REPLAY. In the same transaction, every unrevoked token this client holds on
//     the code's endpoint is revoked — a replayed code means the plaintext leaked, so whatever it
//     bought must die with it — and ErrCodeReplayed is returned. (Tokens are keyed by
//     client+endpoint, the exact set a code's exchange can have minted; revoking that set is the
//     smallest stroke that provably covers "tokens issued from that code".)
//   - unknown or expired → ErrNotFound.
//
// Governing: SPEC-0016 REQ "Authorization Code Flow With Consent" ("Codes SHALL be ... marked used
// on redemption; replayed codes SHALL be rejected and SHALL revoke tokens issued from that code").
func (s *Store) RedeemOAuthCode(ctx context.Context, codeHash string) (OAuthCode, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OAuthCode{}, fmt.Errorf("store: redeem code begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed; rolls back on any early return

	var c OAuthCode
	err = tx.QueryRow(ctx, `
		UPDATE oauth_codes SET used_at = now()
		WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING id::text, client_id, COALESCE(endpoint_id::text, ''), COALESCE(human_id::text, ''), pkce_challenge, redirect_uri, expires_at, used_at`,
		codeHash,
	).Scan(&c.ID, &c.ClientID, &c.EndpointID, &c.HumanID, &c.PKCEChallenge, &c.RedirectURI, &c.ExpiresAt, &c.UsedAt)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return OAuthCode{}, fmt.Errorf("store: redeem code commit: %w", err)
		}
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OAuthCode{}, fmt.Errorf("store: redeem code: %w", err)
	}

	// No spendable row. Distinguish replay (row exists, already used) from unknown/expired, and on
	// replay revoke the grant's tokens before reporting it.
	var clientID, endpointID, humanID string
	var usedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT client_id, COALESCE(endpoint_id::text, ''), COALESCE(human_id::text, ''), used_at
		FROM oauth_codes WHERE code_hash = $1`,
		codeHash,
	).Scan(&clientID, &endpointID, &humanID, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthCode{}, ErrNotFound
	}
	if err != nil {
		return OAuthCode{}, fmt.Errorf("store: redeem code lookup: %w", err)
	}
	if usedAt == nil {
		// Present but not spendable with used_at NULL means it expired.
		return OAuthCode{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_tokens SET revoked_at = now()
		WHERE client_id = $1
		  AND COALESCE(endpoint_id::text, '') = $2 AND COALESCE(human_id::text, '') = $3
		  AND revoked_at IS NULL`,
		clientID, endpointID, humanID); err != nil {
		return OAuthCode{}, fmt.Errorf("store: revoke replayed grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return OAuthCode{}, fmt.Errorf("store: redeem code commit: %w", err)
	}
	return OAuthCode{}, ErrCodeReplayed
}
