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

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
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
	wh, err := st.CreateWebhookWithTrust(ctx, ep.ID, sourceType, queue, trustMode, token, secret, 3, allowAllFor(sourceType))
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return h, ep, wh
}

// allowAllFor is the trust list the receiver tests seed: {"allow_all": true} on a source with a
// trust gate, which is exactly what migration 0023 gave every webhook that existed before the gate.
// These tests exercise verification, dedup and routing, not trust, so they run past the gate. The
// gate itself is tested in trust_gate_test.go, starting from the fail-closed empty list.
func allowAllFor(sourceType string) []byte {
	if routing.HasActorProjection(sourceType) {
		return []byte(`{"allow_all":true}`)
	}
	return nil
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

// giteaSig computes the X-Gitea-Signature header value: bare hex HMAC-SHA256(secret, body), no
// "sha256=" prefix (that is GitHub's format).
func giteaSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// The self-managed gitea signed happy path: X-Gitea-Signature uses bare hex HMAC-SHA256 (no
// "sha256=" prefix), X-Gitea-Delivery provides the idempotency key, and the persisted event is
// verified=true under trust_mode=signed. A redelivery of the same delivery id dedups.
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency"; ADR-0003.
func TestSelfManagedGiteaSignedVerifiedRoundTrip(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_gitearounds"
	_, owner, _ := seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "route-token-gitea", secret)
	ch, cancel := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancel()

	body := `{"action":"opened","pull_request":{"number":42,"title":"Add gitea support","user":{"login":"alice"}},"repository":{"full_name":"stump.wtf/switchboard"}}`
	rec := postSelfManaged(ing, "route-token-gitea", body,
		map[string]string{"X-Gitea-Delivery": "guid-g1", "X-Gitea-Signature": giteaSig(secret, body)})
	id1, queue := accepted202(t, rec)
	if queue != "reviews" {
		t.Fatalf("queue = %q, want reviews", queue)
	}
	var resp struct {
		TrustMode string `json:"trust_mode"`
		Verified  bool   `json:"verified"`
	}
	decode(t, rec.Body.Bytes(), &resp)
	if resp.TrustMode != "signed" || !resp.Verified {
		t.Fatalf("202 = %+v, want trust_mode=signed verified=true", resp)
	}

	var mode, detail string
	var verified bool
	if err := pool.QueryRow(ctx,
		`SELECT trust_mode, verified, COALESCE(verify_detail,'') FROM events WHERE source='gitea' AND external_id LIKE '%:guid-g1'`,
	).Scan(&mode, &verified, &detail); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if mode != "signed" || !verified || !strings.Contains(detail, "hmac") {
		t.Fatalf("event = (mode=%q verified=%v detail=%q), want signed/true/hmac", mode, verified, detail)
	}

	if n := drainHub(ch); n != 1 {
		t.Fatalf("owner doorbell = %d, want 1", n)
	}

	// Redelivery dedup.
	id2, _ := accepted202(t, postSelfManaged(ing, "route-token-gitea", body,
		map[string]string{"X-Gitea-Delivery": "guid-g1", "X-Gitea-Signature": giteaSig(secret, body)}))
	if id2 != id1 {
		t.Fatalf("redelivery todo id = %q, want dedup to %q", id2, id1)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 1 {
		t.Fatalf("todos in reviews = %d, want 1 (dedup)", n)
	}
	if n := drainHub(ch); n != 0 {
		t.Fatalf("redelivery doorbell = %d, want 0", n)
	}
}

// A self-managed gitea delivery with an invalid signature is rejected 401 and nothing is persisted.
func TestSelfManagedGiteaBadSignatureRejected(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_giteabad"
	seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "route-token-gitea-bad", secret)

	body := `{"action":"opened"}`
	rec := postSelfManaged(ing, "route-token-gitea-bad", body,
		map[string]string{"X-Gitea-Delivery": "guid-bad", "X-Gitea-Signature": "deadbeef"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: got %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE source='gitea'`); n != 0 {
		t.Fatalf("rejected delivery must not persist: %d events", n)
	}
}

// A self-managed gitea delivery with a missing signature is rejected 401.
func TestSelfManagedGiteaMissingSignatureRejected(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_giteanone"
	seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "route-token-gitea-none", secret)

	rec := postSelfManaged(ing, "route-token-gitea-none", `{"action":"opened"}`,
		map[string]string{"X-Gitea-Delivery": "guid-none"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: got %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE source='gitea'`); n != 0 {
		t.Fatalf("rejected delivery must not persist: %d events", n)
	}
}

// verifyGitea unit tests: valid, invalid, empty signature.
func TestVerifyGitea(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"action":"opened"}`)
	good := giteaSig(secret, string(body))

	if !verifyGitea(secret, body, good) {
		t.Fatal("valid gitea signature must verify")
	}
	if verifyGitea("other", body, good) {
		t.Fatal("wrong secret must not verify")
	}
	if verifyGitea(secret, []byte(`{"action":"closed"}`), good) {
		t.Fatal("wrong body must not verify")
	}
	if verifyGitea(secret, body, "") || verifyGitea(secret, body, "deadbeef") {
		t.Fatal("empty or bogus signature must not verify")
	}
	// The sha256= prefix format that GitHub uses must NOT verify for gitea.
	githubFormat := "sha256=" + good
	if verifyGitea(secret, body, githubFormat) {
		t.Fatal("sha256= prefixed signature must not verify for gitea (bare hex only)")
	}
}

// summarizeSelfManagedTitle produces forge-aware titles for gitea payloads — the single gitea
// summarizer since the operator-configured receivers were stripped (issue #181).
func TestSummarizeSelfManagedTitleGitea(t *testing.T) {
	pr := `{"action":"opened","pull_request":{"number":7,"title":"Fix login","user":{"login":"bob"}},"repository":{"full_name":"stump.wtf/switchboard"}}`
	got := summarizeSelfManagedTitle("gitea", "pull_request", []byte(pr))
	if want := "PR #7 opened in stump.wtf/switchboard — Fix login"; got != want {
		t.Fatalf("gitea PR title = %q, want %q", got, want)
	}

	// Issue events get a real title too — the PR-only summarizer swallowed them into the generic
	// one-liner, which matters because issues route to their own queue.
	issue := `{"action":"opened","issue":{"number":3,"title":"Bug"},"repository":{"full_name":"o/r"}}`
	got = summarizeSelfManagedTitle("gitea", "issues", []byte(issue))
	if want := "Issue #3 opened in o/r — Bug"; got != want {
		t.Fatalf("gitea issue title = %q, want %q", got, want)
	}

	// An unknown event still names the provider and repo rather than going fully generic.
	got = summarizeSelfManagedTitle("gitea", "release", []byte(issue))
	if want := "gitea release in o/r"; got != want {
		t.Fatalf("gitea release title = %q, want %q", got, want)
	}

	// No event header at all → the generic one-liner; there is nothing to summarize against.
	got = summarizeSelfManagedTitle("gitea", "", []byte(pr))
	if got != "self-managed gitea delivery" {
		t.Fatalf("eventless gitea title = %q, want fallback", got)
	}

	// Non-forge source types always fall back.
	got = summarizeSelfManagedTitle("stripe", "pull_request", []byte(pr))
	if got != "self-managed stripe delivery" {
		t.Fatalf("stripe title = %q, want fallback", got)
	}
}

// A self-managed gitea delivery that presents only the GitHub-compatible signature header
// (X-Hub-Signature-256: sha256=<hex>) verifies too — the operator-configured /webhooks/gitea
// receiver has always keyed on that header, so both Gitea paths MUST accept it.
func TestSelfManagedGiteaHubSignatureAccepted(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_giteahub"
	seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "route-token-gitea-hub", secret)

	body := `{"action":"opened","pull_request":{"number":9,"title":"Hub sig"},"repository":{"full_name":"o/r"}}`
	rec := postSelfManaged(ing, "route-token-gitea-hub", body, map[string]string{
		"X-Gitea-Delivery":    "guid-hub",
		"X-Gitea-Event":       "pull_request",
		"X-Hub-Signature-256": "sha256=" + giteaSig(secret, body),
	})
	if _, queue := accepted202(t, rec); queue != "reviews" {
		t.Fatalf("queue = %q, want reviews", queue)
	}
	var title string
	if err := pool.QueryRow(ctx,
		`SELECT title FROM todos WHERE queue='reviews'`).Scan(&title); err != nil {
		t.Fatalf("query todo: %v", err)
	}
	if want := "PR #9 opened in o/r — Hub sig"; title != want {
		t.Fatalf("todo title = %q, want %q", title, want)
	}
}

// A generic self-managed webhook carrying a forge event header must get a real title.
//
// This is the shape `switchboard endpoint vend` produces — it mints exactly one generic webhook —
// so it is the ordinary way a forge ends up wired to a vended endpoint, not an edge case. The
// title is what the doorbell says, so before this every Gitea delivery woke the agent with
// "self-managed generic delivery": it knew something arrived, but not that an issue had been
// assigned to it, and had to claim and unpack the payload to find out.
func TestSummarizeSelfManagedTitleForgeOnGenericWebhook(t *testing.T) {
	issue := []byte(`{"action":"assigned","repository":{"full_name":"stump.wtf/switchboard"},` +
		`"issue":{"number":154,"title":"E2E: confirm the handoff lane"}}`)
	pr := []byte(`{"action":"review_requested","repository":{"full_name":"stump.wtf/switchboard"},` +
		`"pull_request":{"number":153,"title":"group verbs under their resource"}}`)

	for _, tc := range []struct {
		name, sourceType, event string
		body                    []byte
		want                    string
	}{
		{"generic webhook, gitea issue", "generic", "issues", issue,
			"Issue #154 assigned in stump.wtf/switchboard — E2E: confirm the handoff lane"},
		{"generic webhook, gitea pull request", "generic", "pull_request", pr,
			"PR #153 review_requested in stump.wtf/switchboard — group verbs under their resource"},
		// A declared forge source keeps behaving exactly as before.
		{"gitea webhook, issue", "gitea", "issues", issue,
			"Issue #154 assigned in stump.wtf/switchboard — E2E: confirm the handoff lane"},
		// An event the summarizer has no special case for still names the repo, and the fallback
		// label keeps a generic webhook distinguishable on the board.
		{"generic webhook, unknown event", "generic", "release", issue,
			"self-managed generic delivery release in stump.wtf/switchboard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeSelfManagedTitle(tc.sourceType, tc.event, tc.body); got != tc.want {
				t.Errorf("summarizeSelfManagedTitle(%q, %q, …)\n got %q\nwant %q",
					tc.sourceType, tc.event, got, tc.want)
			}
		})
	}
}

// With no event header there is nothing to infer from, so the generic one-liner stands. A body
// that is not JSON must not panic or produce a half-built title either.
func TestSummarizeSelfManagedTitleFallbacks(t *testing.T) {
	if got := summarizeSelfManagedTitle("generic", "", []byte(`{}`)); got != "self-managed generic delivery" {
		t.Errorf("no event: got %q", got)
	}
	if got := summarizeSelfManagedTitle("generic", "issues", []byte("not json at all")); got == "" {
		t.Error("unparseable body produced an empty title")
	}
}

// selfManagedEvent prefers X-GitHub-Event but must still read Gitea's own header, which is the one
// present when a Gitea webhook is pointed at a generic ingest URL.
func TestSelfManagedEventReadsBothForgeHeaders(t *testing.T) {
	for _, tc := range []struct{ name, header, want string }{
		{"github header", "X-GitHub-Event", "issues"},
		{"gitea header", "X-Gitea-Event", "issues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/webhooks/w/abc", nil)
			r.Header.Set(tc.header, "issues")
			if got := selfManagedEvent(r); got != tc.want {
				t.Errorf("selfManagedEvent = %q, want %q", got, tc.want)
			}
		})
	}
}

// A token-mode generic webhook honours the sender's own delivery id the way a forge webhook honours
// the forge's: a retry with a re-serialized body collapses onto the first todo. The id is scoped to
// the webhook it arrived on, so the same id on another webhook is another delivery. Governing:
// SPEC-0001 REQ "Idempotency Key Extraction and Dedup" (scenario "Generic redelivery with the same
// delivery id dedups").
func TestSelfManagedGenericHonoursDeliveryID(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	seedWebhook(t, st, ctx, "generic", "token", "reviews", "route-generic-delivery-id", "")
	seedWebhook(t, st, ctx, "generic", "token", "reviews", "route-generic-delivery-id-2", "")

	stamped := map[string]string{"X-Delivery-Id": "job-7"}
	id1, _ := accepted202(t, postSelfManaged(ing, "route-generic-delivery-id", `{"n":1,"sent":"a"}`, stamped))
	id2, _ := accepted202(t, postSelfManaged(ing, "route-generic-delivery-id", `{"n":1,"sent":"b"}`, stamped))
	if id2 != id1 {
		t.Fatalf("redelivery with the same X-Delivery-Id must dedup despite a different body: %s vs %s", id1, id2)
	}
	id3, _ := accepted202(t, postSelfManaged(ing, "route-generic-delivery-id-2", `{"n":1,"sent":"a"}`, stamped))
	if id3 == id1 {
		t.Fatal("a delivery id is scoped to the webhook it arrived on")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("todos = %d, want 2", n)
	}
}

// The Standard Webhooks spelling (Webhook-Id) is honoured like X-Delivery-Id, a different id is a
// different delivery even under a body already seen (the id outranks the hash), and an id too long
// to be a key is ignored so the body hash applies — a hostile sender cannot grow the dedup index
// with the header, and two distinct bodies under one oversized id stay two todos.
//
// These two scenarios were only ever exercised through the operator-configured generic receiver
// (TestGenericDeliveryIDDedup), which was removed in #181; they move here so the behaviour #285
// added keeps its coverage on the one surface that still has it.
// Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup" (scenarios "Generic redelivery
// with the same delivery id dedups", "Oversized delivery id falls back to the body hash").
func TestSelfManagedGenericDeliveryIDSpellingsAndOversize(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	seedWebhook(t, st, ctx, "generic", "token", "reviews", "route-generic-spellings", "")

	post := func(body string, hdr map[string]string) string {
		id, _ := accepted202(t, postSelfManaged(ing, "route-generic-spellings", body, hdr))
		return id
	}

	same := `{"event":"deploy","at":"10:00:00"}`
	first := post(same, map[string]string{"Webhook-Id": "dep-42"})
	if again := post(`{"event":"deploy","at":"10:00:07"}`, map[string]string{"Webhook-Id": "dep-42"}); again != first {
		t.Fatalf("redelivery with the same Webhook-Id must dedup despite a different body: %s vs %s", first, again)
	}
	if other := post(same, map[string]string{"Webhook-Id": "dep-43"}); other == first {
		t.Fatal("a distinct delivery id must mint a distinct todo even under a body already seen")
	}

	huge := strings.Repeat("x", maxGenericDeliveryID+1)
	a := post(`{"n":1}`, map[string]string{"X-Delivery-Id": huge})
	b := post(`{"n":2}`, map[string]string{"X-Delivery-Id": huge})
	if a == b {
		t.Fatal("an oversized delivery id must fall back to the body hash, not collapse distinct bodies")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 4 {
		t.Fatalf("todos = %d, want 4", n)
	}
}
