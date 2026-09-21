package main

// The OAuth login and the credential lifecycle it feeds, end to end against the fake deployment:
// discovery → registration → authorize (browser) → loopback callback → PKCE exchange → saved
// credentials; denial; a stray callback that must not abort the login; and the API client's
// proactive refresh, 401-retry, and refusal once the grant is gone.

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// browserThatApproves is the test's browser: it fetches the authorize URL and follows the
// redirect to the CLI's loopback listener, the way a real browser lands the callback.
func browserThatApproves(t *testing.T, seen chan<- *url.URL) func(string) error {
	return func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			t.Errorf("authorize URL does not parse: %v", err)
			return err
		}
		seen <- u
		go func() {
			resp, err := http.Get(raw) //nolint:noctx // test browser
			if err != nil {
				t.Errorf("browser GET: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
		return nil
	}
}

func TestLoginFlowEndToEnd(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	seen := make(chan *url.URL, 1)
	tc.openBrowser = browserThatApproves(t, seen)

	if code := tc.run(t, "login", f.base()+"/"); code != exitOK {
		t.Fatalf("login: code %d\nstdout: %s\nstderr: %s", code, tc.stdout.String(), tc.stderr.String())
	}
	mustContain(t, "login output", tc.stdout.String(), "Opening your browser", "Logged in to "+f.base(), "Credentials saved to "+tc.env["SWITCHBOARD_CREDENTIALS"])

	// The wire shape: an operator grant (resource = <base>/api), S256 PKCE, exact loopback redirect.
	authz := <-seen
	q := authz.Query()
	if q.Get("resource") != f.base()+"/api" {
		t.Errorf("resource = %q, want the operator resource %q", q.Get("resource"), f.base()+"/api")
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 16 {
		t.Errorf("authorize query = %v", q)
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") || !strings.HasSuffix(q.Get("redirect_uri"), "/callback") {
		t.Errorf("redirect_uri = %q, want a loopback callback", q.Get("redirect_uri"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.registered) != 1 || f.registered[0]["token_endpoint_auth_method"] != "none" {
		t.Errorf("registration = %v, want a public client", f.registered)
	}
	if uris, _ := f.registered[0]["redirect_uris"].([]any); len(uris) != 1 || uris[0] != q.Get("redirect_uri") {
		t.Errorf("registered redirect_uris = %v, want exactly %q", f.registered[0]["redirect_uris"], q.Get("redirect_uri"))
	}
	if len(f.tokenCalls) != 1 || f.tokenCalls[0].Get("grant_type") != "authorization_code" {
		t.Fatalf("token calls = %v", f.tokenCalls)
	}
	sum := sha256.Sum256([]byte(f.tokenCalls[0].Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Error("the exchanged verifier does not hash to the authorize challenge")
	}
	if f.tokenCalls[0].Get("redirect_uri") != q.Get("redirect_uri") {
		t.Error("exchange did not echo the redirect_uri the code was issued for")
	}

	// What login persisted: the pair, the client, the normalized base URL, an expiry from expires_in.
	creds, err := tc.loadCredentials()
	if err != nil {
		t.Fatalf("credentials after login: %v", err)
	}
	if creds.BaseURL != f.base() || creds.ClientID != "cid-test" || creds.AccessToken != "at-1" || creds.RefreshToken != "rt-1" {
		t.Fatalf("credentials = %+v", creds)
	}
	if want := tc.clock.Add(time.Hour); !creds.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", creds.ExpiresAt, want)
	}
	if info, err := os.Stat(tc.env["SWITCHBOARD_CREDENTIALS"]); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file: %v %v", info, err)
	}
}

func TestLoginNoBrowserPrintsTheURL(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.openBrowser = func(string) error { t.Error("--no-browser must not open a browser"); return nil }
	tc.env["SWITCHBOARD_URL"] = f.base()

	done := make(chan int, 1)
	go func() { done <- tc.run(t, "login", "--no-browser") }()

	// The URL is printed for the human; this test plays the human and opens it.
	var authz string
	deadline := time.Now().Add(5 * time.Second)
	for authz == "" && time.Now().Before(deadline) {
		for _, line := range strings.Split(tc.stdout.String(), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), f.base()+"/oauth/authorize?") {
				authz = strings.TrimSpace(line)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if authz == "" {
		t.Fatalf("login URL never printed: %q", tc.stdout.String())
	}
	resp, err := http.Get(authz) //nolint:noctx // test browser
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if code := <-done; code != exitOK {
		t.Fatalf("login --no-browser: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "no-browser output", tc.stdout.String(), "Open this URL in your browser", "Logged in to "+f.base())
}

func TestLoginDeniedAndUsage(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	f.deny = true
	seen := make(chan *url.URL, 1)
	tc.openBrowser = browserThatApproves(t, seen)
	if code := tc.run(t, "login", f.base()); code != exitFailure {
		t.Fatalf("denied login: code %d, want 1 (stdout %q)", code, tc.stdout.String())
	}
	mustContain(t, "denial", tc.stderr.String(), "login denied: access_denied (the operator denied the request)")
	if _, err := tc.loadCredentials(); !errors.Is(err, errNotLoggedIn) {
		t.Fatalf("a denied login must persist nothing: %v", err)
	}

	// No URL anywhere → usage, not a network call.
	if code := tc.run(t, "login"); code != exitUsage {
		t.Fatalf("login without a URL: code %d, want 2", code)
	}
	mustContain(t, "login usage", tc.stderr.String(), "give the deployment URL", "SWITCHBOARD_URL")
	if code := tc.run(t, "login", "ftp://nope"); code != exitUsage {
		t.Fatalf("login with a non-http URL: code %d, want 2", code)
	}
	if code := tc.run(t, "login", f.base(), "extra"); code != exitUsage {
		t.Fatalf("login with two URLs: code %d, want 2", code)
	}
	if code := tc.run(t, "login", "http://127.0.0.1:1"); code != exitFailure {
		t.Fatalf("login against nothing: code %d, want 1", code)
	}
	mustContain(t, "unreachable deployment", tc.stderr.String(), "discovering http://127.0.0.1:1")
}

func TestLoginReusesTheSavedDeployment(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(-time.Hour)) // an old login to the same deployment
	seen := make(chan *url.URL, 1)
	tc.openBrowser = browserThatApproves(t, seen)
	if code := tc.run(t, "login"); code != exitOK {
		t.Fatalf("re-login without a URL: code %d, stderr %q", code, tc.stderr.String())
	}
	if (<-seen).Host != strings.TrimPrefix(f.base(), "http://") {
		t.Fatal("re-login did not target the saved deployment")
	}
}

func TestLoginIgnoresStrayCallbacks(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.openBrowser = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		cb := u.Query().Get("redirect_uri")
		go func() {
			// A forged hit first: wrong state, a code of the attacker's choosing.
			for _, stray := range []string{cb + "?state=forged&code=evil", cb + "?state=&error=access_denied", cb} {
				resp, err := http.Get(stray) //nolint:noctx // test browser
				if err != nil {
					t.Errorf("stray GET: %v", err)
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("stray callback %q answered %d, want 400", stray, resp.StatusCode)
				}
			}
			// Then the real one.
			resp, err := http.Get(raw) //nolint:noctx // test browser
			if err != nil {
				t.Errorf("browser GET: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
		return nil
	}
	if code := tc.run(t, "login", f.base()); code != exitOK {
		t.Fatalf("login: code %d, stderr %q", code, tc.stderr.String())
	}
	creds, err := tc.loadCredentials()
	if err != nil || creds.AccessToken != "at-1" {
		t.Fatalf("credentials after login = %+v, %v", creds, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenCalls) != 1 || f.tokenCalls[0].Get("code") == "evil" {
		t.Fatalf("token calls = %v: the forged code must never be exchanged", f.tokenCalls)
	}
}

func TestLoginTimesOut(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loginTimeout = 50 * time.Millisecond
	tc.openBrowser = func(string) error { return nil } // "opened", but the human never comes back
	if code := tc.run(t, "login", f.base()); code != exitFailure {
		t.Fatalf("code %d, want 1", code)
	}
	mustContain(t, "timeout", tc.stderr.String(), "timed out waiting for the browser")
}

// --- the API client keeps the pair alive ---

func TestAPIClientRefreshesAndRetries(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)

	// 1. About to expire (inside the leeway) → refreshed BEFORE the call, no 401 spent.
	tc.loggedIn(t, f, tc.clock.Add(10*time.Second))
	if code := tc.run(t, "agents"); code != exitOK {
		t.Fatalf("agents (near expiry): code %d, stderr %q", code, tc.stderr.String())
	}
	f.mu.Lock()
	if len(f.tokenCalls) != 1 || f.tokenCalls[0].Get("grant_type") != "refresh_token" || f.tokenCalls[0].Get("refresh_token") != "rt-1" {
		t.Fatalf("token calls = %v, want one proactive refresh of rt-1", f.tokenCalls)
	}
	f.mu.Unlock()
	creds, _ := tc.loadCredentials()
	if creds.AccessToken != "at-2" || creds.RefreshToken != "rt-2" || !creds.ExpiresAt.Equal(tc.clock.Add(time.Hour)) {
		t.Fatalf("credentials after proactive refresh = %+v", creds)
	}

	// 2. The stored token still looks live but the server rejects it (revoked) → 401 → refresh → retry.
	f.mu.Lock()
	f.revoked["at-2"] = true
	f.mu.Unlock()
	if code := tc.run(t, "agents"); code != exitOK {
		t.Fatalf("agents (revoked access): code %d, stderr %q", code, tc.stderr.String())
	}
	creds, _ = tc.loadCredentials()
	if creds.AccessToken != "at-3" {
		t.Fatalf("credentials after 401 retry = %+v", creds)
	}

	// 3. The grant is gone: refresh refused → a clear instruction, no infinite retry.
	f.mu.Lock()
	f.revoked["at-3"] = true
	f.refresh = "rotated-elsewhere"
	f.mu.Unlock()
	if code := tc.run(t, "agents"); code != exitFailure {
		t.Fatalf("agents (dead grant): code %d, want 1", code)
	}
	mustContain(t, "dead grant", tc.stderr.String(), "refreshing credentials", "invalid_grant", "run `switchboard login` again")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenCalls) != 3 {
		t.Fatalf("token calls = %d, want exactly 3 (one per refresh attempt)", len(f.tokenCalls))
	}
}

func TestAPIClientWithoutRefreshTokenAsksForLogin(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(-time.Minute))
	creds, _ := tc.loadCredentials()
	creds.RefreshToken = ""
	if err := tc.saveCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if code := tc.run(t, "endpoints"); code != exitFailure {
		t.Fatalf("code %d, want 1", code)
	}
	mustContain(t, "no refresh token", tc.stderr.String(), "no refresh token is stored", "switchboard login")
}
