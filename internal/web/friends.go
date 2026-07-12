package web

// Operator Friends view (capability-gated) and the friend-edge approval flow.
//
// Governing: SPEC-0013 REQ "Friends View" — two switchable layouts (cards + grouped ledger), filter
// pills with counts, per-status actions, and an add-friend modal; SPEC-0010 REQ "Approval Is the
// Vend, Narrow-Only" (approve mints the scoped endpoint), REQ "Per-Direction, Revocable,
// Non-Transitive Edges" (revoke kills one direction), REQ "Approval Delivered as a Todo" (the durable
// approval todo). The web layer renders and dispatches; the store owns every lifecycle transition and
// its sentinel errors (this layer implements no friend-edge rules of its own).
//
// Capability gating: the view and all its routes 404 until the friending capability is enabled
// (h.cfg.FriendingEnabled), and the rail entry is hidden — hidden-not-broken (SPEC-0013 IA & Nav).

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// friendGroups is the canonical ordered ledger grouping (SPEC-0013 Incoming / Outgoing / Active /
// Blocked). The filter pills are these four plus "all".
var friendGroupOrder = []struct{ Key, Label string }{
	{"incoming", "Incoming"},
	{"outgoing", "Outgoing"},
	{"active", "Active"},
	{"blocked", "Blocked"},
}

// friendCard is the render model for one friendship (a single friend edge) in both the cards and the
// grouped-ledger layouts. Governing: SPEC-0013 REQ "Friends View" (each friendship shows local agent,
// remote agent + board host, status, negotiated/requested intents, and status-appropriate actions).
type friendCard struct {
	ID           string   // friend-edge id
	RowID        string   // stable DOM id sb-fr-<id> for OOB swaps
	Group        string   // incoming | outgoing | active | blocked
	Status       string   // raw edge state: pending | approved | denied | revoked
	StatusLabel  string   // human-facing status label
	Direction    string   // raw edge direction ("outgoing" = locally sent; anything else = inbound)
	Arrow        string   // direction glyph: → outgoing pending, ← incoming pending, ↔ established
	LocalPersona string   // the local face of the edge (to_persona inbound, from_persona outgoing)
	RemoteHandle string   // the remote agent handle (from_persona inbound, to_persona outgoing)
	RemoteHost   string   // the remote board host, parsed best-effort from the handle
	Intents      []string // negotiated (granted) verbs when active, else the requested verbs
	Negotiated   bool     // true when Intents are the negotiated grant (active), false when requested
	Queues       []string // negotiated or requested queues (same rule as Intents)
	Reason       string   // the requester's legible reason
	OOB          bool     // render as an hx-swap-oob replacement (live SSE update)
}

// friendGroup is one ledger section (a group key + its label + its cards).
type friendGroup struct {
	Key   string
	Label string
	Cards []friendCard
}

// friendCounts backs the Friends filter-pill counts (All / Incoming / Outgoing / Active / Blocked).
type friendCounts struct {
	All      int
	Incoming int
	Outgoing int
	Active   int
	Blocked  int
}

// friendPanelView feeds the "friends_panel" fragment: the layout toggle + filter pills + the active
// layout (cards grid or grouped ledger). Carries the current layout/filter so action POSTs re-render
// the panel in the operator's chosen view, plus the human's agents for the approve/add-friend forms.
type friendPanelView struct {
	Layout   string // cards | ledger
	Filter   string // all | incoming | outgoing | active | blocked
	Counts   friendCounts
	Groups   []friendGroup // the four ledger sections (already filtered)
	Cards    []friendCard  // the flat, filtered card list (cards layout)
	Agents   []store.Agent // the human's local agents (approve target + add-friend local agent)
	CSRF     string
	Filtered bool // whether a non-"all" filter is active (affects empty-state copy)
}

// friendCountsView feeds the "friend_counts" fragment: the Friends filter-pill counts and the rail
// incoming badge, refreshed OOB in the same HTTP response after a friend action (never over the
// shared SSE hub — friend counts are per-human and must not fan out cross-tenant).
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

// normalizeFriendLayout maps a query/form layout to a canonical value, defaulting to cards.
func normalizeFriendLayout(s string) string {
	if strings.ToLower(strings.TrimSpace(s)) == "ledger" {
		return "ledger"
	}
	return "cards"
}

