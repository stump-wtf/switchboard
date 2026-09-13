package store

// Plaintext-signing-secret warning.
//
// An empty SWITCHBOARD_SECRET_ENCRYPTION_KEY leaves the legacy plaintext behavior: self-managed
// webhook HMAC signing secrets are written to Postgres in the clear. That is a supported
// configuration, but it used to be entirely silent — the stack looked healthy, nothing logged, and
// the only way for an operator to discover it was to read the database. This file makes the write
// say so.
//
// The warning fires at the moment a plaintext signing secret is persisted, not only at startup. A
// startup-only check is correct when it fires and silent when it does not: an operator can boot
// with no signed webhooks (correctly quiet), create one an hour later, and store a secret in the
// clear with startup long past.
//
// Scope is deliberately narrow. sealSecret is shared with the provider-registry secrets in
// adapters.go, which are a different class of secret and not covered by the decision behind this
// warning, so the call sites warn rather than sealSecret itself.
//
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", ADR-0002.
//
// @joestump-agent 09/12/2026 - Added the predicate, the optional logger, and the two call-site
// warnings in webhooks.go (create + rotate).

import "log/slog"

// trustModeSigned is the only trust mode whose webhooks retain a signing secret. The insert and
// update statements enforce the same thing in SQL ("signing_secret is non-NULL iff
// trust_mode='signed'"), so the predicate below has to agree with that CASE or it would warn about
// a token/open webhook whose secret was never stored.
const trustModeSigned = "signed"

// WithLogger attaches a logger used for security-relevant warnings about what was persisted.
// Optional and nil-safe: omit it and the store behaves exactly as before. It is an Option rather
// than a constructor parameter so no existing New(pool) call site has to change.
//
// A logger is used here in preference to returning a signal for the caller to log, because a
// returned flag is silent whenever a caller forgets to check it — which is the same class of defect
// this warning exists to close, reintroduced one layer up. There are two call sites today and
// nothing stops a third being added without the check.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.log = l }
}

// storesPlaintextSigningSecret reports whether persisting this webhook wrote an unencrypted signing
// secret: no cipher is configured, the webhook is signed (so the column is actually populated), and
// a secret was supplied.
//
// Kept as a free function over explicit inputs — rather than a method reaching into Store — so it
// is unit-testable with no database. The DB-backed tests in this package skip unless
// SWITCHBOARD_TEST_DATABASE_URL is set, and CI does not provide Postgres (issue #141), so a test
// that only exercised this through Postgres would report ok while asserting nothing.
func storesPlaintextSigningSecret(cipher SecretCipher, trustMode, secret string) bool {
	return cipher == nil && trustMode == trustModeSigned && secret != ""
}

// warnPlaintextSigningSecret emits the warning when a signing secret has just been persisted
// unencrypted. No-op when a cipher is configured, when the webhook is not signed, or when no logger
// was attached. The secret itself is never logged — only the identifiers needed to find the row.
func (s *Store) warnPlaintextSigningSecret(trustMode, secret, webhookID, endpointID string) {
	if s.log == nil || !storesPlaintextSigningSecret(s.secretCipher, trustMode, secret) {
		return
	}
	s.log.Warn("webhook signing secret stored without at-rest encryption",
		"webhook", webhookID,
		"endpoint", endpointID,
		"reason", "SWITCHBOARD_SECRET_ENCRYPTION_KEY is empty",
		"impact", "the HMAC signing secret is readable by anyone who can read the database",
	)
}
