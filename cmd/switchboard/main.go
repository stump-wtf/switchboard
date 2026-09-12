// Command switchboard is the single switchboard binary: the server AND the operator CLI.
//
//	switchboard [serve]    Run the central service: webhooks → verify → durable todo queue,
//	                       the human web UI (OIDC login + vend), the operator API (/api/v1),
//	                       and the vended MCP endpoints served exclusively over Streamable
//	                       HTTP (ADR-0017; SPEC-0014). The default when no command is given;
//	                       configured entirely from the environment (internal/config).
//	switchboard login      Sign the operator in to a deployment over OAuth (authorization
//	                       code + PKCE, loopback redirect — the gh pattern).
//	switchboard vend       Register an agent and vend its endpoint, queue, and webhook in one call.
//	switchboard endpoints  List the vended endpoints you own.
//	switchboard agents     List your registered agents.
//	switchboard status     Show where you are logged in and whether the credentials are live.
//	switchboard logout     Forget the local operator credentials.
//	switchboard version    Print the build version.
//
// Agents connect with nothing but the vended URL + bearer credential — there is no local
// subprocess or stdio adapter. The operator CLI shares the same OAuth model as everything else
// (ADR-0019/ADR-0023). The design record is the source of truth:
// https://switchboard.stump.wtf/docs/
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/server"
)

// version is stamped at build time (go build -ldflags "-X main.version=…"); "dev" otherwise.
var version = "dev"

func main() {
	// Routing rules evaluate in a re-executed child of this binary (internal/routing/sandbox.go). The
	// hook must run before anything else: the child has no environment, no config, and no business
	// touching the CLI. Governing: SPEC-0020 Security Requirements "Expression sandboxing".
	routing.RunChildIfRequested()
	os.Exit(newCLI().run(os.Args[1:]))
}

// runServe runs the service. It takes no flags: configuration comes from the environment, so the
// only arguments it honors are the help spellings.
func runServe(c *cli, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(c.stdout, serveUsage)
			return exitOK
		}
		fmt.Fprintf(c.stderr, "switchboard serve: unexpected argument %q\n\n%s", args[0], serveUsage)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: config.LogLevel()}))
	cfg := config.FromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, cfg, log); err != nil {
		log.Error("serve", "err", err)
		return exitFailure
	}
	return exitOK
}

const serveUsage = `usage: switchboard serve

Run the switchboard service. serve takes no flags: every setting comes from
the environment (SWITCHBOARD_DATABASE_URL, SWITCHBOARD_BASE_URL,
SWITCHBOARD_LOG_LEVEL,
SWITCHBOARD_ADDR, the SWITCHBOARD_OIDC_* client, the capability flags, …).
See https://switchboard.stump.wtf/docs/ for the full list.
`
