package web

// Operator Friends view (capability-gated) and the friend-edge approval flow.
//
// Governing: SPEC-0015 REQ "Friends View And Approval Flow" — the view separates *pending · in
// your queue* from *established* edges (direction, peer, queues, last-seen, revoke), and the
// approval flow presents "approving IS the vend": a full confirm page shows the scoped endpoint
// approval mints (vend target agent + narrow-only granted scope), and a successful approval
// surfaces the vended result explicitly on the page — never only a toast. SPEC-0010 REQ "Approval
// Is the Vend, Narrow-Only" (approve mints the scoped endpoint), REQ "Per-Direction, Revocable,
// Non-Transitive Edges" (revoke kills one direction), REQ "Approval Delivered as a Todo" (the
// durable approval todo). The web layer renders and dispatches; the store owns every lifecycle
// transition and its sentinel errors (this layer implements no friend-edge rules of its own).
//
// The approve and revoke confirm pages follow the full-page confirm pattern of the vend/revoke
// flows (SPEC-0015 REQ "Wizard Interaction Pattern": destructive and irreversible steps confirm
// before executing; no-JS completes identically). Approval is single-step, so it needs no wizard
// step state (wizard.go) — the confirm page IS the flow.
//
// Capability gating: the view and all its routes 404 until the friending capability is enabled
// (h.friendsEnabled — the single capability seam), and the rail entry is hidden —
// hidden-not-broken (SPEC-0013 gating doctrine carried into the SPEC-0015 IA).

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// friendGroupOrder is the canonical ordered grouping of the SPEC-0015 framing: the pending edges
// sitting in YOUR queue (awaiting this operator's decision) lead, then pending requests awaiting
// the remote operator, then the established links, then the blocked remainder.
var friendGroupOrder = []struct{ Key, Title string }{
	{"incoming", "Pending · in your queue"},
	{"outgoing", "Pending · awaiting their operator"},
	{"active", "Established"},
	{"blocked", "Blocked"},
}

// friendIntentOptions is the add-friend modal's "Requested intents" chip vocabulary: the verbs a
// remote board can grant on a vended friend endpoint — the SPEC-0007 work-handoff verb (create_for,
// the primary A2A intent, pre-checked so the no-JS form still submits a valid scope) followed by
// the SPEC-0006 drain verbs (mirrors drainVerbs in endpoints.go). Governing: SPEC-0010 REQ
// "Friend-Request Lifecycle" (requested_scope verbs).
var friendIntentOptions = []string{"create_for", "list_todos", "claim", "complete", "fail"}

// friendCard is the render model for one friendship (a single friend edge). Governing: SPEC-0015
// REQ "Friends View And Approval Flow" (direction, peer, queues, last-seen, revoke per edge).
type friendCard struct {
	ID           string   // friend-edge id
	RowID        string   // stable DOM id sb-fr-<id> for OOB swaps
	Group        string   // incoming | outgoing | active | blocked
	Status       string   // raw edge state: pending | approved | denied | revoked
	StatusLabel  string   // human-facing status label (incoming · awaiting · established · blocked)
	Direction    string   // raw edge direction ("outgoing" = locally sent; anything else = inbound)
	Arrow        string   // direction glyph: → outgoing pending, ← incoming pending, ↔ established
	LocalPersona string   // the local face of the edge (to_persona inbound, from_persona outgoing)
	RemoteHandle string   // the remote agent handle (from_persona inbound, to_persona outgoing)
	RemoteHost   string   // the remote board host, parsed best-effort from the handle
	Intents      []string // negotiated (granted) verbs when established, else the requested verbs
	Negotiated   bool     // true when Intents/Queues are the negotiated grant (established)
	Queues       []string // negotiated or requested queues (same rule as Intents)
	Reason       string   // the requester's legible reason (quoted-callout note on incoming cards)
	Meta         string   // the footer meta line ("requested 8m ago" / "last A2A call 2m ago")
	OOB          bool     // render as an hx-swap-oob replacement (live SSE update)
}

// friendGroup is one view section (a group key + its SPEC-0015 heading + its cards).
type friendGroup struct {
	Key   string
	Title string
	Cards []friendCard
}

// friendCounts backs the rail badge (Incoming) and the DB-less helper tests.
type friendCounts struct {
	All      int
	Incoming int
	Outgoing int
	Active   int
	Blocked  int
}

