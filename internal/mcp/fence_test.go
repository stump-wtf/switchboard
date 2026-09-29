package mcp

// Lease-token fence over MCP
//
// The real SDK client and server over HTTP, against the fake store's model of the fence: claim and
// claim_next mint the token only when asked and return it only in that response, heartbeat,
// complete and fail present it back, a mismatch is conflict, and the token reaches no log line and
// no later response.
//
// Governing: SPEC-0034 REQ-6 "Lease Token Fence", REQ-19 "Error Handling Standards".
//
// @joestump-agent 09/25/2026 - Added for #325 (epic #313).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

// fenceSession connects a drain-verb session whose Handler logs, at debug level, into the returned
// buffer, so a test can grep everything the server logged.
func fenceSession(t *testing.T, ctx context.Context, f *fakeStore) (*sdk.ClientSession, *syncBuffer) {
	t.Helper()
	token := vend(t, f, defaultTestSlug, []string{"reviews"},
		[]string{"list_todos", "claim", "claim_next", "heartbeat", "complete", "fail"})
	logs := &syncBuffer{}
	h := New(f, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	cs, err := connect(t, ctx, ts.URL+"/mcp/"+defaultTestSlug, token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, logs
}

// callRaw makes a tool call and returns the whole result, content and structured output, as JSON.
func callRaw(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", tool, err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal %s result: %v", tool, err)
	}
	return string(b)
}

// assertLeaseToken checks a minted token's shape: base64url without padding over 16 bytes.
func assertLeaseToken(t *testing.T, token string) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != leaseTokenBytes {
		t.Fatalf("lease_token %q is not base64url over %d bytes (decoded %d, err %v)", token, leaseTokenBytes, len(raw), err)
	}
}

// REQ-6: the fence fields are on the advertised schemas, so a client can discover them.
func TestFenceFieldsAdvertised(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, _ := fenceSession(t, ctx, newFakeStore())
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	want := map[string]string{"claim": "require_fence", "claim_next": "require_fence",
		"heartbeat": "lease_token", "complete": "lease_token", "fail": "lease_token"}
	for _, tool := range res.Tools {
		field, ok := want[tool.Name]
		if !ok {
			continue
		}
		b, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(b), `"`+field+`"`) {
			t.Errorf("%s input schema lacks %s: %s", tool.Name, field, b)
		}
		delete(want, tool.Name)
		if tool.Name == "claim" || tool.Name == "claim_next" {
			if b, _ := json.Marshal(tool.OutputSchema); !strings.Contains(string(b), `"lease_token"`) {
				t.Errorf("%s output schema lacks lease_token: %s", tool.Name, b)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("tools not advertised: %v", want)
	}
}

// REQ-6 end to end: fenced claim and claim_next return a token once, top level, alongside every
// todo field; the holder's token applies; no token, a wrong token, or a token on an unfenced claim
// is conflict and leaves the todo claimed; an unfenced client is unchanged.
func TestFenceOverMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	for _, id := range []string{"td-claim", "td-open"} {
		f.putTodo(store.Todo{ID: id, EndpointID: defaultTestEndpointID, Queue: "reviews", Title: id, State: "pending"})
	}
	cs, _ := fenceSession(t, ctx, f)

	var fenced claimOut
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td-claim", "require_fence": true}, &fenced)
	assertLeaseToken(t, fenced.LeaseToken)
	if fenced.ID != "td-claim" || fenced.State != "claimed" || fenced.Queue != "reviews" || fenced.Attempt != 1 {
		t.Fatalf("fenced claim lost todo fields: %+v", fenced)
	}

	var open map[string]any
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td-open"}, &open)
	if _, has := open["lease_token"]; has || open["id"] != "td-open" {
		t.Fatalf("unfenced claim = %v, want the todo with no lease_token", open)
	}

	callErr(t, ctx, cs, "heartbeat", map[string]any{"id": "td-claim"}, codeConflict)
	callErr(t, ctx, cs, "heartbeat", map[string]any{"id": "td-claim", "lease_token": "wrong"}, codeConflict)
	callErr(t, ctx, cs, "complete", map[string]any{"id": "td-claim"}, codeConflict)
	callErr(t, ctx, cs, "fail", map[string]any{"id": "td-claim", "lease_token": "wrong"}, codeConflict)
	if got := f.todoState("td-claim"); got != "claimed" {
		t.Fatalf("missed calls moved the fenced todo to %s", got)
	}
	callErr(t, ctx, cs, "complete", map[string]any{"id": "td-open", "lease_token": fenced.LeaseToken}, codeConflict)

	var hb todoOut
	callOK(t, ctx, cs, "heartbeat", map[string]any{"id": "td-claim", "lease_token": fenced.LeaseToken}, &hb)
	var done todoOut
	callOK(t, ctx, cs, "complete", map[string]any{"id": "td-claim", "lease_token": fenced.LeaseToken}, &done)
	if done.State != "done" {
		t.Fatalf("holder's complete left state %s", done.State)
	}
	callOK(t, ctx, cs, "heartbeat", map[string]any{"id": "td-open"}, &hb)
	callOK(t, ctx, cs, "complete", map[string]any{"id": "td-open"}, &done)

	f.putTodo(store.Todo{ID: "td-next", EndpointID: defaultTestEndpointID, Queue: "reviews", Title: "next", State: "pending"})
	var next claimNextOut
	callOK(t, ctx, cs, "claim_next", map[string]any{"require_fence": true}, &next)
	if next.Todo == nil || next.Todo.ID != "td-next" {
		t.Fatalf("claim_next = %+v, want td-next", next)
	}
	assertLeaseToken(t, next.LeaseToken)
	if next.LeaseToken == fenced.LeaseToken {
		t.Fatal("two claims minted the same lease_token")
	}
	callErr(t, ctx, cs, "fail", map[string]any{"id": "td-next", "lease_token": fenced.LeaseToken}, codeConflict)
	var failed todoOut
	callOK(t, ctx, cs, "fail", map[string]any{"id": "td-next", "lease_token": next.LeaseToken}, &failed)
	if failed.State != "failed" {
		t.Fatalf("holder's fail left state %s", failed.State)
	}

	var empty claimNextOut
	callOK(t, ctx, cs, "claim_next", map[string]any{"require_fence": true}, &empty)
	if !empty.Empty || empty.LeaseToken != "" {
		t.Fatalf("empty claim_next = %+v, want empty with no lease_token", empty)
	}
}

