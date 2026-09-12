package ingest

// Registry-resolving dispatch tests (ADR-0020, SPEC-0017 REQ "Runtime Provider Registry" /
// "Environment Config Import"): the webhook receivers resolve providers from the DB-backed
// registry at request time — a provider created in the registry is live with no restart and no
// entry in the boot-time env map, a registry row wins over drifted env config, and a disabled row
// stops the line. DB-backed like the other accept-path tests; skips cleanly without
// SWITCHBOARD_TEST_DATABASE_URL.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stump-wtf/switchboard/internal/store"
)

// testRegistryIngest builds an Ingest whose store the test can also seed registry rows through,
// plus the pool for row-level asserts. Same DB isolation as testIngest (ingestTestPool).
//
// The registry decides a provider's SECRET and QUEUE; it does not decide tenancy. An
// operator-configured receiver still needs an owning endpoint for the todo it mints (ADR-0022;
// todos.endpoint_id NOT NULL), so the legacy endpoint is seeded and wired here exactly as in
// testIngest — otherwise the 202 assertions below would come back 503.
func testRegistryIngest(t *testing.T, cfg Config) (*Ingest, *store.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := ingestTestPool(t)
	st := store.New(pool)
	cfg.LegacyEndpointID = seedLegacyEndpoint(t, st, ctx)
	ing := New(st, NewHub(), slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	return ing, st, pool, ctx
}

// seedProvider seeds one provider registry row through the store, failing the test on error.
func seedProvider(t *testing.T, st *store.Store, ctx context.Context, seed store.ProviderSeed) {
	t.Helper()
	if _, err := st.SeedProvider(ctx, seed); err != nil {
		t.Fatalf("seed provider %s: %v", seed.Name, err)
	}
}

// registryEventRow reads the persisted trust classification of the single event for source.
func registryEventRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, source string) (string, bool) {
	t.Helper()
	var mode string
	var verified bool
	if err := pool.QueryRow(ctx,
		`SELECT trust_mode, verified FROM events WHERE source = $1`, source).Scan(&mode, &verified); err != nil {
		t.Fatalf("query event for %s: %v", source, err)
	}
	return mode, verified
}

