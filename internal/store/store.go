// Package store is the raw-SQL data-access layer over PostgreSQL (ADR-0002): a thin set of typed
// functions, no ORM. It owns humans/sessions/agents/endpoints, the durable todo queue, and events.
package store

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// TodoTransitionHook observes committed todo lifecycle transitions for best-effort in-process
// fan-out (e.g. the web UI's SSE hub). verb is one of created|claimed|done|failed|pending
// (pending = a retry/requeue). Implementations MUST NOT block: delivery is presentation only —
// PostgreSQL remains the source of truth and a missed call costs nothing but a UI refresh.
// Governing: SPEC-0012 REQ "Live Updates via SSE" (best-effort presentation).
type TodoTransitionHook func(verb string, t Todo)

// Store wraps a pgx pool.
type Store struct {
	pool *pgxpool.Pool
	// todoHook is read on every todo transition and set (rarely) at wiring time; atomic so a
	// late SetTodoTransitionHook can never race in-flight transitions.
	todoHook atomic.Pointer[TodoTransitionHook]
}

// New builds a Store over the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SetTodoTransitionHook registers fn to observe committed todo transitions. Safe to call
// concurrently with store use; passing nil clears the hook.
func (s *Store) SetTodoTransitionHook(fn TodoTransitionHook) {
	if fn == nil {
		s.todoHook.Store(nil)
		return
	}
	s.todoHook.Store(&fn)
}

// fireTodoHook invokes the registered transition hook, if any. Called only after a transition has
// durably committed, so observers can never see a state the database does not.
func (s *Store) fireTodoHook(verb string, t Todo) {
	if fn := s.todoHook.Load(); fn != nil {
		(*fn)(verb, t)
	}
}

// Human is an OIDC-authenticated principal (Pocket ID subject). ADR-0008/011.
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
