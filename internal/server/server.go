// Package server wires the switchboard HTTP surface: webhooks, the human web UI, the vended MCP
// endpoints (Streamable HTTP; ADR-0017), static assets, and background workers (lease reaper,
// retention pruner) — one chi router, one PostgreSQL layer.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	switchboard "github.com/stump-wtf/switchboard"
	"github.com/stump-wtf/switchboard/internal/a2a"
	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/ingest"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
	"github.com/stump-wtf/switchboard/internal/web"
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
	encryptionEnabled := false
	if key, err := cred.ParseSecretBoxKey(cfg.SecretEncryptionKey); err != nil {
		return err
	} else if len(key) > 0 {
		box, err := cred.NewSecretBox(key)
		if err != nil {
			return err
		}
		storeOpts = append(storeOpts, store.WithSecretCipher(box))
		encryptionEnabled = true
		log.Info("webhook signing secrets encrypted at rest")
	}
	// The logger is what makes a plaintext signing-secret write audible at the moment it happens;
	// without it the store stays silent exactly as before.
	storeOpts = append(storeOpts, store.WithLogger(log))
	st := store.New(pool, storeOpts...)

	// An empty key is a supported configuration, but it must not be a silent one when the deployment
	// is already holding signing secrets in the clear. This covers the webhooks that exist at boot;
	// one created later is caught by the per-write warning in the store, because a startup-only check
	// is correct when it fires and silent when it does not.
	if !encryptionEnabled {
		if n, err := st.CountSignedWebhooks(ctx); err != nil {
			log.Warn("could not check for signed webhooks at startup", "err", err)
		} else if n > 0 {
			log.Warn("webhook signing secrets are stored in plaintext",
				"signed_webhooks", n,
				"reason", "SWITCHBOARD_SECRET_ENCRYPTION_KEY is empty",
				"impact", "anyone who can read the database can read the HMAC signing secrets",
				"fix", "set SWITCHBOARD_SECRET_ENCRYPTION_KEY to a 32-byte key (openssl rand -base64 32)",
			)
		}
	}
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
	// Enable the SPEC-0013 Personas view only when the capability flag is on (ADR-0023: personas
	// are an advanced surface, hidden by default — the flag flip brings the rail entry, the
	// /personas routes, and the vend wizard's persona slot back).
	// Governing: SPEC-0013 REQ "Personas View" (capability-gated), ADR-0023 REQ "Feature Flags
	// Hide Advanced Surfaces".
	webh.SetPersonasEnabled(cfg.PersonasEnabled)
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
	// ADR-0023: the A2UI resource surface is an advanced capability — registered on live sessions
	// only when the flag is on; a default deployment's tools/list and resources/list never show it.
	mcph.SetA2UIEnabled(cfg.A2UIEnabled)
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
	icfg := ingest.Config{}.Normalized()
	ing := ingest.New(st, hub, log, icfg)
	// Ephemeral received-lane instrumentation (SPEC-0015 REQ "Patch Panel Board"): the receivers
	// report in-flight deliveries — arrival, redacted rejection, dedup collapse — so the board's
	// received lane renders the moment of verification live. SSE-only; nothing new is persisted,
	// and the SPEC-0001 rejection doctrine is unchanged.
	ing.SetInstrument(webh)

	r := newRouter(routerDeps{
		st:    st,
		cfg:   cfg,
		authr: authr,
		webh:  webh,
		ing:   ing,
		mcp:   mcph,
		// The A2A task surface (ADR-0021, SPEC-0018) authenticates against the SAME vended-endpoint
		// credential the MCP surface uses — *store.Store satisfies both EndpointStore interfaces, so an
		// A2A call and an MCP call resolve a credential identically.
		a2a:     a2a.New(st, log),
		oauth:   oauthsrv.New(st, cfg.BaseURL, log),
		friends: newFriendIntake(st, authr, log),
		ping:    pool.Ping,
		log:     log,
	})
	// (The operator API is mounted inside newRouter — the route table's single owner.)

	// The reaper also enforces vend-time credential lifetimes: an endpoint whose expires_at has
	// passed is flipped to revoked in the store and its live MCP sessions are torn down through the
	// SAME CloseEndpointSessions path the web UI's revoke uses — expiry IS revocation, not a
	// parallel lifecycle. Governing: SPEC-0016 REQ "Credential Lifetime", ADR-0019.
	go reaper(ctx, st, log, mcph.CloseEndpointSessions, mcph.PublishTodoReady, reapInterval)
	// Retention pruner: the periodic task SPEC-0004 mandates so events and terminal todos cannot
	// grow unbounded. Same lifecycle pattern as the reaper — context-managed, exits on shutdown.
	go pruner(ctx, st, log, pruneInterval)
	// todo_ready LISTEN loop (SPEC-0004 "In-Database Wakeups via LISTEN/NOTIFY"): the consumer
	// for the pg_notify the store already emits on every committed enqueue. Each notification
	// nudges the web SSE hub (count regions re-render from the database) and re-rings the MCP
	// channel doorbell for push-eligible pending todos owned by the endpoint the notification
	// names — so wakeups no longer depend on this process's HTTP-path store hooks alone.
	// Context-managed like the reaper; reconnects with backoff inside (listen.go).
	//
	// The payload is "<endpoint_id>:<queue>" (store.TodoReadyPayload, ADR-0022). nudgeDoorbells
	// parses it and scopes its read to that endpoint. The web SSE nudge wants only the queue name:
	// its count regions re-render from the database under the viewing human's own session scope, so
	// it is told WHICH queue moved, never whose todo moved.
	go listenTodoReady(ctx, cfg.DatabaseURL, log, func(nctx context.Context, payload string) {
		queue := payload
		if _, after, ok := strings.Cut(payload, ":"); ok {
			queue = after
		}
		webh.PublishQueueNudge(queue)
		nudgeDoorbells(nctx, st, doorbells, mcph.PublishTodoReady, payload, log)
	})

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
	return nil
}

