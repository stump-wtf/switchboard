package ingest

import (
	"net/http"
	"strings"
	"testing"
)

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
