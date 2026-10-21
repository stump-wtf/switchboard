package main

// CLI: widen an endpoint's scope without re-vending it (issue #590, ADR-0048 proposed).
//
// `endpoint scope widen` is the operator's front door for the one scope change that cannot break a
// working route: adding an allowed webhook queue, an ingest source type, a verb, or raising the webhook
// ceiling. It runs in two calls, like the web vend flow it is meant to match: the first asks the server
// what the endpoint would grant, prints old and new side by side, and changes nothing; the second,
// after the operator says yes (or passes --yes), applies exactly that. A widen rotates no credential —
// the ingest URL and its signing secret stay as they were — which is the whole reason it exists.
//
// `endpoint scope add-queue SLUG QUEUE` is the same call with one flag pre-filled, because "one more
// lane needs a route here" is the request operators actually make.
//
// Governing: ADR-0048 (proposed), ADR-0008, SPEC-0006 REQ "Webhook Self-Management Within a Vended
// Ceiling", SPEC-0035 REQ "CLI Parity".
//
// @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// widenScopeRequest is the POST body; confirm is what separates the preview from the change.
type widenScopeRequest struct {
	AddWebhookQueues      []string `json:"add_webhook_queues,omitempty"`
	AddWebhookSourceTypes []string `json:"add_webhook_source_types,omitempty"`
	AddScopeVerbs         []string `json:"add_scope_verbs,omitempty"`
	WebhookMax            *int     `json:"webhook_max,omitempty"`
	Confirm               bool     `json:"confirm"`
}

// scopeView mirrors the API's scope object.
type scopeView struct {
	ScopeQueues        []string `json:"scope_queues"`
	ScopeVerbs         []string `json:"scope_verbs"`
	WebhookMax         int      `json:"webhook_max"`
	WebhookSourceTypes []string `json:"webhook_source_types"`
	WebhookQueues      []string `json:"webhook_queues"`
}

type wideningView struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// widenScopeResponse mirrors the API's response, previewed or applied.
type widenScopeResponse struct {
	Slug                 string         `json:"slug"`
	AgentName            string         `json:"agent_name"`
	Applied              bool           `json:"applied"`
	RequiresConfirmation bool           `json:"requires_confirmation"`
	Changed              bool           `json:"changed"`
	Before               scopeView      `json:"before"`
	After                scopeView      `json:"after"`
	Widenings            []wideningView `json:"widenings"`
}

// parseNameList splits a comma-separated flag value, dropping blanks.
func parseNameList(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// cmdEndpointScopeWiden previews and then applies an additive scope change.
func cmdEndpointScopeWiden(c *cli, args []string) int {
	fs := c.flagSet("endpoint scope widen", "SLUG|ID",
		"Widen an endpoint's scope without re-vending it: more allowed webhook queues, more ingest\n"+
			"source types, more verbs, a larger webhook ceiling. Nothing is removed — narrowing or\n"+
			"replacing scope is still a revoke and a re-vend. No credential changes: the ingest URL and\n"+
			"its signing secret stay as they were.\n"+
			"Shows the old and the new scope and asks before it applies anything; --yes applies directly.")
	addQueues := fs.String("add-queue", "", "queues its webhooks may newly route to, comma-separated")
	addSources := fs.String("add-source-type", "", "ingest source types its agents may newly create webhooks for, comma-separated")
	addVerbs := fs.String("add-verb", "", "MCP verbs to add to the grant, comma-separated (verb names and vend presets)")
	webhookMax := fs.Int("webhook-max", -1, "raise the ceiling on how many self-managed webhooks its agents may create; never lowers it")
	yes := fs.Bool("yes", false, "apply without asking, after showing the change (for scripts)")
	asJSON := fs.Bool("json", false, "print the raw API response")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) == 0:
		return c.usageError(fs, "name the endpoint to widen (its slug, from `switchboard endpoint list`)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	req := widenScopeRequest{
		AddWebhookQueues:      parseNameList(*addQueues),
		AddWebhookSourceTypes: parseNameList(*addSources),
		AddScopeVerbs:         parseNameList(*addVerbs),
	}
	if *webhookMax >= 0 {
		req.WebhookMax = webhookMax
	}
	if len(req.AddWebhookQueues) == 0 && len(req.AddWebhookSourceTypes) == 0 &&
		len(req.AddScopeVerbs) == 0 && req.WebhookMax == nil {
		return c.usageError(fs, "nothing to widen: pass at least one of --add-queue, --add-source-type, --add-verb, --webhook-max")
	}
	return widenScope(c, fs, pos[0], req, *yes, *asJSON)
}

// cmdEndpointScopeAddQueue is the one-flag shape of a widen: the request an operator makes when another
// lane needs a route and re-vending would rotate a secret for nothing.
func cmdEndpointScopeAddQueue(c *cli, args []string) int {
	fs := c.flagSet("endpoint scope add-queue", "SLUG|ID QUEUE",
		"Add a queue an endpoint's webhooks may route to, without re-vending it. Use this when a new\n"+
			"lane needs a route: a re-vend would mint a new endpoint, a new webhook, a new ingest URL and\n"+
			"a new signing secret, and would need an edit in the source system. This changes none of them.")
	yes := fs.Bool("yes", false, "apply without asking, after showing the change (for scripts)")
	asJSON := fs.Bool("json", false, "print the raw API response")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) < 2:
		return c.usageError(fs, "name the endpoint and the queue to add (switchboard endpoint scope add-queue SLUG QUEUE)")
	case len(pos) > 2:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[2]))
	case strings.TrimSpace(pos[1]) == "":
		return c.usageError(fs, "the queue to add must not be empty")
	}
	return widenScope(c, fs, pos[0], widenScopeRequest{AddWebhookQueues: []string{strings.TrimSpace(pos[1])}}, *yes, *asJSON)
}

