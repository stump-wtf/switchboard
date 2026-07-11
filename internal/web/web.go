// Package web is the human-facing UI (ADR-0016: the "Operator" design language over html/template):
// the Board landing view, agent registration, and vend/revoke of scoped MCP endpoints. Handlers
// marked "requires human" read the authenticated principal from context (the server wraps them in
// auth.RequireHuman).
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

// Handler serves the web UI.
type Handler struct {
	store *store.Store
	cfg   config.Config
	log   *slog.Logger
	pages map[string]*template.Template

	// SSE plumbing (SPEC-0012 "Live Updates via SSE"). sseRetryMS and keepAlive are fields so
	// tests can shrink intervals; production values come from New.
	events     *EventHub
	sseRetryMS func(ctx context.Context) int
	keepAlive  time.Duration
}

// New parses the templates and returns a Handler.
func New(st *store.Store, cfg config.Config, log *slog.Logger) (*Handler, error) {
	funcs := template.FuncMap{"reltime": relTime, "tag": providerTag}
	h := &Handler{store: st, cfg: cfg, log: log, pages: map[string]*template.Template{},
		events: newEventHub(), keepAlive: defaultKeepAlive}
	h.sseRetryMS = h.sseRetrySetting
	for _, p := range []string{"login", "board", "dashboard", "agent", "vended"} {
		t, err := template.New(p).Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("parse templates for %q: %w", p, err)
		}
		h.pages[p] = t
	}
	return h, nil
}

// shell carries the layout-shell state every authenticated view renders: the active rail entry,
// live counts, database connectivity, and the avatar initials.
// Governing: SPEC-0013 REQ "Information Architecture and Navigation".
type shell struct {
	Active      string // board | todos | endpoints — marks aria-current on the rail
	TodoCount   int    // pending todos, shown beside the Todos rail entry
	LiveRate    int    // events/min for the LIVE pill (hidden when zero)
	DBConnected bool   // pool ping result — the rail footer indicator
	Initials    string // avatar initials
}

type view struct {
	Title          string
	Human          *store.Human
	CSRF           string
	Shell          shell
	OIDCConfigured bool
	DevLogin       bool
	Stats          store.BoardStats
	Events         []store.EventSummary
	Agents         []store.Agent
	Agent          *store.Agent
	Endpoints      []store.Endpoint
	Endpoint       *store.Endpoint
	Token          string
	MCPJSON        string
}

// buildShell computes the layout-shell state. Store errors are logged and rendered as the
// degraded shell (zero counts, disconnected indicator) rather than failing the page — the shell's
// job is precisely to show that degradation (SPEC-0013 "Database connectivity is reflected").
func (h *Handler) buildShell(ctx context.Context, active string, human *store.Human) (shell, store.BoardStats) {
	sh := shell{Active: active, Initials: initials(human)}
	if err := h.store.Ping(ctx); err != nil {
		h.log.Warn("shell db ping", "err", err)
		return sh, store.BoardStats{}
	}
	sh.DBConnected = true
	stats, err := h.store.BoardStats(ctx)
	if err != nil {
		h.log.Warn("shell board stats", "err", err)
		return sh, store.BoardStats{}
	}
	sh.TodoCount = stats.AwaitingClaim
	sh.LiveRate = stats.EventsPerMin
	return sh, stats
}

// Login renders the public login page.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login", view{Title: "Log in", OIDCConfigured: h.cfg.OIDCConfigured(), DevLogin: h.cfg.DevLogin})
}

// Board renders the landing view: trust legend, stat tiles, and the recent-events feed, all
// server-rendered from the database (SSE live updates are a separate story). Requires human.
// Governing: SPEC-0013 REQ "Board View — Live Incoming Lines" (static slice).
func (h *Handler) Board(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	sh, stats := h.buildShell(r.Context(), "board", &human)
	var events []store.EventSummary
	if sh.DBConnected {
		var err error
		if events, err = h.store.RecentEvents(r.Context(), 12); err != nil {
			// Suppressed to a log so the Board still renders its shell (with whatever tiles
			// resolved); the feed shows its empty state.
			h.log.Warn("board recent events", "err", err)
		}
	}
	h.render(w, "board", view{
		Title: "The Board", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, Stats: stats, Events: events,
	})
}

