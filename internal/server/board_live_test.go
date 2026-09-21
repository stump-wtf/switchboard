// End-to-end SSE coverage for the patch-panel board against the REAL router + PostgreSQL (#37,
// companion to #25): a signed webhook's card crosses received → verified → patched through as
// typed SSE frames carrying the OOB removal + insertion pairs, a rejected caller surfaces as a
// transient redacted card with NOTHING persisted behind it, an idempotent redelivery resolves
// without a lane advance, and every frame on the wire belongs to the typed event taxonomy.
// The store hooks and the ingest instrument are wired to the web handler exactly as Run wires
// production, so the frames observed here are the frames a browser receives. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL, matching the store test pattern.
// Governing: SPEC-0015 REQ "Patch Panel Board" (scenarios "A signed webhook crosses the board",
// "Rejected caller"), REQ "Live Fragment Architecture" (typed events, OOB removal + insertion);
// SPEC-0012 REQ "Live Updates via SSE" (transport unchanged); SPEC-0001 rejection doctrine.
package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/ingest"
	"github.com/stump-wtf/switchboard/internal/store"
	"github.com/stump-wtf/switchboard/internal/web"
)

// liveGitHubSecret is the minted signing secret of the fixture's self-managed webhook, and
// liveWebhookToken its ingest token: deliveries go to /webhooks/w/{token} and verify per-provider
// against the held secret (SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and
// Idempotency").
const (
	liveGitHubSecret = "live-board-test-secret"
	liveWebhookToken = "board-live-hook-token"
)

// newLiveBoardRouter builds the production router like newDBRouter, additionally wiring the web
// handler as the store's committed-transition observers and as the ingest instrument — the exact
// hook set Run wires — so webhook deliveries and lifecycle transitions publish their typed SSE
// frames end-to-end. Shares the package-dedicated test database (tests in this package run
// serially; each harness truncates).
// The returned endpoint OWNS the fixture's webhook, and so owns every todo a delivery to it mints
// (ADR-0022: todos.endpoint_id is NOT NULL, and a self-managed webhook carries its owner). This
// harness used to drive the operator-configured /webhooks/github receiver, which had no owner of its
// own and needed one stated out of band; that receiver is gone, and the self-managed path is the
// only ingestion surface.
//
// @joestump 09/21/2026 - Rebuilt on the self-managed webhook path for the shared-receiver teardown
// (#181). The board frames are unchanged: the instrument and transition hooks are the same ones.
//
// Governing: ADR-0012, ADR-0022, SPEC-0001 REQ "Enqueue Accepted Delivery as Endpoint-Owned Todo".
func newLiveBoardRouter(t *testing.T) (chi.Router, *store.Store, context.Context, store.Endpoint) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run board SSE end-to-end tests")
	}
	ctx := context.Background()
	// The same package-dedicated database newDBRouter provisions (tests in this package run
	// serially, and every harness truncates before use).
	const serverTestDB = "switchboard_test_server"
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (admin): %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+serverTestDB); err != nil &&
		!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	u.Path = "/" + serverTestDB
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, todos, events, sessions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	st := store.New(pool)
	// Vend the webhook's owning tenant before the board hooks are attached, so the fixture's own
	// setup writes cannot be mistaken for board traffic on the SSE wire.
	operator, err := st.UpsertHuman(ctx, "board-live-operator", "Board Operator", "board-op@example.com")
	if err != nil {
		t.Fatalf("upsert receiver-owner human: %v", err)
	}
	ownerEP := seedEndpoint(t, st, ctx, operator.ID, "board-live-receiver", "hash-board", "sbk_board0")
	if _, err := st.CreateWebhook(ctx, ownerEP.ID, "github", "reviews", "signed",
		liveWebhookToken, liveGitHubSecret, 3); err != nil {
		t.Fatalf("create self-managed webhook: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com"}
	authr, err := auth.New(ctx, cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	// Production hook wiring (server.go Run): committed store transitions and in-flight ingest
	// observations become the board's typed SSE frames.
	st.SetTodoTransitionHook(webh.PublishTodoTransition)
	st.SetEventHook(webh.PublishEventReceived)
	st.SetEndpointSeenHook(webh.PublishEndpointSeen)
	ing := ingest.New(st, ingest.NewHub(), log, ingest.Config{})
	ing.SetInstrument(webh)
	r := newRouter(routerDeps{
		st:    st,
		authr: authr,
		webh:  webh,
		ing:   ing,
		ping:  pool.Ping,
		log:   log,
	})
	return r, st, ctx, ownerEP
}

// sseFrame is one named frame captured off the live stream.
type sseFrame struct {
	Name string
	Data string
}

// sseStream consumes a real /events HTTP stream, frame by frame, remembering every event name it
// saw so a test can check the whole wire against the typed taxonomy afterwards.
type sseStream struct {
	t    *testing.T
	ch   <-chan sseFrame
	Seen []string
}

// openSSE opens the authenticated /events stream against the live test server and parses frames
// off the wire. It returns once the stream headers are in (the hub subscription exists before
// headers are written, so frames published after this call cannot be missed).
func openSSE(t *testing.T, ts *httptest.Server, token string) (*sseStream, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("build /events request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /events: status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		cancel()
		t.Fatalf("GET /events: Content-Type %q", got)
	}
	ch := make(chan sseFrame, 64)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		var name, data string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if name != "" {
					ch <- sseFrame{Name: name, Data: data}
				}
				name, data = "", ""
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
				// retry: and ": keep-alive" comment lines are transport framing, not frames.
			}
		}
	}()
	stop := func() { cancel() }
	return &sseStream{t: t, ch: ch}, stop
}

