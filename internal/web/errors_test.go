package web

// Governing: SPEC-0012 REQ "Error Handling" — issue #74 AC: store.ErrNotFound maps to a generic
// 404, every other store error maps to a generic 500, render failures map to a generic 500, and
// the underlying error detail is logged server-side only (never sent to the client).

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

// testErrHandler returns a Handler whose slog output is captured in the returned buffer, so tests
// can assert that error detail lands in the server log and nowhere else.
func testErrHandler() (*Handler, *bytes.Buffer) {
	var logs bytes.Buffer
	return &Handler{log: slog.New(slog.NewTextHandler(&logs, nil))}, &logs
}

func TestNotFoundOrMapsErrNotFoundTo404(t *testing.T) {
	h, _ := testErrHandler()
	rec := httptest.NewRecorder()

	h.notFoundOr(rec, fmt.Errorf("get agent: %w", store.ErrNotFound))

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "not found" {
		t.Fatalf("body = %q, want generic %q", got, "not found")
	}
}

func TestNotFoundOrMapsOtherStoreErrorsTo500(t *testing.T) {
	h, logs := testErrHandler()
	rec := httptest.NewRecorder()

	h.notFoundOr(rec, errors.New("pq: connection refused"))

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "internal error" {
		t.Fatalf("body = %q, want generic %q", got, "internal error")
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("response leaked internal error detail: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("server log missing error detail: %q", logs.String())
	}
}

func TestFailReturns500GenericAndLogsDetail(t *testing.T) {
	h, logs := testErrHandler()
	rec := httptest.NewRecorder()

	h.fail(rec, errors.New("pq: deadlock detected"))

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "internal error" {
		t.Fatalf("body = %q, want generic %q", got, "internal error")
	}
	if strings.Contains(rec.Body.String(), "deadlock") {
		t.Fatalf("response leaked internal error detail: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "deadlock detected") {
		t.Fatalf("server log missing error detail: %q", logs.String())
	}
}

func TestRenderFailureReturns500Generic(t *testing.T) {
	h, logs := testErrHandler()
	// A template that parses but fails at execute time (view has no such field), exercising the
	// buffered-render failure path: nothing is written to the client before the 500.
	h.pages = map[string]*template.Template{
		"broken": template.Must(template.New("layout").Parse(`{{.NoSuchField}}`)),
	}
	rec := httptest.NewRecorder()

	h.render(rec, "broken", view{})

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "render error" {
		t.Fatalf("body = %q, want generic %q", got, "render error")
	}
	if strings.Contains(rec.Body.String(), "NoSuchField") {
		t.Fatalf("response leaked template error detail: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "NoSuchField") {
		t.Fatalf("server log missing render error detail: %q", logs.String())
	}
}
