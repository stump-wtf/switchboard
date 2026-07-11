package ingest

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A webhook body over the 5 MiB ceiling must be rejected with 413 BEFORE any HMAC verification —
// never truncated-then-verified. Governing: SPEC-0001 REQ body limits.
func TestGitHubRejectsOversizedBody(t *testing.T) {
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{GitHubSecret: "secret"})

	big := bytes.Repeat([]byte("x"), maxBody+1)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(big))
	req.Header.Set("X-Hub-Signature-256", sign("secret", big)) // valid sig, but body is too large
	rec := httptest.NewRecorder()

	i.GitHub(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook body: got %d, want 413", rec.Code)
	}
}
