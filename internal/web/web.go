// Package web is the human-facing UI (ADR-0018: the charm-web design language over html/template):
// the Board landing view, agent registration, and vend/revoke of scoped MCP endpoints. Handlers
// marked "requires human" read the authenticated principal from context (the server wraps them in
// auth.RequireHuman).
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	switchboard "github.com/joestump/switchboard"
	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/store"
)

//go:embed templates/*.html templates/fragments/*.html
var tmplFS embed.FS

// pageNames are the page templates composed with layout.html and the per-view fragment files.
// Startup parses every one of them.
// Governing: SPEC-0012 REQ "Server-Rendered Pages from Embedded Templates", SPEC-0015 REQ
// "Application Shell And Navigation" (providers joins the IA).
var pageNames = []string{"home", "login", "board", "todos", "todo", "endpoints", "vend", "quickvend", "revoke", "personas", "personawiz", "friends", "friend_approve", "friend_revoke", "providers", "authorize", "connect"}

// operatorLeaseTTL is the visibility lease granted when the operator claims from the Board —
// the same default agents get (internal/mcp defaultLeaseTTL). Governing: SPEC-0003 lease.
const operatorLeaseTTL = 5 * time.Minute

// Handler serves the web UI.
type Handler struct {
	store *store.Store
	cfg   config.Config
	log   *slog.Logger
	pages map[string]*template.Template
	frags *template.Template // per-view live fragments (templates/fragments/*.html), standalone-renderable

	// personasEnabled gates the Personas view AND the persona chip on endpoint cards + the persona
	// select on the vend wizard's persona step. The server sets it by feature detection (personas
	// store + well-known Agent Card route both wired); while false the Personas rail entry is
	// hidden, every /personas route 404s, and the Endpoints surface renders no persona slot.
	// Governing: SPEC-0013 REQ "Personas View" (capability-gated), SPEC-0015 REQ "Endpoints View
	// And Vend Wizard".
	personasEnabled bool

	// SSE plumbing (SPEC-0012 "Live Updates via SSE"). sseRetryMS and keepAlive are fields so
	// tests can shrink intervals; production values come from New.
	events     *EventHub
	sseRetryMS func(ctx context.Context) int
	keepAlive  time.Duration

	// Ordered live-publish queue (SPEC-0013 typed events; live.go). Lazily started on first
	// publish so template-only construction never spins a worker.
	liveOnce sync.Once
	liveCh   chan func(context.Context)

	// wizards holds the server-side step state for every full-page wizard (SPEC-0015 REQ "Wizard
	// Interaction Pattern"; wizard.go). One table serves all wizards — entries are keyed by opaque
	// per-flow cookie tokens.
	wizards *wizardStates

	// endpointRevoked, when set, observes successful endpoint revocations (endpoint id). The
	// server wires it to the MCP mount so revoking an endpoint also closes its live notification
	// streams promptly, not just future requests. Governing: SPEC-0014 REQ "Concurrency Safety"
	// scenario "Revocation closes live streams", ADR-0008 (revoke is instant and total).
	endpointRevoked func(endpointID string)
}

// SetEndpointRevokedHook registers fn to observe successful endpoint revocations. Wire it before
// the handler serves traffic; passing nil clears the hook.
func (h *Handler) SetEndpointRevokedHook(fn func(endpointID string)) { h.endpointRevoked = fn }

// New parses the templates and returns a Handler. A parse failure is returned to the caller, so
// server startup fails loudly instead of serving broken pages (SPEC-0012 "fail startup if any
// template fails to parse").
func New(st *store.Store, cfg config.Config, log *slog.Logger) (*Handler, error) {
	pages, err := parsePages(tmplFS)
	if err != nil {
		return nil, err
	}
	frags, err := parseFrags(tmplFS)
	if err != nil {
		return nil, err
	}
	h := &Handler{store: st, cfg: cfg, log: log, pages: pages, frags: frags,
		events: newEventHub(), keepAlive: defaultKeepAlive,
		wizards: newWizardStates(wizardTTL)}
	h.sseRetryMS = h.sseRetrySetting
	return h, nil
}

