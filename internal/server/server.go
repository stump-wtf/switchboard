// Package server wires the switchboard HTTP surface: webhooks, the human web UI, the vended MCP
// endpoints (Streamable HTTP; ADR-0017), static assets, and background workers (lease reaper,
// retention pruner) — one chi router, one PostgreSQL layer.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	switchboard "github.com/joestump/switchboard"
	"github.com/joestump/switchboard/internal/adapter/runner"
	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/ingest"
	mcpsrv "github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/oauthsrv"
	"github.com/joestump/switchboard/internal/store"
	"github.com/joestump/switchboard/internal/web"
)

// Run connects to Postgres, migrates, builds the router, and serves until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	log.Info("database ready")

	// When SWITCHBOARD_SECRET_ENCRYPTION_KEY is set, hold self-managed webhook signing secrets as
	// AES-256-GCM ciphertext at rest (key held outside the DB). A malformed key fails startup loudly
	// rather than silently persisting plaintext. Governing: SPEC-0006 REQ "Switchboard Owns Secrets,
	// Verification, and Idempotency".
	var storeOpts []store.Option
	if key, err := cred.ParseSecretBoxKey(cfg.SecretEncryptionKey); err != nil {
		return err
	} else if len(key) > 0 {
		box, err := cred.NewSecretBox(key)
		if err != nil {
			return err
		}
		storeOpts = append(storeOpts, store.WithSecretCipher(box))
		log.Info("webhook signing secrets encrypted at rest")
	}
	st := store.New(pool, storeOpts...)
	// The ingest Hub is the accept-path's lossy, in-process new-todo doorbell (ingest.Hub); the
	// production channel push to live MCP sessions flows through the store doorbell hook wired below.
	// (The stdio adapter and the /agent REST surface — and their agentapi.Hub — were retired in the
	// SPEC-0014 cutover; MCP is served exclusively over HTTP at /mcp/{endpoint}.)
	hub := ingest.NewHub()
	authr, err := auth.New(ctx, cfg, st, log)
	if err != nil {
		return err
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		return err
	}
	// Enable the SPEC-0013 Personas view by feature detection: the personas store and the well-known
	// Agent Card route (registered below) are both wired in this Run, so the capability has landed and
	// the rail entry + /personas routes come alive. Governing: SPEC-0013 REQ "Personas View"
	// (capability-gated), design.md "Capability gating for Personas and Friends".
	webh.SetPersonasEnabled(true)
	// Feed the web SSE hub from committed transitions (process-local publish hooks): todo
	// lifecycle changes, newly accepted inbound events, and endpoint last-seen stamps become
	// the SPEC-0013 typed event stream. Best-effort by design: the hub drops on full buffers
	// and PostgreSQL stays authoritative. Governing: SPEC-0012 REQ "Live Updates via SSE",
	// SPEC-0013 REQ "Live Updates and Toasts".
	st.SetTodoTransitionHook(webh.PublishTodoTransition)
	st.SetEventHook(webh.PublishEventReceived)
	st.SetEndpointSeenHook(webh.PublishEndpointSeen)

	// The MCP mount owns live Streamable HTTP sessions; Close tears them (and their goroutines)
	// down on shutdown. Governing: SPEC-0014 REQ "Concurrency Safety".
	mcph := mcpsrv.New(st, log)
	defer mcph.Close()
	// The externally-reachable origin the webhook self-management verbs build ingest URLs from
	// (SPEC-0006 create_webhook/rotate_webhook return an ingest_url).
	mcph.SetBaseURL(cfg.BaseURL)
	// Same committed-transition publish source as the web SSE hub, one consumer per surface:
	// the store's doorbell hook fans verified todo creations out to in-scope MCP sessions as
	// notifications/claude/channel doorbells. Governing: SPEC-0014 REQ "Channels Push over the
	// HTTP Stream", SPEC-0011 (push semantics; queue stays the ledger). The doorbellGate is
	// shared with the todo_ready LISTEN loop below so the two wakeup paths (in-process hook,
	// in-database notification) never double-ring the same todo.
	doorbells := newDoorbellGate(doorbellGateTTL)
	st.SetTodoDoorbellHook(func(t store.Todo) {
		if doorbells.first(t.ID) {
			mcph.PublishTodoReady(t)
		}
	})
	// Revoking an endpoint in the web UI also closes its live notification streams promptly
	// (SPEC-0014 scenario "Revocation closes live streams").
	webh.SetEndpointRevokedHook(mcph.CloseEndpointSessions)
	// Generic (token/open) providers are explicit operator opt-in via SWITCHBOARD_GENERIC_PROVIDERS;
	// a malformed or invalid-mode config fails startup loudly rather than silently opening an
	// endpoint. Governing: SPEC-0001 REQ "Explicit Open Trust Mode".
	generic, err := ingest.ParseGenericProviders(os.Getenv("SWITCHBOARD_GENERIC_PROVIDERS"))
	if err != nil {
		return err
	}
	icfg := ingest.Config{
		GitHubSecret: os.Getenv("SWITCHBOARD_GITHUB_SECRET"),
		GitHubQueue:  os.Getenv("SWITCHBOARD_GITHUB_QUEUE"),
		StripeSecret: os.Getenv("SWITCHBOARD_STRIPE_SECRET"),
		StripeQueue:  os.Getenv("SWITCHBOARD_STRIPE_QUEUE"),
		SlackSecret:  os.Getenv("SWITCHBOARD_SLACK_SECRET"),
		SlackQueue:   os.Getenv("SWITCHBOARD_SLACK_QUEUE"),
		Generic:      generic,
		DevLogin:     cfg.DevLogin,
	}.Normalized()
	ing := ingest.New(st, hub, log, icfg)
	// Env config becomes an idempotent boot seed into the provider registry (create-if-absent,
	// never clobber operator edits); the registry is authoritative thereafter, and dispatch
	// resolves it live. Governing: ADR-0020, SPEC-0017 REQ "Environment Config Import".
	if err := seedEnvProviders(ctx, st, icfg, log); err != nil {
		return err
	}
	// list_providers (SPEC-0005) reads the provider registry LIVE, so runtime-created providers
	// enumerate without a restart; the output shape (presence/absence classification only, never
	// secret material) is unchanged. Governing: SPEC-0005 REQ "Provider Enumeration Without
	// Secrets"; ADR-0020, SPEC-0017 REQ "Runtime Provider Registry".
	mcph.SetProviderSource(func(pctx context.Context) []mcpsrv.ProviderStatus {
		rows, err := st.ListProviders(pctx)
		if err != nil {
			log.Error("list providers from registry", "err", err)
			return nil
		}
		return providerStatuses(rows)
	})

	r := newRouter(routerDeps{
		st:      st,
		authr:   authr,
		webh:    webh,
		ing:     ing,
		mcp:     mcph,
		oauth:   oauthsrv.New(st, cfg.BaseURL, log),
		friends: newFriendIntake(st, authr, log),
		ping:    pool.Ping,
		log:     log,
	})

	// The reaper also enforces vend-time credential lifetimes: an endpoint whose expires_at has
	// passed is flipped to revoked in the store and its live MCP sessions are torn down through the
	// SAME CloseEndpointSessions path the web UI's revoke uses — expiry IS revocation, not a
	// parallel lifecycle. Governing: SPEC-0016 REQ "Credential Lifetime", ADR-0019.
	go reaper(ctx, st, log, mcph.CloseEndpointSessions, reapInterval)
	// Retention pruner: the periodic task SPEC-0004 mandates so events and terminal todos cannot
	// grow unbounded. Same lifecycle pattern as the reaper — context-managed, exits on shutdown.
	go pruner(ctx, st, log, pruneInterval)
	// todo_ready LISTEN loop (SPEC-0004 "In-Database Wakeups via LISTEN/NOTIFY"): the consumer
	// for the pg_notify the store already emits on every committed enqueue. Each notification
	// nudges the web SSE hub (count regions re-render from the database) and re-rings the MCP
	// channel doorbell for push-eligible pending todos on that queue — so wakeups no longer
	// depend on this process's HTTP-path store hooks alone. Context-managed like the reaper;
	// reconnects with backoff inside (listen.go).
	go listenTodoReady(ctx, cfg.DatabaseURL, log, func(nctx context.Context, queue string) {
		webh.PublishQueueNudge(queue)
		nudgeDoorbells(nctx, st, doorbells, mcph.PublishTodoReady, queue, log)
	})

	// Pull-adapter poll loops (ADR-0014; SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety"):
	// each queue-family registry row (adapters table) attaches one context-managed worker — enabled-
	// flag gated, backing off on broker errors, health-stamped on the adapters table — and shuts
	// down cleanly with the server. Registry rows hold the NON-SECRET consume topology; the broker
	// DSN comes from SWITCHBOARD_REDIS_URL. Row membership/config is read once here, so adding or
	// editing rows takes a restart; the enabled flag alone is honored at runtime (the full semantics
	// live on registerQueueAdapters). closeAdapters releases the shared broker client and runs (via
	// defer, LIFO) only after the <-runnerDone join below — no worker outlives its client.
	adapters := runner.New(st, log, runner.Options{})
	closeAdapters, err := registerQueueAdapters(ctx, st, adapters, cfg.RedisURL, log)
	if err != nil {
		return err
	}
	defer func() { _ = closeAdapters() }()
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = adapters.Run(ctx) // returns only after every poll loop has been joined
	}()

	srv := &http.Server{Addr: cfg.Addr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Info("switchboard listening", "addr", cfg.Addr, "base_url", cfg.BaseURL,
		"oidc", cfg.OIDCConfigured(), "dev_login", cfg.DevLogin)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	// http.ErrServerClosed means ctx was cancelled (the shutdown goroutine above ran); join the
	// adapter poll loops so no worker goroutine outlives Run — the graceful-shutdown half of
	// SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety".
	<-runnerDone
	return nil
}

