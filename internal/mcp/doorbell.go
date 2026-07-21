package mcp

// Channel doorbells over the Streamable HTTP notification stream.
//
// Governing: ADR-0013 (push is a doorbell, the durable queue is the ledger), SPEC-0014 REQ
// "Channels Push over the HTTP Stream", SPEC-0011 REQ "Push Notification Shape", "Scope-Filtered
// Fan-Out", "Best-Effort Lossy Delivery and Degradation to Pull", "Sender Gate and Injection
// Safety", "Error Handling Standards".

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/joestump/switchboard/internal/store"
)

const (
	// notificationChannel is the Claude Code Channels doorbell method: the harness registers a
	// channel listener against the experimental "claude/channel" capability and surfaces these
	// notifications in-session as <channel source="switchboard"> events.
	notificationChannel = "notifications/claude/channel"

	// doorbellBuffer bounds each session's fan-out buffer. A full buffer drops the push for that
	// subscriber — never blocks the publisher — because the queue is the ledger and the push is
	// only a doorbell (SPEC-0011 scenario "Slow subscriber is dropped, not blocked").
	doorbellBuffer = 16
)

// PublishTodoReady fans one committed, push-eligible todo out to every attached session whose
// vended scope covers the todo's queue. The caller (the store's doorbell hook) has already applied
// the sender gate: only verified, human-attributed todos reach this point. Delivery is lossy and
// non-blocking by design: no session, no open stream, or a full buffer all degrade to pull, where
// list_todos returns the todo unchanged.
//
// Governing: ADR-0021 — the PRIMARY filter is endpoint ownership. A session minted under endpoint
// A MUST NEVER receive a doorbell for a todo owned by endpoint B, even when both share a queue
// name. Queue membership (s.queues) remains as a secondary intra-endpoint scope (SPEC-0011
// "Scope-Filtered Fan-Out" at a finer grain). System todos (empty EndpointID, e.g. friend
// approvals) are never pushed — they are operator-Board-only.
func (h *Handler) PublishTodoReady(t store.Todo) {
	if t.EndpointID == "" {
		return // system todo: never push to agent sessions
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		// Tenant boundary (ADR-0021): a session only ever receives doorbells for todos pinned to
		// its own endpoint. This is the cross-tenant isolation check; it runs first.
		if s.endpointID != t.EndpointID {
			continue
		}
		// Governing: SPEC-0011 REQ "Scope-Filtered Fan-Out" — sessions never receive doorbells
		// for queues outside their endpoint's grant.
		if !s.queues[t.Queue] {
			continue
		}
		select {
		case s.doorbells <- t:
		default: // slow subscriber: drop — the todo remains recoverable by pull
		}
	}
}

// pump drains one session's doorbell buffer onto its transport connection. A write with a
// detached context is routed to the session's standalone SSE stream by the SDK transport; when no
// stream is open the write is rejected and the doorbell is dropped with a log line — never
// retried into a blocking path (SPEC-0011 REQ "Error Handling Standards"). The pump exits when
// reapOnClose closes the buffer after the session ends.
func (h *Handler) pump(s *mcpSession) {
	defer h.wg.Done()
	for t := range s.doorbells {
		msg, err := channelNotification(t)
		if err != nil {
			h.log.Error("mcp doorbell encode", "slug", s.slug, "todo_id", t.ID, "err", err)
			continue
		}
		if err := s.conn.Write(context.Background(), msg); err != nil {
			// Expected whenever the agent holds no open notification stream: the push is only a
			// hint, so it is logged and dropped, and the todo stays pending for the pull loop.
			h.log.Debug("mcp doorbell dropped", "slug", s.slug, "todo_id", t.ID, "err", err)
		} else {
			h.log.Debug("mcp doorbell delivered", "slug", s.slug, "todo_id", t.ID, "queue", t.Queue)
		}
	}
}

// channelClose matches a literal </channel> close tag in any case, so payload-derived text can
// never break out of the harness's <channel> wrapper (SPEC-0011 scenario "Payload cannot break out
// of the channel wrapper").
var channelClose = regexp.MustCompile(`(?i)</channel>`)

// neutralize rewrites </channel> to a safe substitute and collapses newlines so the summary stays
// a single legible line and cannot forge additional stream frames.
func neutralize(s string) string {
	s = channelClose.ReplaceAllString(s, "«/channel»")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// channelNotification builds the doorbell JSON-RPC notification for a ready todo. content is a
// one-line summary; meta carries snake_case routing identifiers only (todo_id and queue always;
// kind and source when present). The notification carries no lease and inlines no payload or
// secret values — the agent claims through the durable verbs.
// Governing: SPEC-0011 REQ "Push Notification Shape".
func channelNotification(t store.Todo) (*jsonrpc.Request, error) {
	meta := map[string]string{
		"todo_id": neutralize(t.ID),
		"queue":   neutralize(t.Queue),
	}
	if t.Kind != "" {
		meta["kind"] = neutralize(t.Kind)
	}
	if t.Source != "" {
		meta["source"] = neutralize(t.Source)
	}
	content := fmt.Sprintf("switchboard: todo %s ready on queue %q — %s",
		neutralize(t.ID), neutralize(t.Queue), neutralize(t.Title))
	params, err := json.Marshal(map[string]any{"content": content, "meta": meta})
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal channel notification: %w", err)
	}
	// A jsonrpc.Request with a zero ID is a notification on the wire.
	return &jsonrpc.Request{Method: notificationChannel, Params: params}, nil
}
