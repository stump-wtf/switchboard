package main

// The worker verbs, end to end against an in-process MCP server that stands in for a vended
// endpoint: the Authorization header must carry the endpoint credential and never the operator
// bearer; the verbs must never read the operator credentials file; next on an empty queue prints
// {"empty": true} and exits 3; payload stays off the one-shot's context unless --payload; and
// --result reaches the tool as a JSON object.

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const testEndpointKey = "sbk_test_endpoint_credential"

type workerCall struct {
	tool string
	in   map[string]any
}

// workerEndpoint is a stateless Streamable HTTP MCP server that answers the worker verbs and
// records every tools/call; like the real surface it only accepts the endpoint bearer.
type workerEndpoint struct {
	t            *testing.T
	srv          *httptest.Server
	mu           sync.Mutex
	calls        []workerCall
	authHeaders  []string
	nextEmpty    bool
	claimPayload map[string]any
	completeErr  string
}

type wListIn struct {
	Queue string `json:"queue,omitempty"`
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type wClaimIn struct {
	ID              string `json:"id,omitempty"`
	Queue           string `json:"queue,omitempty"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds,omitempty"`
	RequireFence    bool   `json:"require_fence,omitempty"`
	Claimant        string `json:"claimant,omitempty"`
}

type wHeartbeatIn struct {
	ID              string `json:"id"`
	LeaseTTLSeconds int    `json:"lease_ttl_seconds,omitempty"`
	LeaseToken      string `json:"lease_token,omitempty"`
}

type wCloseIn struct {
	ID         string `json:"id"`
	Result     any    `json:"result,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Artifact   string `json:"artifact,omitempty"`
	LeaseToken string `json:"lease_token,omitempty"`
}

type wReleaseIn struct {
	ID         string `json:"id"`
	Summary    string `json:"summary,omitempty"`
	Artifact   string `json:"artifact,omitempty"`
	LeaseToken string `json:"lease_token,omitempty"`
}

func rawArgs(v any) map[string]any {
	in := map[string]any{}
	if b, err := json.Marshal(v); err == nil {
		_ = json.Unmarshal(b, &in)
	}
	return in
}

func newWorkerEndpoint(t *testing.T) *workerEndpoint {
	t.Helper()
	w := &workerEndpoint{t: t, claimPayload: map[string]any{"work_order": map[string]any{"id": "ABC123"}}}
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake-switchboard", Version: "test"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "list_todos"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wListIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("list_todos", in)
			return nil, map[string]any{"todos": []any{workerRow("td_1", "lane-m", "pending")}}, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "claim_next"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wClaimIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("claim_next", in)
			w.mu.Lock()
			empty := w.nextEmpty
			w.mu.Unlock()
			if empty {
				return nil, map[string]any{"empty": true}, nil
			}
			return nil, map[string]any{"empty": false, "todo": workerTodo("td_9", "lane-m", w.claimPayload),
				"attempt_seq": 1, "attempts_total": 1, "prior_attempts": []any{}}, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "claim"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wClaimIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("claim", in)
			return nil, map[string]any{"id": "td_9", "queue": "lane-m", "title": "a claimed todo",
				"state": "claimed", "attempt": 1, "max_attempts": 5, "created_at": "2026-10-15T12:00:00Z",
				"payload": w.claimPayload, "payload_size": 512, "attempt_seq": 2, "attempts_total": 2,
				"prior_attempts": []any{}}, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "get_todo"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wCloseIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("get_todo", in)
			row := workerTodo("td_9", "lane-m", w.claimPayload)
			row["result"] = map[string]any{"summary": "earlier attempt"}
			return nil, row, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "heartbeat"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wHeartbeatIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("heartbeat", in)
			row := workerRow("td_9", "lane-m", "claimed")
			row["lease_expires_at"] = "2026-10-15T14:00:00Z"
			return nil, row, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "complete"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wCloseIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("complete", in)
			w.mu.Lock()
			toolErr := w.completeErr
			w.mu.Unlock()
			if toolErr != "" {
				return nil, nil, errFakeTool(toolErr)
			}
			return nil, workerRow(in.ID, "lane-m", "done"), nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "fail"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wCloseIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("fail", in)
			return nil, workerRow(in.ID, "lane-m", "failed"), nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "release"},
		func(_ context.Context, _ *sdk.CallToolRequest, in wReleaseIn) (*sdk.CallToolResult, map[string]any, error) {
			w.note("release", in)
			return nil, workerRow(in.ID, "lane-m", "pending"), nil
		})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.authHeaders = append(w.authHeaders, r.Header.Get("Authorization"))
		ok := r.Header.Get("Authorization") == "Bearer "+testEndpointKey
		w.mu.Unlock()
		if !ok {
			rw.Header().Set("WWW-Authenticate", `Bearer realm="switchboard"`)
			http.Error(rw, "invalid or revoked credential", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(rw, r)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *workerEndpoint) note(tool string, args any) {
	in := rawArgs(args)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, workerCall{tool: tool, in: in})
}

func (w *workerEndpoint) url() string { return w.srv.URL + "/mcp/lane-m-key" }

func (w *workerEndpoint) toolCalls() []workerCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]workerCall(nil), w.calls...)
}

type errFakeTool string

func (e errFakeTool) Error() string { return string(e) }

func workerRow(id, queue, state string) map[string]any {
	return map[string]any{"id": id, "queue": queue, "title": "a todo", "state": state,
		"attempt": 1, "max_attempts": 5, "next_retry_at": nil, "dead_letter": false,
		"created_at": "2026-10-15T12:00:00Z", "payload_size": 42}
}

func workerTodo(id, queue string, payload map[string]any) map[string]any {
	row := workerRow(id, queue, "claimed")
	row["payload"] = payload
	row["payload_size"] = 512
	return row
}

// wire points the CLI at the fake endpoint with the endpoint credential in the environment.
func (w *workerEndpoint) wire(tc *testCLI) {
	tc.env["SWITCHBOARD_URL"] = w.url()
	tc.env["SWITCHBOARD_API_KEY"] = testEndpointKey
}

func TestWorkerListHidesPayloadRows(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "list"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	out := tc.stdout.String()
	if !strings.Contains(out, "td_1") || !strings.Contains(out, "lane-m") {
		t.Errorf("row missing from output: %q", out)
	}
	if strings.Contains(out, "work_order") {
		t.Errorf("list rows must carry no payload: %q", out)
	}
	if got := w.toolCalls(); len(got) != 1 || got[0].tool != "list_todos" {
		t.Fatalf("calls = %+v", got)
	}
}

func TestWorkerListJSON(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "list", "--json"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	if !json.Valid(tc.stdout.Bytes()) {
		t.Fatalf("--json must print valid JSON: %q", tc.stdout.String())
	}
}

func TestWorkerNextClaimsAndHidesPayload(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "next", "--lease-ttl", "7200", "--claimant", "handoff-m"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	out := tc.stdout.String()
	if !strings.Contains(out, "td_9") || !strings.Contains(out, "payload omitted") {
		t.Errorf("claim summary missing: %q", out)
	}
	if strings.Contains(out, "ABC123") {
		t.Errorf("payload echoed without --payload: %q", out)
	}
	calls := w.toolCalls()
	if len(calls) != 1 || calls[0].tool != "claim_next" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].in["lease_ttl_seconds"] != float64(7200) || calls[0].in["claimant"] != "handoff-m" {
		t.Errorf("claim_next args = %+v", calls[0].in)
	}
}

func TestWorkerNextPayloadFlag(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "next", "--json", "--payload"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	if !strings.Contains(tc.stdout.String(), "ABC123") {
		t.Errorf("--payload must include the payload: %q", tc.stdout.String())
	}
}

func TestWorkerNextEmptyExitsDistinct(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.nextEmpty = true
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "next"}...); code != exitNoWork {
		t.Fatalf("exit %d, want %d; stderr %s", code, exitNoWork, tc.stderr)
	}
	if got := strings.TrimSpace(tc.stdout.String()); got != `{"empty": true}` {
		t.Fatalf("stdout = %q, want the empty answer", got)
	}
}

func TestWorkerClaimKeepsEveryField(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "claim", "td_9"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	if !strings.Contains(tc.stdout.String(), "td_9") || strings.Contains(tc.stdout.String(), "ABC123") {
		t.Errorf("claim output: %q", tc.stdout.String())
	}
	calls := w.toolCalls()
	if len(calls) != 1 || calls[0].tool != "claim" || calls[0].in["id"] != "td_9" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestWorkerHeartbeatCarriesTTLAndToken(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "heartbeat", "td_9", "--lease-ttl", "7200", "--lease-token", "ltok"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	calls := w.toolCalls()
	if len(calls) != 1 || calls[0].tool != "heartbeat" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].in["lease_ttl_seconds"] != float64(7200) || calls[0].in["lease_token"] != "ltok" {
		t.Errorf("heartbeat args = %+v", calls[0].in)
	}
	if !strings.Contains(tc.stdout.String(), "2026-10-15T14:00:00Z") {
		t.Errorf("stdout = %q", tc.stdout.String())
	}
}

func TestWorkerCompleteSendsJSONObjectResult(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	args := []string{"todo", "complete", "td_9", "--result", `{"outcome":"ok"}`, "--summary", "shipped it",
		"--artifact", "mcp://cairn/ABC123"}
	if code := tc.run(t, args...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	calls := w.toolCalls()
	if len(calls) != 1 || calls[0].tool != "complete" {
		t.Fatalf("calls = %+v", calls)
	}
	res, ok := calls[0].in["result"].(map[string]any)
	if !ok || res["outcome"] != "ok" {
		t.Errorf("result did not arrive as a JSON object: %+v", calls[0].in["result"])
	}
	if calls[0].in["summary"] != "shipped it" || calls[0].in["artifact"] != "mcp://cairn/ABC123" {
		t.Errorf("complete args = %+v", calls[0].in)
	}
	if !strings.Contains(tc.stdout.String(), "completed td_9") {
		t.Errorf("stdout = %q", tc.stdout.String())
	}
}

func TestWorkerCompleteResultMustBeAnObject(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "complete", "td_9", "--result", `["not","an","object"]`}...); code != exitFailure {
		t.Fatalf("exit %d, want %d; stderr %s", code, exitFailure, tc.stderr)
	}
	if !strings.Contains(tc.stderr.String(), "JSON object") {
		t.Errorf("stderr = %q", tc.stderr.String())
	}
	if len(w.toolCalls()) != 0 {
		t.Errorf("a malformed --result must not reach the server: %+v", w.toolCalls())
	}
}

func TestWorkerCompleteResultFromFile(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	dir := t.TempDir()
	path := dir + "/result.json"
	if err := os.WriteFile(path, []byte(`{"pr":"https://gitea.stump.rocks/stump.wtf/switchboard/pulls/1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := tc.run(t, []string{"todo", "complete", "td_9", "--result", "@" + path}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	calls := w.toolCalls()
	res, _ := calls[0].in["result"].(map[string]any)
	if res == nil || !strings.HasPrefix(res["pr"].(string), "https://") {
		t.Errorf("--result @file did not arrive: %+v", calls[0].in)
	}
}

func TestWorkerFailAndRelease(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "fail", "td_9", "--summary", "looping: the step would not move"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	if code := tc.run(t, []string{"todo", "release", "td_9", "--summary", "stopping"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	calls := w.toolCalls()
	if len(calls) != 2 || calls[0].tool != "fail" || calls[1].tool != "release" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestWorkerToolErrorIsAFailure(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.completeErr = "conflict: you no longer hold this todo"
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "complete", "td_9"}...); code != exitFailure {
		t.Fatalf("exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(tc.stderr.String(), "conflict") {
		t.Errorf("the server's tool error must reach stderr: %q", tc.stderr.String())
	}
}

// The auth contract (#556): the endpoint credential is required, the operator login is never a
// fallback, and when both exist only the endpoint credential goes on the wire.
func TestWorkerVerbsNeverFallBackToOperatorCredentials(t *testing.T) {
	f := newFakeDeployment(t)
	tc := newTestCLI(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	if code := tc.run(t, []string{"todo", "next"}...); code != exitFailure {
		t.Fatalf("exit %d, want %d; stderr %s", code, exitUsage, tc.stderr)
	}
	if !strings.Contains(tc.stderr.String(), "never fall back to the operator login") {
		t.Errorf("stderr = %q", tc.stderr.String())
	}
	if code := tc.run(t, "todo", "list"); code != exitFailure {
		t.Fatalf("todo list: exit %d; stderr %s", code, tc.stderr)
	}
	if !strings.Contains(tc.stderr.String(), "never fall back") {
		t.Errorf("todo list must refuse too: %s", tc.stderr)
	}
}

func TestWorkerVerbsIgnoreOperatorCredentialsWhenEndpointIsSet(t *testing.T) {
	f := newFakeDeployment(t)
	tc := newTestCLI(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "next"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.apiCalls) != 0 {
		t.Fatalf("worker verbs hit the operator API: %+v", f.apiCalls)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range w.authHeaders {
		if h != "Bearer "+testEndpointKey {
			t.Fatalf("wire credential = %q", h)
		}
	}
}

func TestWorkerVerbsRefuseAWrongKey(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	tc.env["SWITCHBOARD_API_KEY"] = "sbk_revoked"
	if code := tc.run(t, []string{"todo", "list"}...); code != exitFailure {
		t.Fatalf("exit %d, want %d; stderr %s", code, exitFailure, tc.stderr)
	}
	if strings.Contains(tc.stderr.String(), "sbk_revoked") {
		t.Errorf("the credential must never be echoed back: %q", tc.stderr.String())
	}
}

func TestWorkerURLNaming(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	tc.env["SWITCHBOARD_API_KEY"] = testEndpointKey
	tc.env["SWITCHBOARD_URL"] = w.srv.URL
	if code := tc.run(t, []string{"todo", "next", "--slug", "lane-m-key"}...); code != exitOK {
		t.Fatalf("base URL + --slug: exit %d, stderr %s", code, tc.stderr)
	}
	if code := tc.run(t, []string{"todo", "next", "--url", w.url()}...); code != exitOK {
		t.Fatalf("--url full endpoint: exit %d, stderr %s", code, tc.stderr)
	}
	if code := tc.run(t, []string{"todo", "next", "--url", w.srv.URL}...); code != exitFailure {
		t.Fatalf("a base URL with no slug must be a usage error, got %d; stderr %s", code, tc.stderr)
	}
}

func TestWorkerUsageMistakes(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	for _, args := range [][]string{
		{"todo", "next", "extra"},
		{"todo", "claim"},
		{"todo", "heartbeat"},
		{"todo", "complete"},
	} {
		if code := tc.run(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", args, code, exitUsage, tc.stderr)
		}
		tc.stderr.Reset()
		tc.stdout.Reset()
	}
}

func TestWorkerGetReadsWithoutClaiming(t *testing.T) {
	tc := newTestCLI(t)
	w := newWorkerEndpoint(t)
	w.wire(tc)
	if code := tc.run(t, []string{"todo", "get", "td_9"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	if strings.Contains(tc.stdout.String(), "ABC123") {
		t.Errorf("get must hide the payload: %q", tc.stdout.String())
	}
	calls := w.toolCalls()
	if len(calls) != 1 || calls[0].tool != "get_todo" || calls[0].in["id"] != "td_9" {
		t.Fatalf("calls = %+v", calls)
	}
	if code := tc.run(t, []string{"todo", "get", "td_9", "--payload", "--json"}...); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, tc.stderr)
	}
	if !strings.Contains(tc.stdout.String(), "ABC123") {
		t.Errorf("--payload must show it: %q", tc.stdout.String())
	}
}

func TestStripPayloads(t *testing.T) {
	raw := json.RawMessage(`{"empty":false,"todo":{"id":"td_9","payload":{"secret":"very"},"payload_size":512},"attempt_seq":2}`)
	got := string(stripPayloads(raw, false))
	if strings.Contains(got, "very") || !strings.Contains(got, "omitted") {
		t.Errorf("nested payload not stripped: %s", got)
	}
	if !strings.Contains(got, `"attempt_seq":2`) {
		t.Errorf("other fields must survive: %s", got)
	}
	if string(stripPayloads(raw, true)) != string(raw) {
		t.Error("--payload must pass the output through untouched")
	}
}