// normalizeFriendFilter maps a query/form filter to a canonical pill key, defaulting to all.
func normalizeFriendFilter(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "incoming":
		return "incoming"
	case "outgoing":
		return "outgoing"
	case "active":
		return "active"
	case "blocked":
		return "blocked"
	default:
		return "all"
	}
}

// friendDirectionOutgoing marks a friend edge the LOCAL human sent (AddFriend). Inbound A2A-intake
// edges carry the store default ("outbound", the requester→target grant direction); only locally
// originated requests are stamped "outgoing", so the two never collide on the live unique index and
// the web layer can group by direction. Governing: SPEC-0010 REQ "Per-Direction, Revocable,
// Non-Transitive Edges"; story #174 (persist outgoing requests; direction semantics).
const friendDirectionOutgoing = "outgoing"

// friendGroupForEdge maps a friend-edge (state, direction) onto its ledger group and a human status
// label. A pending edge is an Incoming request when it arrived over A2A intake, and an Outgoing
// request when this human sent it (direction=outgoing, awaiting the remote operator); approved is
// Active; denied/revoked are Blocked regardless of who initiated.
func friendGroupForEdge(state, direction string) (group, label string) {
	switch state {
	case "pending":
		if direction == friendDirectionOutgoing {
			return "outgoing", "requested"
		}
		return "incoming", "pending"
	case "approved":
		return "active", "active"
	case "denied":
		return "blocked", "declined"
	case "revoked":
		return "blocked", "revoked"
	default:
		return "blocked", state
	}
}

// friendArrowForGroup is the card identity-row direction glyph: → an outgoing pending request, ← an
// incoming pending request, ↔ an established (or terminal) link. Matches the design canvas Friends
// section (direction arrows per status). Governing: SPEC-0013 REQ "Friends View"; DESIGN (Friends).
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

// friendCardFromEdge builds one friend card from a store edge. Active (approved) edges show the
// negotiated (granted) scope; every other state shows the requested scope. The local/remote split is
// direction-aware: an inbound edge's local face is to_persona (the remote requester is from_persona);
// a locally sent (direction=outgoing) edge inverts that — from_persona is the local agent and
// to_persona is the remote handle being asked.
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

// friendCountsFrom tallies the filter-pill counts across all cards (unfiltered).
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

// filterFriendCards returns the cards visible under the active filter ("all" = every card).
func filterFriendCards(cards []friendCard, filter string) []friendCard {
	if filter == "all" {
		return cards
	}
	out := make([]friendCard, 0, len(cards))
	for _, card := range cards {
		if card.Group == filter {
			out = append(out, card)
		}
	}
	return out
}

// groupFriendCards buckets the (already filter-visible) cards into the canonical ledger sections,
// preserving order. Empty sections are retained under the "all" filter so the ledger always shows the
// four groups; under a specific filter only the matching section is returned.
func groupFriendCards(cards []friendCard, filter string) []friendGroup {
	byKey := map[string][]friendCard{}
	for _, card := range cards {
		byKey[card.Group] = append(byKey[card.Group], card)
	}
	var groups []friendGroup
	for _, g := range friendGroupOrder {
		if filter != "all" && filter != g.Key {
			continue
		}
		groups = append(groups, friendGroup{Key: g.Key, Label: g.Label, Cards: byKey[g.Key]})
	}
	return groups
}

