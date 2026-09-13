// Server-side session & session-gated surface tests: opaque token minting (hash-only storage),
// expiry enforcement, the RequireHuman gate (missing/garbage/expired cookies redirect to login and
// dead cookies are cleared), server-side logout revocation, and session-fixation resistance (login
// always mints a fresh token, never adopting a presented cookie value).
// Governing: SPEC-0008 REQ "Server-Side Session Establishment", REQ "Session-Gated Human Surface".
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/store"
)

// newSessionAuth builds an Authenticator with no OIDC provider — session machinery only.
func newSessionAuth(fs *fakeStore, baseURL string) *Authenticator {
	return &Authenticator{
		cfg:    config.Config{BaseURL: baseURL},
		store:  fs,
		log:    discardLog(),
		secure: baseURL[:8] == "https://",
	}
}

// establish mints a session for a test human and returns the plaintext cookie token.
func establish(t *testing.T, a *Authenticator) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := a.establishSession(context.Background(), rec, identity{Issuer: "https://id.example", Subject: testSubject, HumanSubject: testSubject, Name: testName, Email: testEmail}); err != nil {
		t.Fatalf("establishSession: %v", err)
	}
	c := cookieByName(t, rec.Result(), sessionCookie)
	if c == nil || c.Value == "" {
		t.Fatal("establishSession must set a session cookie")
	}
	return c.Value
}

// gated wraps a probe handler in RequireHuman and reports whether it ran and with which human.
func gated(a *Authenticator) (http.Handler, *store.Human, *bool) {
	var human store.Human
	var called bool
	h := a.RequireHuman(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		human, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	return h, &human, &called
}

func requestWithSession(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	return r
}

// requireRedirectToLogin asserts the RequireHuman rejection contract: 302 to /login, protected
// handler never ran. SPEC-0008 scenario "Unauthenticated access is redirected".
func requireRedirectToLogin(t *testing.T, rec *httptest.ResponseRecorder, called bool) {
	t.Helper()
	if called {
		t.Fatal("protected handler must not run for an unauthenticated request")
	}
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("must redirect to /login, got %q", loc)
	}
}

func requireClearedSessionCookie(t *testing.T, resp *http.Response) {
	t.Helper()
	c := cookieByName(t, resp, sessionCookie)
	if c == nil || c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("dead session cookie must be cleared: %+v", c)
	}
}

// --- session establishment ------------------------------------------------------------------------

// SPEC-0008 scenario "Session cookie carries only an opaque token": the cookie holds a high-entropy
// opaque token; the store holds only its SHA-256 hash with an expiry; cookie attributes are
// HttpOnly + SameSite=Lax (Secure under HTTPS is proven in TestCookiesSecureUnderHTTPSBaseURL).
func TestSessionStoresOnlyTokenHash(t *testing.T) {
	fs := newFakeStore()
	a := newSessionAuth(fs, "http://127.0.0.1:8080")

	rec := httptest.NewRecorder()
	if err := a.establishSession(context.Background(), rec, identity{Issuer: "https://id.example", Subject: testSubject, HumanSubject: testSubject, Name: testName, Email: testEmail}); err != nil {
		t.Fatalf("establishSession: %v", err)
	}
	c := cookieByName(t, rec.Result(), sessionCookie)
	if c == nil || c.Value == "" {
		t.Fatal("session cookie must be set")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie must be HttpOnly + SameSite=Lax: %+v", c)
	}
	if c.MaxAge <= 0 {
		t.Fatalf("session cookie must have a bounded lifetime, MaxAge=%d", c.MaxAge)
	}

	// Opaque high-entropy token: 32 random bytes, base64url without padding.
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token must be 32 random bytes base64url-encoded: len=%d err=%v", len(raw), err)
	}

	// The store holds only the SHA-256 hash, never the plaintext.
	if _, ok := fs.sessions[hashToken(c.Value)]; !ok {
		t.Fatal("store must hold the SHA-256 hash of the session token")
	}
	if _, ok := fs.sessions[c.Value]; ok {
		t.Fatal("store must never hold the plaintext session token")
	}

	// The stored session carries a bounded expiry matching the session TTL.
	s := fs.sessions[hashToken(c.Value)]
	if until := time.Until(s.expiresAt); until <= 0 || until > sessionTTL {
		t.Fatalf("stored expiry must be bounded by the session TTL, got %v", until)
	}
}

