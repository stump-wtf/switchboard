// Package agentapi is the vended-endpoint surface an agent calls with its credential (ADR-008).
//
// Every request is authenticated by the bearer credential (resolved to an endpoint + its immutable
// scope) and enforced at the boundary: a verb outside scope.verbs or a queue outside scope.queues is
// forbidden. It exposes the todo work verbs (list/claim/complete/fail) as JSON, plus an SSE stream of
// newly-created todos in the endpoint's queues — the signal the Channels stdio adapter turns into
// `notifications/claude/channel` pushes (ADR-013). The durable queue remains the ledger; the stream
// is a lossy doorbell.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// Hub fans newly-created todos out to subscribed agents, filtered by queue. Lossy by design.
type Hub struct {
	mu   sync.RWMutex
	subs map[int]*subscriber
	next int
}

type subscriber struct {
	queues map[string]bool
	ch     chan store.Todo
}

// NewHub builds an empty Hub.
func NewHub() *Hub { return &Hub{subs: map[int]*subscriber{}} }

// Subscribe returns a channel of todos in the given queues plus an unsubscribe func.
func (h *Hub) Subscribe(queues []string) (<-chan store.Todo, func()) {
	s := &subscriber{queues: map[string]bool{}, ch: make(chan store.Todo, 32)}
	for _, q := range queues {
		s.queues[q] = true
	}
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = s
	h.mu.Unlock()
	return s.ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(s.ch)
		}
		h.mu.Unlock()
	}
}

// Publish fans a todo out to subscribers whose scope includes its queue. Never blocks.
func (h *Hub) Publish(t store.Todo) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subs {
		if s.queues[t.Queue] {
			select {
			case s.ch <- t:
			default: // slow consumer: drop — the queue is the ledger, this is a doorbell
			}
		}
	}
}

// API serves the agent-facing surface.
type API struct {
	store *store.Store
	hub   *Hub
	log   *slog.Logger
}

// New builds an API.
func New(st *store.Store, hub *Hub, log *slog.Logger) *API {
	return &API{store: st, hub: hub, log: log}
}

type ctxKey int

const epKey ctxKey = 0

// Routes returns the agent API router (mount under /agent).
func (a *API) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(a.authMiddleware)
	r.Get("/whoami", a.whoami)
	r.Get("/todos", a.listTodos)
	r.Post("/todos/{id}/claim", a.claim)
	r.Post("/todos/{id}/complete", a.complete)
	r.Post("/todos/{id}/fail", a.failTodo)
	r.Get("/stream", a.stream)
	return r
}

func (a *API) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "missing bearer credential")
			return
		}
		ep, err := a.store.EndpointByCredHash(r.Context(), cred.Hash(tok))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "invalid or revoked credential")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), epKey, ep)))
	})
}

func endpoint(r *http.Request) store.AuthEndpoint {
	ep, _ := r.Context().Value(epKey).(store.AuthEndpoint)
	return ep
}

func (a *API) whoami(w http.ResponseWriter, r *http.Request) {
	ep := endpoint(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent": ep.AgentName, "agent_id": ep.AgentID, "owner_id": ep.OwnerHumanID,
		"queues": ep.ScopeQueues, "verbs": ep.ScopeVerbs,
	})
}