// routerDeps carries the wired components newRouter assembles into the HTTP surface. Extracted from
// Run so tests can build the REAL route table (auth grouping included) without a database.
type routerDeps struct {
	st      *store.Store
	authr   *auth.Authenticator
	webh    *web.Handler
	ing     *ingest.Ingest
	mcp     *mcpsrv.Handler             // the Run-wired MCP handler (doorbell + revocation hooks attached)
	oauth   *oauthsrv.Handler           // OAuth AS surface: discovery metadata + dynamic client registration (ADR-0019)
	friends *friendIntake               // A2A friend-request intake (OIDC-provenance authenticated)
	ping    func(context.Context) error // /healthz DB probe
	log     *slog.Logger
}

// newRouter builds the full switchboard route table. Route grouping is the security baseline:
// everything not explicitly registered as a public route sits behind bearer auth (/mcp) or
// auth.RequireHuman (the web UI, including the Board landing at GET /).
// Governing: SPEC-0012 REQ "Screen Set and Routes", REQ "Authentication Boundary";
// SPEC-0013 REQ "Information Architecture and Navigation".
func newRouter(d routerDeps) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// Governing: SPEC-0001/0005/0006/0007/0008/0012 REQ "Security headers on all responses". Applied
	// as the outermost app middleware so every surface (webhook, /agent, MCP, web, errors) carries them.
	r.Use(secureHeaders)

	// Rate limiters (SPEC-0001 inbound webhook; SPEC-0009 persona card). The inbound webhook gets an
	// IP throttle; the durable queue stays the source of truth, so throttling only bounds abuse, never
	// drops work. The vended MCP surface carries its own per-endpoint limiter inside internal/mcp.
	webhookRL := newRateLimiter(10, 20)
	// Public A2A Agent Card endpoint (SPEC-0009): per-IP throttle to blunt enumeration of persona ids.
	// RECOMMENDED default is 60 req/min ≈ 1 req/s; burst 30 absorbs legitimate discovery tooling.
	cardRL := newRateLimiter(1, 30)

	// Static assets + health.
	r.Handle("/static/*", staticHandler())
	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		if err := d.ping(req.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})

	// Inbound ingestion (verified per-provider; ADR-0003). MaxBytesReader inside each receiver
	// bounds the body to 5 MiB → 413 before HMAC verification (SPEC-0001). Stripe/Slack add a
	// replay window over the signed timestamp; GitHub's scheme signs no timestamp, so none is
	// fabricated (SPEC-0001 REQ "Replay-Window Enforcement for Timestamped Signatures").
	r.Group(func(wr chi.Router) {
		wr.Use(webhookRL.middleware)
		wr.Post("/webhooks/github", d.ing.GitHub)
		wr.Post("/webhooks/stripe", d.ing.Stripe)
		wr.Post("/webhooks/slack", d.ing.Slack)
		// Generic token/open providers (SPEC-0001): shared-secret token compared constant-time, or
		// explicit operator-opted-in open mode; unknown names 404, never a fall-through to open.
		wr.Post("/webhooks/generic/{name}", d.ing.Generic)
		// Agent self-managed webhooks (SPEC-0006): the ingest_url create_webhook/rotate_webhook hand
		// back. The unguessable 128-bit path token routes to exactly one webhook; an unknown token
		// 404s. Delivery → dedup → todo. Governing: ADR-0012.
		wr.Post("/webhooks/w/{token}", d.ing.SelfManaged)
	})
	r.Post("/dev/todos", d.ing.DevCreateTodo)

	// Public A2A Agent Card endpoint (ADR-0009; SPEC-0009). This is a DELIBERATE public route — the
	// only one in the personas capability — registered outside auth.RequireHuman: A2A discovery
	// requires peers to read a persona's card before any friendship exists, and the card grants
	// nothing, exposing only owner-approved discovery metadata (name, description, derived skills,
	// owner display name). It is served ONLY for personas the owner marked discoverable; every other
	// slug 404s without leaking existence. GET-only (a state-changing method 405s at the router), per
	// the read-only requirement, with a per-IP throttle and a default-src 'none' CSP set in the
	// handler. Governing: SPEC-0009 REQ "Well-Known Card Endpoint", REQ "Discoverability Is
	// Owner-Controlled", "Security Requirements → Authentication / Rate Limiting".
	r.With(cardRL.middleware).Get("/a/{persona_id}/.well-known/agent-card.json", d.webh.AgentCard)

	// OAuth authorization-server surface (ADR-0019; SPEC-0016): RFC 8414 AS metadata, RFC 9728
	// protected-resource metadata per MCP mount, and RFC 7591 dynamic client registration. All three
	// are DELIBERATELY public — discovery documents are how an unauthenticated MCP client learns to
	// authorize at all, and DCR is anonymous by design (public clients + PKCE; the flow's human gate
	// is the consent screen behind the session, not registration). The metadata GETs read nothing
	// from the store; registration validates every field (exact redirect-URI rules) and its body is
	// bounded at 64 KiB — a registration is a handful of URIs and a name, never a blob. One per-IP
	// throttle covers the surface, same limiter shape as the other public groups.
	// Governing: SPEC-0016 REQ "Protected Resource Metadata", REQ "Authorization Server Metadata",
	// REQ "Dynamic Client Registration", "Security notes" (rate limiting on /oauth/* consistent with
	// the existing limiter).
	oauthRL := newRateLimiter(5, 20)
	r.Group(func(or chi.Router) {
		or.Use(oauthRL.middleware)
		or.Get(oauthsrv.ASMetadataPath, d.oauth.ASMetadata)
		or.Get(oauthsrv.ProtectedResourcePrefix+"/mcp/{endpoint}", d.oauth.ProtectedResourceMetadata)
		or.With(maxBytes(64<<10)).Post(oauthsrv.RegisterPath, d.oauth.Register)
	})

	// Vended MCP endpoints over Streamable HTTP (ADR-0017; SPEC-0014). Bearer auth, per-endpoint
	// rate limit, and the 1 MiB body cap all live inside the package's own middleware stack.
	// The handler is Run-wired (doorbell + revocation hooks) and passed in — never constructed here.
	r.Mount("/mcp", d.mcp.Routes())

	// A2A friend-request intake (ADR-0010; SPEC-0010). Inbound only, and deliberately NOT
	// session-authenticated: the request carries the requesting human's OIDC-signed provenance
	// in-band, and that token IS the credential — missing/invalid provenance → 401 with no pending
	// edge (SPEC-0010 "Missing or invalid provenance is rejected"). This is why it sits outside the
	// RequireHuman group rather than being a "public, ungoverned" route: it is authenticated, just by
	// signed provenance instead of a session cookie or bearer. Body bounded at 64 KiB (a requested
	// scope is a small list + reason, not a blob) and per-IP rate-limited to blunt flooding; the
	// per-requester standing-backlog quota is enforced in the handler. No remote discovery-doc fetch
	// happens here (provenance is verified against the already-configured issuer's JWKS), so there is
	// no per-request SSRF surface. Governing: SPEC-0010 "Security Requirements → Authentication,
	// Rate Limiting, Request Body Size Limits".
	friendRL := newRateLimiter(5, 10) // friend requests are low-frequency per source; burst 10
	r.Group(func(fr chi.Router) {
		fr.Use(friendRL.middleware, maxBytes(64<<10))
		fr.Post("/a2a/friend-requests", d.friends.Intake)
	})

	// Auth (OIDC RP against Pocket ID; ADR-0011). The login screen is the ONLY public web page
	// (SPEC-0012 "Security Requirements → Authentication"; REQ "Screen Set and Routes": public GET /login).
	// These public entry points share a per-IP throttle that blunts credential-stuffing and
	// OIDC-callback abuse — the native limiter SPEC-0008 recommends, applied in-process rather than
	// left solely to a front proxy. Dev-login is config-gated (404 unless SWITCHBOARD_DEV_LOGIN) and,
	// like every auth form POST, bounds its body at 64 KiB. It establishes the session, so no prior
	// session exists to derive a synchronizer CSRF token from; its defenses are the config gate,
	// POST-only method, body cap, and this throttle (the OIDC state cookie plays that role for the
	// OIDC login/callback exchange).
	// Governing: SPEC-0008 REQ "Development Login Guard", REQ "Request Body Size Limits",
	// "Security Requirements → Rate Limiting".
	authRL := newRateLimiter(5, 10) // auth is low-frequency per human; burst 10 covers real logins
	r.Group(func(ar chi.Router) {
		ar.Use(authRL.middleware)
		ar.Get("/login", d.webh.Login)
		ar.Get("/auth/login", d.authr.Login)
		ar.Get("/auth/callback", d.authr.Callback)
		ar.With(maxBytes(64<<10)).Post("/auth/dev-login", d.authr.DevLogin)
	})

	// Human web UI (requires an authenticated human; ADR-0001/008). Form bodies capped at 1 MiB;
	// RequireCSRF guards every state-changing form with a per-session synchronizer token (SPEC-0008).
	// Logout is a session-gated POST — never a GET — so it cannot be triggered cross-site.
	// One shared token bucket meters every state-changing POST on this surface (vend, revoke,
	// persona, friend, and todo actions alike), keyed per authenticated human; GETs and the SSE
	// stream are exempt (postMiddleware). 3 req/s sustained with burst 30 clears any real
	// operator clicking through the Board while blunting scripted abuse of the mutation surface.
	// Governing: SPEC-0013 "Rate Limiting" (shared human-surface limiter on state-changing POSTs).
	humanRL := newRateLimiter(3, 30)
	r.Group(func(pr chi.Router) {
		pr.Use(maxBytes(1 << 20))
		pr.Use(d.authr.RequireHuman)
		pr.Use(humanRL.postMiddleware) // after RequireHuman: keyed by the authenticated human
		pr.Use(d.authr.RequireCSRF)
		// Governing: SPEC-0013 REQ "Information Architecture and Navigation" — GET / renders the
		// Board; the agents screen moves to /agents (surfaced as "Endpoints" in the rail).
		pr.Get("/", d.webh.Board)
		// Todos view: the durable-queue table + detail drawer (SPEC-0013). GET /todos/{id} serves the
		// drawer fragment (HTMX) or a standalone page (deep link / no-JS fallback).
		pr.Get("/todos", d.webh.Todos)
		pr.Get("/todos/{id}", d.webh.TodoDrawer)
		// Endpoints view + vend modal (SPEC-0013). The retired SPEC-0012 /agents screens 303-redirect
		// here; GET /endpoints/vend serves the modal fragment, POST /endpoints/vend mints + reveals once.
		pr.Get("/endpoints", d.webh.Endpoints)
		pr.Get("/endpoints/vend", d.webh.VendModal)
		pr.Post("/endpoints/vend", d.webh.Vend)
		pr.Get("/agents", d.webh.AgentsRedirect)
		pr.Get("/agents/{id}", d.webh.AgentsRedirect)
		// Providers view shell placement (SPEC-0015 "Providers joins the IA"); the SPEC-0017
		// registry backs it in a later story. Governing: SPEC-0015 REQ "Application Shell And
		// Navigation", ADR-0020.
		pr.Get("/providers", d.webh.Providers)
		// Live updates stream (SPEC-0012): session-authenticated SSE; per-session stream cap inside.
		pr.Get("/events", d.webh.Events)
		// Operator todo lifecycle actions (SPEC-0013 endpoints table). Each dispatches to a SPEC-0003
		// store transition; the UI implements no lifecycle rules of its own. CSRF arrives via the
		// layout's hx-headers token; the group's RequireCSRF validates it.
		pr.Post("/todos/{id}/claim", d.webh.ClaimTodo)
		pr.Post("/todos/{id}/complete", d.webh.CompleteTodo)
		pr.Post("/todos/{id}/fail", d.webh.FailTodo)
		pr.Post("/todos/{id}/retry", d.webh.RetryTodo)
		pr.Post("/todos/{id}/extend", d.webh.ExtendTodo)
		pr.Post("/todos/{id}/release", d.webh.ReleaseTodo)
		pr.Post("/endpoints/{id}/revoke", d.webh.Revoke)
		// Permanently delete a revoked endpoint's card (SPEC-0007 REQ "Permanent Deletion of Revoked
		// Endpoints"). Store constrains to state='revoked' + ownership; active endpoints must be revoked
		// first. CSRF arrives via the layout hx-headers / hidden field; the group's RequireCSRF validates.
		pr.Post("/endpoints/{id}/delete", d.webh.DeleteEndpoint)
		// Friends view + approval flow (SPEC-0013 endpoints table; SPEC-0010 approval-is-vend). The
		// handlers 404 until the friending capability is enabled (capability gating lives in the
		// handler, so the routes stay classified session-gated for the route-table baseline). Approve
		// mints a scoped endpoint onto a target-OWNED agent; CSRF arrives via the layout hx-headers.
		pr.Get("/friends", d.webh.Friends)
		pr.Get("/friends/new", d.webh.AddFriendModal)
		pr.Get("/friends/resolve", d.webh.ResolveFriendHandle)
		pr.Post("/friends", d.webh.AddFriend)
		pr.Post("/friends/{id}/approve", d.webh.ApproveFriend)
		pr.Post("/friends/{id}/decline", d.webh.DeclineFriend)
		pr.Post("/friends/{id}/withdraw", d.webh.WithdrawFriend)
		pr.Post("/friends/{id}/revoke", d.webh.RevokeFriend)
		pr.Post("/friends/{id}/unblock", d.webh.UnblockFriend)
		// Personas view (SPEC-0013 endpoints table). Capability-gated inside the handler: while the
		// personas capability is disabled these 404 (hidden-not-broken); the routes stay session- and
		// CSRF-gated like every other web mutation. Governing: SPEC-0013 REQ "Personas View".
		pr.Get("/personas", d.webh.Personas)
		pr.Post("/personas", d.webh.CreatePersona)
		pr.Post("/personas/{id}", d.webh.UpdatePersona)
		pr.Post("/personas/{id}/delete", d.webh.DeletePersona)
		pr.Post("/logout", d.authr.Logout)
	})

	return r
}