// await consumes frames until the named one arrives (recording everything seen on the way) or
// fails the test. Sequential awaits therefore also prove wire ORDER: a frame that already passed
// cannot be awaited afterwards.
func (s *sseStream) await(name string) sseFrame {
	s.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-s.ch:
			if !ok {
				s.t.Fatalf("SSE stream closed while awaiting %q (seen: %v)", name, s.Seen)
			}
			s.Seen = append(s.Seen, f.Name)
			if f.Name == name {
				return f
			}
		case <-deadline:
			s.t.Fatalf("no %q frame within deadline (seen: %v)", name, s.Seen)
		}
	}
}

// assertTaxonomy fails on any frame name outside the typed SSE event contract (live.go taxonomy):
// the wire may carry nothing a rendered page has no sse-swap subscription for.
func (s *sseStream) assertTaxonomy() {
	s.t.Helper()
	typed := map[string]bool{
		"lane_received": true, "lane_rejected": true, "lane_deduped": true,
		"todo_created": true, "todo_claimed": true, "todo_completed": true,
		"todo_failed": true, "todo_resurfaced": true,
		"counts": true, "endpoint_seen": true,
	}
	for _, name := range s.Seen {
		if !typed[name] {
			s.t.Errorf("SSE wire carried %q — not in the typed event taxonomy", name)
		}
	}
}