func (a *API) listTodos(w http.ResponseWriter, r *http.Request) {
	ep := endpoint(r)
	if !hasVerb(ep, "list_todos") {
		writeErr(w, http.StatusForbidden, "forbidden", "verb list_todos not in scope")
		return
	}
	queues := ep.ScopeQueues
	if q := r.URL.Query().Get("queue"); q != "" {
		if !inScope(ep.ScopeQueues, q) {
			writeErr(w, http.StatusForbidden, "forbidden", "queue not in scope")
			return
		}
		queues = []string{q}
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	todos, err := a.store.ListTodos(r.Context(), queues, r.URL.Query().Get("state"), limit)
	if err != nil {
		a.serverErr(w, err)
		return
	}
	out := make([]todoJSON, 0, len(todos))
	for _, t := range todos {
		out = append(out, toJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"todos": out})
}

func (a *API) claim(w http.ResponseWriter, r *http.Request) {
	a.transition(w, r, "claim", func(ctx context.Context, ep store.AuthEndpoint, id string, body map[string]any) (store.Todo, error) {
		ttl := time.Duration(atoiFloat(body["lease_ttl_seconds"], 300)) * time.Second
		return a.store.ClaimTodo(ctx, id, owner(ep), ttl)
	})
}

func (a *API) complete(w http.ResponseWriter, r *http.Request) {
	a.transition(w, r, "complete", func(ctx context.Context, ep store.AuthEndpoint, id string, body map[string]any) (store.Todo, error) {
		return a.store.CompleteTodo(ctx, id, owner(ep), rawJSON(body["result"]))
	})
}

func (a *API) failTodo(w http.ResponseWriter, r *http.Request) {
	a.transition(w, r, "fail", func(ctx context.Context, ep store.AuthEndpoint, id string, body map[string]any) (store.Todo, error) {
		return a.store.FailTodo(ctx, id, owner(ep), rawJSON(body["result"]))
	})
}

// transition wraps the verb-check, queue-scope-check, body-decode, and error-mapping shared by
// claim/complete/fail.
func (a *API) transition(w http.ResponseWriter, r *http.Request, verb string,
	do func(context.Context, store.AuthEndpoint, string, map[string]any) (store.Todo, error)) {
	ep := endpoint(r)
	if !hasVerb(ep, verb) {
		writeErr(w, http.StatusForbidden, "forbidden", "verb "+verb+" not in scope")
		return
	}
	id := chi.URLParam(r, "id")
	td, err := a.store.GetTodo(r.Context(), id)
	if err != nil {
		a.mapErr(w, err)
		return
	}
	if !inScope(ep.ScopeQueues, td.Queue) {
		writeErr(w, http.StatusForbidden, "forbidden", "todo's queue not in scope")
		return
	}
	body := decodeBody(r)
	updated, err := do(r.Context(), ep, id, body)
	if err != nil {
		a.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(updated))
}

// stream is an SSE feed of new todos in the endpoint's scope queues (the Channels doorbell).
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ep := endpoint(r)
	ch, unsub := a.hub.Subscribe(ep.ScopeQueues)
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("retry: 3000\n\n"))
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = w.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		case t, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(toJSON(t))
			_, _ = w.Write([]byte("event: todo\ndata: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

// --- helpers ---

type todoJSON struct {
	ID        string          `json:"id"`
	Queue     string          `json:"queue"`
	Source    string          `json:"source,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	Owner     string          `json:"owner,omitempty"`
	Assignee  string          `json:"assignee,omitempty"`
	Attempt   int             `json:"attempt"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func toJSON(t store.Todo) todoJSON {
	return todoJSON{
		ID: t.ID, Queue: t.Queue, Source: t.Source, Kind: t.Kind, Title: t.Title, State: t.State,
		Owner: t.Owner, Assignee: t.Assignee, Attempt: t.Attempt, CreatedAt: t.CreatedAt,
		Payload: json.RawMessage(t.Payload),
	}
}

func owner(ep store.AuthEndpoint) string { return "agent:" + ep.AgentID }

func hasVerb(ep store.AuthEndpoint, verb string) bool { return inScope(ep.ScopeVerbs, verb) }

func inScope(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (a *API) mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "todo not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", "todo not in the expected state")
	default:
		a.serverErr(w, err)
	}
}

func (a *API) serverErr(w http.ResponseWriter, err error) {
	a.log.Error("agentapi", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func decodeBody(r *http.Request) map[string]any {
	m := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&m)
	}
	return m
}

func rawJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func atoiFloat(v any, def int) int {
	if f, ok := v.(float64); ok && f > 0 {
		return int(f)
	}
	return def
}
