// Package web is the human-facing UI (ADR-0001: html/template, themed with the switchboard palette):
// log in, register agents, and vend/revoke scoped MCP endpoints. Handlers marked "requires human"
// read the authenticated principal from context (the server wraps them in auth.RequireHuman).
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

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
}

// New parses the templates and returns a Handler.
func New(st *store.Store, cfg config.Config, log *slog.Logger) (*Handler, error) {
	h := &Handler{store: st, cfg: cfg, log: log, pages: map[string]*template.Template{}}
	for _, p := range []string{"login", "dashboard", "agent", "vended"} {
		t, err := template.ParseFS(tmplFS, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		h.pages[p] = t
	}
	return h, nil
}

type view struct {
	Title          string
	Human          *store.Human
	OIDCConfigured bool
	DevLogin       bool
	Agents         []store.Agent
	Agent          *store.Agent
	Endpoints      []store.Endpoint
	Endpoint       *store.Endpoint
	Token          string
	MCPJSON        string
}

// Login renders the public login page.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login", view{Title: "Log in", OIDCConfigured: h.cfg.OIDCConfigured(), DevLogin: h.cfg.DevLogin})
}

// Dashboard lists the human's agents. Requires human.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	agents, err := h.store.ListAgents(r.Context(), human.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.render(w, "dashboard", view{Title: "Agents", Human: &human, Agents: agents})
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
	h.render(w, "agent", view{Title: ag.Name, Human: &human, Agent: &ag, Endpoints: eps})
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
	token, hash, prefix := cred.Mint()
	ep, err := h.store.CreateEndpoint(r.Context(), ag.ID, hash, prefix, queues, verbs)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.render(w, "vended", view{
		Title: "Vended", Human: &human, Agent: &ag, Endpoint: &ep,
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
	http.Redirect(w, r, r.Header.Get("Referer"), http.StatusSeeOther)
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

func buildMCPJSON(baseURL, token string) string {
	m := map[string]any{"mcpServers": map[string]any{"switchboard": map[string]any{
		"command": "switchboard",
		"args":    []string{"channel"},
		"env":     map[string]string{"SWITCHBOARD_URL": baseURL, "SWITCHBOARD_TOKEN": token},
	}}}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}
