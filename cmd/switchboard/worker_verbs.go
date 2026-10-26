package main

// Worker Verbs On The Endpoint Credential
//
// `switchboard todo next|claim|heartbeat|complete|fail|release|list` act as a vended endpoint:
// they authenticate with the endpoint credential (SWITCHBOARD_URL + SWITCHBOARD_API_KEY, or
// --url/--key) and drive the endpoint's own MCP session over Streamable HTTP, so the verbs carry
// exactly the endpoint's verb scope and the server enforces it unchanged. The credential is never
// optional and never inferred: these verbs do not read the operator login, and a machine that has
// only an operator login gets an error rather than a quiet fallback (#556).
//
// The endpoint is named either by the full MCP URL the vend response prints as mcp_url
// (<base>/mcp/<slug>) or by the deployment base plus --slug; --key (or SWITCHBOARD_API_KEY) is
// the vended sbk_ credential. A credential whose slug does not match the path is refused by the
// server with 403, exactly as it refuses an MCP client wiring pointed at the wrong endpoint.
//
// Output is the tool's structured JSON, so what a script parses is byte-for-byte what the MCP
// tools return. Context hygiene (#556): `next` and `claim` print the todo without the webhook
// payload unless --payload is passed, and `next` on an empty queue prints {"empty": true} and
// exits 3, so a one-shot can branch without reading a body.
//
// @joestump-agent 10/15/2026 - Added (stump.wtf/switchboard#556): operators end up CLI-only and
// the MCP retires from interactive configs; workers keep the MCP until this lands.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stump-wtf/switchboard/internal/buildinfo"
)

// exitNoWork is `todo next`'s distinct exit code when no work was available: not a failure (the
// queue is simply empty) and not 0, so a one-shot branches on $? without parsing stdout.
const exitNoWork = 3

// workerFlags are the credential flags every worker verb carries. The environment is the default
// so a worker's profile sets SWITCHBOARD_URL/SWITCHBOARD_API_KEY once and every call is bare.
type workerFlags struct {
	url, key, slug *string
}

func workerFlagSet(c *cli, name, args, summary string) (*flag.FlagSet, *workerFlags) {
	fs := c.flagSet(name, args, summary)
	return fs, &workerFlags{
		url:  fs.String("url", "", "deployment base URL or the endpoint's full <base>/mcp/<slug> URL (default: $SWITCHBOARD_URL)"),
		key:  fs.String("key", "", "the vended endpoint credential, sbk_… (default: $SWITCHBOARD_API_KEY); never the operator login"),
		slug: fs.String("slug", "", "endpoint slug, when --url names the deployment base rather than the endpoint"),
	}
}

// resolveWorker resolves the endpoint credential. It reads only the flags and the two
// SWITCHBOARD_URL/SWITCHBOARD_API_KEY variables: the operator credentials file is never consulted
// and never used, so a verb can silently act with operator authority.
func (c *cli) resolveWorker(fs *flag.FlagSet, w *workerFlags) (workerConn, error) {
	endpoint := strings.TrimSpace(*w.url)
	if endpoint == "" {
		endpoint = strings.TrimSpace(c.getenv("SWITCHBOARD_URL"))
	}
	key := strings.TrimSpace(*w.key)
	if key == "" {
		key = strings.TrimSpace(c.getenv("SWITCHBOARD_API_KEY"))
	}
	slug := strings.TrimSpace(*w.slug)
	if endpoint == "" && slug == "" {
		return workerConn{}, errors.New("no endpoint credential: set SWITCHBOARD_URL and SWITCHBOARD_API_KEY (or --url/--key); these verbs act as the endpoint and never fall back to the operator login")
	}
	if key == "" {
		return workerConn{}, errors.New("no endpoint credential: set SWITCHBOARD_API_KEY (or --key); these verbs act as the endpoint and never fall back to the operator login")
	}
	if endpoint != "" && slug != "" && strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/mcp/"+slug) {
		c.usageError(fs, "--slug is redundant: --url already names the endpoint")
		return workerConn{}, errUsageReturned
	}
	if endpoint == "" {
		base := strings.TrimSpace(c.getenv("SWITCHBOARD_URL"))
		if base == "" {
			return workerConn{}, errors.New("--slug names the endpoint under $SWITCHBOARD_URL, which is not set")
		}
		endpoint = strings.TrimRight(base, "/") + "/mcp/" + slug
	}
	if slug != "" {
		endpoint = strings.TrimRight(endpoint, "/") + "/mcp/" + slug
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return workerConn{}, fmt.Errorf("--url/SWITCHBOARD_URL must be an http(s) URL: %q", endpoint)
	}
	if _, got, ok := strings.Cut(strings.TrimRight(u.Path, "/"), "/mcp/"); ok {
		if got == "" || strings.Contains(got, "/") {
			return workerConn{}, fmt.Errorf("the endpoint URL must be <base>/mcp/<slug>: %q", endpoint)
		}
	} else if slug == "" {
		return workerConn{}, errors.New("SWITCHBOARD_URL names a deployment base, not an endpoint: point it at <base>/mcp/<slug> (the vend response's mcp_url) or pass --slug")
	}
	return workerConn{endpoint: strings.TrimRight(endpoint, "/"), key: key}, nil
}