// templateFuncs is the shared FuncMap wired into every page set and the standalone fragments.
func templateFuncs() template.FuncMap {
	return template.FuncMap{"reltime": relTime, "tag": providerTag, "iconpath": providerIconPath, "eventiconpath": eventIconPath, "dict": dict, "lanestate": laneStateLabel, "join": joinScope, "countdown": countdown, "trustdef": trustDef}
}

// parsePages composes layout.html and the per-view fragment files (templates/fragments/*.html)
// with each page template from fsys (pages reuse the shared feed rows, tiles, and pills). The
// fragments are one file per view so a todos fragment change never touches a board or friends
// template file (SPEC-0015 REQ "Live Fragment Architecture"). Split from New so tests can prove
// that a broken or missing template surfaces a startup-failing error.
// Governing: SPEC-0012 REQ "Server-Rendered Pages from Embedded Templates", ADR-0018.
func parsePages(fsys fs.FS) (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, len(pageNames))
	for _, p := range pageNames {
		t, err := template.New(p).Funcs(templateFuncs()).ParseFS(fsys,
			"templates/layout.html", "templates/fragments/*.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("parse templates for %q: %w", p, err)
		}
		pages[p] = t
	}
	return pages, nil
}

// parseFrags parses the per-view fragment files once standalone for the SSE publisher (live.go
// renders fragments with no page around them). Like parsePages, a parse failure fails startup.
func parseFrags(fsys fs.FS) (*template.Template, error) {
	frags, err := template.New("fragments").Funcs(templateFuncs()).ParseFS(fsys, "templates/fragments/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse fragment templates: %w", err)
	}
	return frags, nil
}

// dict builds a map for passing multiple named args to a sub-template ({{template "x" dict "K" v}}).
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, errors.New("dict: arguments must be key/value pairs")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}

// shell carries the layout-shell state every authenticated view renders: the active nav entry,
// live counts, database connectivity, and the avatar initials.
// Governing: SPEC-0015 REQ "Application Shell And Navigation" (six-view IA).
type shell struct {
	Active          string // board | todos | endpoints | personas | friends | providers — marks aria-current on the nav
	TodoCount       int    // total todos (every state), shown beside the Todos rail entry (design record, #179)
	LiveRate        int    // events/min for the LIVE pill (hidden when zero)
	DBConnected     bool   // pool ping result — the rail footer indicator
	Initials        string // avatar initials
	PersonasEnabled bool   // render the Personas rail entry only when the capability is enabled
	FriendsEnabled  bool   // friending capability on — reveal the Friends rail entry (SPEC-0013)
	FriendsIncoming int    // pending incoming friend requests — the rail badge (shown when nonzero)
}

