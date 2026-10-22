package store

// Self-managed webhook records: an agent creates/rotates/deletes its own ingestion webhooks through
// the vended MCP endpoint, bounded by the endpoint's ceiling. For a signed webhook whose secret
// switchboard mints, that secret is generated here and held in plaintext — it must recompute the
// provider HMAC over every inbound body to verify the delivery per SPEC-0003, which a one-way hash
// could not do. A provider-origin webhook (stripe, slack) instead holds the value its owner pasted,
// sealed under the same envelope (SPEC-0032). The minted plaintext is revealed to the agent exactly
// once (at create/rotate) so it can configure the producer; ListWebhooks selects the stored secret
// only to fingerprint it and never returns it. The count ceiling is enforced under a per-endpoint row
// lock (SELECT … FOR UPDATE on endpoints) inside CreateWebhook, so two concurrent creates at a full
// ceiling serialize behind that lock and can never both slip past.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling",
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", ADR-0012, ADR-0003, ADR-0037.
//
// @joestump-agent 10/21/2026 - secret_supplied's doc now says it marks any caller-supplied secret,
// provider kinds included, which is what the column and rotate_webhook actually do; the TL;DR now
// reflects SPEC-0032's provider-origin rows and ListWebhooks' fingerprint-only secret read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/sourcekind"
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
	// TrustedActors is the stored trust list (JSON; routing.DecodeTrustedActors reads it). It is
	// always present on github, gitea and cairn webhooks (the trusted_actors_required CHECK) and nil
	// elsewhere. Governing: SPEC-0026 REQ-5.
	TrustedActors []byte
	// SecretOrigin is how this webhook's signing secret originates: minted, provider or none
	// (SPEC-0032 REQ-1). It is read off the row rather than recomputed from the source-type table, so
	// a webhook keeps answering with what was decided when it was written even if the table changes.
	SecretOrigin string
	// SecretSet reports whether a signing secret is actually stored. A provider-origin signed webhook
	// with none is awaiting its secret: deliveries are refused with 503 (SPEC-0032 REQ-2).
	SecretSet bool
	// SecretFingerprint is the first 12 hex of the stored secret's SHA-256 and SecretSetAt when it was
	// written; both are empty/nil until a secret exists. Neither is the secret, which is the point of
	// reporting them (SPEC-0032 REQ-2, REQ-3).
	SecretFingerprint string
	SecretSetAt       *time.Time
	// SecretSupplied says the stored signing secret came from its caller: always for a provider-origin
	// row (the provider signs with a value only it knows), and for a minted-origin row only under the
	// operator opt-in (SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET, SPEC-0032 REQ-1). rotate_webhook
	// mints a fresh secret and clears it.
	SecretSupplied bool
	// SecretSetByKind/SecretSetByID name the actor behind the latest audited secret write, for
	// list_webhooks' secret_set_by (SPEC-0032 REQ-13). Both are empty while no caller-supplied secret
	// has ever been written for the webhook.
	SecretSetByKind string
	SecretSetByID   string
}

// webhookRowColumns are the columns a webhook read returns, in scan order: the row's identity and
// routing shape, then the SPEC-0032 secret fields. One list rather than five hand-copied SELECT lists,
// because a column spelled into four of them is a field that silently reads empty on the fifth.
const webhookRowColumns = `id::text, endpoint_id::text, source_type, target_queue, trust_mode,
		ingest_token, created_at, rotated_at, trusted_actors,
		secret_origin, secret_supplied, secret_set_at`

// scanTargets returns the destinations matching webhookRowColumns, in order.
func (w *Webhook) scanTargets() []any {
	return []any{&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode,
		&w.IngestToken, &w.CreatedAt, &w.RotatedAt, &w.TrustedActors,
		&w.SecretOrigin, &w.SecretSupplied, &w.SecretSetAt}
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
	return s.CreateWebhookWithTrust(ctx, endpointID, sourceType, targetQueue, trustMode, ingestToken, secret, max, nil)
}

