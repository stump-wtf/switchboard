package mcp

// tools/call integration tests for the SPEC-0006 verb registry over MCP (SPEC-0014 REQ "Agent Tool
// Surface over MCP" + "Error Handling Standards"). The real SDK client and server run over HTTP
// against the fake store, so schemas, structured outputs, scope enforcement, and error mapping are
// all exercised end-to-end.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
)

// session vends a credential with the given scope, seeds todos, and returns a connected session.
func session(t *testing.T, ctx context.Context, f *fakeStore, queues, verbs []string) *sdk.ClientSession {
	t.Helper()
	token := vend(t, f, "agent-a-11111111", queues, verbs)
	ts := newTestServer(t, f)
	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callOK asserts the tool call succeeded and unmarshals its structured output into out.
func callOK(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("tools/call %s returned tool error: %s", tool, contentText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
}

// callErr asserts the tool call returned a tool error (IsError) whose text carries the stable code.
func callErr(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any, wantCode string) string {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: protocol error %v, want tool error %q", tool, err, wantCode)
	}
	if !res.IsError {
		t.Fatalf("tools/call %s succeeded, want tool error %q", tool, wantCode)
	}
	text := contentText(res)
	if !strings.HasPrefix(text, wantCode+":") {
		t.Fatalf("tools/call %s error = %q, want code %q", tool, text, wantCode)
	}
	return text
}

func contentText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TestToolsCallScopeViolation: a verb outside the allowlist is a stable scope error — code
// "forbidden", no store mutation — while a truly unknown tool stays a protocol error.
// Governing: SPEC-0014 scenario "Scope filters the advertised tools".
func TestToolsCallScopeViolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td_1", Queue: "reviews", Title: "review PR", State: "claimed",
		Owner: "agent:ag-1", Attempt: 1})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"list_todos", "claim"})

	// complete is a real SPEC-0006 verb, just not granted here: scope error, todo untouched.
	callErr(t, ctx, cs, "complete", map[string]any{"id": "td_1"}, "forbidden")
	if got := f.todoState("td_1"); got != "claimed" {
		t.Fatalf("scope-violating complete mutated the todo: state = %q, want claimed", got)
	}

	// An unknown tool is not a scope violation — the SDK answers a JSON-RPC protocol error.
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "frobnicate", Arguments: map[string]any{}}); err == nil {
		t.Fatal("tools/call of an unknown tool should be a protocol error")
	}
}

// TestListTodosScoped: list_todos is confined to granted queues, and an explicit out-of-scope
// queue filter is refused. Governing: SPEC-0006 REQ "Scope Enforcement at the Boundary".
func TestListTodosScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td_r", Queue: "reviews", Title: "in scope", State: "pending",
		Payload: []byte(`{"pr":7}`)})
	f.putTodo(store.Todo{ID: "td_d", Queue: "deploys", Title: "out of scope", State: "pending"})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	var out struct {
		Todos []struct {
			ID      string         `json:"id"`
			Queue   string         `json:"queue"`
			State   string         `json:"state"`
			Attempt int            `json:"attempt"`
			Payload map[string]any `json:"payload"`
		} `json:"todos"`
	}
	callOK(t, ctx, cs, "list_todos", map[string]any{}, &out)
	if len(out.Todos) != 1 || out.Todos[0].ID != "td_r" {
		t.Fatalf("list_todos = %+v, want exactly td_r", out.Todos)
	}
	// The structured row carries the SPEC-0006 minimum (id, queue, state, attempt) plus payload.
	if out.Todos[0].Queue != "reviews" || out.Todos[0].State != "pending" {
		t.Fatalf("todo row = %+v, want queue=reviews state=pending", out.Todos[0])
	}
	if out.Todos[0].Payload["pr"] != float64(7) {
		t.Fatalf("payload = %v, want pr=7", out.Todos[0].Payload)
	}

	callErr(t, ctx, cs, "list_todos", map[string]any{"queue": "deploys"}, "forbidden")
}

// TestClaimCompleteLifecycle drives claim → heartbeat → complete over tools/call, checking the
// structured outputs at each step. Lifecycle semantics themselves are SPEC-0003's (store-tested);
// this asserts the MCP binding records the agent identity and lease.
func TestClaimCompleteLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td_1", Queue: "reviews", Title: "review PR", State: "pending"})
	cs := session(t, ctx, f, []string{"reviews"},
		[]string{"list_todos", "claim", "complete", "fail", "heartbeat"})

	var claimed struct {
		ID             string `json:"id"`
		State          string `json:"state"`
		Owner          string `json:"owner"`
		Attempt        int    `json:"attempt"`
		LeaseExpiresAt string `json:"lease_expires_at"`
	}
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_1", "lease_ttl_seconds": 60}, &claimed)
	if claimed.State != "claimed" || claimed.Owner != "agent:ag-1" || claimed.Attempt != 1 {
		t.Fatalf("claim output = %+v, want claimed by agent:ag-1 attempt 1", claimed)
	}
	if claimed.LeaseExpiresAt == "" {
		t.Fatal("claim output missing lease_expires_at")
	}

	var beat struct {
		LeaseExpiresAt string `json:"lease_expires_at"`
	}
	callOK(t, ctx, cs, "heartbeat", map[string]any{"id": "td_1", "lease_ttl_seconds": 600}, &beat)
	if beat.LeaseExpiresAt <= claimed.LeaseExpiresAt {
		t.Fatalf("heartbeat did not extend the lease: %s -> %s", claimed.LeaseExpiresAt, beat.LeaseExpiresAt)
	}

	var done struct {
		State string `json:"state"`
	}
	callOK(t, ctx, cs, "complete", map[string]any{"id": "td_1", "result": map[string]any{"ok": true}}, &done)
	if done.State != "done" {
		t.Fatalf("complete output state = %q, want done", done.State)
	}

	// A second claim of the now-terminal todo loses: stable conflict code.
	callErr(t, ctx, cs, "claim", map[string]any{"id": "td_1"}, "conflict")
}

