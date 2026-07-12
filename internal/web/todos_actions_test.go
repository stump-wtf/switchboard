package web

// Handler-level coverage for the SPEC-0013 Todos drawer actions that runs in the `go test ./...`
// gate WITHOUT a database: the store-error → generic-response mapping (respondTodoAction) and the
// background-transition toast copy (toastText). The end-to-end store transitions live in the
// DB-backed internal/server suite (skipped without SWITCHBOARD_TEST_DATABASE_URL); these bind the
// pure response/copy contract that the CI gate can actually exercise.
//
// Governing: SPEC-0013 REQ "Error Handling Standards" (generic to the user, specific in the log),
// REQ "Live Updates and Toasts" (toast on background transition).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// TestRespondTodoActionMapsStoreErrors proves the SPEC-0013 error-handling standard: a drawer action
// that fails against the store returns a GENERIC response with no internal detail. The sentinel
// errors map to distinguishable status codes (409 conflict / 404 not-found); any other error is a
// generic 500. The error branches return before any store read, so a nil-store handler exercises
// them in CI (the success branch is covered end-to-end by the DB-backed server suite).
func TestRespondTodoActionMapsStoreErrors(t *testing.T) {
	h := newTestHandler(t)

	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string // the exact generic body — proof no internal detail leaks
	}{
		{"conflict", store.ErrConflict, http.StatusConflict, "conflict"},
		{"not found", store.ErrNotFound, http.StatusNotFound, "not found"},
		{"opaque", errors.New("pq: deadlock detected on relation todos"), http.StatusInternalServerError, "internal error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/todos/td_1/complete", nil)
			h.respondTodoAction(rec, req, "CompleteTodo", store.Todo{}, c.err)

			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantCode)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != c.wantBody {
				t.Fatalf("body = %q, want %q", got, c.wantBody)
			}
			// The opaque driver error must never reach the client — only the generic body may.
			if c.name == "opaque" && strings.Contains(rec.Body.String(), "deadlock") {
				t.Errorf("internal store error leaked to the client: %q", rec.Body.String())
			}
		})
	}
}

// TestToastTextByTransition pins the background-transition toast copy (SPEC-0013 "Toast on
// background transition"): each announced verb carries the short todo id and a human-readable
// outcome; verbs that only move the feed (creation) produce no toast. Resolvable owners degrade to
// the pure label with a nil store, so this runs in the CI gate.
func TestToastTextByTransition(t *testing.T) {
	h := newTestHandler(t)
	td := store.Todo{ID: "td_8f2a99aa", Owner: "op:h1"}
	id := shortID(td.ID) // the id the operator sees in the toast

	cases := map[string]string{
		"todo_claimed":    id + " · claimed · operator",
		"todo_completed":  id + " · completed · ack sent",
		"todo_failed":     id + " · failed · attempts exhausted",
		"todo_resurfaced": id + " · re-surfaced to queue",
	}
	for name, want := range cases {
		if got := h.toastText(t.Context(), name, td); got != want {
			t.Errorf("toastText(%q) = %q, want %q", name, got, want)
		}
	}
	// A below-cap fail carries a scheduled retry window (SPEC-0003 scheduled backoff) and the toast
	// tells the truthful story — it will retry, not "attempts exhausted".
	next := time.Now().Add(30 * time.Second)
	scheduled := td
	scheduled.NextRetryAt = &next
	if got, want := h.toastText(t.Context(), "todo_failed", scheduled), id+" · failed · will retry with backoff"; got != want {
		t.Errorf("toastText(todo_failed, retry scheduled) = %q, want %q", got, want)
	}
	// todo_created (a new row appearing announces itself) and any unmapped name produce no toast.
	if got := h.toastText(t.Context(), "todo_created", td); got != "" {
		t.Errorf("toastText(todo_created) = %q, want empty (row appearance is its own announcement)", got)
	}
	if got := h.toastText(t.Context(), "unknown", td); got != "" {
		t.Errorf("toastText(unknown) = %q, want empty", got)
	}
}
