package mcp

// Resource-server OAuth acceptance tests (SPEC-0016 REQ "Resource-Server Token Validation"): the
// auth middleware accepts OAuth access tokens and sbk_ static bearers interchangeably — both
// resolve to an endpoint, after which the tool surface and scope limits are identical — and an
// expired/revoked/unknown OAuth token draws the same RFC 9728 challenge as a bad static bearer.
// Governing: ADR-0019, SPEC-0016 scenario "Two credential shapes, one capability".

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
)

// vendOAuth mints an OAuth access token (oauthsrv.MintToken — unprefixed, hashed with the same
// primitive) resolving to the given endpoint, mimicking store.EndpointByOAuthToken.
func vendOAuth(t *testing.T, f *fakeStore, ep store.AuthEndpoint) (token string) {
	t.Helper()
	token, hash, err := oauthsrv.MintToken()
	if err != nil {
		t.Fatalf("mint oauth token: %v", err)
	}
	f.byOAuthHash[hash] = ep
	return token
}

// listToolNames connects with the given bearer and returns the sorted advertised tool names.
func listToolNames(t *testing.T, ctx context.Context, url, token string) []string {
	t.Helper()
	cs, err := connect(t, ctx, url, token)
	if err != nil {
		t.Fatalf("connect with %q-shaped credential: %v", token[:4], err)
	}
	defer func() { _ = cs.Close() }()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// TestTwoCredentialShapesOneCapability is the SPEC-0016 scenario verbatim: the same endpoint
// accessed once via its static sbk_ bearer and once via an OAuth access token sees the identical
// tool surface and scope limits — everything past "resolve to endpoint ID" is one code path.
func TestTwoCredentialShapesOneCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	static := vend(t, f, "dual-agent-ab12cd34", []string{"github", "ci"}, []string{"list_todos", "claim", "complete"})
	// The OAuth token resolves to the SAME endpoint row the static bearer does.
	oauthTok := vendOAuth(t, f, f.byHash[cred.Hash(static)])
	ts := newTestServer(t, f)
	url := ts.URL + "/mcp/dual-agent-ab12cd34"

	staticTools := listToolNames(t, ctx, url, static)
	oauthTools := listToolNames(t, ctx, url, oauthTok)
	if !slices.Equal(staticTools, oauthTools) {
		t.Fatalf("tool surfaces differ: static=%v oauth=%v", staticTools, oauthTools)
	}
	// list_todos implies get_todo (SPEC-0034 REQ-8).
	if want := []string{"claim", "complete", "get_todo", "list_todos"}; !slices.Equal(oauthTools, want) {
		t.Fatalf("advertised tools = %v, want %v", oauthTools, want)
	}
}

// TestOAuthTokenAuthMatrix: unknown/revoked OAuth tokens draw 401 with the RFC 9728
// resource_metadata challenge (once a base URL is wired), and a valid OAuth token presented
// against a DIFFERENT endpoint's path draws 403 — the same matrix as static bearers.
func TestOAuthTokenAuthMatrix(t *testing.T) {
	f := newFakeStore()
	epA := store.AuthEndpoint{ID: "ep-a", AgentID: "ag-1", AgentName: "a", OwnerHumanID: "h-1",
		Slug: "agent-a-11111111", ScopeQueues: []string{"q"}, ScopeVerbs: []string{"list_todos"}}
	tokenA := vendOAuth(t, f, epA)
	ts, h := newTestServerHandler(t, f)
	h.SetBaseURL("https://sb.example.com")

	post := func(url, token string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		return res
	}

	// Unknown OAuth-shaped token → 401 + challenge (identical to a bad static bearer).
	res := post(ts.URL+"/mcp/agent-a-11111111", "not-a-real-oauth-token-aaaaaaaaaaaaaaaaaaaa")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown oauth token: status = %d, want 401", res.StatusCode)
	}
	if ch := res.Header.Get("WWW-Authenticate"); !strings.Contains(ch, "Bearer") ||
		!strings.Contains(ch, "resource_metadata=") {
		t.Fatalf("challenge = %q, want RFC 9728 resource_metadata challenge", ch)
	}
	_ = res.Body.Close()

	// "Revoked": the resolution disappears (store returns ErrNotFound once revoked/expired) → 401.
	f.byOAuthHash = map[string]store.AuthEndpoint{}
	res = post(ts.URL+"/mcp/agent-a-11111111", tokenA)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked oauth token: status = %d, want 401", res.StatusCode)
	}
	_ = res.Body.Close()

	// Valid token, wrong endpoint path → 403, same as a static bearer.
	tokenA2 := vendOAuth(t, f, epA)
	res = post(ts.URL+"/mcp/agent-b-22222222", tokenA2)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-endpoint oauth token: status = %d, want 403", res.StatusCode)
	}
	_ = res.Body.Close()
}
