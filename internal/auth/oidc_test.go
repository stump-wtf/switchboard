// OIDC relying-party flow tests: login initiation (state + nonce + PKCE stashed in a short-lived
// cookie, S256 challenge in the redirect) and callback verification (state match, PKCE code
// exchange, ID-token signature + nonce verification, human upsert, session establishment).
// A fake IdP (discovery + JWKS + token endpoint, RS256-signed ID tokens) stands in for Pocket ID so
// every scenario runs hermetically. Governing: SPEC-0008 REQ "OIDC Relying-Party Login",
// REQ "Callback Verification", REQ "Human Upsert Keyed on OIDC Subject".
package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/store"
)

// --- fake IdP -----------------------------------------------------------------------------------

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint that mints RS256-signed
// ID tokens. Tests steer it via nonce (the nonce echoed into the ID token) and badSignature (sign
// with a key absent from the JWKS).
type fakeIdP struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	wrongKey *rsa.PrivateKey

	mu           sync.Mutex
	nonce        string         // echoed into the minted ID token's nonce claim
	badSignature bool           // sign the ID token with a key that is not in the JWKS
	lastVerifier string         // code_verifier presented at the token endpoint ("" = no exchange yet)
	extraClaims  map[string]any // merged into the minted ID token (e.g. amr/acr for assurance tests)
}

const (
	testClientID = "test-client"
	testSubject  = "pocket|test-sub"
	testName     = "Test Human"
	testEmail    = "human@example.com"
)

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate wrong RSA key: %v", err)
	}
	f := &fakeIdP{key: key, wrongKey: wrongKey}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
				"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.lastVerifier = r.PostFormValue("code_verifier")
		nonce := f.nonce
		signKey := f.key
		if f.badSignature {
			signKey = f.wrongKey
		}
		extra := f.extraClaims
		f.mu.Unlock()
		now := time.Now()
		claims := map[string]any{
			"iss":   f.srv.URL,
			"sub":   testSubject,
			"aud":   testClientID,
			"exp":   now.Add(time.Hour).Unix(),
			"iat":   now.Unix(),
			"nonce": nonce,
			"name":  testName,
			"email": testEmail,
		}
		for k, v := range extra {
			claims[k] = v
		}
		idToken := signJWT(t, signKey, claims)
		writeJSON(w, map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) setNonce(n string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce = n
}

func (f *fakeIdP) setExtraClaims(c map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extraClaims = c
}

func (f *fakeIdP) verifierSeen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastVerifier
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// signJWT mints a compact RS256 JWT (header.payload.signature) for the fake IdP.
func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// --- fake session store -------------------------------------------------------------------------

// fakeStore is an in-memory sessionStore: upsert keyed on OIDC subject, sessions keyed on token hash.
// Sessions carry an expiry so tests can prove expired sessions are not honored, mirroring the real
// store's `expires_at > now()` admission check.
type fakeStore struct {
	mu       sync.Mutex
	humans   map[string]store.Human // by OIDC subject
	sessions map[string]fakeSession // token hash -> session
	// sessionErr, when non-nil, is returned by SessionHuman ahead of any lookup — it models a
	// transient store failure (e.g. a DB blip) that is NOT store.ErrNotFound.
	sessionErr error
}

type fakeSession struct {
	humanID   string
	expiresAt time.Time
	issuer    string
	sub       string
}

func newFakeStore() *fakeStore {
	return &fakeStore{humans: map[string]store.Human{}, sessions: map[string]fakeSession{}}
}

func (f *fakeStore) UpsertHuman(_ context.Context, subject, displayName, email string) (store.Human, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.humans[subject]
	if !ok {
		h = store.Human{ID: fmt.Sprintf("human-%d", len(f.humans)+1), OIDCSubject: subject, CreatedAt: time.Now()}
	}
	if displayName != "" {
		h.DisplayName = displayName
	}
	if email != "" {
		h.Email = email
	}
	f.humans[subject] = h
	return h, nil
}

func (f *fakeStore) CreateSession(_ context.Context, tokenHash, humanID string, ttl time.Duration, issuer, providerSub string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[tokenHash] = fakeSession{humanID: humanID, expiresAt: time.Now().Add(ttl), issuer: issuer, sub: providerSub}
	return nil
}

