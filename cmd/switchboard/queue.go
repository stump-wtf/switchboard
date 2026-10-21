package main

// Queue Verbs
//
// `switchboard queue ls|pause|resume` drive the human API's queue routes (ADR-0048) as the logged-in
// human, so an operator can quiesce a queue's delivery from a terminal while a downstream consumer
// drains: inbound webhooks keep landing as pending todos — nothing is dropped or failed — but claim,
// claim_next, the doorbell and the wake hooks hand out nothing for a paused queue. Resume re-rings
// the backlog. Queues are a global namespace, so a pause covers every endpoint that drains the queue.
//
// Every verb takes --json, which prints the API response verbatim.
//
// Governing: ADR-0048 (proposed), SPEC-0035 REQ "CLI Parity".
//
// @joestump-agent 10/20/2026 - Added for stump.wtf/switchboard#581.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
)

// queuesDoc is GET /api/v1/queues (internal/server api_queue.go).
type queuesDoc struct {
	Queues []struct {
		Queue    string `json:"queue"`
		Pending  int64  `json:"pending"`
		Claimed  int64  `json:"claimed"`
		Paused   bool   `json:"paused"`
		Reason   string `json:"reason,omitempty"`
		PausedAt string `json:"paused_at,omitempty"`
	} `json:"queues"`
}

// queuePath escapes a queue name into one path segment; queue names are free text.
func queuePath(queue string) string {
	return "/api/v1/queues/" + url.PathEscape(queue)
}

func cmdQueueLs(c *cli, args []string) int {
	fs := c.flagSet("queue ls", "", "List every known queue with its live counts and whether it is paused.\n"+
		"A paused queue keeps accepting webhooks; nothing is delivered from it until resume.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	if _, exit, ok := c.parseArgs(fs, args); !ok {
		return exit
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.get("/api/v1/queues")
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var doc queuesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return c.fail(err)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "QUEUE\tPENDING\tCLAIMED\tSTATE")
	for _, q := range doc.Queues {
		state := "live"
		if q.Paused {
			state = "PAUSED"
			if r := strings.TrimSpace(q.Reason); r != "" {
				state += " — " + r
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", q.Queue, q.Pending, q.Claimed, state)
	}
	if err := tw.Flush(); err != nil {
		return c.fail(err)
	}
	return exitOK
}

func cmdQueuePause(c *cli, args []string) int {
	fs := c.flagSet("queue pause", "QUEUE", "Stop delivering work from QUEUE without stopping intake: webhooks keep landing as\n"+
		"pending todos, but claims and wakes hand out nothing until you resume. In-flight claims\n"+
		"finish. Resume re-rings whatever piled up.")
	reason := fs.String("reason", "", "why the queue is paused (shown by queue ls)")
	asJSON := fs.Bool("json", false, "print the raw API response")
	positional, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return c.usageError(fs, "queue pause needs one QUEUE")
	}
	queue := strings.TrimSpace(positional[0])
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.post(queuePath(queue)+"/pause", map[string]any{"reason": *reason})
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	fmt.Fprintf(c.stdout, "Queue %s is paused: intake continues, delivery waits.\n", queue)
	fmt.Fprintf(c.stdout, "Resume with `switchboard queue resume %s` — the backlog re-rings then.\n", queue)
	return exitOK
}

func cmdQueueResume(c *cli, args []string) int {
	fs := c.flagSet("queue resume", "QUEUE", "Lift a queue's pause and re-ring its backlog. Resuming a queue that was never paused\n"+
		"is not an error; it simply was not paused.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	positional, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return c.usageError(fs, "queue resume needs one QUEUE")
	}
	queue := strings.TrimSpace(positional[0])
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.post(queuePath(queue)+"/resume", nil)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var out struct {
		WasPaused bool `json:"was_paused"`
	}
	if err := json.Unmarshal(body, &out); err == nil && !out.WasPaused {
		fmt.Fprintf(c.stdout, "Queue %s was not paused; it stays live.\n", queue)
		return exitOK
	}
	fmt.Fprintf(c.stdout, "Queue %s is live again; its backlog re-rang.\n", queue)
	return exitOK
}
