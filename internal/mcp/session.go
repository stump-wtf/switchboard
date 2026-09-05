package mcp

// Session management for the Streamable HTTP mount.
//
// The SDK's bundled StreamableHTTPHandler owns its per-session transports privately, which leaves
// no supported way to emit a custom server→client notification (`notifications/claude/channel` is
// not a standard MCP method, and ServerSession only sends methods from the standard registry). So
// this file keeps ALL protocol mechanics in the SDK — sdk.StreamableServerTransport implements the
// wire transport, sdk.Server.Connect runs the session — but owns the thin session registry itself,
// capturing each session's transport Connection at connect time. Connection.Write is documented
// safe for concurrent use, and per the StreamableServerTransport contract a notification written
// with a detached context is routed to the standalone SSE stream (the GET notification stream) —
// or rejected when no stream is open, which is exactly SPEC-0011's lossy doorbell.
//
// Governing: ADR-0017 (Streamable HTTP only), SPEC-0014 REQ "Channels Push over the HTTP Stream",
// SPEC-0014 REQ "Concurrency Safety".

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
)

// Streamable HTTP wire headers (fixed by the MCP spec; the SDK's own constants are unexported).
const (
	sessionIDHeader = "Mcp-Session-Id"
)

// mcpSession is one live Streamable HTTP session: the SDK transport/session pair, the captured
// transport connection the doorbell pump writes to, and bookkeeping for idle expiry.
type mcpSession struct {
	id         string
	endpointID string
	slug       string
	queues     map[string]bool // immutable scope snapshot from vend time (SPEC-0007)

	transport *sdk.StreamableServerTransport
	session   *sdk.ServerSession
	conn      sdk.Connection

	// doorbells is the per-subscriber bounded buffer (SPEC-0011 "Slow subscriber is dropped, not
	// blocked"): the publisher never blocks on it, and the pump drains it onto the stream.
	doorbells chan store.Todo

	// inflight counts HTTP requests currently being served for this session. An open notification
	// stream (hanging GET) keeps inflight > 0, so it also keeps the session from idling out.
	inflight atomic.Int64
	// lastSeen is the unix-nano timestamp of the last completed request, for idle expiry.
	lastSeen atomic.Int64

	// doorbellFails counts CONSECUTIVE failed doorbell writes, reset by any success. A single
	// failure is routine — the agent simply has no stream open at that instant — but a session
	// that has never once accepted a push is a deaf consumer, and that state is otherwise
	// indistinguishable from a healthy one from the outside: the session is connected and
	// initialized, the queue is filling, and the agent does nothing. This is what makes it sayable.
	doorbellFails atomic.Int64
	// doorbellWarned records whether the deaf-consumer warning has already been emitted for the
	// current failure run, so the log carries one line per broken session rather than one per push.
	doorbellWarned atomic.Bool
}

// connCapturingTransport wraps the SDK transport so the Connection handed to the SDK server is
// also retained for the doorbell pump. Connect is called exactly once, synchronously, inside
// sdk.Server.Connect.
type connCapturingTransport struct {
	inner *sdk.StreamableServerTransport
	conn  sdk.Connection
}

func (c *connCapturingTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := c.inner.Connect(ctx)
	c.conn = conn
	return conn, err
}

