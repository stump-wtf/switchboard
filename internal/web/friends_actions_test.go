package web

// DB-less handler + helper coverage for the SPEC-0013 Friends surface that runs in the `go test ./...`
// gate: capability gating (every route 404s while friending is off, before any store access),
// add-friend input validation, the friend-action error → generic-status mapping, and the pure
// render-model helpers (state → group, handle → host, counts/filter/group). The end-to-end
// approve/decline/revoke/withdraw/unblock POSTs with CSRF and the owned-agent guard live in the
// DB-backed internal/server suite (skipped without SWITCHBOARD_TEST_DATABASE_URL).
//
// Governing: SPEC-0013 REQ "Friends View", REQ "Error Handling Standards" (#110 companion story).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
// friend handler returns 404 BEFORE touching auth context or the store — hidden-not-broken. The gate
// is the first statement in each handler, so a nil-store handler exercises it in the CI gate.
func TestFriendRoutes404WhenCapabilityDisabled(t *testing.T) {
	h := disabledFriendsHandler(t)
	cases := map[string]http.HandlerFunc{
		"GET /friends":         h.Friends,
		"GET /friends/new":     h.AddFriendModal,
		"GET /friends/resolve": h.ResolveFriendHandle,
		"POST /friends":        h.AddFriend,
		"approve":              h.ApproveFriend,
		"decline":              h.DeclineFriend,
		"revoke":               h.RevokeFriend,
		"withdraw":             h.WithdrawFriend,
		"unblock":              h.UnblockFriend,
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

// TestFailFriendActionMapsStoreErrors pins the generic error contract (SPEC-0013): each friend-edge
// sentinel maps to a distinguishable status with no internal detail; anything else is a generic 500.
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
	cases := []struct {
		state, direction, wantGroup string
	}{
		{"pending", "", "incoming"},         // inbound intake (store default direction)
		{"pending", "outbound", "incoming"}, // inbound intake, explicit default
		{"pending", "outgoing", "outgoing"}, // locally sent → Outgoing group (#174)
		{"approved", "", "active"},
		{"approved", "outgoing", "active"}, // an accepted outgoing request is just active
		{"denied", "", "blocked"},
		{"denied", "outgoing", "blocked"},
		{"revoked", "", "blocked"},
	}
	for _, c := range cases {
		if g, _ := friendGroupForEdge(c.state, c.direction); g != c.wantGroup {
			t.Errorf("friendGroupForEdge(%q, %q) group = %q, want %q", c.state, c.direction, g, c.wantGroup)
		}
	}
}

// TestFriendArrowForGroup pins the direction glyphs (#174 / DESIGN Friends): → outgoing pending,
// ← incoming pending, ↔ established (and terminal/blocked).
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
// from_persona and the remote handle (+ parsed host) comes from to_persona (#174).
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

func TestFriendCountsAndFilter(t *testing.T) {
	cards := friendCardsFromEdges(sampleEdges())
	c := friendCountsFrom(cards)
	if c.All != 6 || c.Incoming != 1 || c.Active != 2 || c.Blocked != 2 || c.Outgoing != 1 {
		t.Fatalf("counts = %+v, want All6 Incoming1 Active2 Blocked2 Outgoing1", c)
	}
	if got := filterFriendCards(cards, "blocked"); len(got) != 2 {
		t.Errorf("blocked filter returned %d cards, want 2", len(got))
	}
	// The ledger under "all" always surfaces the four canonical groups (even the empty Outgoing).
	groups := groupFriendCards(cards, "all")
	if len(groups) != 4 {
		t.Fatalf("all-filter groups = %d, want 4", len(groups))
	}
	// A specific filter narrows to that one section.
	if g := groupFriendCards(cards, "active"); len(g) != 1 || g[0].Key != "active" {
		t.Errorf("active-filter groups = %+v, want a single active section", g)
	}
}

func TestNormalizeFriendLayoutAndFilter(t *testing.T) {
	if normalizeFriendLayout("LEDGER") != "ledger" || normalizeFriendLayout("junk") != "cards" {
		t.Error("layout normalization wrong")
	}
	if normalizeFriendFilter("Blocked") != "blocked" || normalizeFriendFilter("") != "all" {
		t.Error("filter normalization wrong")
	}
}