// friendPanelView feeds the "friends_panel" fragment (the swappable sections region) and the
// add-friend modal. Only the modal render populates Agents/IntentOptions.
type friendPanelView struct {
	Groups []friendGroup
	CSRF   string
	// Agents is the add-friend modal's local-agent picker (the human's own agents).
	Agents []store.Agent
	// IntentOptions is the add-friend modal's requested-intent chip vocabulary (friendIntentOptions).
	IntentOptions []string
}

// friendCountsView feeds the "friend_counts" fragment: the rail incoming badge, refreshed OOB in
// the same HTTP response after a friend action (never over the shared SSE hub — friend counts are
// per-human and must not fan out cross-tenant).
type friendCountsView struct {
	Counts friendCounts
	OOB    bool
}

// friendResolveView feeds the "friend_resolve" fragment: the add-friend modal's live handle preview.
type friendResolveView struct {
	Handle string
	Host   string
	Valid  bool
}

// friendApproveView feeds the approve confirm page (templates/friend_approve.html): the pending
// incoming edge under review plus the human's own agents (the vend target picker). The page IS the
// "approving is the vend" presentation — it shows the scoped endpoint approval mints before the
// operator executes it. Governing: SPEC-0015 REQ "Friends View And Approval Flow"; SPEC-0010 REQ
// "Approval Is the Vend, Narrow-Only".
type friendApproveView struct {
	Card   friendCard
	Agents []store.Agent
}

// friendRevokeView feeds the revoke confirm page (templates/friend_revoke.html): the established
// edge whose vended endpoint dies on confirm. Governing: SPEC-0015 REQ "Wizard Interaction
// Pattern" (destructive and irreversible steps confirm); SPEC-0010 (revoke kills one direction).
type friendRevokeView struct {
	Card friendCard
}

// friendVendedView feeds the "friend_vended" result panel rendered inline on the Friends page as
// the response to a successful approval: the vended endpoint's identity (agent, MCP URL, credential
// display prefix) and its granted scope, stated explicitly — not only a toast. The plaintext
// credential is NOT here: it is delivered to the remote agent in the A2A response (SPEC-0010), never
// to this operator. Governing: SPEC-0015 REQ "Friends View And Approval Flow" (scenario "Approve
// mints and shows the grant").
type friendVendedView struct {
	Peer       string // the remote handle the grant was approved for
	PeerHost   string // the remote board host, parsed best-effort from the handle
	AgentName  string // the local agent the endpoint vended onto
	URL        string // the minted /mcp/{slug} endpoint URL
	CredPrefix string // non-secret credential display prefix (never a reusable credential)
	Queues     []string
	Verbs      []string
}

// friendDirectionOutgoing marks a friend edge the LOCAL human sent (AddFriend). Inbound A2A-intake
// edges carry the store default ("outbound", the requester→target grant direction); only locally
// originated requests are stamped "outgoing", so the two never collide on the live unique index and
// the web layer can group by direction. Governing: SPEC-0010 REQ "Per-Direction, Revocable,
// Non-Transitive Edges"; story #174 (persist outgoing requests; direction semantics).
const friendDirectionOutgoing = "outgoing"

// friendGroupForEdge maps a friend-edge (state, direction) onto its view group and status-badge
// label (incoming · awaiting · established · blocked). A pending edge is in YOUR queue when it
// arrived over A2A intake, and awaiting the remote operator when this human sent it
// (direction=outgoing); approved is established (the SPEC-0015 scenario's word); denied/revoked
// are blocked regardless of who initiated (the meta line preserves the declined-vs-revoked
// distinction).
func friendGroupForEdge(state, direction string) (group, label string) {
	switch state {
	case "pending":
		if direction == friendDirectionOutgoing {
			return "outgoing", "awaiting"
		}
		return "incoming", "incoming"
	case "approved":
		return "active", "established"
	case "denied", "revoked":
		return "blocked", "blocked"
	default:
		return "blocked", state
	}
}