// ghSign computes the X-Hub-Signature-256 value for a GitHub delivery body.
func ghSign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(liveGitHubSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postGitHub delivers a webhook to the live server with the given delivery GUID and signature.
func postGitHub(t *testing.T, ts *httptest.Server, delivery, sig string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/w/"+liveWebhookToken, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sig)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /webhooks/w/{token}: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

var (
	rxIDRe      = regexp.MustCompile(`id="(sb-rx-[0-9a-f]{16})"`)
	hxHeadersRe = regexp.MustCompile(`\{&#34;X-CSRF-Token&#34;:&#34;([0-9a-f]+)&#34;\}|\{"X-CSRF-Token":"([0-9a-f]+)"\}`)
)

// scrapeHXCSRF pulls the per-session CSRF token out of the layout's hx-headers attribute — the
// same place the board's HTMX actions read it from.
func scrapeHXCSRF(t *testing.T, body string) string {
	t.Helper()
	m := hxHeadersRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no hx-headers CSRF token in page: %.300s", body)
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// TestSignedWebhookCrossesTheBoardOverSSE walks the SPEC-0015 scenario "A signed webhook crosses
// the board" end-to-end against the live store: the delivery's ephemeral card appears in
// *received* (lane_received), the committed todo REMOVES that exact node and inserts the durable
// card into *verified* (todo_created, correlated without any shared in-process state — purely by
// the ids on the wire), an idempotent redelivery resolves transiently without a lane advance
// (lane_deduped), the operator's claim moves the card *verified* → *patched through*
// (todo_claimed), and completion updates it in place (todo_completed). Sequential awaits pin the
// wire order; the taxonomy check pins that nothing untyped rode along.
func TestSignedWebhookCrossesTheBoardOverSSE(t *testing.T) {
	r, st, ctx, ownerEP := newLiveBoardRouter(t)
	// Subscribe as the human who OWNS the receiver's endpoint. Live frames are routed to the
	// owning tenant now, so a session for anyone else legitimately receives nothing — that is the
	// behaviour under test elsewhere, not a fixture detail to work around here. The subject
	// matches newLiveBoardRouter's receiver owner, and mintSession upserts on it.
	_, token := mintSession(t, st, ctx, "board-live-operator", "Board Operator", "board-op@example.com")
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	stream, stop := openSSE(t, ts, token)
	defer stop()

	// A signed push arrives.
	body := []byte(`{"repository":{"full_name":"joestump/switchboard"}}`)
	resp := postGitHub(t, ts, "d-live-1", ghSign(body), body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("signed webhook status = %d, want 202", resp.StatusCode)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil || accepted.ID == "" {
		t.Fatalf("decode webhook response: id=%q err=%v", accepted.ID, err)
	}

	// Frame 1: the in-flight card enters *received* — ephemeral, verifying.
	received := stream.await("lane_received")
	m := rxIDRe.FindStringSubmatch(received.Data)
	if m == nil {
		t.Fatalf("lane_received carries no sb-rx card id: %q", received.Data)
	}
	rxID := m[1]
	for _, want := range []string{
		`hx-swap-oob="afterbegin:#sb-lane-received-cards"`,
		`data-sb-lane-card="received"`,
		`data-sb-ephemeral="60000"`,
		"checking signature",
	} {
		if !strings.Contains(received.Data, want) {
			t.Errorf("lane_received missing %q: %q", want, received.Data)
		}
	}

	// Frame 2: the committed todo advances the SAME node into *verified* — OOB removal of the
	// ephemeral card plus insertion of the durable card, correlated purely by wire ids.
	created := stream.await("todo_created")
	for _, want := range []string{
		`<li id="` + rxID + `" hx-swap-oob="delete"></li>`,
		`hx-swap-oob="afterbegin:#sb-lane-verified-cards"`,
		`id="sb-td-` + accepted.ID + `"`,
		">queued<",
		// The Todos view row rides the same frame as a second OOB swap (SPEC-0015 REQ "Todos View
		// And Drawer": SSE row updates preserved).
		`id="sb-tr-` + accepted.ID + `"`,
	} {
		if !strings.Contains(created.Data, want) {
			t.Errorf("todo_created missing %q: %q", want, created.Data)
		}
	}

	// An idempotent redelivery (same delivery GUID, valid signature) collapses onto the live todo:
	// transient resolution in *received*, no second durable card, no lane advance.
	resp = postGitHub(t, ts, "d-live-1", ghSign(body), body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202 (idempotent accept)", resp.StatusCode)
	}
	deduped := stream.await("lane_deduped")
	for _, want := range []string{
		`<li id="` + rxID + `" hx-swap-oob="delete"></li>`,
		">deduped<",
		"collapsed onto an existing todo",
		`data-sb-ephemeral="8000"`,
	} {
		if !strings.Contains(deduped.Data, want) {
			t.Errorf("lane_deduped missing %q: %q", want, deduped.Data)
		}
	}

	// The operator claims from the board card (hx-swap="none"): the direct response already
	// carries the OOB movement so the card crosses even if the SSE frame drops.
	page := getAs(t, r, token, "/")
	csrf := scrapeHXCSRF(t, page.Body.String())
	claim := postAs(t, r, token, csrf, "", "/todos/"+accepted.ID+"/claim")
	if claim.Code != http.StatusOK {
		t.Fatalf("POST claim: %d body=%s", claim.Code, claim.Body.String())
	}
	for _, want := range []string{
		`<li id="sb-td-` + accepted.ID + `" hx-swap-oob="delete"></li>`,
		`hx-swap-oob="afterbegin:#sb-lane-patched-cards"`,
	} {
		if !strings.Contains(claim.Body.String(), want) {
			t.Errorf("claim direct response missing %q: %q", want, claim.Body.String())
		}
	}

	// Frame: the claim moves the durable card *verified* → *patched through* on every OTHER open
	// view, announced with a toast.
	claimed := stream.await("todo_claimed")
	for _, want := range []string{
		`<li id="sb-td-` + accepted.ID + `" hx-swap-oob="delete"></li>`,
		`hx-swap-oob="afterbegin:#sb-lane-patched-cards"`,
		"claimed · operator",
		accepted.ID[:7] + " · claimed · operator", // background-transition toast
	} {
		if !strings.Contains(claimed.Data, want) {
			t.Errorf("todo_claimed missing %q: %q", want, claimed.Data)
		}
	}

	// Completion updates the card in place within *patched through*.
	complete := postAs(t, r, token, csrf, "", "/todos/"+accepted.ID+"/complete")
	if complete.Code != http.StatusOK {
		t.Fatalf("POST complete: %d body=%s", complete.Code, complete.Body.String())
	}
	completed := stream.await("todo_completed")
	for _, want := range []string{
		`<li id="sb-td-` + accepted.ID + `" hx-swap-oob="delete"></li>`,
		`hx-swap-oob="afterbegin:#sb-lane-patched-cards"`,
		">done<",
	} {
		if !strings.Contains(completed.Data, want) {
			t.Errorf("todo_completed missing %q: %q", want, completed.Data)
		}
	}

	// The counts bundle follows the transition in the same ordered worker, carrying the
	// lane-header count swap targets ("lane headers SHALL carry live counts").
	counts := stream.await("counts")
	for _, want := range []string{
		`id="sb-lane-count-verified" class="sb-lane__n" hx-swap-oob="true"`,
		`id="sb-lane-count-patched" class="sb-lane__n" hx-swap-oob="true"`,
	} {
		if !strings.Contains(counts.Data, want) {
			t.Errorf("counts frame missing %q: %q", want, counts.Data)
		}
	}

	// Database truth behind the frames: the todo really is done. The read goes THROUGH the
	// receiver's owning endpoint, which makes the lookup itself the ownership assertion — GetTodo
	// predicates on endpoint_id, so a todo that landed in any other tenant (or in no tenant at all)
	// is ErrNotFound here, not a wrong-looking field. That is the whole point of the scope: a todo
	// is reachable from exactly one endpoint, never from whoever happens to share its queue name.
	// Governing: ADR-0022, SPEC-0001 REQ "Enqueue Accepted Delivery as Endpoint-Owned Todo".
	got, err := st.GetTodo(ctx, ownerEP.ID, accepted.ID)
	if err != nil || got.State != "done" {
		t.Fatalf("todo after flow = %+v err=%v, want state done owned by endpoint %s", got, err, ownerEP.ID)
	}
	stream.assertTaxonomy()
}

// TestRejectedCallerTransientCardOverSSE walks the SPEC-0015 scenario "Rejected caller" end-to-end:
// a delivery failing signature verification surfaces on the received lane as a REDACTED, transient
// rejection card (client-expired via its data-sb-ephemeral stamp — sb-live.js's contract, guarded
// template-side by lanes_render_test.go), while NOTHING is persisted (SPEC-0001 unchanged) and no
// durable frame follows.
func TestRejectedCallerTransientCardOverSSE(t *testing.T) {
	r, st, ctx, _ := newLiveBoardRouter(t)
	rejOp, token := mintSession(t, st, ctx, "board-rej-op", "Op", "op@example.com")
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	stream, stop := openSSE(t, ts, token)
	defer stop()

	// A forged delivery: distinctive payload marker + bogus signature.
	body := []byte(`{"secret_probe":"payload-marker-never-on-the-wire"}`)
	resp := postGitHub(t, ts, "d-forged-1", "sha256=deadbeefdeadbeef", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged webhook status = %d, want 401", resp.StatusCode)
	}

	// The in-flight card appears…
	received := stream.await("lane_received")
	m := rxIDRe.FindStringSubmatch(received.Data)
	if m == nil {
		t.Fatalf("lane_received carries no sb-rx card id: %q", received.Data)
	}
	rxID := m[1]

	// …and resolves into the transient, redacted rejection surface: removal of the same node plus
	// a short-TTL card in the SAME lane (no advance — nothing was persisted to advance to).
	rejected := stream.await("lane_rejected")
	for _, want := range []string{
		`<li id="` + rxID + `" hx-swap-oob="delete"></li>`,
		`hx-swap-oob="afterbegin:#sb-lane-received-cards"`,
		">rejected<",
		"signature verification failed",
		`data-sb-ephemeral="8000"`, // transient by design — sb-live.js expires it client-side
	} {
		if !strings.Contains(rejected.Data, want) {
			t.Errorf("lane_rejected missing %q: %q", want, rejected.Data)
		}
	}
	// Redaction: neither the payload body nor the presented signature may reach any frame.
	for _, frame := range []sseFrame{received, rejected} {
		for _, banned := range []string{"payload-marker-never-on-the-wire", "secret_probe", "deadbeef"} {
			if strings.Contains(frame.Data, banned) {
				t.Errorf("%s frame leaked %q: %q", frame.Name, banned, frame.Data)
			}
		}
	}

	// Nothing persisted behind the transient surface (SPEC-0001 rejection doctrine): no todo, no
	// durable frame.
	counts, err := st.TodoCounts(ctx, rejOp.ID)
	if err != nil {
		t.Fatalf("TodoCounts: %v", err)
	}
	if counts.All != 0 {
		t.Fatalf("rejected delivery persisted %d todos, want 0", counts.All)
	}
	for _, name := range stream.Seen {
		if name == "todo_created" {
			t.Error("a rejected delivery must never produce a todo_created frame")
		}
	}
	stream.assertTaxonomy()
}