// errUsageReturned marks that the message (and usage) already reached stderr.
var errUsageReturned = errors.New("usage error")

// workerConn is one endpoint's MCP session surface: the URL and its credential.
type workerConn struct {
	endpoint, key string
}

func (w workerConn) call(ctx context.Context, c *cli, tool string, args map[string]any) (json.RawMessage, error) {
	conn, err := c.connectWorker(ctx, w)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	res, err := conn.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	if res.IsError {
		return nil, errors.New(tool + ": " + toolErrorText(res))
	}
	if res.StructuredContent == nil {
		return nil, fmt.Errorf("%s: no structured output", tool)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding output: %w", tool, err)
	}
	return json.RawMessage(b), nil
}

// toolErrorText renders the server's tool error (code: message) for stderr.
func toolErrorText(res *sdk.CallToolResult) string {
	var sb strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*sdk.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(tc.Text)
		}
	}
	if sb.Len() == 0 {
		return "tool call failed"
	}
	return sb.String()
}

// workerConnect is the injectable edge that opens the endpoint session; tests point the whole
// surface at an in-process MCP server.
func defaultWorkerConnect(c *cli) func(ctx context.Context, w workerConn) (*sdk.ClientSession, error) {
	return func(ctx context.Context, w workerConn) (*sdk.ClientSession, error) {
		hc := *c.http
		hc.Transport = headerTransport{next: c.http.Transport, key: w.key}
		client := sdk.NewClient(&sdk.Implementation{
			Name:    "switchboard-cli",
			Version: buildinfo.Get().Version,
		}, nil)
		tr := &sdk.StreamableClientTransport{
			Endpoint: w.endpoint, HTTPClient: &hc,
			// A CLI verb is one request-response: no standalone SSE stream to hold the one-shot open.
			DisableStandaloneSSE: true,
		}
		return client.Connect(ctx, tr, nil)
	}
}

