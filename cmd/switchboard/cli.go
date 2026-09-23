package main

// Operator CLI Dispatch
//
// One binary, two roles: `switchboard serve` (the default) runs the service; every other verb is
// the operator CLI over /api/v1. Each verb owns a flag.FlagSet, so -h works everywhere, flags may
// come before or after the positional arguments, and an unknown flag is an error rather than a
// silently ignored word. Usage mistakes exit 2 (the sysexits convention); runtime failures exit 1.
//
// Everything the verbs touch — stdout/stderr, the environment, the browser opener, the HTTP
// client, the credentials file, the clock — is an injected edge on cli, so the whole surface is
// exercised in tests without a terminal, a browser, or a real deployment.
//
// @joestump-agent 09/03/2026 - Reworked from the hand-rolled argv loop: per-verb flag sets,
// help/version, --json output, a status verb, an idempotent logout, and a per-OS browser opener.
//
// @joestump-agent 09/04/2026 - Grouped the verbs under the resource they manage: `endpoint list`,
// `endpoint vend`, `endpoint revoke`, `agent list`. A flat verb table stops scaling the moment a
// second thing can be revoked — `revoke` alone would have to mean endpoints by fiat, and the next
// resource has nowhere to go. The pre-grouping spellings (`endpoints`, `agents`, `vend`) still
// work as hidden aliases so anything already scripted keeps running.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/buildinfo"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// cli carries the injectable edges of the operator CLI.
type cli struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	getenv         func(string) string
	openBrowser    func(url string) error
	http           *http.Client
	serve          func(c *cli, args []string) int
	now            func() time.Time
	// loginTimeout bounds the wait for the browser to come back with the authorization code.
	loginTimeout time.Duration
	// readFile resolves a --payload @file argument; an injected edge like the rest, so tests
	// exercise the verb without touching the real filesystem.
	readFile func(name string) ([]byte, error)

	verbUsage map[*flag.FlagSet]verbUsage // per-verb synopsis, filled by flagSet
}

func newCLI() *cli {
	return &cli{
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		stdin:        os.Stdin,
		getenv:       os.Getenv,
		openBrowser:  openBrowser,
		http:         &http.Client{Timeout: 30 * time.Second},
		serve:        runServe,
		now:          time.Now,
		loginTimeout: 5 * time.Minute,
		readFile:     os.ReadFile,
		verbUsage:    map[*flag.FlagSet]verbUsage{},
	}
}

// command is one verb of the CLI, or — when subs is non-empty — a resource that groups them.
type command struct {
	name    string
	args    string // the argument synopsis shown in usage
	summary string
	run     func(c *cli, args []string) int
	// subs are the verbs of a resource group ("endpoint list", "endpoint revoke"). A group has no
	// run of its own: naming it without a verb is a usage error listing what it does support.
	subs []command
	// hidden keeps a command working without advertising it. Used for the pre-grouping aliases,
	// which stay for compatibility but should not teach the old shape to anyone reading help.
	hidden bool
}

// commands is the command table in usage order. serve is listed first because it is the default;
// the resource groups follow, then the session and meta verbs that belong to no resource.
func commands() []command {
	return []command{
		{name: "serve", summary: "run the service (the default; configured from the environment)",
			run: func(c *cli, args []string) int { return c.serve(c, args) }},
		{name: "endpoint", args: "<verb>", summary: "manage vended endpoints", subs: []command{
			{name: "list", summary: "list the vended endpoints you own", run: cmdEndpoints},
			{name: "vend", args: "NAME", summary: "register an agent and vend its endpoint in one call", run: cmdVend},
			{name: "revoke", args: "SLUG|ID", summary: "kill an endpoint: its credential stops working immediately", run: cmdEndpointRevoke},
		}},
		{name: "agent", args: "<verb>", summary: "manage registered agents", subs: []command{
			{name: "list", summary: "list your registered agents", run: cmdAgents},
		}},
		{name: "todo", args: "<verb>", summary: "hand work to your agents", subs: []command{
			{name: "push", args: "ENDPOINT TITLE", summary: "mint a todo on an endpoint you own and ring its doorbell", run: cmdTodoPush},
		}},
		{name: "login", args: "[URL]", summary: "sign in to a deployment over OAuth (opens your browser)", run: cmdLogin},
		{name: "status", summary: "show where you are logged in and whether the credentials are live", run: cmdStatus},
		{name: "logout", summary: "forget the local credentials", run: cmdLogout},
		{name: "version", summary: "print the build version", run: cmdVersion},
		{name: "help", args: "[command]", summary: "show help for a command", run: cmdHelp},

		// Pre-grouping spellings. Kept working, deliberately unlisted.
		{name: "endpoints", summary: "list the vended endpoints you own", run: cmdEndpoints, hidden: true},
		{name: "agents", summary: "list your registered agents", run: cmdAgents, hidden: true},
		{name: "vend", args: "NAME", summary: "register an agent and vend its endpoint in one call", run: cmdVend, hidden: true},
	}
}

