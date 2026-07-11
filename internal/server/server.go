// Package server wires the switchboard HTTP surface: webhooks, the human web UI, the vended agent
// API, static assets, and a background lease reaper — one chi router, one PostgreSQL layer.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	switchboard "github.com/joestump/switchboard"
	"github.com/joestump/switchboard/internal/adapter/runner"
	"github.com/joestump/switchboard/internal/agentapi"
	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/ingest"
	mcpsrv "github.com/joestump/switchboard/internal/mcp"
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

	st := store.New(pool)
	hub := agentapi.NewHub()
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
	// HTTP Stream", SPEC-0011 (push semantics; queue stays the ledger).
	st.SetTodoDoorbellHook(mcph.PublishTodoReady)
	// Revoking an endpoint in the web UI also closes its live notification streams promptly
	// (SPEC-0014 scenario "Revocation closes live streams").
	webh.SetEndpointRevokedHook(mcph.CloseEndpointSessions)
	api := agentapi.New(st, hub, log)
	// Generic (token/open) providers are explicit operator opt-in via SWITCHBOARD_GENERIC_PROVIDERS;
	// a malformed or invalid-mode config fails startup loudly rather than silently opening an
	// endpoint. Governing: SPEC-0001 REQ "Explicit Open Trust Mode".
	generic, err := ingest.ParseGenericProviders(os.Getenv("SWITCHBOARD_GENERIC_PROVIDERS"))
	if err != nil {
		return err
	}
	ing := ingest.New(st, hub, log, ingest.Config{
		GitHubSecret: os.Getenv("SWITCHBOARD_GITHUB_SECRET"),
		GitHubQueue:  os.Getenv("SWITCHBOARD_GITHUB_QUEUE"),
		StripeSecret: os.Getenv("SWITCHBOARD_STRIPE_SECRET"),
		StripeQueue:  os.Getenv("SWITCHBOARD_STRIPE_QUEUE"),
		SlackSecret:  os.Getenv("SWITCHBOARD_SLACK_SECRET"),
		SlackQueue:   os.Getenv("SWITCHBOARD_SLACK_QUEUE"),
		Generic:      generic,
		DevLogin:     cfg.DevLogin,
	})
	// list_providers (SPEC-0005) serves this snapshot: presence/absence classification only, never
	// the secret material. Governing: SPEC-0005 REQ "Provider Enumeration Without Secrets".
	mcph.SetProviders(providerStatuses(
		os.Getenv("SWITCHBOARD_GITHUB_SECRET") != "",
		os.Getenv("SWITCHBOARD_STRIPE_SECRET") != "",
		os.Getenv("SWITCHBOARD_SLACK_SECRET") != "",
		generic))

	r := newRouter(routerDeps{
		st:      st,
		authr:   authr,
		webh:    webh,
		api:     api,
		ing:     ing,
		mcp:     mcph,
		friends: newFriendIntake(st, authr, log),
		ping:    pool.Ping,
		log:     log,
	})

	go reaper(ctx, st, log)

	// Pull-adapter poll loops (ADR-0014; SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety"):
	// each registered adapter runs as a context-managed worker — enabled-flag gated, backing off on
	// broker errors, health-stamped on the adapters table — and shuts down cleanly with the server.
	// The full seam exists (redis transports + adapter.StoreSink enqueue coupling); concrete
	// instances attach here (runner.Add) once operator broker config — which streams/lists to
	// consume, adapters.config jsonb vs. environment — is resolved (a SPEC-0002 design open
	// question). Until then the runner supervises an empty registry.
	adapters := runner.New(st, log, runner.Options{})
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
	api     *agentapi.API
	ing     *ingest.Ingest
	mcp     *mcpsrv.Handler             // the Run-wired MCP handler (doorbell + revocation hooks attached)
	friends *friendIntake               // A2A friend-request intake (OIDC-provenance authenticated)
	ping    func(context.Context) error // /healthz DB probe
	log     *slog.Logger
}

