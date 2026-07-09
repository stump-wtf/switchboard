package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCSRFTokenBoundToSession(t *testing.T) {
	if csrfToken("session-a") != csrfToken("session-a") {
		t.Fatal("csrfToken must be deterministic for a given session")
	}
	if csrfToken("session-a") == csrfToken("session-b") {
		t.Fatal("distinct sessions must yield distinct CSRF tokens")
	}
	if strings.Contains(csrfToken("super-secret-session"), "super-secret-session") {
		t.Fatal("CSRF token must not leak the session token")
	}
}

func TestRequireCSRF(t *testing.T) {
	a := &Authenticator{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ok := a.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	const sess = "the-session-token"
	good := csrfToken(sess)

	// GET (safe method) passes without a token.
	rec := httptest.NewRecorder()
	ok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET should pass: got %d", rec.Code)
	}

	newPost := func(token string, withCookie bool) *http.Request {
		body := url.Values{"csrf_token": {token}}.Encode()
		r := httptest.NewRequest(http.MethodPost, "/agents", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withCookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		}
		return r
	}

	// POST without a session cookie → 403.
	rec = httptest.NewRecorder()
	ok.ServeHTTP(rec, newPost(good, false))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without session should be 403: got %d", rec.Code)
	}

	// POST with a wrong token → 403.
	rec = httptest.NewRecorder()
	ok.ServeHTTP(rec, newPost("forged", true))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST with forged token should be 403: got %d", rec.Code)
	}

	// POST with the matching token → passes.
	rec = httptest.NewRecorder()
	ok.ServeHTTP(rec, newPost(good, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with valid token should pass: got %d", rec.Code)
	}

	// The X-CSRF-Token header is accepted too (for non-form clients).
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/agents", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
	r.Header.Set("X-CSRF-Token", good)
	ok.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST with valid X-CSRF-Token header should pass: got %d", rec.Code)
	}
}