// CreateWebhookWithTrust is CreateWebhook with the webhook's trust list. A nil trustedActors stores
// the source's empty list on github, gitea and cairn (which trusts no one: new webhooks fail closed)
// and NULL on other sources. The secret is switchboard's own — minted for a minted-origin source,
// none for the rest — so no audit row is written; see CreateWebhookWithOrigin for a secret the caller
// supplied. Governing: SPEC-0026 REQ-5 ("omitting it MUST store an empty list").
func (s *Store) CreateWebhookWithTrust(ctx context.Context, endpointID, sourceType, targetQueue, trustMode, ingestToken, secret string, max int, trustedActors []byte) (Webhook, error) {
	return s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: endpointID, SourceType: sourceType, TargetQueue: targetQueue, TrustMode: trustMode,
		IngestToken: ingestToken, Secret: secret, Max: max, TrustedActors: trustedActors,
	})
}

// CreateWebhookInput is one webhook insert, spelled as fields rather than eleven positional
// arguments now that the secret's provenance matters as much as its value.
type CreateWebhookInput struct {
	EndpointID    string
	SourceType    string
	TargetQueue   string
	TrustMode     string // switchboard-derived: signed|token, never caller-supplied
	IngestToken   string
	Secret        string // plaintext; empty stores NULL. Sealed here, never by the caller.
	Max           int
	TrustedActors []byte
	// SecretSupplied says Secret came from the caller rather than from switchboard's mint. It is what
	// makes the write auditable: a supplied secret needs an at-rest key, sets secret_supplied, and
	// appends an audit row; a minted one does none of that (SPEC-0032 REQ-1, REQ-3, REQ-13).
	SecretSupplied bool
	// Actor is who is writing, recorded on the audit row. Ignored unless SecretSupplied.
	Actor SecretActor
}

