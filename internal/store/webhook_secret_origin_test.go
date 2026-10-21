package store

// DB-backed tests for SPEC-0032 REQ-1/2/3/13: a webhook row states who issues its signing secret, a
// provider-origin webhook created without one is a valid row that awaits that secret, a supplied
// secret is stored encrypted, audited in its own table inside the write's transaction, and reported
// only as a fingerprint. Skips cleanly without SWITCHBOARD_TEST_DATABASE_URL like every other store
// test.
//
// Governing: SPEC-0032 REQ-1, REQ-2, REQ-3, REQ-12, REQ-13.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// originEndpoint vends an endpoint owned by a fresh human/agent pair, so each test has its own tenant.
func originEndpoint(t *testing.T, s *Store, ctx context.Context, tag string) Endpoint {
	t.Helper()
	h := mustHuman(t, s, ctx, "pocket|"+tag, "Joe "+tag)
	ag := mustAgent(t, s, ctx, h.ID, "bot-"+tag)
	slug, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-"+tag, "sbk_"+tag, slug, []string{"reviews"}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	return ep
}

func countRows(t *testing.T, s *Store, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func auditRowText(t *testing.T, s *Store, ctx context.Context, webhookID string) string {
	t.Helper()
	var text string
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(string_agg(a::text, '|'), '') FROM webhook_secret_audit a WHERE webhook_id = $1`,
		webhookID).Scan(&text); err != nil {
		t.Fatalf("audit text: %v", err)
	}
	return text
}

// TestProviderWebhookAwaitsItsSecret is REQ-2's first scenario at the row level: creating a stripe
// webhook stores no secret, so the row reads secret_origin=provider, secret_set=false, and nothing is
// revealed on any read path. Rotating it must not mint a secret over the empty slot either.
func TestProviderWebhookAwaitsItsSecret(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	ep := originEndpoint(t, s, ctx, "origprov")

	w, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: ep.ID, SourceType: "stripe", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_origprov",
	})
	if err != nil {
		t.Fatalf("create provider webhook: %v", err)
	}
	if w.SecretOrigin != "provider" {
		t.Errorf("secret_origin = %q, want provider", w.SecretOrigin)
	}
	if w.SecretSet || w.SecretFingerprint != "" || w.SecretSetAt != nil {
		t.Errorf("awaiting webhook reports a secret: %+v", w)
	}
	if raw := rawSigningSecret(t, s, ctx, w.IngestToken); raw != "" {
		t.Errorf("signing_secret column = %q, want NULL", raw)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM webhook_secret_audit`); n != 0 {
		t.Errorf("an unwritten secret was audited (%d rows)", n)
	}
	if _, secret, err := s.GetWebhookSecretByToken(ctx, w.IngestToken); err != nil || secret != "" {
		t.Errorf("delivery read revealed %q (%v), want empty", secret, err)
	}

	list, err := s.ListWebhooks(ctx, ep.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d (%v)", len(list), err)
	}
	if list[0].SecretOrigin != "provider" || list[0].SecretSet || list[0].SecretFingerprint != "" {
		t.Errorf("list row = %+v, want provider and no secret", list[0])
	}

	rotated, err := s.RotateWebhookSecret(ctx, w.ID, ep.ID, "whsec_mintedOverProvider1", "tok_origprov_rot")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.SecretSet || rotated.SecretFingerprint != "" {
		t.Errorf("rotate minted a secret over a provider webhook: %+v", rotated)
	}
	if rotated.RotatedAt == nil {
		t.Error("rotate did not stamp rotated_at")
	}
	if raw := rawSigningSecret(t, s, ctx, "tok_origprov_rot"); raw != "" {
		t.Errorf("rotate stored %q in a provider webhook's secret column", raw)
	}
}

