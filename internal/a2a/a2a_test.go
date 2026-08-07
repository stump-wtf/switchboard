package a2a

// Integration tests for the A2A HTTP/JSON-RPC mount (SPEC-0018). A fake EndpointStore stands in for
// Postgres so the full HTTP handshake — the same bearer-auth middleware the MCP surface uses plus
// the JSON-RPC router — runs everywhere, including CI runners without a database.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// fakeStore maps credential hashes to endpoints, mimicking EndpointByCredHash/EndpointByOAuthToken
// semantics (ErrNotFound for unknown AND revoked credentials — the store only resolves active rows),
// exactly as the MCP surface's test fake does. It is deliberately the SAME shape so "A2A rejects
// like MCP" is a property of the shared mechanism, not of a divergent test double.
type fakeStore struct {
	byHash      map[string]store.AuthEndpoint
	byOAuthHash map[string]store.AuthEndpoint
	touches     int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byHash:      map[string]store.AuthEndpoint{},
		byOAuthHash: map[string]store.AuthEndpoint{},
	}
}

func (f *fakeStore) EndpointByCredHash(_ context.Context, hash string) (store.AuthEndpoint, error) {
	ep, ok := f.byHash[hash]
	if !ok {
		return store.AuthEndpoint{}, store.ErrNotFound
	}
	return ep, nil
}

func (f *fakeStore) EndpointByOAuthToken(_ context.Context, hash string) (store.AuthEndpoint, error) {
	ep, ok := f.byOAuthHash[hash]
	if !ok {
		return store.AuthEndpoint{}, store.ErrNotFound
	}
	return ep, nil
}

func (f *fakeStore) TouchEndpoint(_ context.Context, _ string) error {
	f.touches++
	return nil
}

// vend mints a static sbk_ credential bound to a slug/scope, mirroring the MCP test helper so the
// two surfaces are exercised against the same credential shape.
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