// newRouter builds the full switchboard route table. Route grouping is the security baseline:
// everything not explicitly registered as a public route sits behind bearer auth (/agent, /mcp) or
// auth.RequireHuman (the web UI, including the Board landing at GET /).
// Governing: SPEC-0012 REQ "Screen Set and Routes", REQ "Authentication Boundary";
// SPEC-0013 REQ "Information Architecture and Navigation".
func newRouter(d routerDeps) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// Governing: SPEC-0001/0005/0006/0007/0008/0012 REQ "Security headers on all responses". Applied
	// as the outermost app middleware so every surface (webhook, /agent, MCP, web, errors) carries them.
	r.Use(secureHeaders)

	// Rate limiters (SPEC-0006 webhook self-mgmt MUST, todo drain SHOULD; SPEC-0009 persona card).
	// The agent API (todo drain + webhook self-management verbs) and inbound webhook get IP throttles;
	// the durable queue stays the source of truth, so throttling only bounds abuse, never drops work.
	agentRL := newRateLimiter(20, 40) // ~20 req/s per IP, burst 40 — comfortable for real drain loops
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

	// Vended agent API (bearer-credential auth inside; ADR-0008). 1 MiB body cap + IP rate limit.
	r.With(agentRL.middleware, maxBytes(1<<20)).Mount("/agent", d.api.Routes())

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
	r.Group(func(pr chi.Router) {
		pr.Use(maxBytes(1 << 20))
		pr.Use(d.authr.RequireHuman)
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
// locked to 'self'. frame-ancestors 'none' backs up X-Frame-Options: DENY.
func secureHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"script-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// providerStatuses projects the configured inbound providers into the SPEC-0005 list_providers
// shape. It receives only presence booleans for the signed providers — never the secret values —
// so no secret material can reach the enumeration surface. A built-in signed provider (github,
// stripe, slack) is only enumerated when its secret is actually configured: with no secret the
// route rejects every delivery, so advertising it would misrepresent an unreachable provider as
// part of the inventory. Generic providers already appear only when the operator declares them.
// Queue-family providers join once the Redis adapters register with the runner (SPEC-0002 chain);
// until then the webhook family is the whole inventory.
// Governing: SPEC-0005 REQ "Provider Enumeration Without Secrets" ("for each configured provider").
func providerStatuses(github, stripe, slack bool, generic map[string]ingest.GenericProvider) []mcpsrv.ProviderStatus {
	var out []mcpsrv.ProviderStatus
	builtin := []struct {
		name, path string
		configured bool
	}{
		{"github", "/webhooks/github", github},
		{"stripe", "/webhooks/stripe", stripe},
		{"slack", "/webhooks/slack", slack},
	}
	for _, b := range builtin {
		if !b.configured {
			continue
		}
		out = append(out, mcpsrv.ProviderStatus{
			Name: b.name, Family: "webhook", TrustMode: "signed", Enabled: true,
			SecretStatus: "configured", Path: b.path,
		})
	}
	for _, name := range slices.Sorted(maps.Keys(generic)) {
		gp := generic[name]
		ps := mcpsrv.ProviderStatus{Name: name, Family: "webhook", TrustMode: gp.Mode,
			Path: "/webhooks/generic/" + name}
		switch gp.Mode {
		case "token":
			// A token provider with no token configured is disabled (403s everything) by design, so
			// it is an unreachable route — skip it, matching the built-in "only configured" rule.
			if gp.Token == "" {
				continue
			}
			ps.Enabled = true
			ps.SecretStatus = "configured"
		case "open":
			ps.Enabled = true
			ps.SecretStatus = "none-by-design"
		}
		out = append(out, ps)
	}
	return out
}

// maxBytes caps a request body at n bytes via http.MaxBytesReader, so a read past the limit errors
// (the reader also writes a 413 when the handler surfaces the error) rather than buffering unbounded
// input. Governing: SPEC-0001 (webhook), SPEC-0006 (/agent), SPEC-0012 (web forms) body limits.
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

// reaper periodically requeues (or dead-letters) todos with expired leases — crash safety (ADR-0002).
func reaper(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
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
		}
	}
}