type view struct {
	Title          string
	Landing        bool // public marketing landing (GET / when logged out): render the full-bleed home template, not the app shell
	Human          *store.Human
	CSRF           string
	Shell          shell
	OIDCConfigured bool
	DevLogin       bool
	Tiles          tilesView        // Board stat band (stats + activity bars)
	Lanes          lanesView        // Board three-lane patch panel (SPEC-0015)
	Counts         store.TodoCounts // Todos view filter-pill counts
	TodoItems      []todoRow        // Todos view table rows
	Filter         string           // active Todos filter pill (all|pending|claimed|done|failed)
	Query          string           // Todos search text
	Drawer         *drawerView      // standalone todo detail page (drawer fallback)

	// Endpoints view + vend wizard (SPEC-0015 REQ "Endpoints View And Vend Wizard").
	EndpointCards   []endpointCard     // the vended-endpoint cards
	PersonasEnabled bool               // gates the persona chip on cards + the wizard's persona step select
	Reveal          *revealView        // set on a successful vend to render the one-time credential reveal inline
	Vend            *vendStepView      // the active vend-wizard step page (templates/vend.html)
	Quick           *quickVendView     // the one-step quick-vend page (templates/quickvend.html; ADR-0023)
	RevokeConfirm   *revokeConfirmView // the revoke confirm page (templates/revoke.html)

	// Personas view + wizard (SPEC-0015 REQ "Personas View And Wizard"): cards, and the active
	// create/edit wizard step page (templates/personawiz.html) with its live A2A card preview.
	Personas   *personasView
	PersonaWiz *personaWizStepView

	// OAuth consent screen (SPEC-0016 REQ "Authorization Code Flow With Consent"): the
	// "authorize access" surface (templates/authorize.html) or its dead-end error state.
	Authorize *authorizeView

	// Friends view + approval flow (SPEC-0015 REQ "Friends View And Approval Flow").
	FriendGroups []friendGroup // ordered sections: pending-in-your-queue / awaiting-them / established / blocked
	FriendCounts friendCounts  // per-group counts (the rail badge reads Incoming)
	// FriendVended renders the post-approval vended result inline on the Friends page — the minted
	// endpoint's identity and scope, surfaced explicitly (never only a toast).
	FriendVended *friendVendedView
	// FriendApprove feeds the approve confirm page (approving IS the vend; templates/friend_approve.html).
	FriendApprove *friendApproveView
	// FriendRevoke feeds the friend revoke confirm page (templates/friend_revoke.html).
	FriendRevoke *friendRevokeView
}

// buildShell computes the layout-shell state. Store errors are logged and rendered as the
// degraded shell (zero counts, disconnected indicator) rather than failing the page — the shell's
// job is precisely to show that degradation (SPEC-0013 "Database connectivity is reflected").
func (h *Handler) buildShell(ctx context.Context, active string, human *store.Human) (shell, store.BoardStats) {
	sh := shell{Active: active, Initials: initials(human), PersonasEnabled: h.personasEnabled}
	if err := h.store.Ping(ctx); err != nil {
		h.log.Warn("shell db ping", "err", err)
		return sh, store.BoardStats{}
	}
	sh.DBConnected = true
	// Friends rail entry + pending-incoming badge, only when the capability is enabled (SPEC-0013:
	// hidden-not-broken). The shell derives from the single friendsEnabled seam (friends.go) — the
	// same check the /friends routes gate on — so the rail and the routes can never disagree.
	// A count-read failure degrades to a hidden badge, never a failed page.
	if h.friendsEnabled() {
		sh.FriendsEnabled = true
		if edges, err := h.store.ListFriendEdges(ctx, human.ID, "pending"); err != nil {
			h.log.Warn("shell friend requests", "err", err)
		} else {
			// Count INCOMING pendings only (requests awaiting THIS human's decision); a locally sent
			// direction=outgoing pending awaits the remote operator and must not nag here (#174).
			for _, e := range edges {
				if e.Direction != friendDirectionOutgoing {
					sh.FriendsIncoming++
				}
			}
		}
	}
	stats, err := h.store.BoardStats(ctx, human.ID)
	if err != nil {
		h.log.Warn("shell board stats", "err", err)
		return sh, store.BoardStats{}
	}
	// The rail badge is the SIZE of the durable queue (all states, design record) — the same number
	// the counts SSE frame swaps in ({{template "counts"}} binds .Todos.All), so page render and
	// live update never disagree.
	sh.TodoCount = stats.TotalTodos
	sh.LiveRate = stats.EventsPerMin
	return sh, stats
}

// Root serves GET / for everyone: an authenticated operator gets the Board (the working surface),
// a logged-out visitor gets the public marketing Home page instead of a bare bounce to /login. The
// route is mounted under auth.LoadHuman (injects the human when a live session is present, never
// redirects), so the auth boundary is unchanged — every DATA route stays behind RequireHuman; only
// the pre-auth face of / becomes a landing page rather than a redirect.
// Governing: SPEC-0012 REQ "Screen Set and Routes" (GET / renders the Board for a human),
// SPEC-0015 REQ "Application Shell And Navigation".
func (h *Handler) Root(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); ok {
		h.Board(w, r)
		return
	}
	h.Home(w, r)
}