// serve routes one authenticated request to its session's SDK transport, creating the session on
// the first (sessionless) POST. It mirrors the SDK handler's transport-level validation so SDK
// clients see identical behavior; session ids are bound to the authenticated endpoint, so a session
// minted on one endpoint can never be replayed against another endpoint's path or credential.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	ep, ok := EndpointFromContext(r.Context())
	if !ok { // cannot happen behind auth; defense in depth
		h.unauthorized(w, r)
		return
	}
	if !h.validateTransportRequest(w, r) {
		return
	}

	var s *mcpSession
	if sid := r.Header.Get(sessionIDHeader); sid != "" {
		s = h.lookupSession(sid, ep.ID)
		if s == nil {
			// Unknown id, or a session that belongs to a different endpoint: same answer either
			// way, revealing nothing about other endpoints' sessions.
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	}

	switch r.Method {
	case http.MethodDelete:
		if s == nil {
			http.Error(w, "Bad Request: DELETE requires an Mcp-Session-Id header", http.StatusBadRequest)
			return
		}
		// Closing the session releases any hanging stream; the reaper goroutine removes it.
		_ = s.session.Close()
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		if s == nil {
			http.Error(w, "Bad Request: GET requires an Mcp-Session-Id header", http.StatusBadRequest)
			return
		}
	case http.MethodPost:
		if s == nil {
			var err error
			if s, err = h.createSession(r, ep); err != nil {
				h.log.Error("mcp create session", "slug", ep.Slug, "err", err)
				http.Error(w, "failed connection", http.StatusInternalServerError)
				return
			}
			// A brand-new session that fails to initialize is torn down when the request ends,
			// so bogus sessionless POSTs can never accrete session state.
			defer func() {
				if s.session.InitializeParams() == nil {
					_ = s.session.Close()
				}
			}()
		}
	}

	s.inflight.Add(1)
	defer func() {
		s.lastSeen.Store(time.Now().UnixNano())
		s.inflight.Add(-1)
	}()
	s.transport.ServeHTTP(w, r)
}

// createSession mints a session id, connects a fresh per-session SDK server over a captured
// transport, registers the session under the endpoint's scope, and starts its doorbell pump plus
// a reaper that cleans up whenever the session closes (DELETE, idle expiry, revocation, shutdown).
func (h *Handler) createSession(r *http.Request, ep store.AuthEndpoint) (*mcpSession, error) {
	id := rand.Text()
	tr := &sdk.StreamableServerTransport{SessionID: id}
	capture := &connCapturingTransport{inner: tr}
	// r.Context() lets middleware values reach the connect path; the jsonrpc2 layer detaches it
	// for the long-lived session, so session lifetime is NOT bound to this one request.
	ss, err := h.newServer(ep).Connect(r.Context(), capture, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect session: %w", err)
	}
	queues := make(map[string]bool, len(ep.ScopeQueues))
	for _, q := range ep.ScopeQueues {
		queues[q] = true
	}
	s := &mcpSession{
		id:         id,
		endpointID: ep.ID,
		slug:       ep.Slug,
		queues:     queues,
		transport:  tr,
		session:    ss,
		conn:       capture.conn,
		doorbells:  make(chan store.Todo, doorbellBuffer),
	}
	s.lastSeen.Store(time.Now().UnixNano())

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = ss.Close()
		return nil, errors.New("mcp: handler is shut down")
	}
	h.sessions[id] = s
	// wg.Add happens under mu, ordered against Close's closed=true + snapshot, so Close's
	// wg.Wait always observes this session's pump and reaper.
	h.wg.Add(2)
	h.mu.Unlock()

	go h.pump(s)
	go h.reapOnClose(s)
	return s, nil
}

// lookupSession resolves a session id, returning nil unless the session exists AND belongs to the
// authenticated endpoint.
func (h *Handler) lookupSession(id, endpointID string) *mcpSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	if s == nil || s.endpointID != endpointID {
		return nil
	}
	return s
}

// reapOnClose blocks until the session's connection closes (any path: DELETE, idle expiry,
// revocation, Handler.Close), then unregisters it and closes its doorbell buffer so the pump
// exits. Publishers only ever reach a session through h.sessions under h.mu, so once the entry is
// deleted no publisher can send on the closed channel.
func (h *Handler) reapOnClose(s *mcpSession) {
	defer h.wg.Done()
	_ = s.session.Wait()
	h.mu.Lock()
	_, live := h.sessions[s.id]
	delete(h.sessions, s.id)
	// Drop this endpoint's doorbell rotation cursor once its last session is gone, so the map
	// tracks live endpoints rather than accumulating one entry per endpoint ever seen.
	stillLive := false
	for _, other := range h.sessions {
		if other.endpointID == s.endpointID {
			stillLive = true
			break
		}
	}
	if !stillLive {
		delete(h.doorbellRR, s.endpointID)
	}
	h.mu.Unlock()
	if live {
		close(s.doorbells)
	}
}

