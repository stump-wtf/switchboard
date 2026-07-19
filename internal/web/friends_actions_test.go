package web

// DB-less handler + helper coverage for the SPEC-0015 Friends surface that runs in the `go test ./...`
// gate: capability gating (every route — including the approve/revoke confirm pages — 404s while
// friending is off, before any store access), add-friend input validation, the friend-action error →
// generic-status mapping, and the pure render-model helpers (state → section, handle → host,
// counts/grouping). The end-to-end approve/decline/revoke/withdraw/unblock POSTs with CSRF and the
// owned-agent guard live in the DB-backed internal/server suite (skipped without
// SWITCHBOARD_TEST_DATABASE_URL).
//
// Governing: SPEC-0015 REQ "Friends View And Approval Flow"; error-handling standards inherited
// from SPEC-0012.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/store"
)

// disabledFriendsHandler is a test handler with the friending capability OFF (the default).
func disabledFriendsHandler(t *testing.T) *Handler {
	t.Helper()
	h := newTestHandler(t)
	h.cfg = config.Config{BaseURL: "https://sb.example.com", FriendingEnabled: false}
	return h
}

// TestFriendRoutes404WhenCapabilityDisabled proves the capability gate: with friending off, every
// friend handler — the view, the confirm pages, and the mutations — returns 404 BEFORE touching
// auth context or the store — hidden-not-broken. The gate is the first statement in each handler,
// so a nil-store handler exercises it in the CI gate.
func TestFriendRoutes404WhenCapabilityDisabled(t *testing.T) {
	h := disabledFriendsHandler(t)
	cases := map[string]http.HandlerFunc{
		"GET /friends":              h.Friends,
		"GET /friends/new":          h.AddFriendModal,
		"GET /friends/resolve":      h.ResolveFriendHandle,
		"POST /friends":             h.AddFriend,
		"GET /friends/{id}/approve": h.ApproveFriendPage,
		"approve":                   h.ApproveFriend,
		"decline":                   h.DeclineFriend,
		"GET /friends/{id}/revoke":  h.RevokeFriendPage,
		"revoke":                    h.RevokeFriend,
		"withdraw":                  h.WithdrawFriend,
		"unblock":                   h.UnblockFriend,
	}
	for name, fn := range cases {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodPost, "/friends", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with friending disabled: got %d, want 404", name, rec.Code)
		}
	}
}

