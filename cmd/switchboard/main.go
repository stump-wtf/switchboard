// Command switchboard is the entrypoint for the Switchboard service — an MCP server that
// receives, verifies, and routes inbound events into a durable todo work-queue that agents drain.
//
// This is the skeleton the code session grows into the full pipeline. The design record is the
// source of truth: https://joestump.github.io/switchboard/ (docs/adr/ + docs/specs/).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joestump/switchboard/internal/config"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.FromEnv()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// TODO(code-session): mount /webhooks/{provider} + /webhooks/generic/{name} (ADR-003/014),
	// the /events SSE stream + web UI (ADR-001), and the MCP server (ADR-005/008).

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("switchboard listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown error", "err", err)
	}
}