// Home renders the public marketing landing page (receive · verify · patch through) with the app's
// own charm-web design language and day/night theme. It reads nothing from the store — it is a
// static, unauthenticated surface — and renders the full-bleed home template (its own header/footer),
// not the operator shell. Governing: ADR-0018 (charm-web design language).
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	h.render(w, "home", view{Landing: true, Title: "switchboard — receive · verify · patch through"})
}

// Login renders the public login page.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login", view{Title: "Log in", OIDCConfigured: h.cfg.OIDCConfigured(), DevLogin: h.cfg.DevLogin})
}

// Board renders the landing view: trust legend, stat tiles (throughput + activity bars), and the
// three-lane patch panel, all server-rendered from the database — the same fragments the SSE
// stream then keeps live, so reload always renders authoritative state. The received lane renders
// EMPTY here by design: its cards are ephemeral in-flight deliveries (SSE-only, never persisted),
// so a quiet reload is truthful. Verified holds durable unclaimed todos; patched through holds
// claimed and beyond. Requires human. Governing: SPEC-0015 REQ "Patch Panel Board".
func (h *Handler) Board(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	sh, stats := h.buildShell(r.Context(), "board", &human)
	var lanes lanesView
	var bars []bar
	if sh.DBConnected {
		// One durable-queue read partitions into the two persisted lanes, newest first, capped per
		// lane. Errors are suppressed to a log so the Board still renders its shell; the lanes show
		// their empty states.
		items, err := h.store.ListTodoItems(r.Context(), human.ID, "", "", boardLaneQuery)
		if err != nil {
			h.log.Warn("board lane todos", "err", err)
		}
		for _, it := range items {
			card := h.laneCardFromItem(r.Context(), it)
			switch card.Lane {
			case laneVerified:
				if len(lanes.Verified) < laneCap {
					lanes.Verified = append(lanes.Verified, card)
				}
			case lanePatched:
				if len(lanes.Patched) < laneCap {
					lanes.Patched = append(lanes.Patched, card)
				}
			}
		}
		counts, err := h.store.TodoCounts(r.Context(), human.ID)
		if err != nil {
			h.log.Warn("board lane counts", "err", err)
		}
		lanes.Counts = laneCountsFrom(counts)
		buckets, err := h.store.EventBuckets(r.Context(), human.ID, activityBuckets)
		if err != nil {
			h.log.Warn("board event buckets", "err", err)
		}
		bars = activityBars(buckets)
	}
	h.render(w, "board", view{
		Title: "The Board", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, Tiles: tilesView{Stats: stats, Bars: bars}, Lanes: lanes,
	})
}

// boardLaneQuery is how many todos the Board reads to fill its two persisted lanes (each capped at
// laneCap after partitioning by state).
const boardLaneQuery = 60

// ClaimTodo claims a pending todo under a lease as the operator (the lane card's Claim action).
// The Board's Claim posts with hx-swap="none" and receives the OOB lane movement (verified →
// patched through) directly, so the card crosses even if the SSE todo_claimed frame is dropped;
// the SSE frame updates every other open view. Requires human (session + CSRF via the layout's
// hx-headers). Governing: SPEC-0015 REQ "Patch Panel Board", SPEC-0003 claim-under-lease semantics
// (the UI implements no lifecycle rules of its own).
func (h *Handler) ClaimTodo(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	t, err := h.store.ClaimTodoOperatorOwned(r.Context(), human.ID, chi.URLParam(r, "id"), "op:"+human.ID, operatorLeaseTTL)
	// respondTodoAction picks the fragment by HTMX target: the Board card's Claim (default) gets
	// the OOB lane movement, while the Todos table and drawer get their own refreshed fragments.
	// On a lost race the SSE stage update tells the operator who won; no internal detail leaks.
	h.respondTodoAction(w, r, "ClaimTodo", t, err)
}

