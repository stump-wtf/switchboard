package auth

// The sign-in round trip must not drop the request that started it: an anonymous hit on a
// protected URL — a pending /oauth/authorize, the URL `switchboard login` prints — bounces
// through /login with a validated return path, and every session-establishing redirect honors
// it. The path is same-origin-only at every hop (SafeReturnPath), so a crafted return_to can
// never become an open redirect. Governing: SPEC-0008 REQ "Server-Side Session Establishment",
// SPEC-0016 scenario "Human absent".

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSafeReturnPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/", "/"},
		{"/oauth/authorize", "/oauth/authorize"},
		{"/oauth/authorize?client_id=cid&code_challenge=c", "/oauth/authorize?client_id=cid&code_challenge=c"},
		{"/todos?id=7", "/todos?id=7"},
		{"https://x@evil.example", ""},               // absolute URL with user info
		{"//user@evil.example", ""},                  // scheme-relative with user info
		{"/user@evil.example", "/user@evil.example"}, // rooted relative path stays same-origin
		{"relative/path", ""},                        // not rooted
		{" /x", ""},                                  // whitespace prefix
		{"/x\x00", ""},                               // control character
		{"/x\nSet-Cookie: a=b", ""},                  // header smuggling via newline
		{"/" + strings.Repeat("a", 3000), ""},        // absurd length
	}
	for _, tc := range cases {
		if got := SafeReturnPath(tc.in); got != tc.want {
			t.Errorf("SafeReturnPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// doLoginReturnTo starts a login with ?return_to= and returns the state cookie and IdP URL.
func doLoginReturnTo(t *testing.T, a *Authenticator, returnTo string) (*http.Cookie, *url.URL) {
	t.Helper()
	target := "/auth/login"
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, target, nil))
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: want 302, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("login: parse redirect: %v", err)
	}
	c := cookieByName(t, resp, stateCookie)
	if c == nil {
		t.Fatal("login: state cookie not set")
	}
	return c, loc
}

// SPEC-0016 scenario "Human absent": the RequireHuman bounce to /login carries the original
// request URI as return_to, so the login page and the flow behind it can send the human back.
func TestRequireHumanBounceCarriesReturnPath(t *testing.T) {
	a := newDevAuth(newFakeStore(), false, nil)

	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?client_id=cid&code_challenge=c&code_challenge_method=S256&redirect_uri=cb&state=s", nil)
	rec := httptest.NewRecorder()
	a.RequireHuman(http.NotFoundHandler()).ServeHTTP(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", resp.StatusCode)
	}
	want := "/login?return_to=" + url.QueryEscape("/oauth/authorize?client_id=cid&code_challenge=c&code_challenge_method=S256&redirect_uri=cb&state=s")
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("bounce = %q, want %q", got, want)
	}
}

// The validated return path rides the state cookie through the IdP redirect.
func TestLoginStateCarriesReturnPath(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())

	c, _ := doLoginReturnTo(t, a, "/oauth/authorize?client_id=cid")

	if st := decodeStateCookie(t, c); st.ReturnTo != "/oauth/authorize?client_id=cid" {
		t.Fatalf("state ReturnTo = %q, want the authorize URI", st.ReturnTo)
	}
}

// The full anonymous round trip lands back on the protected URL, query intact.
func TestCallbackRedirectsToReturnPath(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	c, loc := doLoginReturnTo(t, a, "/oauth/authorize?client_id=cid")
	idp.setNonce(loc.Query().Get("nonce"))
	resp := doCallback(t, a, loc.Query().Get("state"), c)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/oauth/authorize?client_id=cid" {
		t.Fatalf("post-login redirect = %q, want the returned authorize URI", got)
	}
}

// An unsafe return path is refused at every hop: the state never carries it and the callback
// falls back to the Board — the flow degrades to today's behavior, never to an open redirect.
func TestCallbackRejectsUnsafeReturnPath(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	for _, unsafe := range []string{
		"https://evil.example/x",
		"//evil.example/x",
		"/\\evil.example",
		"https://trusted.example\tevil.example",
	} {
		c, loc := doLoginReturnTo(t, a, unsafe)
		if st := decodeStateCookie(t, c); st.ReturnTo != "" {
			t.Fatalf("unsafe return_to %q must not enter the state: %q", unsafe, st.ReturnTo)
		}
		idp.setNonce(loc.Query().Get("nonce"))
		resp := doCallback(t, a, loc.Query().Get("state"), c)
		if got := resp.Header.Get("Location"); got != "/" {
			t.Fatalf("unsafe return_to %q: post-login redirect = %q, want /", unsafe, got)
		}
	}
}

// Dev login is a session-establishment path too: its form carries the same validated return path.
func TestDevLoginHonorsReturnPath(t *testing.T) {
	fs := newFakeStore()
	a := newDevAuth(fs, true, nil)

	form := url.Values{"return_to": {"/oauth/authorize?client_id=cid"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/dev-login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.DevLogin(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("dev-login: want 302, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/oauth/authorize?client_id=cid" {
		t.Fatalf("dev-login redirect = %q, want the returned authorize URI", got)
	}
}

// An unsafe dev-login return path falls back to the Board like every other path.
func TestDevLoginRejectsUnsafeReturnPath(t *testing.T) {
	fs := newFakeStore()
	a := newDevAuth(fs, true, nil)

	form := url.Values{"return_to": {"https://evil.example/x"}}
	req := httptest.NewRequest(http.MethodPost, "/auth/dev-login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.DevLogin(rec, req)
	resp := rec.Result()

	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("dev-login with unsafe return_to: got %d %q, want 302 /", resp.StatusCode, resp.Header.Get("Location"))
	}
}
