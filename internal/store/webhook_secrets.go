package store

// Signing-secret writes: the sentinel set SPEC-0032 REQ-12 names, the fingerprint every response and
// audit row reports instead of the secret, and the audit append that runs inside the write's own
// transaction.
//
// A caller-supplied secret is the one secret switchboard does not generate, so it is the one secret
// whose provenance a reader cannot infer from the row: someone pasted it, at some time, from some
// surface. That is what webhook_secret_audit records, and why the append has to share the write's
// transaction — a trail that survives a rolled-back write would blame someone for a secret that was
// never stored, and a write that commits without its row loses the only answer to "who rolled this".
//
// Governing: SPEC-0032 REQ-3 (write-only secret, encryption required), REQ-12 (sentinels), REQ-13
// (one audit row per successful write, none for a refused one); design.md "Secret writes are audited
// in their own table"; ADR-0037.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Sentinels for the secret paths (SPEC-0032 REQ-12). Each is wrapped with context at the boundary
// that hit it, so a caller matches on the sentinel and the log reads the reason.
var (
	// ErrAwaitingSecret is a provider-origin webhook whose signing secret has never been set: it
	// cannot verify anything yet, so a delivery is refused with 503 rather than counted as a bad
	// signature. It is a state, not a caller's mistake.
	ErrAwaitingSecret = errors.New("store: webhook awaits its signing secret")
	// ErrEncryptionRequired is a caller-supplied secret refused because no at-rest key is
	// configured. Storing a value somebody else issued, unencrypted, in a database anyone with read
	// access to it can dump, is not a degradation to accept silently the way a minted secret's
	// plaintext warning is: the caller has to be told, and nothing gets written.
	ErrEncryptionRequired = errors.New("store: signing secret requires an at-rest encryption key")
	// ErrInvalidArgument marks a well-formed request whose VALUES are not usable — a secret that
	// fails its length, printability or provider format rule. The tool layer maps it to the stable
	// invalid_argument code, and the wrapped text names the field, never the value.
	ErrInvalidArgument = errors.New("store: invalid argument")
)

// audit actions and actor/path vocabularies (SPEC-0032 REQ-13). They are the CHECK values in
// migration 0032; a value outside them is refused by the database.
const (
	SecretActionSet        = "set"
	SecretActionReplace    = "replace"
	SecretActionPrevErased = "previous_erased"
	SecretActorEndpoint    = "endpoint"
	SecretActorHuman       = "human"
	SecretActorSystem      = "system"
	SecretPathMCP          = "mcp"
	SecretPathWeb          = "web"
	SecretPathAPI          = "api"
	SecretPathSweep        = "sweep"
	// secretFingerprintHexChars is SPEC-0032 REQ-2's "first 12 hex characters of SHA-256".
	secretFingerprintHexChars = 12
)

// SecretActor is who wrote a secret, for the audit row. Kind is endpoint|human|system, ID the
// endpoint or human id (empty for system), Path the surface it arrived on.
type SecretActor struct {
	Kind string
	ID   string
	Path string
}

// SecretFingerprint is what every response, list row and audit row reports in place of a secret: the
// first 12 hex characters of its SHA-256. Long enough that two providers' secrets do not collide in
// practice, short and one-way enough that reading it gets an attacker nothing.
func SecretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:secretFingerprintHexChars]
}

// appendSecretAudit inserts one audit row inside the transaction that is writing the secret. Call it
// only on the paths that actually persisted a caller-supplied secret: a refused write must leave no
// row (SPEC-0032 REQ-13), which is why the refusal paths log instead and never reach here.
//
// prevFingerprint is the secret being displaced, when there was one. The row carries no secret, no
// fragment of one, and nothing from the request body.
func appendSecretAudit(ctx context.Context, tx pgx.Tx, webhookID, action string, actor SecretActor, fingerprint, prevFingerprint string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO webhook_secret_audit (webhook_id, action, actor_kind, actor_id, path, fingerprint, prev_fingerprint)
		VALUES ($1::uuid, $2, $3, NULLIF($4, '')::uuid, $5, $6, NULLIF($7, ''))`,
		webhookID, action, actor.Kind, actor.ID, actor.Path, fingerprint, prevFingerprint)
	if err != nil {
		return fmt.Errorf("store: append secret audit: %w", err)
	}
	return nil
}

// logRefusedSecretWrite logs a secret write that was turned away. Identifiers and the reason, and
// nothing else: no secret, no body, no fingerprint of a value that was never stored (SPEC-0032
// REQ-13). A nil logger is fine — the refusal is still returned to the caller either way.
func (s *Store) logRefusedSecretWrite(webhookID, endpointID, actorID, reason string) {
	if s.log == nil {
		return
	}
	s.log.Warn("webhook signing secret write refused",
		"webhook", webhookID, "endpoint", endpointID, "actor", actorID, "reason", reason)
}
