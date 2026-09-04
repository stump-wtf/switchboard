package store

// OAuth token storage: opaque access/refresh pairs, hashed at rest, each bound to exactly one
// vended endpoint. Issuance clamps the access expiry to the endpoint's own expiry so an OAuth
// credential can never outlive the capability it exercises; refresh rotates (the old pair is
// revoked in the same transaction that mints the new one) and fails closed the moment the endpoint
// is revoked or expired. The resource-server lookup joins through the endpoint row's state, so a
// killed endpoint invalidates every token instantly even before the revocation cascade stamps the
// rows. Governing: ADR-0019, SPEC-0016 REQ "Token Issuance And Refresh", REQ "Resource-Server
// Token Validation", REQ "Revocation Cascade".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OAuthToken is an issued access/refresh pair as the token endpoint sees it: identifiers and
// clocks only — the row never holds (and this struct never carries) raw token material, only the
// caller-supplied hashes went to the database. Governing: SPEC-0016 ("Raw token values SHALL never
// be logged or stored").
type OAuthToken struct {
	ID         string
	ClientID   string
	EndpointID string // set on an agent grant; HumanID is empty
	HumanID    string // set on an operator grant; EndpointID is empty
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// CreateOAuthToken mints a token row. An agent grant (endpointID set) clamps the access expiry to
// the endpoint's own expires_at in the same statement — the endpoint read, the clamp, and the
// insert are one atomic write, so a revoke racing the exchange can never produce a token that
// outlives its endpoint, and a dead endpoint inserts nothing (ErrNotFound). An operator grant
// (humanID set) clamps nothing: the human is the principal and the grant lives until revoked.
// desiredExpiry is the token-endpoint policy expiry (now + access TTL). Governing: SPEC-0016 REQ
// "Token Issuance And Refresh" ("Access tokens SHALL carry an expiry no later than the endpoint's
// own expiry").
func (s *Store) CreateOAuthToken(ctx context.Context, tokenHash, refreshHash, clientID, endpointID, humanID string, desiredExpiry time.Time) (OAuthToken, error) {
	return createOAuthToken(ctx, s.pool, tokenHash, refreshHash, clientID, endpointID, humanID, desiredExpiry)
}

// createOAuthToken is the querier-based core of CreateOAuthToken, shared with the refresh
// rotation so the mint runs inside the rotation's transaction. Exactly one of endpointID/humanID
// is non-empty; the UNION's other branch matches no row, so the insert resolves to one principal
// row — the schema's num_nonnulls CHECK is the backstop.
func createOAuthToken(ctx context.Context, q querier, tokenHash, refreshHash, clientID, endpointID, humanID string, desiredExpiry time.Time) (OAuthToken, error) {
	var tok OAuthToken
	err := q.QueryRow(ctx, `
		INSERT INTO oauth_tokens (token_hash, refresh_hash, client_id, endpoint_id, human_id, expires_at)
		SELECT $1, $2, $3, e.id, NULL::uuid, LEAST($4::timestamptz, COALESCE(e.expires_at, $4::timestamptz))
		FROM endpoints e
		WHERE $5 = '' AND e.id = NULLIF($6, '')::uuid AND e.state = 'active'
		  AND (e.expires_at IS NULL OR e.expires_at > now())
		UNION ALL
		SELECT $1, $2, $3, NULL::uuid, hu.id, $4::timestamptz
		FROM humans hu
		WHERE $6 = '' AND hu.id = NULLIF($5, '')::uuid
		RETURNING id::text, client_id, COALESCE(endpoint_id::text, ''), COALESCE(human_id::text, ''), expires_at, created_at`,
		tokenHash, refreshHash, clientID, desiredExpiry, humanID, endpointID,
	).Scan(&tok.ID, &tok.ClientID, &tok.EndpointID, &tok.HumanID, &tok.ExpiresAt, &tok.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthToken{}, ErrNotFound
	}
	if err != nil {
		return OAuthToken{}, fmt.Errorf("store: create oauth token: %w", err)
	}
	return tok, nil
}

// RotateOAuthToken exchanges a live refresh token for a fresh access/refresh pair, atomically:
// in one transaction the presented refresh (matched by hash AND the presenting client) is revoked
// and the replacement is minted against the SAME endpoint under the same expiry clamp. Exactly one
// concurrent rotation can win — the revoking UPDATE takes the row lock, so a racing second
// presentation observes revoked_at set and reports ErrNotFound. A refresh whose endpoint has been
// revoked or expired also reports ErrNotFound, but only AFTER the old pair's revocation commits:
// a dead endpoint eats the refresh token rather than leaving it presentable. Governing: SPEC-0016
// REQ "Token Issuance And Refresh" ("refresh SHALL rotate (old refresh invalidated) and SHALL
// fail once the endpoint is revoked or expired"), scenario "Refresh after endpoint death".
func (s *Store) RotateOAuthToken(ctx context.Context, refreshHash, clientID, newTokenHash, newRefreshHash string, desiredExpiry time.Time) (OAuthToken, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OAuthToken{}, fmt.Errorf("store: rotate token begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed; rolls back on any early return

	var endpointID, humanID string
	err = tx.QueryRow(ctx, `
		UPDATE oauth_tokens SET revoked_at = now()
		WHERE refresh_hash = $1 AND client_id = $2 AND revoked_at IS NULL
		RETURNING COALESCE(endpoint_id::text, ''), COALESCE(human_id::text, '')`,
		refreshHash, clientID,
	).Scan(&endpointID, &humanID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, already rotated, or another client's refresh — uniformly not found, nothing spent.
		return OAuthToken{}, ErrNotFound
	}
	if err != nil {
		return OAuthToken{}, fmt.Errorf("store: rotate token revoke: %w", err)
	}

	tok, err := createOAuthToken(ctx, tx, newTokenHash, newRefreshHash, clientID, endpointID, humanID, desiredExpiry)
	if errors.Is(err, ErrNotFound) {
		// Endpoint dead: commit the revocation of the presented pair (refresh cannot recover
		// access, and the spent token must not remain presentable), then refuse the grant.
		if cerr := tx.Commit(ctx); cerr != nil {
			return OAuthToken{}, fmt.Errorf("store: rotate token commit: %w", cerr)
		}
		return OAuthToken{}, ErrNotFound
	}
	if err != nil {
		return OAuthToken{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OAuthToken{}, fmt.Errorf("store: rotate token commit: %w", err)
	}
	return tok, nil
}

// EndpointByOAuthToken resolves an active endpoint from a presented OAuth access-token hash — the
// resource server's OAuth twin of EndpointByCredHash, returning the SAME AuthEndpoint shape so
// everything downstream of "resolve to endpoint ID" (scope guard, sessions, doorbells) is
// byte-for-byte identical for both credential shapes. The lookup requires a live token (unexpired,
// unrevoked) AND a live endpoint (active, unexpired), stamping the token's last_used_at in the
// same statement; every failure mode is uniformly ErrNotFound. Governing: SPEC-0016 REQ
// "Resource-Server Token Validation", SPEC-0007 REQ "Instant, Total Revocation".
func (s *Store) EndpointByOAuthToken(ctx context.Context, tokenHash string) (AuthEndpoint, error) {
	var a AuthEndpoint
	err := s.pool.QueryRow(ctx, `
		UPDATE oauth_tokens t SET last_used_at = now()
		FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND t.expires_at > now()
		  AND e.id = t.endpoint_id AND e.state = 'active'
		  AND (e.expires_at IS NULL OR e.expires_at > now())
		RETURNING e.id::text, e.agent_id::text, ag.name, ag.owner_human_id::text, e.slug,
		          e.scope_queues, e.scope_verbs, e.webhook_max, e.webhook_source_types, e.webhook_queues`,
		tokenHash,
	).Scan(&a.ID, &a.AgentID, &a.AgentName, &a.OwnerHumanID, &a.Slug, &a.ScopeQueues, &a.ScopeVerbs,
		&a.WebhookMax, &a.WebhookSourceTypes, &a.WebhookQueues)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthEndpoint{}, ErrNotFound
	}
	if err != nil {
		return AuthEndpoint{}, fmt.Errorf("store: endpoint by oauth token: %w", err)
	}
	return a, nil
}

// HumanByOAuthToken resolves the human principal from a presented operator-grant access-token
// hash — the /api/v1 bearer resolution for tokens minted by an operator OAuth grant (the CLI's
// login, ADR-0023). Requires a live token (unexpired, unrevoked), stamping last_used_at in the
// same statement; every failure mode is uniformly ErrNotFound. Endpoint-bound tokens do not
// resolve here — the operator API is a human surface, never an agent one. Governing: ADR-0019
// (same token model), ADR-0023 (operator API rides it).
func (s *Store) HumanByOAuthToken(ctx context.Context, tokenHash string) (Human, error) {
	if s == nil || s.pool == nil {
		// A nil-backed Store (route-table tests) resolves nothing — fail closed.
		return Human{}, ErrNotFound
	}
	var h Human
	err := s.pool.QueryRow(ctx, `
		UPDATE oauth_tokens t SET last_used_at = now()
		FROM humans hu
		WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND t.expires_at > now()
		  AND hu.id = t.human_id
		RETURNING hu.id::text, hu.oidc_subject, hu.display_name, hu.email, hu.created_at`,
		tokenHash,
	).Scan(&h.ID, &h.OIDCSubject, &h.DisplayName, &h.Email, &h.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Human{}, ErrNotFound
	}
	if err != nil {
		return Human{}, fmt.Errorf("store: human by oauth token: %w", err)
	}
	return h, nil
}

// revokeEndpointOAuth is the OAuth half of the revocation cascade, run inside the SAME transaction
// that kills the endpoint rows (RevokeEndpoint, ExpireEndpoints): every unrevoked token on the
// endpoints is stamped revoked and every unspent code is force-expired, so one act kills every way
// of exercising the capability and consent history cannot resurrect access. The lookups already
// fail closed on endpoint state alone; this stamp makes the kill durable in the token rows
// themselves (visible to audits, immune to any future lookup that forgets the join). Governing:
// SPEC-0016 REQ "Revocation Cascade", SPEC-0007 ("revoke is instant and total").
func revokeEndpointOAuth(ctx context.Context, q querier, endpointIDs []string) error {
	if len(endpointIDs) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, `
		UPDATE oauth_tokens SET revoked_at = now()
		WHERE endpoint_id = ANY($1::uuid[]) AND revoked_at IS NULL`, endpointIDs); err != nil {
		return fmt.Errorf("store: revoke endpoint tokens: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE oauth_codes SET expires_at = now()
		WHERE endpoint_id = ANY($1::uuid[]) AND used_at IS NULL AND expires_at > now()`, endpointIDs); err != nil {
		return fmt.Errorf("store: expire endpoint codes: %w", err)
	}
	return nil
}
