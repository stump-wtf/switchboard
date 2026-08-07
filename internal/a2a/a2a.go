// Governing: ADR-0021 (A2A task-delegation transport), SPEC-0018 REQ "SendMessage Requires a Vended
// Endpoint", REQ "Error Handling Standards"
//
// This file is the A2A HTTP/JSON-RPC binding: a per-endpoint mount at /a2a/{endpoint} whose bearer
// auth middleware reuses the EXACT mechanism the vended MCP surface uses (internal/mcp) — the token
// is hashed (internal/cred), resolved to an ACTIVE endpoint row, checked against the {endpoint} path
// slug, and the endpoint's immutable scope is injected into the request context for the RPC layer to
// enforce. A missing or invalid credential is rejected identically to an unauthenticated MCP call
// (401), and a valid credential presented against a different endpoint's path gets 403. The URL
// alone grants nothing and cookies carry no authority; only the Authorization header is consulted.
//
// The router dispatches a single JSON-RPC 2.0 request to a per-method handler. The individual method
// handlers (SendMessage, GetTask, ListTasks, CancelTask, SubscribeToTask, ...) are separate stories;
// here they are thin stubs that establish the request/response envelope and error mapping every one
// of them will use. Adding a real handler is registering it in the methods map — the envelope,
// auth, body-limit, and error-mapping plumbing is done once, here.
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// maxBodyBytes caps A2A request bodies before JSON-RPC parsing. 256 KiB matches SPEC-0018's Security
// Requirements ("Request Body Size Limits ... Default limit: 256 KiB, matching the largest reasonable
// A2A Message/Part payload this capability accepts").
const maxBodyBytes = 256 << 10

// EndpointStore is the slice of the store the A2A auth middleware needs — identical to the MCP
// surface's EndpointStore (internal/mcp): resolution of BOTH credential shapes (the static sbk_
// bearer and the OAuth access token, SPEC-0016) plus the last-seen stamp. Reusing the same lookups
// means an A2A call and an MCP call authenticate byte-for-byte identically; *store.Store satisfies
// it and tests substitute a fake. Governing: SPEC-0018 REQ "SendMessage Requires a Vended Endpoint"
// (same authorization path create_for uses).
type EndpointStore interface {
	EndpointByCredHash(ctx context.Context, credHash string) (store.AuthEndpoint, error)
	EndpointByOAuthToken(ctx context.Context, tokenHash string) (store.AuthEndpoint, error)
	TouchEndpoint(ctx context.Context, endpointID string) error
}

// Handler mounts the per-endpoint A2A JSON-RPC surface and dispatches to per-method handlers.
type Handler struct {
	store   EndpointStore
	log     *slog.Logger
	methods map[string]methodHandler
}

// methodHandler handles one A2A JSON-RPC method for an authenticated endpoint. It returns the
// result value to place in the response envelope, or an error the router maps onto a JSON-RPC error
// object via ErrorToRPC. The endpoint (identity + immutable scope) is read from ctx via
// EndpointFromContext; params is the raw JSON-RPC `params` member, decoded per handler.
type methodHandler func(ctx context.Context, ep store.AuthEndpoint, params json.RawMessage) (any, error)

// New builds the A2A surface and registers its method handlers. The individual RPC handlers are
// separate stories; the stubs registered here return ErrUnsupportedOperation so the routing,
// envelope, and error mapping are exercised and tested before any handler has real behavior. A story
// implementing a method replaces its stub in this map.
func New(st EndpointStore, log *slog.Logger) *Handler {
	h := &Handler{store: st, log: log}
	h.methods = map[string]methodHandler{
		// SendMessage creates a task (a todo) — the state-changing entry point, gated by the vended
		// endpoint and rate-limited (SPEC-0018). Stub until its story lands.
		"message/send":          h.unimplemented("message/send"),
		"message/stream":        h.unimplemented("message/stream"),
		"tasks/get":             h.unimplemented("tasks/get"),
		"tasks/list":            h.unimplemented("tasks/list"),
		"tasks/cancel":          h.unimplemented("tasks/cancel"),
		"tasks/resubscribe":     h.unimplemented("tasks/resubscribe"),
		"agent/getExtendedCard": h.unimplemented("agent/getExtendedCard"),
	}
	return h
}