// newTestServer mounts Routes() under /a2a exactly as internal/server would.
func newTestServer(t *testing.T, f *fakeStore) *httptest.Server {
	t.Helper()
	h := New(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Mount("/a2a", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// rpcCall POSTs a JSON-RPC request to url with the given bearer token (empty = none) and returns the
// HTTP status and decoded response envelope.
func rpcCall(t *testing.T, url, token, method string, params any) (int, rpcResponse) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var out rpcResponse
	if resp.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp.StatusCode, out
}

// TestAuthRejectsMissingAndInvalidLikeMCP asserts the A2A auth boundary rejects a missing credential
// and an invalid/revoked credential with 401 — identically to an unauthenticated MCP call — and a
// valid credential presented against the wrong endpoint path with 403. Governing: SPEC-0018 REQ
// "SendMessage Requires a Vended Endpoint" ("rejected identically to an unauthenticated MCP call");
// ADR-0021 Confirmation ("SendMessage without a valid vended-endpoint credential is rejected exactly
// like an unauthenticated MCP create_for call").
func TestAuthRejectsMissingAndInvalidLikeMCP(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"create_for"})
	ts := newTestServer(t, f)
	url := ts.URL + "/a2a/agent-a-11111111/"

	t.Run("missing credential is 401", func(t *testing.T) {
		status, _ := rpcCall(t, url, "", "tasks/get", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("garbage credential is 401", func(t *testing.T) {
		status, _ := rpcCall(t, url, "sbk_not-a-real-token", "tasks/get", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("unprefixed garbage (tried as OAuth) is 401", func(t *testing.T) {
		status, _ := rpcCall(t, url, "definitely-not-an-oauth-token", "tasks/get", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("revoked credential is 401", func(t *testing.T) {
		// Drop the mapping to mimic the store's active-row filter after revocation: the hash no longer
		// resolves, so auth answers 401 exactly like a never-vended credential.
		delete(f.byHash, cred.Hash(token))
		status, _ := rpcCall(t, url, token, "tasks/get", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("valid credential wrong path is 403", func(t *testing.T) {
		tok := vend(t, f, "agent-b-22222222", []string{"deploys"}, []string{"create_for"})
		// Present agent-b's credential against agent-a's URL: valid token, wrong endpoint path.
		status, _ := rpcCall(t, ts.URL+"/a2a/agent-a-11111111/", tok, "tasks/get", nil)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})
}

// TestUnauthorizedResponseShape asserts the 401 answer carries the same bare bearer challenge the MCP
// surface uses and never reflects the path slug into the response header.
func TestUnauthorizedResponseShape(t *testing.T) {
	f := newFakeStore()
	ts := newTestServer(t, f)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a/some-slug/", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="switchboard"` {
		t.Fatalf("WWW-Authenticate = %q, want bare bearer challenge", got)
	}
}

// TestAuthenticatedCallReachesRouter asserts a valid credential passes auth and reaches the router,
// which dispatches to the (stub) method handler and returns a well-formed JSON-RPC error — proving
// the auth→router→handler→envelope path is wired end to end. The stub returns UnsupportedOperation.
func TestAuthenticatedCallReachesRouter(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"create_for"})
	ts := newTestServer(t, f)
	url := ts.URL + "/a2a/agent-a-11111111/"

	status, resp := rpcCall(t, url, token, "tasks/get", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC error carried in body)", status)
	}
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error from the stub handler, got none")
	}
	if resp.Error.Code != CodeUnsupportedOperation {
		t.Fatalf("error code = %d, want %d (UnsupportedOperation)", resp.Error.Code, CodeUnsupportedOperation)
	}
	if f.touches == 0 {
		t.Fatalf("expected TouchEndpoint to be called on a successful auth")
	}
}

// TestRouterErrorEnvelopes covers the JSON-RPC parse/validation/dispatch error branches: an unknown
// method, a non-2.0 envelope, and malformed JSON each map to the correct JSON-RPC error code.
func TestRouterErrorEnvelopes(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, nil)
	ts := newTestServer(t, f)
	url := ts.URL + "/a2a/agent-a-11111111/"

	t.Run("unknown method is MethodNotFound", func(t *testing.T) {
		status, resp := rpcCall(t, url, token, "tasks/doesNotExist", nil)
		if status != http.StatusOK || resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
			t.Fatalf("status=%d err=%+v, want MethodNotFound", status, resp.Error)
		}
	})

	t.Run("wrong jsonrpc version is InvalidRequest", func(t *testing.T) {
		raw := `{"jsonrpc":"1.0","id":1,"method":"tasks/get"}`
		status, resp := rawPost(t, url, token, raw)
		if status != http.StatusOK || resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
			t.Fatalf("status=%d err=%+v, want InvalidRequest", status, resp.Error)
		}
	})

	t.Run("empty method is InvalidRequest", func(t *testing.T) {
		raw := `{"jsonrpc":"2.0","id":1}`
		status, resp := rawPost(t, url, token, raw)
		if status != http.StatusOK || resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
			t.Fatalf("status=%d err=%+v, want InvalidRequest", status, resp.Error)
		}
	})

	t.Run("malformed JSON is ParseError", func(t *testing.T) {
		raw := `{not valid json`
		status, resp := rawPost(t, url, token, raw)
		if status != http.StatusOK || resp.Error == nil || resp.Error.Code != CodeParseError {
			t.Fatalf("status=%d err=%+v, want ParseError", status, resp.Error)
		}
	})
}

// TestBodyLimit asserts a declared-oversize body is rejected with 413 before the JSON-RPC parse.
func TestBodyLimit(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, nil)
	ts := newTestServer(t, f)
	url := ts.URL + "/a2a/agent-a-11111111/"

	big := strings.Repeat("x", maxBodyBytes+1)
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestSecurityHeaders asserts the SPEC-0018 security headers are present on responses.
func TestSecurityHeaders(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, nil)
	ts := newTestServer(t, f)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/a2a/agent-a-11111111/",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tasks/get"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

// TestResponseEchoesID asserts the JSON-RPC id is echoed verbatim on a normal response and is the
// literal null (never omitted) when the request could not be parsed — a valid JSON-RPC 2.0 response
// always carries an id member.
func TestResponseEchoesID(t *testing.T) {
	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, nil)
	ts := newTestServer(t, f)
	url := ts.URL + "/a2a/agent-a-11111111/"

	t.Run("id echoed on dispatch", func(t *testing.T) {
		raw := `{"jsonrpc":"2.0","id":"req-7","method":"tasks/get"}`
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		var env map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(env["id"]) != `"req-7"` {
			t.Fatalf("id = %s, want \"req-7\"", env["id"])
		}
	})

	t.Run("id is null on parse error", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{bad json`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		var env map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		id, ok := env["id"]
		if !ok || string(id) != "null" {
			t.Fatalf("id present=%v value=%s, want the literal null", ok, id)
		}
	})
}

// rawPost POSTs a raw body string (bypassing rpcCall's marshaling) so malformed/edge envelopes can
// be exercised directly.
func rawPost(t *testing.T, url, token, raw string) (int, rpcResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var out rpcResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}
