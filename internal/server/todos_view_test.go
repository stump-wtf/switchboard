// End-to-end coverage for the SPEC-0013 Todos view and detail drawer through the real router +
// PostgreSQL: the durable-queue table renders with filter pills, HTMX filter/search returns just the
// panel fragment, the drawer fragment opens for a todo, and the operator lifecycle actions
// (claim → complete via the drawer) drive real store transitions and return the appropriate
// fragment. Skipped without SWITCHBOARD_TEST_DATABASE_URL, matching the store test pattern.
// Governing: SPEC-0013 REQ "Todos View — Durable Queue", REQ "Todo Detail Drawer".
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/store"
)

// postAs issues a session-authenticated HTMX POST with the CSRF header and an HX-Target, the way the
// operator board's buttons do (CSRF arrives via the layout's hx-headers).
func postAs(t *testing.T, r chi.Router, token, csrf, hxTarget, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	if hxTarget != "" {
		req.Header.Set("HX-Target", hxTarget)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func getHX(t *testing.T, r chi.Router, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestTodosViewAndDrawerFlow(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	h, token := mintSession(t, st, ctx, "todos-op", "Op", "op@example.com")
	// Every todo names its owning endpoint (ADR-0022; todos.endpoint_id is NOT NULL), so the view
	// tests seed a real tenant rather than a bare queue string.
	ep := seedEndpoint(t, st, ctx, h.ID, "todos-view-agent", "hash-todos", "sbk_todos0")

	// A webhook-born todo lands in the durable queue.
	_, td, created, err := st.CreateEventTodo(ctx,
		store.EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "d-flow", TrustMode: "signed", Verified: true, Payload: []byte(`{"action":"opened"}`)},
		store.CreateTodoParams{EndpointID: ep.ID, Queue: "reviews", Source: "github", Kind: "push", Title: "review", Payload: []byte(`{"action":"opened"}`), IdempotencyKey: "d-flow"})
	if err != nil || !created {
		t.Fatalf("seed todo: created=%v err=%v", created, err)
	}

	// Full Todos page: pills + the row.
	page := getAs(t, r, token, "/todos")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /todos: %d", page.Code)
	}
	csrf := scrapeCSRF(t, page.Body.String())
	for _, want := range []string{`id="sb-todos-panel"`, "sb-fpill", "id=\"sb-tr-" + td.ID + "\"", "github"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("GET /todos missing %q", want)
		}
	}

	// HTMX filter: only the panel fragment (no full-page shell).
	panel := getHX(t, r, token, "/todos?filter=pending")
	if panel.Code != http.StatusOK {
		t.Fatalf("GET /todos?filter=pending (HX): %d", panel.Code)
	}
	if strings.Contains(panel.Body.String(), "<!doctype html>") {
		t.Errorf("HTMX filter should return the panel fragment, not a full page")
	}
	if !strings.Contains(panel.Body.String(), "sb-fpill--active") || !strings.Contains(panel.Body.String(), "sb-tr-"+td.ID) {
		t.Errorf("panel fragment missing active pill or row: %.300s", panel.Body.String())
	}

	// HTMX drawer fragment.
	drawer := getHX(t, r, token, "/todos/"+td.ID)
	if drawer.Code != http.StatusOK {
		t.Fatalf("GET /todos/{id} (HX): %d", drawer.Code)
	}
	db := drawer.Body.String()
	if !strings.Contains(db, `role="dialog"`) || !strings.Contains(db, "opened") {
		t.Errorf("drawer fragment missing dialog or payload: %.300s", db)
	}
	if strings.Contains(db, "<!doctype html>") {
		t.Errorf("drawer HTMX response should be a fragment, not a full page")
	}

	// Standalone drawer fallback (no HX-Request) renders the full page.
	standalone := getAs(t, r, token, "/todos/"+td.ID)
	if !strings.Contains(standalone.Body.String(), "<!doctype html>") || !strings.Contains(standalone.Body.String(), "sb-drawer-inline") {
		t.Errorf("standalone drawer page should render the full shell")
	}

	// Operator claim from the drawer → refreshed drawer showing the claimed state + lease card.
	claim := postAs(t, r, token, csrf, "sb-overlay", "/todos/"+td.ID+"/claim")
	if claim.Code != http.StatusOK {
		t.Fatalf("POST claim: %d body=%s", claim.Code, claim.Body.String())
	}
	if !strings.Contains(claim.Body.String(), "sb-lease") || !strings.Contains(claim.Body.String(), "/release") {
		t.Errorf("claim response should be the drawer with a lease card: %.300s", claim.Body.String())
	}
	// Read back through the ENDPOINT SCOPE: this asserts the operator's action drove the real store
	// transition and that the todo is still pinned to the endpoint that owned it — an operator
	// claim moves state, never tenancy.
	got, _ := st.GetTodo(ctx, ep.ID, td.ID)
	if got.State != "claimed" || got.Owner != "op:"+h.ID {
		t.Fatalf("todo after claim = %+v, want claimed by op:%s", got, h.ID)
	}

	// Complete from the drawer → done.
	complete := postAs(t, r, token, csrf, "sb-overlay", "/todos/"+td.ID+"/complete")
	if complete.Code != http.StatusOK {
		t.Fatalf("POST complete: %d body=%s", complete.Code, complete.Body.String())
	}
	done, _ := st.GetTodo(ctx, ep.ID, td.ID)
	if done.State != "done" {
		t.Fatalf("todo after complete = %q, want done", done.State)
	}

	// A missing todo action is a 404; an out-of-state action is a 409 (sentinel-driven, generic body).
	if rec := postAs(t, r, token, csrf, "sb-overlay", "/todos/td_missing/complete"); rec.Code != http.StatusNotFound {
		t.Fatalf("complete missing: %d, want 404", rec.Code)
	}
	if rec := postAs(t, r, token, csrf, "sb-overlay", "/todos/"+td.ID+"/complete"); rec.Code != http.StatusConflict {
		t.Fatalf("re-complete a done todo: %d, want 409", rec.Code)
	}
}

// A table-row action (HX-Target sb-tr-<id>) returns the refreshed row, not the drawer.
func TestTodoRowActionReturnsRow(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	h, token := mintSession(t, st, ctx, "row-op", "Op", "row@example.com")
	ep := seedEndpoint(t, st, ctx, h.ID, "row-view-agent", "hash-row", "sbk_row000")

	td, _, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: ep.ID, Queue: "reviews", Source: "github", Kind: "push", Title: "t", IdempotencyKey: "row-key"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	page := getAs(t, r, token, "/todos")
	csrf := scrapeCSRF(t, page.Body.String())

	rec := postAs(t, r, token, csrf, "sb-tr-"+td.ID, "/todos/"+td.ID+"/claim")
	if rec.Code != http.StatusOK {
		t.Fatalf("row claim: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="sb-tr-`+td.ID+`"`) || !strings.Contains(body, "<tr") {
		t.Errorf("row action should return the refreshed <tr>: %.300s", body)
	}
	if strings.Contains(body, `role="dialog"`) {
		t.Errorf("row action must not return the drawer")
	}
}
