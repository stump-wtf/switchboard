package web

// Template render coverage for the SPEC-0013 Friends view that runs in the DB-less `go test ./...`
// gate: the cards vs grouped-ledger layouts, per-status action buttons, intent chips (including the
// none-negotiated fallback), the add-friend modal, and the capability-gated rail entry. Templates
// parse from the embedded FS via New (newTestHandler), so a broken friend template fails these tests.
//
// Governing: SPEC-0013 REQ "Friends View", REQ "Design Language Conformance" (#110 companion story).

import (
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/store"
)

// enabledFriendsHandler is a test handler with the friending capability turned on.
func enabledFriendsHandler(t *testing.T) *Handler {
	t.Helper()
	h := newTestHandler(t)
	h.cfg = config.Config{BaseURL: "https://sb.example.com", FriendingEnabled: true}
	return h
}

// sampleEdges seeds one edge per lifecycle state — including a locally sent direction=outgoing
// pending (#174) — so a render exercises every group + status.
func sampleEdges() []store.FriendEdge {
	return []store.FriendEdge{
		{ID: "e-pending", State: "pending", ToPersona: "b-persona", FromPersona: "alice@remote.example",
			RequestedVerbs: []string{"create_for", "list_todos"}, RequestedQueues: []string{"reviews"}, Reason: "hand you PR reviews"},
		{ID: "e-outgoing", State: "pending", Direction: "outgoing", FromPersona: "b-agent", ToPersona: "zed@far.example",
			RequestedVerbs: []string{"create_for"}, Reason: "want to hand you deploy checks"},
		{ID: "e-active", State: "approved", ToPersona: "b-persona", FromPersona: "carol@peer.example",
			RequestedVerbs: []string{"create_for"}, GrantedVerbs: []string{"create_for"}, GrantedQueues: []string{"reviews"}},
		{ID: "e-bare", State: "approved", ToPersona: "b-persona", FromPersona: "dave@peer.example",
			RequestedVerbs: []string{"create_for"}}, // approved but nothing negotiated → none-negotiated chip
		{ID: "e-denied", State: "denied", ToPersona: "b-persona", FromPersona: "mallory@bad.example",
			RequestedVerbs: []string{"delete_all"}},
		{ID: "e-revoked", State: "revoked", ToPersona: "b-persona", FromPersona: "erin@peer.example",
			GrantedVerbs: []string{"create_for"}},
	}
}

func friendsView(h *Handler, layout, filter string, edges []store.FriendEdge, agents []store.Agent) view {
	panel := h.friendPanelView(friendCardsFromEdges(edges), agents, layout, filter, "tok")
	return view{
		Title: "Friends", Human: testHuman(), CSRF: "tok",
		Shell:        shell{Active: "friends", DBConnected: true, Initials: "JS", FriendsEnabled: true, FriendsIncoming: panel.Counts.Incoming},
		FriendGroups: panel.Groups, FriendCards: panel.Cards, FriendCounts: panel.Counts,
		FriendLayout: layout, FriendFilter: filter, Agents: agents,
	}
}

func TestFriendsCardsLayoutRendersStatusesAndActions(t *testing.T) {
	h := enabledFriendsHandler(t)
	agents := []store.Agent{{ID: "a1", Name: "b-agent"}}
	body := renderPage(t, h, "friends", friendsView(h, "cards", "all", sampleEdges(), agents))

	for _, want := range []string{
		"sb-fcards",                              // cards grid
		"b-persona",                              // local persona
		"alice@remote.example", "remote.example", // remote handle + parsed board host
		"sb-badge--pending", "sb-badge--approved", "sb-badge--denied", "sb-badge--revoked", // status badges w/ text
		"sb-chip", "create_for", "list_todos", // intent chips
		"none negotiated",                          // approved edge with no granted scope
		"hx-post=\"/friends/e-pending/approve\"",   // incoming → approve
		"hx-post=\"/friends/e-pending/decline\"",   // incoming → decline
		"hx-post=\"/friends/e-outgoing/withdraw\"", // outgoing pending → withdraw (#174)
		"hx-post=\"/friends/e-active/revoke\"",     // active → revoke
		"hx-post=\"/friends/e-denied/unblock\"",    // blocked → unblock
		"hx-post=\"/friends/e-revoked/unblock\"",   // blocked → unblock
		"name=\"agent_id\"", ">b-agent<",           // approve vends onto a selectable owned agent
		"id=\"sb-fr-e-pending\"",            // stable OOB row id
		"name=\"csrf_token\" value=\"tok\"", // CSRF field for the no-JS fallback
		// Direction-aware identity row (#174 / DESIGN Friends): arrows per status + distinct remote
		// styling; the outgoing card's local face is the sending agent, remote is the asked handle.
		"sb-fcard__arrow--incoming\" aria-hidden=\"true\">←<",
		"sb-fcard__arrow--outgoing\" aria-hidden=\"true\">→<",
		"sb-fcard__arrow--active\" aria-hidden=\"true\">↔<",
		"sb-fcard__remote-handle",
		"sb-fcard--outgoing",
		"zed@far.example", "far.example", // outgoing remote handle + parsed board host
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends cards: missing %q", want)
		}
	}
	// The outgoing pending card must never offer Approve/Decline (those are the target's actions).
	if strings.Contains(body, "hx-post=\"/friends/e-outgoing/approve\"") ||
		strings.Contains(body, "hx-post=\"/friends/e-outgoing/decline\"") {
		t.Error("an outgoing pending edge must offer only Withdraw")
	}
	// The approve action must NEVER target a decline/revoke on the wrong edge, and the denied edge
	// must not surface a revoke (it is not active).
	if strings.Contains(body, "hx-post=\"/friends/e-denied/revoke\"") {
		t.Error("a declined edge must not offer Revoke")
	}
	if strings.Contains(strings.ToLower(body), "pico") {
		t.Error("friends: references Pico")
	}
}

