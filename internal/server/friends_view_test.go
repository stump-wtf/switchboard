// End-to-end Friends view + approval-flow tests through the real router + PostgreSQL: the
// approve/decline/revoke POSTs require session + CSRF, the approve confirm page presents the vend
// ("approving IS the vend"), approving mints a scoped endpoint, moves the edge to established, and
// surfaces the vended result explicitly in the response (SPEC-0015 scenario "Approve mints and
// shows the grant"), and — the load-bearing hardening — approve only ever vends onto a
// TARGET-OWNED agent (a caller-supplied foreign agent id is refused with no mint). Skipped without
// SWITCHBOARD_TEST_DATABASE_URL (Gitea CI runs DB-less), matching the ownership_test pattern.
// Governing: SPEC-0015 REQ "Friends View And Approval Flow", SPEC-0010 REQ "Approval Is the Vend,
// Narrow-Only", wave-4 verification finding / hardening #152 (approve must resolve an owned agent).
package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/ingest"
	"github.com/joestump/switchboard/internal/store"
	"github.com/joestump/switchboard/internal/web"
)

// newFriendsRouter builds the production router with the friending capability ENABLED against a
// dedicated test database, so the Friends routes serve rather than 404.
func newFriendsRouter(t *testing.T) (chi.Router, *store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run friends-view tests")
	}
	ctx := context.Background()
	const friendsTestDB = "switchboard_test_friends"
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (admin): %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+friendsTestDB); err != nil &&
		!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	u.Path = "/" + friendsTestDB
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, todos, events, sessions, friend_edges RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	st := store.New(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com", FriendingEnabled: true}
	authr, err := auth.New(ctx, cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	hub := ingest.NewHub()
	r := newRouter(routerDeps{
		st: st, authr: authr, webh: webh,
		ing:     ingest.New(st, hub, log, ingest.Config{}),
		friends: newFriendIntake(st, authr, log), ping: pool.Ping, log: log,
	})
	return r, st, ctx
}

// postFormAs (session-authenticated, CSRF-bearing HTMX-style form POST) is shared with the other
// operator-view server tests — defined in personas_view_test.go.

// seedIncomingEdge records a pending inbound friend edge targeting the given human (as the A2A intake
// would), returning its id.
func seedIncomingEdge(t *testing.T, st *store.Store, ctx context.Context, toHuman, fromPersona string) string {
	t.Helper()
	e, err := st.CreateFriendRequest(ctx, store.CreateFriendRequestParams{
		FromPersona: fromPersona, ToPersona: "b-persona", ToHuman: toHuman,
		RequestedQueues: []string{"reviews"}, RequestedVerbs: []string{"create_for"},
		Reason: "hand you PR reviews", ProvenanceVerified: true,
	})
	if err != nil {
		t.Fatalf("seed friend edge: %v", err)
	}
	return e.ID
}

// TestFriendsViewGatedAndListsOwnEdges: the Friends view serves (200) when the capability is on, lists
// the session human's own incoming request, and never a different human's edge.
func TestFriendsViewGatedAndListsOwnEdges(t *testing.T) {
	r, st, ctx := newFriendsRouter(t)
	alice, tokenA := mintSession(t, st, ctx, "test|alice-fr", "Alice", "alice@example.com")
	bob, _ := mintSession(t, st, ctx, "test|bob-fr", "Bob", "bob@example.com")
	seedIncomingEdge(t, st, ctx, alice.ID, "peer://a-for-alice")
	seedIncomingEdge(t, st, ctx, bob.ID, "peer://a-for-bob")

	rec := getAs(t, r, tokenA, "/friends")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /friends: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "peer://a-for-alice") {
		t.Error("Alice must see her own incoming request")
	}
	if strings.Contains(body, "peer://a-for-bob") {
		t.Error("Alice must NOT see Bob's edge (ownership isolation)")
	}
}

