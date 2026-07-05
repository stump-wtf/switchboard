// Package store is the raw-SQL data-access layer over PostgreSQL (ADR-002): a thin set of typed
// functions, no ORM. It owns humans/sessions/agents/endpoints, the durable todo queue, and events.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// Store wraps a pgx pool.
type Store struct{ pool *pgxpool.Pool }

// New builds a Store over the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Human is an OIDC-authenticated principal (Pocket ID subject). ADR-008/011.
type Human struct {
	ID          string
	OIDCSubject string
	DisplayName string
	Email       string
	CreatedAt   time.Time
}

// UpsertHuman inserts or updates a human keyed on the OIDC subject, returning the current row.
func (s *Store) UpsertHuman(ctx context.Context, subject, displayName, email string) (Human, error) {
	var h Human
	err := s.pool.QueryRow(ctx, `
		INSERT INTO humans (oidc_subject, display_name, email)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''))
		ON CONFLICT (oidc_subject) DO UPDATE
			SET display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), humans.display_name),
			    email        = COALESCE(NULLIF(EXCLUDED.email, ''), humans.email)
		RETURNING id::text, oidc_subject, COALESCE(display_name, ''), COALESCE(email, ''), created_at`,
		subject, displayName, email,
	).Scan(&h.ID, &h.OIDCSubject, &h.DisplayName, &h.Email, &h.CreatedAt)
	return h, err
}

// CreateSession records a server-side session (the caller stores the hash of the opaque cookie token).
func (s *Store) CreateSession(ctx context.Context, tokenHash, humanID string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, human_id, expires_at) VALUES ($1, $2, now() + $3::interval)`,
		tokenHash, humanID, ttl.String(),
	)
	return err
}

// SessionHuman returns the human for a live (unexpired) session, or ErrNotFound.
func (s *Store) SessionHuman(ctx context.Context, tokenHash string) (Human, error) {
	var h Human
	err := s.pool.QueryRow(ctx, `
		SELECT h.id::text, h.oidc_subject, COALESCE(h.display_name, ''), COALESCE(h.email, ''), h.created_at
		FROM sessions s JOIN humans h ON h.id = s.human_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`,
		tokenHash,
	).Scan(&h.ID, &h.OIDCSubject, &h.DisplayName, &h.Email, &h.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Human{}, ErrNotFound
	}
	return h, err
}

// DeleteSession removes a session (logout / revoke).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}
