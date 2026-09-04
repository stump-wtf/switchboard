package main

// The Operator Verbs
//
// What each subcommand does against /api/v1, and how it prints. Human output is the default;
// --json prints the API's own response for scripts and agents. The vend reveal prints the
// one-time credential the same way the web wizard does, because it is never retrievable again
// (SPEC-0007) — and it prints the same .mcp.json wiring, straight from the API's mcp_json field.
//
// @joestump-agent 09/03/2026 - Per-verb flag sets, --json, status, and an idempotent logout.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// --- login / logout / status ---

func cmdLogin(c *cli, args []string) int {
	fs := c.flagSet("login", "[URL]", "Sign in to a switchboard deployment over OAuth (opens your browser).\n"+
		"The URL may also come from $SWITCHBOARD_URL, or from the saved credentials when logging in again.")
	noBrowser := fs.Bool("no-browser", false, "print the login URL instead of opening a browser")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	if len(pos) > 1 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	base := ""
	if len(pos) == 1 {
		base = pos[0]
	}
	if base == "" {
		base = c.getenv("SWITCHBOARD_URL")
	}
	if base == "" {
		if saved, err := c.loadCredentials(); err == nil {
			base = saved.BaseURL
		}
	}
	if base == "" {
		return c.usageError(fs, "give the deployment URL (switchboard login https://sb.example.com) or set SWITCHBOARD_URL")
	}
	base, err := normalizeBaseURL(base)
	if err != nil {
		return c.usageError(fs, err.Error())
	}

	opener := c.openBrowser
	if *noBrowser {
		opener = nil
	}
	creds, err := c.login(context.Background(), base, opener)
	if err != nil {
		return c.fail(err)
	}
	if err := c.saveCredentials(creds); err != nil {
		return c.fail(err)
	}
	path, _ := c.credentialsPath()
	fmt.Fprintf(c.stdout, "Logged in to %s\nCredentials saved to %s\n", creds.BaseURL, path)
	return exitOK
}

func cmdLogout(c *cli, args []string) int {
	fs := c.flagSet("logout", "", "Forget the local credentials. The grant itself stays valid until it expires or the\n"+
		"deployment revokes it; logging out only removes the tokens from this machine.")
	if pos, exit, ok := c.parseArgs(fs, args); !ok {
		return exit
	} else if len(pos) > 0 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[0]))
	}
	creds, err := c.loadCredentials()
	if errors.Is(err, errNotLoggedIn) {
		fmt.Fprintln(c.stdout, "Not logged in.")
		return exitOK
	}
	if err := c.removeCredentials(); err != nil {
		return c.fail(err)
	}
	if creds != nil && creds.BaseURL != "" {
		fmt.Fprintf(c.stdout, "Logged out of %s.\n", creds.BaseURL)
	} else {
		fmt.Fprintln(c.stdout, "Logged out.")
	}
	return exitOK
}

func cmdStatus(c *cli, args []string) int {
	fs := c.flagSet("status", "", "Show where you are logged in and whether the local credentials are live.")
	if pos, exit, ok := c.parseArgs(fs, args); !ok {
		return exit
	} else if len(pos) > 0 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[0]))
	}
	path, _ := c.credentialsPath()
	creds, err := c.loadCredentials()
	if err != nil {
		fmt.Fprintf(c.stdout, "Not logged in. Run `switchboard login <URL>` to sign in.\n  credentials  %s\n", path)
		return exitFailure
	}
	fmt.Fprintf(c.stdout, "Logged in to %s\n  credentials  %s\n", creds.BaseURL, path)
	switch {
	case creds.ExpiresAt.IsZero():
		fmt.Fprintln(c.stdout, "  access token no expiry recorded")
	case creds.ExpiresAt.After(c.now()):
		fmt.Fprintf(c.stdout, "  access token expires in %s (refreshes automatically)\n", roundDuration(creds.ExpiresAt.Sub(c.now())))
	default:
		fmt.Fprintf(c.stdout, "  access token expired %s ago (refreshes automatically on the next call)\n", roundDuration(c.now().Sub(creds.ExpiresAt)))
	}
	return exitOK
}

