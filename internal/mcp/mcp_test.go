package mcp

// Integration tests for the Streamable HTTP MCP mount (SPEC-0014). A fake EndpointStore stands in
// for Postgres so the full HTTP handshake — auth middleware plus the real SDK client and server —
// runs everywhere, including CI runners without a database.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// fakeStore maps credential hashes to endpoints, mimicking EndpointByCredHash semantics
// (ErrNotFound for unknown AND revoked credentials — the store only resolves active rows).
type fakeStore struct {
	byHash  map[string]store.AuthEndpoint
	touches atomic.Int64
}

func (f *fakeStore) EndpointByCredHash(_ context.Context, hash string) (store.AuthEndpoint, error) {
	ep, ok := f.byHash[hash]
	if !ok {
		return store.AuthEndpoint{}, store.ErrNotFound
	}
	return ep, nil
}

func (f *fakeStore) TouchEndpoint(_ context.Context, _ string) error {
	f.touches.Add(1)
	return nil
}

// vend mints a real credential (like the web vend path) and registers it in the fake store.
func vend(t *testing.T, f *fakeStore, slug string, queues, verbs []string) (token string) {
	t.Helper()
	token, hash, _, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	f.byHash[hash] = store.AuthEndpoint{
		ID: "ep-" + slug, AgentID: "ag-1", AgentName: "test-agent", OwnerHumanID: "h-1",
		Slug: slug, ScopeQueues: queues, ScopeVerbs: verbs,
	}
	return token
}

