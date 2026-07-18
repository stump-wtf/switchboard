package store

// OAuth authorization-server storage: dynamically registered clients (RFC 7591). Codes and tokens
// (the rest of the 0010_oauth schema) gain their store surface with the authorize/token stories;
// this file carries exactly what discovery + registration need.
// Governing: ADR-0019, SPEC-0016 REQ "Dynamic Client Registration".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

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