// staticHandler serves /static/* from the embedded static FS — no runtime CDN, so the UI works
// offline and under the strict same-origin CSP. Split from Run so tests can exercise the exact
// production mount without a database.
// Governing: ADR-0001 (single binary, embedded assets), SPEC-0012 REQ "Server-Rendered Pages from
// Embedded Templates" (scenario "Static assets served from embed, not a CDN").
func staticHandler() http.Handler {
	staticSub, _ := fs.Sub(switchboard.StaticFS, "static")
	return http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))
}

// secureHeaders sets defensive response headers on every route (SPEC-0001/0005/0006/0007/0008/0012).
// The CSP is tuned to the actual web UI: an external stylesheet under /static plus an inline <style>
// block and inline style="" attributes (hence style-src 'unsafe-inline'); the only scripts are the
// vendored htmx assets embedded and served from /static (ADR-0001: no CDN), so script-src stays
// locked to 'self'. connect-src 'self' is explicit — it permits exactly the same-origin SSE stream
// (/events) and HTMX fetches, so injected markup cannot exfiltrate to another origin even under
// default-src drift. base-uri 'none' forbids <base> entirely (no page needs one, and an injected
// <base> would rebase every relative form action and asset URL). frame-ancestors 'none' backs up
// X-Frame-Options: DENY. Governing: SPEC-0012 REQ "Security Headers" (base-uri 'none', explicit
// connect-src), SPEC-0013 "Security Headers" (same-origin connect-src for SSE).
func secureHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// maxBytes caps a request body at n bytes via http.MaxBytesReader, so a read past the limit errors
// (the reader also writes a 413 when the handler surfaces the error) rather than buffering unbounded
// input. Governing: SPEC-0001 (webhook), SPEC-0012 (web forms) body limits (the vended MCP surface
// bounds its own bodies inside internal/mcp per SPEC-0014).
func maxBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pruneInterval is how often retention runs. The retention bounds are coarse — days of age and
// hundreds of thousands of rows — so enforcement lagging the bound by up to an hour is invisible,
// while hourly (vs. the reaper's 30s) keeps the four DELETE scans off the hot path. Prune is a
// cheap no-op when nothing qualifies, so the steady-state cost is one short transaction per hour.
const pruneInterval = time.Hour