func TestFriendsLedgerLayoutGroups(t *testing.T) {
	h := enabledFriendsHandler(t)
	body := renderPage(t, h, "friends", friendsView(h, "ledger", "all", sampleEdges(), []store.Agent{{ID: "a1", Name: "b-agent"}}))

	for _, want := range []string{
		"sb-fledger",
		">Incoming ", ">Outgoing ", ">Active ", ">Blocked ", // the four ledger group headings
		"sb-fgroup",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends ledger: missing %q", want)
		}
	}
	// The Outgoing group populates from locally persisted direction=outgoing edges (#174), with the
	// same direction-aware identity row and Withdraw as the cards layout (friend_card is shared).
	for _, want := range []string{
		"id=\"sb-fr-e-outgoing\"",
		"hx-post=\"/friends/e-outgoing/withdraw\"",
		"sb-fcard__arrow--outgoing\" aria-hidden=\"true\">→<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends ledger: missing %q", want)
		}
	}
	// A group with no edges still renders its empty note, not a broken section.
	incomingOnly := sampleEdges()[:1]
	empty := renderPage(t, h, "friends", friendsView(h, "ledger", "all", incomingOnly, nil))
	if !strings.Contains(empty, "no Outgoing friendships") {
		t.Error("empty Outgoing group must render its empty note")
	}
}

func TestFriendsFilterScopesCards(t *testing.T) {
	h := enabledFriendsHandler(t)
	// Active filter: only the two approved edges appear; the pending/denied ones are excluded.
	body := renderPage(t, h, "friends", friendsView(h, "cards", "active", sampleEdges(), nil))
	if !strings.Contains(body, "sb-fr-e-active") {
		t.Error("active filter must show the approved edge")
	}
	if strings.Contains(body, "sb-fr-e-pending") || strings.Contains(body, "sb-fr-e-denied") {
		t.Error("active filter must exclude non-active edges")
	}
	// The pill counts always reflect ALL edges, not just the filtered slice.
	if !strings.Contains(body, "id=\"sb-fc-incoming\"") {
		t.Error("filter pills must render count spans")
	}
}

func TestFriendsRailEntryGatedByCapability(t *testing.T) {
	h := newTestHandler(t)
	// Enabled + incoming pending → rail entry present with the badge count.
	on := renderPage(t, h, "board", view{Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS", FriendsEnabled: true, FriendsIncoming: 2}})
	if !strings.Contains(on, "href=\"/friends\"") {
		t.Error("Friends rail entry must appear when the capability is enabled")
	}
	if !strings.Contains(on, "id=\"sb-friends-badge\"") || !strings.Contains(on, ">2</span>") {
		t.Error("pending-incoming badge must render its count")
	}
	// Disabled → hidden from the rail (SPEC-0013 hidden-not-broken).
	off := renderPage(t, h, "board", view{Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS", FriendsEnabled: false}})
	if strings.Contains(off, "href=\"/friends\"") {
		t.Error("Friends rail entry must be hidden when the capability is disabled")
	}
}

func TestAddFriendModalRenders(t *testing.T) {
	h := enabledFriendsHandler(t)
	frag, err := h.renderFragment("add_friend_modal", friendPanelView{Agents: []store.Agent{{ID: "a1", Name: "b-agent"}}, CSRF: "tok"})
	if err != nil {
		t.Fatalf("render add_friend_modal: %v", err)
	}
	for _, want := range []string{
		"data-sb-drawer",                 // reuses the overlay focus-trap
		"action=\"/friends\"",            // submits the request
		"name=\"agent_id\"", ">b-agent<", // local agent
		"name=\"handle\"", "hx-get=\"/friends/resolve\"", // remote handle w/ live resolution preview
		"name=\"intents\"",                           // requested intents
		"name=\"reason\"",                            // message to the operator
		"mutual and non-transitive",                  // states the mutual + non-transitive contract
		"pending until the remote operator approves", // pending-until-remote-approves
		"name=\"csrf_token\" value=\"tok\"",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("add-friend modal: missing %q", want)
		}
	}
}

func TestFriendResolvePreview(t *testing.T) {
	h := enabledFriendsHandler(t)
	frag, err := h.renderFragment("friend_resolve", friendResolveView{Handle: "alice@remote.example", Host: "remote.example", Valid: true})
	if err != nil {
		t.Fatalf("render friend_resolve: %v", err)
	}
	if !strings.Contains(frag, "alice@remote.example") || !strings.Contains(frag, "remote.example") {
		t.Errorf("resolve preview must echo the parsed handle + host: %q", frag)
	}
}
