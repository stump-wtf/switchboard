package store

// DB-backed tests for the SPEC-0006 webhook self-management store methods. Skip cleanly without
// SWITCHBOARD_TEST_DATABASE_URL like every other store test; Gitea CI is the gate.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestWebhookCreateListRotateDelete(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|wh", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, err := MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-wh", "sbk_wh0001", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	// Create two webhooks within a max of 3.
	w1, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "tok-1", "secrethash-1", 3)
	if err != nil {
		t.Fatalf("create webhook 1: %v", err)
	}
	if w1.ID == "" || w1.TrustMode != "signed" || w1.IngestToken != "tok-1" {
		t.Fatalf("webhook 1 = %+v", w1)
	}
	if _, err := s.CreateWebhook(ctx, ep.ID, "generic", "reviews", "token", "tok-2", "secrethash-2", 3); err != nil {
		t.Fatalf("create webhook 2: %v", err)
	}

	// List returns both, newest first, with no secret material on the struct.
	list, err := s.ListWebhooks(ctx, ep.ID)
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}

	// Rotate mints a new token and stamps rotated_at, returning the same id.
	rot, err := s.RotateWebhookSecret(ctx, w1.ID, ep.ID, "secrethash-1b", "tok-1b")
	if err != nil {
		t.Fatalf("rotate webhook: %v", err)
	}
	if rot.ID != w1.ID || rot.IngestToken != "tok-1b" || rot.RotatedAt == nil {
		t.Fatalf("rotate result = %+v, want same id, new token, rotated_at set", rot)
	}

	// Delete tears it down; a second delete is not found.
	if err := s.DeleteWebhook(ctx, w1.ID, ep.ID); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	if err := s.DeleteWebhook(ctx, w1.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
	if list, _ := s.ListWebhooks(ctx, ep.ID); len(list) != 1 {
		t.Fatalf("after delete list len = %d, want 1", len(list))
	}
}

// TestWebhookCeilingIsAtomic: CreateWebhook enforces the max-count ceiling atomically — a create that
// would exceed max is rejected with ErrCeilingExceeded and rolled back (no row persists).
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling" + "Concurrency Safety".
func TestWebhookCeilingIsAtomic(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|whc", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-whc", "sbk_wh0002", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	if _, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "c1", "hash1", 1); err != nil {
		t.Fatalf("create within ceiling: %v", err)
	}
	// max=1 and one exists: the next create must be refused and must not persist.
	if _, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "c2", "hash2", 1); !errors.Is(err, ErrCeilingExceeded) {
		t.Fatalf("over-ceiling create err = %v, want ErrCeilingExceeded", err)
	}
	list, _ := s.ListWebhooks(ctx, ep.ID)
	if len(list) != 1 {
		t.Fatalf("after refused create list len = %d, want 1 (rollback)", len(list))
	}
}

// TestWebhookCeilingSerializesConcurrentCreates exercises the check-then-act RACE the atomic-count
// comment claims to close: many create_webhook calls fired at once against a ceiling of maxN must let
// through EXACTLY maxN, never more. Under READ COMMITTED an insert-then-count without the endpoint
// row lock would let several concurrent creates each see only their own uncommitted insert and all
// commit, overshooting the ceiling. Governing: SPEC-0006 REQ "Concurrency Safety".
func TestWebhookCeilingSerializesConcurrentCreates(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|whcc", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-whcc", "sbk_wh0003", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	const maxN, racers = 3, 12
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention at the boundary
			tok := fmt.Sprintf("race-%d", i)
			_, results[i] = s.CreateWebhook(ctx, ep.ID, "generic", "reviews", "token", tok, "h", maxN)
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, refused int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrCeilingExceeded):
			refused++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if ok != maxN {
		t.Fatalf("concurrent creates succeeded = %d, want exactly %d (ceiling overshoot = race not serialized)", ok, maxN)
	}
	if refused != racers-maxN {
		t.Fatalf("refused = %d, want %d", refused, racers-maxN)
	}
	if list, _ := s.ListWebhooks(ctx, ep.ID); len(list) != maxN {
		t.Fatalf("persisted webhooks = %d, want %d", len(list), maxN)
	}
}