// lookup resolves a command by name, groups included.
func lookup(name string) (command, bool) {
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

// runGroup dispatches "<resource> <verb>". Naming a resource with no verb, or with one it does not
// have, lists the verbs it does — the shape a person is most likely to need at that moment.
func (c *cli) runGroup(group command, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(c.stderr, "switchboard %s: no verb given\n\n", group.name)
		c.groupUsage(group, c.stderr)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		c.groupUsage(group, c.stdout)
		return exitOK
	}
	for _, sub := range group.subs {
		if sub.name == args[0] {
			return sub.run(c, args[1:])
		}
	}
	fmt.Fprintf(c.stderr, "switchboard %s: unknown verb %q\n\n", group.name, args[0])
	c.groupUsage(group, c.stderr)
	return exitUsage
}

func (c *cli) groupUsage(group command, w io.Writer) {
	fmt.Fprintf(w, "usage: switchboard %s <verb> [flags]\n\n%s\n\nverbs:\n", group.name, group.summary)
	for _, sub := range group.subs {
		synopsis := sub.name
		if sub.args != "" {
			synopsis += " " + sub.args
		}
		fmt.Fprintf(w, "  switchboard %s %-18s %s\n", group.name, synopsis, sub.summary)
	}
	fmt.Fprintf(w, "\nRun \"switchboard %s <verb> -h\" for that verb's flags.\n", group.name)
}

// run dispatches argv (without the program name) and returns the process exit code.
func (c *cli) run(args []string) int {
	if len(args) == 0 {
		return c.serve(c, nil)
	}
	name := args[0]
	switch name {
	case "-h", "--help":
		return cmdHelp(c, args[1:])
	case "-v", "--version":
		return cmdVersion(c, nil)
	}
	if strings.HasPrefix(name, "-") {
		fmt.Fprintf(c.stderr, "switchboard: unknown flag %q\n\n", name)
		c.usage(c.stderr)
		return exitUsage
	}
	if cmd, ok := lookup(name); ok {
		if len(cmd.subs) > 0 {
			return c.runGroup(cmd, args[1:])
		}
		return cmd.run(c, args[1:])
	}
	fmt.Fprintf(c.stderr, "switchboard: unknown command %q\n\n", name)
	c.usage(c.stderr)
	return exitUsage
}