type headerTransport struct {
	next http.RoundTripper
	key  string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	next := h.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// --- payload hygiene ---

// stripPayloads drops the webhook payload from a todo or claim output unless --payload was
// passed: the one-shot's context is the budget (#556). Field order is preserved; the payload is
// replaced with its size so the line still says what claiming the full todo would carry.
func stripPayloads(raw json.RawMessage, show bool) json.RawMessage {
	if show {
		return raw
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw
	}
	changed := false
	if p, ok := top["payload"]; ok {
		var v any
		if json.Unmarshal(p, &v) == nil && v != nil {
			top["payload"] = json.RawMessage(`{"omitted":true}`)
			changed = true
		}
	}
	if td, ok := top["todo"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(td, &nested) == nil {
			if p, ok := nested["payload"]; ok {
				var v any
				if json.Unmarshal(p, &v) == nil && v != nil {
					nested["payload"] = json.RawMessage(`{"omitted":true}`)
					changed = true
					b, err := json.Marshal(nested)
					if err == nil {
						top["todo"] = json.RawMessage(b)
					}
				}
			}
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(top)
	if err != nil {
		return raw
	}
	return json.RawMessage(b)
}

// readResultArg resolves --result: a JSON object inline, or @file (@- for stdin). A todo result
// is an object, so anything else is a usage mistake, not a server round trip.
func (c *cli) readResultArg(fs *flag.FlagSet, spec string) (json.RawMessage, error) {
	raw, err := c.readPayload(spec)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("--result must be JSON (inline, @file, or @- for stdin)")
	}
	if trimmed := strings.TrimSpace(string(raw)); trimmed == "" || (trimmed[0] != '{') {
		return nil, fmt.Errorf("--result must be a JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("--result must be a JSON object: %w", err)
	}
	return json.RawMessage(raw), nil
}

// --- verbs ---

func cmdTodoList(c *cli, args []string) int {
	fs, wf := workerFlagSet(c, "todo list", "", "List the todos in this endpoint's granted queues, newest first, as compact rows:\n"+
		"the same rows list_todos advertises, with no payload on the wire.")
	asJSON := fs.Bool("json", false, "print the tool's structured JSON verbatim")
	queue := fs.String("queue", "", "restrict to one granted queue")
	state := fs.String("state", "", "filter by state: pending, claimed, done, or failed")
	limit := fs.Int("limit", 0, "maximum rows (default 50, cap 200)")
	fields := fs.String("fields", "", "project each row's payload server-side to these comma-separated paths (issue.number,repository.full_name); a listing never carries whole payloads")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	if len(pos) > 0 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[0]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := map[string]any{}
	if *queue != "" {
		in["queue"] = *queue
	}
	if *state != "" {
		in["state"] = *state
	}
	if *limit > 0 {
		in["limit"] = *limit
	}
	if f := splitFields(*fields); len(f) > 0 {
		in["payload_fields"] = f
	}
	out, err := conn.call(context.Background(), c, "list_todos", in)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(out)
	}
	var doc struct {
		Todos []struct {
			ID          string `json:"id"`
			Queue       string `json:"queue"`
			Title       string `json:"title"`
			State       string `json:"state"`
			Attempt     int    `json:"attempt"`
			PayloadSize int    `json:"payload_size"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return c.fail(fmt.Errorf("todo list: malformed response: %w", err))
	}
	if len(doc.Todos) == 0 {
		fmt.Fprintln(c.stdout, "No todos in this endpoint's queues.")
		return exitOK
	}
	for _, td := range doc.Todos {
		fmt.Fprintf(c.stdout, "%s  %-8s %-12s attempt=%d payload=%dB  %s\n",
			td.ID, td.State, td.Queue, td.Attempt, td.PayloadSize, td.Title)
	}
	return exitOK
}

// claimArgs wires the flags claim and next share.
type claimArgs struct {
	leaseTTL *int
	fence    *bool
	claimant *string
	payload  *bool
	fields   *string
	asJSON   *bool
	queue    *string
}

func claimFlagSet(c *cli, name, args, summary string) (*flag.FlagSet, *workerFlags, *claimArgs) {
	fs, wf := workerFlagSet(c, name, args, summary)
	ca := &claimArgs{
		leaseTTL: fs.Int("lease-ttl", 0, "lease TTL in seconds (default: the endpoint's default lease; max 86400)"),
		fence:    fs.Bool("fence", false, "require_fence: the response carries a lease_token this attempt must present to heartbeat/complete/fail/release"),
		claimant: fs.String("claimant", "", "label for who is making this attempt, shown to later claimers"),
		payload:  fs.Bool("payload", false, "include the todo's full payload in the output"),
		fields:   fs.String("fields", "", "project the payload server-side to these comma-separated paths (issue.number,issue.labels[].name): fewer bytes on the wire and in context"),
		asJSON:   fs.Bool("json", false, "print the tool's structured JSON verbatim (the claim's full response)"),
	}
	return fs, wf, ca
}

func (ca *claimArgs) in() map[string]any {
	in := map[string]any{}
	if *ca.leaseTTL > 0 {
		in["lease_ttl_seconds"] = *ca.leaseTTL
	}
	if *ca.fence {
		in["require_fence"] = true
	}
	if *ca.claimant != "" {
		in["claimant"] = *ca.claimant
	}
	if f := splitFields(*ca.fields); len(f) > 0 {
		in["payload_fields"] = f
	}
	return in
}

// splitFields turns --fields a,b[].c into the verb's list, dropping blanks; an empty result means the
// argument is left off the call entirely.
func splitFields(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdTodoNext(c *cli, args []string) int {
	fs, wf, ca := claimFlagSet(c, "todo next", "", "Claim the oldest available todo in this endpoint's queues — the CLI shape of\n"+
		"claim_next. An empty queue is the normal idle answer: it prints {\"empty\": true} and\n"+
		"exits 3, so a one-shot branches without reading a body.")
	ca.queue = fs.String("queue", "", "restrict the scan to one granted queue")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	if len(pos) > 0 {
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[0]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := ca.in()
	if *ca.queue != "" {
		in["queue"] = *ca.queue
	}
	out, err := conn.call(context.Background(), c, "claim_next", in)
	if err != nil {
		return c.fail(err)
	}
	var res struct {
		Empty *bool `json:"empty"`
	}
	empty := false
	if json.Unmarshal(out, &res) == nil && res.Empty != nil {
		empty = *res.Empty
	}
	if empty {
		if *ca.asJSON {
			if code := c.printJSON(out); code != exitOK {
				return code
			}
		} else {
			fmt.Fprintln(c.stdout, `{"empty": true}`)
		}
		return exitNoWork
	}
	return printClaim(c, out, ca)
}

func cmdTodoClaim(c *cli, args []string) int {
	fs, wf, ca := claimFlagSet(c, "todo claim", "ID", "Claim one todo by id and print it, payload hidden unless --payload is passed.")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 1 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "a todo id is required (switchboard todo claim ID)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := ca.in()
	in["id"] = strings.TrimSpace(pos[0])
	out, err := conn.call(context.Background(), c, "claim", in)
	if err != nil {
		return c.fail(err)
	}
	return printClaim(c, out, ca)
}

// printClaim renders a claim/next response: the todo, payload-stripped unless --payload or --fields
// asked for it, or the whole structured response with --json.
func printClaim(c *cli, raw json.RawMessage, ca *claimArgs) int {
	// --fields already asked the server for the only bytes worth printing, so its answer is shown.
	show := *ca.payload || len(splitFields(*ca.fields)) > 0
	if *ca.asJSON {
		return c.printJSON(stripPayloads(raw, show))
	}
	var td struct {
		ID          string `json:"id"`
		Queue       string `json:"queue"`
		Title       string `json:"title"`
		State       string `json:"state"`
		AttemptSeq  int    `json:"attempt_seq"`
		PayloadSize int    `json:"payload_size"`
		LeaseToken  string `json:"lease_token"`
	}
	// claim_next answers the todo nested under "todo"; claim answers it at the top level.
	var top map[string]json.RawMessage
	body := raw
	if json.Unmarshal(raw, &top) == nil {
		if sub, ok := top["todo"]; ok {
			body = sub
			if tok, ok := top["lease_token"]; ok {
				_ = json.Unmarshal(tok, &td.LeaseToken)
			}
			if seq, ok := top["attempt_seq"]; ok {
				_ = json.Unmarshal(seq, &td.AttemptSeq)
			}
		}
	}
	// A claim always answers a todo; if the shape moved, showing the raw JSON beats showing nothing.
	if err := json.Unmarshal(body, &td); err != nil {
		return c.printJSON(stripPayloads(raw, show))
	}
	fmt.Fprintf(c.stdout, "claimed %s queue=%s state=%s attempt=%d payload=%dB\n%s\n",
		td.ID, td.Queue, td.State, td.AttemptSeq, td.PayloadSize, td.Title)
	if td.LeaseToken != "" {
		fmt.Fprintf(c.stdout, "lease_token=%s — pass it as --lease-token to heartbeat/complete/fail/release\n", td.LeaseToken)
	}
	if !show && td.PayloadSize > 0 {
		fmt.Fprintf(c.stdout, "payload omitted (%d bytes): rerun with --payload or read it with `switchboard todo get %s --payload`\n",
			td.PayloadSize, td.ID)
	}
	return exitOK
}

func cmdTodoGet(c *cli, args []string) int {
	fs, wf := workerFlagSet(c, "todo get", "ID", "Read one todo without claiming it: its row, result, retry state and attempts,\n"+
		"newest first. The payload stays out of context unless --payload is passed.")
	payload := fs.Bool("payload", false, "include the todo's full payload in the output")
	fields := fs.String("fields", "", "project the payload server-side to these comma-separated paths (issue.number,issue.labels[].name): fewer bytes on the wire and in context")
	asJSON := fs.Bool("json", false, "print the tool's structured JSON verbatim")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 1 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "a todo id is required (switchboard todo get ID)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := map[string]any{"id": strings.TrimSpace(pos[0])}
	if f := splitFields(*fields); len(f) > 0 {
		in["payload_fields"] = f
	}
	out, err := conn.call(context.Background(), c, "get_todo", in)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(stripPayloads(out, *payload || len(splitFields(*fields)) > 0))
	}
	var row struct {
		ID          string `json:"id"`
		Queue       string `json:"queue"`
		Title       string `json:"title"`
		State       string `json:"state"`
		Attempt     int    `json:"attempt"`
		DeadLetter  bool   `json:"dead_letter"`
		PayloadSize int    `json:"payload_size"`
	}
	if err := json.Unmarshal(out, &row); err != nil {
		return c.printJSON(stripPayloads(out, *payload || len(splitFields(*fields)) > 0))
	}
	fmt.Fprintf(c.stdout, "%s  %s  queue=%s attempt=%d payload=%dB  %s\n",
		row.ID, row.State, row.Queue, row.Attempt, row.PayloadSize, row.Title)
	if row.DeadLetter {
		fmt.Fprintln(c.stdout, "dead_letter: nothing will re-queue this todo")
	}
	return exitOK
}

func cmdTodoHeartbeat(c *cli, args []string) int {
	fs, wf := workerFlagSet(c, "todo heartbeat", "ID", "Extend this attempt's lease on a claimed todo. The lease is the todo's, not the\n"+
		"connection's: heartbeat before any slow step and any irreversible one.")
	leaseTTL := fs.Int("lease-ttl", 0, "new lease TTL in seconds from now (default: the endpoint's default lease; max 86400)")
	leaseToken := fs.String("lease-token", "", "the lease_token from a claim made with --fence")
	asJSON := fs.Bool("json", false, "print the tool's structured JSON verbatim")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 1 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "a todo id is required (switchboard todo heartbeat ID)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := map[string]any{"id": strings.TrimSpace(pos[0])}
	if *leaseTTL > 0 {
		in["lease_ttl_seconds"] = *leaseTTL
	}
	if *leaseToken != "" {
		in["lease_token"] = *leaseToken
	}
	out, err := conn.call(context.Background(), c, "heartbeat", in)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(out)
	}
	var row struct {
		ID             string `json:"id"`
		State          string `json:"state"`
		LeaseExpiresAt string `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(out, &row); err != nil {
		return c.printJSON(out)
	}
	fmt.Fprintf(c.stdout, "lease extended: %s state=%s expires=%s\n", row.ID, row.State, row.LeaseExpiresAt)
	return exitOK
}

func cmdTodoComplete(c *cli, args []string) int {
	fs, wf := workerFlagSet(c, "todo complete", "ID", "Close a claimed todo as done. The result and summary you pass here are what the\n"+
		"next reader (and Cairn history) sees — make them say what happened.")
	result := fs.String("result", "", "JSON object recorded on the todo: inline, @file, or @- for stdin")
	summary := fs.String("summary", "", "note for later claimers on what this attempt did")
	artifact := fs.String("artifact", "", "mcp://cairn/<id> handle or absolute https URL kept on the attempt; never fetched")
	leaseToken := fs.String("lease-token", "", "the lease_token from a claim made with --fence")
	asJSON := fs.Bool("json", false, "print the tool's structured JSON verbatim")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 1 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "a todo id is required (switchboard todo complete ID)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := map[string]any{"id": strings.TrimSpace(pos[0])}
	if *result != "" {
		raw, err := c.readResultArg(fs, *result)
		if err != nil {
			return c.fail(err)
		}
		in["result"] = json.RawMessage(raw)
	}
	if *summary != "" {
		in["summary"] = *summary
	}
	if *artifact != "" {
		in["artifact"] = *artifact
	}
	if *leaseToken != "" {
		in["lease_token"] = *leaseToken
	}
	out, err := conn.call(context.Background(), c, "complete", in)
	if err != nil {
		return c.fail(err)
	}
	return printRow(c, out, "completed", *asJSON)
}

func cmdTodoFail(c *cli, args []string) int {
	fs, wf := workerFlagSet(c, "todo fail", "ID", "Fail a claimed todo: it retries while attempts remain and dead-letters when they are\n"+
		"exhausted. Say in the summary what you tried — the next claimer reads it.")
	result := fs.String("result", "", "JSON object recorded as failure detail: inline, @file, or @- for stdin")
	summary := fs.String("summary", "", "note for the next claimer on what this attempt tried and why it failed")
	artifact := fs.String("artifact", "", "mcp://cairn/<id> handle or absolute https URL kept on the attempt; never fetched")
	leaseToken := fs.String("lease-token", "", "the lease_token from a claim made with --fence")
	asJSON := fs.Bool("json", false, "print the tool's structured JSON verbatim")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 1 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "a todo id is required (switchboard todo fail ID)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := map[string]any{"id": strings.TrimSpace(pos[0])}
	if *result != "" {
		raw, err := c.readResultArg(fs, *result)
		if err != nil {
			return c.fail(err)
		}
		in["result"] = json.RawMessage(raw)
	}
	if *summary != "" {
		in["summary"] = *summary
	}
	if *artifact != "" {
		in["artifact"] = *artifact
	}
	if *leaseToken != "" {
		in["lease_token"] = *leaseToken
	}
	out, err := conn.call(context.Background(), c, "fail", in)
	if err != nil {
		return c.fail(err)
	}
	return printRow(c, out, "failed", *asJSON)
}

func cmdTodoRelease(c *cli, args []string) int {
	fs, wf := workerFlagSet(c, "todo release", "ID", "Hand a claimed todo back to the queue without a verdict: it stays pending, its\n"+
		"attempt counter does not move, and no retry backoff applies.")
	summary := fs.String("summary", "", "note on why the attempt ended (shutdown, operator stop, usage limit)")
	artifact := fs.String("artifact", "", "mcp://cairn/<id> handle or absolute https URL kept on the attempt; never fetched")
	leaseToken := fs.String("lease-token", "", "the lease_token from a claim made with --fence")
	asJSON := fs.Bool("json", false, "print the tool's structured JSON verbatim")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 1 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "a todo id is required (switchboard todo release ID)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	conn, err := c.resolveWorker(fs, wf)
	if err != nil {
		if errors.Is(err, errUsageReturned) {
			return exitUsage
		}
		return c.fail(err)
	}
	in := map[string]any{"id": strings.TrimSpace(pos[0])}
	if *summary != "" {
		in["summary"] = *summary
	}
	if *artifact != "" {
		in["artifact"] = *artifact
	}
	if *leaseToken != "" {
		in["lease_token"] = *leaseToken
	}
	out, err := conn.call(context.Background(), c, "release", in)
	if err != nil {
		return c.fail(err)
	}
	return printRow(c, out, "released", *asJSON)
}

// printRow renders a verb's compact todo row.
func printRow(c *cli, raw json.RawMessage, verb string, asJSON bool) int {
	if asJSON {
		return c.printJSON(raw)
	}
	var row struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return c.printJSON(raw)
	}
	fmt.Fprintf(c.stdout, "%s %s (state %s)\n", verb, row.ID, row.State)
	return exitOK
}
