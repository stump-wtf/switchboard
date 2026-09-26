// Package mcp serves every vended endpoint over the MCP Streamable HTTP transport at
// /mcp/{endpoint}, using the official Go MCP SDK (ADR-0017, SPEC-0014).
//
// A chi middleware authenticates the bearer credential BEFORE the SDK sees the request: the token
// — a static sbk_ bearer or an OAuth access token (SPEC-0016), both accepted interchangeably — is
// hashed (internal/cred), resolved to an ACTIVE endpoint row, checked against the {endpoint}
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

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
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

	// a2uiInstructions is appended only when the A2UI surface is registered (ADR-0023 flag): the
	// instructions must never advertise a resource the session cannot read.
	a2uiInstructions = ` A2UI surfaces at switchboard://queue/{name}/a2ui and switchboard://todo/{id}/a2ui render ` +
		`the queue and per-todo detail as application/a2ui+json for hosts that support it. Both ` +
		`accept an optional ?w=N width hint (ignored by these surfaces) so a host may append it ` +
		`uniformly to any /a2ui URI.`
)

// sessionInstructions is the instructions block a new session receives: the doorbell contract,
// plus the A2UI paragraph only while that surface is on.
func (h *Handler) sessionInstructions() string {
	if h.a2uiOn() {
		return instructions + a2uiInstructions
	}
	return instructions
}

// EndpointStore is the slice of the store the auth middleware needs: resolution of BOTH credential
// shapes — the static sbk_ bearer and the OAuth access token — plus the last-seen stamp. Both
// lookups return the same AuthEndpoint, so everything past "resolve to endpoint ID" is identical
// regardless of shape (SPEC-0016 REQ "Resource-Server Token Validation"). *store.Store satisfies
// it; tests substitute a fake.
type EndpointStore interface {
	EndpointByCredHash(ctx context.Context, credHash string) (store.AuthEndpoint, error)
	EndpointByOAuthToken(ctx context.Context, tokenHash string) (store.AuthEndpoint, error)
	TouchEndpoint(ctx context.Context, endpointID string) error
}

// ToolStore is the full store slice the MCP surface consumes: endpoint auth, the SPEC-0006 drain
// verbs the tool layer wraps (lifecycle semantics live in internal/store per SPEC-0003), and the
// SPEC-0005 event-history reads (events.go).
type ToolStore interface {
	EndpointStore
	ListTodos(ctx context.Context, endpointID string, queues []string, state string, limit int) ([]store.Todo, error)
	GetTodo(ctx context.Context, endpointID, id string) (store.Todo, error)
	ClaimTodo(ctx context.Context, endpointID, id, owner string, ttl time.Duration) (store.Todo, error)
	ClaimNext(ctx context.Context, endpointID string, queues []string, owner string, ttl time.Duration) (store.Todo, error)
	// RingOnAttach is the catch-up ring behind a freshly opened notification stream (SPEC-0011
	// scenario "Reconnecting session is rung for waiting work"): the store picks and charges the
	// rows; the session that opened the stream is the only one rung.
	RingOnAttach(ctx context.Context, endpointID string, queues []string) ([]store.Todo, error)
	HeartbeatTodo(ctx context.Context, endpointID, id, owner string, ttl time.Duration) (store.Todo, error)
	CompleteTodo(ctx context.Context, endpointID, id, owner string, result []byte) (store.Todo, error)
	FailTodo(ctx context.Context, endpointID, id, owner string, result []byte) (store.Todo, error)
	// The attempt-aware lifecycle the drain verbs call (SPEC-0034): claim options (the lease-token
	// fence) in, the report and presented token hash out.
	ClaimTodoWith(ctx context.Context, endpointID, id, owner string, o store.ClaimOpts) (store.Todo, store.ClaimedAttempt, error)
	ClaimNextWith(ctx context.Context, endpointID string, queues []string, owner string, o store.ClaimOpts) (store.Todo, store.ClaimedAttempt, error)
	HeartbeatTodoWith(ctx context.Context, endpointID, id, owner string, ttl time.Duration, tokenHash []byte) (store.Todo, error)
	CompleteTodoWith(ctx context.Context, endpointID, id, owner string, r store.Report) (store.Todo, error)
	FailTodoWith(ctx context.Context, endpointID, id, owner string, r store.Report) (store.Todo, error)
	// ReleaseTodoWith is release's transition (SPEC-0034 REQ-9): back to pending, attempt unchanged.
	ReleaseTodoWith(ctx context.Context, endpointID, id, owner string, r store.Report) (store.Todo, error)
	// TodoAttempts is get_todo's history read (SPEC-0034 REQ-8, REQ-10): endpoint-scoped, newest first.
	TodoAttempts(ctx context.Context, endpointID, id string, limit int) ([]store.Attempt, int, int, error)
	ListEventHistory(ctx context.Context, f store.EventHistoryFilter) ([]store.EventHistoryItem, error)
	EventHistoryByID(ctx context.Context, id int64) (store.EventHistoryDetail, error)
	// SPEC-0006 webhook self-management (webhooks.go): switchboard mints and HOLDS the signing
	// secret so it can HMAC-verify inbound deliveries per SPEC-0003; the plaintext secret is
	// persisted server-side and revealed exactly once at create/rotate time.
	// Governing: ADR-0012 (agents self-manage webhooks), SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency".
	CreateWebhook(ctx context.Context, endpointID, sourceType, targetQueue, trustMode, ingestToken, secret string, max int) (store.Webhook, error)
	ListWebhooks(ctx context.Context, endpointID string) ([]store.Webhook, error)
	RotateWebhookSecret(ctx context.Context, id, endpointID, newSecret, newIngestToken string) (store.Webhook, error)
	DeleteWebhook(ctx context.Context, id, endpointID string) error
	// ADR-0022 webhook route fan-out (webhook_routes.go): an agent points one of its own webhooks at
	// additional target endpoints, and every delivery mints one endpoint-owned todo per target. The
	// three reads below are the authorization substrate the verb layer enforces ownership with —
	// AddWebhookRoute itself documents that its CALLER owns that check.
	// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)", ADR-0010.
	AddWebhookRoute(ctx context.Context, webhookID, targetEndpointID, grantedByHumanID string) error
	RemoveWebhookRoute(ctx context.Context, webhookID, targetEndpointID string) error
	ListWebhookRoutes(ctx context.Context, webhookID string) ([]store.WebhookRoute, error)
	WebhookOwnerEndpointForHuman(ctx context.Context, webhookID, ownerHumanID string) (string, error)
	EndpointOwnerHuman(ctx context.Context, endpointID string) (string, error)
	FriendEdgeAuthorizesDelivery(ctx context.Context, fromHumanID, toHumanID string) (bool, error)
	// ADR-0024 routing rules (webhook_rules.go): read and read-modify-write a webhook's jq rules under
	// human ownership, compute the grant from its live delivery targets, and scope a dry-run's stored
	// event to the webhook it arrived on. Governing: ADR-0024, SPEC-0020.
	WebhookRoutingForHuman(ctx context.Context, webhookID, ownerHumanID string) (store.WebhookRouting, error)
	UpdateWebhookRouting(ctx context.Context, webhookID, ownerHumanID string, mutate func(store.WebhookRouting) (routing.Config, error)) (store.WebhookRouting, error)
	ResolveWebhookTargets(ctx context.Context, webhookID, ownerEndpointID string) ([]string, error)
	EventForWebhook(ctx context.Context, eventID int64, webhookID string) (store.EventHistoryDetail, error)
	// EndpointScopeQueues feeds the grant's per-target scopes for exclusive delivery (ADR-0025).
	EndpointScopeQueues(ctx context.Context, endpointIDs []string) (map[string][]string, error)
	// SettingString backs replay target resolution (SPEC-0005 REQ "Replay Safety"): the
	// `replay_default_target` fallback and the `replay_allowed_targets` allowlist both read here.
	SettingString(ctx context.Context, key, def string) (string, error)
}

