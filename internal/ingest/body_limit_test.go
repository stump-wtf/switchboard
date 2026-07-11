package ingest

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// requireJSONError asserts the uniform rejection shape shared by every receiver: a JSON body
// carrying a non-empty "error" message. Governing: SPEC-0001 REQ "Error Handling Standards".
func requireJSONError(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("rejection Content-Type = %q, want application/json (body %s)", ct, rec.Body.String())
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Error == "" {
		t.Fatalf("rejection body must be {\"error\": ...}: %s (%v)", rec.Body.String(), err)
	}
}

// Every body-accepting endpoint shares the same bounded-read contract: a body over the 5 MiB
// ceiling is rejected 413 BEFORE any verification or persist — never truncated-then-verified. The
// nil store proves nothing is persisted (a persist attempt would panic). Governing: SPEC-0001 REQ
// "Request Body Size Limits", REQ "Error Handling Standards".
func TestOversizedBodyRejectedEverywhere(t *testing.T) {
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		GitHubSecret: "secret", StripeSecret: "secret", SlackSecret: "secret",
		DevLogin: true,
	})

	big := bytes.Repeat([]byte("x"), maxBody+1)
	cases := []struct {
		name    string
		path    string
		hdr     map[string]string
		handler http.HandlerFunc
	}{
		{"github", "/webhooks/github",
			map[string]string{"X-Hub-Signature-256": sign("secret", big)}, // valid sig, body too large
			i.GitHub},
		{"stripe", "/webhooks/stripe", nil, i.Stripe},
		{"slack", "/webhooks/slack", nil, i.Slack},
		{"dev todos", "/dev/todos", nil, i.DevCreateTodo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(big))
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized body on %s: got %d, want 413", tc.path, rec.Code)
			}
			requireJSONError(t, rec)
		})
	}
}

// Domain rejections map to their specific status (401/403/404/413/503), never a generic 500, and
// every rejection shares the uniform JSON error shape. The nil store proves no rejection path
// persists anything. Governing: SPEC-0001 REQ "Error Handling Standards", REQ "Signed Webhook
// Verification" (401, no persist), scenario "Signature secret not configured" (503).
func TestRejectionStatusAndShape(t *testing.T) {
	configured := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		GitHubSecret: "secret", StripeSecret: "secret", SlackSecret: "secret",
	})
	unconfigured := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})

	cases := []struct {
		name    string
		handler http.HandlerFunc
		hdr     map[string]string
		want    int
	}{
		{"github bad signature", configured.GitHub,
			map[string]string{"X-Hub-Signature-256": "sha256=deadbeef"}, http.StatusUnauthorized},
		{"github missing signature", configured.GitHub, nil, http.StatusUnauthorized},
		{"stripe bad signature", configured.Stripe,
			map[string]string{"Stripe-Signature": "t=1,v1=deadbeef"}, http.StatusUnauthorized},
		{"slack missing signature", configured.Slack, nil, http.StatusUnauthorized},
		{"github secret not configured", unconfigured.GitHub, nil, http.StatusServiceUnavailable},
		{"stripe secret not configured", unconfigured.Stripe, nil, http.StatusServiceUnavailable},
		{"slack secret not configured", unconfigured.Slack, nil, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhooks/x", strings.NewReader(`{"a":1}`))
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			requireJSONError(t, rec)
		})
	}
}