// TestAddFriendValidation proves the add-friend submit rejects a request missing the local agent, the
// remote handle, or the requested intents — a 400 returned before any store access.
func TestAddFriendValidation(t *testing.T) {
	h := enabledFriendsHandler(t)
	cases := map[string]string{
		"missing all":     "",
		"missing intents": "agent_id=a1&handle=alice@remote.example",
		"missing handle":  "agent_id=a1&intents=create_for",
		"missing agent":   "handle=alice@remote.example&intents=create_for",
	}
	for name, form := range cases {
		req := httptest.NewRequest(http.MethodPost, "/friends", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.AddFriend(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("AddFriend %q: got %d, want 400", name, rec.Code)
		}
	}
}

// TestFailFriendActionMapsStoreErrors pins the generic error contract: each friend-edge sentinel
// maps to a distinguishable status with no internal detail; anything else is a generic 500.
func TestFailFriendActionMapsStoreErrors(t *testing.T) {
	h := newTestHandler(t)
	cases := []struct {
		err      error
		wantCode int
		wantBody string
	}{
		{store.ErrNotFound, http.StatusNotFound, "not found"},
		{store.ErrInvalidTransition, http.StatusConflict, "conflict"},
		{store.ErrScopeExceedsRequest, http.StatusBadRequest, "granted scope exceeds requested"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.failFriendAction(rec, "ApproveFriend", "e1", c.err)
		if rec.Code != c.wantCode {
			t.Errorf("failFriendAction(%v) status = %d, want %d", c.err, rec.Code, c.wantCode)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != c.wantBody {
			t.Errorf("failFriendAction(%v) body = %q, want %q", c.err, got, c.wantBody)
		}
	}
}

func TestFriendGroupForEdge(t *testing.T) {
	// Labels are the SPEC-0015 status vocabulary: incoming · awaiting · established · blocked (the
	// scenario's word for an approved edge is "established"). The blocked meta line, not the badge,
	// distinguishes declined from revoked.
	cases := []struct {
		state, direction, wantGroup, wantLabel string
	}{
		{"pending", "", "incoming", "incoming"},         // inbound intake (store default direction)
		{"pending", "outbound", "incoming", "incoming"}, // inbound intake, explicit default
		{"pending", "outgoing", "outgoing", "awaiting"}, // locally sent → awaiting-them (#174)
		{"approved", "", "active", "established"},
		{"approved", "outgoing", "active", "established"}, // an accepted outgoing request is just established
		{"denied", "", "blocked", "blocked"},
		{"denied", "outgoing", "blocked", "blocked"},
		{"revoked", "", "blocked", "blocked"},
	}
	for _, c := range cases {
		g, l := friendGroupForEdge(c.state, c.direction)
		if g != c.wantGroup || l != c.wantLabel {
			t.Errorf("friendGroupForEdge(%q, %q) = (%q, %q), want (%q, %q)",
				c.state, c.direction, g, l, c.wantGroup, c.wantLabel)
		}
	}
}

// TestFriendMetaForEdge pins the footer meta line: relative request/sent ages, the established
// edge's last A2A activity from its vended endpoint's last-seen (there is no per-edge call counter
// — epic #173), declined-vs-revoked preserved on blocked entries, and zero times degrading to an
// empty meta instead of a bogus age.
func TestFriendMetaForEdge(t *testing.T) {
	now := time.Now()
	seen := now.Add(-2 * time.Minute)
	decided := now.Add(-3 * 24 * time.Hour)
	revoked := now.Add(-5 * 24 * time.Hour)
	cases := []struct {
		name string
		edge store.FriendEdge
		want string
	}{
		{"incoming", store.FriendEdge{State: "pending", CreatedAt: now.Add(-8 * time.Minute)}, "requested 8m ago"},
		{"outgoing", store.FriendEdge{State: "pending", Direction: "outgoing", CreatedAt: now.Add(-1 * time.Hour)}, "sent 1h ago · awaiting them"},
		{"established seen", store.FriendEdge{State: "approved", EndpointLastSeenAt: &seen}, "last A2A call 2m ago"},
		{"established never", store.FriendEdge{State: "approved"}, "no A2A calls yet"},
		{"declined", store.FriendEdge{State: "denied", DecidedAt: &decided}, "declined 3d ago"},
		{"revoked", store.FriendEdge{State: "revoked", RevokedAt: &revoked}, "revoked 5d ago"},
		{"zero-time incoming", store.FriendEdge{State: "pending"}, ""},
		{"zero-time blocked", store.FriendEdge{State: "denied"}, ""},
	}
	for _, c := range cases {
		group, _ := friendGroupForEdge(c.edge.State, c.edge.Direction)
		if got := friendMetaForEdge(c.edge, group); got != c.want {
			t.Errorf("%s: friendMetaForEdge = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFriendArrowForGroup pins the direction glyphs: → outgoing pending, ← incoming pending, ↔
// established (and terminal/blocked).
func TestFriendArrowForGroup(t *testing.T) {
	cases := map[string]string{"outgoing": "→", "incoming": "←", "active": "↔", "blocked": "↔"}
	for group, want := range cases {
		if got := friendArrowForGroup(group); got != want {
			t.Errorf("friendArrowForGroup(%q) = %q, want %q", group, got, want)
		}
	}
}

// TestFriendCardDirectionAwareIdentity proves the local/remote split follows the edge direction: an
// inbound edge's local face is to_persona, while a locally sent (outgoing) edge's local face is
// from_persona and the remote handle (+ parsed host) comes from to_persona (#174). Established
// edges carry the negotiated scope (queues + verbs) — the SPEC-0015 established columns.
func TestFriendCardDirectionAwareIdentity(t *testing.T) {
	in := friendCardFromEdge(store.FriendEdge{
		ID: "e-in", State: "pending", ToPersona: "b-persona", FromPersona: "alice@remote.example"})
	if in.Group != "incoming" || in.Arrow != "←" || in.LocalPersona != "b-persona" ||
		in.RemoteHandle != "alice@remote.example" || in.RemoteHost != "remote.example" {
		t.Errorf("inbound card = %+v", in)
	}
	out := friendCardFromEdge(store.FriendEdge{
		ID: "e-out", State: "pending", Direction: "outgoing",
		FromPersona: "b-agent", ToPersona: "zed@far.example"})
	if out.Group != "outgoing" || out.Arrow != "→" || out.LocalPersona != "b-agent" ||
		out.RemoteHandle != "zed@far.example" || out.RemoteHost != "far.example" {
		t.Errorf("outgoing card = %+v", out)
	}
	est := friendCardFromEdge(store.FriendEdge{
		ID: "e-est", State: "approved", ToPersona: "b-persona", FromPersona: "carol@peer.example",
		RequestedVerbs: []string{"create_for", "claim"}, RequestedQueues: []string{"reviews", "deploys"},
		GrantedVerbs: []string{"create_for"}, GrantedQueues: []string{"reviews"}})
	if !est.Negotiated || est.StatusLabel != "established" ||
		len(est.Intents) != 1 || est.Intents[0] != "create_for" ||
		len(est.Queues) != 1 || est.Queues[0] != "reviews" {
		t.Errorf("established card must carry the NEGOTIATED scope: %+v", est)
	}
}

func TestRemoteHostFromHandle(t *testing.T) {
	cases := map[string]string{
		"alice@board.example":         "board.example",
		"peer://agent@host.example/x": "host.example",
		"https://board.example/a/x":   "board.example",
		"bare-handle":                 "",
	}
	for handle, want := range cases {
		if got := remoteHostFromHandle(handle); got != want {
			t.Errorf("remoteHostFromHandle(%q) = %q, want %q", handle, got, want)
		}
	}
}

// TestFriendCountsAndGroups pins the counts helper (the rail badge reads Incoming) and the
// SPEC-0015 section grouping: canonical order (pending-in-your-queue → awaiting-them →
// established → blocked), empty sections dropped.
func TestFriendCountsAndGroups(t *testing.T) {
	cards := friendCardsFromEdges(sampleEdges())
	c := friendCountsFrom(cards)
	if c.All != 6 || c.Incoming != 1 || c.Active != 2 || c.Blocked != 2 || c.Outgoing != 1 {
		t.Fatalf("counts = %+v, want All6 Incoming1 Active2 Blocked2 Outgoing1", c)
	}
	groups := groupFriendCards(cards)
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want 4", len(groups))
	}
	wantOrder := []struct{ key, title string }{
		{"incoming", "Pending · in your queue"},
		{"outgoing", "Pending · awaiting their operator"},
		{"active", "Established"},
		{"blocked", "Blocked"},
	}
	for i, w := range wantOrder {
		if groups[i].Key != w.key || groups[i].Title != w.title {
			t.Errorf("groups[%d] = (%q, %q), want (%q, %q)", i, groups[i].Key, groups[i].Title, w.key, w.title)
		}
	}
	if got := groupFriendCards(cards[:1]); len(got) != 1 || got[0].Key != "incoming" {
		t.Errorf("sparse groups = %+v, want only the populated pending-in-your-queue section", got)
	}
}
