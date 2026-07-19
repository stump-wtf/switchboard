// Token-endpoint conformance tests (SPEC-0016 REQ "Token Issuance And Refresh"; design.md
// "Security notes" names PKCE failure modes, redirect mismatches, and code replay as part of the
// definition of done): the code + PKCE exchange, its rejection matrix, refresh rotation, and the
// refresh-after-endpoint-death scenario — all over a functional in-memory TokenStore so every
// grant decision the HANDLER makes is pinned independently of Postgres (the storage semantics have
// their own tests in internal/store).
package oauthsrv

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// fakeTokenStore implements TokenStore with real single-use/rotation semantics: codes redeem
// exactly once (replay reported as ErrCodeReplayed, mimicking the store's token-revoking replay
// branch), tokens rotate atomically under a mutex, and killing an endpoint makes both issuance
// and rotation fail with ErrNotFound — the exact contract *store.Store provides over Postgres.
type fakeTokenStore struct {
	unusedClientStore

	mu       sync.Mutex
	codes    map[string]*store.OAuthCode // by code hash
	tokens   map[string]fakeToken        // by refresh hash
	endpoint string                      // the one live endpoint id; "" = dead
	epExpiry *time.Time                  // endpoint expires_at (clamp source), nil = no expiry
}

type fakeToken struct {
	accessHash string
	clientID   string
	endpointID string
	revoked    bool
}

func (f *fakeTokenStore) RedeemOAuthCode(_ context.Context, codeHash string) (store.OAuthCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[codeHash]
	if !ok || time.Now().After(c.ExpiresAt) {
		return store.OAuthCode{}, store.ErrNotFound
	}
	if c.UsedAt != nil {
		// The real store revokes the grant's tokens inside the same transaction; mirror that.
		for rh, t := range f.tokens {
			if t.clientID == c.ClientID && t.endpointID == c.EndpointID {
				t.revoked = true
				f.tokens[rh] = t
			}
		}
		return store.OAuthCode{}, store.ErrCodeReplayed
	}
	now := time.Now()
	c.UsedAt = &now
	return *c, nil
}

func (f *fakeTokenStore) CreateOAuthToken(_ context.Context, tokenHash, refreshHash, clientID, endpointID string, desiredExpiry time.Time) (store.OAuthToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createLocked(tokenHash, refreshHash, clientID, endpointID, desiredExpiry)
}

func (f *fakeTokenStore) createLocked(tokenHash, refreshHash, clientID, endpointID string, desiredExpiry time.Time) (store.OAuthToken, error) {
	if endpointID != f.endpoint || f.endpoint == "" {
		return store.OAuthToken{}, store.ErrNotFound
	}
	expires := desiredExpiry
	if f.epExpiry != nil && f.epExpiry.Before(expires) {
		expires = *f.epExpiry
	}
	f.tokens[refreshHash] = fakeToken{accessHash: tokenHash, clientID: clientID, endpointID: endpointID}
	return store.OAuthToken{ID: "tok-" + refreshHash[:8], ClientID: clientID, EndpointID: endpointID,
		ExpiresAt: expires, CreatedAt: time.Now()}, nil
}

func (f *fakeTokenStore) RotateOAuthToken(_ context.Context, refreshHash, clientID, newTokenHash, newRefreshHash string, desiredExpiry time.Time) (store.OAuthToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[refreshHash]
	if !ok || t.revoked || t.clientID != clientID {
		return store.OAuthToken{}, store.ErrNotFound
	}
	t.revoked = true
	f.tokens[refreshHash] = t
	return f.createLocked(newTokenHash, newRefreshHash, clientID, t.endpointID, desiredExpiry)
}

// unusedClientStore panics on any use: token-endpoint tests must never reach registration.
type unusedClientStore struct{}

func (unusedClientStore) CreateOAuthClient(context.Context, string, string, []string) (store.OAuthClient, error) {
	panic("client store used in a token test")
}

const (
	tokTestClient   = "client-abc123"
	tokTestEndpoint = "ep-1111"
	tokTestRedirect = "https://c.example.com/cb"
	tokTestVerifier = "verifier-verifier-verifier-verifier-verifier" // 44 chars, RFC 7636 alphabet
)