// TestFailRetriesThenDeadLetters: fail returns a claimed todo to pending while attempts remain and
// dead-letters it once exhausted (SPEC-0006 scenario "Fail retries until attempts are exhausted").
func TestFailRetriesThenDeadLetters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td_1", Queue: "reviews", Title: "flaky", State: "pending", MaxAttempts: 2})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "fail"})

	var out struct {
		State   string `json:"state"`
		Attempt int    `json:"attempt"`
	}
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_1"}, &out)
	callOK(t, ctx, cs, "fail", map[string]any{"id": "td_1"}, &out)
	if out.State != "pending" || out.Attempt != 1 {
		t.Fatalf("first fail = %+v, want pending attempt 1", out)
	}
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_1"}, &out)
	callOK(t, ctx, cs, "fail", map[string]any{"id": "td_1", "result": map[string]any{"reason": "gave up"}}, &out)
	if out.State != "failed" {
		t.Fatalf("final fail state = %q, want failed (dead-letter)", out.State)
	}
}

// TestQueueScopeEnforcedBeforeMutation: a transition verb targeting a todo whose queue is outside
// the endpoint's grant is refused with "forbidden" and no side effects, even though the verb
// itself is allowlisted. Governing: SPEC-0006 scenario "Todo outside granted queues".
func TestQueueScopeEnforcedBeforeMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td_d", Queue: "deploys", Title: "not yours", State: "pending"})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim"})

	callErr(t, ctx, cs, "claim", map[string]any{"id": "td_d"}, "forbidden")
	if got := f.todoState("td_d"); got != "pending" {
		t.Fatalf("out-of-scope claim mutated the todo: state = %q, want pending", got)
	}
}

// TestNotFoundAndConflictCodes maps the store sentinels onto the stable SPEC-0006 codes.
func TestNotFoundAndConflictCodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td_1", Queue: "reviews", Title: "held elsewhere", State: "claimed",
		Owner: "agent:other", Attempt: 1})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "heartbeat"})

	callErr(t, ctx, cs, "claim", map[string]any{"id": "td_missing"}, "not_found")
	// Heartbeat on a lease held by someone else: refused without extending (SPEC-0006 scenario
	// "Heartbeat extends only the holder's lease").
	callErr(t, ctx, cs, "heartbeat", map[string]any{"id": "td_1"}, "conflict")
}

// TestStoreFailureIsGenericToClient: a DB failure inside a tools/call reaches the client only as
// the generic "internal" code, while the server log records the endpoint slug, the tool, and the
// wrapped error chain — and never the bearer credential.
// Governing: SPEC-0014 scenario "Store failure surfaces as a tool error".
func TestStoreFailureIsGenericToClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf strings.Builder
	f := newFakeStore()
	f.failErr = errors.New("pg: connection refused host=db-internal-secret")
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})

	h := New(f, slog.New(slog.NewTextHandler(&buf, nil)))
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)

	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	defer func() { _ = cs.Close() }()

	text := callErr(t, ctx, cs, "list_todos", map[string]any{}, "internal")
	if strings.Contains(text, "db-internal-secret") {
		t.Fatalf("internal error detail leaked to the client: %q", text)
	}
	logs := buf.String()
	for _, want := range []string{"agent-a-11111111", "list_todos", "db-internal-secret"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log missing %q; logs:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, token) {
		t.Fatal("bearer credential leaked into logs")
	}
}

// TestChunkedOversizedBodyGets413: an oversized body with no declared Content-Length (chunked
// encoding) must also answer 413, not the SDK's 400 after MaxBytesReader trips mid-parse.
// Governing: SPEC-0014 REQ "Request Body Size Limits".
func TestChunkedOversizedBodyGets413(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts := newTestServer(t, f)

	// Wrapping the reader hides its concrete type from http.NewRequest, so no Content-Length is
	// sniffed and the transport sends chunked encoding.
	body := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", maxBodyBytes+10))}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp/agent-a-11111111", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && req.ContentLength > 0 {
		t.Fatalf("test bug: request had declared Content-Length %d, want chunked", req.ContentLength)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized body status = %d, want 413", resp.StatusCode)
	}
}
