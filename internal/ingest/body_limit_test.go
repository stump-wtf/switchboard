package ingest

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// requireJSONError asserts the uniform rejection shape shared by every body-accepting surface: a
// JSON body carrying a non-empty "error" message. Governing: SPEC-0001 REQ "Error Handling
// Standards".
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

// Every body-accepting surface shares the same bounded-read contract: a body over the 5 MiB
// ceiling is rejected 413 BEFORE any lookup, verification, or persist — never
// truncated-then-verified. The nil store proves nothing is persisted (a persist or lookup attempt
// would panic). Governing: SPEC-0001 REQ "Request Body Size Limits", REQ "Error Handling
// Standards".
func TestOversizedBodyRejectedEverywhere(t *testing.T) {
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{DevLogin: true})

	big := bytes.Repeat([]byte("x"), maxBody+1)
	cases := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		// readBody runs before the token lookup, so the 413 fires with a nil store.
		{"self-managed webhook", "/webhooks/w/sometoken", i.SelfManaged},
		// readBody runs first here too; nothing is persisted on rejection.
		{"dev todos", "/dev/todos", i.DevCreateTodo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(big))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized body on %s: got %d, want 413", tc.path, rec.Code)
			}
			requireJSONError(t, rec)
		})
	}
}
