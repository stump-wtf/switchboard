package store

// Outbound notify hooks (SPEC-0024): an endpoint registers an HTTPS URL, and Switchboard POSTs a
// signed, payload-free notification there when a push-eligible todo it owns becomes ready.
//
// Ownership: a hook row carries only endpoint_id, and inherits that endpoint's owner scope. Every
// method here therefore takes the caller's endpointID and puts it in the WHERE clause, so another
// endpoint's hook id, including one on another endpoint of the same human, is ErrNotFound, exactly
// like an unknown id (REQ-2 "Another endpoint's hook id").
//
// Secrets: a hook secret is written ONLY through the SecretCipher envelope. Unlike the inbound
// webhook secret, which falls back to plaintext with a warning when no key is configured, a notify
// hook refuses to exist without SWITCHBOARD_SECRET_ENCRYPTION_KEY (ErrSecretCipherRequired), and a
// stored value that is not enc:v1: ciphertext is refused on read. NotifyHook carries no secret
// field, so no list or get path can leak one; the only read of plaintext is NotifyHookSigningSecrets,
// for the dispatcher.
//
// Governing: ADR-0029, SPEC-0024 REQ-1 "Hook Ownership and Scope", REQ-2 "Management Verbs", REQ-4
// "Signing (Standard Webhooks)", REQ-13 "Concurrency and Database Standards".

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSecretCipherRequired is returned when a notify hook secret would be written or read with no
// SecretCipher configured. Hooks fail closed: no key, no hook (SPEC-0024 REQ-4, the at-rest rule).
var ErrSecretCipherRequired = errors.New("store: notify hooks require SWITCHBOARD_SECRET_ENCRYPTION_KEY")

// ErrSecretNotSealed is returned when a stored hook secret is not envelope ciphertext. Nothing in
// this package writes one, so it means the row was tampered with or written outside the store.
var ErrSecretNotSealed = errors.New("store: notify hook secret is not sealed")

// MaxNotifyHookURLLen mirrors the column CHECK (SPEC-0024 REQ-3: at most 2048 bytes).
const MaxNotifyHookURLLen = 2048

// sealedSecretPrefix is the internal/cred envelope marker; a hook secret must always carry it.
const sealedSecretPrefix = "enc:v1:"

// Hook disabled reasons (the column CHECK).
const (
	NotifyHookDisabledFailures = "consecutive_failures"
	NotifyHookDisabledOperator = "operator"
)

// NotifyHook is one hook's metadata and health. It has no secret field by construction.
type NotifyHook struct {
	ID                  string
	EndpointID          string
	URL                 string
	Queues              []string // empty = every queue the endpoint's scope grants, at fire time
	IgnorePresence      bool
	Enabled             bool
	DisabledReason      *string
	DisabledAt          *time.Time
	ConsecutiveFailures int
	LastAttemptAt       *time.Time
	LastStatus          *int
	LastError           *string
	CreatedAt           time.Time
	RotatedAt           *time.Time
	// PrevSecretExpiresAt is when the dual-signing grace after the last rotation ends; nil when no
	// previous secret is held.
	PrevSecretExpiresAt *time.Time
}

// NotifyHookSecrets is the plaintext signing material for one hook, for the dispatcher only.
// Previous is "" unless a rotation grace is still running.
type NotifyHookSecrets struct {
	Current           string
	Previous          string
	PreviousExpiresAt *time.Time
}

const notifyHookColumns = `id::text, endpoint_id::text, url, queues, ignore_presence, enabled, disabled_reason,
	disabled_at, consecutive_failures, last_attempt_at, last_status, last_error, created_at, rotated_at,
	prev_secret_expires_at`

func scanNotifyHook(row pgx.Row) (NotifyHook, error) {
	var h NotifyHook
	err := row.Scan(&h.ID, &h.EndpointID, &h.URL, &h.Queues, &h.IgnorePresence, &h.Enabled, &h.DisabledReason,
		&h.DisabledAt, &h.ConsecutiveFailures, &h.LastAttemptAt, &h.LastStatus, &h.LastError, &h.CreatedAt,
		&h.RotatedAt, &h.PrevSecretExpiresAt)
	if h.Queues == nil {
		h.Queues = []string{}
	}
	return h, err
}

// sealHookSecret seals a hook secret, refusing to store one without a cipher or an empty one.
func (s *Store) sealHookSecret(secret string) (string, error) {
	if s.secretCipher == nil {
		return "", ErrSecretCipherRequired
	}
	if secret == "" {
		return "", errors.New("store: notify hook secret is empty")
	}
	sealed, err := s.secretCipher.Encrypt(secret)
	if err != nil {
		return "", fmt.Errorf("store: seal notify hook secret: %w", err)
	}
	return sealed, nil
}