// routerDeps carries the wired components newRouter assembles into the HTTP surface. Extracted from
// Run so tests can build the REAL route table (auth grouping included) without a database.
type routerDeps struct {
	st      *store.Store
	cfg     config.Config // capability gates (ADR-0023): personas / A2A / A2UI / API token
	authr   *auth.Authenticator
	webh    *web.Handler
	ing     *ingest.Ingest
	mcp     *mcpsrv.Handler             // the Run-wired MCP handler (doorbell + revocation hooks attached)
	a2a     *a2a.Handler                // the A2A JSON-RPC task surface (ADR-0021, SPEC-0018); same vended-endpoint auth as MCP
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
		// Agent self-managed webhooks (SPEC-0006): the ingest_url create_webhook/rotate_webhook hand
		// back. The unguessable 128-bit path token routes to exactly one webhook; an unknown token
		// 404s. Delivery → dedup → todo. Governing: ADR-0012.
		wr.Post("/webhooks/w/{token}", d.ing.SelfManaged)
	})
	r.Post("/dev/todos", d.ing.DevCreateTodo)

	// Public A2A Agent Card endpoint (ADR-0009; SPEC-0009). A2A-flag-gated (ADR-0023: the whole
	// A2A surface is advanced and hidden unless SWITCHBOARD_A2A=1). This is a DELIBERATE public route — the
	// only one in the personas capability — registered outside auth.RequireHuman: A2A discovery
	// requires peers to read a persona's card before any friendship exists, and the card grants
	// nothing, exposing only owner-approved discovery metadata (name, description, derived skills,
	// owner display name). It is served ONLY for personas the owner marked discoverable; every other
	// slug 404s without leaking existence. GET-only (a state-changing method 405s at the router), per
	// the read-only requirement, with a per-IP throttle and a default-src 'none' CSP set in the
	// handler. Governing: SPEC-0009 REQ "Well-Known Card Endpoint", REQ "Discoverability Is
	// Owner-Controlled", "Security Requirements → Authentication / Rate Limiting".
	if d.cfg.A2AEnabled {
		r.With(cardRL.middleware).Get("/a/{persona_id}/.well-known/agent-card.json", d.webh.AgentCard)
	}

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
		or.Get(oauthsrv.ProtectedResourcePrefix+"/api", d.oauth.OperatorResourceMetadata)
		or.With(maxBytes(64<<10)).Post(oauthsrv.RegisterPath, d.oauth.Register)
		// The token endpoint (SPEC-0016 REQ "Token Issuance And Refresh") is public like the rest of
		// the AS surface: clients are public (no client secret), so the proof is PKCE possession on
		// the code grant and the rotating refresh token on the refresh grant — never a session. Same
		// per-IP throttle, same 64 KiB bound (a token request is a handful of short form fields).
		or.With(maxBytes(64<<10)).Post(oauthsrv.TokenPath, d.oauth.Token)
	})

	// Vended MCP endpoints over Streamable HTTP (ADR-0017; SPEC-0014). Bearer auth, per-endpoint
	// rate limit, and the 1 MiB body cap all live inside the package's own middleware stack.
	// The handler is Run-wired (doorbell + revocation hooks) and passed in — never constructed here.
	r.Mount("/mcp", d.mcp.Routes())

	// The operator API (ADR-0023 registration-vends-everything) rides the same OAuth model as
	// everything else: its bearer is an operator OAuth grant (resource = base + "/api") resolved
	// to the signed-in human. No static token — the CLI performs the OAuth flow gh-style.
	// Governing: ADR-0023 REQ "Registration Vends the Whole Happy Path"; ADR-0019.
	r.Mount("/api/v1", newAPIHandler(d.st, d.cfg.BaseURL, d.log, apiRevokeHook(d)).Routes())

	// Native A2A task RPC surface (ADR-0021; SPEC-0018) mounted per vended endpoint at
	// /a2a/{endpoint}. A2A-flag-gated (ADR-0023): hidden unless SWITCHBOARD_A2A=1. It is a SECOND wire protocol over the same authorized relationship the MCP
	// surface serves: the same bearer credential, resolved the same way, gates both — an A2A caller
	// without a valid vended-endpoint credential is rejected identically to an unauthenticated MCP
	// call. Bearer auth, the 256 KiB body cap, and the security headers all live inside the package's
	// own middleware stack. Governing: SPEC-0018 REQ "SendMessage Requires a Vended Endpoint".
	if d.cfg.A2AEnabled {
		r.Mount("/a2a", d.a2a.Routes())
	}

	// A2A friend-request intake (ADR-0010; SPEC-0010). A2A-flag-gated (ADR-0023): hidden unless
	// SWITCHBOARD_A2A=1. Inbound only, and deliberately NOT
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
	if d.cfg.A2AEnabled {
		r.Group(func(fr chi.Router) {
			fr.Use(friendRL.middleware, maxBytes(64<<10))
			fr.Post("/a2a/friend-requests", d.friends.Intake)
		})
	}

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

	// GET / is the one dual-mode surface. auth.LoadHuman injects the human when a live session is
	// present but NEVER redirects, so the Root handler renders the operator Board for an
	// authenticated human and the public marketing Home page for a logged-out visitor — the
	// homepage, not a bare bounce to /login. Alongside /login this is the only web route reachable
	// without a session, and it exposes nothing sensitive: the Home page is static, and an
	// authenticated Board render still relies on the human LoadHuman just injected. Every data and
	// mutation route stays behind RequireHuman in the group below.
	// Governing: SPEC-0012 REQ "Screen Set and Routes", REQ "Authentication Boundary" (the human
	// surface is session-gated; / adds a public landing face without opening any data route).
	r.With(d.authr.LoadHuman).Get("/", d.webh.Root)

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
		// Todos view: the durable-queue table + detail drawer (SPEC-0013). GET /todos/{id} serves the
		// drawer fragment (HTMX) or a standalone page (deep link / no-JS fallback).
		pr.Get("/todos", d.webh.Todos)
		pr.Get("/todos/{id}", d.webh.TodoDrawer)
		// Endpoints view + the vend wizard (SPEC-0015 REQ "Endpoints View And Vend Wizard", REQ
		// "Wizard Interaction Pattern"). The retired SPEC-0012 /agents screens 303-redirect here.
		// GET /endpoints/vend starts the wizard (mints server-side step state, 303 → the first step);
		// GET/POST /endpoints/vend/{step} are the routed step pages (agent → queues → verbs →
		// lifetime → confirm) — the confirm POST is the mint. POST /endpoints/vend remains the direct
		// single-form mint path (same executeVend, same validation gates).
		pr.Get("/endpoints", d.webh.Endpoints)
		pr.Get("/endpoints/quick", d.webh.QuickVendStart)
		pr.Post("/endpoints/quick", d.webh.QuickVendSubmit)
		pr.Get("/endpoints/vend", d.webh.VendStart)
		pr.Get("/endpoints/vend/persona", d.webh.VendLegacyPersonaRedirect)
		pr.Get("/endpoints/vend/{step}", d.webh.VendStep)
		pr.Post("/endpoints/vend/{step}", d.webh.VendStepSubmit)
		pr.Post("/endpoints/vend", d.webh.Vend)
		pr.Get("/agents", d.webh.AgentsRedirect)
		pr.Get("/agents/{id}", d.webh.AgentsRedirect)
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
		// OAuth consent (SPEC-0016 REQ "Authorization Code Flow With Consent"): GET renders the
		// "authorize access" screen, its POST records the decision (approve mints the single-use
		// PKCE-bound code; deny returns the standard error). DELIBERATELY inside the RequireHuman
		// group — consent is the flow's human gate, so an anonymous authorize request is bounced
		// through login first (scenario "Human absent") — with CSRF on the decision POST and the
		// shared human-surface limiter, like every other session mutation. Governing: ADR-0019.
		pr.Get(oauthsrv.AuthorizePath, d.webh.OAuthAuthorize)
		pr.Post(oauthsrv.AuthorizePath, d.webh.OAuthDecision)
		// Revocation is irreversible, so it confirms on a full page first (SPEC-0015 REQ "Wizard
		// Interaction Pattern"): GET renders the confirm, the POST from that page executes the kill.
		pr.Get("/endpoints/{id}/revoke", d.webh.RevokeConfirm)
		pr.Post("/endpoints/{id}/revoke", d.webh.Revoke)
		// Permanently delete a revoked endpoint's card (SPEC-0007 REQ "Permanent Deletion of Revoked
		// Endpoints"). Store constrains to state='revoked' + ownership; active endpoints must be revoked
		// first. CSRF arrives via the layout hx-headers / hidden field; the group's RequireCSRF validates.
		pr.Post("/endpoints/{id}/delete", d.webh.DeleteEndpoint)
		// Friends view + approval flow (SPEC-0015 REQ "Friends View And Approval Flow"; SPEC-0010
		// approval-is-vend). The handlers 404 until the friending capability is enabled (capability
		// gating lives in the handler, so the routes stay classified session-gated for the route-table
		// baseline). Approve and revoke follow the full-page confirm pattern (SPEC-0015 REQ "Wizard
		// Interaction Pattern"): GET renders the confirm page — the approve page presents the scoped
		// endpoint approval mints — and the POST from that page executes. Approve mints a scoped
		// endpoint onto a target-OWNED agent; CSRF arrives via the layout hx-headers.
		pr.Get("/friends", d.webh.Friends)
		pr.Get("/friends/new", d.webh.AddFriendModal)
		pr.Get("/friends/resolve", d.webh.ResolveFriendHandle)
		pr.Post("/friends", d.webh.AddFriend)
		pr.Get("/friends/{id}/approve", d.webh.ApproveFriendPage)
		pr.Post("/friends/{id}/approve", d.webh.ApproveFriend)
		pr.Post("/friends/{id}/decline", d.webh.DeclineFriend)
		pr.Post("/friends/{id}/withdraw", d.webh.WithdrawFriend)
		pr.Get("/friends/{id}/revoke", d.webh.RevokeFriendPage)
		pr.Post("/friends/{id}/revoke", d.webh.RevokeFriend)
		pr.Post("/friends/{id}/unblock", d.webh.UnblockFriend)
		// Personas view + wizard (SPEC-0015 REQ "Personas View And Wizard", REQ "Wizard Interaction
		// Pattern"). Capability-gated inside the handler: while the personas capability is disabled
		// these 404 (hidden-not-broken); the routes stay session- and CSRF-gated like every other web
		// mutation. GET /personas/wizard starts the create wizard (mints server-side step state, 303
		// → the first step); GET /personas/{id}/edit starts the edit wizard seeded from the persona;
		// GET/POST /personas/wizard/{step} are the routed step pages (identity → scope → publish) —
		// the publish POST is the save. POST /personas/wizard/preview is the HTMX live A2A card
		// preview over the UNSAVED draft (nothing persisted). POST /personas and /personas/{id}
		// remain the direct single-form create/update paths (same store validation gates).
		pr.Get("/personas", d.webh.Personas)
		pr.Get("/personas/wizard", d.webh.PersonaWizardStart)
		pr.Get("/personas/wizard/{step}", d.webh.PersonaWizardStep)
		pr.Post("/personas/wizard/preview", d.webh.PersonaCardPreview)
		pr.Post("/personas/wizard/{step}", d.webh.PersonaWizardStepSubmit)
		pr.Get("/personas/{id}/edit", d.webh.PersonaWizardEdit)
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

