// Package mcp serves every vended endpoint over the MCP Streamable HTTP transport at
// /mcp/{endpoint}, using the official Go MCP SDK (ADR-0017, SPEC-0014).
//
// A chi middleware authenticates the bearer credential BEFORE the SDK sees the request: the token
// is hashed (internal/cred), resolved to an ACTIVE endpoint row, checked against the {endpoint}
// path slug, and the endpoint's immutable scope is injected into the request context for the tool
// layer to enforce. Unknown/revoked credentials get 401; a valid credential presented against a
// different endpoint's path gets 403. The URL alone grants nothing, and session cookies carry no
// authority here — only the Authorization header is ever consulted.
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

	// sessionIdleTimeout closes MCP sessions that receive no HTTP requests, bounding the session
	// registry. An open notification stream keeps a session live; idle abandoned sessions expire.
	sessionIdleTimeout = 30 * time.Minute
)

// EndpointStore is the slice of the store the MCP surface needs: credential resolution and the
// last-seen stamp. *store.Store satisfies it; tests substitute a fake.
type EndpointStore interface {
	EndpointByCredHash(ctx context.Context, credHash string) (store.AuthEndpoint, error)
	TouchEndpoint(ctx context.Context, endpointID string) error
}

// Handler mounts the per-endpoint Streamable HTTP MCP servers.
type Handler struct {
	store EndpointStore
	log   *slog.Logger
	rl    *rateLimiter

	// mu guards handlers: one SDK Streamable HTTP handler per endpoint slug, created lazily.
	// Per-slug handlers keep session registries isolated, so a session id minted on one
	// endpoint can never be replayed against another endpoint's path.
	mu       sync.Mutex
	handlers map[string]*sdk.StreamableHTTPHandler
}

// New builds the MCP surface.
func New(st EndpointStore, log *slog.Logger) *Handler {
	return &Handler{
		store: st,
		log:   log,
		// Governing: SPEC-0014 REQ "Rate Limiting" — per-endpoint token bucket (same shape and
		// budget as the /agent surface) covering request POSTs and stream (re)establishment.
		rl:       newRateLimiter(20, 40),
		handlers: map[string]*sdk.StreamableHTTPHandler{},
	}
}

// Routes returns the router to mount at /mcp. All Streamable HTTP methods (POST requests,
// GET notification stream, DELETE session teardown) land on the same per-endpoint URL.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Route("/{endpoint}", func(r chi.Router) {
		r.Use(h.headers)
		r.Use(h.rateLimit)
		r.Use(h.limitBody)
		r.Use(h.auth)
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

// rateLimit throttles per endpoint (keyed by path slug, so it also bounds credential guessing
// against a single endpoint before auth runs). 429 + Retry-After when the bucket is empty.
func (h *Handler) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.rl.allow(chi.URLParam(r, "endpoint")) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody bounds request bodies at 1 MiB, answering 413 for declared-oversize requests up front
// and capping chunked bodies via http.MaxBytesReader. Governing: SPEC-0014 REQ body size limits.
func (h *Handler) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
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

// serve dispatches to the endpoint's SDK Streamable HTTP handler, creating it on first use.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	ep, ok := EndpointFromContext(r.Context())
	if !ok { // cannot happen behind auth; defense in depth
		unauthorized(w)
		return
	}
	h.mu.Lock()
	sh := h.handlers[ep.Slug]
	if sh == nil {
		sh = sdk.NewStreamableHTTPHandler(h.newServer, &sdk.StreamableHTTPOptions{
			Logger:         h.log,
			SessionTimeout: sessionIdleTimeout,
		})
		h.handlers[ep.Slug] = sh
	}
	h.mu.Unlock()
	sh.ServeHTTP(w, r)
}

// newServer builds the per-session MCP server. The SDK owns protocol-version negotiation and
// session mechanics. The tools capability is advertised now; the SPEC-0006 verb registry (filtered
// by the scope in the request context) is the follow-up story, so tools/list is empty until then.
func (h *Handler) newServer(r *http.Request) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: serverName, Version: serverVersion}, &sdk.ServerOptions{
		Logger:       h.log,
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{ListChanged: true}},
	})
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

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard"`)
	http.Error(w, "invalid or revoked credential", http.StatusUnauthorized)
}