// friendingEnabled reports whether the friending capability is on; when off it writes a 404 and the
// caller returns immediately (SPEC-0013: the view and its routes 404 until the capability is enabled).
func (h *Handler) friendingEnabled(w http.ResponseWriter) bool {
	if !h.cfg.FriendingEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// Friends renders the capability-gated Friends view: the layout toggle, the filter pills, and either
// the cards grid or the grouped ledger, all server-rendered from the owner's friend edges. HTMX
// layout/filter swaps return just the panel fragment. Requires human. Governing: SPEC-0013 REQ
// "Friends View".
func (h *Handler) Friends(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	layout := normalizeFriendLayout(r.URL.Query().Get("layout"))
	filter := normalizeFriendFilter(r.URL.Query().Get("filter"))

	sh, _ := h.buildShell(r.Context(), "friends", &human)
	var (
		cards  []friendCard
		agents []store.Agent
	)
	if sh.DBConnected {
		edges, err := h.store.ListFriendEdges(r.Context(), human.ID)
		if err != nil {
			// Suppressed to a log so the view still renders (degraded/empty); a reload recovers.
			h.log.Warn("friends list", "err", err)
		}
		cards = friendCardsFromEdges(edges)
		if agents, err = h.store.ListAgents(r.Context(), human.ID); err != nil {
			h.log.Warn("friends agents", "err", err)
		}
	}
	panel := h.friendPanelView(cards, agents, layout, filter, auth.CSRFFromContext(r.Context()))

	if isHTMX(r) {
		frag, err := h.renderFragment("friends_panel", panel)
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(frag))
		return
	}
	h.render(w, "friends", view{
		Title: "Friends", Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		FriendGroups: panel.Groups, FriendCards: panel.Cards, FriendCounts: panel.Counts,
		FriendLayout: layout, FriendFilter: filter, Agents: agents,
	})
}

// friendPanelView assembles the panel render model: counts over ALL cards, with the groups and the
// flat card list scoped to the active filter.
func (h *Handler) friendPanelView(cards []friendCard, agents []store.Agent, layout, filter, csrf string) friendPanelView {
	return friendPanelView{
		Layout:   layout,
		Filter:   filter,
		Counts:   friendCountsFrom(cards),
		Groups:   groupFriendCards(filterFriendCards(cards, filter), filter),
		Cards:    filterFriendCards(cards, filter),
		Agents:   agents,
		CSRF:     csrf,
		Filtered: filter != "all",
	}
}

// AddFriendModal renders the add-friend modal into the overlay slot (HTMX). It collects the local
// agent, the remote handle (with a live resolution preview), the requested intents, and a message to
// the remote operator. Requires human. Governing: SPEC-0013 REQ "Friends View" (Add friend modal).
func (h *Handler) AddFriendModal(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	agents, err := h.store.ListAgents(r.Context(), human.ID)
	if err != nil {
		h.log.Warn("add-friend agents", "err", err)
	}
	frag, err := h.renderFragment("add_friend_modal", friendPanelView{Agents: agents, CSRF: auth.CSRFFromContext(r.Context())})
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// ResolveFriendHandle returns the add-friend modal's live handle-resolution preview. The resolution is
// purely syntactic (it parses the handle into a remote agent + board host and never dials the host),
// so it adds no SSRF surface. Requires human. Governing: SPEC-0013 REQ "Friends View" (remote handle
// with live resolution preview).
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
// only visible to) the sending human, so the Outgoing group populates immediately and Withdraw is
// reachable. The pending edge grants nothing (SPEC-0010); the outbound A2A send that records it on
// the REMOTE board is a separate wire concern (epic #173) and its absence never loses the local
// record. A duplicate live request for the same (local agent, handle) pair is refused (409) by the
// store's anti-flood unique index. Requires human + CSRF. Governing: SPEC-0013 REQ "Friends View"
// (Add friend modal; Withdraw for outgoing), SPEC-0010 REQ "Friend-Request Lifecycle" (pending edge
// grants nothing), REQ "Per-Direction, Revocable, Non-Transitive Edges"; story #174.
func (h *Handler) AddFriend(w http.ResponseWriter, r *http.Request) {
	if !h.friendingEnabled(w) {
		return
	}
	human, _ := auth.FromContext(r.Context())
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	handle := strings.TrimSpace(r.FormValue("handle"))
	intents := splitCSV(r.FormValue("intents"))
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
	h.respondFriendAction(w, r, human, "friend request sent · "+handle+" · pending remote approval")
}

// ApproveFriend approves an incoming request (POST /friends/{id}/approve). Approval IS the vend: it
// mints a scoped MCP endpoint onto a TARGET-OWNED agent and transitions the edge to active. The
// owned-agent guard is load-bearing (wave-4 verification finding / hardening #152): the agent the
// endpoint is minted onto is resolved from the APPROVING human's own agents via GetAgentOwned, so a
// caller-supplied foreign agent id can never receive a vended credential. Requires human + CSRF.
// Governing: SPEC-0010 REQ "Approval Is the Vend, Narrow-Only", ADR-0008 (approval mints the grant).
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
	// Optional narrowing from the approve form; empty means "no narrowing" (grant the requested scope).
	if _, _, err := h.store.ApproveFriendRequest(r.Context(), store.ApproveFriendRequestParams{
		EdgeID:           edgeID,
		OwnerHumanID:     human.ID,
		AgentID:          ag.ID,
		GrantedQueues:    splitCSV(r.FormValue("granted_queues")),
		GrantedVerbs:     splitCSV(r.FormValue("granted_verbs")),
		CredentialHash:   hash,
		CredentialPrefix: prefix,
		Slug:             slug,
	}); err != nil {
		h.failFriendAction(w, "ApproveFriend", edgeID, err)
		return
	}
	// Clear the durable approval todo. The live move to the Active group is delivered by re-rendering
	// the acting operator's panel below (their approve button targets #sb-friends-panel). It is
	// deliberately NOT broadcast over the shared SSE hub: that hub fans out to every connected
	// session regardless of human, so publishing one owner's friend state there would leak it
	// cross-tenant. Other sessions reconcile on reload (SSE is best-effort; the DB is authoritative).
	if err := h.store.ResolveApprovalTodo(r.Context(), edgeID); err != nil {
		h.log.Warn("resolve approval todo", "edge", edgeID, "err", err)
	}
	h.respondFriendAction(w, r, human, "friend request approved · endpoint vended")
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
	h.respondFriendAction(w, r, human, "friend request declined")
}