// friendMetaForEdge renders the card footer meta line: relative request/sent ages, the established
// edge's last A2A activity, and the declined-vs-revoked distinction on blocked entries. The store
// keeps no per-edge A2A call COUNTER (the wire layer lands with epic #173), so the established line
// surfaces the vended endpoint's last-seen stamp — every authenticated A2A call touches it. Zero/
// absent times render no clause, so legacy rows and DB-less fixtures degrade to an empty meta
// rather than a bogus age.
func friendMetaForEdge(e store.FriendEdge, group string) string {
	switch group {
	case "incoming":
		if e.CreatedAt.IsZero() {
			return ""
		}
		return "requested " + relTime(e.CreatedAt)
	case "outgoing":
		if e.CreatedAt.IsZero() {
			return ""
		}
		return "sent " + relTime(e.CreatedAt) + " · awaiting them"
	case "active":
		if e.EndpointLastSeenAt != nil {
			return "last A2A call " + relTime(*e.EndpointLastSeenAt)
		}
		return "no A2A calls yet"
	default: // blocked — keep the declined-vs-revoked distinction the badge label folds away
		verb, at := "declined", e.DecidedAt
		if e.State == "revoked" {
			verb, at = "revoked", e.RevokedAt
		}
		if at == nil {
			if e.CreatedAt.IsZero() {
				return ""
			}
			at = &e.CreatedAt
		}
		return verb + " " + relTime(*at)
	}
}

// friendArrowForGroup is the card identity-row direction glyph: → an outgoing pending request, ← an
// incoming pending request, ↔ an established (or terminal) link. Governing: SPEC-0015 REQ "Friends
// View And Approval Flow" (direction per edge).
func friendArrowForGroup(group string) string {
	switch group {
	case "outgoing":
		return "→"
	case "incoming":
		return "←"
	default:
		return "↔"
	}
}

// remoteHostFromHandle extracts a best-effort board host from a remote persona handle for display
// (e.g. "alice@board.example" → "board.example", "peer://agent@host/x" → "host"). It never dials the
// host; it is a purely syntactic split, so it adds no SSRF surface. Returns "" when no host is legible.
func remoteHostFromHandle(handle string) string {
	h := handle
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	// Trim any path/query after the host.
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if h == handle {
		// No scheme and no "@": the handle carries no separable host.
		if !strings.Contains(handle, "@") && !strings.Contains(handle, "://") {
			return ""
		}
	}
	return h
}

// friendCardFromEdge builds one friend card from a store edge. Established (approved) edges show
// the negotiated (granted) scope; every other state shows the requested scope. The local/remote
// split is direction-aware: an inbound edge's local face is to_persona (the remote requester is
// from_persona); a locally sent (direction=outgoing) edge inverts that — from_persona is the local
// agent and to_persona is the remote handle being asked.
func friendCardFromEdge(e store.FriendEdge) friendCard {
	group, label := friendGroupForEdge(e.State, e.Direction)
	intents, queues, negotiated := e.RequestedVerbs, e.RequestedQueues, false
	if e.State == "approved" {
		intents, queues, negotiated = e.GrantedVerbs, e.GrantedQueues, true
	}
	local, remote := e.ToPersona, e.FromPersona
	if e.Direction == friendDirectionOutgoing {
		local, remote = e.FromPersona, e.ToPersona
	}
	return friendCard{
		ID:           e.ID,
		RowID:        "sb-fr-" + e.ID,
		Group:        group,
		Status:       e.State,
		StatusLabel:  label,
		Direction:    e.Direction,
		Arrow:        friendArrowForGroup(group),
		LocalPersona: local,
		RemoteHandle: remote,
		RemoteHost:   remoteHostFromHandle(remote),
		Intents:      intents,
		Negotiated:   negotiated,
		Queues:       queues,
		Reason:       e.Reason,
		Meta:         friendMetaForEdge(e, group),
	}
}

// friendCardsFromEdges maps edges to cards preserving the store's newest-first order.
func friendCardsFromEdges(edges []store.FriendEdge) []friendCard {
	cards := make([]friendCard, 0, len(edges))
	for _, e := range edges {
		cards = append(cards, friendCardFromEdge(e))
	}
	return cards
}

// friendCountsFrom tallies the per-group counts (the rail badge reads Incoming).
func friendCountsFrom(cards []friendCard) friendCounts {
	c := friendCounts{All: len(cards)}
	for _, card := range cards {
		switch card.Group {
		case "incoming":
			c.Incoming++
		case "outgoing":
			c.Outgoing++
		case "active":
			c.Active++
		case "blocked":
			c.Blocked++
		}
	}
	return c
}