// wideningsView mirrors one recorded widening: the field, the value it replaced, the value it became.
type wideningsView struct {
	Field     string `json:"field"`
	Before    any    `json:"before"`
	After     any    `json:"after"`
	CreatedAt string `json:"created_at"`
}

// scopeWideningsResponse mirrors the history route's response.
type scopeWideningsResponse struct {
	Slug      string          `json:"slug"`
	AgentName string          `json:"agent_name"`
	Widenings []wideningsView `json:"widenings"`
}

// cmdEndpointScopeHistory answers "when did this lane get that route, and what did it have before?" —
// the question an audit trail exists to answer, and the reason a widen that changes nothing writes no
// row: the record should read as changes, not as calls.
func cmdEndpointScopeHistory(c *cli, args []string) int {
	fs := c.flagSet("endpoint scope history", "SLUG|ID",
		"Print the scope widenings recorded for an endpoint, newest first: the field, the value it\n"+
			"replaced, the value it became, and when. A widen that changed nothing is not recorded,\n"+
			"so this reads as a list of changes rather than a list of calls.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) == 0:
		return c.usageError(fs, "name the endpoint to read (its slug, from `switchboard endpoint list`)")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.get("/api/v1/endpoints/" + url.PathEscape(pos[0]) + "/scope/widenings")
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var out scopeWideningsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return c.fail(fmt.Errorf("endpoint scope history: malformed response: %w", err))
	}
	if len(out.Widenings) == 0 {
		fmt.Fprintf(c.stdout, "%s (%s): no scope has been widened since it was vended.\n", out.Slug, out.AgentName)
		return exitOK
	}
	fmt.Fprintf(c.stdout, "%s (%s): %d widening%s since it was vended.\n", out.Slug, out.AgentName,
		len(out.Widenings), plural(len(out.Widenings)))
	for _, w := range out.Widenings {
		fmt.Fprintf(c.stdout, "  %s  %s\n", w.CreatedAt, w.Field)
		fmt.Fprintf(c.stdout, "    was  %s\n", scopeValueText(w.Before))
		fmt.Fprintf(c.stdout, "    now  %s\n", scopeValueText(w.After))
	}
	return exitOK
}

// widenScope shows the change, then applies it if the operator agrees. The preview is a real request to
// a real server: the numbers printed are the ones the server would write, not a client-side guess.
func widenScope(c *cli, fs *flag.FlagSet, ref string, req widenScopeRequest, yes, asJSON bool) int {
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	path := "/api/v1/endpoints/" + url.PathEscape(ref) + "/scope/widen"

	req.Confirm = false
	previewBody, err := api.post(path, req)
	if err != nil {
		return c.fail(err)
	}
	var preview widenScopeResponse
	if err := json.Unmarshal(previewBody, &preview); err != nil {
		return c.fail(fmt.Errorf("endpoint scope widen: malformed response: %w", err))
	}
	if asJSON && !yes {
		return c.printJSON(previewBody)
	}
	if !preview.Changed {
		fmt.Fprintf(c.stdout, "%s already grants everything you asked for — nothing changed.\n", preview.Slug)
		return exitOK
	}
	printScopeChange(c, preview)
	if !yes {
		fmt.Fprint(c.stdout, "Apply this widen? [y/N] ")
		if !c.confirm() {
			fmt.Fprintln(c.stdout, "Nothing changed. Re-run with --yes to apply without being asked.")
			return exitOK
		}
	}

	req.Confirm = true
	appliedBody, err := api.post(path, req)
	if err != nil {
		return c.fail(err)
	}
	if asJSON {
		return c.printJSON(appliedBody)
	}
	var applied widenScopeResponse
	if err := json.Unmarshal(appliedBody, &applied); err != nil {
		return c.fail(fmt.Errorf("endpoint scope widen: malformed response: %w", err))
	}
	fmt.Fprintf(c.stdout, "Widened %s (%s). Its credential, ingest URLs and signing secrets are unchanged.\n",
		applied.Slug, applied.AgentName)
	fmt.Fprintln(c.stdout, "Routing that was refused for being outside the grant now validates; nothing else moved.")
	return exitOK
}

// printScopeChange renders the confirmation: one line per field that moves, old then new.
func printScopeChange(c *cli, out widenScopeResponse) {
	fmt.Fprintf(c.stdout, "%s (%s): %d widening%s to apply.\n", out.Slug, out.AgentName,
		len(out.Widenings), plural(len(out.Widenings)))
	for _, w := range out.Widenings {
		fmt.Fprintf(c.stdout, "  %s\n", w.Field)
		fmt.Fprintf(c.stdout, "    was  %s\n", scopeValueText(w.Before))
		fmt.Fprintf(c.stdout, "    now  %s\n", scopeValueText(w.After))
	}
}

// scopeValueText renders one audit value: the list joined, or the number.
func scopeValueText(v any) string {
	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, fmt.Sprint(item))
		}
		if len(parts) == 0 {
			return "(none)"
		}
		return strings.Join(parts, " ")
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case nil:
		return "(none)"
	}
	return fmt.Sprint(v)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
