package store

// DB-backed tests for at-rest encryption of self-managed webhook signing secrets (issue #153).
// Skip cleanly without SWITCHBOARD_TEST_DATABASE_URL like every other store test.
//
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency".

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/db"
)

// testStoreWithCipher is testStore's sibling that enables at-rest encryption, so the DB-backed tests
// exercise the real encrypt-on-write / decrypt-on-read path against Postgres.
func testStoreWithCipher(t *testing.T) (*Store, context.Context, *cred.SecretBox) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run store tests")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, personas, friend_edges, todos, events, sessions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	box, err := cred.NewSecretBox(key)
	if err != nil {
		t.Fatalf("new secret box: %v", err)
	}
	return New(pool, WithSecretCipher(box)), ctx, box
}

// rawSigningSecret reads the signing_secret column exactly as persisted, bypassing the store's
// decrypt path, so the test can prove what actually sits on disk.
func rawSigningSecret(t *testing.T, s *Store, ctx context.Context, ingestToken string) string {
	t.Helper()
	var raw *string
	if err := s.pool.QueryRow(ctx,
		`SELECT signing_secret FROM endpoint_webhooks WHERE ingest_token = $1`, ingestToken,
	).Scan(&raw); err != nil {
		t.Fatalf("raw select signing_secret: %v", err)
	}
	if raw == nil {
		return ""
	}
	return *raw
}

// TestWebhookSecretEncryptedAtRest is the core acceptance test for #153: with a cipher configured, the
// minted signing secret is stored as enc:v1: ciphertext (never plaintext), yet the delivery-path read
// transparently decrypts back to the original so HMAC verification is unchanged. Rotation re-encrypts.
func TestWebhookSecretEncryptedAtRest(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)

	h := mustHuman(t, s, ctx, "pocket|whenc", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-whenc", "sbk_whe001", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	const plaintext = "whsec_super_secret_hmac_key"
	if _, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "enc-tok", plaintext, 3); err != nil {
		t.Fatalf("create signed webhook: %v", err)
	}

	// On disk: ciphertext, not plaintext. This is the "not stored in plaintext" proof.
	raw := rawSigningSecret(t, s, ctx, "enc-tok")
	if !cred.IsEncrypted(raw) {
		t.Fatalf("stored signing_secret %q is not enc:v1: ciphertext", raw)
	}
	if strings.Contains(raw, plaintext) {
		t.Fatalf("stored signing_secret %q leaks the plaintext secret", raw)
	}
	if raw == plaintext {
		t.Fatal("stored signing_secret equals the plaintext secret")
	}

	// Delivery path: decrypts back to the exact minted secret, so verification behavior is unchanged.
	wh, secret, err := s.GetWebhookSecretByToken(ctx, "enc-tok")
	if err != nil {
		t.Fatalf("get secret by token: %v", err)
	}
	if wh.TrustMode != "signed" || secret != plaintext {
		t.Fatalf("decrypted secret = (mode=%q secret=%q), want signed/%q", wh.TrustMode, secret, plaintext)
	}

	// Rotation re-encrypts the new secret at rest and still round-trips.
	const rotated = "whsec_rotated_key_value"
	if _, err := s.RotateWebhookSecret(ctx, wh.ID, ep.ID, rotated, "enc-tok-2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rawRot := rawSigningSecret(t, s, ctx, "enc-tok-2")
	if !cred.IsEncrypted(rawRot) || strings.Contains(rawRot, rotated) {
		t.Fatalf("rotated stored secret %q is not encrypted or leaks plaintext", rawRot)
	}
	if _, rotSecret, err := s.GetWebhookSecretByToken(ctx, "enc-tok-2"); err != nil || rotSecret != rotated {
		t.Fatalf("rotated round-trip = (%q, %v), want %q", rotSecret, err, rotated)
	}
}

// TestWebhookSecretEncryptionBackwardCompatible: a store WITH a cipher must still read a legacy
// plaintext row (written before encryption was enabled), so turning encryption on strands no rows.
// A signed webhook created by a no-cipher store stores plaintext; the cipher-store reads it verbatim.
func TestWebhookSecretEncryptionBackwardCompatible(t *testing.T) {
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run store tests")
	}
	// Seed a plaintext row via the plain store, then read through an encrypted store on the same DB.
	plain, ctx := testStore(t)
	h := mustHuman(t, plain, ctx, "pocket|whbc", "Joe")
	ag := mustAgent(t, plain, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := plain.CreateEndpoint(ctx, ag.ID, "credhash-whbc", "sbk_whb001", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	const legacy = "whsec_legacy_plaintext_value"
	if _, err := plain.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "legacy-tok", legacy, 3); err != nil {
		t.Fatalf("create legacy webhook: %v", err)
	}
	// Confirm it really is stored as plaintext (no cipher on the plain store).
	if raw := rawSigningSecret(t, plain, ctx, "legacy-tok"); raw != legacy {
		t.Fatalf("plain store should persist plaintext, got %q", raw)
	}

	// Now attach a cipher to a store over the same pool and read the legacy row: passthrough, no error.
	key := make([]byte, 32)
	box, err := cred.NewSecretBox(key)
	if err != nil {
		t.Fatalf("new secret box: %v", err)
	}
	enc := New(plain.pool, WithSecretCipher(box))
	_, secret, err := enc.GetWebhookSecretByToken(ctx, "legacy-tok")
	if err != nil {
		t.Fatalf("read legacy row through cipher store: %v", err)
	}
	if secret != legacy {
		t.Fatalf("legacy passthrough = %q, want %q", secret, legacy)
	}
}
