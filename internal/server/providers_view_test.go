// End-to-end Providers view + lifecycle tests through the real router + PostgreSQL: the SPEC-0017
// view renders trust chips over the registry with NO secret material, and the lifecycle POSTs
// (session + CSRF) actually bite on the ingestion path — disable rejects new deliveries while
// history stays queryable, rotate kills the old secret and reveals the new one exactly once, and
// remove kills the route while events/todos survive. Skipped without SWITCHBOARD_TEST_DATABASE_URL
// (no DSN configured), matching the ownership_test pattern.
//
// Governing: SPEC-0017 REQ "Providers View" (scenario "Trust at a glance"), REQ "Provider
// Lifecycle" (scenario "Disable stops the line"); ADR-0020.
package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/store"
)

// postWebhook delivers a generic webhook with a shared-secret token (empty = none presented).
func postWebhook(t *testing.T, r chi.Router, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Webhook-Token", token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

var revealSecretRe = regexp.MustCompile(`aria-labelledby="sb-prreveal-secret-label">([0-9a-f]{64})<`)

func TestProvidersLifecycleFlow(t *testing.T) {
	r, st, ctx, legacyEP := newReceiverDBRouter(t)
	// The operator reads below are tenant-scoped now, so they need the human who owns the
	// receiver's endpoint — the same principal whose board these events would appear on.
	receiverOwner, err := st.EndpointOwner(ctx, legacyEP.ID)
	if err != nil {
		t.Fatalf("receiver endpoint owner: %v", err)
	}
	_, token := mintSession(t, st, ctx, "prov-op", "Prov Op", "prov@example.com")

	// A token provider with a known held secret, as the boot env import would seed it.
	if _, err := st.SeedProvider(ctx, store.ProviderSeed{
		Name: "homelab", Family: "webhook", Kind: "generic", TrustMode: "token",
		Secret: "tok-old-plaintext", Config: []byte(`{"queue":"lab"}`),
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// Scenario "Trust at a glance": the view shows the enforced trust mode as a chip and never the
	// held secret — only its presence classification.
	page := getAs(t, r, token, "/providers")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /providers: %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{`id="sb-pr-homelab"`, `data-sb-trust="token"`, `data-sb-secret-status="configured"`} {
		if !strings.Contains(body, want) {
			t.Errorf("providers view missing %q", want)
		}
	}
	if strings.Contains(body, "tok-old-plaintext") {
		t.Fatal("the held secret must NEVER render on the Providers view")
	}
	csrf := scrapeCSRF(t, body)

	// The line accepts a correctly-tokened delivery.
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", "tok-old-plaintext", `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("ingest before disable: %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	// Scenario "Disable stops the line": the ingestion URL rejects new calls immediately…
	if rec := postFormAs(t, r, token, csrf, "/providers/homelab/disable", nil); rec.Code != http.StatusOK {
		t.Fatalf("POST disable: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", "tok-old-plaintext", `{"n":2}`); rec.Code != http.StatusForbidden {
		t.Fatalf("ingest after disable: %d, want 403", rec.Code)
	}
	// …while existing events and todos remain queryable.
	events, err := st.RecentEvents(ctx, receiverOwner, 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	var sawHomelab bool
	for _, e := range events {
		if e.Source == "homelab" {
			sawHomelab = true
		}
	}
	if !sawHomelab {
		t.Fatal("disable must keep previously ingested events queryable")
	}

	// Re-enable restores the line as it was.
	if rec := postFormAs(t, r, token, csrf, "/providers/homelab/enable", nil); rec.Code != http.StatusOK {
		t.Fatalf("POST enable: %d", rec.Code)
	}
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", "tok-old-plaintext", `{"n":3}`); rec.Code != http.StatusAccepted {
		t.Fatalf("ingest after re-enable: %d, want 202", rec.Code)
	}

	// Rotate: the response reveals a fresh secret exactly once; the old secret is dead on the next
	// delivery and the new one authenticates — no restart anywhere.
	rec := postFormAs(t, r, token, csrf, "/providers/homelab/rotate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rotate: %d (%s)", rec.Code, rec.Body.String())
	}
	m := revealSecretRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("rotate response must reveal the new secret once: %.400s", rec.Body.String())
	}
	newSecret := m[1]
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", "tok-old-plaintext", `{"n":4}`); rec.Code != http.StatusForbidden {
		t.Fatalf("old secret after rotate: %d, want 403", rec.Code)
	}
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", newSecret, `{"n":5}`); rec.Code != http.StatusAccepted {
		t.Fatalf("new secret after rotate: %d, want 202", rec.Code)
	}
	// The refreshed Providers view still renders no secret material.
	if body := getAs(t, r, token, "/providers").Body.String(); strings.Contains(body, newSecret) {
		t.Fatal("the rotated secret must not render outside the one-time reveal")
	}

	// Remove: the confirmation renders (no-JS gets the full page with the inline confirm), the POST
	// kills the route, and history — events and todos — survives the removal.
	conf := getAs(t, r, token, "/providers/homelab/confirm/remove")
	if conf.Code != http.StatusOK || !strings.Contains(conf.Body.String(), `data-sb-provider-confirm="remove"`) {
		t.Fatalf("confirm remove: %d, body must carry the confirmation", conf.Code)
	}
	if rec := postFormAs(t, r, token, csrf, "/providers/homelab/remove", nil); rec.Code != http.StatusOK {
		t.Fatalf("POST remove: %d", rec.Code)
	}
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", newSecret, `{"n":6}`); rec.Code != http.StatusNotFound {
		t.Fatalf("ingest after remove: %d, want 404", rec.Code)
	}
	if body := getAs(t, r, token, "/providers").Body.String(); strings.Contains(body, `id="sb-pr-homelab"`) {
		t.Fatal("removed provider must leave the view")
	}
	events, err = st.RecentEvents(ctx, receiverOwner, 10)
	if err != nil {
		t.Fatalf("recent events after remove: %v", err)
	}
	sawHomelab = false
	for _, e := range events {
		if e.Source == "homelab" {
			sawHomelab = true
		}
	}
	if !sawHomelab {
		t.Fatal("removal must NOT delete previously ingested events")
	}
	counts, err := st.TodoCounts(ctx, receiverOwner)
	if err != nil {
		t.Fatalf("todo counts: %v", err)
	}
	if counts.All == 0 {
		t.Fatal("removal must NOT delete todos")
	}

	// Rotate on a kind with no rotatable secret is refused.
	if _, err := st.SeedProvider(ctx, store.ProviderSeed{
		Name: "lan", Family: "webhook", Kind: "generic", TrustMode: "open",
	}); err != nil {
		t.Fatalf("seed open provider: %v", err)
	}
	if rec := postFormAs(t, r, token, csrf, "/providers/lan/rotate", nil); rec.Code != http.StatusConflict {
		t.Fatalf("rotate open provider: %d, want 409", rec.Code)
	}
}
