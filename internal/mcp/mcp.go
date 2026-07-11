// Package mcp serves every vended endpoint over the MCP Streamable HTTP transport at
// /mcp/{endpoint}, using the official Go MCP SDK (ADR-0017, SPEC-0014).
//
// A chi middleware authenticates the bearer credential BEFORE the SDK sees the request: the token
// is hashed (internal/cred), resolved to an ACTIVE endpoint row, checked against the {endpoint}
// path slug, and the endpoint's immutable scope is injected into the request context for the tool
// layer to enforce. Unknown/revoked credentials get 401; a valid credential presented against a
// different endpoint's path gets 403. The URL alone grants nothing, and session cookies carry no
// authority here — only the Authorization header is ever consulted.
//
// Each session also advertises the experimental Claude Code Channels capability and receives
// todo-ready doorbells (`notifications/claude/channel`) on its notification stream — see
// doorbell.go and session.go (SPEC-0011, SPEC-0014 REQ "Channels Push over the HTTP Stream").
package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

const (
	serverName    = "switchboard"
	serverVersion = "0.1.0"

	// maxBodyBytes caps MCP request bodies before JSON-RPC parsing (SPEC-0014 REQ body size limits).
	maxBodyBytes = 1 << 20 // 1 MiB

	// sessionIdleTimeout closes MCP sessions with no HTTP request in flight and none seen for this
	// long, bounding the session registry. An open notification stream counts as in flight, so it
	// keeps a session live; idle abandoned sessions expire.
	sessionIdleTimeout = 30 * time.Minute

	// instructions describe the doorbell-over-durable-queue contract to connecting harnesses.
	// Governing: SPEC-0011 REQ "Channel Capability on the Vended Session".
	instructions = `Todos routed to you arrive as <channel source="switchboard"> doorbell events ` +
		`(notifications/claude/channel) on this session's notification stream. The durable todo ` +
		`queue is the record: a doorbell is only a hint, and a missed push is never a lost todo. ` +
		`Use list_todos to see work, claim to take a todo (which sets a lease), then complete or fail it.`
)

// EndpointStore is the slice of the store the auth middleware needs: credential resolution and the
// last-seen stamp. *store.Store satisfies it; tests substitute a fake.
type EndpointStore interface {
	EndpointByCredHash(ctx context.Context, credHash string) (store.AuthEndpoint, error)
	TouchEndpoint(ctx context.Context, endpointID string) error
}

// ToolStore is the full store slice the MCP surface consumes: endpoint auth, the SPEC-0006 drain
// verbs the tool layer wraps (lifecycle semantics live in internal/store per SPEC-0003), and the
// SPEC-0005 event-history reads (events.go).
type ToolStore interface {
	EndpointStore
	ListTodos(ctx context.Context, queues []string, state string, limit int) ([]store.Todo, error)
	GetTodo(ctx context.Context, id string) (store.Todo, error)
	ClaimTodo(ctx context.Context, id, owner string, ttl time.Duration) (store.Todo, error)
	HeartbeatTodo(ctx context.Context, id, owner string, ttl time.Duration) (store.Todo, error)
	CompleteTodo(ctx context.Context, id, owner string, result []byte) (store.Todo, error)
	FailTodo(ctx context.Context, id, owner string, result []byte) (store.Todo, error)
	ListEventHistory(ctx context.Context, f store.EventHistoryFilter) ([]store.EventHistoryItem, error)
	EventHistoryByID(ctx context.Context, id int64) (store.EventHistoryDetail, error)
}

// Handler mounts the per-endpoint Streamable HTTP MCP sessions, their scope-filtered tool
// registry, and the doorbell fan-out.
type Handler struct {
	store ToolStore
	log   *slog.Logger

	// preRL bounds unauthenticated volumetric abuse per client IP before any store lookup; rl is
	// the per-endpoint budget, keyed by endpoint id AFTER authentication so an unauthenticated
	// caller can neither drain a real endpoint's bucket nor grow the map with slug spam.
	preRL *rateLimiter
	rl    *rateLimiter

	// providers is the configured-provider snapshot served by list_providers (SPEC-0005),
	// installed at wiring time via SetProviders. Atomic so live sessions read it race-free.
	providers atomic.Pointer[[]ProviderStatus]

	idleTimeout time.Duration

	// mu guards sessions and closed. sessions is the live Streamable HTTP session registry, keyed
	// by Mcp-Session-Id and bound to the endpoint that minted each session.
	mu       sync.Mutex
	sessions map[string]*mcpSession
	closed   bool

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

// New builds the MCP surface and starts its idle-session janitor. Callers own the lifecycle and
// must call Close on shutdown so sessions and goroutines are torn down (SPEC-0014 REQ
// "Concurrency Safety").
func New(st ToolStore, log *slog.Logger) *Handler {
	h := &Handler{
		store: st,
		log:   log,
		// Governing: SPEC-0014 REQ "Rate Limiting" — two tiers. The generous pre-auth per-IP
		// bucket bounds credential guessing and slug spam without letting an unauthenticated
		// caller starve a known endpoint; the per-endpoint bucket (same shape and budget as the
		// /agent surface) covers request POSTs and stream (re)establishment post-auth.
		preRL:       newRateLimiter(50, 100),
		rl:          newRateLimiter(20, 40),
		idleTimeout: sessionIdleTimeout,
		sessions:    map[string]*mcpSession{},
		done:        make(chan struct{}),
	}
	h.wg.Add(1)
	go h.janitor()
	return h
}

// Routes returns the router to mount at /mcp. All Streamable HTTP methods (POST requests,
// GET notification stream, DELETE session teardown) land on the same per-endpoint URL.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Route("/{endpoint}", func(r chi.Router) {
		r.Use(h.headers)
		r.Use(h.preRateLimit)
		r.Use(h.limitBody)
		r.Use(h.auth)
		r.Use(h.rateLimit)
		r.Handle("/", http.HandlerFunc(h.serve))
	})
	return r
}