// --- vend ---

// vendResponse is the API's vend document (docs/reference/openapi.yaml VendEndpointOut).
type vendResponse struct {
	AgentName string   `json:"agent_name"`
	Slug      string   `json:"slug"`
	MCPURL    string   `json:"mcp_url"`
	Token     string   `json:"token"`
	Queue     string   `json:"queue"`
	Verbs     []string `json:"verbs"`
	Webhook   struct {
		WebhookID string `json:"webhook_id"`
		IngestURL string `json:"ingest_url"`
		TrustMode string `json:"trust_mode"`
	} `json:"webhook"`
	MCPJSON   json.RawMessage `json:"mcp_json"`
	ExpiresAt *time.Time      `json:"expires_at"`
}

func cmdVend(c *cli, args []string) int {
	fs := c.flagSet("endpoint vend", "NAME", "Register an agent and vend its scoped MCP endpoint, its queue, and a token-trust\n"+
		"ingestion webhook in one call. The credential is printed ONCE — store it now.")
	queue := fs.String("queue", "inbox", "the queue the endpoint drains and the webhook feeds")
	fs.StringVar(queue, "q", "inbox", "shorthand for --queue")
	asJSON := fs.Bool("json", false, "print the raw API response")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) == 0 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "an agent name is required (switchboard vend NAME)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	if strings.TrimSpace(*queue) == "" {
		return c.usageError(fs, "--queue must not be empty")
	}

	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.post("/api/v1/endpoints", map[string]string{"name": strings.TrimSpace(pos[0]), "queue": strings.TrimSpace(*queue)})
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var v vendResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return c.fail(fmt.Errorf("vend: malformed response: %w", err))
	}
	printVendReveal(c.stdout, v)
	return exitOK
}

// printVendReveal renders the one-time reveal: the same fields, warning, and .mcp.json wiring the
// web wizard's reveal shows (SPEC-0007, SPEC-0015).
func printVendReveal(w io.Writer, v vendResponse) {
	fmt.Fprintf(w, "Vended %s (slug %s) on queue %s.\n", v.AgentName, v.Slug, v.Queue)
	fmt.Fprintln(w, "This credential is shown ONCE and cannot be recovered — store it now.")
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  MCP endpoint\t%s\n", v.MCPURL)
	fmt.Fprintf(tw, "  Bearer token\t%s\n", v.Token)
	if v.Webhook.IngestURL != "" {
		fmt.Fprintf(tw, "  Ingest URL\t%s\n", v.Webhook.IngestURL)
		fmt.Fprintf(tw, "  \t(any producer POSTs here; the unguessable URL is its credential)\n")
	} else {
		fmt.Fprintf(tw, "  Ingest URL\tnot minted — the endpoint works over MCP; create a webhook with create_webhook\n")
	}
	fmt.Fprintf(tw, "  Verbs\t%s\n", strings.Join(v.Verbs, " "))
	if v.ExpiresAt != nil {
		fmt.Fprintf(tw, "  Expires\t%s\n", v.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintf(tw, "  Expires\tnever (valid until revoked)\n")
	}
	_ = tw.Flush()
	if len(v.MCPJSON) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, v.MCPJSON, "", "  ") == nil {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Client wiring — paste into your MCP client's .mcp.json:")
			fmt.Fprintln(w, pretty.String())
		}
	}
}

// --- endpoints / agents ---