// usage prints the top-level synopsis.
func (c *cli) usage(w io.Writer) {
	fmt.Fprintln(w, "switchboard — the switchboard server and its operator CLI")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage:")
	for _, cmd := range commands() {
		if cmd.hidden {
			continue
		}
		synopsis := cmd.name
		if cmd.args != "" {
			synopsis += " " + cmd.args
		}
		fmt.Fprintf(w, "  switchboard %-22s %s\n", synopsis, cmd.summary)
		for _, sub := range cmd.subs {
			subSyn := cmd.name + " " + sub.name
			if sub.args != "" {
				subSyn += " " + sub.args
			}
			fmt.Fprintf(w, "  switchboard %-22s %s\n", subSyn, sub.summary)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, `Run "switchboard <command> -h" for that command's flags.`)
}

func cmdHelp(c *cli, args []string) int {
	if len(args) == 0 {
		c.usage(c.stdout)
		return exitOK
	}
	cmd, ok := lookup(args[0])
	if !ok {
		fmt.Fprintf(c.stderr, "switchboard: unknown command %q\n\n", args[0])
		c.usage(c.stderr)
		return exitUsage
	}
	if len(cmd.subs) == 0 {
		return cmd.run(c, []string{"-h"})
	}
	if len(args) == 1 {
		c.groupUsage(cmd, c.stdout)
		return exitOK
	}
	for _, sub := range cmd.subs {
		if sub.name == args[1] {
			return sub.run(c, []string{"-h"})
		}
	}
	fmt.Fprintf(c.stderr, "switchboard %s: unknown verb %q\n\n", cmd.name, args[1])
	c.groupUsage(cmd, c.stderr)
	return exitUsage
}

func cmdVersion(c *cli, _ []string) int {
	fmt.Fprintf(c.stdout, "switchboard %s\n", buildinfo.Get().Version)
	return exitOK
}

// --- per-verb flag handling ---

// flagSet builds a verb's flag set. Usage output is silenced on the set itself and printed by
// parseArgs, so help lands on stdout and mistakes land on stderr.
func (c *cli) flagSet(name, args, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() {}
	c.verbUsage[fs] = verbUsage{args: args, summary: summary}
	return fs
}

type verbUsage struct{ args, summary string }

// parseArgs parses args against fs with flags allowed on either side of the positional
// arguments (`vend NAME --queue Q` and `vend --queue Q NAME` are the same command); a bare "--"
// ends flag parsing. It returns the positional arguments and, when parsing did not succeed, the
// exit code to return: 0 after printing help for -h, 2 after a usage error.
func (c *cli) parseArgs(fs *flag.FlagSet, args []string) (positional []string, exit int, ok bool) {
	var tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				c.printVerbUsage(fs, c.stdout)
				return nil, exitOK, false
			}
			// flag already wrote the specific complaint to stderr.
			fmt.Fprintln(c.stderr)
			c.printVerbUsage(fs, c.stderr)
			return nil, exitUsage, false
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	return append(positional, tail...), exitOK, true
}

// usageError reports a usage mistake the flag package cannot see (a missing or extra positional).
func (c *cli) usageError(fs *flag.FlagSet, msg string) int {
	fmt.Fprintf(c.stderr, "switchboard %s: %s\n\n", fs.Name(), msg)
	c.printVerbUsage(fs, c.stderr)
	return exitUsage
}

func (c *cli) printVerbUsage(fs *flag.FlagSet, w io.Writer) {
	u := c.verbUsage[fs]
	synopsis := "switchboard " + fs.Name()
	hasFlags := false
	fs.VisitAll(func(*flag.Flag) { hasFlags = true })
	if hasFlags {
		synopsis += " [flags]"
	}
	if u.args != "" {
		synopsis += " " + u.args
	}
	fmt.Fprintf(w, "usage: %s\n\n%s\n", synopsis, u.summary)
	if hasFlags {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "flags:")
		out := fs.Output()
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(out)
	}
}

// confirm reads one line and reports whether it is an explicit yes. Anything else — "n", empty,
// EOF from a non-interactive stdin — is no, so a destructive verb piped input it did not expect
// declines rather than proceeding. Scripts pass -y instead.
func (c *cli) confirm() bool {
	if c.stdin == nil {
		return false
	}
	line, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// fail reports a runtime failure in the conventional "program: error" shape.
func (c *cli) fail(err error) int {
	fmt.Fprintln(c.stderr, "switchboard:", err)
	return exitFailure
}

// --- the browser ---

// openBrowser opens url in the operator's browser: $BROWSER when set (the gh/xdg convention),
// else the platform opener. Failure is not fatal — login prints the URL either way.
func openBrowser(url string) error {
	if b := os.Getenv("BROWSER"); b != "" {
		return exec.Command(b, url).Start()
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