// pruneStore is the single store seam the pruner needs; *store.Store satisfies it. Narrowed to an
// interface so the loop wiring is unit-testable without a database.
type pruneStore interface {
	Prune(ctx context.Context) (store.PruneResult, error)
}

// pruner periodically enforces the hybrid age + row-cap retention policy by invoking store.Prune,
// which deletes over-age events/terminal todos then trims past the row cap in ONE transaction
// (policy read from the settings table: retention_max_age_days / retention_max_rows). It runs once
// at startup — restart-heavy deployments still get at-least-once enforcement per process — then on
// every tick, and exits when ctx is cancelled (graceful shutdown, mirroring the reaper). Errors are
// logged and the loop keeps going: a transient DB failure must not disable retention for the life
// of the process. Governing: SPEC-0004 REQ "Hybrid Retention and Bounded Growth", ADR-0002.
func pruner(ctx context.Context, st pruneStore, log *slog.Logger, interval time.Duration) {
	prune := func() {
		res, err := st.Prune(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown cancelled the in-flight prune; not a failure worth logging
			}
			log.Warn("retention prune failed", "err", err)
			return
		}
		if res.Total() > 0 {
			log.Info("retention pruned",
				"events_aged", res.EventsAged, "events_capped", res.EventsCapped,
				"todos_aged", res.TodosAged, "todos_capped", res.TodosCapped,
				"total", res.Total())
		}
	}
	prune()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// reapInterval is how often the reaper ticks. Lease recovery, due retries, and endpoint expiry all