// janitor expires sessions that have no request in flight (no open stream) and have been idle
// past the timeout, bounding the registry. An open notification stream keeps inflight > 0 and so
// keeps its session alive until the client disconnects.
func (h *Handler) janitor() {
	defer h.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case now := <-t.C:
			h.mu.Lock()
			var idle []*mcpSession
			for _, s := range h.sessions {
				if s.inflight.Load() == 0 && now.Sub(time.Unix(0, s.lastSeen.Load())) > h.idleTimeout {
					idle = append(idle, s)
				}
			}
			h.mu.Unlock()
			for _, s := range idle {
				_ = s.session.Close() // reapOnClose unregisters it
			}
		}
	}
}

// CloseEndpointSessions promptly closes every live session vended to the given endpoint. Wired to
// the web UI's revoke action: revoke = credential invalidated (auth already answers 401) AND any
// open notification stream torn down, immediately and totally.
// Governing: SPEC-0014 REQ "Concurrency Safety" scenario "Revocation closes live streams", ADR-0008.
func (h *Handler) CloseEndpointSessions(endpointID string) {
	h.mu.Lock()
	var doomed []*mcpSession
	for _, s := range h.sessions {
		if s.endpointID == endpointID {
			doomed = append(doomed, s)
		}
	}
	h.mu.Unlock()
	for _, s := range doomed {
		_ = s.session.Close() // releases the hanging GET; reapOnClose unregisters
	}
}

// Close shuts the mount down: no new sessions, every live session closed, and all pump/reaper/
// janitor goroutines joined — nothing leaks on server shutdown (SPEC-0014 REQ "Concurrency Safety").
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		open := make([]*mcpSession, 0, len(h.sessions))
		for _, s := range h.sessions {
			open = append(open, s)
		}
		h.mu.Unlock()
		close(h.done)
		for _, s := range open {
			_ = s.session.Close()
		}
		h.wg.Wait()
	})
}

// validateTransportRequest mirrors the SDK handler's HTTP-level validation: method allowlist,
// Accept negotiation, POST Content-Type, and the localhost DNS-rebinding guard. Returns false
// after writing the error response.
func (h *Handler) validateTransportRequest(w http.ResponseWriter, r *http.Request) bool {
	if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && localAddr != nil {
		if isLoopbackHost(localAddr.String()) && !isLoopbackHost(r.Host) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
			return false
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return false
	}
	if r.Method == http.MethodPost {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			http.Error(w, "Content-Type must be 'application/json'", http.StatusUnsupportedMediaType)
			return false
		}
	}
	jsonOK, streamOK := acceptsStreamable(r.Header.Values("Accept"))
	if r.Method == http.MethodGet && !streamOK {
		http.Error(w, "Accept must contain 'text/event-stream' for GET requests", http.StatusBadRequest)
		return false
	}
	if r.Method == http.MethodPost && (!jsonOK || !streamOK) {
		http.Error(w, "Accept must contain both 'application/json' and 'text/event-stream'", http.StatusBadRequest)
		return false
	}
	return true
}

// acceptsStreamable reports whether the Accept headers admit JSON and SSE responses, matching the
// SDK handler's negotiation (wildcards and media-type parameters included).
func acceptsStreamable(values []string) (jsonOK, streamOK bool) {
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			token := strings.TrimSpace(raw)
			base, _, _ := strings.Cut(token, ";")
			switch strings.ToLower(strings.TrimSpace(base)) {
			case "application/json", "application/*":
				jsonOK = true
			case "text/event-stream", "text/*":
				streamOK = true
			case "*/*":
				jsonOK = true
				streamOK = true
			}
		}
	}
	return jsonOK, streamOK
}

// isLoopbackHost reports whether a host or host:port string names a loopback address.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if hp, _, err := net.SplitHostPort(hostport); err == nil {
		host = hp
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