// --- session-gated surface (RequireHuman) ---------------------------------------------------------

func TestRequireHumanMissingCookieRedirects(t *testing.T) {
	a := newSessionAuth(newFakeStore(), "http://127.0.0.1:8080")
	h, _, called := gated(a)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession(""))
	requireRedirectToLogin(t, rec, *called)
	// No cookie was presented, so there is nothing to clear.
	if c := cookieByName(t, rec.Result(), sessionCookie); c != nil {
		t.Fatalf("no session cookie should be set when none was presented: %+v", c)
	}
}

func TestRequireHumanGarbageCookieRedirectsAndClears(t *testing.T) {
	a := newSessionAuth(newFakeStore(), "http://127.0.0.1:8080")
	h, _, called := gated(a)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession("not-a-real-token"))
	requireRedirectToLogin(t, rec, *called)
	requireClearedSessionCookie(t, rec.Result())
}

// SPEC-0008 scenario "Expired session is not honored": a cookie whose stored session has passed its
// expiry is treated as unauthenticated — redirect to login and the dead cookie is cleared.
func TestRequireHumanExpiredSessionRedirectsAndClears(t *testing.T) {
	fs := newFakeStore()
	a := newSessionAuth(fs, "http://127.0.0.1:8080")

	// Mint a session that is already past its expiry.
	hu, err := fs.UpsertHuman(context.Background(), testSubject, testName, testEmail)
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	tok, err := randToken()
	if err != nil {
		t.Fatalf("randToken: %v", err)
	}
	if err := fs.CreateSession(context.Background(), hashToken(tok), hu.ID, -time.Second, "https://id.example", testSubject); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h, _, called := gated(a)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession(tok))
	requireRedirectToLogin(t, rec, *called)
	requireClearedSessionCookie(t, rec.Result())
}

// A transient store failure (e.g. a DB blip) must NOT be treated as a dead session: RequireHuman
// still denies the request (fail closed) and redirects to login, but it must NOT clear the session
// cookie — otherwise a momentary database hiccup would silently log every operator out. Only a
// provably dead cookie (store.ErrNotFound) is cleared. Governing: SPEC-0008 REQ "Session-Gated
// Human Surface" (carry-over fix, issue #55).
func TestRequireHumanTransientStoreErrorPreservesCookie(t *testing.T) {
	fs := newFakeStore()
	fs.sessionErr = errors.New("db connection reset by peer")
	a := newSessionAuth(fs, "http://127.0.0.1:8080")

	h, _, called := gated(a)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession("a-valid-looking-token"))

	// Access is denied for this request (fail closed).
	requireRedirectToLogin(t, rec, *called)

	// But the session cookie is left intact — no Set-Cookie clearing it on a transient failure.
	if c := cookieByName(t, rec.Result(), sessionCookie); c != nil {
		t.Fatalf("a transient store error must NOT clear the session cookie, got: %+v", c)
	}
}