// tolerate up to one interval of lag; expired CREDENTIALS are additionally refused at auth the
// instant the expiry passes (store.EndpointByCredHash), so the tick only bounds how long a live
// session can linger and when the card flips to revoked.
const reapInterval = 30 * time.Second

// reapStore is the store seam the reaper needs; *store.Store satisfies it. Narrowed to an
// interface so the loop wiring is unit-testable without a database (mirroring pruneStore).
type reapStore interface {
	ReapExpired(ctx context.Context) (int64, error)
	RequeueDueRetries(ctx context.Context) (int64, error)
	ExpireEndpoints(ctx context.Context) ([]string, error)
}

// reaper periodically requeues (or dead-letters) todos with expired leases — crash safety (ADR-0002)
// — and re-queues failed todos whose scheduled retry backoff has elapsed (SPEC-0003 REQ "Bounded
// Retries via max_attempts", scheduled backoff). The claim scan also picks up due retries directly,
// so this loop only bounds how long a due retry can sit without a claimant asking.
//
// It also enforces endpoint credential lifetimes: every tick, endpoints whose vend-time expiry has
// passed are flipped to revoked in the store and each affected endpoint's live MCP sessions are
// closed via closeSessions — the same path a web-UI revoke rings — so an agent holding a live
// session loses it and subsequent bearer or OAuth access fails identically to revocation.
// Errors are logged and the loop keeps going, mirroring the pruner: a transient DB failure must not
// disable enforcement for the life of the process. Governing: SPEC-0016 REQ "Credential Lifetime"
// (scenario "Expiry enforcement"), SPEC-0007 revocation semantics reused, ADR-0019.
func reaper(ctx context.Context, st reapStore, log *slog.Logger, closeSessions func(endpointID string), interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := st.ReapExpired(ctx); err != nil {
				log.Warn("reaper", "err", err)
			} else if n > 0 {
				log.Info("reaped expired leases", "count", n)
			}
			if n, err := st.RequeueDueRetries(ctx); err != nil {
				log.Warn("retry scheduler", "err", err)
			} else if n > 0 {
				log.Info("re-queued scheduled retries", "count", n)
			}
			if ids, err := st.ExpireEndpoints(ctx); err != nil {
				log.Warn("endpoint expiry", "err", err)
			} else if len(ids) > 0 {
				// Close sessions AFTER the store flip: auth already refuses the expired credential,
				// so a session re-established between flip and close is impossible.
				for _, id := range ids {
					if closeSessions != nil {
						closeSessions(id)
					}
				}
				log.Info("expired endpoints revoked", "count", len(ids))
			}
		}
	}
}