// Register installs (or replaces) the handler for an A2A method. Handler-implementing stories call
// this at wiring time to swap a stub for real behavior, keeping the envelope/auth plumbing in one
// place. It is not safe to call concurrently with request serving; wire everything before Routes is
// mounted and serving.
func (h *Handler) Register(method string, fn methodHandler) {
	h.methods[method] = fn
}

// Routes returns the router to mount at /a2a. Every A2A JSON-RPC call for an endpoint lands on the
// same per-endpoint URL, behind the same middleware stack shape the MCP surface uses: security
// headers, body limit, bearer auth. (Rate limiting is a SendMessage-specific concern handled inside
// that handler's story, not a blanket middleware, so read-path calls are not throttled by the
// task-creation bucket.)
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Route("/{endpoint}", func(r chi.Router) {
		r.Use(h.headers)
		r.Use(h.limitBody)
		r.Use(h.auth)
		r.Post("/", h.serve)
	})
	return r
}

// headers pins the SPEC-0018 "Security Headers" response headers on every A2A response (including
// SSE stream responses a streaming handler writes): this is a JSON/SSE API that never embeds
// content, so the CSP locks everything down.
func (h *Handler) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'none'")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		hdr.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// limitBody bounds request bodies at maxBodyBytes, answering 413 for declared-oversize requests up
// front and draining chunked bodies through http.MaxBytesReader BEFORE the JSON-RPC parse, so an
// oversized chunked body also gets 413 rather than a mid-parse 400. Governing: SPEC-0018 REQ
// "Request Body Size Limits".
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

// auth is the boundary, mirroring internal/mcp.Handler.auth exactly: bearer token → hash → active
// endpoint → path-slug match → scope in context. Missing/invalid credentials get 401 (the same
// answer an unauthenticated MCP call gets); a valid credential against the wrong endpoint path gets
// 403. Governing: SPEC-0018 REQ "SendMessage Requires a Vended Endpoint" ("rejected identically to
// an unauthenticated MCP call"); ADR-0008 (URL + credential together = the grant).
func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "endpoint")
		tok := bearer(r)
		if tok == "" {
			h.unauthorized(w)
			return
		}
		// Two credential shapes, one capability (SPEC-0016): sbk_ marks a static endpoint bearer,
		// everything else is tried as an OAuth access token. Both resolve to the same AuthEndpoint
		// shape, so scope enforcement downstream is identical regardless of shape.
		var ep store.AuthEndpoint
		var err error
		if strings.HasPrefix(tok, "sbk_") {
			ep, err = h.store.EndpointByCredHash(r.Context(), cred.Hash(tok))
		} else {
			ep, err = h.store.EndpointByOAuthToken(r.Context(), cred.Hash(tok))
		}
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Unknown or revoked — same answer either way, revealing nothing about the slug.
				h.unauthorized(w)
				return
			}
			h.log.Error("a2a auth", "slug", slug, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if ep.Slug != slug {
			h.log.Warn("a2a credential/path mismatch", "path_slug", slug, "credential_slug", ep.Slug)
			http.Error(w, "credential does not match this endpoint", http.StatusForbidden)
			return
		}
		// Stamp last-seen; failure to stamp is logged but never blocks an authenticated call.
		if err := h.store.TouchEndpoint(r.Context(), ep.ID); err != nil {
			h.log.Warn("a2a touch endpoint", "slug", slug, "err", err)
		}
		next.ServeHTTP(w, r.WithContext(withEndpoint(r.Context(), ep)))
	})
}