// SPEC-0008 REQ "Session-Gated Human Surface": a live session admits the request and the
// authenticated human (plus the per-session CSRF token) is carried in request context.
func TestRequireHumanLiveSessionAdmitsAndCarriesHuman(t *testing.T) {
	fs := newFakeStore()
	a := newSessionAuth(fs, "http://127.0.0.1:8080")
	tok := establish(t, a)

	var csrf string
	h := a.RequireHuman(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hu, ok := FromContext(r.Context())
		if !ok || hu.OIDCSubject != testSubject {
			t.Fatalf("authenticated human must be in context: %+v ok=%v", hu, ok)
		}
		csrf = CSRFFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithSession(tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("live session must admit: got %d", rec.Code)
	}
	if csrf != csrfToken(tok) {
		t.Fatal("per-session CSRF token must be derivable from the session and present in context")
	}
}

// --- CSRF over the session-gated group -----------------------------------------------------------

// The human web routes are wired RequireHuman → RequireCSRF (see newRouter). This proves that
// composition end-to-end for a state-changing POST like logout: a live session alone is not enough —
// a matching per-session synchronizer token is also required, and an anonymous caller never even
// reaches the CSRF check (RequireHuman redirects first). Governing: SPEC-0008 "Security Requirements
// → CSRF Protection"; REQ "Session-Gated Human Surface".
func TestSessionGroupRequiresCSRFOnPOST(t *testing.T) {
	fs := newFakeStore()
	a := newSessionAuth(fs, "http://127.0.0.1:8080")
	tok := establish(t, a)

	var reached bool
	chain := a.RequireHuman(a.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})))

	post := func(csrf string, withSession bool) *httptest.ResponseRecorder {
		body := url.Values{}
		if csrf != "" {
			body.Set("csrf_token", csrf)
		}
		r := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withSession {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, r)
		return rec
	}

	// Anonymous: RequireHuman redirects to /login before CSRF is ever consulted.
	reached = false
	if rec := post(csrfToken(tok), false); rec.Code != http.StatusFound || rec.Result().Header.Get("Location") != "/login" {
		t.Fatalf("anonymous POST should redirect to /login, got %d", rec.Code)
	} else if reached {
		t.Fatal("anonymous POST must not reach the protected handler")
	}

	// Valid session, missing CSRF token → 403.
	reached = false
	if rec := post("", true); rec.Code != http.StatusForbidden {
		t.Fatalf("session POST without CSRF token should be 403, got %d", rec.Code)
	} else if reached {
		t.Fatal("CSRF-less POST must not reach the protected handler")
	}

	// Valid session, forged CSRF token → 403.
	if rec := post("forged-token", true); rec.Code != http.StatusForbidden {
		t.Fatalf("session POST with forged CSRF token should be 403, got %d", rec.Code)
	}

	// Valid session, correct per-session token → passes.
	reached = false
	if rec := post(csrfToken(tok), true); rec.Code != http.StatusOK || !reached {
		t.Fatalf("session POST with valid CSRF token should pass, got %d reached=%v", rec.Code, reached)
	}
}

// --- logout ---------------------------------------------------------------------------------------

// SPEC-0008 scenario "Logout revokes the session": logout deletes the server-side record and clears
// the cookie, so replaying the prior cookie no longer authenticates (revocation is server-side, not
// merely cookie deletion in the browser).
func TestLogoutRevokesServerSideSession(t *testing.T) {
	fs := newFakeStore()
	a := newSessionAuth(fs, "http://127.0.0.1:8080")
	tok := establish(t, a)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	a.Logout(rec, r)

	resp := rec.Result()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("logout must redirect to /: got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	requireClearedSessionCookie(t, resp)
	if _, ok := fs.sessions[hashToken(tok)]; ok {
		t.Fatal("logout must delete the server-side session record")
	}

	// An attacker who kept a copy of the cookie can no longer use it.
	h, _, called := gated(a)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, requestWithSession(tok))
	requireRedirectToLogin(t, rec2, *called)
}

// --- session fixation ------------------------------------------------------------------------------

// Session fixation must not be possible: login mints a brand-new token and never adopts a session
// cookie the browser already carried (e.g. one planted by an attacker). The full OIDC callback runs
// with an attacker-planted session cookie on the request; the minted session must differ and the
// planted value must never authenticate. Governing: SPEC-0008 REQ "Server-Side Session Establishment".
func TestLoginMintsFreshSessionTokenNoFixation(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	c, loc := doLogin(t, a)
	idp.setNonce(loc.Query().Get("nonce"))

	// Replay the IdP redirect with an attacker-planted session cookie alongside the state cookie.
	const planted = "attacker-planted-token"
	q := loc.Query()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+q.Get("state")+"&code=test-code", nil)
	r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: planted})
	rec := httptest.NewRecorder()
	a.Callback(rec, r)
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d", resp.StatusCode)
	}

	sc := cookieByName(t, resp, sessionCookie)
	if sc == nil || sc.Value == "" {
		t.Fatal("login must set a session cookie")
	}
	if sc.Value == planted {
		t.Fatal("login must mint a fresh session token, never adopting the presented cookie value")
	}
	if _, ok := fs.sessions[hashToken(planted)]; ok {
		t.Fatal("the planted token must never become a valid session")
	}

	// A second login mints yet another distinct token — session ids are never reused.
	tok2 := establish(t, &Authenticator{cfg: a.cfg, store: fs, log: discardLog()})
	if tok2 == sc.Value {
		t.Fatal("each login must mint a distinct session token")
	}
	if len(fs.sessions) != 2 {
		t.Fatalf("want 2 distinct sessions, got %d", len(fs.sessions))
	}

	// The planted token still does not authenticate after the victim logged in.
	h, _, called := gated(a)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, requestWithSession(planted))
	requireRedirectToLogin(t, rec2, *called)
}
