package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifyGitHub(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"action":"opened"}`)
	good := sign(secret, body)

	if !verifyGitHub(secret, body, good) {
		t.Fatal("valid signature should verify")
	}
	// Wrong secret.
	if verifyGitHub("other", body, good) {
		t.Fatal("wrong secret must fail")
	}
	// Tampered body.
	if verifyGitHub(secret, []byte(`{"action":"closed"}`), good) {
		t.Fatal("tampered body must fail")
	}
	// Missing / malformed prefix.
	if verifyGitHub(secret, body, "") || verifyGitHub(secret, body, "deadbeef") {
		t.Fatal("missing/malformed signature must fail")
	}
}

func TestSummarizeGitHub(t *testing.T) {
	pr := []byte(`{"action":"opened","pull_request":{"number":482,"title":"Fix login"},"repository":{"full_name":"joestump/switchboard"}}`)
	got := summarizeGitHub("pull_request", pr)
	if !strings.Contains(got, "#482") || !strings.Contains(got, "joestump/switchboard") || !strings.Contains(got, "Fix login") {
		t.Fatalf("summary missing fields: %q", got)
	}
	// Unknown event falls back gracefully.
	if got := summarizeGitHub("ping", []byte(`{}`)); got != "github ping" {
		t.Fatalf("fallback summary: %q", got)
	}
}

// A review delivery must title the pull request it is about, not just name its own event. The event
// Gitea sends is the verdict (its body action is always "reviewed"), and a review comment arrives in
// the issue shape rather than the pull-request one, so both shapes are exercised here. Eleven todos
// covering six pull requests all read "gitea pull_request_comment in abrahms/forge" before this:
// a review queue nobody could read without claiming every row.
func TestSummarizeForgeReviewEvents(t *testing.T) {
	reviewed := `"action":"reviewed","repository":{"full_name":"abrahms/forge"},` +
		`"pull_request":{"number":211,"title":"Name the fix instead of counting lines"}`
	commented := `"action":"created","is_pull":true,"repository":{"full_name":"stump.wtf/harness"},` +
		`"issue":{"number":306,"title":"feat(scheduler): suspend-safe wall-clock tick",` +
		`"pull_request":{"merged":false}}`
	requested := `"action":"review_requested","repository":{"full_name":"abrahms/ops"},` +
		`"pull_request":{"number":31,"title":"Read a number only when the whole argument is one"}`

	for _, tc := range []struct {
		name, provider, event, body, want string
	}{
		{"approved", "gitea", "pull_request_approved", "{" + reviewed + "}",
			"PR #211 approved in abrahms/forge — Name the fix instead of counting lines"},
		{"rejected", "gitea", "pull_request_rejected", "{" + reviewed + "}",
			"PR #211 changes requested in abrahms/forge — Name the fix instead of counting lines"},
		{"comment, issue-shaped payload", "gitea", "pull_request_comment", "{" + commented + "}",
			"PR #306 comment in stump.wtf/harness — feat(scheduler): suspend-safe wall-clock tick"},
		{"review requested", "gitea", "pull_request_review_request", "{" + requested + "}",
			"PR #31 review requested in abrahms/ops — Read a number only when the whole argument is one"},
		// A review delivery with no pull request in it names nothing, so it keeps the event-only title
		// rather than inventing "PR #0 approved".
		{"no pull request in the payload", "github", "pull_request_approved", `{"action":"reviewed"}`,
			"github pull_request_approved"},
		{"no pull request, with a repo", "gitea", "pull_request_rejected",
			`{"repository":{"full_name":"o/r"}}`, "gitea pull_request_rejected in o/r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeForge(tc.provider, tc.event, []byte(tc.body)); got != tc.want {
				t.Errorf("summarizeForge(%q, %q, …)\n got %q\nwant %q", tc.provider, tc.event, got, tc.want)
			}
		})
	}
}

func TestSanitizeHeadersRedacts(t *testing.T) {
	h := map[string][]string{
		"X-Hub-Signature-256": {"sha256=secret"},
		"Content-Type":        {"application/json"},
	}
	out := string(sanitizeHeaders(h))
	if strings.Contains(out, "sha256=secret") {
		t.Fatalf("signature must be redacted: %s", out)
	}
	if !strings.Contains(out, "«redacted»") || !strings.Contains(out, "application/json") {
		t.Fatalf("sanitize wrong: %s", out)
	}
}