// SPEC-0017 scenario "Wizard-created provider is live immediately" (dispatch half): a generic
// token provider that exists ONLY in the registry — nothing in the boot-time env map — accepts and
// trust-checks deliveries at its ingestion URL without any restart or re-wiring.
func TestGenericRegistryProviderLiveWithoutRestart(t *testing.T) {
	ing, st, pool, ctx := testRegistryIngest(t, Config{}) // empty boot map: the registry is the source

	seedProvider(t, st, ctx, store.ProviderSeed{
		Name: "wizard", Family: "webhook", Kind: "generic", TrustMode: "token",
		Secret: "tok-wiz", Config: []byte(`{"queue":"wizq"}`),
	})

	// Wrong token: trust-checked and rejected, nothing persisted.
	rec := httptest.NewRecorder()
	ing.Generic(rec, genericRequest("wizard", `{"n":1}`, map[string]string{"X-Webhook-Token": "nope"}, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	// Correct token: accepted as token trust, routed to the registry-configured queue.
	rec = httptest.NewRecorder()
	ing.Generic(rec, genericRequest("wizard", `{"n":1}`, map[string]string{"X-Webhook-Token": "tok-wiz"}, ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid token: got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"queue":"wizq"`) {
		t.Fatalf("todo must land on the registry-configured queue: %s", rec.Body.String())
	}
	mode, verified := registryEventRow(t, pool, ctx, "wizard")
	if mode != "token" || verified {
		t.Fatalf("persisted trust wrong: mode=%q verified=%v", mode, verified)
	}
}

// SPEC-0017 REQ "Environment Config Import": after import the registry is authoritative — for a
// name both the registry and the env map hold, the registry row's secret and queue decide the
// request, and drifted env values no longer authenticate anything.
func TestGenericRegistryWinsOverEnvConfig(t *testing.T) {
	ing, st, _, ctx := testRegistryIngest(t, Config{
		Generic: map[string]GenericProvider{"dockerhub": {Mode: "token", Token: "env-tok", Queue: "envq"}},
	})

	seedProvider(t, st, ctx, store.ProviderSeed{
		Name: "dockerhub", Family: "webhook", Kind: "generic", TrustMode: "token",
		Secret: "reg-tok", Config: []byte(`{"queue":"regq"}`),
	})

	// The drifted env token must NOT authenticate: the registry row wins.
	rec := httptest.NewRecorder()
	ing.Generic(rec, genericRequest("dockerhub", `{"tag":"latest"}`, map[string]string{"X-Webhook-Token": "env-tok"}, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("env token against registry row: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	ing.Generic(rec, genericRequest("dockerhub", `{"tag":"latest"}`, map[string]string{"X-Webhook-Token": "reg-tok"}, ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("registry token: got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"queue":"regq"`) {
		t.Fatalf("registry queue must win over env queue: %s", rec.Body.String())
	}
}

// SPEC-0017 REQ "Provider Lifecycle" (scenario "Disable stops the line"), dispatch half: a
// disabled registry provider rejects new deliveries at request time — no restart — and persists
// nothing; the flip is visible immediately because registry writes invalidate the dispatch cache.
func TestGenericRegistryDisabledRejects(t *testing.T) {
	ing, st, pool, ctx := testRegistryIngest(t, Config{})

	seedProvider(t, st, ctx, store.ProviderSeed{
		Name: "homelab", Family: "webhook", Kind: "generic", TrustMode: "open",
	})

	rec := httptest.NewRecorder()
	ing.Generic(rec, genericRequest("homelab", `{"n":1}`, nil, ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enabled open provider: got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}

	if err := st.SetAdapterEnabled(ctx, "homelab", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	rec = httptest.NewRecorder()
	ing.Generic(rec, genericRequest("homelab", `{"n":2}`, nil, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled provider: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source = 'homelab'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("disabled delivery must not persist: %d events, want 1", events)
	}
}

// SPEC-0017 REQ "Runtime Provider Registry": signed adapters read their secret registry-or-env —
// a registry row's (envelope-held) secret verifies deliveries even with no env secret configured,
// and when both exist the registry secret wins. SPEC-0001 verification semantics are untouched:
// same HMAC scheme, same 401-without-persist on mismatch.
func TestSignedSecretRegistryOrEnv(t *testing.T) {
	ing, st, pool, ctx := testRegistryIngest(t, Config{GitHubSecret: "env-secret"})

	seedProvider(t, st, ctx, store.ProviderSeed{
		Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed",
		Secret: "reg-secret", Config: []byte(`{"queue":"reviews"}`),
	})

	body := `{"action":"opened"}`
	// Signed with the drifted ENV secret: the registry row wins, so this must 401.
	rec := httptest.NewRecorder()
	ing.GitHub(rec, githubRequest(body, sign("env-secret", []byte(body))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("env-signed against registry row: got %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}

	// Signed with the registry secret: verified and persisted as signed.
	rec = httptest.NewRecorder()
	ing.GitHub(rec, githubRequest(body, sign("reg-secret", []byte(body))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("registry-signed: got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	mode, verified := registryEventRow(t, pool, ctx, "github")
	if mode != "signed" || !verified {
		t.Fatalf("persisted trust wrong: mode=%q verified=%v", mode, verified)
	}
}

// A disabled signed provider's route rejects even a correctly signed delivery — the registry's
// enabled flag gates dispatch for the signed family too. Governing: SPEC-0017 REQ "Runtime
// Provider Registry", REQ "Provider Lifecycle".
func TestSignedRegistryDisabledRejects(t *testing.T) {
	ing, st, _, ctx := testRegistryIngest(t, Config{})

	seedProvider(t, st, ctx, store.ProviderSeed{
		Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed",
		Secret: "reg-secret", Config: []byte(`{"queue":"reviews"}`),
	})
	if err := st.SetAdapterEnabled(ctx, "github", false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	body := `{"action":"opened"}`
	rec := httptest.NewRecorder()
	ing.GitHub(rec, githubRequest(body, sign("reg-secret", []byte(body))))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled signed provider: got %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// githubRequest builds a signed-GitHub-webhook request for the receiver under test.
func githubRequest(body, sig string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sig)
	return req
}