// TestSuppliedSecretStoredAuditedAndFingerprinted is REQ-3 plus REQ-13: the producer's own secret is
// stored encrypted, reported only as a fingerprint and a timestamp, and one audit row names the
// endpoint, the mcp path and the fingerprint — with no trace of the value in the audit row.
func TestSuppliedSecretStoredAuditedAndFingerprinted(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	ep := originEndpoint(t, s, ctx, "origsup")
	const secret = "whsec_SuppliedByTheProviderNotUs"

	w, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: ep.ID, SourceType: "stripe", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_origsup", Secret: secret, SecretSupplied: true,
		Actor: SecretActor{Kind: SecretActorEndpoint, ID: ep.ID, Path: SecretPathMCP},
	})
	if err != nil {
		t.Fatalf("create with supplied secret: %v", err)
	}
	if !w.SecretSet || !w.SecretSupplied {
		t.Errorf("secret_set/supplied = %v/%v, want true/true", w.SecretSet, w.SecretSupplied)
	}
	if w.SecretFingerprint != SecretFingerprint(secret) {
		t.Errorf("fingerprint = %q, want %q", w.SecretFingerprint, SecretFingerprint(secret))
	}
	if len(w.SecretFingerprint) != 12 {
		t.Errorf("fingerprint %q is not 12 hex characters", w.SecretFingerprint)
	}
	if w.SecretSetAt == nil {
		t.Error("secret_set_at is nil for a stored secret")
	}
	if w.SecretSetByKind != SecretActorEndpoint || w.SecretSetByID != ep.ID {
		t.Errorf("secret_set_by = %s/%s, want endpoint/%s", w.SecretSetByKind, w.SecretSetByID, ep.ID)
	}
	raw := rawSigningSecret(t, s, ctx, w.IngestToken)
	if !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, secret) {
		t.Errorf("stored value is not sealed ciphertext: %q", raw)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM webhook_secret_audit WHERE webhook_id = $1::uuid`, w.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	text := auditRowText(t, s, ctx, w.ID)
	for _, want := range []string{"set", "endpoint", "mcp", w.SecretFingerprint} {
		if !strings.Contains(text, want) {
			t.Errorf("audit row %q missing %q", text, want)
		}
	}
	if strings.Contains(text, secret) {
		t.Errorf("audit row contains the secret: %q", text)
	}
	if _, got, err := s.GetWebhookSecretByToken(ctx, w.IngestToken); err != nil || got != secret {
		t.Errorf("delivery read = %q (%v), want the stored secret so HMAC verifies", got, err)
	}

	list, err := s.ListWebhooks(ctx, ep.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d (%v)", len(list), err)
	}
	if list[0].SecretFingerprint != w.SecretFingerprint || list[0].SecretSetAt == nil {
		t.Errorf("list row lost fingerprint/set_at: %+v", list[0])
	}
	if list[0].SecretSetByKind != SecretActorEndpoint || list[0].SecretSetByID != ep.ID {
		t.Errorf("list secret_set_by = %s/%s", list[0].SecretSetByKind, list[0].SecretSetByID)
	}
	if strings.Contains(auditRowText(t, s, ctx, w.ID), secret) {
		t.Error("secret reached the audit table")
	}
}

// TestMintedSecretIsNotAudited keeps REQ-13's other half true: switchboard's own mint is not an event
// worth a row, so the table stays a record of secrets somebody handed over.
func TestMintedSecretIsNotAudited(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	ep := originEndpoint(t, s, ctx, "origmint")

	w, err := s.CreateWebhookWithTrust(ctx, ep.ID, "github", "reviews", "signed", "tok_origmint",
		"whsec_WeMintedThisOneOurselves", 1, nil)
	if err != nil {
		t.Fatalf("create minted webhook: %v", err)
	}
	if w.SecretOrigin != "minted" || !w.SecretSet || w.SecretSupplied {
		t.Errorf("minted row = %+v, want origin minted, set, not supplied", w)
	}
	if w.SecretSetByKind != "" || w.SecretSetByID != "" {
		t.Errorf("minted webhook reports a secret_set_by: %s/%s", w.SecretSetByKind, w.SecretSetByID)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM webhook_secret_audit WHERE webhook_id = $1::uuid`, w.ID); n != 0 {
		t.Errorf("a minted secret wrote %d audit rows, want none", n)
	}
}

// TestSuppliedSecretWithoutAtRestKeyWritesNothing is REQ-3's hard floor and REQ-12's
// encryption_required sentinel: no key, no stored provider secret, and no partial row or audit trail.
func TestSuppliedSecretWithoutAtRestKeyWritesNothing(t *testing.T) {
	s, ctx := testStore(t)
	ep := originEndpoint(t, s, ctx, "originociph")

	_, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: ep.ID, SourceType: "stripe", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_originociph",
		Secret: "whsec_CannotBeStoredInPlainText", SecretSupplied: true,
		Actor: SecretActor{Kind: SecretActorEndpoint, ID: ep.ID, Path: SecretPathMCP},
	})
	if !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("err = %v, want ErrEncryptionRequired", err)
	}
	if strings.Contains(err.Error(), "whsec_CannotBeStoredInPlainText") {
		t.Errorf("error text echoes the secret: %v", err)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM endpoint_webhooks`); n != 0 {
		t.Errorf("refused create left %d webhook rows", n)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM webhook_secret_audit`); n != 0 {
		t.Errorf("refused create left %d audit rows", n)
	}
}

