package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// fakeGitHub serves the token endpoint and the two API calls Finish makes
// (GET /user, GET /user/emails). tokenSeen records the bearer token presented
// to the API, so a test can assert containment: the token never reaches a
// session row, cookie, or store (SPEC-0021 REQ "Token Containment").
type fakeGitHub struct {
	apiSrv    *httptest.Server
	tokenSrv  *httptest.Server
	profile   map[string]any
	emails    []map[string]any
	emailCode int
	tokenSeen string
	exchanges int
}

func newFakeGitHub(t *testing.T, profile map[string]any, emails []map[string]any) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{profile: profile, emails: emails}

	gh.apiSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.tokenSeen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(gh.profile)
		case "/user/emails":
			if gh.emailCode != 0 {
				w.WriteHeader(gh.emailCode)
				return
			}
			_ = json.NewEncoder(w).Encode(gh.emails)
		default:
			http.NotFound(w, r)
		}
	}))
	gh.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.exchanges++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_test_token", "token_type": "bearer"})
	}))
	t.Cleanup(func() {
		gh.apiSrv.Close()
		gh.tokenSrv.Close()
	})
	t.Cleanup(func() {})

	return gh
}

// newGitHubTestAuth builds an Authenticator whose registry carries only the
// GitHub provider wired at the fake's endpoints, plus the pocket-id entry
// (unconfigured: no OIDC) so the default-provider resolution behaves.
func newGitHubTestAuth(t *testing.T, gh *fakeGitHub) (*Authenticator, *fakeStore) {
	t.Helper()
	fs := newFakeStore()
	g := &githubProvider{
		log:  discardLog(),
		http: gh.apiSrv.Client(),
		oauth: oauth2.Config{
			ClientID:     "cid",
			ClientSecret: "secret",
			RedirectURL:  "http://127.0.0.1:8080/auth/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  gh.tokenSrv.URL + "/authorize",
				TokenURL: gh.tokenSrv.URL + "/token",
			},
			Scopes: []string{"read:user", "user:email"},
		},
	}
	oldAPI := githubAPI
	githubAPI = gh.apiSrv.URL
	t.Cleanup(func() { githubAPI = oldAPI })

	a := &Authenticator{store: fs, log: discardLog(), secure: false, providers: map[string]AuthProvider{}}
	a.providers[PocketIDProviderID] = &pocketIDProvider{a: a}
	a.providers[GitHubProviderID] = g
	return a, fs
}

// doGitHubLogin drives Login(?provider=github) and returns the state cookie and
// the authorize URL.
func doGitHubLogin(t *testing.T, a *Authenticator) (*http.Cookie, *url.URL) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?provider=github", nil))
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: want 302, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("login redirect: %v", err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == stateCookie {
			return c, loc
		}
	}
	t.Fatal("login must set the state cookie")
	return nil, nil
}

// SPEC-0021 REQ "GitHub OAuth Callback Exchange", scenario "Successful GitHub
// login": a verified primary email yields a session for the same Human
// principal, with iss=github.com and the provider subject recorded.
func TestGitHubLoginHappyPath(t *testing.T) {
	gh := newFakeGitHub(t,
		map[string]any{"login": "octocat", "id": 583231, "name": "The Octocat"},
		[]map[string]any{
			{"email": "secondary@example.com", "primary": false, "verified": true},
			{"email": "octocat@example.com", "primary": true, "verified": true},
		})
	a, fs := newGitHubTestAuth(t, gh)

	c, loc := doGitHubLogin(t, a)
	code := "test-code"

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?provider=github&code="+code+"&state="+loc.Query().Get("state"), nil)
	r.AddCookie(c)
	a.Callback(rec, r)
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d: %s", resp.StatusCode, rec.Body.String())
	}
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("callback must land on the board, got %q", resp.Header.Get("Location"))
	}

	if len(fs.humans) != 1 {
		t.Fatalf("want one human upserted, got %v", fs.humans)
	}
	// Human keyed on the provider-namespaced subject so a Pocket ID sub can
	// never collide with a GitHub account id.
	hu, ok := fs.humans["github.com|583231"]
	if !ok {
		t.Fatalf("human must be keyed on the namespaced subject, got %v", fs.humans)
	}
	if hu.Email != "octocat@example.com" || hu.DisplayName != "The Octocat" {
		t.Fatalf("human profile: got %+v", hu)
	}
	// Session provenance: issuer + provider sub recorded (session parity).
	for _, s := range fs.sessions {
		if s.issuer != "https://github.com/" || s.sub != "583231" {
			t.Fatalf("session provenance: got issuer=%q sub=%q, want github issuer and 583231", s.issuer, s.sub)
		}
	}

	// Only one token exchange happened, and the token was presented to the API
	// during Finish — never stored.
	if gh.exchanges != 1 {
		t.Fatalf("want exactly one token exchange, got %d", gh.exchanges)
	}
	if gh.tokenSeen == "" {
		t.Fatal("the profile fetch must have presented the exchanged token")
	}
	for hash := range fs.sessions {
		if strings.Contains(hash, "gho_") {
			t.Fatal("the GitHub access token must never reach the session store")
		}
	}
}

