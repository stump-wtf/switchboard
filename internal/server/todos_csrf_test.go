// CSRF enforcement bound to the actual SPEC-0013 Todos drawer action routes. RequireCSRF runs after
// RequireHuman, so isolating a 403 (rather than the anonymous → /login redirect) needs a valid
// session — hence the DB-backed router, skipped without SWITCHBOARD_TEST_DATABASE_URL and run against
// Postgres on the GitHub mirror. This complements the generic session-group CSRF proof in
// internal/auth by asserting every one of the six operator lifecycle POSTs is guarded.
// Governing: SPEC-0013 REQ "Todo Detail Drawer" (actions are CSRF-guarded HTMX POSTs), SPEC-0008 CSRF.
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// postNoCSRF issues a session-authenticated HTMX POST WITHOUT any CSRF token.
func postNoCSRF(t *testing.T, r chi.Router, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// postForgedCSRF issues a session-authenticated POST with a CSRF token that is not the session's.
func postForgedCSRF(t *testing.T, r chi.Router, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", "not-the-session-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestTodoActionsRequireCSRF proves each operator lifecycle POST rejects a missing or forged CSRF
// token with 403 — before any store transition runs — so a cross-site form can never claim, complete,
// fail, retry, extend, or release a todo. A valid session is present; only the CSRF token is bad.
func TestTodoActionsRequireCSRF(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, token := mintSession(t, st, ctx, "csrf-op", "Op", "csrf@example.com")

	// The id need not resolve: RequireCSRF rejects before the handler reads the store, so a 403 here
	// proves the guard fires ahead of any lifecycle logic.
	actions := []string{"claim", "complete", "fail", "retry", "extend", "release"}
	for _, action := range actions {
		path := "/todos/td_any/" + action
		if rec := postNoCSRF(t, r, token, path); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF: got %d, want 403", path, rec.Code)
		}
		if rec := postForgedCSRF(t, r, token, path); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with forged CSRF: got %d, want 403", path, rec.Code)
		}
	}
}
