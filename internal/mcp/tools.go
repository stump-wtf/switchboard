package mcp

// This file is the SPEC-0006 agent verb registry served over MCP: list_todos, claim, complete,
// fail, heartbeat — thin wrappers over internal/store with SDK-declared input/output schemas and
// structured outputs. Tool semantics (lifecycle, lease, retry) defer entirely to SPEC-0003; this
// layer only enforces scope at the boundary and maps store sentinels onto the SPEC-0006 stable
// error codes.
//
// Governing: SPEC-0014 REQ "Agent Tool Surface over MCP", SPEC-0006 REQ "Todo Drain Verbs",
// SPEC-0006 REQ "Lease Lifecycle and Crash Safety".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
)

const (
	defaultLeaseTTL = 5 * time.Minute
	maxLeaseTTL     = 24 * time.Hour
)

// Stable machine error codes (SPEC-0006 REQ "Structured Output and Stable Error Shape").
const (
	codeForbidden = "forbidden"
	codeNotFound  = "not_found"
	codeConflict  = "conflict"
	codeInternal  = "internal"
)

// agentVerbs is the full SPEC-0006 drain-verb surface (verbs.go DrainVerbs). A tools/call naming
// one of these outside the endpoint's allowlist is a scope violation (distinguishable from an
// unknown tool); anything else is left to the SDK's unknown-tool protocol error.
var agentVerbs = verbSet(DrainVerbs())

// toolError is the client-visible tool failure: a stable machine `code` plus a human message that
// never carries secret material or internal error text. Error() renders "code: message" — exactly
// what the MCP client receives in the error content. Governing: SPEC-0006 REQ "Structured Output
// and Stable Error Shape", SPEC-0014 REQ "Error Handling Standards".
type toolError struct {
	code string
	msg  string
}

func (e *toolError) Error() string { return e.code + ": " + e.msg }

// errForbidden is the sentinel for scope violations (verb outside the allowlist or queue outside
// the grant); store.ErrNotFound and store.ErrConflict are the not-found and lease-conflict
// sentinels, and unauthorized is refused by the HTTP middleware before the tool layer runs.
// Governing: SPEC-0014 REQ "Error Handling Standards" (sentinel errors).
var errForbidden = errors.New("mcp: forbidden")

// --- tool input/output shapes (SDK-inferred JSON schemas) ---

// todoOut is the structured todo row every verb returns: at minimum id, queue, state, and attempt
// (SPEC-0006 REQ "Todo Drain Verbs"), matching the retired /agent/* JSON shape.
type todoOut struct {
	ID             string `json:"id" jsonschema:"the todo id"`
	Queue          string `json:"queue" jsonschema:"the queue the todo belongs to"`
	Source         string `json:"source,omitempty" jsonschema:"originating source, if any"`
	Kind           string `json:"kind,omitempty" jsonschema:"todo kind, if any"`
	Title          string `json:"title" jsonschema:"one-line summary"`
	State          string `json:"state" jsonschema:"lifecycle state: pending, claimed, done, or failed"`
	Owner          string `json:"owner,omitempty" jsonschema:"current lease owner (agent:<agent_id>)"`
	Assignee       string `json:"assignee,omitempty" jsonschema:"pinned assignee, if any"`
	Attempt        int    `json:"attempt" jsonschema:"attempts consumed so far"`
	MaxAttempts    int    `json:"max_attempts" jsonschema:"attempt budget before dead-letter"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty" jsonschema:"RFC 3339 lease expiry while claimed"`
	CreatedAt      string `json:"created_at" jsonschema:"RFC 3339 creation time"`
	Payload        any    `json:"payload,omitempty" jsonschema:"the todo's JSON payload"`
}

type listTodosIn struct {
	Queue string `json:"queue,omitempty" jsonschema:"restrict to one granted queue (default: all granted queues)"`
	State string `json:"state,omitempty" jsonschema:"filter by state: pending, claimed, done, or failed"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum rows to return (default 50, cap 200)"`
}

type listTodosOut struct {
	Todos []todoOut `json:"todos" jsonschema:"todo rows in the endpoint's granted queues, newest first"`
}

type claimIn struct {
	ID              string `json:"id" jsonschema:"the todo id to claim"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds,omitempty" jsonschema:"lease TTL in seconds (default 300)"`
}

type completeIn struct {
	ID     string `json:"id" jsonschema:"the claimed todo id to complete"`
	Result any    `json:"result,omitempty" jsonschema:"optional JSON result recorded on the todo"`
}

type failIn struct {
	ID     string `json:"id" jsonschema:"the claimed todo id to fail (retries until attempts are exhausted, then dead-letters)"`
	Result any    `json:"result,omitempty" jsonschema:"optional JSON failure detail recorded on the todo"`
}