// groupFriendCards buckets cards into the canonical SPEC-0015 sections, preserving order. Empty
// sections are dropped (the view renders only populated groups; the whole-view empty state covers
// "none at all").
func groupFriendCards(cards []friendCard) []friendGroup {
	byKey := map[string][]friendCard{}
	for _, card := range cards {
		byKey[card.Group] = append(byKey[card.Group], card)
	}
	var groups []friendGroup
	for _, g := range friendGroupOrder {
		if len(byKey[g.Key]) == 0 {
			continue
		}
		groups = append(groups, friendGroup{Key: g.Key, Title: g.Title, Cards: byKey[g.Key]})
	}
	return groups
}

// friendsEnabled is THE single capability check for the friending surface — the shell's rail
// entry (buildShell) and every /friends handler (via friendingEnabled below) consult this one
// seam instead of reading config/env in scattered places, mirroring how personasEnabled gates the
// Personas surface. Today the capability derives from the one-time config load (the
// SWITCHBOARD_FRIENDING opt-in stays a deliberate operator switch); flipping it to store-backed
// feature detection later touches only this method.
func (h *Handler) friendsEnabled() bool { return h.cfg.FriendingEnabled }

// friendingEnabled reports whether the friending capability is on; when off it writes a 404 and the
// caller returns immediately (the view and its routes 404 until the capability is enabled —
// hidden-not-broken).
func (h *Handler) friendingEnabled(w http.ResponseWriter) bool {
	if !h.friendsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// Friends renders the capability-gated Friends view: the pending-in-your-queue / awaiting-them /
// established / blocked sections, server-rendered from the owner's friend edges. (The panel is
// re-rendered by friend action POSTs — respondFriendAction — not by HTMX GETs; the SPEC-0013
// layout/filter toolbar is retired.) Requires human. Governing: SPEC-0015 REQ "Friends View And
// Approval Flow".
func (h *Handler) Friends(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	h.renderFriendsPage(w, r, human, nil)
}

// renderFriendsPage renders the full Friends page, optionally with the post-approval vended-result
// panel inline (SPEC-0015: the vended result is surfaced explicitly on the page, not only a toast).
// A list failure while the DB is up is degraded to a logged empty view; a reload recovers.
func (h *Handler) renderFriendsPage(w http.ResponseWriter, r *http.Request, human store.Human, vended *friendVendedView) {
	sh, _ := h.buildShell(r.Context(), "friends", &human)
	var cards []friendCard
	if sh.DBConnected {
		edges, err := h.store.ListFriendEdges(r.Context(), human.ID)
		if err != nil {
			h.log.Warn("friends list", "err", err)
		}
		cards = friendCardsFromEdges(edges)
	}
	h.render(w, "friends", view{
		Title: "Friends", Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		FriendGroups: groupFriendCards(cards), FriendCounts: friendCountsFrom(cards),
		FriendVended: vended,
	})
}

// AddFriendModal renders the add-friend modal into the overlay slot (HTMX). It collects the local
// agent, the remote handle (with a live resolution preview), the requested intents, and a message to
// the remote operator. Requires human.
func (h *Handler) AddFriendModal(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	agents, err := h.store.ListAgents(r.Context(), human.ID)
	if err != nil {
		h.log.Warn("add-friend agents", "err", err)
	}
	frag, err := h.renderFragment("add_friend_modal", friendPanelView{
		Agents: agents, CSRF: auth.CSRFFromContext(r.Context()), IntentOptions: friendIntentOptions,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// ResolveFriendHandle returns the add-friend modal's live handle-resolution preview. The resolution is
// purely syntactic (it parses the handle into a remote agent + board host and never dials the host),
// so it adds no SSRF surface. Requires human.
func (h *Handler) ResolveFriendHandle(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	handle := strings.TrimSpace(r.URL.Query().Get("handle"))
	rv := friendResolveView{Handle: handle, Host: remoteHostFromHandle(handle), Valid: handle != ""}
	frag, err := h.renderFragment("friend_resolve", rv)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// AddFriend handles the add-friend modal submit (POST /friends): a friendship request to a remote
// board's persona. It validates the local agent (owned), a non-empty remote handle, and at least one
// requested intent, then durably records the request as a LOCAL pending edge with direction=outgoing
// — from_persona is the local agent, to_persona the remote handle, and the edge is owned by (and
// only visible to) the sending human, so the awaiting-them group populates immediately and Withdraw
// is reachable. The pending edge grants nothing (SPEC-0010); the outbound A2A send that records it on
// the REMOTE board is a separate wire concern (epic #173) and its absence never loses the local
// record. A duplicate live request for the same (local agent, handle) pair is refused (409) by the
// store's anti-flood unique index. Requires human + CSRF. Governing: SPEC-0010 REQ "Friend-Request
// Lifecycle" (pending edge grants nothing), REQ "Per-Direction, Revocable, Non-Transitive Edges";
// story #174.
func (h *Handler) AddFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	handle := strings.TrimSpace(r.FormValue("handle"))
	// The modal's intent toggle chips submit one value per checked box; multiValues also accepts a
	// single comma-separated value so a hand-rolled or legacy client degrades to the same scope.
	intents := multiValues(r, "intents")
	if agentID == "" || handle == "" || len(intents) == 0 {
		http.Error(w, "local agent, remote handle, and at least one intent are required", http.StatusBadRequest)
		return
	}
	// Owned-agent guard: the local agent must belong to the requesting human (never a foreign id).
	ag, err := h.store.GetAgentOwned(r.Context(), agentID, human.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown local agent", http.StatusBadRequest)
			return
		}
		h.fail(w, err)
		return
	}
	if _, err := h.store.CreateFriendRequest(r.Context(), store.CreateFriendRequestParams{
		FromPersona:    ag.Name,
		ToPersona:      handle,
		Direction:      friendDirectionOutgoing,
		FromHuman:      human.ID,
		ToHuman:        human.ID, // ownership key: the sender owns (lists, withdraws) their outgoing edge
		RequestedVerbs: intents,
		Reason:         strings.TrimSpace(r.FormValue("reason")),
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			http.Error(w, "a live friend request for this pair already exists", http.StatusConflict)
			return
		}
		h.fail(w, err)
		return
	}
	// closeOverlay: a successful submit dismisses the add-friend modal in the same response (#175);
	// an error response (4xx above) swaps nothing, so the modal stays open for correction.
	h.respondFriendAction(w, r, human, "request sent to "+handle+" · awaiting their operator", true)
}

// ApproveFriendPage renders the approval confirm page (GET /friends/{id}/approve): the "approving
// IS the vend" presentation. It shows the pending request (peer, their pitch, the requested scope)
// and exactly what approval mints — a scoped endpoint on one of YOUR agents, with the granted
// queues/verbs as narrow-only toggle chips pre-checked to the requested scope. Only a pending,
// non-outgoing edge owned by this human renders; anything else 404s without leaking which check
// failed. Requires human. Governing: SPEC-0015 REQ "Friends View And Approval Flow"; SPEC-0010 REQ
// "Approval Is the Vend, Narrow-Only".
func (h *Handler) ApproveFriendPage(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")
	edges, err := h.store.ListFriendEdges(r.Context(), human.ID, "pending")
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, e := range edges {
		// A locally sent (outgoing) pending edge awaits the REMOTE operator — it can never be
		// approved here.
		if e.ID != edgeID || e.Direction == friendDirectionOutgoing {
			continue
		}
		agents, err := h.store.ListAgents(r.Context(), human.ID)
		if err != nil {
			h.log.Warn("approve-friend agents", "err", err)
		}
		sh, _ := h.buildShell(r.Context(), "friends", &human)
		h.render(w, "friend_approve", view{
			Title: "Approve friendship", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
			Shell: sh, FriendApprove: &friendApproveView{Card: friendCardFromEdge(e), Agents: agents},
		})
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ApproveFriend executes the approval (POST /friends/{id}/approve, reached from the confirm page).
// Approval IS the vend: it mints a scoped MCP endpoint onto a TARGET-OWNED agent and transitions
// the edge to established, then re-renders the Friends page with the vended result — endpoint
// identity (agent, MCP URL, credential prefix) and granted scope — surfaced explicitly (SPEC-0015:
// never only a toast). The plaintext credential is delivered to the remote agent by the A2A
// response, not shown here. The owned-agent guard is load-bearing (wave-4 verification finding /
// hardening #152): the agent the endpoint is minted onto is resolved from the APPROVING human's own
// agents via GetAgentOwned, so a caller-supplied foreign agent id can never receive a vended
// credential. Requires human + CSRF. Governing: SPEC-0015 REQ "Friends View And Approval Flow"
// (scenario "Approve mints and shows the grant"); SPEC-0010 REQ "Approval Is the Vend,
// Narrow-Only"; ADR-0008 (approval mints the grant).
func (h *Handler) ApproveFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")

	// Resolve the vend target from the approving human's OWN agents — never trust the posted id blindly.
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	ag, err := h.store.GetAgentOwned(r.Context(), agentID, human.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No owned agent selected/known: approval cannot mint onto a foreign or absent agent.
			http.Error(w, "select one of your agents to vend the endpoint for", http.StatusBadRequest)
			return
		}
		h.fail(w, err)
		return
	}

	token, hash, prefix, err := cred.Mint()
	if err != nil {
		h.fail(w, err)
		return
	}
	_ = token // the plaintext is delivered to the remote out-of-band by the A2A response (not surfaced here)
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	// Narrowing from the confirm page's toggle chips (one value per checked box; multiValues also
	// accepts CSV for legacy/hand-rolled clients). Unchecking everything means "no narrowing" —
	// the store grants the requested scope (SPEC-0010 narrow-only doctrine lives in the store).
	edge, ep, err := h.store.ApproveFriendRequest(r.Context(), store.ApproveFriendRequestParams{
		EdgeID:           edgeID,
		OwnerHumanID:     human.ID,
		AgentID:          ag.ID,
		GrantedQueues:    multiValues(r, "granted_queues"),
		GrantedVerbs:     multiValues(r, "granted_verbs"),
		CredentialHash:   hash,
		CredentialPrefix: prefix,
		Slug:             slug,
	})
	if err != nil {
		h.failFriendAction(w, "ApproveFriend", edgeID, err)
		return
	}
	// Clear the durable approval todo. The state change is deliberately NOT broadcast over the
	// shared SSE hub: that hub fans out to every connected session regardless of human, so
	// publishing one owner's friend state there would leak it cross-tenant. Other sessions
	// reconcile on reload (SSE is best-effort; the DB is authoritative).
	if err := h.store.ResolveApprovalTodo(r.Context(), edgeID); err != nil {
		h.log.Warn("resolve approval todo", "edge", edgeID, "err", err)
	}
	// Surface the vended result explicitly: the full page re-renders with the minted endpoint's
	// identity and scope inline, and the edge now sits in the Established section.
	peer := friendCardFromEdge(edge)
	h.renderFriendsPage(w, r, human, &friendVendedView{
		Peer:       peer.RemoteHandle,
		PeerHost:   peer.RemoteHost,
		AgentName:  ag.Name,
		URL:        mcpEndpointURL(h.cfg.BaseURL, ep.Slug),
		CredPrefix: ep.CredentialPrefix,
		Queues:     edge.GrantedQueues,
		Verbs:      edge.GrantedVerbs,
	})
}

// DeclineFriend declines an incoming request (POST /friends/{id}/decline) → terminal denied, no vend.
// Requires human + CSRF. Governing: SPEC-0010 REQ "Friend-Request Lifecycle".
func (h *Handler) DeclineFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")
	if _, err := h.store.DenyFriendRequest(r.Context(), edgeID, human.ID); err != nil {
		h.failFriendAction(w, "DeclineFriend", edgeID, err)
		return
	}
	if err := h.store.ResolveApprovalTodo(r.Context(), edgeID); err != nil {
		h.log.Warn("resolve approval todo", "edge", edgeID, "err", err)
	}
	h.respondFriendAction(w, r, human, "friend request declined", false)
}

// RevokeFriendPage renders the revoke confirm page (GET /friends/{id}/revoke) for an established
// edge: revocation kills the vended endpoint — the remote agent's credential dies instantly — and
// is one-sided (the reverse-direction edge, if any, is a separate row and is untouched), so it
// confirms before executing per the wizard interaction pattern. An unknown, foreign, or
// non-established edge 404s without leaking which of those it was. Requires human. Governing:
// SPEC-0015 REQ "Wizard Interaction Pattern"; SPEC-0010 REQ "Per-Direction, Revocable,
// Non-Transitive Edges".
func (h *Handler) RevokeFriendPage(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")
	edges, err := h.store.ListFriendEdges(r.Context(), human.ID, "approved")
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, e := range edges {
		if e.ID != edgeID {
			continue
		}
		sh, _ := h.buildShell(r.Context(), "friends", &human)
		h.render(w, "friend_revoke", view{
			Title: "Revoke friendship", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
			Shell: sh, FriendRevoke: &friendRevokeView{Card: friendCardFromEdge(e)},
		})
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// RevokeFriend revokes an established link (POST /friends/{id}/revoke, reached from the confirm
// page) → kills the vended endpoint, one direction only. Requires human + CSRF. Governing:
// SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges", ADR-0008 (revoke is instant and
// total).
func (h *Handler) RevokeFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")
	edge, err := h.store.RevokeFriendEdge(r.Context(), edgeID, human.ID)
	if err != nil {
		h.failFriendAction(w, "RevokeFriend", edgeID, err)
		return
	}
	// Tearing down the endpoint's live MCP streams promptly mirrors the Endpoints Revoke path.
	if edge.EndpointID != "" && h.endpointRevoked != nil {
		h.endpointRevoked(edge.EndpointID)
	}
	h.respondFriendAction(w, r, human, "friendship revoked · endpoint killed", false)
}

// WithdrawFriend withdraws an outgoing pending request (POST /friends/{id}/withdraw) → removes the
// entry. Requires human + CSRF. Governing: SPEC-0010 REQ "Friend-Request Lifecycle"; story #174.
func (h *Handler) WithdrawFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")
	if _, err := h.store.RemoveFriendEdge(r.Context(), edgeID, human.ID, "pending"); err != nil {
		h.failFriendAction(w, "WithdrawFriend", edgeID, err)
		return
	}
	h.respondFriendAction(w, r, human, "request withdrawn", false)
}

// UnblockFriend removes a blocked (declined/revoked) entry from the view (POST
// /friends/{id}/unblock). Requires human + CSRF.
func (h *Handler) UnblockFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	edgeID := chi.URLParam(r, "id")
	if _, err := h.store.RemoveFriendEdge(r.Context(), edgeID, human.ID, "denied", "revoked"); err != nil {
		h.failFriendAction(w, "UnblockFriend", edgeID, err)
		return
	}
	h.respondFriendAction(w, r, human, "entry removed", false)
}

// failFriendAction maps a friend-edge store transition error onto a generic HTTP status with no
// internal detail (error-handling standards inherited from SPEC-0012): not-found → 404, invalid
// transition → 409, scope-exceeds-request → 400, anything else → a logged 500.
func (h *Handler) failFriendAction(w http.ResponseWriter, handler, edgeID string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalidTransition):
		http.Error(w, "conflict", http.StatusConflict)
	case errors.Is(err, store.ErrScopeExceedsRequest):
		http.Error(w, "granted scope exceeds requested", http.StatusBadRequest)
	default:
		h.log.Error("friend action", "handler", handler, "edge", edgeID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// respondFriendAction re-renders the Friends panel so an action that moves a card between sections
// reflects immediately, and prepends a toast. closeOverlay additionally clears the shared overlay
// OOB in the same response — the add-friend modal dismisses itself on a successful submit (#175) —
// while card-level actions leave the overlay alone. A non-HTMX submit redirects back to the view.
// Requires the human to rebuild their scoped listing.
func (h *Handler) respondFriendAction(w http.ResponseWriter, r *http.Request, human store.Human, toast string, closeOverlay bool) {
	if !isHTMX(r) {
		// Post-action redirects target a fixed same-origin path (no user-supplied target).
		http.Redirect(w, r, "/friends", http.StatusSeeOther)
		return
	}
	edges, err := h.store.ListFriendEdges(r.Context(), human.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	cards := friendCardsFromEdges(edges)
	frag, err := h.renderFragment("friends_panel", friendPanelView{
		Groups: groupFriendCards(cards), CSRF: auth.CSRFFromContext(r.Context()),
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	if toast != "" {
		if t, err := h.renderFragment("toast", toast); err == nil {
			frag += t
		} else {
			h.log.Error("render friend toast", "err", err)
		}
	}
	// Refresh the rail badge OOB alongside the panel swap.
	if cf, err := h.renderFragment("friend_counts", friendCountsView{Counts: friendCountsFrom(cards), OOB: true}); err == nil {
		frag += cf
	}
	if closeOverlay {
		if oc, err := h.renderFragment("overlay_clear", nil); err == nil {
			frag += oc
		} else {
			h.log.Error("render overlay clear", "err", err)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}
