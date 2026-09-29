package main

// Webhook And Routing-Rule Verbs
//
// `switchboard webhook list` and `switchboard webhook rules get|test|set` drive the human API's
// webhook routes (SPEC-0035) as the logged-in human, so a webhook's rules can be read, dry-run and
// replaced from a terminal whatever the owning endpoint's MCP scope allows. That is the case for
// every webhook owned by an endpoint vended before the rule verbs existed.
//
// The file format is the API's own: `rules get --json` prints a document that `rules set --file`
// and `rules test --file` accept unchanged (they ignore the read-only webhook_id, grant and so on).
// Saving without a params key keeps the stored params; "params": null clears them. JSON inputs
// accept a path, @path, or - for stdin.
//
// @joestump-agent 09/29/2026 - Added webhook list and rules get/test/set.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// --- wire shapes (internal/manage RulesOut / TestOut, docs/reference/openapi.yaml) ---

type ruleAction struct {
	Queue     string   `json:"queue"`
	Drop      bool     `json:"drop"`
	Endpoints []string `json:"endpoints"`
	Exclusive bool     `json:"exclusive"`
	Once      bool     `json:"once"`
	WorkOrder bool     `json:"work_order"`
}

type rulesDoc struct {
	WebhookID     string      `json:"webhook_id"`
	SourceType    string      `json:"source_type"`
	TargetQueue   string      `json:"target_queue"`
	DefaultAction *ruleAction `json:"default_action"`
	Rules         []struct {
		ID     string     `json:"id"`
		Name   string     `json:"name"`
		Expr   string     `json:"expr"`
		Action ruleAction `json:"action"`
	} `json:"rules"`
	Params map[string]any `json:"params"`
	Grant  struct {
		Queues    []string `json:"queues"`
		Endpoints []string `json:"endpoints"`
	} `json:"grant"`
}

func rulesURL(id string) string { return "/api/v1/webhooks/" + url.PathEscape(id) + "/rules" }

// --- webhook list ---

