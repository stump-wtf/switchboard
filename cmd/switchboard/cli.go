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

import (
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
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// cli carries the injectable edges of the operator CLI.
type cli struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	openBrowser    func(url string) error
	http           *http.Client
	serve          func(c *cli, args []string) int
	now            func() time.Time
	// loginTimeout bounds the wait for the browser to come back with the authorization code.
	loginTimeout time.Duration

	verbUsage map[*flag.FlagSet]verbUsage // per-verb synopsis, filled by flagSet
}

func newCLI() *cli {
	return &cli{
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		getenv:       os.Getenv,
		openBrowser:  openBrowser,
		http:         &http.Client{Timeout: 30 * time.Second},
		serve:        runServe,
		now:          time.Now,
		loginTimeout: 5 * time.Minute,
		verbUsage:    map[*flag.FlagSet]verbUsage{},
	}
}

// command is one verb of the CLI.
type command struct {
	name    string
	args    string // the argument synopsis shown in usage
	summary string
	run     func(c *cli, args []string) int
}

// commands is the verb table in usage order. serve is listed first because it is the default.
func commands() []command {
	return []command{
		{"serve", "", "run the service (the default; configured from the environment)", func(c *cli, args []string) int { return c.serve(c, args) }},
		{"login", "[URL]", "sign in to a deployment over OAuth (opens your browser)", cmdLogin},
		{"vend", "NAME", "register an agent and vend its endpoint in one call", cmdVend},
		{"endpoints", "", "list the vended endpoints you own", cmdEndpoints},
		{"agents", "", "list your registered agents", cmdAgents},
		{"status", "", "show where you are logged in and whether the credentials are live", cmdStatus},
		{"logout", "", "forget the local credentials", cmdLogout},
		{"version", "", "print the build version", cmdVersion},
		{"help", "[command]", "show help for a command", cmdHelp},
	}
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
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd.run(c, args[1:])
		}
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
		synopsis := cmd.name
		if cmd.args != "" {
			synopsis += " " + cmd.args
		}
		fmt.Fprintf(w, "  switchboard %-22s %s\n", synopsis, cmd.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, `Run "switchboard <command> -h" for that command's flags.`)
}

func cmdHelp(c *cli, args []string) int {
	if len(args) == 0 {
		c.usage(c.stdout)
		return exitOK
	}
	for _, cmd := range commands() {
		if cmd.name == args[0] {
			return cmd.run(c, []string{"-h"})
		}
	}
	fmt.Fprintf(c.stderr, "switchboard: unknown command %q\n\n", args[0])
	c.usage(c.stderr)
	return exitUsage
}

func cmdVersion(c *cli, _ []string) int {
	fmt.Fprintf(c.stdout, "switchboard %s\n", version)
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
