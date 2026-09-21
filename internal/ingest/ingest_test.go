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