// Dashboard lists the human's agents (surfaced as "Endpoints" in the rail until the SPEC-0013
// Endpoints view lands). Requires human.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	agents, err := h.store.ListAgents(r.Context(), human.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	h.render(w, "dashboard", view{Title: "Endpoints", Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh, Agents: agents})
}

// CreateAgent registers an agent. Requires human.
func (h *Handler) CreateAgent(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	ag, err := h.store.CreateAgent(r.Context(), human.ID, name, strings.TrimSpace(r.FormValue("description")))
	if err != nil {
		h.fail(w, err)
		return
	}
	http.Redirect(w, r, "/agents/"+ag.ID, http.StatusSeeOther)
}

// Agent renders one agent + its endpoints + the vend form. Requires human.
func (h *Handler) Agent(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	ag, err := h.store.GetAgentOwned(r.Context(), chi.URLParam(r, "id"), human.ID)
	if err != nil {
		h.notFoundOr(w, err)
		return
	}
	eps, err := h.store.ListEndpoints(r.Context(), ag.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	h.render(w, "agent", view{Title: ag.Name, Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh, Agent: &ag, Endpoints: eps})
}

// Vend mints a scoped endpoint credential and shows it once with wiring instructions. Requires human.
func (h *Handler) Vend(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	ag, err := h.store.GetAgentOwned(r.Context(), chi.URLParam(r, "id"), human.ID)
	if err != nil {
		h.notFoundOr(w, err)
		return
	}
	queues := splitCSV(r.FormValue("queues"))
	verbs := splitCSV(r.FormValue("verbs"))
	if len(queues) == 0 || len(verbs) == 0 {
		http.Error(w, "queues and verbs are required", http.StatusBadRequest)
		return
	}
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		h.fail(w, err)
		return
	}
	// Governing: SPEC-0014 REQ "Streamable HTTP MCP Endpoint" — the per-endpoint URL slug is
	// minted and persisted at vend time; /mcp/{slug} is where this credential is honored.
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	ep, err := h.store.CreateEndpoint(r.Context(), ag.ID, hash, prefix, slug, queues, verbs)
	if err != nil {
		h.fail(w, err)
		return
	}
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	h.render(w, "vended", view{
		Title: "Vended", Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh, Agent: &ag, Endpoint: &ep,
		Token: token, MCPJSON: buildMCPJSON(h.cfg.BaseURL, token),
	})
}

// Revoke revokes an endpoint. Requires human.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if err := h.store.RevokeEndpoint(r.Context(), chi.URLParam(r, "id"), human.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.fail(w, err)
		return
	}
	// Governing: SPEC-0007/0012 REQ open-redirect defense. Never redirect to a raw attacker-supplied
	// Referer; return to a same-origin in-app PATH only, defaulting to the dashboard.
	http.Redirect(w, r, h.safeRedirectTarget(r, "/"), http.StatusSeeOther)
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
	target := u.Path
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}

func (h *Handler) render(w http.ResponseWriter, page string, v view) {
	var buf bytes.Buffer
	if err := h.pages[page].ExecuteTemplate(&buf, "layout", v); err != nil {
		h.log.Error("render", "page", page, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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

// providerTags maps known source names to their design-doc two-letter chips
// (GH/ST/SL/DH/HL/RD per docs/design/03-components.md).
var providerTags = map[string]string{
	"github": "GH", "stripe": "ST", "slack": "SL", "dockerhub": "DH",
	"healthchecks": "HL", "redis": "RD",
}

// providerTag renders the two-letter provider chip for a source name (github → GH); unknown
// sources fall back to their first two letters upper-cased.
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

func buildMCPJSON(baseURL, token string) string {
	m := map[string]any{"mcpServers": map[string]any{"switchboard": map[string]any{
		"command": "switchboard",
		"args":    []string{"channel"},
		"env":     map[string]string{"SWITCHBOARD_URL": baseURL, "SWITCHBOARD_TOKEN": token},
	}}}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}