// CreateWebhookWithOrigin is the full webhook insert: it derives the row's secret_origin from the
// SPEC-0032 source table, stores the secret under the same envelope the minted path uses, and — for a
// secret the caller supplied — requires an at-rest key and appends the audit row inside this same
// transaction.
//
// A provider-origin insert with no secret is the expected shape, not an error: the row is created with
// a NULL secret, which is what `awaiting_secret` means, and the first delivery answers 503 until its
// owner sets the provider-issued value (SPEC-0032 REQ-2).
//
// The ceiling is still enforced under the endpoint row lock, exactly as CreateWebhook documented: an
// insert-then-count under READ COMMITTED does not serialize two concurrent creates, so both would
// pass at the boundary.
//
// Governing: SPEC-0032 REQ-1, REQ-2, REQ-3, REQ-13; SPEC-0006 REQ "Webhook Self-Management Within a
// Vended Ceiling" (ceiling, concurrency), SPEC-0026 REQ-5 (trust list).
func (s *Store) CreateWebhookWithOrigin(ctx context.Context, in CreateWebhookInput) (Webhook, error) {
	sourceType, targetQueue, trustMode, ingestToken, secret, max := in.SourceType, in.TargetQueue, in.TrustMode, in.IngestToken, in.Secret, in.Max
	if err := CheckQueueNames(targetQueue); err != nil {
		return Webhook{}, err // the reserved quarantine queue is never a target (SPEC-0026 REQ-6)
	}
	if in.TrustedActors == nil {
		if empty, ok := routing.DefaultTrustedActors(sourceType); ok {
			raw, err := json.Marshal(empty)
			if err != nil {
				return Webhook{}, fmt.Errorf("store: create webhook trust list: %w", err)
			}
			in.TrustedActors = raw
		}
	}
	trustedActors := in.TrustedActors
	origin := sourcekind.OriginOf(sourceType, trustMode)

	// A secret somebody else issued is the one value switchboard cannot regenerate, so it is not
	// stored in the clear the way a minted one is allowed to be. Both rules are enforced here rather
	// than at one call site: the shape because a paste error is otherwise invisible until every
	// delivery 401s, the key because a provider-issued value must never land unencrypted. Nothing is
	// written either way. Governing: SPEC-0032 REQ-3, REQ-12.
	if in.SecretSupplied {
		if err := sourcekind.CheckSecret(sourceType, secret); err != nil {
			s.logRefusedSecretWrite("", in.EndpointID, in.Actor.ID, "invalid_argument")
			return Webhook{}, fmt.Errorf("%w: %w", ErrInvalidArgument, err)
		}
		if s.secretCipher == nil {
			s.logRefusedSecretWrite("", in.EndpointID, in.Actor.ID, "encryption_required")
			return Webhook{}, ErrEncryptionRequired
		}
	}
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
	err = tx.QueryRow(ctx, `SELECT id::text FROM endpoints WHERE id = $1 FOR UPDATE`, in.EndpointID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook lock endpoint: %w", err)
	}

	// Encrypt the held secret at rest when a cipher is configured: the column stores enc:v1: ciphertext
	// under a key held outside the database, so a DB-only compromise never yields a live signing secret.
	// The delivery path (GetWebhookSecretByToken) decrypts transparently, so verification is unchanged.
	// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency".
	storedSecret, err := s.sealSecret(secret)
	if err != nil {
		return Webhook{}, err
	}

	// The CASE keeps the invariant "signing_secret is non-NULL only when trust_mode='signed'":
	// switchboard holds a secret only where an HMAC is what verifies a delivery. SPEC-0032 loosens the
	// other direction: a provider-origin signed webhook with no secret yet is a valid row, and NULL is
	// exactly the state "awaiting its secret" reads from (design.md "awaiting_secret is derived").
	var w Webhook
	err = tx.QueryRow(ctx, `
		INSERT INTO endpoint_webhooks (endpoint_id, source_type, target_queue, trust_mode, ingest_token,
			signing_secret, trusted_actors, secret_origin, secret_set_at, secret_supplied)
		VALUES ($1, $2, $3, $4, $5,
			CASE WHEN $4 = 'signed' THEN NULLIF($6, '') ELSE NULL END,
			$7::jsonb, $8,
			CASE WHEN $4 = 'signed' AND NULLIF($6, '') IS NOT NULL THEN now() ELSE NULL END, $9)
		RETURNING `+webhookRowColumns,
		in.EndpointID, sourceType, targetQueue, trustMode, ingestToken, storedSecret, trustedActors,
		origin, in.SecretSupplied,
	).Scan(w.scanTargets()...)
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook insert: %w", err)
	}
	w.SecretSet = w.SecretSetAt != nil
	if w.SecretSet {
		w.SecretFingerprint = SecretFingerprint(secret)
	}

	// A caller-supplied secret is auditable; a minted one is not (SPEC-0032 REQ-13). The row goes in
	// here, before the ceiling check can roll the insert back, so an audit row and its secret either
	// both exist or neither does.
	if in.SecretSupplied && w.SecretSet {
		if err := appendSecretAudit(ctx, tx, w.ID, SecretActionSet, in.Actor, w.SecretFingerprint, ""); err != nil {
			return Webhook{}, err
		}
		w.SecretSetByKind, w.SecretSetByID = in.Actor.Kind, in.Actor.ID
	}

	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM endpoint_webhooks WHERE endpoint_id = $1`, in.EndpointID,
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
	// Warn only after the commit: the ceiling check above can roll this insert back, and a warning
	// before that point would report a plaintext write that never happened.
	s.warnPlaintextSigningSecret(w.TrustMode, secret, w.ID, w.EndpointID)
	return w, nil
}

// ListWebhooks returns an endpoint's self-managed webhooks, newest first, with the SPEC-0032 secret
// fields a caller needs to see what state each webhook is in: which kind of secret it takes, whether
// it has one, the fingerprint and moment of the current one, and who last wrote a supplied one.
//
// The secret value is read here only to hash it. It is scanned into a local, never leaves this
// function, and a row whose stored value cannot be decrypted still reports secret_set with an empty
// fingerprint rather than failing the whole list — the webhook's state is knowable even when the key
// changed under it. Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling" (list
// returns metadata, no secret values), SPEC-0032 REQ-2 (origin, set, fingerprint, set_at), REQ-13
// (secret_set_by from the latest audited write).
func (s *Store) ListWebhooks(ctx context.Context, endpointID string) ([]Webhook, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+webhookRowColumns+`, w.signing_secret, aud.actor_kind, aud.actor_id::text
		FROM endpoint_webhooks w
		LEFT JOIN LATERAL (
			SELECT actor_kind, actor_id FROM webhook_secret_audit s
			WHERE s.webhook_id = w.id AND s.action IN ('set', 'replace')
			ORDER BY s.at DESC LIMIT 1) aud ON true
		WHERE w.endpoint_id = $1 ORDER BY w.created_at DESC`, endpointID)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		var stored *string
		var setByKind, setByID *string
		if err := rows.Scan(append(w.scanTargets(), &stored, &setByKind, &setByID)...); err != nil {
			return nil, fmt.Errorf("store: list webhooks scan: %w", err)
		}
		if stored != nil {
			s.secretStateFromStored(&w, stored)
		} else {
			w.SecretSet = false
		}
		if setByKind != nil {
			w.SecretSetByKind = *setByKind
		}
		if setByID != nil {
			w.SecretSetByID = *setByID
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
		SELECT `+webhookRowColumns+`
		FROM endpoint_webhooks w WHERE ingest_token = $1`, ingestToken,
	).Scan(w.scanTargets()...)
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
	if s == nil || s.pool == nil {
		// A nil-backed Store (a route-table test, or a process started without a database) resolves
		// nothing: fail closed like every other nil-guarded read, because a delivery that dereferences
		// a nil pool takes the whole process down instead of answering 404.
		return Webhook{}, "", ErrNotFound
	}
	var w Webhook
	var secret *string
	err := s.pool.QueryRow(ctx, `
		SELECT `+webhookRowColumns+`, w.signing_secret
		FROM endpoint_webhooks w WHERE ingest_token = $1`, ingestToken,
	).Scan(append(w.scanTargets(), &secret)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, "", ErrNotFound
	}
	if err != nil {
		return Webhook{}, "", fmt.Errorf("store: get webhook secret by token: %w", err)
	}
	s2 := ""
	if secret != nil {
		s2 = *secret
		w.SecretSet = true
	}
	// Decrypt transparently on the delivery path: the receiver needs the plaintext to recompute the
	// provider HMAC. openSecret returns legacy plaintext unchanged, so mixed (pre/post-encryption) rows
	// both verify. Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency".
	plaintext, err := s.openSecret(s2)
	if err != nil {
		return Webhook{}, "", err
	}
	if w.SecretSet {
		w.SecretFingerprint = SecretFingerprint(plaintext)
	}
	return w, plaintext, nil
}