type heartbeatIn struct {
	ID              string `json:"id" jsonschema:"the claimed todo id whose lease to extend"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds,omitempty" jsonschema:"new lease TTL in seconds from now (default 300)"`
}

// registerTools installs the endpoint's allowlisted SPEC-0006 verbs on the per-session server.
// tools/list therefore advertises exactly the allowlist — a verb outside it is never registered.
// Governing: SPEC-0014 REQ "Agent Tool Surface over MCP" (scope filters the advertised tools).
func (h *Handler) registerTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "list_todos") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "list_todos",
			Description: "List todos in this endpoint's granted queues, newest first.",
		}, h.listTodosTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "claim") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "claim",
			Description: "Atomically claim a pending todo, acquiring a time-bounded lease.",
		}, h.claimTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "complete") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "complete",
			Description: "Complete a todo this endpoint holds, transitioning it to done.",
		}, h.completeTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "fail") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "fail",
			Description: "Fail a claimed todo: retried while attempts remain, dead-lettered when exhausted.",
		}, h.failTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "heartbeat") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "heartbeat",
			Description: "Extend the lease on a claimed todo this endpoint holds.",
		}, h.heartbeatTool(ep))
	}
}

// scopeGuard intercepts tools/call before dispatch: a known verb (SPEC-0006 agent verb or
// SPEC-0005 event-history tool) outside the endpoint's allowlist returns a stable scope error
// (code "forbidden") with no side effects, rather than the SDK's unknown-tool protocol error.
// Governing: SPEC-0006 REQ "Scope Enforcement at the Boundary".
func (h *Handler) scopeGuard(ep store.AuthEndpoint) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/call" {
				if p, ok := req.GetParams().(*sdk.CallToolParamsRaw); ok &&
					(agentVerbs[p.Name] || eventVerbs[p.Name] || webhookVerbs[p.Name]) && !hasScope(ep.ScopeVerbs, p.Name) {
					h.log.Warn("mcp verb out of scope", "slug", ep.Slug, "tool", p.Name,
						"err", fmt.Errorf("tools/call %s: %w", p.Name, errForbidden))
					res := &sdk.CallToolResult{}
					res.SetError(&toolError{codeForbidden, "verb " + p.Name + " not in this endpoint's scope"})
					return res, nil
				}
			}
			return next(ctx, method, req)
		}
	}
}

// --- verb handlers (thin wrappers over internal/store; lifecycle semantics are SPEC-0003's) ---

func (h *Handler) listTodosTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[listTodosIn, listTodosOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in listTodosIn) (*sdk.CallToolResult, listTodosOut, error) {
		queues := ep.ScopeQueues
		if in.Queue != "" {
			// Queue scope enforced at the boundary, before the store sees the call.
			if !hasScope(ep.ScopeQueues, in.Queue) {
				return nil, listTodosOut{}, &toolError{codeForbidden, "queue " + in.Queue + " not in this endpoint's scope"}
			}
			queues = []string{in.Queue}
		}
		todos, err := h.store.ListTodos(ctx, ep.ID, queues, in.State, in.Limit)
		if err != nil {
			return nil, listTodosOut{}, h.mapStoreErr(ep, "list_todos", err)
		}
		out := listTodosOut{Todos: make([]todoOut, 0, len(todos))}
		for _, t := range todos {
			out.Todos = append(out.Todos, toOut(t))
		}
		return nil, out, nil
	}
}

func (h *Handler) claimTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[claimIn, todoOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in claimIn) (*sdk.CallToolResult, todoOut, error) {
		if err := h.guardQueue(ctx, ep, "claim", in.ID); err != nil {
			return nil, todoOut{}, err
		}
		t, err := h.store.ClaimTodo(ctx, ep.ID, in.ID, owner(ep), leaseTTL(in.LeaseTTLSeconds))
		if err != nil {
			return nil, todoOut{}, h.mapStoreErr(ep, "claim", err)
		}
		return nil, toOut(t), nil
	}
}

func (h *Handler) completeTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[completeIn, todoOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in completeIn) (*sdk.CallToolResult, todoOut, error) {
		if err := h.guardQueue(ctx, ep, "complete", in.ID); err != nil {
			return nil, todoOut{}, err
		}
		t, err := h.store.CompleteTodo(ctx, ep.ID, in.ID, owner(ep), rawJSON(in.Result))
		if err != nil {
			return nil, todoOut{}, h.mapStoreErr(ep, "complete", err)
		}
		return nil, toOut(t), nil
	}
}