func cmdWebhookList(c *cli, args []string) int {
	fs := c.flagSet("webhook list", "", "List the webhooks your endpoints own, with their routing-rule counts. Ingest URLs and\n"+
		"signing secrets are never shown: they are revealed once, at create or rotate.")
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
	body, err := api.get("/api/v1/webhooks")
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var doc struct {
		Webhooks []struct {
			WebhookID     string `json:"webhook_id"`
			EndpointSlug  string `json:"endpoint_slug"`
			EndpointState string `json:"endpoint_state"`
			SourceType    string `json:"source_type"`
			TargetQueue   string `json:"target_queue"`
			RuleCount     int    `json:"rule_count"`
			HasDefault    bool   `json:"has_default_action"`
			HasParams     bool   `json:"has_params"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return c.fail(fmt.Errorf("webhook list: malformed response: %w", err))
	}
	if len(doc.Webhooks) == 0 {
		fmt.Fprintln(c.stdout, "No webhooks yet. An agent creates one with create_webhook, and `endpoint vend` mints one.")
		return exitOK
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WEBHOOK\tENDPOINT\tSTATE\tSOURCE\tQUEUE\tRULES")
	for _, w := range doc.Webhooks {
		rules := strconv.Itoa(w.RuleCount)
		var extra []string
		if w.HasDefault {
			extra = append(extra, "default")
		}
		if w.HasParams {
			extra = append(extra, "params")
		}
		if len(extra) > 0 {
			rules += " (+" + strings.Join(extra, ", ") + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", w.WebhookID, w.EndpointSlug, w.EndpointState, w.SourceType, w.TargetQueue, rules)
	}
	_ = tw.Flush()
	return exitOK
}

// --- webhook rules get ---

func cmdWebhookRulesGet(c *cli, args []string) int {
	fs := c.flagSet("webhook rules get", "WEBHOOK_ID", "Show a webhook's routing rules in evaluation order (first match wins), its default action,\n"+
		"its params, and the queues and endpoints its rules may reach. --json prints a document that\n"+
		"`webhook rules set --file` accepts as it stands.")
	asJSON := fs.Bool("json", false, "print the raw API response (a valid input for `rules set`)")
	id, exit, ok := c.webhookArg(fs, args)
	if !ok {
		return exit
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.get(rulesURL(id))
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	return c.printRules(body, "")
}

// --- webhook rules set ---

func cmdWebhookRulesSet(c *cli, args []string) int {
	fs := c.flagSet("webhook rules set", "WEBHOOK_ID", "Replace a webhook's whole routing configuration from a file: {\"rules\": [...],\n"+
		"\"default_action\": {...}, \"params\": {...}} — the shape `webhook rules get --json` prints. The save\n"+
		"is validated, dry-run against the webhook's 50 latest deliveries, and refused (keeping the\n"+
		"current rules) if anything fails. A file without a params key KEEPS the stored params;\n"+
		"\"params\": null clears them.")
	file := fs.String("file", "", "the rules document: a path, @path, or - for stdin (required)")
	fs.StringVar(file, "f", "", "shorthand for --file")
	asJSON := fs.Bool("json", false, "print the raw API response")
	id, exit, ok := c.webhookArg(fs, args)
	if !ok {
		return exit
	}
	if *file == "" {
		return c.usageError(fs, "--file is required (the rules document, e.g. from `webhook rules get --json`)")
	}
	raw, doc, err := c.readJSONObject("--file", *file)
	if err != nil {
		return c.usageError(fs, err.Error())
	}
	if _, has := doc["rules"]; !has {
		return c.usageError(fs, `--file has no "rules" key: give the complete ordered list, or [] to remove every rule`)
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	// The document goes as written, so nothing (a large number, key order) is re-encoded on the way.
	body, err := api.put(rulesURL(id), json.RawMessage(raw))
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	note := "Saved."
	if _, has := doc["params"]; !has {
		note = "Saved (the file has no params, so the stored params were kept)."
	}
	return c.printRules(body, note)
}

// --- webhook rules test ---

// headerFlags collects repeated --header "Name: value" flags.
type headerFlags map[string]string

func (h headerFlags) String() string { return "" }

func (h headerFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok || strings.TrimSpace(k) == "" {
		return errors.New(`want "Name: value"`)
	}
	h[strings.TrimSpace(k)] = strings.TrimSpace(val)
	return nil
}

func cmdWebhookRulesTest(c *cli, args []string) int {
	fs := c.flagSet("webhook rules test", "WEBHOOK_ID", "Dry-run routing: evaluate candidate rules (--file), or the saved ones, against one of the\n"+
		"webhook's stored deliveries (--event) or a sample payload (--payload), and print where it would\n"+
		"go and why. Saves nothing. A faulted result is blocking: that delivery would be routed nowhere.")
	file := fs.String("file", "", "candidate rules document (the `rules get --json` shape): a path, @path, or - for stdin; omit to test the saved rules")
	fs.StringVar(file, "f", "", "shorthand for --file")
	event := fs.Int64("event", 0, "route this stored delivery of the webhook (its event id)")
	payload := fs.String("payload", "", "route this sample body instead: a path, @path, or - for stdin")
	headers := headerFlags{}
	fs.Var(headers, "header", `a sample request header for --payload, "Name: value" (repeatable; e.g. "X-GitHub-Event: issues")`)
	envelope := fs.Bool("envelope", false, "also print the envelope the rules evaluated")
	asJSON := fs.Bool("json", false, "print the raw API response")
	id, exit, ok := c.webhookArg(fs, args)
	if !ok {
		return exit
	}
	if (*event > 0) == (*payload != "") {
		return c.usageError(fs, "give exactly one of --event EVENT_ID or --payload FILE")
	}
	if *file == "-" && *payload == "-" {
		return c.usageError(fs, "--file and --payload cannot both read stdin")
	}
	body := map[string]any{}
	if *file != "" {
		_, doc, err := c.readJSONObject("--file", *file)
		if err != nil {
			return c.usageError(fs, err.Error())
		}
		for _, k := range []string{"rules", "default_action"} {
			if v, has := doc[k]; has {
				body[k] = v
			}
		}
		// The same params semantics as `rules set`: absent keeps the stored params, null clears them
		// (a dry run treats an absent or null params as "the saved ones", so a clear is sent as {}).
		if v, has := doc["params"]; has {
			if strings.TrimSpace(string(v)) == "null" {
				v = json.RawMessage(`{}`)
			}
			body["params"] = v
		}
	}
	if *event > 0 {
		body["event_id"] = *event
	} else {
		raw, err := c.readJSONInput("--payload", *payload)
		if err != nil {
			return c.usageError(fs, err.Error())
		}
		body["payload"] = json.RawMessage(raw)
		if len(headers) > 0 {
			body["headers"] = map[string]string(headers)
		}
	}
	if !*envelope && !*asJSON {
		body["omit_envelope"] = true
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	resp, err := api.post(rulesURL(id)+"/test", body)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(resp)
	}
	return c.printDecision(resp)
}

// --- shared helpers ---

// webhookArg parses a rules verb's flags and its one WEBHOOK_ID argument.
func (c *cli) webhookArg(fs *flag.FlagSet, args []string) (string, int, bool) {
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return "", exit, false
	}
	switch {
	case len(pos) == 0 || strings.TrimSpace(pos[0]) == "":
		return "", c.usageError(fs, "a webhook id is required (see `switchboard webhook list`)"), false
	case len(pos) > 1:
		return "", c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1])), false
	}
	return strings.TrimSpace(pos[0]), exitOK, true
}

// readJSONInput reads a JSON document from a path, @path, or - (stdin), and checks it is JSON.
func (c *cli) readJSONInput(flagName, arg string) ([]byte, error) {
	var b []byte
	var err error
	if arg == "-" {
		if c.stdin == nil {
			return nil, fmt.Errorf("%s -: no stdin", flagName)
		}
		b, err = io.ReadAll(c.stdin)
	} else {
		read := c.readFile
		if read == nil {
			read = os.ReadFile
		}
		b, err = read(strings.TrimPrefix(arg, "@"))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flagName, err)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("%s: %s is not JSON", flagName, arg)
	}
	return b, nil
}

// readJSONObject is readJSONInput for a document that must be a JSON object. It returns the bytes
// as read and the top-level members, each kept verbatim.
func (c *cli) readJSONObject(flagName, arg string) ([]byte, map[string]json.RawMessage, error) {
	b, err := c.readJSONInput(flagName, arg)
	if err != nil {
		return nil, nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil || doc == nil {
		return nil, nil, fmt.Errorf("%s: %s must be a JSON object", flagName, arg)
	}
	return b, doc, nil
}

// printRules renders a rules document for a person: header, the ordered rules (work-order rules
// marked), the default, params and grant.
func (c *cli) printRules(body []byte, note string) int {
	var d rulesDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return c.fail(fmt.Errorf("webhook rules: malformed response: %w", err))
	}
	if note != "" {
		fmt.Fprintln(c.stdout, note)
	}
	fmt.Fprintf(c.stdout, "Webhook %s (%s, target queue %s)\n\n", d.WebhookID, d.SourceType, d.TargetQueue)
	if len(d.Rules) == 0 {
		fmt.Fprintln(c.stdout, "No rules: every delivery takes the default.")
	} else {
		tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tID\tNAME\tACTION\tEXPR")
		for i, r := range d.Rules {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", i, r.ID, r.Name, describeAction(r.Action), r.Expr)
		}
		_ = tw.Flush()
	}
	fmt.Fprintln(c.stdout)
	if d.DefaultAction != nil {
		fmt.Fprintf(c.stdout, "Default:  %s\n", describeAction(*d.DefaultAction))
	} else {
		fmt.Fprintf(c.stdout, "Default:  queue %s (the webhook's target queue, every target)\n", d.TargetQueue)
	}
	if len(d.Params) == 0 {
		fmt.Fprintln(c.stdout, "Params:   none")
	} else {
		keys := make([]string, 0, len(d.Params))
		for k := range d.Params {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fmt.Fprintln(c.stdout, "Params:")
		for _, k := range keys {
			v, _ := json.Marshal(d.Params[k])
			fmt.Fprintf(c.stdout, "  %s = %s\n", k, v)
		}
	}
	fmt.Fprintf(c.stdout, "Grant:    queues %s; endpoints %s\n", orNone(d.Grant.Queues), orNone(d.Grant.Endpoints))
	return exitOK
}

// describeAction renders an action on one line; a work-order action says so, since it is
// switchboard vouching for the delivery (SPEC-0035 REQ "CLI Parity").
func describeAction(a ruleAction) string {
	if a.Drop {
		return "drop"
	}
	s := "queue " + a.Queue
	var flags []string
	if a.WorkOrder {
		flags = append(flags, "WORK ORDER")
	}
	if a.Exclusive {
		flags = append(flags, "exclusive")
	}
	if a.Once {
		flags = append(flags, "once")
	}
	if len(a.Endpoints) > 0 {
		flags = append(flags, "endpoints "+strings.Join(a.Endpoints, ","))
	}
	if len(flags) > 0 {
		s += " [" + strings.Join(flags, ", ") + "]"
	}
	return s
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// printDecision renders a dry run for a person: the outcome first, then why.
func (c *cli) printDecision(body []byte) int {
	var d struct {
		Decision struct {
			Drop        bool     `json:"drop"`
			Queue       string   `json:"queue"`
			Endpoints   []string `json:"endpoints"`
			Faulted     bool     `json:"faulted"`
			Unavailable bool     `json:"unavailable"`
			Fault       *struct {
				RuleID string `json:"rule_id"`
				Cause  string `json:"cause"`
				Detail string `json:"detail"`
			} `json:"fault"`
		} `json:"decision"`
		Trace struct {
			Stage    string `json:"stage"`
			Cause    string `json:"cause"`
			RuleID   string `json:"rule_id"`
			RuleName string `json:"rule_name"`
		} `json:"trace"`
		OnceKey   string          `json:"once_key"`
		WorkOrder json.RawMessage `json:"work_order"`
		Envelope  json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return c.fail(fmt.Errorf("webhook rules test: malformed response: %w", err))
	}
	dec := d.Decision
	switch {
	case dec.Unavailable:
		fmt.Fprintln(c.stdout, "Unavailable: the rule evaluator could not run, so this says nothing about the rules. Retry.")
	case dec.Faulted:
		fmt.Fprintln(c.stdout, "FAULTED (blocking): this delivery would be recorded and routed nowhere.")
		if dec.Fault != nil {
			fmt.Fprintf(c.stdout, "  rule %s: %s", dec.Fault.RuleID, dec.Fault.Cause)
			if dec.Fault.Detail != "" {
				fmt.Fprintf(c.stdout, " — %s", dec.Fault.Detail)
			}
			fmt.Fprintln(c.stdout)
		}
	case dec.Drop:
		fmt.Fprintln(c.stdout, "Dropped: recorded, no todo, no doorbell.")
	default:
		fmt.Fprintf(c.stdout, "Routed to queue %s on %s.\n", dec.Queue, orNone(dec.Endpoints))
	}
	switch {
	case d.Trace.RuleID != "":
		label := d.Trace.RuleID
		if d.Trace.RuleName != "" {
			label += " (" + d.Trace.RuleName + ")"
		}
		fmt.Fprintf(c.stdout, "Matched:  rule %s\n", label)
	case d.Trace.Stage != "":
		fmt.Fprintf(c.stdout, "Matched:  %s", d.Trace.Stage)
		if d.Trace.Cause != "" {
			fmt.Fprintf(c.stdout, " (%s)", d.Trace.Cause)
		}
		fmt.Fprintln(c.stdout)
	}
	if d.OnceKey != "" {
		fmt.Fprintf(c.stdout, "Once key: %s\n", d.OnceKey)
	}
	if len(d.WorkOrder) > 0 && string(d.WorkOrder) != "null" {
		fmt.Fprintln(c.stdout, "Work order: attached (run with --json to see it)")
	}
	if len(d.Envelope) > 0 && string(d.Envelope) != "null" {
		fmt.Fprintln(c.stdout, "\nEnvelope:")
		return c.printJSON(d.Envelope)
	}
	return exitOK
}