// newTestServer mounts Routes() under /mcp exactly as internal/server does.
func newTestServer(t *testing.T, f *fakeStore) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Mount("/mcp", New(f, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// bearerTransport injects the Authorization header on every request, like a real MCP client
// configured with `"headers": {"Authorization": "Bearer <credential>"}`.
type bearerTransport struct{ token string }

func (bt bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if bt.token != "" {
		req.Header.Set("Authorization", "Bearer "+bt.token)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func connect(t *testing.T, ctx context.Context, url, token string) (*sdk.ClientSession, error) {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	return client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
}

// TestHandshake drives the full Streamable HTTP lifecycle with a vended credential: initialize,
// tools/list, ping, session teardown. Governing: SPEC-0014 REQ "Streamable HTTP MCP Endpoint".
func TestHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token := vend(t, f, "test-agent-ab12cd34", []string{"reviews"}, []string{"list_todos", "claim"})
	ts := newTestServer(t, f)

	cs, err := connect(t, ctx, ts.URL+"/mcp/test-agent-ab12cd34", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	defer func() { _ = cs.Close() }()

	init := cs.InitializeResult()
	if init.ServerInfo.Name != serverName {
		t.Fatalf("server name = %q, want %q", init.ServerInfo.Name, serverName)
	}
	if init.ProtocolVersion == "" {
		t.Fatal("no protocol version negotiated")
	}
	if init.Capabilities.Tools == nil {
		t.Fatal("tools capability not advertised")
	}

	// tools/list works on the session; the verb registry lands in the follow-up story, so empty.
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) != 0 {
		t.Fatalf("expected no tools yet, got %d", len(tools.Tools))
	}

	if err := cs.Ping(ctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("session teardown: %v", err)
	}
	if f.touches.Load() == 0 {
		t.Fatal("successful authentication must stamp last-seen")
	}
}

// TestAuthRejections covers the SPEC-0014 auth matrix: 401 missing/unknown/revoked, 403 mismatch.
func TestAuthRejections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	tokenA := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	vend(t, f, "agent-b-22222222", []string{"deploys"}, []string{"list_todos"})

	ts := newTestServer(t, f)

	cases := []struct {
		name  string
		url   string
		token string
		want  int
	}{
		{"missing credential", ts.URL + "/mcp/agent-a-11111111", "", http.StatusUnauthorized},
		{"unknown credential", ts.URL + "/mcp/agent-a-11111111", "sbk_bogus", http.StatusUnauthorized},
		// Revoked endpoints resolve to ErrNotFound exactly like unknown ones (state='active' filter),
		// so a revoked credential is indistinguishable from a never-vended one: 401, no processing.
		{"revoked credential", ts.URL + "/mcp/agent-a-11111111", revokedToken(t, f), http.StatusUnauthorized},
		// Agent A's valid credential presented at agent B's path: 403, nothing executed.
		{"credential/path mismatch", ts.URL + "/mcp/agent-b-22222222", tokenA, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			touchesBefore := f.touches.Load()
			if _, err := connect(t, ctx, tc.url, tc.token); err == nil {
				t.Fatalf("handshake unexpectedly succeeded")
			}
			// Confirm the raw status code with a bare POST too.
			resp := rawPost(t, tc.url, tc.token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if f.touches.Load() != touchesBefore {
				t.Fatal("rejected request must not stamp last-seen")
			}
		})
	}
}

// revokedToken mints a credential and does NOT register it — the store contract returns ErrNotFound
// for revoked rows, which is exactly what an unregistered hash produces.
func revokedToken(t *testing.T, _ *fakeStore) string {
	t.Helper()
	token, _, _, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	return token
}

// TestSessionCookieCarriesNoAuthority: a valid web session cookie without a bearer credential is
// still 401 — cookies are never consulted on /mcp/*. Governing: SPEC-0014 REQ "Authentication".
func TestSessionCookieCarriesNoAuthority(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts := newTestServer(t, f)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp/agent-a-11111111",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.AddCookie(&http.Cookie{Name: "sb_session", Value: "some-valid-web-session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie-only request status = %d, want 401", resp.StatusCode)
	}
}

// TestSecurityHeadersAndBodyCap: nosniff + no-store on /mcp/* responses; >1 MiB bodies get 413.
func TestSecurityHeadersAndBodyCap(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts := newTestServer(t, f)

	resp := rawPost(t, ts.URL+"/mcp/agent-a-11111111", token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	// Governing: SPEC-0014 REQ "Request Body Size Limits" — 1 MiB cap → 413.
	big := strings.Repeat("x", maxBodyBytes+1)
	resp = rawPost(t, ts.URL+"/mcp/agent-a-11111111", token, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", resp.StatusCode)
	}
}

// TestPerEndpointRateLimit exhausts one endpoint's bucket and confirms 429 + Retry-After, while a
// second endpoint's bucket is untouched. Governing: SPEC-0014 REQ "Rate Limiting".
func TestPerEndpointRateLimit(t *testing.T) {
	rl := newRateLimiter(20, 40)
	now := time.Now()
	for i := 0; i < 40; i++ {
		if !rl.allowAt("agent-a", now) {
			t.Fatalf("request %d within burst should pass", i)
		}
	}
	if rl.allowAt("agent-a", now) {
		t.Fatal("41st instantaneous request should be limited")
	}
	if !rl.allowAt("agent-b", now) {
		t.Fatal("a different endpoint must have its own bucket")
	}
	// Refill: one second later the drained bucket admits more requests.
	if !rl.allowAt("agent-a", now.Add(time.Second)) {
		t.Fatal("bucket should refill over time")
	}

	// And over HTTP: an empty bucket answers 429 with Retry-After.
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts := newTestServer(t, f)
	var last *http.Response
	for i := 0; i < 45; i++ {
		last = rawPost(t, ts.URL+"/mcp/agent-a-11111111", token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after exhausting the bucket status = %d, want 429", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

// TestCredentialNeverLogged asserts the bearer value stays out of middleware logs on both the
// success and failure paths (SPEC-0014 REQ "Error Handling Standards").
func TestCredentialNeverLogged(t *testing.T) {
	var buf strings.Builder
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})

	r := chi.NewRouter()
	r.Mount("/mcp", New(f, slog.New(slog.NewTextHandler(&buf, nil))).Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	rawPost(t, ts.URL+"/mcp/agent-a-11111111", token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	rawPost(t, ts.URL+"/mcp/agent-b-22222222", token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`) // 403 path logs a warning
	if strings.Contains(buf.String(), token) {
		t.Fatal("bearer credential leaked into logs")
	}
}

func rawPost(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp
}

// Compile-time check: the real store satisfies the interface the handler consumes.
var _ EndpointStore = (*store.Store)(nil)
