// Package store is the raw-SQL data-access layer over PostgreSQL (ADR-0002): a thin set of typed
// functions, no ORM. It owns humans/sessions/agents/endpoints, the durable todo queue, and events.
package store

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// TodoTransitionHook observes committed todo lifecycle transitions for best-effort in-process
// fan-out (e.g. the web UI's SSE hub). verb is one of created|claimed|done|failed|pending
// (pending = a retry/requeue/reaper re-surface). Implementations MUST NOT block: delivery is
// presentation only — PostgreSQL remains the source of truth and a missed call costs nothing but a
// UI refresh. Governing: SPEC-0012 REQ "Live Updates via SSE" (best-effort presentation).
type TodoTransitionHook func(verb string, t Todo)

// TodoDoorbellHook observes committed todo creations that are eligible for a channel push. The
// store applies the SPEC-0011 sender gate before firing: only todos persisted together with a
// VERIFIED delivery event ring the doorbell — per-source verification (ADR-0003) plus human
// ownership of the receiving endpoint (ADR-0008) are the "gate on the sender" the Channels
// standard requires. Implementations MUST NOT block: the push is only a doorbell, PostgreSQL
// remains the ledger, and a missed call costs latency, never work.
// Governing: SPEC-0011 REQ "Sender Gate and Injection Safety", ADR-0013.
type TodoDoorbellHook func(t Todo)

// EventHook observes newly committed inbound events (accepted deliveries) for best-effort
// in-process fan-out — the Board's live "incoming lines" feed. Duplicate deliveries (idempotent
// event dedup) do not fire. Same contract as TodoTransitionHook: never block, never authoritative.
// Governing: SPEC-0013 REQ "Board View — Live Incoming Lines".
type EventHook func(e EventSummary)

// EndpointSeenHook observes committed endpoint last-seen stamps (a vended credential just
// authenticated successfully) for best-effort in-process fan-out — the endpoint_seen typed SSE
// event. Same contract as TodoTransitionHook: never block, never authoritative.
// Governing: SPEC-0013 REQ "Live Updates and Toasts" (endpoint last-seen updates).
type EndpointSeenHook func(endpointID string, seenAt time.Time)

// SecretCipher seals and opens held secrets that switchboard must keep recoverable (it cannot hash
// a webhook signing secret it has to recompute HMACs with). It is satisfied by *cred.SecretBox.
// Decrypt returns a non-prefixed (legacy plaintext) value unchanged, so a store configured with a
// cipher still reads rows written before encryption was enabled. Governing: SPEC-0006 REQ
// "Switchboard Owns Secrets, Verification, and Idempotency".
type SecretCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(stored string) (string, error)
}

// Option configures a Store at construction. Kept variadic so New's signature stays stable for the
// many existing call sites that pass only a pool.
type Option func(*Store)

// WithSecretCipher enables at-rest encryption of held secrets (currently self-managed webhook signing
// secrets). Omit it (or pass nil) to keep the legacy plaintext behavior. Governing: SPEC-0006 REQ
// "Switchboard Owns Secrets, Verification, and Idempotency".
func WithSecretCipher(c SecretCipher) Option {
	return func(s *Store) { s.secretCipher = c }
}

// Store wraps a pgx pool.
type Store struct {
	pool *pgxpool.Pool
	// secretCipher, when set, encrypts held secrets at rest (webhook signing secrets). nil = plaintext.
	secretCipher SecretCipher
	// log, when set (WithLogger), carries security-relevant warnings about what was persisted —
	// currently the plaintext-signing-secret warning in secretwarn.go. Optional and nil-safe: the
	// store's data-access behaviour does not depend on it.
	log *slog.Logger
	// todoHook/eventHook/endpointSeenHook are read on every transition and set (rarely) at wiring
	// time; atomic so a late Set*Hook can never race in-flight transitions.
	todoHook         atomic.Pointer[TodoTransitionHook]
	eventHook        atomic.Pointer[EventHook]
	endpointSeenHook atomic.Pointer[EndpointSeenHook]
	// doorbellHook mirrors todoHook for push-eligible creations (the MCP channel doorbell).
	doorbellHook atomic.Pointer[TodoDoorbellHook]
	// metricsSink receives the SPEC-0023 REQ-3 lifecycle counters (metrics.go). Nil = no-op.
	metricsSink atomic.Pointer[Metrics]
}

