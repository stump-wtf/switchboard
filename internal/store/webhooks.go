package store

// Self-managed webhook records: an agent creates/rotates/deletes its own ingestion webhooks through
// the vended MCP endpoint, bounded by the endpoint's ceiling. For a signed-type webhook switchboard
// MINTS the HMAC signing secret and HOLDS the plaintext here — it must recompute the provider HMAC
// over every inbound body to verify the delivery per SPEC-0003, which a one-way hash could not do.
// The plaintext is revealed to the agent exactly once (at create/rotate) so it can configure the
// producer; the ListWebhooks path deliberately never selects it. The count ceiling is enforced under
// a per-endpoint row lock (SELECT … FOR UPDATE on endpoints) inside CreateWebhook, so two concurrent
// creates at a full ceiling serialize behind that lock and can never both slip past.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling",
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", ADR-0012, ADR-0003.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrCeilingExceeded is returned by CreateWebhook when creating another webhook would exceed the
// endpoint's vended max count. It is a distinct sentinel (not ErrConflict) so the tool layer can map
// it to the stable `ceiling_exceeded` error code. Governing: SPEC-0006 REQ "Webhook Self-Management
// Within a Vended Ceiling" (create beyond the count ceiling is refused).
var ErrCeilingExceeded = errors.New("store: webhook ceiling exceeded")

// Webhook is one agent self-managed ingestion webhook. It carries metadata only — the minted signing
// secret is NOT a field here, so a Webhook value (as returned by CreateWebhook/RotateWebhookSecret/
// ListWebhooks) can never leak the secret. The plaintext secret is handled out-of-band: the tool
// layer supplies it to CreateWebhook/RotateWebhookSecret and reveals it once, and the delivery
// receiver reads it back through GetWebhookSecretByToken.
type Webhook struct {
	ID          string
	EndpointID  string
	SourceType  string
	TargetQueue string
	TrustMode   string // signed|token — derived by switchboard, never agent-supplied
	IngestToken string // non-secret URL routing token
	CreatedAt   time.Time
	RotatedAt   *time.Time // nil until the first rotate
}

// CreateWebhook inserts a webhook for an endpoint, enforcing the max-count ceiling under a
// per-endpoint row lock: the transaction first takes SELECT … FOR UPDATE on the endpoint row, then
// inserts and counts, rolling back with ErrCeilingExceeded if the new total would exceed max. The row
// lock is the serialization point — under PostgreSQL's default READ COMMITTED isolation an
// insert-then-count alone does NOT serialize concurrent creates (each transaction sees only its own
// uncommitted insert, so both could pass at the boundary). Locking the shared endpoint row forces
// concurrent creates for the same endpoint to run one at a time, matching the house FOR UPDATE
// pattern in todos.go and closing the check-then-act race (SPEC-0006 REQ "Concurrency Safety").
// secret is the minted HMAC signing secret switchboard holds so it can verify signed deliveries; it
// is stored in plaintext (an empty string persists NULL, for token/open webhooks that need none).
func (s *Store) CreateWebhook(ctx context.Context, endpointID, sourceType, targetQueue, trustMode, ingestToken, secret string, max int) (Webhook, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize concurrent creates for this endpoint behind an exclusive lock on the endpoint row.
	// Two simultaneous create_webhook calls at the ceiling boundary would each, under READ COMMITTED,
	// see only their own uncommitted insert and both pass a bare post-insert count; the row lock makes
	// the second wait until the first commits (and its insert is visible) or rolls back. A missing
	// endpoint id affects no row and surfaces as ErrNotFound. Governing: SPEC-0006 REQ "Concurrency
	// Safety" (check-then-act must serialize), matching todos.go's FOR UPDATE pattern.
	var lockedID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM endpoints WHERE id = $1 FOR UPDATE`, endpointID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook lock endpoint: %w", err)
	}

	// The CASE keeps the invariant "signing_secret is non-NULL iff trust_mode='signed'": switchboard
	// holds a secret only for a signed webhook (to recompute the HMAC); a token/open webhook is
	// authenticated by its unguessable ingest URL and its secret column stays NULL.
	var w Webhook
	err = tx.QueryRow(ctx, `
		INSERT INTO endpoint_webhooks (endpoint_id, source_type, target_queue, trust_mode, ingest_token, signing_secret)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $4 = 'signed' THEN NULLIF($6, '') ELSE NULL END)
		RETURNING id::text, endpoint_id::text, source_type, target_queue, trust_mode, ingest_token, created_at, rotated_at`,
		endpointID, sourceType, targetQueue, trustMode, ingestToken, secret,
	).Scan(&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode, &w.IngestToken, &w.CreatedAt, &w.RotatedAt)
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook insert: %w", err)
	}

	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM endpoint_webhooks WHERE endpoint_id = $1`, endpointID,
	).Scan(&count); err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook count: %w", err)
	}
	if count > max {
		// Roll back the speculative insert: this create would push the endpoint past its ceiling.
		return Webhook{}, ErrCeilingExceeded
	}
	if err := tx.Commit(ctx); err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook commit: %w", err)
	}
	return w, nil
}

