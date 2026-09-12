package mcp

// Tests for the RFC 9728 WWW-Authenticate challenge on MCP 401s (ADR-0019; SPEC-0016 REQ
// "Protected Resource Metadata" scenario "Client discovers how to authorize"): an unauthenticated
// request to /mcp/{slug} must come back with resource_metadata pointing at that mount's
// protected-resource metadata document, from which the client discovers the authorization server.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/oauthsrv"
)

// TestChallengeCarriesResourceMetadata: with a base URL wired (as Run always does), every 401
// shape — missing, unknown, and revoked credential — carries the resource_metadata challenge for
// the exact mount that was addressed.
func TestChallengeCarriesResourceMetadata(t *testing.T) {
	f := newFakeStore()
	vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts, h := newTestServerHandler(t, f)
	h.SetBaseURL("https://sb.example.com")

	wantChallenge := `Bearer realm="switchboard", ` +
		`resource_metadata="https://sb.example.com/.well-known/oauth-protected-resource/mcp/agent-a-11111111"`

	cases := []struct {
		name  string
		token string
	}{
		{"missing credential", ""},
		{"unknown credential", "sbk_bogus"},
		{"revoked credential", revokedToken(t, f)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := rawPost(t, ts.URL+"/mcp/agent-a-11111111", tc.token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != wantChallenge {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, wantChallenge)
			}
		})
	}

	// The metadata URL in the challenge must be derived per mount, not pinned to one slug.
	resp := rawPost(t, ts.URL+"/mcp/other-b-22222222", "", `{}`)
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got,
		`resource_metadata="https://sb.example.com/.well-known/oauth-protected-resource/mcp/other-b-22222222"`) {
		t.Fatalf("challenge for another mount = %q, want its own resource_metadata URL", got)
	}
}

// TestChallengeWithoutBaseURL: with no base URL wired the challenge stays the bare realm — a
// well-formed header with no fabricated origin.
func TestChallengeWithoutBaseURL(t *testing.T) {
	f := newFakeStore()
	ts := newTestServer(t, f)

	resp := rawPost(t, ts.URL+"/mcp/agent-a-11111111", "", `{}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="switchboard"` {
		t.Fatalf("WWW-Authenticate = %q, want the bare realm challenge", got)
	}
}

// TestChallengeNeverReflectsJunkSlugs: a non-slug-shaped path segment is never echoed into the
// response header — the challenge falls back to the bare realm (SPEC-0016 "Security Requirements →
// Output encoding for user-supplied data").
func TestChallengeNeverReflectsJunkSlugs(t *testing.T) {
	f := newFakeStore()
	ts, h := newTestServerHandler(t, f)
	h.SetBaseURL("https://sb.example.com")

	for _, junk := range []string{"UPPER", "semi;colon", "quote%22mark", "pct%0d%0aSet-Cookie"} {
		resp := rawPost(t, ts.URL+"/mcp/"+junk, "", `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", junk, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="switchboard"` {
			t.Fatalf("%s: WWW-Authenticate = %q, want the bare realm challenge (no reflection)", junk, got)
		}
	}
}

// TestChallengeURLMatchesMetadataMount: the URL advertised in the challenge is byte-identical to
// the one internal/oauthsrv serves the document at (shared derivation, asserted so the resource
// server and AS surfaces can never drift apart). SPEC-0016 scenario "Client discovers how to
// authorize" hinges on this equality.
func TestChallengeURLMatchesMetadataMount(t *testing.T) {
	f := newFakeStore()
	ts, h := newTestServerHandler(t, f)
	h.SetBaseURL("https://sb.example.com")

	resp := rawPost(t, ts.URL+"/mcp/agent-a-11111111", "", `{}`)
	challenge := resp.Header.Get("WWW-Authenticate")
	const marker = `resource_metadata="`
	i := strings.Index(challenge, marker)
	if i < 0 {
		t.Fatalf("challenge %q carries no resource_metadata", challenge)
	}
	urlPart := challenge[i+len(marker):]
	urlPart = strings.TrimSuffix(urlPart, `"`)
	if want := oauthsrv.ResourceMetadataURL("https://sb.example.com", "agent-a-11111111"); urlPart != want {
		t.Fatalf("challenge URL = %q, want %q (the oauthsrv metadata mount)", urlPart, want)
	}
}
