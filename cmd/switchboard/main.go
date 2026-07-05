// Command switchboard is the entrypoint for the Switchboard service.
//
//	switchboard serve     Run the central service: webhooks → verify → durable todo queue,
//	                      the human web UI (OIDC login + vend), and the vended agent API.
//	switchboard channel   Run the local Claude Code Channels stdio adapter: bridges a session to
//	                      central switchboard with a vended credential (SWITCHBOARD_URL + SWITCHBOARD_TOKEN),
//	                      serving the work tools and pushing new todos in as <channel> events.
//
// The design record is the source of truth: https://joestump.github.io/switchboard/
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joestump/switchboard/internal/channel"
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
	case "channel":
		runChannel()
	default:
		fmt.Fprintf(os.Stderr, "usage: switchboard [serve|channel]\n")
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

func runChannel() {
	// stdout is the JSON-RPC pipe to Claude Code — ALL logs go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := channel.Run(ctx, os.Getenv("SWITCHBOARD_URL"), os.Getenv("SWITCHBOARD_TOKEN")); err != nil {
		log.Error("channel", "err", err)
		os.Exit(1)
	}
}