// New builds a Store over the given pool, applying any options (e.g. WithSecretCipher).
func New(pool *pgxpool.Pool, opts ...Option) *Store {
	s := &Store{pool: pool}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

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

// SetEventHook registers fn to observe newly committed inbound events. Safe to call concurrently
// with store use; passing nil clears the hook.
func (s *Store) SetEventHook(fn EventHook) {
	if fn == nil {
		s.eventHook.Store(nil)
		return
	}
	s.eventHook.Store(&fn)
}

// fireEventHook invokes the registered event hook, if any. Called only after the event row has
// durably committed (post-Commit for transactional inserts).
func (s *Store) fireEventHook(e EventSummary) {
	if fn := s.eventHook.Load(); fn != nil {
		(*fn)(e)
	}
}

// SetEndpointSeenHook registers fn to observe committed endpoint last-seen stamps. Safe to call
// concurrently with store use; passing nil clears the hook.
func (s *Store) SetEndpointSeenHook(fn EndpointSeenHook) {
	if fn == nil {
		s.endpointSeenHook.Store(nil)
		return
	}
	s.endpointSeenHook.Store(&fn)
}

// fireEndpointSeenHook invokes the registered endpoint-seen hook, if any. Called only after the
// last_seen_at stamp has durably committed.
func (s *Store) fireEndpointSeenHook(endpointID string, seenAt time.Time) {
	if fn := s.endpointSeenHook.Load(); fn != nil {
		(*fn)(endpointID, seenAt)
	}
}

// SetTodoDoorbellHook registers fn to observe committed, push-eligible todo creations. Safe to
// call concurrently with store use; passing nil clears the hook.
func (s *Store) SetTodoDoorbellHook(fn TodoDoorbellHook) {
	if fn == nil {
		s.doorbellHook.Store(nil)
		return
	}
	s.doorbellHook.Store(&fn)
}

// fireDoorbell invokes the registered doorbell hook, if any. Callers fire it only after a durable
// commit AND only when the todo's delivery event passed per-source verification (the sender gate).
func (s *Store) fireDoorbell(t Todo) {
	// A quarantined todo never rings an ordinary doorbell, whatever path reaches here: the
	// SPEC-0011 sender gate is amended by SPEC-0026 REQ-6. Classifier doorbells (REQ-8) carry no todo.
	if t.Queue == QueueQuarantine {
		return
	}
	if fn := s.doorbellHook.Load(); fn != nil {
		(*fn)(t)
	}
}

// Human is an authenticated principal (provider subject: Pocket ID OIDC today,
// GitHub OAuth per ADR-0026). Issuer/ProviderSub are the session's recorded
// provenance (SPEC-0021 REQ "Session Parity and Provenance") — blank on a
// session created before provenance existed.
type Human struct {
	ID          string
	OIDCSubject string
	DisplayName string
	Email       string
	CreatedAt   time.Time
	Issuer      string
	ProviderSub string
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

// CreateSession records a server-side session (the caller stores the hash of the opaque cookie token)
// together with its login provenance: the trusted issuer and the provider-side subject
// (SPEC-0021 REQ "Session Parity and Provenance"). Both are empty for a session minted before
// provenance existed; a migration backfills nothing — provenance is recorded at establishment and
// only there.
func (s *Store) CreateSession(ctx context.Context, tokenHash, humanID string, ttl time.Duration, issuer, providerSub string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, human_id, expires_at, issuer, provider_sub) VALUES ($1, $2, now() + $3::interval, $4, $5)`,
		tokenHash, humanID, ttl.String(), issuer, providerSub,
	)
	return err
}

// SessionHuman returns the human for a live (unexpired) session, or ErrNotFound, along with the
// session's recorded provenance.
func (s *Store) SessionHuman(ctx context.Context, tokenHash string) (Human, error) {
	var h Human
	err := s.pool.QueryRow(ctx, `
		SELECT h.id::text, h.oidc_subject, COALESCE(h.display_name, ''), COALESCE(h.email, ''), h.created_at,
		       COALESCE(s.issuer, ''), COALESCE(s.provider_sub, '')
		FROM sessions s JOIN humans h ON h.id = s.human_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`,
		tokenHash,
	).Scan(&h.ID, &h.OIDCSubject, &h.DisplayName, &h.Email, &h.CreatedAt, &h.Issuer, &h.ProviderSub)
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