func (f *fakeStore) SessionHuman(_ context.Context, tokenHash string) (store.Human, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionErr != nil {
		return store.Human{}, f.sessionErr
	}
	s, ok := f.sessions[tokenHash]
	if !ok || !time.Now().Before(s.expiresAt) { // only live (unexpired) sessions admit access
		return store.Human{}, store.ErrNotFound
	}
	for _, h := range f.humans {
		if h.ID == s.humanID {
			return h, nil
		}
	}
	return store.Human{}, store.ErrNotFound
}

func (f *fakeStore) DeleteSession(_ context.Context, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, tokenHash)
	return nil
}

// --- helpers ------------------------------------------------------------------------------------

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestAuth builds an Authenticator against the fake IdP via New (real discovery), then swaps in
// the in-memory store.
func newTestAuth(t *testing.T, idp *fakeIdP, baseURL string, fs *fakeStore) *Authenticator {
	t.Helper()
	cfg := config.Config{
		BaseURL:          baseURL,
		OIDCIssuer:       idp.srv.URL,
		OIDCClientID:     testClientID,
		OIDCClientSecret: "test-secret",
		OIDCRedirectURL:  baseURL + "/auth/callback",
	}
	a, err := New(context.Background(), cfg, nil, discardLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.store = fs
	return a
}

func cookieByName(t *testing.T, resp *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func requireNoSessionCookie(t *testing.T, resp *http.Response) {
	t.Helper()
	if c := cookieByName(t, resp, sessionCookie); c != nil {
		t.Fatalf("no session cookie must be set, got %q", c.Value)
	}
}

// doLogin runs Login and returns the state cookie and the parsed IdP redirect URL.
func doLogin(t *testing.T, a *Authenticator) (*http.Cookie, *url.URL) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
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

func decodeStateCookie(t *testing.T, c *http.Cookie) oidcState {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatalf("decode state cookie: %v", err)
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal state cookie: %v", err)
	}
	return st
}

// doCallback replays the IdP redirect: GET /auth/callback?state=...&code=... with the state cookie.
func doCallback(t *testing.T, a *Authenticator, state string, cookie *http.Cookie) *http.Response {
	t.Helper()
	q := url.Values{"state": {state}, "code": {"test-code"}}
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?"+q.Encode(), nil)
	if cookie != nil {
		r.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	a.Callback(rec, r)
	return rec.Result()
}

// --- login initiation ---------------------------------------------------------------------------

// SPEC-0008 scenario "OIDC not configured" / SPEC-0021 "provider=github returns 404": an
// unconfigured provider's login route is a plain 404 — no session, no cookies. (The old
// single-provider contract returned 503 with an explanatory body; the provider model 404s a
// provider that does not exist for this deployment, which is the same answer an unknown provider
// gets and confirms nothing about configuration.)
func TestLoginNotConfigured(t *testing.T) {
	a := &Authenticator{log: discardLog()}
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	resp := rec.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	requireNoSessionCookie(t, resp)
	if len(resp.Cookies()) != 0 {
		t.Fatalf("no cookies must be set, got %v", resp.Cookies())
	}
}

// SPEC-0008 scenario "Login redirects to the IdP": short-lived state cookie (state + nonce + PKCE
// verifier) and a redirect to the authorization endpoint with an S256 challenge.
func TestLoginRedirectsToIdPWithPKCE(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())

	c, loc := doLogin(t, a)

	// Redirect goes to the IdP's authorization endpoint.
	if got, want := loc.Scheme+"://"+loc.Host+loc.Path, idp.srv.URL+"/authorize"; got != want {
		t.Fatalf("redirect target: got %s, want %s", got, want)
	}
	q := loc.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type: got %q, want code", q.Get("response_type"))
	}
	if q.Get("client_id") != testClientID {
		t.Fatalf("client_id: got %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:8080/auth/callback" {
		t.Fatalf("redirect_uri: got %q", q.Get("redirect_uri"))
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope must include openid: got %q", q.Get("scope"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method: got %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("state/nonce/code_challenge must all be present: %v", q)
	}

	// The stashed cookie carries the same per-login state + nonce and the PKCE verifier behind the
	// S256 challenge.
	st := decodeStateCookie(t, c)
	if st.State != q.Get("state") {
		t.Fatal("cookie state must match the redirect's state parameter")
	}
	if st.Nonce != q.Get("nonce") {
		t.Fatal("cookie nonce must match the redirect's nonce parameter")
	}
	if st.Verifier == "" {
		t.Fatal("cookie must stash a PKCE verifier")
	}
	if oauth2.S256ChallengeFromVerifier(st.Verifier) != q.Get("code_challenge") {
		t.Fatal("code_challenge must be the S256 challenge of the stashed verifier")
	}

	// Short-lived, HttpOnly, SameSite=Lax; not Secure under a plain-HTTP base URL.
	if !c.HttpOnly {
		t.Fatal("state cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie SameSite: got %v, want Lax", c.SameSite)
	}
	if c.Secure {
		t.Fatal("state cookie must not be Secure under an http:// base URL")
	}
	if c.MaxAge <= 0 || c.MaxAge > 600 {
		t.Fatalf("state cookie must be short-lived (<=10m): MaxAge=%d", c.MaxAge)
	}
}

// Per-login values must be fresh: two logins never share state, nonce, or verifier.
func TestLoginGeneratesPerLoginValues(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())

	c1, _ := doLogin(t, a)
	c2, _ := doLogin(t, a)
	st1, st2 := decodeStateCookie(t, c1), decodeStateCookie(t, c2)
	if st1.State == st2.State || st1.Nonce == st2.Nonce || st1.Verifier == st2.Verifier {
		t.Fatal("state, nonce, and PKCE verifier must be unique per login")
	}
}

// SPEC-0008 REQ "Server-Side Session Establishment": cookies are Secure whenever the base URL is HTTPS.
func TestCookiesSecureUnderHTTPSBaseURL(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "https://switchboard.example.com", newFakeStore())

	c, _ := doLogin(t, a)
	if !c.Secure {
		t.Fatal("state cookie must be Secure under an https:// base URL")
	}
}

