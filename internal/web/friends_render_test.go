package web

// Template render coverage for the SPEC-0015 Friends view + approval flow that runs in the DB-less
// `go test ./...` gate: the pending-in-your-queue / awaiting-them / established / blocked sections,
// per-status actions (the irreversible ones routing through their full confirm pages), the approve
// confirm page ("approving IS the vend" — it presents the scoped endpoint approval mints), the
// post-approval vended-result panel, the friend revoke confirm page, the add-friend modal, and the
// capability-gated rail entry. Assertions key on data-sb-* attributes, ids, and rendered copy.
// Templates parse from the embedded FS via New (newTestHandler), so a broken friend template fails
// these tests.
//
// Governing: SPEC-0015 REQ "Friends View And Approval Flow" (scenario "Approve mints and shows the
// grant"), REQ "Wizard Interaction Pattern"; SPEC-0010 semantics unchanged; ADR-0018.

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
// pending (#174) — so a render exercises every section + status. The stamped times back the footer
// meta lines: requested/sent ages from created_at, blocked ages from decided_at/revoked_at, and the
// established edge's last A2A activity from its vended endpoint's last-seen.
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

// friendsView builds the Friends page view model the handler would (groups over all cards, counts
// for the rail badge, optionally the post-approval vended panel).
func friendsView(edges []store.FriendEdge, vended *friendVendedView) view {
	cards := friendCardsFromEdges(edges)
	counts := friendCountsFrom(cards)
	return view{
		Title: "Friends", Human: testHuman(), CSRF: "tok",
		Shell:        shell{Active: "friends", DBConnected: true, Initials: "JS", FriendsEnabled: true, FriendsIncoming: counts.Incoming},
		FriendGroups: groupFriendCards(cards), FriendCounts: counts, FriendVended: vended,
	}
}

// TestFriendsViewSeparatesPendingFromEstablished pins the SPEC-0015 framing: ordered sections —
// pending · in your queue first, then awaiting-them, established, blocked — each edge carrying
// direction, peer, scope, and its meta line, with the status-appropriate actions (irreversible
// ones as LINKS to their confirm pages).
func TestFriendsViewSeparatesPendingFromEstablished(t *testing.T) {
	h := enabledFriendsHandler(t)
	body := renderPage(t, h, "friends", friendsView(sampleEdges(), nil))

	for _, want := range []string{
		// The four sections, keyed by their behavior hook.
		`data-sb-friend-group="incoming"`, `data-sb-friend-group="outgoing"`,
		`data-sb-friend-group="active"`, `data-sb-friend-group="blocked"`,
		// SPEC-0015 section headings: pending · in your queue vs established.
		"Pending · in your queue", "Pending · awaiting their operator", "Established", "Blocked",
		// Status badges (established is the SPEC-0015 scenario's word for an approved edge).
		">incoming<", ">awaiting<", ">established<", ">blocked<",
		// Direction-aware identity: local face, arrow glyph, remote handle + parsed board host.
		"b-persona", "alice@remote.example", "remote.example",
		"zed@far.example", "far.example",
		">←<", ">→<", ">↔<",
		// Scope chips: requested until established, negotiated (queues + verbs) after.
		"reviews", "create_for", "list_todos",
		"none negotiated", // approved edge with no granted scope
		// Stable row ids for OOB swaps.
		`id="sb-fr-e-pending"`, `id="sb-fr-e-active"`,
		// Incoming: Review & approve LINKS to the approval page (approving is the vend); Decline posts.
		`href="/friends/e-pending/approve"`, "data-sb-friend-approve",
		`hx-post="/friends/e-pending/decline"`,
		// Outgoing pending: Withdraw posts (#174).
		`hx-post="/friends/e-outgoing/withdraw"`,
		// Established: Revoke LINKS to the confirm page (irreversible steps confirm — SPEC-0015).
		`href="/friends/e-active/revoke"`, "data-sb-friend-revoke",
		// Blocked: Unblock posts.
		`hx-post="/friends/e-denied/unblock"`, `hx-post="/friends/e-revoked/unblock"`,
		`name="csrf_token" value="tok"`, // CSRF field for the no-JS fallback forms
		// Footer meta lines: relative request/decision times per section and last A2A activity
		// (the vended endpoint's last-seen) for established links.
		"requested 8m ago",
		"sent 1h ago · awaiting them",
		"last A2A call 2m ago",
		"no A2A calls yet",
		"declined 3d ago",
		"revoked 5d ago",
		// The incoming requester's note renders as a quoted callout.
		"“hand you PR reviews”",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friends view: missing %q", want)
		}
	}
	// Pending-in-your-queue leads; established follows (the SPEC-0015 separation is ORDERED).
	if strings.Index(body, `data-sb-friend-group="incoming"`) > strings.Index(body, `data-sb-friend-group="active"`) {
		t.Error("the pending-in-your-queue section must render before established")
	}
	// Approve is a full-page flow now: the card must NOT carry an inline approve POST.
	if strings.Contains(body, `hx-post="/friends/e-pending/approve"`) {
		t.Error("an incoming card must link to the approve confirm page, not post inline")
	}
	// The outgoing pending card must never offer Approve/Decline (those are the target's actions).
	if strings.Contains(body, "/friends/e-outgoing/approve") ||
		strings.Contains(body, "/friends/e-outgoing/decline") {
		t.Error("an outgoing pending edge must offer only Withdraw")
	}
	// Your own outgoing message is not echoed back on the card (the note is an incoming affordance).
	if strings.Contains(body, "want to hand you deploy checks") {
		t.Error("an outgoing card must not render the sender's own note")
	}
	// A declined edge is not established: no revoke.
	if strings.Contains(body, "/friends/e-denied/revoke") {
		t.Error("a declined edge must not offer Revoke")
	}
	if strings.Contains(strings.ToLower(body), "pico") {
		t.Error("friends: references Pico")
	}
}

