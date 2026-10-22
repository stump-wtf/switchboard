package ingest

// A signed webhook whose signing secret has never been supplied answers 503 and persists nothing —
// SPEC-0032 REQ-2's awaiting_secret. The message and the metric reason differ by how the webhook got
// into that state: a provider-origin webhook is waiting for its owner to paste the producer's secret,
// a minted-origin one with no secret is a misconfiguration.
//
// Governing: SPEC-0032 REQ-2, REQ-12; SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and
// Idempotency" (a delivery that cannot be verified is refused, never trusted).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

// awaitingWebhook seeds a webhook that verifies signed deliveries but holds no secret.
func awaitingWebhook(t *testing.T, st *store.Store, ctx context.Context, sourceType, token string) store.Webhook {
	t.Helper()
	_, _, wh := seedWebhook(t, st, ctx, sourceType, "signed", "reviews", token, "")
	if wh.SecretSet {
		t.Fatalf("seeded webhook already holds a secret: %+v", wh)
	}
	return wh
}

// TestAwaitingSecretRefusesDelivery is REQ-2's scenario at the receiver: the provider webhook answers
// 503 naming the state, nothing is stored, and the counted reason is awaiting_secret — not
// bad_signature, which would blame the producer for a secret nobody pasted.
func TestAwaitingSecretRefusesDelivery(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	wh := awaitingWebhook(t, st, ctx, "stripe", "tok-awaiting-secret")

	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	body := `{"type":"charge.succeeded"}`
	res := postSelfManaged(ing, "tok-awaiting-secret", body, map[string]string{
		"Stripe-Signature": "t=1700000000,v1=deadbeef",
	})
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (body %s)", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "webhook awaiting signing secret") {
		t.Errorf("body = %s, want it to name the awaiting-secret state", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "deadbeef") {
		t.Errorf("response echoed request material: %s", res.Body.String())
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != 0 {
		t.Errorf("refused delivery persisted %d events", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 0 {
		t.Errorf("refused delivery persisted %d todos", n)
	}
	var reasons []string
	for _, f := range rec.failures {
		reasons = append(reasons, f.provider+"="+f.reason)
	}
	if len(reasons) != 1 || reasons[0] != "stripe=awaiting_secret" {
		t.Errorf("verify failures = %v, want [stripe=awaiting_secret]", reasons)
	}
	if len(rec.deliveries) != 1 || rec.deliveries[0].verdict != verdictRejected {
		t.Errorf("delivery verdicts = %+v, want one rejected", rec.deliveries)
	}
	// The row is readable by its own receiver path and still reports no secret.
	if _, secret, err := st.GetWebhookSecretByToken(ctx, wh.IngestToken); err != nil || secret != "" {
		t.Errorf("GetWebhookSecretByToken = %q (%v), want empty", secret, err)
	}
}

// TestMissingMintedSecretStillReportsNotConfigured keeps the older message honest: a minted-origin
// webhook with no secret is not awaiting a provider's value, it is broken, and the reason stays
// not_configured so the two states do not blur in the dashboard.
func TestMissingMintedSecretStillReportsNotConfigured(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	awaitingWebhook(t, st, ctx, "github", "tok-minted-no-secret")

	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	body := `{"action":"opened"}`
	res := postSelfManaged(ing, "tok-minted-no-secret", body, map[string]string{
		"X-Hub-Signature-256": githubSig("whsec_whateverTheClientTried", body),
	})
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (body %s)", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "webhook not configured") {
		t.Errorf("body = %s, want the misconfiguration message", res.Body.String())
	}
	if len(rec.failures) != 1 || rec.failures[0].reason != reasonNotConfigured {
		t.Errorf("verify failures = %+v, want reason %s", rec.failures, reasonNotConfigured)
	}
}