// challengeFor computes the S256 code_challenge for a verifier, exactly as a client would.
func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newTokenHandler builds a Handler over a fakeTokenStore holding one live endpoint and one
// unredeemed code (PKCE-bound to tokTestVerifier, issued for tokTestRedirect). Returns the
// handler, the fake, and the plaintext code.
func newTokenHandler(t *testing.T) (*Handler, *fakeTokenStore, string) {
	t.Helper()
	code, codeHash, err := MintCode()
	if err != nil {
		t.Fatalf("mint code: %v", err)
	}
	fs := &fakeTokenStore{
		codes: map[string]*store.OAuthCode{codeHash: {
			ID: "code-1", ClientID: tokTestClient, EndpointID: tokTestEndpoint,
			PKCEChallenge: challengeFor(tokTestVerifier), RedirectURI: tokTestRedirect,
			ExpiresAt: time.Now().Add(CodeTTL),
		}},
		tokens:   map[string]fakeToken{},
		endpoint: tokTestEndpoint,
	}
	h := New(fs, testBase, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h, fs, code
}

// postToken drives POST /oauth/token with form values and decodes the JSON response.
func postToken(t *testing.T, h *Handler, form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, TokenPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Token(rec, req)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("token response is not JSON (status %d): %s", rec.Code, rec.Body.String())
	}
	return rec, doc
}

func exchangeForm(code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {tokTestVerifier},
		"client_id":     {tokTestClient},
		"redirect_uri":  {tokTestRedirect},
	}
}

