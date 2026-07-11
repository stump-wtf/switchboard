// Command switchboard is the entrypoint for the Switchboard service.
//
//	switchboard serve     Run the central service: webhooks → verify → durable todo queue,
//	                      the human web UI (OIDC login + vend), and the vended MCP endpoints
//	                      served exclusively over Streamable HTTP (ADR-0017; SPEC-0014).
//
// Agents connect with nothing but the vended URL + bearer credential — there is no local binary,
// subprocess, or stdio adapter. The design record is the source of truth:
// https://joestump.github.io/switchboard/
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/server"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		runServe()
	default:
		fmt.Fprintf(os.Stderr, "usage: switchboard [serve]\n")
		os.Exit(2)
	}
}

func runServe() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.FromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, cfg, log); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