// TestFriendsSectionsDropEmptyAndEmptyState: sections with no edges are dropped, not rendered as
// empty shells; no friendships at all renders the whole-view empty state.
func TestFriendsSectionsDropEmptyAndEmptyState(t *testing.T) {
	h := enabledFriendsHandler(t)
	sparse := renderPage(t, h, "friends", friendsView(sampleEdges()[:1], nil))
	if strings.Contains(sparse, "Pending · awaiting their operator") || strings.Contains(sparse, `data-sb-friend-group="blocked"`) {
		t.Error("an empty section must be dropped entirely")
	}
	if !strings.Contains(sparse, "Pending · in your queue") {
		t.Error("a populated section must still render")
	}
	empty := renderPage(t, h, "friends", friendsView(nil, nil))
	if !strings.Contains(empty, "no friendships yet") {
		t.Error("an empty view must render the empty state")
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
	// Disabled → hidden from the rail (hidden-not-broken).
	off := renderPage(t, h, "board", view{Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS", FriendsEnabled: false}})
	if strings.Contains(off, "href=\"/friends\"") {
		t.Error("Friends rail entry must be hidden when the capability is disabled")
	}
}

// TestApproveFriendPagePresentsTheVend pins the "approving IS the vend" confirm page (SPEC-0015):
// it shows the requester, the vend target picker over the human's OWN agents, and the scoped
// endpoint approval mints — granted queues/verbs as narrow-only toggle chips pre-checked to the
// requested scope — before anything executes.
func TestApproveFriendPagePresentsTheVend(t *testing.T) {
	h := enabledFriendsHandler(t)
	card := friendCardFromEdge(sampleEdges()[0]) // e-pending
	body := renderPage(t, h, "friend_approve", view{
		Title: "Approve friendship", Human: testHuman(), CSRF: "tok",
		Shell:         shell{Active: "friends", DBConnected: true, Initials: "JS", FriendsEnabled: true},
		FriendApprove: &friendApproveView{Card: card, Agents: []store.Agent{{ID: "a1", Name: "b-agent"}}},
	})
	for _, want := range []string{
		"data-sb-friend-approve-confirm",
		"approving is the vend", // tagline states the doctrine
		// The requester's identity + pitch.
		"alice@remote.example", "remote.example", "“hand you PR reviews”",
		// The vend target: one of the approving human's OWN agents.
		`name="agent_id"`, ">b-agent<",
		// The scoped endpoint approval mints: narrow-only chips pre-checked to the requested scope.
		"data-sb-approve-queues", `name="granted_queues" value="reviews" checked`,
		"data-sb-approve-verbs", `name="granted_verbs" value="create_for" checked`,
		`name="granted_verbs" value="list_todos" checked`,
		"narrow-only",
		// The irreversible act confirms before executing, as a plain no-JS form.
		`action="/friends/e-pending/approve"`, `name="csrf_token" value="tok"`,
		"data-sb-friend-approve-submit",
		">Cancel</a>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("approve page: missing %q", want)
		}
	}
	// Without an owned agent there is nothing to vend onto: no submit, a register hint instead.
	noAgents := renderPage(t, h, "friend_approve", view{
		Title: "Approve friendship", Human: testHuman(), CSRF: "tok",
		Shell:         shell{Active: "friends", DBConnected: true, Initials: "JS", FriendsEnabled: true},
		FriendApprove: &friendApproveView{Card: card},
	})
	if strings.Contains(noAgents, "data-sb-friend-approve-submit") {
		t.Error("approve page must not offer the vend submit without an owned agent")
	}
	if !strings.Contains(noAgents, "register an agent first") {
		t.Error("approve page must explain that approval needs an owned agent")
	}
}

// TestFriendVendedResultPanel pins the SPEC-0015 scenario "Approve mints and shows the grant": a
// successful approval renders the vended endpoint's identity and scope explicitly on the page —
// never only a toast — while the plaintext credential stays with the A2A response.
func TestFriendVendedResultPanel(t *testing.T) {
	h := enabledFriendsHandler(t)
	edges := sampleEdges()[2:3] // the now-established edge
	body := renderPage(t, h, "friends", friendsView(edges, &friendVendedView{
		Peer: "carol@peer.example", PeerHost: "peer.example", AgentName: "b-agent",
		URL: "https://sb.example.com/mcp/b-agent-x1y2", CredPrefix: "sb_live_abc12345",
		Queues: []string{"reviews"}, Verbs: []string{"create_for"},
	}))
	for _, want := range []string{
		"data-sb-friend-vended",
		"Friendship established · endpoint vended",
		"carol@peer.example", "b-agent",
		// The vended endpoint's identity...
		"https://sb.example.com/mcp/b-agent-x1y2",
		"sb_live_abc12345", "…",
		// ...and its granted scope, explicit on the page.
		"granted scope", ">reviews<", ">create_for<",
		// The credential is the REMOTE agent's, delivered over A2A — never shown to this operator.
		"never shown here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vended result panel: missing %q", want)
		}
	}
	// The edge itself now sits in the Established section on the same page.
	if !strings.Contains(body, `data-sb-friend-group="active"`) || !strings.Contains(body, ">established<") {
		t.Error("the approved edge must render as established alongside the vended result")
	}
	// A render WITHOUT a fresh approval must not show a vended panel.
	if plain := renderPage(t, h, "friends", friendsView(edges, nil)); strings.Contains(plain, "data-sb-friend-vended") {
		t.Error("the vended panel must only render as the response to an approval")
	}
}