// ListWebhooks returns an endpoint's self-managed webhooks, newest first. The signing secret is
// deliberately not selected. Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended
// Ceiling" (list returns metadata plus the ceiling, no secret values).
func (s *Store) ListWebhooks(ctx context.Context, endpointID string) ([]Webhook, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, endpoint_id::text, source_type, target_queue, trust_mode, ingest_token, created_at, rotated_at
		FROM endpoint_webhooks WHERE endpoint_id = $1 ORDER BY created_at DESC`, endpointID)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode, &w.IngestToken, &w.CreatedAt, &w.RotatedAt); err != nil {
			return nil, fmt.Errorf("store: list webhooks scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWebhookByToken resolves an inbound delivery's ingest token (the non-secret routing token carried
// in /webhooks/w/{token}) to exactly one webhook, or ErrNotFound if the token matches none. The
// unique index on ingest_token guarantees at most one row. Only metadata is returned; the receiver
// reads the signing secret separately through GetWebhookSecretByToken so the secret never travels
// on the metadata struct. Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and
// Idempotency" (delivery routes by token to one webhook), ADR-0012.
func (s *Store) GetWebhookByToken(ctx context.Context, ingestToken string) (Webhook, error) {
	var w Webhook
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, endpoint_id::text, source_type, target_queue, trust_mode, ingest_token, created_at, rotated_at
		FROM endpoint_webhooks WHERE ingest_token = $1`, ingestToken,
	).Scan(&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode, &w.IngestToken, &w.CreatedAt, &w.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: get webhook by token: %w", err)
	}
	return w, nil
}

// GetWebhookSecretByToken returns the webhook metadata plus the minted signing secret switchboard
// holds for it, resolving by the inbound ingest token. This is the delivery-path read: the
// /webhooks/w/{token} receiver needs the plaintext secret to recompute the provider HMAC and verify
// a signed delivery per SPEC-0003. A token/open webhook has no secret, so secret is "". An unknown
// token is ErrNotFound. Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and
// Idempotency" (switchboard holds the secret and verifies), SPEC-0003 (per-provider HMAC).
func (s *Store) GetWebhookSecretByToken(ctx context.Context, ingestToken string) (Webhook, string, error) {
	var w Webhook
	var secret *string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, endpoint_id::text, source_type, target_queue, trust_mode, ingest_token, created_at, rotated_at, signing_secret
		FROM endpoint_webhooks WHERE ingest_token = $1`, ingestToken,
	).Scan(&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode, &w.IngestToken, &w.CreatedAt, &w.RotatedAt, &secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, "", ErrNotFound
	}
	if err != nil {
		return Webhook{}, "", fmt.Errorf("store: get webhook secret by token: %w", err)
	}
	s2 := ""
	if secret != nil {
		s2 = *secret
	}
	return w, s2, nil
}

// RotateWebhookSecret stores a freshly minted signing secret and a new ingest token for a webhook the
// given endpoint owns, retiring the old secret/URL in a single conditional UPDATE. The endpoint_id
// guard in the WHERE clause is the ownership check: another endpoint's (or an unknown) id affects no
// rows and returns ErrNotFound. The secret is retained only for a signed webhook (the CASE keeps the
// invariant "signing_secret is non-NULL iff trust_mode='signed'"): a token/open webhook is
// authenticated by its unguessable ingest URL and its secret column stays NULL even though the caller
// always mints one. Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency"
// (rotate mints a new secret and retires the old), ADR-0003 (per-source trust model).
func (s *Store) RotateWebhookSecret(ctx context.Context, id, endpointID, newSecret, newIngestToken string) (Webhook, error) {
	var w Webhook
	err := s.pool.QueryRow(ctx, `
		UPDATE endpoint_webhooks
		SET signing_secret = CASE WHEN trust_mode = 'signed' THEN NULLIF($3, '') ELSE NULL END,
			ingest_token = $4, rotated_at = now()
		WHERE id = $1 AND endpoint_id = $2
		RETURNING id::text, endpoint_id::text, source_type, target_queue, trust_mode, ingest_token, created_at, rotated_at`,
		id, endpointID, newSecret, newIngestToken,
	).Scan(&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode, &w.IngestToken, &w.CreatedAt, &w.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: rotate webhook: %w", err)
	}
	return w, nil
}

// DeleteWebhook tears down a webhook the given endpoint owns. The endpoint_id guard is the ownership
// check: another endpoint's (or an unknown) id affects no rows and returns ErrNotFound.
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling" (delete tears down).
func (s *Store) DeleteWebhook(ctx context.Context, id, endpointID string) error {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM endpoint_webhooks WHERE id = $1 AND endpoint_id = $2`, id, endpointID)
	if err != nil {
		return fmt.Errorf("store: delete webhook: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