// --- callback verification ----------------------------------------------------------------------

func TestCallbackNotConfigured(t *testing.T) {
	a := &Authenticator{log: discardLog()}
	resp := doCallback(t, a, "whatever", nil)
	// State is validated before provider dispatch, so a missing state cookie is
	// a 400 regardless of what is configured.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	requireNoSessionCookie(t, resp)
}

func TestCallbackMissingStateCookie(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())

	resp := doCallback(t, a, "some-state", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	requireNoSessionCookie(t, resp)
	if idp.verifierSeen() != "" {
		t.Fatal("no code exchange may happen without a state cookie")
	}
}

func TestCallbackMalformedStateCookie(t *testing.T) {
	idp := newFakeIdP(t)
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", newFakeStore())

	resp := doCallback(t, a, "some-state", &http.Cookie{Name: stateCookie, Value: "%%%not-base64url"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	requireNoSessionCookie(t, resp)
	if idp.verifierSeen() != "" {
		t.Fatal("no code exchange may happen with a malformed state cookie")
	}
}

// SPEC-0008 scenario "State or nonce mismatch aborts login" (state half): a tampered state parameter
// is rejected before any code exchange, and no session is minted.
func TestCallbackStateMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	c, _ := doLogin(t, a)
	resp := doCallback(t, a, "tampered-state", c)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if !strings.Contains(respBody(t, resp), "state mismatch") {
		t.Fatal("want a state mismatch error")
	}
	requireNoSessionCookie(t, resp)
	if idp.verifierSeen() != "" {
		t.Fatal("state mismatch must abort before the code exchange")
	}
	if len(fs.sessions) != 0 || len(fs.humans) != 0 {
		t.Fatal("state mismatch must not upsert a human or mint a session")
	}
}

// SPEC-0008 scenario "State or nonce mismatch aborts login" (nonce half): the ID token verifies but
// its nonce does not match the stashed nonce — reject, no session. Also proves the PKCE verifier
// from the cookie is what gets presented at the token endpoint.
func TestCallbackNonceMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	c, loc := doLogin(t, a)
	idp.setNonce("attacker-nonce") // IdP echoes a different nonce than the one stashed

	resp := doCallback(t, a, loc.Query().Get("state"), c)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if !strings.Contains(respBody(t, resp), "nonce mismatch") {
		t.Fatal("want a nonce mismatch error")
	}
	requireNoSessionCookie(t, resp)
	if len(fs.sessions) != 0 || len(fs.humans) != 0 {
		t.Fatal("nonce mismatch must not upsert a human or mint a session")
	}

	// The exchange must have used the PKCE verifier stashed in the state cookie.
	if got, want := idp.verifierSeen(), decodeStateCookie(t, c).Verifier; got != want {
		t.Fatalf("token exchange must present the stashed PKCE verifier: got %q, want %q", got, want)
	}
}

