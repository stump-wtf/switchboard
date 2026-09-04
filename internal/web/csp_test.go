package web

import (
	"strings"
	"testing"
)

// The consent screen's whole purpose is to submit and then be redirected to the OAuth client's
// callback. form-action governs that redirect, so the callback's origin — and only its origin —
// has to be in the policy the consent document carries.
func TestCSPAllowingFormActionTo(t *testing.T) {
	for _, tc := range []struct {
		name, redirectURI, want string
	}{
		{
			// The case that was broken: an RFC 8252 loopback callback, which is what every CLI
			// (including `switchboard login`) registers.
			name:        "loopback callback",
			redirectURI: "http://127.0.0.1:51688/callback",
			want:        "form-action 'self' http://127.0.0.1:51688;",
		},
		{
			// Only the origin is granted. A path or query in the policy would be meaningless to
			// form-action and would leak the request's shape into a response header.
			name:        "path and query are stripped",
			redirectURI: "https://client.example.com/oauth/cb?next=%2Fdash&x=1",
			want:        "form-action 'self' https://client.example.com;",
		},
		{
			name:        "custom scheme keeps its origin",
			redirectURI: "https://app.example.com:8443/cb",
			want:        "form-action 'self' https://app.example.com:8443;",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CSPAllowingFormActionTo(tc.redirectURI)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("policy = %q, want it to contain %q", got, tc.want)
			}
			// Widening one directive must not disturb the rest of the policy.
			for _, directive := range []string{
				"default-src 'self'", "script-src 'self'", "connect-src 'self'",
				"base-uri 'none'", "frame-ancestors 'none'",
			} {
				if !strings.Contains(got, directive) {
					t.Errorf("policy = %q, lost directive %q", got, directive)
				}
			}
		})
	}
}

// A redirect URI that cannot yield an origin must widen nothing rather than emit a malformed
// policy — a broken directive would fail open on some parsers.
func TestCSPAllowingFormActionToRejectsUnusableURIs(t *testing.T) {
	for _, uri := range []string{"", "not a url", "/relative/only", "urn:ietf:wg:oauth:2.0:oob", "://nohost"} {
		if got := CSPAllowingFormActionTo(uri); got != ContentSecurityPolicy {
			t.Errorf("CSPAllowingFormActionTo(%q) = %q, want the unwidened base policy", uri, got)
		}
	}
}

// CSPAllowingFormActionTo rewrites the base policy by matching a literal substring. If the policy
// is ever reworded so that literal no longer appears, the rewrite would silently no-op and the
// consent screen would regress to exactly the bug this fixes.
func TestBasePolicyContainsTheDirectiveTheRewriteTargets(t *testing.T) {
	if !strings.Contains(ContentSecurityPolicy, formActionSelf) {
		t.Fatalf("ContentSecurityPolicy = %q, does not contain %q — CSPAllowingFormActionTo would no-op",
			ContentSecurityPolicy, formActionSelf)
	}
}
