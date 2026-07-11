// Package server wires the switchboard HTTP surface: webhooks, the human web UI, the vended agent
// API, static assets, and a background lease reaper — one chi router, one PostgreSQL layer.
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

	r := newRouter(routerDeps{
		st:    st,
		authr: authr,
		webh:  webh,
		api:   api,
		ing:   ing,
		mcp:   mcph,
		ping:  pool.Ping,
		log:   log,
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
	st    *store.Store
	authr *auth.Authenticator
	webh  *web.Handler
	api   *agentapi.API
	ing   *ingest.Ingest
	mcp   *mcpsrv.Handler             // the Run-wired MCP handler (doorbell + revocation hooks attached)
	ping  func(context.Context) error // /healthz DB probe
	log   *slog.Logger
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
	})
	r.Post("/dev/todos", d.ing.DevCreateTodo)

	// Vended agent API (bearer-credential auth inside; ADR-0008). 1 MiB body cap + IP rate limit.
	r.With(agentRL.middleware, maxBytes(1<<20)).Mount("/agent", d.api.Routes())

	// Vended MCP endpoints over Streamable HTTP (ADR-0017; SPEC-0014). Bearer auth, per-endpoint
	// rate limit, and the 1 MiB body cap all live inside the package's own middleware stack.
	// The handler is Run-wired (doorbell + revocation hooks) and passed in — never constructed here.
	r.Mount("/mcp", d.mcp.Routes())

	// Auth (OIDC RP against Pocket ID; ADR-0011). The login screen is the ONLY public web page
	// (SPEC-0012 "Security Requirements → Authentication"; REQ "Screen Set and Routes": public GET /login).
	r.Get("/login", d.webh.Login)
	r.Get("/auth/login", d.authr.Login)
	r.Get("/auth/callback", d.authr.Callback)
	// Dev-login is config-gated (404 unless SWITCHBOARD_DEV_LOGIN) and, like every auth form POST,
	// bounds its body at 64 KiB. Governing: SPEC-0008 REQ "Development Login Guard",
	// REQ "Request Body Size Limits".
	r.With(maxBytes(64<<10)).Post("/auth/dev-login", d.authr.DevLogin)

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
		pr.Get("/agents", d.webh.Dashboard)
		// Live updates stream (SPEC-0012): session-authenticated SSE; per-session stream cap inside.
		pr.Get("/events", d.webh.Events)
		// Operator claim from the Board feed (SPEC-0013 endpoints table). CSRF arrives via the
		// layout's hx-headers token; the group's RequireCSRF validates it.
		pr.Post("/todos/{id}/claim", d.webh.ClaimTodo)
		pr.Post("/agents", d.webh.CreateAgent)
		pr.Get("/agents/{id}", d.webh.Agent)
		pr.Post("/agents/{id}/vend", d.webh.Vend)
		pr.Post("/endpoints/{id}/revoke", d.webh.Revoke)
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
