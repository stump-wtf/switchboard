package ingest

import (
	"net/http"
	"strings"
	"testing"
)

// Every signature-, token-, and secret-bearing header named by the spec — plus unenumerated
// credential-suggesting names caught by the fragment match — must be redacted before persist,
// while ordinary headers survive verbatim. Governing: SPEC-0001 REQ "Header and Secret
// Sanitization Before Persist".
func TestSanitizeHeadersMatrix(t *testing.T) {
	cases := []struct {
		header   string
		value    string
		redacted bool
	}{
		// The spec's explicit denylist.
		{"X-Hub-Signature", "sha1=SECRETSIG", true},
		{"X-Hub-Signature-256", "sha256=SECRETSIG", true},
		{"Stripe-Signature", "t=1,v1=SECRETSIG", true},
		{"X-Slack-Signature", "v0=SECRETSIG", true},
		{"Authorization", "Bearer SECRETTOKEN", true},
		{"Proxy-Authorization", "Basic SECRETTOKEN", true},
		{"Cookie", "session=SECRETCOOKIE", true},
		{"Set-Cookie", "session=SECRETCOOKIE", true},
		{"X-Api-Key", "SECRETKEY", true},
		{"X-Webhook-Token", "SECRETTOKEN", true},
		// Unenumerated credential-bearing names (fragment match — defense in depth).
		{"X-Gitlab-Token", "SECRETTOKEN", true},
		{"X-Custom-Secret", "SECRETVALUE", true},
		{"X-Vendor-Api-Key", "SECRETKEY", true},
		{"X-Auth-Ticket", "SECRETTICKET", true},
		// Ordinary headers are preserved verbatim.
		{"Content-Type", "application/json", false},
		{"X-GitHub-Event", "pull_request", false},
		{"X-GitHub-Delivery", "72d3162e-cc78-11e3", false},
		{"User-Agent", "GitHub-Hookshot/abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)
			out := string(sanitizeHeaders(h))
			if tc.redacted {
				if strings.Contains(out, tc.value) {
					t.Errorf("value of %s must not survive sanitizeHeaders: %s", tc.header, out)
				}
				if !strings.Contains(out, "«redacted»") {
					t.Errorf("redaction marker missing for %s: %s", tc.header, out)
				}
			} else if !strings.Contains(out, tc.value) {
				t.Errorf("non-secret header %s must be preserved: %s", tc.header, out)
			}
		})
	}
}

// A secret carried in a URL query string inside a header value (e.g. a ?token= webhook URL echoed in
// Referer/Link) must be redacted before the headers are persisted. Governing: SPEC-0001.
func TestSanitizeHeadersRedactsURLSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Referer", "https://hooks.example.com/cb?token=SUPERSECRETVALUE&repo=x")
	h.Set("Link", "<https://api.example.com/next?api_key=KEY12345&page=2>; rel=next")
	h.Set("X-Normal", "nothing-secret-here")

	out := string(sanitizeHeaders(h))

	for _, leak := range []string{"SUPERSECRETVALUE", "KEY12345"} {
		if strings.Contains(out, leak) {
			t.Errorf("secret %q must not survive sanitizeHeaders: %s", leak, out)
		}
	}
	if !strings.Contains(out, "«redacted»") {
		t.Errorf("redaction marker missing: %s", out)
	}
	// Non-secret content is preserved.
	if !strings.Contains(out, "repo=x") || !strings.Contains(out, "nothing-secret-here") {
		t.Errorf("non-secret content should be preserved: %s", out)
	}
}