// Handler mounts the per-endpoint Streamable HTTP MCP sessions, their scope-filtered tool
// registry, and the doorbell fan-out.
type Handler struct {
	store ToolStore
	log   *slog.Logger

	// preRL bounds unauthenticated volumetric abuse per client IP before any store lookup; rl is
	// the per-endpoint budget, keyed by endpoint id AFTER authentication so an unauthenticated
	// caller can neither drain a real endpoint's bucket nor grow the map with slug spam. replayRL
	// is the tighter per-endpoint budget guarding the one side-effecting tool (replay_webhook_event),
	// which performs an outbound POST and could be abused for amplification/SSRF probing.
	// Governing: SPEC-0005 REQ "Rate Limiting" (replay bounded more tightly than reads).
	preRL    *rateLimiter
	rl       *rateLimiter
	replayRL *rateLimiter

	// baseURL is the externally-reachable origin used to build the ingest_url returned by
	// create_webhook/rotate_webhook (SPEC-0006). Installed at wiring time via SetBaseURL; read
	// atomically so live sessions never race the install.
	baseURL atomic.Pointer[string]

	// a2uiEnabled gates the A2UI resource surface (ADR-0023: advanced capabilities are hidden by
	// default). Nil until SetA2UIEnabled runs, which reads as off. Read at per-session registration
	// time, so new sessions pick up a flip without racing live ones.
	a2uiEnabled atomic.Pointer[bool]

	// summaryFromResult is the operator's SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT opt-in: complete and
	// fail with no summary store the result's compact JSON as the attempt summary (report.go
	// resultReport). The zero value is off, the spec's default. Governing: SPEC-0034 REQ-5.
	summaryFromResult atomic.Bool

	// router evaluates rules for test_webhook_rules — the same out-of-process sandbox the receiver
	// uses (New installs it; tests swap in routing.InProcess). Governing: ADR-0024, SPEC-0020.
	router atomic.Pointer[routing.Router]

	idleTimeout time.Duration

	// mu guards sessions and closed. sessions is the live Streamable HTTP session registry, keyed
	// by Mcp-Session-Id and bound to the endpoint that minted each session.
	mu       sync.Mutex
	sessions map[string]*mcpSession
	closed   bool

	// doorbellRR is the per-endpoint round-robin cursor for unicast doorbell delivery, guarded by
	// mu alongside sessions. Keyed by endpoint id; entries are dropped when an endpoint's last
	// session goes away, so the map tracks live endpoints rather than growing forever.
	doorbellRR map[string]uint64

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
		preRL: newRateLimiter(50, 100),
		rl:    newRateLimiter(20, 40),
		// Replay is bounded well under the read budget (5 rps / 20 burst vs. 20 / 40): enough for
		// interactive local-consumer testing, far too little for amplification or SSRF sweeps.
		replayRL:    newRateLimiter(5, 20),
		idleTimeout: sessionIdleTimeout,
		sessions:    map[string]*mcpSession{},
		doorbellRR:  map[string]uint64{},
		done:        make(chan struct{}),
	}
	if sb, err := routing.NewSandbox(""); err == nil {
		h.SetRouter(sb)
	} else if log != nil {
		log.Error("routing sandbox unavailable; test_webhook_rules will route by default", "err", err)
	}
	h.wg.Add(1)
	go h.janitor()
	return h
}

