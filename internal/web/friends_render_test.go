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
	"time"

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
// pending (#174) — so a render exercises every group + status. The stamped times back the footer
// meta lines (#175): requested/sent ages from created_at, blocked ages from decided_at/revoked_at,
// and the active edge's last A2A activity from its vended endpoint's last-seen.
func sampleEdges() []store.FriendEdge {
	now := time.Now()
	lastSeen := now.Add(-2 * time.Minute)
	declined := now.Add(-3 * 24 * time.Hour)
	revoked := now.Add(-5 * 24 * time.Hour)
	return []store.FriendEdge{
		{ID: "e-pending", State: "pending", ToPersona: "b-persona", FromPersona: "alice@remote.example",
			RequestedVerbs: []string{"create_for", "list_todos"}, RequestedQueues: []string{"reviews"},
			Reason: "hand you PR reviews", CreatedAt: now.Add(-8 * time.Minute)},
		{ID: "e-outgoing", State: "pending", Direction: "outgoing", FromPersona: "b-agent", ToPersona: "zed@far.example",
			RequestedVerbs: []string{"create_for"}, Reason: "want to hand you deploy checks",
			CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "e-active", State: "approved", ToPersona: "b-persona", FromPersona: "carol@peer.example",
			RequestedVerbs: []string{"create_for"}, GrantedVerbs: []string{"create_for"}, GrantedQueues: []string{"reviews"},
			CreatedAt: now.Add(-6 * 24 * time.Hour), EndpointLastSeenAt: &lastSeen},
		{ID: "e-bare", State: "approved", ToPersona: "b-persona", FromPersona: "dave@peer.example",
			RequestedVerbs: []string{"create_for"}, CreatedAt: now.Add(-12 * 24 * time.Hour)}, // approved, nothing negotiated, never seen
		{ID: "e-denied", State: "denied", ToPersona: "b-persona", FromPersona: "mallory@bad.example",
			RequestedVerbs: []string{"delete_all"}, CreatedAt: now.Add(-4 * 24 * time.Hour), DecidedAt: &declined},
		{ID: "e-revoked", State: "revoked", ToPersona: "b-persona", FromPersona: "erin@peer.example",
			GrantedVerbs: []string{"create_for"}, CreatedAt: now.Add(-9 * 24 * time.Hour), RevokedAt: &revoked},
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
		// Status badges keyed by ledger group, with the design canvas labels (#175 / DESIGN
		// Friends FR_STATUS: incoming · awaiting · active · blocked).
		"sb-badge--fr-incoming\">incoming<", "sb-badge--fr-outgoing\">awaiting<",
		"sb-badge--fr-active\">active<", "sb-badge--fr-blocked\">blocked<",
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
		// Footer meta lines (#175 / DESIGN Friends): relative request/decision times per group and
		// last A2A activity (the vended endpoint's last-seen) for active links.
		"sb-fcard__meta\">requested 8m ago<",
		"sb-fcard__meta\">sent 1h ago · awaiting them<",
		"sb-fcard__meta\">last A2A call 2m ago<",
		"sb-fcard__meta\">no A2A calls yet<",
		"sb-fcard__meta\">declined 3d ago<",
		"sb-fcard__meta\">revoked 5d ago<",
		// The incoming requester's note renders as a quoted callout (#175 / DESIGN Friends).
		"sb-fcard__note\">“hand you PR reviews”</blockquote>",
		// Blocked cards carry the dimming group class (opacity via CSS).
		"sb-fcard--blocked",
		// The view toggle carries the design's 2a/2b variant badges (#175).
		"sb-seg__badge\">2a<", "sb-seg__badge\">2b<",
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
	// The quoted note callout is an INCOMING affordance (the requester's pitch to the approver);
	// your own outgoing message is not echoed back on the card (DESIGN Friends).
	if strings.Contains(body, "want to hand you deploy checks") {
		t.Error("an outgoing card must not render the sender's own note")
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
		"sb-fledger", "sb-fgroup",
		// The design canvas group headings, each with its status dot (#175 / DESIGN Friends 2b).
		">Incoming requests ", ">Outgoing · pending ", ">Active friendships ", ">Blocked ",
		"sb-fgroup__dot--incoming", "sb-fgroup__dot--outgoing", "sb-fgroup__dot--active", "sb-fgroup__dot--blocked",
		// 2b is a compact row-based table (sb-frows/sb-frow), NOT the cards grid regrouped (#175).
		"sb-frows", "sb-frow sb-frow--incoming",
		// Rows carry the intents chips and the meta cell alongside the identity + actions.
		"sb-frow__intents", "sb-frow__meta\">requested 8m ago<", "sb-frow__meta\">last A2A call 2m ago<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends ledger: missing %q", want)
		}
	}
	if strings.Contains(body, "sb-fcards") {
		t.Error("the ledger layout must not render the cards grid")
	}
	// The ledger carries no filter pills — its group headings ARE the grouping (DESIGN Friends).
	if strings.Contains(body, "sb-fpill\"") || strings.Contains(body, "id=\"sb-fc-all\"") {
		t.Error("the ledger layout must not render the filter pills")
	}
	// The Outgoing group populates from locally persisted direction=outgoing edges (#174), with the
	// same direction-aware identity row and Withdraw as the cards layout (fragments are shared).
	for _, want := range []string{
		"id=\"sb-fr-e-outgoing\"",
		"hx-post=\"/friends/e-outgoing/withdraw\"",
		"sb-fcard__arrow--outgoing\" aria-hidden=\"true\">→<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends ledger: missing %q", want)
		}
	}
	// Groups with no edges are dropped, not rendered as empty shells (DESIGN Friends 2b).
	incomingOnly := sampleEdges()[:1]
	sparse := renderPage(t, h, "friends", friendsView(h, "ledger", "all", incomingOnly, nil))
	if strings.Contains(sparse, ">Outgoing · pending ") || strings.Contains(sparse, ">Blocked ") {
		t.Error("an empty ledger group must be dropped entirely")
	}
	if !strings.Contains(sparse, ">Incoming requests ") {
		t.Error("a populated ledger group must still render")
	}
	// No friendships at all → the whole-ledger empty state.
	empty := renderPage(t, h, "friends", friendsView(h, "ledger", "all", nil, nil))
	if !strings.Contains(empty, "no friendships yet") {
		t.Error("an empty ledger must render the empty state")
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
	frag, err := h.renderFragment("add_friend_modal", friendPanelView{
		Agents: []store.Agent{{ID: "a1", Name: "b-agent"}}, CSRF: "tok", IntentOptions: friendIntentOptions,
	})
	if err != nil {
		t.Fatalf("render add_friend_modal: %v", err)
	}
	for _, want := range []string{
		"data-sb-drawer",                 // reuses the overlay focus-trap
		"Request an A2A friendship",      // design canvas modal title (#175)
		"action=\"/friends\"",            // submits the request
		"name=\"agent_id\"", ">b-agent<", // local agent
		"name=\"handle\"", "hx-get=\"/friends/resolve\"", // remote handle w/ live resolution preview
		// Requested intents are toggle chips, not free text (#175 / DESIGN Friends): one checkbox
		// per registry verb, with the primary work-handoff intent pre-checked for the no-JS form.
		"sb-chip--toggle",
		"name=\"intents\" value=\"create_for\" checked",
		"name=\"intents\" value=\"list_todos\"",
		"name=\"reason\"",                            // message to their operator
		"mutual and non-transitive",                  // states the mutual + non-transitive contract
		"pending until the remote operator approves", // pending-until-remote-approves
		"name=\"csrf_token\" value=\"tok\"",
		">Cancel<", "Send request →", // design canvas footer actions
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("add-friend modal: missing %q", want)
		}
	}
	// A free-text intents input must be gone — chips only (#175).
	if strings.Contains(frag, "comma-separated") {
		t.Error("add-friend modal must not fall back to a comma-separated intents input")
	}
}

// TestOverlayClearFragment pins the OOB overlay dismissal a successful add-friend submit rides on
// (#175): an innerHTML swap against the shared #sb-overlay (replacing the element itself would
// detach sb.js's MutationObserver).
func TestOverlayClearFragment(t *testing.T) {
	h := enabledFriendsHandler(t)
	frag, err := h.renderFragment("overlay_clear", nil)
	if err != nil {
		t.Fatalf("render overlay_clear: %v", err)
	}
	if !strings.Contains(frag, "hx-swap-oob=\"innerHTML:#sb-overlay\"") {
		t.Errorf("overlay_clear must OOB-clear #sb-overlay: %q", frag)
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