// wantTokenError asserts an RFC 6749 §5.2 error document with the given code.
func wantTokenError(t *testing.T, rec *httptest.ResponseRecorder, doc map[string]any, status int, errCode string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if doc["error"] != errCode {
		t.Fatalf("error = %v, want %q (body %s)", doc["error"], errCode, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// TestTokenExchangeIssuesPair: a valid code + verifier exchange answers the RFC 6749 §5.1 success
// document — opaque access + refresh, Bearer, bounded expiry, no-store — and the persisted
// material is hashes only (the fake indexes by hash; the plaintexts never reach it).
func TestTokenExchangeIssuesPair(t *testing.T) {
	h, fs, code := newTokenHandler(t)
	rec, doc := postToken(t, h, exchangeForm(code))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	access, _ := doc["access_token"].(string)
	refresh, _ := doc["refresh_token"].(string)
	if access == "" || refresh == "" || access == refresh {
		t.Fatalf("token pair = %q / %q", access, refresh)
	}
	if doc["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v", doc["token_type"])
	}
	if ei, ok := doc["expires_in"].(float64); !ok || ei <= 0 || ei > AccessTokenTTL.Seconds() {
		t.Fatalf("expires_in = %v, want (0, %v]", doc["expires_in"], AccessTokenTTL.Seconds())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// Stored at rest: the refresh HASH indexes the row, and the row holds the access HASH — never
	// either plaintext (SPEC-0016: "stored hashed", "raw token values SHALL never be logged or stored").
	row, ok := fs.tokens[cred.Hash(refresh)]
	if !ok {
		t.Fatal("refresh token not persisted under its hash")
	}
	if row.accessHash != cred.Hash(access) {
		t.Fatal("access token not persisted as its hash")
	}
}

// TestTokenExchangePKCEFailureModes pins the PKCE rejection matrix: a wrong verifier, a
// well-formed verifier for a different challenge, a malformed (short / bad-alphabet) verifier,
// and a missing verifier all refuse the grant — and the code is single-use, so a failed guess
// burns it rather than leaving it retryable.
func TestTokenExchangePKCEFailureModes(t *testing.T) {
	cases := []struct {
		name     string
		verifier string
		errCode  string
	}{
		{"wrong verifier", "wrong-verifier-wrong-verifier-wrong-verifier", "invalid_grant"},
		{"too short", "short", "invalid_grant"},
		{"bad alphabet", strings.Repeat("!", 60), "invalid_grant"},
		{"missing", "", "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, code := newTokenHandler(t)
			form := exchangeForm(code)
			form.Set("code_verifier", tc.verifier)
			if tc.verifier == "" {
				form.Del("code_verifier")
			}
			rec, doc := postToken(t, h, form)
			wantTokenError(t, rec, doc, http.StatusBadRequest, tc.errCode)

			// The failed exchange must not have minted anything, and (except for the pre-redemption
			// missing-verifier reject) it spent the code: the correct verifier now meets replay.
			if tc.errCode == "invalid_grant" {
				rec, doc = postToken(t, h, exchangeForm(code))
				wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")
			}
		})
	}
}

// TestTokenExchangeRedirectMismatch: a redirect_uri that is not byte-for-byte the one the code was
// issued for refuses the grant (design.md "Security notes": redirect mismatches are part of the
// conformance definition of done). Omitting redirect_uri is tolerated per OAuth 2.1 — PKCE binds.
func TestTokenExchangeRedirectMismatch(t *testing.T) {
	h, _, code := newTokenHandler(t)
	form := exchangeForm(code)
	form.Set("redirect_uri", "https://evil.example.com/cb")
	rec, doc := postToken(t, h, form)
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")

	h2, _, code2 := newTokenHandler(t)
	form2 := exchangeForm(code2)
	form2.Del("redirect_uri")
	if rec2, _ := postToken(t, h2, form2); rec2.Code != http.StatusOK {
		t.Fatalf("omitted redirect_uri: status = %d, body %s", rec2.Code, rec2.Body.String())
	}
}

// TestTokenExchangeClientMismatch: a code presented by a different client_id than it was issued to
// refuses the grant.
func TestTokenExchangeClientMismatch(t *testing.T) {
	h, _, code := newTokenHandler(t)
	form := exchangeForm(code)
	form.Set("client_id", "some-other-client")
	rec, doc := postToken(t, h, form)
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")
}

// TestTokenExchangeCodeReplay: the second redemption of a code is refused AND revokes the tokens
// the first exchange issued — the SPEC-0016 replay teeth, observed end-to-end through the handler:
// after the replay, the previously issued refresh token no longer rotates.
func TestTokenExchangeCodeReplay(t *testing.T) {
	h, _, code := newTokenHandler(t)
	rec, doc := postToken(t, h, exchangeForm(code))
	if rec.Code != http.StatusOK {
		t.Fatalf("first exchange: status = %d", rec.Code)
	}
	refresh := doc["refresh_token"].(string)

	rec, doc = postToken(t, h, exchangeForm(code))
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")

	rec, doc = postToken(t, h, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {tokTestClient},
	})
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")
}

// TestTokenExchangeUnknownCode: a never-issued code is invalid_grant, indistinguishable from an
// expired one.
func TestTokenExchangeUnknownCode(t *testing.T) {
	h, _, _ := newTokenHandler(t)
	form := exchangeForm("never-issued-code-value")
	rec, doc := postToken(t, h, form)
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")
}

// TestRefreshRotation: a refresh grant answers a NEW pair, invalidates the presented refresh
// (second presentation = invalid_grant), and the new refresh keeps working — the rotating-chain
// contract of SPEC-0016 ("refresh SHALL rotate (old refresh invalidated)").
func TestRefreshRotation(t *testing.T) {
	h, _, code := newTokenHandler(t)
	_, doc := postToken(t, h, exchangeForm(code))
	refresh1 := doc["refresh_token"].(string)

	refreshForm := func(rt string) url.Values {
		return url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {rt},
			"client_id":     {tokTestClient},
		}
	}
	rec, doc := postToken(t, h, refreshForm(refresh1))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotation: status = %d, body %s", rec.Code, rec.Body.String())
	}
	refresh2, _ := doc["refresh_token"].(string)
	if refresh2 == "" || refresh2 == refresh1 {
		t.Fatalf("rotation did not mint a new refresh token")
	}

	// The old refresh is dead.
	rec, doc = postToken(t, h, refreshForm(refresh1))
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")

	// A LIVE refresh presented by the wrong client is refused — and not consumed: the pair is
	// client-bound, and a foreign presentation must not burn the rightful client's token.
	rec, doc = postToken(t, h, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh2},
		"client_id":     {"some-other-client"},
	})
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")

	// The new refresh still rotates for its own client.
	if rec, _ := postToken(t, h, refreshForm(refresh2)); rec.Code != http.StatusOK {
		t.Fatalf("second rotation: status = %d", rec.Code)
	}
}