func (h *Handler) failTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[failIn, todoOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in failIn) (*sdk.CallToolResult, todoOut, error) {
		if err := h.guardQueue(ctx, ep, "fail", in.ID); err != nil {
			return nil, todoOut{}, err
		}
		t, err := h.store.FailTodo(ctx, ep.ID, in.ID, owner(ep), rawJSON(in.Result))
		if err != nil {
			return nil, todoOut{}, h.mapStoreErr(ep, "fail", err)
		}
		return nil, toOut(t), nil
	}
}

func (h *Handler) heartbeatTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[heartbeatIn, todoOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in heartbeatIn) (*sdk.CallToolResult, todoOut, error) {
		if err := h.guardQueue(ctx, ep, "heartbeat", in.ID); err != nil {
			return nil, todoOut{}, err
		}
		t, err := h.store.HeartbeatTodo(ctx, ep.ID, in.ID, owner(ep), leaseTTL(in.LeaseTTLSeconds))
		if err != nil {
			return nil, todoOut{}, h.mapStoreErr(ep, "heartbeat", err)
		}
		return nil, toOut(t), nil
	}
}

// guardQueue enforces the queue grant before any state-changing store call: the target todo is
// read (never mutated) under the endpoint's tenant scope (ADR-0021) and its queue checked against
// the endpoint's grant. Out-of-scope targets are refused with the stable forbidden code and no side
// effects. The endpoint_id predicate is the tenant boundary; the queue check is a secondary
// intra-endpoint scope.
// Governing: SPEC-0006 REQ "Scope Enforcement at the Boundary", ADR-0021.
func (h *Handler) guardQueue(ctx context.Context, ep store.AuthEndpoint, tool, id string) error {
	t, err := h.store.GetTodo(ctx, ep.ID, id)
	if err != nil {
		return h.mapStoreErr(ep, tool, err)
	}
	if !hasScope(ep.ScopeQueues, t.Queue) {
		h.log.Warn("mcp todo queue out of scope", "slug", ep.Slug, "tool", tool,
			"err", fmt.Errorf("todo %s in queue %s: %w", id, t.Queue, errForbidden))
		return &toolError{codeForbidden, "todo's queue not in this endpoint's scope"}
	}
	return nil
}

// mapStoreErr maps store sentinels onto the stable SPEC-0006 codes. Unexpected failures reach the
// client only as the generic "internal" code while the log records the endpoint slug, the tool,
// and the wrapped error chain — never the bearer credential (it never enters this layer).
// Governing: SPEC-0014 REQ "Error Handling Standards".
func (h *Handler) mapStoreErr(ep store.AuthEndpoint, tool string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &toolError{codeNotFound, "todo not found"}
	case errors.Is(err, store.ErrConflict):
		return &toolError{codeConflict, "todo not in the expected state or lease not held"}
	default:
		h.log.Error("mcp tool store failure", "slug", ep.Slug, "tool", tool,
			"err", fmt.Errorf("tools/call %s: %w", tool, err))
		return &toolError{codeInternal, "internal error"}
	}
}

// --- helpers ---

func toOut(t store.Todo) todoOut {
	out := todoOut{
		ID: t.ID, Queue: t.Queue, Source: t.Source, Kind: t.Kind, Title: t.Title, State: t.State,
		Owner: t.Owner, Assignee: t.Assignee, Attempt: t.Attempt, MaxAttempts: t.MaxAttempts,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.LeaseExpiresAt != nil {
		out.LeaseExpiresAt = t.LeaseExpiresAt.UTC().Format(time.RFC3339)
	}
	if len(t.Payload) > 0 {
		var v any
		// A payload that fails to parse is omitted from the structured row rather than failing the
		// verb — the todo row itself (id/queue/state/attempt) is the contract, the payload a bonus.
		if err := json.Unmarshal(t.Payload, &v); err == nil {
			out.Payload = v
		}
	}
	return out
}

// owner is the acting identity recorded on claimed/completed todos (SPEC-0006: agent:<agent_id>).
func owner(ep store.AuthEndpoint) string { return "agent:" + ep.AgentID }

func hasScope(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// leaseTTL clamps a caller-supplied TTL to sane bounds, defaulting when absent (SPEC-0006 default
// lease TTL; the cap keeps a typo from parking a todo for a year).
func leaseTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultLeaseTTL
	}
	d := time.Duration(seconds) * time.Second
	if d > maxLeaseTTL {
		return maxLeaseTTL
	}
	return d
}

// rawJSON marshals an already-decoded JSON value back to bytes for jsonb storage; nil stays nil.
func rawJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil { // unreachable for values produced by json.Unmarshal; belt and braces
		return nil
	}
	return b
}