// openHookSecret opens a sealed hook secret. Unlike openSecret it never passes plaintext through:
// a hook secret was sealed on every write, so an unsealed value is refused.
func (s *Store) openHookSecret(stored string) (string, error) {
	if s.secretCipher == nil {
		return "", ErrSecretCipherRequired
	}
	if !strings.HasPrefix(stored, sealedSecretPrefix) {
		return "", ErrSecretNotSealed
	}
	plaintext, err := s.secretCipher.Decrypt(stored)
	if err != nil {
		return "", fmt.Errorf("store: open notify hook secret: %w", err)
	}
	return plaintext, nil
}

// notifyHookCeilingErr names the notify-hook resource in the error text while still matching
// errors.Is(err, ErrCeilingExceeded), whose own message says "webhook" (it predates notify hooks).
func notifyHookCeilingErr(max int) error {
	return fmt.Errorf("store: notify hook ceiling (%d) reached: %w", max, ErrCeilingExceeded)
}

// CreateNotifyHook inserts a hook for an endpoint, enforcing the per-endpoint ceiling in the same
// transaction: it locks the endpoint row (SELECT … FOR UPDATE), inserts, counts, and rolls back with
// ErrCeilingExceeded when the count passes max, so two racing creates at the boundary serialize and
// exactly one succeeds (REQ-13 "Concurrent creates at the ceiling"). max <= 0 refuses every create
// (the operator kill switch, REQ-1). Nothing is persisted on any error. secret is the minted
// Standard Webhooks secret; it is sealed before it reaches the statement. URL and queue validation
// belong to the caller (the verb layer runs the SSRF validator); the store enforces only the length.
func (s *Store) CreateNotifyHook(ctx context.Context, endpointID, url string, queues []string, ignorePresence bool, secret string, max int) (NotifyHook, error) {
	if max <= 0 {
		return NotifyHook{}, notifyHookCeilingErr(max)
	}
	if url == "" || len(url) > MaxNotifyHookURLLen {
		return NotifyHook{}, fmt.Errorf("store: notify hook url must be 1..%d bytes", MaxNotifyHookURLLen)
	}
	sealed, err := s.sealHookSecret(secret)
	if err != nil {
		return NotifyHook{}, err
	}
	if queues == nil {
		queues = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return NotifyHook{}, fmt.Errorf("store: create notify hook begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize creates for this endpoint behind its row lock, the house pattern from CreateWebhook:
	// under READ COMMITTED a bare insert-then-count lets two racing creates both pass.
	var lockedID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM endpoints WHERE id = $1 FOR UPDATE`, endpointID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotifyHook{}, ErrNotFound
	}
	if err != nil {
		return NotifyHook{}, fmt.Errorf("store: create notify hook lock endpoint: %w", err)
	}

	h, err := scanNotifyHook(tx.QueryRow(ctx, `
		INSERT INTO notify_hooks (endpoint_id, url, secret, queues, ignore_presence)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+notifyHookColumns,
		endpointID, url, sealed, queues, ignorePresence))
	if err != nil {
		return NotifyHook{}, fmt.Errorf("store: create notify hook insert: %w", err)
	}

	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM notify_hooks WHERE endpoint_id = $1`, endpointID).Scan(&count); err != nil {
		return NotifyHook{}, fmt.Errorf("store: create notify hook count: %w", err)
	}
	if count > max {
		return NotifyHook{}, notifyHookCeilingErr(max)
	}
	if err := tx.Commit(ctx); err != nil {
		return NotifyHook{}, fmt.Errorf("store: create notify hook commit: %w", err)
	}
	return h, nil
}

// ListNotifyHooks returns an endpoint's hooks, oldest first, with health and no secret.
func (s *Store) ListNotifyHooks(ctx context.Context, endpointID string) ([]NotifyHook, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+notifyHookColumns+`
		FROM notify_hooks WHERE endpoint_id = $1 ORDER BY created_at, id`, endpointID)
	if err != nil {
		return nil, fmt.Errorf("store: list notify hooks: %w", err)
	}
	defer rows.Close()
	out := []NotifyHook{}
	for rows.Next() {
		h, err := scanNotifyHook(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list notify hooks scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetNotifyHook returns one hook the endpoint owns, or ErrNotFound.
func (s *Store) GetNotifyHook(ctx context.Context, id, endpointID string) (NotifyHook, error) {
	h, err := scanNotifyHook(s.pool.QueryRow(ctx, `SELECT `+notifyHookColumns+`
		FROM notify_hooks WHERE id = $1 AND endpoint_id = $2`, id, endpointID))
	if errors.Is(err, pgx.ErrNoRows) {
		return NotifyHook{}, ErrNotFound
	}
	if err != nil {
		return NotifyHook{}, fmt.Errorf("store: get notify hook %s: %w", id, err)
	}
	return h, nil
}

// RotateNotifyHookSecret installs a new secret for a hook the endpoint owns, keeping the current
// one as prev_secret until now()+grace so every attempt in the grace is dual-signed (REQ-4). A hook
// that REQ-8 auto-disabled (consecutive_failures) is re-enabled with its failure count reset; an
// operator disable is the human's call and survives a rotation. One conditional UPDATE: another
// endpoint's or an unknown id affects no row and is ErrNotFound.
//
// The store trusts its caller for both inputs, as CreateNotifyHook does. grace is the
// rotate_notify_hook verb's fixed 24h (notifyhook.RotationGrace, #354); a non-positive grace ends
// the dual-signing at once. The secret's Standard Webhooks format is guaranteed by
// notifyhook.MintSecret, its only producer; the store cannot re-check it with
// notifyhook.DecodeSecret, because the dispatcher in package notifyhook (#358) imports store.
func (s *Store) RotateNotifyHookSecret(ctx context.Context, id, endpointID, newSecret string, grace time.Duration) (NotifyHook, error) {
	sealed, err := s.sealHookSecret(newSecret)
	if err != nil {
		return NotifyHook{}, err
	}
	h, err := scanNotifyHook(s.pool.QueryRow(ctx, `
		UPDATE notify_hooks
		   SET prev_secret = secret,
		       prev_secret_expires_at = now() + make_interval(secs => $4),
		       secret = $3,
		       rotated_at = now(),
		       enabled = CASE WHEN disabled_reason = 'consecutive_failures' THEN true ELSE enabled END,
		       consecutive_failures = CASE WHEN disabled_reason = 'consecutive_failures' THEN 0 ELSE consecutive_failures END,
		       disabled_at = CASE WHEN disabled_reason = 'consecutive_failures' THEN NULL ELSE disabled_at END,
		       disabled_reason = CASE WHEN disabled_reason = 'consecutive_failures' THEN NULL ELSE disabled_reason END
		 WHERE id = $1 AND endpoint_id = $2
		RETURNING `+notifyHookColumns,
		id, endpointID, sealed, grace.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return NotifyHook{}, ErrNotFound
	}
	if err != nil {
		return NotifyHook{}, fmt.Errorf("store: rotate notify hook %s: %w", id, err)
	}
	return h, nil
}

// DeleteNotifyHook removes a hook the endpoint owns, with both its secrets, or returns ErrNotFound.
func (s *Store) DeleteNotifyHook(ctx context.Context, id, endpointID string) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM notify_hooks WHERE id = $1 AND endpoint_id = $2`, id, endpointID)
	if err != nil {
		return fmt.Errorf("store: delete notify hook %s: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// NotifyHookSigningSecrets returns the plaintext signing secrets of a hook the endpoint owns, for
// the dispatcher's signer and nothing else. A previous secret past its grace is not returned (and
// DestroyExpiredNotifyHookSecrets removes it from the row). The values must never be logged.
func (s *Store) NotifyHookSigningSecrets(ctx context.Context, id, endpointID string) (NotifyHookSecrets, error) {
	if s.secretCipher == nil {
		return NotifyHookSecrets{}, ErrSecretCipherRequired
	}
	var cur string
	var prev *string
	var prevExp *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT secret,
		       CASE WHEN prev_secret_expires_at > now() THEN prev_secret END,
		       CASE WHEN prev_secret_expires_at > now() THEN prev_secret_expires_at END
		  FROM notify_hooks WHERE id = $1 AND endpoint_id = $2`, id, endpointID).Scan(&cur, &prev, &prevExp)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotifyHookSecrets{}, ErrNotFound
	}
	if err != nil {
		return NotifyHookSecrets{}, fmt.Errorf("store: read notify hook %s secrets: %w", id, err)
	}
	out := NotifyHookSecrets{}
	if out.Current, err = s.openHookSecret(cur); err != nil {
		return NotifyHookSecrets{}, fmt.Errorf("store: notify hook %s current secret: %w", id, err)
	}
	if prev != nil {
		if out.Previous, err = s.openHookSecret(*prev); err != nil {
			return NotifyHookSecrets{}, fmt.Errorf("store: notify hook %s previous secret: %w", id, err)
		}
		out.PreviousExpiresAt = prevExp
	}
	return out, nil
}

// DestroyExpiredNotifyHookSecrets clears every previous secret whose rotation grace has ended, so an
// old secret is destroyed rather than merely ignored (REQ-4). One statement; safe to run from any
// number of instances. Returns the number of hooks cleared.
func (s *Store) DestroyExpiredNotifyHookSecrets(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `
		UPDATE notify_hooks SET prev_secret = NULL, prev_secret_expires_at = NULL
		 WHERE prev_secret IS NOT NULL AND prev_secret_expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: destroy expired notify hook secrets: %w", err)
	}
	return ct.RowsAffected(), nil
}
