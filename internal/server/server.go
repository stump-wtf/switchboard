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
	"github.com/joestump/switchboard/internal/agentapi"
	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/ingest"
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
	api := agentapi.New(st, hub, log)
	ing := ingest.New(st, hub, log, os.Getenv("SWITCHBOARD_GITHUB_SECRET"), os.Getenv("SWITCHBOARD_GITHUB_QUEUE"), cfg.DevLogin)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	// Static assets + health.
	staticSub, _ := fs.Sub(switchboard.StaticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})

	// Inbound ingestion (verified per-provider; ADR-0003).
	r.Post("/webhooks/github", ing.GitHub)
	r.Post("/dev/todos", ing.DevCreateTodo)

	// Vended agent API (bearer-credential auth inside; ADR-0008).
	r.Mount("/agent", api.Routes())

	// Auth (OIDC RP against Pocket ID; ADR-0011).
	r.Get("/login", webh.Login)
	r.Get("/auth/login", authr.Login)
	r.Get("/auth/callback", authr.Callback)
	r.Post("/auth/dev-login", authr.DevLogin)
	r.Get("/logout", authr.Logout)

	// Human web UI (requires an authenticated human; ADR-0001/008).
	r.Group(func(pr chi.Router) {
		pr.Use(authr.RequireHuman)
		pr.Get("/", webh.Dashboard)
		pr.Post("/agents", webh.CreateAgent)
		pr.Get("/agents/{id}", webh.Agent)
		pr.Post("/agents/{id}/vend", webh.Vend)
		pr.Post("/endpoints/{id}/revoke", webh.Revoke)
	})

	go reaper(ctx, st, log)

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