// AgentsRedirect folds the retired SPEC-0012 dashboard/agent screens into the Endpoints view: the
// old /agents and /agents/{id} routes 303-redirect to /endpoints. It performs no store lookup, so it
// cannot leak whether an agent id exists — every request lands on the same authoritative view.
// Requires human. Governing: SPEC-0013 REQ "Endpoints View and Vend Modal" (routes fold into
// /endpoints).
func (h *Handler) AgentsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/endpoints", http.StatusSeeOther)
}

// Revoke revokes (kills) an endpoint the human owns. Revoke is instant and total (ADR-0008); the
// scope is immutable, so changing access means revoke + re-vend. The kill POST is reached only
// from the full-page confirm (RevokeConfirm, endpoints.go) per the SPEC-0015 wizard pattern.
// Requires human. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard", SPEC-0007 REQ
// "Instant, Total Revocation".
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	err := h.store.RevokeEndpoint(r.Context(), id, human.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.fail(w, err)
		return
	}
	if err == nil && h.endpointRevoked != nil {
		// Revocation committed: also tear down any live MCP notification stream immediately.
		h.endpointRevoked(id)
	}
	// Governing: SPEC-0007/0012 REQ open-redirect defense. Never redirect to a raw attacker-supplied
	// Referer; return to a same-origin in-app PATH only, defaulting to the Endpoints view.
	http.Redirect(w, r, h.safeRedirectTarget(r, "/endpoints"), http.StatusSeeOther)
}

// DeleteEndpoint permanently removes a revoked endpoint the human owns, clearing its dead card from
// the Endpoints view. The store constrains the delete to state='revoked' + ownership, so an active
// endpoint (which must be revoked first, to tear down its live sessions) or another human's endpoint
// resolves to not-found and is left untouched. ErrNotFound is swallowed so the action is idempotent —
// a double submit or an already-gone card lands back on the same authoritative view. No live-session
// teardown hook fires here: revocation already did that, and a revoked endpoint has no live sessions.
// Requires human. Governing: SPEC-0007 REQ "Permanent Deletion of Revoked Endpoints", SPEC-0013 REQ
// "Endpoints View and Vend Modal".
func (h *Handler) DeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteEndpoint(r.Context(), id, human.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.fail(w, err)
		return
	}
	// Governing: SPEC-0007/0012 REQ open-redirect defense. Same-origin in-app PATH only.
	http.Redirect(w, r, h.safeRedirectTarget(r, "/endpoints"), http.StatusSeeOther)
}

// safeRedirectTarget returns a same-origin, path-only redirect target derived from the request's
// Referer, or fallback when the Referer is absent, cross-origin, or not an in-app path. It strips
// any scheme/host so the response can never bounce a user to another origin (open redirect).
func (h *Handler) safeRedirectTarget(r *http.Request, fallback string) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return fallback
	}
	u, err := url.Parse(ref)
	if err != nil {
		return fallback
	}
	// If the Referer names a host, it must match our own origin (configured base URL or request host).
	if u.Host != "" {
		base, _ := url.Parse(h.cfg.BaseURL)
		if (base == nil || u.Host != base.Host) && u.Host != r.Host {
			return fallback
		}
	}
	if !strings.HasPrefix(u.Path, "/") {
		return fallback
	}
	// Never bounce back to the page that submitted this request. A destructive action's confirm
	// page (endpoint revoke, friend revoke, delete) is reachable only while the thing still exists,
	// so returning to it after the action succeeds lands on a 404 and reads as "the revoke failed"
	// when it in fact worked. Revoke hit this 100% of the time: the POST's Referer IS the confirm
	// page, and RevokeConfirm only matches an ACTIVE endpoint.
	if u.Path == r.URL.Path {
		return fallback
	}
	target := u.Path
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}