// TestMalformedSuppliedSecretWritesNothing is REQ-3's shape rule at the store: the web and API paths
// cannot bypass the check the MCP layer also runs, and a refused value leaves nothing behind.
func TestMalformedSuppliedSecretWritesNothing(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	ep := originEndpoint(t, s, ctx, "origbad")

	const bad = "not_a_stripe_key_0123456789"
	_, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: ep.ID, SourceType: "stripe", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_origbad", Secret: bad, SecretSupplied: true,
		Actor: SecretActor{Kind: SecretActorEndpoint, ID: ep.ID, Path: SecretPathMCP},
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("error text echoes the secret: %v", err)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM endpoint_webhooks WHERE ingest_token = 'tok_origbad'`); n != 0 {
		t.Errorf("refused create left %d webhook rows", n)
	}
	if n := countRows(t, s, ctx, `SELECT count(*) FROM webhook_secret_audit`); n != 0 {
		t.Errorf("refused create left %d audit rows", n)
	}
}

// TestRotateClearsSuppliedAndLeavesProviderSecret covers REQ-1's claim that rotate clears
// secret_supplied, and the invariant that rotating a provider webhook never overwrites the producer's
// secret with a value Stripe has never heard of.
func TestRotateClearsSuppliedAndLeavesProviderSecret(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	ep := originEndpoint(t, s, ctx, "origrot")

	supplied, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: ep.ID, SourceType: "github", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_origrot_a",
		Secret: "whsec_SuppliedForAMintedType11", SecretSupplied: true,
		Actor: SecretActor{Kind: SecretActorEndpoint, ID: ep.ID, Path: SecretPathMCP},
	})
	if err != nil {
		t.Fatalf("create supplied minted webhook: %v", err)
	}
	if !supplied.SecretSupplied {
		t.Fatal("supplied minted webhook does not say so")
	}
	rotated, err := s.RotateWebhookSecret(ctx, supplied.ID, ep.ID, "whsec_FreshlyMintedAfterRotate", "tok_origrot_a2")
	if err != nil {
		t.Fatalf("rotate supplied webhook: %v", err)
	}
	if rotated.SecretSupplied {
		t.Error("rotate left secret_supplied set")
	}
	if rotated.SecretFingerprint != SecretFingerprint("whsec_FreshlyMintedAfterRotate") {
		t.Errorf("rotate fingerprint = %q, want the freshly stored secret's", rotated.SecretFingerprint)
	}

	provider, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: ep.ID, SourceType: "stripe", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_origrot_b",
		Secret: "whsec_TheProvidersOwnSecret1", SecretSupplied: true,
		Actor: SecretActor{Kind: SecretActorEndpoint, ID: ep.ID, Path: SecretPathMCP},
	})
	if err != nil {
		t.Fatalf("create provider webhook with secret: %v", err)
	}
	after, err := s.RotateWebhookSecret(ctx, provider.ID, ep.ID, "whsec_MustBeIgnoredByRotate1", "tok_origrot_b2")
	if err != nil {
		t.Fatalf("rotate provider webhook: %v", err)
	}
	if after.SecretFingerprint != provider.SecretFingerprint {
		t.Errorf("rotate changed a provider webhook's secret fingerprint: %q -> %q",
			provider.SecretFingerprint, after.SecretFingerprint)
	}
	if after.SecretSupplied {
		t.Error("rotate left secret_supplied set on a provider webhook")
	}
	if _, got, err := s.GetWebhookSecretByToken(ctx, "tok_origrot_b2"); err != nil || got != "whsec_TheProvidersOwnSecret1" {
		t.Errorf("provider secret after rotate = %q (%v)", got, err)
	}
}

// TestSecretStateFollowsTheOwner is the tenancy rule for this story: another endpoint's list never
// shows the webhook, its fingerprint, or who wrote its secret, and reading it by id is not_found.
func TestSecretStateFollowsTheOwner(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	a := originEndpoint(t, s, ctx, "origtena")
	b := originEndpoint(t, s, ctx, "origtenb")
	const secret = "whsec_OnlyTenantA MayEverReadThis"

	w, err := s.CreateWebhookWithOrigin(ctx, CreateWebhookInput{
		EndpointID: a.ID, SourceType: "stripe", TargetQueue: "reviews",
		Max: 5, TrustMode: "signed", IngestToken: "tok_origtena", Secret: secret, SecretSupplied: true,
		Actor: SecretActor{Kind: SecretActorEndpoint, ID: a.ID, Path: SecretPathMCP},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := s.ListWebhooks(ctx, b.ID)
	if err != nil {
		t.Fatalf("list for B: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("B sees A's webhooks: %+v", list)
	}
	if _, err := s.WebhookForEndpoint(ctx, w.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("WebhookForEndpoint for another tenant = %v, want ErrNotFound", err)
	}
	mine, err := s.ListWebhooks(ctx, a.ID)
	if err != nil || len(mine) != 1 || mine[0].SecretSetByID != a.ID {
		t.Fatalf("A's list = %+v (%v)", mine, err)
	}
}