// rpcRequest is a JSON-RPC 2.0 request. ID is echoed verbatim in the response (any JSON value, or
// absent for a notification). Params is the raw method params, decoded by the method handler.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response. Exactly one of Result or Error is set. ID echoes the
// request id and is REQUIRED by the JSON-RPC 2.0 spec even on error — it is the JSON literal `null`
// when the request id could not be determined (a parse/read failure), never omitted. So the field
// carries no omitempty; writers pass echoID to normalize a nil id to `null`.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// echoID returns the request id to echo in a response, substituting the JSON literal `null` when the
// id is absent (nil), so the response is a valid JSON-RPC 2.0 object rather than one missing the
// required `id` member.
func echoID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// serve parses the JSON-RPC envelope, dispatches to the method handler, and writes the response.
// Parse/validation failures and an unknown method are answered with the correct JSON-RPC error code;
// a handler error is mapped via ErrorToRPC. The endpoint is guaranteed present in ctx (auth ran).
// Governing: SPEC-0018 REQ "Error Handling Standards" (no silent swallowing; structured logging).
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	ep, ok := EndpointFromContext(r.Context())
	if !ok { // cannot happen behind auth; defense in depth
		h.unauthorized(w)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, nil, &RPCError{Code: CodeParseError, Message: "could not read request body"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, nil, &RPCError{Code: CodeParseError, Message: "invalid JSON"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		h.writeError(w, req.ID, &RPCError{Code: CodeInvalidRequest, Message: "not a valid JSON-RPC 2.0 request"})
		return
	}
	handler, ok := h.methods[req.Method]
	if !ok {
		h.writeError(w, req.ID, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + req.Method})
		return
	}
	result, err := handler(r.Context(), ep, req.Params)
	if err != nil {
		rpcErr := ErrorToRPC(err)
		// Structured logging at the dispatch boundary: the endpoint slug, the method, and the wrapped
		// error chain — never the bearer credential (it never reaches this layer). An internal error is
		// logged at Error; an expected/mapped condition at a lower level to keep logs signal-rich.
		if rpcErr.Code == CodeInternalError {
			h.log.Error("a2a method failed", "slug", ep.Slug, "method", req.Method, "err", err)
		} else {
			h.log.Info("a2a method rejected", "slug", ep.Slug, "method", req.Method,
				"code", rpcErr.Code, "err", err)
		}
		h.writeError(w, req.ID, rpcErr)
		return
	}
	h.writeResult(w, req.ID, result)
}

// unimplemented is the method-handler stub used until a method's own story lands. It returns
// ErrUnsupportedOperation, which ErrorToRPC maps to the A2A UnsupportedOperation code — a caller
// invoking a not-yet-built method gets the specific "unsupported operation" error, not a generic
// failure. Governing: SPEC-0018 REQ "Error Handling Standards".
func (h *Handler) unimplemented(method string) methodHandler {
	return func(_ context.Context, _ store.AuthEndpoint, _ json.RawMessage) (any, error) {
		return nil, fmt.Errorf("%s: %w", method, ErrUnsupportedOperation)
	}
}

func (h *Handler) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	h.writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: echoID(id), Result: result})
}

// writeError writes a JSON-RPC error response. The HTTP status is 200 for JSON-RPC-level errors
// (the error is carried in the body per JSON-RPC 2.0), except the rate-limit code which also sets
// 429 so an HTTP-layer client sees the throttle without parsing the body (SPEC-0018 Rate Limiting).
func (h *Handler) writeError(w http.ResponseWriter, id json.RawMessage, rpcErr *RPCError) {
	status := http.StatusOK
	if rpcErr.Code == CodeRateLimited {
		status = http.StatusTooManyRequests
	}
	h.writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: echoID(id), Error: rpcErr})
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already written, so this can only be logged, never recovered into a
		// different response — but it must not be swallowed. Governing: SPEC-0018 REQ "Error Handling
		// Standards" (no silent swallowing).
		h.log.Error("a2a encode response", "err", err)
	}
}

// unauthorized answers a failed bearer authentication with a bare 401 and a realm challenge,
// matching the MCP surface's unauthenticated answer. The RFC 9728 resource_metadata pointer the MCP
// surface adds is an OAuth-discovery affordance tied to that mount; the A2A binding keeps the bare
// challenge here and leaves discovery-doc wiring to a later story if A2A callers need it.
func (h *Handler) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard"`)
	http.Error(w, "invalid or revoked credential", http.StatusUnauthorized)
}

// --- context plumbing (same shape as internal/mcp) ---

type ctxKey int

const epKey ctxKey = 0

func withEndpoint(ctx context.Context, ep store.AuthEndpoint) context.Context {
	return context.WithValue(ctx, epKey, ep)
}

// EndpointFromContext returns the authenticated endpoint (identity + immutable scope) injected by
// the auth middleware. Method handlers read it to enforce queue scope on the todos they touch.
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