func (h *Handler) render(w http.ResponseWriter, page string, v view) {
	h.renderStatus(w, http.StatusOK, page, v)
}

// renderStatus renders a page with an explicit response status — wizard steps re-render themselves
// with 400 on a validation failure (SPEC-0015: a failed step is re-shown with the entered values,
// never a dead-end error page).
func (h *Handler) renderStatus(w http.ResponseWriter, status int, page string, v view) {
	var buf bytes.Buffer
	if err := h.pages[page].ExecuteTemplate(&buf, "layout", v); err != nil {
		h.log.Error("render", "page", page, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	h.log.Error("web handler", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (h *Handler) notFoundOr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.fail(w, err)
}

// joinScope renders a scope list (queues/verbs/intents) as a comma-separated string for display,
// returning "" for the empty list so templates can show a "none negotiated" fallback.
func joinScope(items []string) string {
	return strings.Join(items, ", ")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// initials derives the avatar initials from a human's display name (first letters of the first two
// words), falling back to the email's first letter, then to the operator glyph "OP".
func initials(human *store.Human) string {
	if human == nil {
		return "OP"
	}
	fields := strings.Fields(human.DisplayName)
	switch {
	case len(fields) >= 2:
		return upperFirst(fields[0]) + upperFirst(fields[1])
	case len(fields) == 1:
		return upperFirst(fields[0])
	case human.Email != "":
		return upperFirst(human.Email)
	default:
		return "OP"
	}
}

func upperFirst(s string) string {
	for _, r := range s {
		return string(unicode.ToUpper(r))
	}
	return ""
}

// countdown renders the compact time-remaining chip for an endpoint card's expiry ("5d", "3h",
// "12m", "<1m", "expired"). The server-rendered value is authoritative at page render; the card
// also stamps data-sb-expires-at so client hydration can tick it live without a reload.
// Governing: SPEC-0016 REQ "Credential Lifetime" (the endpoints view shows the countdown).
func countdown(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		// Round half-up per displayed unit so "vended for 30m" reads 30m, not 29m.
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Hour)/time.Hour))
	default:
		return fmt.Sprintf("%dd", int((d+12*time.Hour)/(24*time.Hour)))
	}
}

// relTime renders a compact relative age for feed rows ("just now", "5m ago", "3h ago", "2d ago").
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// trustDefs is the single source for the one-line definition every trust-mode pill carries as
// its tooltip (SPEC-0001 chip vocabulary). The Board legend, provider cards, the catalog, and the
// todos trust column all read their copy from this map so the definitions never drift.
// Governing: #84 (trust modes need a legend).
var trustDefs = map[string]string{
	"signed": "signature verified per delivery",
	"token":  "shared-secret token",
	"open":   "no verification — explicitly open",
	"queue":  "drained from a queue, trusted transport",
}

// trustDef renders the one-line definition for a trust-mode pill's title tooltip; an unknown mode
// falls back to a neutral gloss so the tooltip never renders empty.
func trustDef(mode string) string {
	if def, ok := trustDefs[strings.ToLower(mode)]; ok {
		return def
	}
	return "delivery trust mode"
}

// providerTags maps known source names to their design-doc two-letter chips
// (GH/ST/SL/DH/HL/RD per docs/design/03-components.md). Used as fallback when
// no SVG icon is available.
var providerTags = map[string]string{
	"github": "GH", "stripe": "ST", "slack": "SL", "dockerhub": "DH",
	"healthchecks": "HL", "redis": "RD", "gitea": "GT",
}

// eventIconSlugs maps an event kind onto the icon slug that represents it; several kinds share one
// glyph (every pull_request_* flavour is one PR icon).
var eventIconSlugs = map[string]string{
	"pull_request": "pull_request", "pull_request_review": "pull_request",
	"pull_request_review_comment": "pull_request",
	"issues":                      "issue", "issue_comment": "issue",
	"push":  "push",
	"merge": "merge", "merge_group": "merge",
	"workflow_run": "ci", "check_run": "ci", "check_suite": "ci",
	"release": "release",
	"ping":    "ping",
}