func cmdEndpoints(c *cli, args []string) int {
	fs := c.flagSet("endpoint list", "", "List the vended endpoints you own. Credentials are never shown: a token is revealed\n"+
		"exactly once, at vend time.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	if pos, exit, ok := c.parseArgs(fs, args); !ok {
		return exit
	} else if len(pos) > 0 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[0]))
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.get("/api/v1/endpoints")
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var rows []struct {
		Slug      string     `json:"slug"`
		AgentName string     `json:"agent_name"`
		State     string     `json:"state"`
		Queues    []string   `json:"queues"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return c.fail(fmt.Errorf("endpoints: malformed response: %w", err))
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.stdout, "No endpoints vended yet. Run `switchboard endpoint vend NAME` to vend one.")
		return exitOK
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tAGENT\tSTATE\tQUEUES\tEXPIRES")
	for _, r := range rows {
		expires := "never"
		if r.ExpiresAt != nil {
			expires = r.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Slug, r.AgentName, r.State, strings.Join(r.Queues, ","), expires)
	}
	_ = tw.Flush()
	return exitOK
}

func cmdAgents(c *cli, args []string) int {
	fs := c.flagSet("agent list", "", "List your registered agents.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	if pos, exit, ok := c.parseArgs(fs, args); !ok {
		return exit
	} else if len(pos) > 0 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[0]))
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.get("/api/v1/agents")
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var rows []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return c.fail(fmt.Errorf("agents: malformed response: %w", err))
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.stdout, "No agents registered yet. Run `switchboard vend NAME` to register one.")
		return exitOK
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tDESCRIPTION")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.ID, r.Name, r.Description)
	}
	_ = tw.Flush()
	return exitOK
}

// --- shared output helpers ---

// printJSON pretty-prints an API response body verbatim (field order preserved).
func (c *cli) printJSON(body []byte) int {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		// Not JSON after all: print what the server sent rather than nothing.
		fmt.Fprintln(c.stdout, strings.TrimSpace(string(body)))
		return exitOK
	}
	fmt.Fprintln(c.stdout, pretty.String())
	return exitOK
}

// normalizeBaseURL accepts what an operator types (a trailing slash, a bare host) and returns the
// canonical http(s) origin the credentials record.
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%q is not an http(s) URL", raw)
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// roundDuration renders a duration the way a human reads one: whole seconds under a minute, whole
// minutes under an hour, otherwise hours and minutes.
func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Minute).String()
	}
}

// statusText renders an HTTP status for error messages ("404 Not Found").
func statusText(code int) string {
	return fmt.Sprintf("%d %s", code, http.StatusText(code))
}

// errNotLoggedIn is the shape every verb reports when no credentials file exists.
var errNotLoggedIn = errors.New("not logged in — run `switchboard login <URL>` first")

// isNotExist reports whether err is the missing-file error, for the idempotent logout path.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }

// cmdEndpointRevoke kills one endpoint. This is the rotation path: a credential that has leaked is
// only actually dead once the endpoint is revoked, and before this verb existed that required the
// web UI — which meant a headless box, or an agent that had just leaked a token, could not fix it.
//
// The endpoint is named by the slug `endpoint list` prints, or by its id. Revocation is terminal
// (SPEC-0007: a changed scope means a new endpoint, never an edited one), so -y exists for scripts
// but the interactive path asks first.
func cmdEndpointRevoke(c *cli, args []string) int {
	fs := c.flagSet("endpoint revoke", "SLUG|ID",
		"Kill an endpoint: its stored credential stops authenticating immediately and any live MCP\n"+
			"session on it is torn down. This cannot be undone — vend a new endpoint instead.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	yes := fs.Bool("y", false, "skip the confirmation prompt")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) == 0:
		return c.usageError(fs, "name the endpoint to revoke (its slug, from `switchboard endpoint list`)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	ref := pos[0]

	if !*yes && !*asJSON {
		fmt.Fprintf(c.stdout, "Revoke %s? Its credential stops working immediately and cannot be restored. [y/N] ", ref)
		if !c.confirm() {
			fmt.Fprintln(c.stdout, "Not revoked.")
			return exitOK
		}
	}

	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.post("/api/v1/endpoints/"+url.PathEscape(ref)+"/revoke", nil)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var out struct {
		Slug      string `json:"slug"`
		AgentName string `json:"agent_name"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return c.fail(fmt.Errorf("endpoint revoke: malformed response: %w", err))
	}
	fmt.Fprintf(c.stdout, "Revoked %s (%s). Its credential no longer authenticates.\n", out.Slug, out.AgentName)
	fmt.Fprintln(c.stdout, "Anything wired to it needs a new endpoint: switchboard endpoint vend NAME --queue Q")
	return exitOK
}