// SPEC-0021 scenario "Unverified or missing primary email": login is rejected,
// no session, no human upsert — and the failure is not a 5xx.
func TestGitHubLoginUnverifiedEmailRejected(t *testing.T) {
	gh := newFakeGitHub(t,
		map[string]any{"login": "octocat", "id": 583231, "name": "The Octocat"},
		[]map[string]any{
			{"email": "octocat@example.com", "primary": true, "verified": false},
			{"email": "secondary@example.com", "primary": false, "verified": true},
		})
	a, fs := newGitHubTestAuth(t, gh)

	c, loc := doGitHubLogin(t, a)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?provider=github&code=x&state="+loc.Query().Get("state"), nil)
	r.AddCookie(c)
	a.Callback(rec, r)
	if rec.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for an unverified primary email, got %d", rec.Result().StatusCode)
	}
	if len(fs.humans) != 0 || len(fs.sessions) != 0 {
		t.Fatal("a rejected login must not upsert a human or mint a session")
	}
}

// The state cookie records which provider began the flow; a callback whose
// query claims a different provider is rejected before any token exchange
// (SPEC-0021: state validated before exchange).
func TestGitHubCallbackProviderMismatchRejected(t *testing.T) {
	gh := newFakeGitHub(t,
		map[string]any{"login": "octocat", "id": 583231},
		[]map[string]any{{"email": "octocat@example.com", "primary": true, "verified": true}})
	a, fs := newGitHubTestAuth(t, gh)

	c, loc := doGitHubLogin(t, a)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?provider=pocket-id&code=x&state="+loc.Query().Get("state"), nil)
	r.AddCookie(c)
	a.Callback(rec, r)
	if rec.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 on provider mismatch, got %d", rec.Result().StatusCode)
	}
	if gh.exchanges != 0 {
		t.Fatal("provider mismatch must abort before the token exchange")
	}
	if len(fs.sessions) != 0 {
		t.Fatal("provider mismatch must not mint a session")
	}
}

// SPEC-0021 REQ "Provider Selection on the Login Page", scenario "GitHub
// provider not configured": an unknown provider is a plain 404.
func TestUnknownProviderLoginIs404(t *testing.T) {
	a, _ := newGitHubTestAuth(t, newFakeGitHub(t, nil, nil))
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?provider=gitlab", nil))
	if rec.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown provider, got %d", rec.Result().StatusCode)
	}
}

// An unconfigured GitHub provider (no credentials) 404s its login route — it
// must not fall through to the default provider.
func TestUnconfiguredGitHubLoginIs404(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?provider=github", nil))
	if rec.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for an unconfigured GitHub provider, got %d", rec.Result().StatusCode)
	}
}

// The default (no provider parameter) stays Pocket ID: Begin must build the
// OIDC authorize URL, not the GitHub one.
func TestDefaultProviderIsPocketID(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, idp.srv.URL) {
		t.Fatalf("default login must go to the OIDC issuer, got %q", loc)
	}
	for _, c := range resp.Cookies() {
		if c.Name == stateCookie {
			if st := decodeStateCookie(t, c); st.ProviderName() != PocketIDProviderID {
				t.Fatalf("default state cookie must record pocket-id, got %q", st.ProviderName())
			}
			return
		}
	}
	t.Fatal("state cookie missing")
}
