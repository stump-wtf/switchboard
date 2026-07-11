// End-to-end Friends view + approval-flow tests through the real router + PostgreSQL: the SPEC-0013
// approve/decline/revoke POSTs require session + CSRF, approve mints a scoped endpoint (the vend) and
// moves the edge active, and — the load-bearing hardening — approve only ever vends onto a
// TARGET-OWNED agent (a caller-supplied foreign agent id is refused with no mint). Skipped without
// SWITCHBOARD_TEST_DATABASE_URL (Gitea CI runs DB-less), matching the ownership_test pattern.
// Governing: SPEC-0013 REQ "Friends View", SPEC-0010 REQ "Approval Is the Vend, Narrow-Only",
// wave-4 verification finding / hardening #152 (approve must resolve an owned agent).
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

	"github.com/joestump/switchboard/internal/agentapi"
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
	hub := agentapi.NewHub()
	r := newRouter(routerDeps{
		st: st, authr: authr, webh: webh,
		api: agentapi.New(st, hub, log), ing: ingest.New(st, hub, log, ingest.Config{}),
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
// is refused (400) and mints nothing, while approving with an OWNED agent vends a scoped endpoint and
// moves the edge to active. Governing: SPEC-0010 REQ "Approval Is the Vend", wave-4 finding / #152.
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

	// Owned agent → success: edge active + endpoint minted.
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