// RevokeFriend revokes an active link (POST /friends/{id}/revoke) → kills the vended endpoint, one
// direction only. Requires human + CSRF. Governing: SPEC-0010 REQ "Per-Direction, Revocable,
// Non-Transitive Edges", ADR-0008 (revoke is instant and total).
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
	h.respondFriendAction(w, r, human, "friendship revoked · endpoint killed")
}

// WithdrawFriend withdraws an outgoing pending request (POST /friends/{id}/withdraw) → removes the
// entry. Requires human + CSRF. Governing: SPEC-0013 Endpoints table (Withdraw outgoing request).
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
	h.respondFriendAction(w, r, human, "request withdrawn")
}

// UnblockFriend removes a blocked (declined/revoked) entry from the ledger (POST
// /friends/{id}/unblock). Requires human + CSRF. Governing: SPEC-0013 Endpoints table (Remove a
// blocked entry).
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
	h.respondFriendAction(w, r, human, "entry removed")
}

// failFriendAction maps a friend-edge store transition error onto a generic HTTP status with no
// internal detail (SPEC-0013 REQ "Error Handling Standards"): not-found → 404, invalid transition →
// 409, scope-exceeds-request → 400, anything else → a logged 500.
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

// respondFriendAction re-renders the Friends panel (in the operator's current layout/filter, read
// from the POST) so an action that moves a card between groups reflects immediately, and prepends a
// toast. A non-HTMX submit redirects back to the view. Requires the human to rebuild their scoped
// listing. Governing: SPEC-0013 REQ "Friends View", REQ "Live Updates and Toasts".
func (h *Handler) respondFriendAction(w http.ResponseWriter, r *http.Request, human store.Human, toast string) {
	if !isHTMX(r) {
		// Post-action redirects target a fixed same-origin path (no user-supplied target).
		http.Redirect(w, r, "/friends", http.StatusSeeOther)
		return
	}
	layout := normalizeFriendLayout(r.FormValue("layout"))
	filter := normalizeFriendFilter(r.FormValue("filter"))
	edges, err := h.store.ListFriendEdges(r.Context(), human.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	agents, err := h.store.ListAgents(r.Context(), human.ID)
	if err != nil {
		h.log.Warn("friend action agents", "err", err)
	}
	panel := h.friendPanelView(friendCardsFromEdges(edges), agents, layout, filter, auth.CSRFFromContext(r.Context()))
	frag, err := h.renderFragment("friends_panel", panel)
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
	// Refresh the rail badge + filter-pill counts OOB alongside the panel swap.
	if cf, err := h.renderFragment("friend_counts", friendCountsView{Counts: panel.Counts, OOB: true}); err == nil {
		frag += cf
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}