// TestRefreshAfterEndpointDeath is SPEC-0016's scenario verbatim: once the grant's endpoint is
// revoked, the refresh grant fails and no new token is issued.
func TestRefreshAfterEndpointDeath(t *testing.T) {
	h, fs, code := newTokenHandler(t)
	_, doc := postToken(t, h, exchangeForm(code))
	refresh := doc["refresh_token"].(string)

	fs.mu.Lock()
	fs.endpoint = "" // the operator revoked the endpoint
	fs.mu.Unlock()

	rec, doc := postToken(t, h, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {tokTestClient},
	})
	wantTokenError(t, rec, doc, http.StatusBadRequest, "invalid_grant")

	// No new token was issued, and the presented refresh died with the endpoint rather than
	// remaining presentable (the store commits the revocation even when the mint refuses).
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.tokens) != 1 {
		t.Fatalf("token rows = %d, want 1 (no new token issued)", len(fs.tokens))
	}
	for _, tok := range fs.tokens {
		if !tok.revoked {
			t.Fatal("the presented refresh must be revoked, not left presentable")
		}
	}
}

// TestTokenUnsupportedGrantAndMissingFields: the request-validation matrix — foreign grant types,
// missing required fields, oversized fields — all answer invalid_request/unsupported_grant_type
// without touching storage.
func TestTokenUnsupportedGrantAndMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		errCode string
	}{
		{"foreign grant", url.Values{"grant_type": {"client_credentials"}}, "unsupported_grant_type"},
		{"no grant type", url.Values{}, "unsupported_grant_type"},
		{"code grant without code", url.Values{"grant_type": {"authorization_code"},
			"code_verifier": {tokTestVerifier}, "client_id": {tokTestClient}}, "invalid_request"},
		{"code grant without client_id", url.Values{"grant_type": {"authorization_code"},
			"code": {"x"}, "code_verifier": {tokTestVerifier}}, "invalid_request"},
		{"refresh grant without token", url.Values{"grant_type": {"refresh_token"},
			"client_id": {tokTestClient}}, "invalid_request"},
		{"refresh grant without client_id", url.Values{"grant_type": {"refresh_token"},
			"refresh_token": {"x"}}, "invalid_request"},
		{"oversized code", url.Values{"grant_type": {"authorization_code"},
			"code": {strings.Repeat("a", maxTokenParamLen+1)}, "code_verifier": {tokTestVerifier},
			"client_id": {tokTestClient}}, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTokenHandler(t)
			rec, doc := postToken(t, h, tc.form)
			wantTokenError(t, rec, doc, http.StatusBadRequest, tc.errCode)
		})
	}
}

// TestVerifyPKCE pins the primitive directly: the RFC 7636 grammar bounds and the S256 equation.
func TestVerifyPKCE(t *testing.T) {
	if err := VerifyPKCE(tokTestVerifier, challengeFor(tokTestVerifier)); err != nil {
		t.Fatalf("valid verifier rejected: %v", err)
	}
	if err := VerifyPKCE(tokTestVerifier, challengeFor("another-verifier-another-verifier-another-x")); err == nil {
		t.Fatal("mismatched challenge accepted")
	}
	if err := VerifyPKCE(strings.Repeat("a", 42), challengeFor("x")); err == nil {
		t.Fatal("42-char verifier accepted (RFC 7636 minimum is 43)")
	}
	if err := VerifyPKCE(strings.Repeat("a", 129), challengeFor("x")); err == nil {
		t.Fatal("129-char verifier accepted (RFC 7636 maximum is 128)")
	}
	if err := VerifyPKCE(strings.Repeat("a", 40)+"$$$", challengeFor("x")); err == nil {
		t.Fatal("verifier outside the unreserved alphabet accepted")
	}
}
