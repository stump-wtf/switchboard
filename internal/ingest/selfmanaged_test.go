package ingest

// Tests for the SPEC-0006 self-managed webhook receiver (POST /webhooks/w/{token}). The verified
// signed round-trip, the wrong-signature rejection, the token-mode accept, and the unknown-token case
// are DB-backed and skip cleanly without SWITCHBOARD_TEST_DATABASE_URL (Gitea CI is the gate; the
// GitHub mirror runs them against Postgres); the oversized-body rejection needs no store.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/store"
)

// selfManagedRequest builds a POST /webhooks/w/{token} request with the chi URL param populated,
// mirroring how the router invokes the handler (the plain `post` helper does not set route params).
func selfManagedRequest(token, body string, hdr map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/w/"+token, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("token", token)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// postSelfManaged drives the receiver with the chi route param set, returning the recorder.
func postSelfManaged(ing *Ingest, token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ing.SelfManaged(rec, selfManagedRequest(token, body, hdr))
	return rec
}

// githubSig computes the X-Hub-Signature-256 header value GitHub sends: sha256=<hmac-sha256(secret, body)>.
func githubSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// An oversized body is rejected 413 before the token is ever looked up — the nil store proves no
// persist is attempted. Governing: SPEC-0001 REQ "Request Body Size Limits".
func TestSelfManagedRejectsOversizedBody(t *testing.T) {
	ing := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	rec := httptest.NewRecorder()
	ing.SelfManaged(rec, selfManagedRequest("any-token", strings.Repeat("x", maxBody+1), nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized self-managed body: got %d, want 413", rec.Code)
	}
}

// seedWebhook creates a human → agent → endpoint → webhook and returns nothing; the caller drives
// deliveries by the ingest token. secret is the minted signing secret switchboard holds (for a signed
// webhook the receiver recomputes the HMAC against it).
func seedWebhook(t *testing.T, st *store.Store, ctx context.Context, sourceType, trustMode, queue, token, secret string) {
	t.Helper()
	h, err := st.UpsertHuman(ctx, "pocket|"+token, "Joe", "")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	ag, err := st.CreateAgent(ctx, h.ID, "hook-bot-"+token, "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, "credhash-"+token, "sbk_seed01", slug, []string{queue}, []string{"create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	if _, err := st.CreateWebhook(ctx, ep.ID, sourceType, queue, trustMode, token, secret, 3); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
}

// The signed happy path: switchboard holds the minted secret, so a delivery signed with that secret
// verifies EXACTLY as a human-configured signed github webhook — verified=true, trust_mode=signed,
// verify_detail "hmac-sha256 ok" — and becomes a todo on the target queue. A redelivery of the same
// delivery id dedups to the SAME todo. Governing: SPEC-0006 REQ "Switchboard Owns Secrets,
// Verification, and Idempotency"; SPEC-0003 (per-provider HMAC).
func TestSelfManagedSignedVerifiedRoundTrip(t *testing.T) {
	ing, _, pool, ctx := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_roundtripsecret"
	seedWebhook(t, st, ctx, "github", "signed", "reviews", "route-token-signed", secret)

	body := `{"action":"opened","number":7}`
	rec := postSelfManaged(ing, "route-token-signed", body,
		map[string]string{"X-GitHub-Delivery": "guid-1", "X-Hub-Signature-256": githubSig(secret, body)})
	id1, queue := accepted202(t, rec)
	if queue != "reviews" {
		t.Fatalf("queue = %q, want reviews", queue)
	}
	// The 202 body reports the webhook's trust mode and that the body WAS verified.
	var resp struct {
		TrustMode string `json:"trust_mode"`
		Verified  bool   `json:"verified"`
	}
	decode(t, rec.Body.Bytes(), &resp)
	if resp.TrustMode != "signed" || !resp.Verified {
		t.Fatalf("202 = %+v, want trust_mode=signed verified=true", resp)
	}

	// The persisted event is verified=true under trust_mode=signed — identical to a human-configured
	// signed webhook.
	var mode, detail string
	var verified bool
	if err := pool.QueryRow(ctx,
		`SELECT trust_mode, verified, COALESCE(verify_detail,'') FROM events WHERE source='github' AND external_id LIKE '%:guid-1'`,
	).Scan(&mode, &verified, &detail); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if mode != "signed" || !verified || !strings.Contains(detail, "hmac") {
		t.Fatalf("event = (mode=%q verified=%v detail=%q), want signed/true/hmac", mode, verified, detail)
	}

	// Redelivery of the same GitHub delivery id dedups to the same todo — exactly one row.
	id2, _ := accepted202(t, postSelfManaged(ing, "route-token-signed", body,
		map[string]string{"X-GitHub-Delivery": "guid-1", "X-Hub-Signature-256": githubSig(secret, body)}))
	if id2 != id1 {
		t.Fatalf("redelivery todo id = %q, want dedup to %q", id2, id1)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 1 {
		t.Fatalf("todos in reviews = %d, want 1 (dedup)", n)
	}
}

// A wrong signature on a signed self-managed webhook is rejected 401 and NOTHING is persisted — the
// receiver fails closed exactly like the operator-configured signed receivers, never faking trust.
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency"; SPEC-0003.
func TestSelfManagedSignedWrongSignatureRejected(t *testing.T) {
	ing, _, pool, ctx := testIngestDeps(t, Config{})
	st := store.New(pool)
	seedWebhook(t, st, ctx, "github", "signed", "reviews", "route-token-badsig", "whsec_realsecret")

	body := `{"action":"opened","number":9}`
	// Signed with the WRONG secret — the HMAC will not match switchboard's held secret.
	rec := postSelfManaged(ing, "route-token-badsig", body,
		map[string]string{"X-GitHub-Delivery": "guid-bad", "X-Hub-Signature-256": githubSig("whsec_attackersecret", body)})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong signature: got %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	// Nothing persisted: no event and no todo.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE source='github' AND external_id LIKE '%:guid-bad'`); n != 0 {
		t.Fatalf("events after rejected signature = %d, want 0 (no persist)", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 0 {
		t.Fatalf("todos after rejected signature = %d, want 0 (no persist)", n)
	}

	// A missing signature is likewise rejected without persisting.
	if rec := postSelfManaged(ing, "route-token-badsig", body, map[string]string{"X-GitHub-Delivery": "guid-none"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: got %d, want 401", rec.Code)
	}
}

// A token-mode self-managed webhook (generic source type) has no signing secret: the unguessable
// ingest URL authenticates the caller, the body is not signature-verified, so the delivery persists
// honestly with verified=false and trust_mode=token — never presented as signed.
func TestSelfManagedTokenModeUnverified(t *testing.T) {
	ing, _, pool, ctx := testIngestDeps(t, Config{})
	st := store.New(pool)
	seedWebhook(t, st, ctx, "generic", "token", "reviews", "route-token-tokenmode", "")

	body := `{"hello":"world"}`
	rec := postSelfManaged(ing, "route-token-tokenmode", body, nil)
	_, queue := accepted202(t, rec)
	if queue != "reviews" {
		t.Fatalf("queue = %q, want reviews", queue)
	}
	var resp struct {
		TrustMode string `json:"trust_mode"`
		Verified  bool   `json:"verified"`
	}
	decode(t, rec.Body.Bytes(), &resp)
	if resp.TrustMode != "token" || resp.Verified {
		t.Fatalf("202 = %+v, want trust_mode=token verified=false", resp)
	}
	var mode string
	var verified bool
	if err := pool.QueryRow(ctx,
		`SELECT trust_mode, verified FROM events WHERE source='generic' ORDER BY id DESC LIMIT 1`,
	).Scan(&mode, &verified); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if mode != "token" || verified {
		t.Fatalf("event = (mode=%q verified=%v), want token/false", mode, verified)
	}
}

// An unknown ingest token is 404 and persists nothing — a guessed URL cannot manufacture a todo.
func TestSelfManagedUnknownTokenIs404(t *testing.T) {
	ing, _, _, _ := testIngestDeps(t, Config{})
	rec := postSelfManaged(ing, "no-such-token", `{"x":1}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}