// headers enforces the MCP-surface response headers: these responses are JSON/SSE carrying scoped
// work data, never HTML, and must not be cached. The SDK sets its own Cache-Control when it opens
// an SSE response, so the values are pinned at write time via a wrapping ResponseWriter rather than
// set once up front. Governing: SPEC-0014 REQ "Security Headers".
func (h *Handler) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&headerWriter{ResponseWriter: w}, r)
	})
}

// headerWriter pins the security headers immediately before the status line is written, winning
// over anything a downstream handler set. It forwards Flush (the SDK streams SSE through it).
type headerWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *headerWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *headerWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController pass-through (deadlines, flushing).
func (w *headerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// preRateLimit throttles unauthenticated traffic per client IP (host portion of RemoteAddr, the
// same key the /agent and webhook surfaces use). It runs before auth so bogus credentials and
// made-up slugs are bounded without ever touching the store or a real endpoint's budget.
func (h *Handler) preRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.preRL.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimit throttles per authenticated endpoint (keyed by endpoint id, which exists only after
// auth — so the per-endpoint bucket cannot be drained by unauthenticated slug spam). 429 +
// Retry-After when the bucket is empty.
func (h *Handler) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep, ok := EndpointFromContext(r.Context())
		if !ok { // cannot happen behind auth; defense in depth
			unauthorized(w)
			return
		}
		if !h.rl.allow(ep.ID) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody bounds request bodies at 1 MiB, answering 413 for declared-oversize requests up front.
// Bodies without a declared length (chunked encoding) are drained through http.MaxBytesReader
// BEFORE the SDK parses anything, so an oversized chunked body also gets 413 — not the 400 the SDK
// would answer once the reader trips mid-parse. MCP request bodies are complete JSON-RPC messages,
// so buffering ≤1 MiB here costs nothing. Governing: SPEC-0014 REQ "Request Body Size Limits".
func (h *Handler) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "error reading request body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		next.ServeHTTP(w, r)
	})
}

// auth is the boundary: bearer token → hash → active endpoint → path-slug match → scope in context.
// Governing: SPEC-0014 REQ "Bearer Authentication Bound to the Vended Endpoint".
func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "endpoint")
		tok := bearer(r)
		if tok == "" {
			unauthorized(w)
			return
		}
		ep, err := h.store.EndpointByCredHash(r.Context(), cred.Hash(tok))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Unknown or revoked — same answer either way, revealing nothing about the slug.
				unauthorized(w)
				return
			}
			h.log.Error("mcp auth", "slug", slug, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if ep.Slug != slug {
			// Valid credential, wrong endpoint path: refuse without executing anything.
			h.log.Warn("mcp credential/path mismatch", "path_slug", slug, "credential_slug", ep.Slug)
			http.Error(w, "credential does not match this endpoint", http.StatusForbidden)
			return
		}
		// Stamp last-seen; failure to stamp is logged but never blocks an authenticated call.
		if err := h.store.TouchEndpoint(r.Context(), ep.ID); err != nil {
			h.log.Warn("mcp touch endpoint", "slug", slug, "err", err)
		}
		next.ServeHTTP(w, r.WithContext(withEndpoint(r.Context(), ep)))
	})
}

// newServer builds the per-session MCP server for the authenticated endpoint. The SDK owns
// protocol-version negotiation and session mechanics. The SPEC-0006 verb registry is filtered by
// the endpoint's immutable verb allowlist, so tools/list advertises exactly the endpoint's scope;
// the scopeGuard middleware turns a tools/call of a known verb outside that allowlist into a
// stable scope error before any handler or store code runs. Alongside tools, every session
// advertises the experimental Claude Code Channels capability and instructions describing the
// doorbell-over-durable-queue contract.
// Governing: SPEC-0014 REQ "Agent Tool Surface over MCP", SPEC-0011 REQ "Channel Capability on
// the Vended Session".
func (h *Handler) newServer(ep store.AuthEndpoint) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, &sdk.ServerOptions{
		Logger:       h.log,
		Instructions: instructions,
		Capabilities: &sdk.ServerCapabilities{
			Tools:        &sdk.ToolCapabilities{ListChanged: true},
			Experimental: map[string]any{"claude/channel": map[string]any{}},
		},
	})
	h.registerTools(srv, ep)
	// The SPEC-0005 event-history contract (list/get/replay/providers) shares the session and the
	// same allowlist-filtered registration (events.go).
	h.registerEventTools(srv, ep)
	srv.AddReceivingMiddleware(h.scopeGuard(ep))
	return srv
}

// --- context plumbing ---

type ctxKey int

const epKey ctxKey = 0

func withEndpoint(ctx context.Context, ep store.AuthEndpoint) context.Context {
	return context.WithValue(ctx, epKey, ep)
}

// EndpointFromContext returns the authenticated endpoint (identity + immutable scope) injected by
// the auth middleware. The tool layer reads it to filter tools/list and enforce scope on tools/call.
func EndpointFromContext(ctx context.Context) (store.AuthEndpoint, bool) {
	ep, ok := ctx.Value(epKey).(store.AuthEndpoint)
	return ep, ok
}

// --- helpers ---

// bearer extracts the Authorization bearer token; cookies are deliberately never consulted.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// clientIP is the pre-auth throttle key: the host portion of RemoteAddr.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard"`)
	http.Error(w, "invalid or revoked credential", http.StatusUnauthorized)
}