// SetA2UIEnabled toggles the A2UI resource surface (ADR-0023: advanced capabilities are hidden by
// default and a deliberate flag flip brings them back). Registration happens per session at
// newServer time, so sessions established after the call see the resources; live ones are
// untouched. Default off.
func (h *Handler) SetA2UIEnabled(enabled bool) { h.a2uiEnabled.Store(&enabled) }

// SetAttemptSummaryFromResult sets the operator's summary-from-result option (SPEC-0034 REQ-5): when
// on, a complete or fail that sends no summary closes its attempt with the result's compact JSON as
// the summary. Default off. It applies to every call after it, on live sessions too.
func (h *Handler) SetAttemptSummaryFromResult(on bool) { h.summaryFromResult.Store(on) }

// a2uiOn reads the gate; nil reads as off.
func (h *Handler) a2uiOn() bool {
	if p := h.a2uiEnabled.Load(); p != nil {
		return *p
	}
	return false
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
			h.unauthorized(w, r)
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
			h.unauthorized(w, r)
			return
		}
		// Two credential shapes, one capability (SPEC-0016 REQ "Resource-Server Token Validation"):
		// the sbk_ prefix marks a static endpoint bearer (internal/cred.Mint), everything else is
		// tried as an OAuth access token (internal/oauthsrv.MintToken — deliberately unprefixed).
		// Both resolve to the SAME AuthEndpoint shape, so scope enforcement, sessions, doorbells,
		// and tooling downstream are byte-for-byte identical; the dispatch only picks which hash
		// column answers. Expired/revoked tokens and dead endpoints are uniformly ErrNotFound → the
		// RFC 9728 challenge below.
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
				h.unauthorized(w, r)
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
		Instructions: h.sessionInstructions(),
		Capabilities: &sdk.ServerCapabilities{
			Tools:        &sdk.ToolCapabilities{ListChanged: true},
			Experimental: map[string]any{"claude/channel": map[string]any{}},
		},
	})
	h.registerTools(srv, ep)
	// The SPEC-0005 event-history contract (list/get/replay/providers) shares the session and the
	// same allowlist-filtered registration (events.go), plus the read-only recent-events resource.
	h.registerEventTools(srv, ep)
	// The SPEC-0006 webhook self-management verbs (create/list/rotate/delete_webhook) share the
	// session and the same allowlist-filtered registration (webhooks.go).
	h.registerWebhookTools(srv, ep)
	// The ADR-0022 routing verbs (add/list/remove_webhook_route) share the webhook verb family and
	// the same allowlist-filtered registration (webhook_routes.go).
	h.registerWebhookRouteTools(srv, ep)
	h.registerWebhookRuleTools(srv, ep)
	h.registerEventResources(srv, ep)
	// The #102 A2UI resource surface (a2ui.go): queue and todo detail rendered as
	// application/a2ui+json for A2UI-capable hosts.
	h.registerA2UIResources(srv, ep)
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

// unauthorized answers a failed bearer authentication with the RFC 9728 challenge: alongside the
// realm, WWW-Authenticate carries resource_metadata pointing at this mount's protected-resource
// metadata document — how a spec-following MCP client discovers the authorization server and
// begins the OAuth flow instead of dead-ending on a bare 401. The challenge stays bare when no
// base URL is wired (partial wiring, tests) or the path slug is not even slug-shaped (client-
// supplied path input is never reflected into a response header).
// Governing: ADR-0019, SPEC-0016 REQ "Protected Resource Metadata".
func (h *Handler) unauthorized(w http.ResponseWriter, r *http.Request) {
	challenge := `Bearer realm="switchboard"`
	if base := h.baseURL.Load(); base != nil && *base != "" {
		if slug := chi.URLParam(r, "endpoint"); oauthsrv.SlugOK(slug) {
			challenge += `, resource_metadata="` + oauthsrv.ResourceMetadataURL(*base, slug) + `"`
		}
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "invalid or revoked credential", http.StatusUnauthorized)
}