// TestFriendRevokePageConfirms pins the friend revoke confirm page (SPEC-0015 REQ "Wizard
// Interaction Pattern"): revocation kills the vended endpoint and is one-sided, and the POST only
// fires from this page's explicit confirm.
func TestFriendRevokePageConfirms(t *testing.T) {
	h := enabledFriendsHandler(t)
	card := friendCardFromEdge(sampleEdges()[2]) // e-active, established
	body := renderPage(t, h, "friend_revoke", view{
		Title: "Revoke friendship", Human: testHuman(), CSRF: "tok",
		Shell:        shell{Active: "friends", DBConnected: true, Initials: "JS", FriendsEnabled: true},
		FriendRevoke: &friendRevokeView{Card: card},
	})
	for _, want := range []string{
		"data-sb-friend-revoke-confirm",
		// What dies: the peer, the local agent it vended onto, the negotiated scope, last activity.
		"carol@peer.example", "peer.example", "b-persona",
		">reviews<", ">create_for<", "last A2A call 2m ago",
		// Consequence stated plainly; one direction only (SPEC-0010 per-direction doctrine).
		"instant and total", "separate edge and is untouched",
		`action="/friends/e-active/revoke"`, `name="csrf_token" value="tok"`,
		"data-sb-friend-revoke-submit",
		">Cancel</a>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("friend revoke page: missing %q", want)
		}
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
		"Request an A2A friendship",      // modal title
		"action=\"/friends\"",            // submits the request
		"name=\"agent_id\"", ">b-agent<", // local agent
		"name=\"handle\"", "hx-get=\"/friends/resolve\"", // remote handle w/ live resolution preview
		// Requested intents are toggle chips: one checkbox per registry verb, with the primary
		// work-handoff intent pre-checked for the no-JS form.
		"sb-chip--toggle",
		"name=\"intents\" value=\"create_for\" checked",
		"name=\"intents\" value=\"list_todos\"",
		"name=\"reason\"",                            // message to their operator
		"mutual and non-transitive",                  // states the mutual + non-transitive contract
		"pending until the remote operator approves", // pending-until-remote-approves
		"name=\"csrf_token\" value=\"tok\"",
		">Cancel<", "Send request →",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("add-friend modal: missing %q", want)
		}
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

// TestFriendCountsFragmentRefreshesBadge pins the OOB rail-badge refresh that rides on every
// friend-action response (same HTTP response, never the shared SSE hub — friend counts are
// per-human and must not fan out cross-tenant).
func TestFriendCountsFragmentRefreshesBadge(t *testing.T) {
	h := enabledFriendsHandler(t)
	frag, err := h.renderFragment("friend_counts", friendCountsView{Counts: friendCounts{Incoming: 3}, OOB: true})
	if err != nil {
		t.Fatalf("render friend_counts: %v", err)
	}
	for _, want := range []string{`id="sb-friends-badge"`, `hx-swap-oob="true"`, ">3</span>"} {
		if !strings.Contains(frag, want) {
			t.Errorf("friend_counts: missing %q in %q", want, frag)
		}
	}
	// Zero incoming hides the badge rather than rendering a zero.
	zero, err := h.renderFragment("friend_counts", friendCountsView{OOB: true})
	if err != nil {
		t.Fatalf("render friend_counts zero: %v", err)
	}
	if !strings.Contains(zero, " hidden") {
		t.Errorf("friend_counts with zero incoming must render hidden: %q", zero)
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