// TestFriendApproveOnlyAcceptsOwnedAgent is the load-bearing guard: approving with a FOREIGN agent id
// is refused (400) and mints nothing, while approving with an OWNED agent vends a scoped endpoint,
// moves the edge to established, and surfaces the vended result in the response. Governing:
// SPEC-0010 REQ "Approval Is the Vend", SPEC-0015 scenario "Approve mints and shows the grant",
// wave-4 finding / #152.
func TestFriendApproveOnlyAcceptsOwnedAgent(t *testing.T) {
	r, st, ctx := newFriendsRouter(t)
	alice, tokenA := mintSession(t, st, ctx, "test|alice-appr", "Alice", "alice@example.com")
	bob, _ := mintSession(t, st, ctx, "test|bob-appr", "Bob", "bob@example.com")
	aliceAgent, err := st.CreateAgent(ctx, alice.ID, "alice-agent", "")
	if err != nil {
		t.Fatalf("create alice agent: %v", err)
	}
	bobAgent, err := st.CreateAgent(ctx, bob.ID, "bob-agent", "")
	if err != nil {
		t.Fatalf("create bob agent: %v", err)
	}
	edgeID := seedIncomingEdge(t, st, ctx, alice.ID, "peer://requester")

	// The approve confirm page presents the vend before anything executes (SPEC-0015: "approving
	// IS the vend"): the requester, the owned-agent vend target, and the narrow-only scope chips.
	page := getAs(t, r, tokenA, "/friends/"+edgeID+"/approve")
	if page.Code != http.StatusOK {
		t.Fatalf("GET approve page: got %d, want 200", page.Code)
	}
	for _, want := range []string{
		"data-sb-friend-approve-confirm", "peer://requester",
		`name="agent_id"`, "alice-agent",
		`name="granted_queues" value="reviews" checked`,
		`name="granted_verbs" value="create_for" checked`,
	} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("approve page: missing %q", want)
		}
	}

	csrf := scrapeCSRF(t, getAs(t, r, tokenA, "/friends").Body.String())

	// Foreign agent (Bob's) → 400, no mint, edge still pending.
	rec := postFormAs(t, r, tokenA, csrf, "/friends/"+edgeID+"/approve",
		url.Values{"agent_id": {bobAgent.ID}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve with a foreign agent: got %d, want 400", rec.Code)
	}
	if edges, _ := st.ListFriendEdges(ctx, alice.ID, "pending"); len(edges) != 1 {
		t.Fatalf("edge must remain pending after a refused approval, got %d pending", len(edges))
	}
	if eps, _ := st.ListEndpoints(ctx, aliceAgent.ID); len(eps) != 0 {
		t.Fatalf("refused approval must mint nothing, got %d endpoints on the owned agent", len(eps))
	}

	// Owned agent → success: edge established + endpoint minted.
	rec = postFormAs(t, r, tokenA, csrf, "/friends/"+edgeID+"/approve",
		url.Values{"agent_id": {aliceAgent.ID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve with an owned agent: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	active, err := st.ListFriendEdges(ctx, alice.ID, "approved")
	if err != nil || len(active) != 1 || active[0].ID != edgeID {
		t.Fatalf("edge must be approved after owned-agent approval: %+v, %v", active, err)
	}
	if active[0].FromAgentID != aliceAgent.ID || active[0].EndpointID == "" {
		t.Fatalf("approval must bind the owned agent and mint an endpoint: %+v", active[0])
	}
	// The response surfaces the vended result explicitly — endpoint identity + scope — and the edge
	// renders as established on the same page (never only a toast).
	eps, err := st.ListEndpoints(ctx, aliceAgent.ID)
	if err != nil || len(eps) != 1 {
		t.Fatalf("approval must mint exactly one endpoint: %+v, %v", eps, err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"data-sb-friend-vended",
		"/mcp/" + eps[0].Slug,
		eps[0].CredentialPrefix,
		"peer://requester",
		"data-sb-friend-group=\"active\"", ">established<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("approve response must surface the vended result: missing %q", want)
		}
	}
}

// TestAddFriendPersistsOutgoingAndWithdraw walks the #174 outgoing lifecycle end-to-end: POST
// /friends persists a pending direction=outgoing edge immediately (from_persona = the local agent,
// to_persona = the remote handle, owned by the sender), the Friends view surfaces it in the Outgoing
// group with a Withdraw action and the → direction arrow, a duplicate live ask is refused (409), and
// POST /friends/{id}/withdraw removes the edge. Governing: SPEC-0013 REQ "Friends View" (Withdraw
// for outgoing), SPEC-0010 REQ "Friend-Request Lifecycle" (pending edge grants nothing).
func TestAddFriendPersistsOutgoingAndWithdraw(t *testing.T) {
	r, st, ctx := newFriendsRouter(t)
	alice, tokenA := mintSession(t, st, ctx, "test|alice-out", "Alice", "alice@example.com")
	agent, err := st.CreateAgent(ctx, alice.ID, "alice-agent", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	csrf := scrapeCSRF(t, getAs(t, r, tokenA, "/friends").Body.String())

	form := url.Values{
		"agent_id": {agent.ID},
		"handle":   {"zed@far.example"},
		"intents":  {"create_for"},
		"reason":   {"hand you deploy checks"},
	}
	if rec := postFormAs(t, r, tokenA, csrf, "/friends", form); rec.Code != http.StatusOK {
		t.Fatalf("POST /friends: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The edge is durably recorded: pending, direction=outgoing, local agent → remote handle.
	edges, err := st.ListFriendEdges(ctx, alice.ID, "pending")
	if err != nil || len(edges) != 1 {
		t.Fatalf("sender must own one pending edge after send: %+v, %v", edges, err)
	}
	e := edges[0]
	if e.Direction != "outgoing" || e.FromPersona != "alice-agent" || e.ToPersona != "zed@far.example" ||
		e.EndpointID != "" || e.Reason != "hand you deploy checks" {
		t.Fatalf("outgoing edge misrecorded: %+v", e)
	}

	// The view renders it in the Outgoing group with Withdraw + the → arrow.
	body := getAs(t, r, tokenA, "/friends").Body.String()
	for _, want := range []string{
		"zed@far.example",
		"/friends/" + e.ID + "/withdraw",
		"sb-fcard__arrow--outgoing",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends view after send: missing %q", want)
		}
	}

	// A duplicate live ask for the same pair is refused with no second edge (anti-flood index).
	if rec := postFormAs(t, r, tokenA, csrf, "/friends", form); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate POST /friends: got %d, want 409", rec.Code)
	}

	// Withdraw removes the pending edge outright.
	if rec := postFormAs(t, r, tokenA, csrf, "/friends/"+e.ID+"/withdraw", nil); rec.Code != http.StatusOK {
		t.Fatalf("withdraw: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if edges, _ := st.ListFriendEdges(ctx, alice.ID); len(edges) != 0 {
		t.Fatalf("withdrawn edge must be gone, got %+v", edges)
	}
}

// TestFriendActionsRequireCSRF proves each friend mutation POST rejects a missing/forged CSRF token
// (RequireCSRF runs before the handler). Governing: SPEC-0013 Security Requirements → CSRF Protection.
func TestFriendActionsRequireCSRF(t *testing.T) {
	r, st, ctx := newFriendsRouter(t)
	alice, tokenA := mintSession(t, st, ctx, "test|alice-csrf", "Alice", "alice@example.com")
	edgeID := seedIncomingEdge(t, st, ctx, alice.ID, "peer://csrf")
	for _, path := range []string{
		"/friends/" + edgeID + "/approve",
		"/friends/" + edgeID + "/decline",
		"/friends/" + edgeID + "/revoke",
		"/friends/" + edgeID + "/withdraw",
		"/friends/" + edgeID + "/unblock",
		"/friends",
	} {
		rec := postFormAs(t, r, tokenA, "not-the-session-token", path, url.Values{"agent_id": {"x"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a forged CSRF token: got %d, want 403", path, rec.Code)
		}
	}
}

// TestFriendDeclineAndRevoke walks decline (pending → declined/blocked) and revoke (approved → killed).
func TestFriendDeclineAndRevoke(t *testing.T) {
	r, st, ctx := newFriendsRouter(t)
	alice, tokenA := mintSession(t, st, ctx, "test|alice-dr", "Alice", "alice@example.com")
	agent, _ := st.CreateAgent(ctx, alice.ID, "alice-agent", "")
	csrf := scrapeCSRF(t, getAs(t, r, tokenA, "/friends").Body.String())

	// Decline a pending edge → it leaves the pending set.
	declineID := seedIncomingEdge(t, st, ctx, alice.ID, "peer://decline-me")
	if rec := postFormAs(t, r, tokenA, csrf, "/friends/"+declineID+"/decline", nil); rec.Code != http.StatusOK {
		t.Fatalf("decline: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if edges, _ := st.ListFriendEdges(ctx, alice.ID, "pending"); len(edges) != 0 {
		t.Fatalf("declined edge must leave the pending set, got %d", len(edges))
	}

	// Approve then revoke → the vended endpoint is killed.
	revokeID := seedIncomingEdge(t, st, ctx, alice.ID, "peer://revoke-me")
	if rec := postFormAs(t, r, tokenA, csrf, "/friends/"+revokeID+"/approve",
		url.Values{"agent_id": {agent.ID}}); rec.Code != http.StatusOK {
		t.Fatalf("approve before revoke: got %d", rec.Code)
	}
	// Revocation is irreversible, so it confirms first: the GET page shows what dies (SPEC-0015
	// REQ "Wizard Interaction Pattern").
	page := getAs(t, r, tokenA, "/friends/"+revokeID+"/revoke")
	if page.Code != http.StatusOK {
		t.Fatalf("GET revoke confirm page: got %d, want 200", page.Code)
	}
	if !strings.Contains(page.Body.String(), "data-sb-friend-revoke-confirm") ||
		!strings.Contains(page.Body.String(), "peer://revoke-me") {
		t.Error("revoke confirm page must present the edge being killed")
	}
	if rec := postFormAs(t, r, tokenA, csrf, "/friends/"+revokeID+"/revoke", nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	eps, err := st.ListEndpoints(ctx, agent.ID)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}
	if len(eps) != 1 || eps[0].State != "revoked" {
		t.Fatalf("revoke must kill the vended endpoint, got %+v", eps)
	}
}
