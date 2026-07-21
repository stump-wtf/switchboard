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

// seedWebhook creates a human → agent → endpoint → webhook and returns all three principals; the
// caller drives deliveries by the ingest token. secret is the minted signing secret switchboard holds
// (for a signed webhook the receiver recomputes the HMAC against it).
//
// It returns the human, the OWNING endpoint and the webhook — it previously returned nothing, which
// was sufficient only while a delivery could mint exactly one todo. Under ADR-0022 a delivery fans
// out to {owner} ∪ webhook_routes, and a route is (webhook id, target endpoint id, granting human),
// so a test cannot express a second target without holding the webhook's id, the granting human's
// id, and a second endpoint to point at.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func seedWebhook(t *testing.T, st *store.Store, ctx context.Context, sourceType, trustMode, queue, token, secret string) (store.Human, store.Endpoint, store.Webhook) {
	t.Helper()
	h, ep := seedEndpoint(t, st, ctx, "hook-"+token, []string{queue})
	wh, err := st.CreateWebhook(ctx, ep.ID, sourceType, queue, trustMode, token, secret, 3)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return h, ep, wh
}

// The signed happy path: switchboard holds the minted secret, so a delivery signed with that secret
// verifies EXACTLY as a human-configured signed github webhook — verified=true, trust_mode=signed,
// verify_detail "hmac-sha256 ok" — and becomes a todo on the target queue. A redelivery of the same
// delivery id dedups to the SAME todo. Governing: SPEC-0006 REQ "Switchboard Owns Secrets,
// Verification, and Idempotency"; SPEC-0003 (per-provider HMAC).
func TestSelfManagedSignedVerifiedRoundTrip(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_roundtripsecret"
	_, owner, _ := seedWebhook(t, st, ctx, "github", "signed", "reviews", "route-token-signed", secret)
	// Subscribe as the webhook's OWNING endpoint so the doorbell assertions below observe a real
	// match rather than a filtered-out miss (ADR-0022).
	ch, cancel := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancel()

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

	// The owning endpoint's doorbell rang exactly once for the newly-minted todo.
	if n := drainHub(ch); n != 1 {
		t.Fatalf("owner doorbell = %d, want 1 (publish only when newly created)", n)
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
	// A redelivery is not news: an already-live todo must NOT ring the doorbell again.
	if n := drainHub(ch); n != 0 {
		t.Fatalf("redelivery doorbell = %d, want 0", n)
	}
	// The persisted todo is pinned to the webhook's owning endpoint — never a null or foreign tenant.
	// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
	var gotEndpoint string
	if err := pool.QueryRow(ctx,
		`SELECT endpoint_id::text FROM todos WHERE queue='reviews'`).Scan(&gotEndpoint); err != nil {
		t.Fatalf("read todo endpoint: %v", err)
	}
	if gotEndpoint != owner.ID {
		t.Fatalf("todo endpoint_id = %q, want the webhook owner %q", gotEndpoint, owner.ID)
	}
}

// Fan-out across a webhook_routes grant: one delivery to a webhook owned by endpoint A that is also
// routed to endpoint B mints TWO todos — one per target, each pinned to its own endpoint — and each
// target's doorbell rings only for its own row. This is the push half of the cross-tenant leak
// ADR-0022 closes: before the endpoint dimension existed, A and B shared the "reviews" queue string
// and so shared a single todo and each other's doorbell, across humans.
//
// The per-target idempotency keys must differ (CreateEventTodos prefixes each target's endpoint id),
// or the second target would collapse onto the first target's row and the fan-out would silently
// deliver to one tenant instead of two.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
func TestSelfManagedFanOutIsPerEndpoint(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_fanout"
	ownerHuman, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "route-token-fanout", secret)
	// A second, independent tenant sharing the SAME queue string — the exact collision that used to
	// leak. It receives this webhook's deliveries ONLY because of the explicit route below.
	friendHuman, friend := seedEndpoint(t, st, ctx, "fanout-friend", []string{"reviews"})
	grantFriendEdge(t, st, ctx, "fanout", ownerHuman.ID, friendHuman.ID)
	if err := st.AddWebhookRoute(ctx, wh.ID, friend.ID, ownerHuman.ID); err != nil {
		t.Fatalf("add webhook route: %v", err)
	}

	ownerCh, cancelOwner := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancelOwner()
	friendCh, cancelFriend := hub.Subscribe(friend.ID, []string{"reviews"})
	defer cancelFriend()
	// A third tenant on the same queue with NO route: it must observe nothing at all.
	_, stranger := seedEndpoint(t, st, ctx, "fanout-stranger", []string{"reviews"})
	strangerCh, cancelStranger := hub.Subscribe(stranger.ID, []string{"reviews"})
	defer cancelStranger()

	body := `{"action":"opened","number":11}`
	hdr := map[string]string{"X-GitHub-Delivery": "guid-fan", "X-Hub-Signature-256": githubSig(secret, body)}
	todos, created := fanOut202(t, postSelfManaged(ing, "route-token-fanout", body, hdr))

	if len(todos) != 2 || created != 2 {
		t.Fatalf("fan-out = %d todos (%d created), want 2/2: %+v", len(todos), created, todos)
	}
	// ResolveWebhookTargets returns the owner first, so the response's compatibility `id` keeps
	// naming the owner's todo.
	if todos[0].EndpointID != owner.ID {
		t.Fatalf("todos[0].endpoint_id = %q, want the owner %q", todos[0].EndpointID, owner.ID)
	}
	if todos[1].EndpointID != friend.ID {
		t.Fatalf("todos[1].endpoint_id = %q, want the routed target %q", todos[1].EndpointID, friend.ID)
	}
	if todos[0].ID == todos[1].ID {
		t.Fatalf("fan-out must mint a DISTINCT todo per endpoint, got the same id %q", todos[0].ID)
	}

	// Two rows, one per endpoint, sharing ONE idempotency key: the separation lives in the
	// (endpoint_id, idempotency_key) composite index (migration 0012), not in the key text. Both
	// halves are asserted — one key (so the row stays join-compatible with the event's external_id,
	// which the Board's dedup count and received-card retirement depend on) across two distinct
	// endpoint_ids (so the two tenants hold independent dedup slots).
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 2 {
		t.Fatalf("todos in reviews = %d, want 2 (one per target endpoint)", n)
	}
	if n := countRows(t, ctx, pool,
		`SELECT count(DISTINCT idempotency_key) FROM todos WHERE queue='reviews'`); n != 1 {
		t.Fatalf("distinct idempotency keys = %d, want 1 (one delivery mints one key)", n)
	}
	if n := countRows(t, ctx, pool,
		`SELECT count(DISTINCT endpoint_id) FROM todos WHERE queue='reviews'`); n != 2 {
		t.Fatalf("distinct endpoint_ids = %d, want 2 (dedup namespace is per-endpoint)", n)
	}

	// Each target's doorbell rang exactly once, for its own todo — and the unrouted stranger, who
	// shares the queue string, heard nothing.
	if n := drainHub(ownerCh); n != 1 {
		t.Fatalf("owner doorbell = %d, want 1", n)
	}
	if n := drainHub(friendCh); n != 1 {
		t.Fatalf("routed target doorbell = %d, want 1", n)
	}
	if n := drainHub(strangerCh); n != 0 {
		t.Fatalf("unrouted endpoint sharing the queue string heard %d todos, want 0 — cross-tenant leak", n)
	}

	// A redelivery collapses independently within EACH target: still two rows, nothing newly created,
	// and no second doorbell for anyone.
	_, createdAgain := fanOut202(t, postSelfManaged(ing, "route-token-fanout", body, hdr))
	if createdAgain != 0 {
		t.Fatalf("redelivery created = %d, want 0 (each target collapses onto its own row)", createdAgain)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 2 {
		t.Fatalf("todos after redelivery = %d, want 2", n)
	}
	if n := drainHub(ownerCh) + drainHub(friendCh); n != 0 {
		t.Fatalf("redelivery doorbells = %d, want 0", n)
	}
}

// A wrong signature on a signed self-managed webhook is rejected 401 and NOTHING is persisted — the
// receiver fails closed exactly like the operator-configured signed receivers, never faking trust.
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency"; SPEC-0003.
func TestSelfManagedSignedWrongSignatureRejected(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
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
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
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
	ing, _, _, _, _ := testIngestDeps(t, Config{})
	rec := postSelfManaged(ing, "no-such-token", `{"x":1}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}