// SPEC-0008 REQ "Callback Verification": an ID token signed by a key outside the issuer's JWKS fails
// signature verification and mints no session.
func TestCallbackBadSignatureRejected(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	c, loc := doLogin(t, a)
	idp.setNonce(loc.Query().Get("nonce"))
	idp.mu.Lock()
	idp.badSignature = true
	idp.mu.Unlock()

	resp := doCallback(t, a, loc.Query().Get("state"), c)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	if !strings.Contains(respBody(t, resp), "id_token verify failed") {
		t.Fatal("want an id_token verification error")
	}
	requireNoSessionCookie(t, resp)
	if len(fs.sessions) != 0 || len(fs.humans) != 0 {
		t.Fatal("a bad ID-token signature must not upsert a human or mint a session")
	}
}

// SPEC-0008 scenario "Valid callback establishes a session": state + nonce match, the code exchanges
// with the PKCE verifier, the ID token verifies, the human is upserted from sub/name/email, a
// server-side session is minted (hash stored, opaque token in an HttpOnly cookie), and the state
// cookie is cleared.
func TestCallbackHappyPathEstablishesSession(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	c, loc := doLogin(t, a)
	idp.setNonce(loc.Query().Get("nonce"))

	resp := doCallback(t, a, loc.Query().Get("state"), c)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("post-login redirect must be the fixed local path /: got %q", resp.Header.Get("Location"))
	}

	// The state cookie is cleared on success.
	cleared := cookieByName(t, resp, stateCookie)
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("state cookie must be cleared on success: %+v", cleared)
	}

	// The human is upserted keyed on the OIDC subject with profile fields from the token.
	h, ok := fs.humans[testSubject]
	if !ok {
		t.Fatalf("human must be upserted keyed on the OIDC subject; humans=%v", fs.humans)
	}
	if h.DisplayName != testName || h.Email != testEmail {
		t.Fatalf("profile fields must come from the ID token: %+v", h)
	}

	// A server-side session exists, keyed on the SHA-256 hash of the opaque cookie token.
	sc := cookieByName(t, resp, sessionCookie)
	if sc == nil || sc.Value == "" {
		t.Fatal("session cookie must be set")
	}
	if !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie must be HttpOnly + SameSite=Lax: %+v", sc)
	}
	if _, ok := fs.sessions[hashToken(sc.Value)]; !ok {
		t.Fatal("the store must hold the SHA-256 hash of the cookie token")
	}
	if _, ok := fs.sessions[sc.Value]; ok {
		t.Fatal("the store must never hold the plaintext session token")
	}
	got, err := fs.SessionHuman(context.Background(), hashToken(sc.Value))
	if err != nil || got.OIDCSubject != testSubject {
		t.Fatalf("session must resolve to the upserted human: %+v, %v", got, err)
	}
}

// SPEC-0008 scenario "Returning human is not duplicated": a second login with the same OIDC subject
// updates the existing record in place. (The SQL upsert's ON CONFLICT semantics are proven against
// PostgreSQL in internal/store's TestHumanAgentVend; this covers the auth-layer wiring.)
func TestReturningHumanNotDuplicated(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

	login := func() {
		t.Helper()
		c, loc := doLogin(t, a)
		idp.setNonce(loc.Query().Get("nonce"))
		resp := doCallback(t, a, loc.Query().Get("state"), c)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("callback: want 302, got %d", resp.StatusCode)
		}
	}

	login()
	first := fs.humans[testSubject]
	login()
	second := fs.humans[testSubject]

	if len(fs.humans) != 1 {
		t.Fatalf("returning login must not create a second human record: %d humans", len(fs.humans))
	}
	if second.ID != first.ID {
		t.Fatalf("human identity must be stable across logins: %q vs %q", first.ID, second.ID)
	}
	if len(fs.sessions) != 2 {
		t.Fatalf("each login mints its own session: got %d", len(fs.sessions))
	}
}

func respBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