// iconPaths is the set of icon URL paths that actually exist in StaticFS, resolved ONCE at init.
// The alternative — probing the embedded FS per call — costs a full read-and-discard of the SVG
// bytes on every render, and these helpers run per todo row, per lane card and per catalog card on
// the hot live-fragment path (SPEC-0015 OOB swaps fire on every doorbell).
var iconPaths = buildIconPaths()

func buildIconPaths() map[string]string {
	paths := map[string]string{}
	for _, dir := range []string{"brands", "ui"} {
		entries, err := fs.ReadDir(switchboard.StaticFS, "static/icons/"+dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".svg") {
				continue
			}
			slug := strings.TrimSuffix(name, ".svg")
			paths[dir+"/"+slug] = "/static/icons/" + dir + "/" + name
		}
	}
	return paths
}

// providerIconPath returns the /static/ URL path for a provider's brand icon, or empty string if
// no icon exists. Templates render this as an <img> tag — no template.HTML escape bypass needed.
func providerIconPath(source string) string {
	slug := strings.ToLower(source)
	if slug == "dockerhub" {
		slug = "docker"
	}
	return iconPaths["brands/"+slug]
}

// eventIconPath returns the /static/ URL path for an event kind icon, or empty string if none.
// NOTE: static/icons/ui/ currently ships no SVGs, so this returns "" for every kind and the
// {{with eventiconpath …}} blocks render nothing — the templates degrade to the bare label.
func eventIconPath(kind string) string {
	slug, ok := eventIconSlugs[strings.ToLower(kind)]
	if !ok {
		return ""
	}
	return iconPaths["ui/"+slug]
}

// providerTag renders the two-letter provider chip for a source name (github → GH); unknown
// sources fall back to their first two letters upper-cased. Used as text fallback alongside
// providerIconSVG.
func providerTag(source string) string {
	if tag, ok := providerTags[strings.ToLower(source)]; ok {
		return tag
	}
	var tag []rune
	for _, r := range source {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			tag = append(tag, unicode.ToUpper(r))
			if len(tag) == 2 {
				break
			}
		}
	}
	if len(tag) == 0 {
		return "··"
	}
	return string(tag)
}

// buildMCPJSON renders the ready-to-paste .mcp.json wiring shown once on the vended page. The
// wiring is Streamable-HTTP only: an MCP client connects to https://<host>/mcp/<slug> and presents
// the minted credential as a bearer token. There is no local binary, PATH install, or stdio command
// — that transport is retired.
// Governing: SPEC-0014 REQ "HTTP Wiring Is the Only Wiring" (scenario "Vend reveal shows HTTP
// wiring"), SPEC-0012 REQ "Vend Flow and One-Time Credential Reveal".
func buildMCPJSON(baseURL, slug, token string) string {
	return mcp.ClientConfigJSON(baseURL, slug, token)
}

// buildMCPJSONURLOnly renders the URL-only .mcp.json variant the reveal offers for OAuth-capable
// clients: same Streamable-HTTP endpoint, no embedded credential — the client discovers the
// authorization server from the endpoint's RFC 9728 metadata and signs the human in through the
// OAuth flow instead of carrying a static bearer. Governing: SPEC-0015 REQ "Endpoints View And
// Vend Wizard", SPEC-0016. Both renderers live in internal/mcp so the operator API's vend response
// (ADR-0023) carries byte-identical wiring.
func buildMCPJSONURLOnly(baseURL, slug string) string {
	return mcp.ClientConfigJSONURLOnly(baseURL, slug)
}

// mcpEndpointURL is the minted endpoint's Streamable-HTTP URL — the same value the reveal shows as
// its standalone "MCP endpoint URL" field and embeds in the .mcp.json wiring (SPEC-0014).
func mcpEndpointURL(baseURL, slug string) string {
	return mcp.EndpointURL(baseURL, slug)
}