// TestGetWebhookByToken resolves an ingest token to its webhook and returns ErrNotFound for an
// unknown token — the routing lookup the /webhooks/w/{token} receiver depends on.
func TestGetWebhookByToken(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|whget", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-whget", "sbk_wh0004", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	created, err := s.CreateWebhook(ctx, ep.ID, "generic", "reviews", "token", "route-tok", "h", 2)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	got, err := s.GetWebhookByToken(ctx, "route-tok")
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if got.ID != created.ID || got.EndpointID != ep.ID || got.SourceType != "generic" || got.TargetQueue != "reviews" || got.TrustMode != "token" {
		t.Fatalf("get by token = %+v, want match of %+v", got, created)
	}
	if _, err := s.GetWebhookByToken(ctx, "no-such-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
}

// TestGetWebhookSecretByToken: the delivery-path read returns the plaintext signing secret switchboard
// holds for a signed webhook (so it can recompute the HMAC), and an empty secret for a token webhook
// (whose column stays NULL by the CASE invariant). An unknown token is ErrNotFound. Governing:
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", SPEC-0003.
func TestGetWebhookSecretByToken(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|whsec", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-whsec", "sbk_wh0005", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	// A signed webhook holds its minted secret; the receiver reads it back verbatim to verify HMAC.
	if _, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "sig-tok", "whsec_deadbeef", 3); err != nil {
		t.Fatalf("create signed webhook: %v", err)
	}
	wh, secret, err := s.GetWebhookSecretByToken(ctx, "sig-tok")
	if err != nil {
		t.Fatalf("get secret by token: %v", err)
	}
	if wh.TrustMode != "signed" || secret != "whsec_deadbeef" {
		t.Fatalf("signed secret round-trip = (mode=%q secret=%q), want signed/whsec_deadbeef", wh.TrustMode, secret)
	}

	// A token webhook holds no secret even if one is passed at create (CASE invariant): secret is "".
	if _, err := s.CreateWebhook(ctx, ep.ID, "generic", "reviews", "token", "tok-tok", "should-be-dropped", 3); err != nil {
		t.Fatalf("create token webhook: %v", err)
	}
	twh, tsecret, err := s.GetWebhookSecretByToken(ctx, "tok-tok")
	if err != nil {
		t.Fatalf("get token secret by token: %v", err)
	}
	if twh.TrustMode != "token" || tsecret != "" {
		t.Fatalf("token secret = (mode=%q secret=%q), want token/empty", twh.TrustMode, tsecret)
	}

	if _, _, err := s.GetWebhookSecretByToken(ctx, "no-such-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
}

// TestWebhookSecretEncryptedAtRest proves the SPEC-0006 at-rest hardening end to end: the minted
// signing secret is stored as AES-GCM ciphertext (a raw DB read of signing_secret never contains the
// plaintext), yet GetWebhookSecretByToken decrypts it transparently so the delivery path can still
// recompute a matching provider HMAC — the encrypt→store→decrypt→verify round-trip with no behavior
// change. Rotation re-encrypts a fresh secret the same way. Governing: SPEC-0006 REQ "Switchboard
// Owns Secrets, Verification, and Idempotency" (encrypt held secrets at rest with a key not stored
// alongside the ciphertext; delivery-path verification transparently decrypts, no behavior change).
func TestWebhookSecretEncryptedAtRest(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|whenc", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-whenc", "sbk_wh0006", slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	const plaintext = "whsec_supersecretvalue"
	if _, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "enc-tok", plaintext, 3); err != nil {
		t.Fatalf("create signed webhook: %v", err)
	}

	// A raw DB read of the column returns opaque ciphertext bytes — never the plaintext secret.
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT signing_secret FROM endpoint_webhooks WHERE ingest_token = 'enc-tok'`).Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("raw signing_secret is empty, want ciphertext")
	}
	if bytes.Contains(raw, []byte(plaintext)) {
		t.Fatalf("raw signing_secret contains the plaintext secret — not encrypted at rest")
	}

	// The delivery path decrypts transparently and recomputes a matching HMAC over a body.
	_, got, err := s.GetWebhookSecretByToken(ctx, "enc-tok")
	if err != nil {
		t.Fatalf("get secret by token: %v", err)
	}
	if got != plaintext {
		t.Fatalf("decrypted secret = %q, want %q", got, plaintext)
	}
	body := []byte(`{"action":"opened"}`)
	if !hmac.Equal(hmacSHA256(plaintext, body), hmacSHA256(got, body)) {
		t.Fatalf("recomputed HMAC with decrypted secret does not match the original secret's HMAC")
	}

	// Rotation re-encrypts a fresh secret: new ciphertext at rest, new plaintext on read.
	const rotated = "whsec_rotatedvalue"
	w, _ := s.GetWebhookByToken(ctx, "enc-tok")
	if _, err := s.RotateWebhookSecret(ctx, w.ID, ep.ID, rotated, "enc-tok-2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	var raw2 []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT signing_secret FROM endpoint_webhooks WHERE ingest_token = 'enc-tok-2'`).Scan(&raw2); err != nil {
		t.Fatalf("raw read after rotate: %v", err)
	}
	if bytes.Contains(raw2, []byte(rotated)) || bytes.Equal(raw2, raw) {
		t.Fatalf("rotated signing_secret is plaintext or unchanged, want fresh ciphertext")
	}
	if _, got2, _ := s.GetWebhookSecretByToken(ctx, "enc-tok-2"); got2 != rotated {
		t.Fatalf("rotated decrypted secret = %q, want %q", got2, rotated)
	}
}

func hmacSHA256(secret string, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return m.Sum(nil)
}

// TestWebhookOwnershipGuard: rotate/delete of another endpoint's webhook is ErrNotFound — the
// endpoint_id WHERE-clause guard is the ownership check.
func TestWebhookOwnershipGuard(t *testing.T) {
	s, ctx := testStore(t)

	h := mustHuman(t, s, ctx, "pocket|who", "Joe")
	ag := mustAgent(t, s, ctx, h.ID, "hook-bot")
	slugA, _ := MintSlug("a")
	slugB, _ := MintSlug("b")
	epA, _ := s.CreateEndpoint(ctx, ag.ID, "credhash-a", "sbk_a00001", slugA, []string{"reviews"}, []string{"create_webhook"})
	epB, _ := s.CreateEndpoint(ctx, ag.ID, "credhash-b", "sbk_b00001", slugB, []string{"reviews"}, []string{"create_webhook"})

	w, err := s.CreateWebhook(ctx, epA.ID, "github", "reviews", "signed", "own-1", "h1", 2)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	// Endpoint B cannot rotate or delete endpoint A's webhook.
	if _, err := s.RotateWebhookSecret(ctx, w.ID, epB.ID, "h2", "own-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-endpoint rotate err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteWebhook(ctx, w.ID, epB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-endpoint delete err = %v, want ErrNotFound", err)
	}
	// A still owns it.
	if list, _ := s.ListWebhooks(ctx, epA.ID); len(list) != 1 {
		t.Fatalf("owner list len = %d, want 1", len(list))
	}
}