// apiRevokeHook gives the operator API the same session teardown the web UI's revoke and the expiry
// reaper use. Nil when there is no MCP handler wired (route-table tests build the router without
// one), in which case revocation still commits and only the live-stream teardown is skipped.
func apiRevokeHook(d routerDeps) func(string) {
	if d.mcp == nil {
		return nil
	}
	return d.mcp.CloseEndpointSessions
}

// secureHeaders sets defensive response headers on every route (SPEC-0001/0005/0006/0007/0008/0012).
// The policy itself and its rationale live on web.ContentSecurityPolicy, because the OAuth consent
// screen widens one of its directives per-response (web.CSPAllowingFormActionTo) and two copies of a
// policy drift. A handler that needs a different policy simply re-Sets the header before writing.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", web.ContentSecurityPolicy)
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
	RingUnclaimed(ctx context.Context) ([]store.Todo, error)
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
func reaper(ctx context.Context, st reapStore, log *slog.Logger, closeSessions func(endpointID string), ring func(store.Todo), interval time.Duration) {
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
			// Doorbell heartbeat: repeat the push for pending todos nobody picked up. A doorbell
			// that arrived while every worker was mid-turn, or was dropped by a transport fault,
			// was previously never repeated — so the todo sat pending forever with nothing to
			// surface it. Bounded and backed off in the store; ring publishes through the same
			// endpoint-scoped path a fresh delivery uses.
			if due, err := st.RingUnclaimed(ctx); err != nil {
				log.Warn("doorbell heartbeat", "err", err)
			} else if len(due) > 0 && ring != nil {
				for _, t := range due {
					ring(t)
				}
				log.Info("re-rang unclaimed todos", "count", len(due))
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