// REQ-6 and REQ-19: the token is returned by the claim and by nothing else. Every later response,
// success or error, and every line the Handler logged at debug level, is grepped for it.
func TestLeaseTokenNeverLoggedOrReturnedAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	f.putTodo(store.Todo{ID: "td-a", EndpointID: defaultTestEndpointID, Queue: "reviews", Title: "a", State: "pending"})
	f.putTodo(store.Todo{ID: "td-b", EndpointID: defaultTestEndpointID, Queue: "reviews", Title: "b", State: "pending",
		CreatedAt: time.Now().Add(time.Minute)})
	cs, logs := fenceSession(t, ctx, f)

	var a claimOut
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td-a", "require_fence": true}, &a)
	var b claimNextOut
	callOK(t, ctx, cs, "claim_next", map[string]any{"require_fence": true}, &b)
	if b.Todo == nil || b.Todo.ID != "td-b" {
		t.Fatalf("claim_next = %+v, want td-b", b)
	}
	tokens := []string{a.LeaseToken, b.LeaseToken}
	for _, tok := range tokens {
		assertLeaseToken(t, tok)
	}

	later := []string{
		callRaw(t, ctx, cs, "list_todos", map[string]any{}),
		callRaw(t, ctx, cs, "heartbeat", map[string]any{"id": "td-a", "lease_token": a.LeaseToken}),
		callRaw(t, ctx, cs, "heartbeat", map[string]any{"id": "td-a", "lease_token": b.LeaseToken}),
		callRaw(t, ctx, cs, "complete", map[string]any{"id": "td-b", "lease_token": a.LeaseToken}),
		callRaw(t, ctx, cs, "complete", map[string]any{"id": "td-a"}),
		callRaw(t, ctx, cs, "claim", map[string]any{"id": "td-a", "require_fence": true}),
		callRaw(t, ctx, cs, "complete", map[string]any{"id": "td-a", "lease_token": a.LeaseToken}),
		callRaw(t, ctx, cs, "fail", map[string]any{"id": "td-b", "lease_token": b.LeaseToken}),
		callRaw(t, ctx, cs, "list_todos", map[string]any{}),
	}
	for _, tok := range tokens {
		for i, res := range later {
			if strings.Contains(res, tok) {
				t.Fatalf("later response %d carries a lease token: %s", i, res)
			}
		}
		if strings.Contains(logs.String(), tok) {
			t.Fatalf("a lease token reached the log:\n%s", logs.String())
		}
	}
	if logs.String() == "" {
		t.Fatal("captured no log output at all; the grep above proves nothing")
	}
}