// sealSecret encrypts a held secret for storage when a SecretCipher is configured, returning the
// enc:v1: ciphertext form; with no cipher (or an empty secret) it returns the value unchanged so the
// existing CASE/NULLIF invariants in the SQL still apply. Governing: SPEC-0006 REQ "Switchboard Owns
// Secrets, Verification, and Idempotency".
func (s *Store) sealSecret(secret string) (string, error) {
	if s.secretCipher == nil || secret == "" {
		return secret, nil
	}
	sealed, err := s.secretCipher.Encrypt(secret)
	if err != nil {
		return "", fmt.Errorf("store: encrypt webhook secret: %w", err)
	}
	return sealed, nil
}

// CountSignedWebhooks reports how many self-managed webhooks currently retain a signing secret
// (trust_mode='signed'). Used at startup to decide whether an empty encryption key is worth warning
// about: a deployment with no signed webhooks is not storing anything in the clear yet.
//
// This is a point-in-time answer and deliberately not the only guard — a signed webhook created
// later is caught by the per-write warning instead. Governing: SPEC-0006 REQ "Switchboard Owns
// Secrets, Verification, and Idempotency".
func (s *Store) CountSignedWebhooks(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM endpoint_webhooks WHERE trust_mode = 'signed' AND signing_secret IS NOT NULL`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count signed webhooks: %w", err)
	}
	return n, nil
}

// openSecret reverses sealSecret on the delivery path. With no cipher it returns the value unchanged
// (legacy plaintext); with a cipher it decrypts enc:v1: ciphertext and passes legacy plaintext
// through, so enabling encryption never strands rows written before it was on.
func (s *Store) openSecret(stored string) (string, error) {
	if s.secretCipher == nil || stored == "" {
		return stored, nil
	}
	plaintext, err := s.secretCipher.Decrypt(stored)
	if err != nil {
		return "", fmt.Errorf("store: decrypt webhook secret: %w", err)
	}
	return plaintext, nil
}

// RotateWebhookSecret stores a freshly minted signing secret and a new ingest token for a webhook the
// given endpoint owns, retiring the old secret/URL in a single conditional UPDATE. The endpoint_id
// guard in the WHERE clause is the ownership check: another endpoint's (or an unknown) id affects no
// rows and returns ErrNotFound. The secret is retained only for a signed webhook whose secret
// switchboard mints: a provider-origin webhook keeps whatever secret its owner pasted (minting one
// there would replace a value the provider signs with by a value it has never heard of), and
// secret_supplied is cleared because the minted value is no longer the caller's. Governing:
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency" (rotate mints a new secret
// and retires the old), SPEC-0032 REQ-1 (a minted webhook that had a supplied secret goes back to
// minted), ADR-0003 (per-source trust model).
func (s *Store) RotateWebhookSecret(ctx context.Context, id, endpointID, newSecret, newIngestToken string) (Webhook, error) {
	// Encrypt the freshly minted secret at rest when a cipher is configured (same envelope as create).
	storedSecret, err := s.sealSecret(newSecret)
	if err != nil {
		return Webhook{}, err
	}
	var w Webhook
	var stored *string
	err = s.pool.QueryRow(ctx, `
		UPDATE endpoint_webhooks
		SET signing_secret = CASE WHEN trust_mode = 'signed' AND secret_origin <> 'provider'
				THEN NULLIF($3, '') ELSE signing_secret END,
			ingest_token = $4, rotated_at = now(),
			secret_supplied = false,
			secret_set_at = CASE WHEN trust_mode = 'signed' AND secret_origin <> 'provider'
				THEN now() ELSE secret_set_at END
		WHERE id = $1 AND endpoint_id = $2
		RETURNING `+webhookRowColumns+`, signing_secret`,
		id, endpointID, storedSecret, newIngestToken,
	).Scan(append(w.scanTargets(), &stored)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: rotate webhook: %w", err)
	}
	// The fingerprint describes what the row holds now, which is not necessarily what rotate was asked
	// to store: a provider-origin row keeps the producer's secret and ignores the minted one, so
	// hashing the minted value here would report a secret that is not in the row.
	s.secretStateFromStored(&w, stored)
	// Rotate mints a secret for every webhook but the CASE above stores one only for a signed webhook
	// switchboard mints for, so the trust mode comes from the row we just wrote, not from the caller.
	if w.SecretOrigin != sourcekind.OriginProvider {
		s.warnPlaintextSigningSecret(w.TrustMode, newSecret, w.ID, w.EndpointID)
	}
	return w, nil
}

// secretStateFromStored fills SecretSet and SecretFingerprint from the value a row actually holds. A
// stored secret that no longer decrypts (the at-rest key changed under it) still counts as set, with an
// empty fingerprint, rather than failing the whole read: the state is the part a caller can act on.
func (s *Store) secretStateFromStored(w *Webhook, stored *string) {
	w.SecretFingerprint = ""
	w.SecretSet = stored != nil
	if stored == nil {
		return
	}
	plaintext, err := s.openSecret(*stored)
	if err == nil {
		w.SecretFingerprint = SecretFingerprint(plaintext)
	} else if s.log != nil {
		s.log.Warn("webhook secret unreadable for fingerprint", "webhook", w.ID,
			"reason", "the stored secret does not decrypt with the configured key")
	}
}

// SetWebhookTrustedActors replaces the trust list of a webhook the given endpoint owns. The
// endpoint_id guard is the ownership check: another endpoint's, an unknown, or a malformed id is
// ErrNotFound. The caller has validated trustedActors against the webhook's source
// (routing.ParseTrustedActors), which is why it reads the source first with WebhookForEndpoint.
// Governing: SPEC-0026 REQ-5 (set_trusted_actors, clear_trusted_actors; foreign ids are not_found).
func (s *Store) SetWebhookTrustedActors(ctx context.Context, id, endpointID string, trustedActors []byte) (Webhook, error) {
	if !isUUID(id) || !isUUID(endpointID) || len(trustedActors) == 0 {
		return Webhook{}, ErrNotFound
	}
	var w Webhook
	err := s.pool.QueryRow(ctx, `
		UPDATE endpoint_webhooks SET trusted_actors = $3::jsonb
		WHERE id = $1 AND endpoint_id = $2
		RETURNING `+webhookRowColumns,
		id, endpointID, trustedActors,
	).Scan(w.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: set webhook trusted actors: %w", err)
	}
	return w, nil
}

// WebhookForEndpoint returns one webhook the given endpoint owns, or ErrNotFound for another
// endpoint's, an unknown, or a malformed id.
func (s *Store) WebhookForEndpoint(ctx context.Context, id, endpointID string) (Webhook, error) {
	if !isUUID(id) || !isUUID(endpointID) {
		return Webhook{}, ErrNotFound
	}
	var w Webhook
	err := s.pool.QueryRow(ctx, `
		SELECT `+webhookRowColumns+`
		FROM endpoint_webhooks w WHERE id = $1 AND endpoint_id = $2`, id, endpointID,
	).Scan(w.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: webhook for endpoint: %w", err)
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
