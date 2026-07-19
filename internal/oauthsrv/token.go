package oauthsrv

// POST /oauth/token (SPEC-0016 REQ "Token Issuance And Refresh"): the code + PKCE exchange and the
// rotating refresh grant. Both grants mint opaque high-entropy tokens — the same entropy class as
// internal/cred's endpoint credentials, hashed with the same primitive — bound to exactly one
// vended endpoint, with the access expiry clamped to the endpoint's own expiry inside the store's
// atomic insert. Raw token material exists only in the JSON response; every persisted and logged
// value is a hash or an identifier. Governing: ADR-0019, design.md "Opaque tokens, hashed at rest".

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// AccessTokenTTL is the policy lifetime of an access token. One hour keeps a leaked access token
// short-lived (the refresh grant exists precisely so clients never need a long one) while the
// store additionally clamps every issuance to the endpoint's own expires_at — the endpoint's
// lifetime always wins when it is nearer. Governing: SPEC-0016 ("Access tokens SHALL carry an
// expiry no later than the endpoint's own expiry").
const AccessTokenTTL = time.Hour

// Token-request parameter bounds (SPEC-0016 "Security Requirements → Input Validation"): every
// legitimate value is a short opaque string (codes and tokens are 43 chars, client_ids 32, PKCE
// verifiers ≤128); redirect_uri shares the registration bound. Anything longer is abuse.
const (
	maxTokenParamLen = 512
)

// TokenStore is the slice of the store the token endpoint needs: code redemption (with its
// replay-revocation semantics), atomic clamped issuance, and atomic refresh rotation.
// *store.Store satisfies it; tests substitute a fake.
type TokenStore interface {
	RedeemOAuthCode(ctx context.Context, codeHash string) (store.OAuthCode, error)
	CreateOAuthToken(ctx context.Context, tokenHash, refreshHash, clientID, endpointID string, desiredExpiry time.Time) (store.OAuthToken, error)
	RotateOAuthToken(ctx context.Context, refreshHash, clientID, newTokenHash, newRefreshHash string, desiredExpiry time.Time) (store.OAuthToken, error)
}

// MintToken mints one opaque OAuth token (access or refresh): 32 random bytes, base64url — the
// same entropy class as internal/cred endpoint credentials — plus the SHA-256 hex the store
// persists. No sbk_ prefix: the prefix marks static endpoint bearers, and the resource server
// dispatches on it to pick the lookup path, so the two credential shapes stay unambiguous. The
// base64url alphabet CAN spell "sbk_" by chance (~1 in 16.7M mints), which would misroute the
// token down the static-bearer path forever, so such a draw is discarded and re-minted.
// Governing: SPEC-0016 ("opaque access token and rotating refresh token, both high-entropy and
// stored hashed"), design.md "Opaque tokens, hashed at rest".
func MintToken() (token, hash string, err error) {
	b := make([]byte, 32)
	for {
		if _, err := rand.Read(b); err != nil {
			return "", "", fmt.Errorf("oauthsrv: read random: %w", err)
		}
		token = base64.RawURLEncoding.EncodeToString(b)
		if !strings.HasPrefix(token, "sbk_") {
			return token, cred.Hash(token), nil
		}
	}
}

// VerifyPKCE reports whether the presented code_verifier satisfies the stored S256 challenge:
// base64url(SHA-256(verifier)) must equal the challenge byte-for-byte, compared constant-time.
// The verifier grammar is checked first (RFC 7636 §4.1: 43–128 unreserved characters) so a
// malformed verifier is rejected as malformed rather than hashed and mismatched. Governing:
// SPEC-0016 REQ "Authorization Code Flow With Consent" (PKCE-bound (S256)).
func VerifyPKCE(verifier, challenge string) error {
	if len(verifier) < minChallengeLen || len(verifier) > maxChallengeLen {
		return fmt.Errorf("code_verifier must be %d–%d characters", minChallengeLen, maxChallengeLen)
	}
	for _, r := range verifier {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '.' && r != '_' && r != '~' {
			return errors.New("code_verifier contains characters outside the RFC 7636 alphabet")
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) != 1 {
		return errors.New("code_verifier does not satisfy the code_challenge")
	}
	return nil
}

// Token serves POST /oauth/token: grant_type=authorization_code (code + PKCE exchange) and
// grant_type=refresh_token (rotation). Clients are public — PKCE is the proof on the code grant,
// possession of the (rotating, single-use) refresh token plus the matching client_id on the
// refresh grant — so no client authentication happens here by design. Every response, success or
// error, is JSON with Cache-Control: no-store (RFC 6749 §5.1). Governing: SPEC-0016 REQ "Token
// Issuance And Refresh".
func (h *Handler) Token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "request body must be application/x-www-form-urlencoded")
		return
	}
	for _, p := range []struct {
		name string
		max  int
	}{
		{"grant_type", maxTokenParamLen},
		{"code", maxTokenParamLen},
		{"code_verifier", maxTokenParamLen},
		{"client_id", maxTokenParamLen},
		{"refresh_token", maxTokenParamLen},
		{"redirect_uri", maxRedirectLen}, // matches the registration bound
	} {
		if len(r.PostForm.Get(p.name)) > p.max {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", p.name+" is too long")
			return
		}
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		h.exchangeCode(w, r)
	case "refresh_token":
		h.refreshGrant(w, r)
	default:
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type",
			"grant_type must be authorization_code or refresh_token")
	}
}

// exchangeCode redeems an authorization code for the first access/refresh pair. Order of
// operations is deliberate: the code is spent (single-use, atomically — replay revokes the
// grant's tokens in the store) BEFORE the PKCE / client / redirect checks, so a failed exchange
// burns the code rather than leaving it retryable with fresh guesses. All post-redemption
// failures are uniformly invalid_grant, leaking nothing about which check failed.
func (h *Handler) exchangeCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	clientID := r.PostForm.Get("client_id")
	switch {
	case code == "":
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	case verifier == "":
		// OAuth 2.1 makes PKCE mandatory: a client that sends no verifier has not done PKCE.
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "code_verifier is required")
		return
	case clientID == "":
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}

	c, err := h.tokens.RedeemOAuthCode(r.Context(), cred.Hash(code))
	if errors.Is(err, store.ErrCodeReplayed) {
		// The store has already revoked every token the replayed grant issued (SPEC-0016: "replayed
		// codes SHALL be rejected and SHALL revoke tokens issued from that code").
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code already redeemed")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is unknown or expired")
		return
	}
	if err != nil {
		h.log.Error("oauth token: redeem code", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}
	if c.ClientID != clientID {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code was not issued to this client")
		return
	}
	// redirect_uri: when the client presents one it must byte-for-byte match the URI the code was
	// issued for (RFC 6749 §4.1.3); absence is tolerated per OAuth 2.1, where PKCE carries the
	// binding the redirect check used to. A mismatch is a rejected grant — and the code is already
	// spent, so the mismatch cannot be retried into an issuance.
	if ru := r.PostForm.Get("redirect_uri"); ru != "" && ru != c.RedirectURI {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if err := VerifyPKCE(verifier, c.PKCEChallenge); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}

	h.issue(w, r, func(accessHash, refreshHash string) (store.OAuthToken, error) {
		return h.tokens.CreateOAuthToken(r.Context(), accessHash, refreshHash, c.ClientID, c.EndpointID,
			time.Now().Add(AccessTokenTTL))
	})
}

// refreshGrant rotates a refresh token: the presented pair is revoked and a fresh pair minted
// atomically in the store, under the same endpoint-expiry clamp as first issuance. A refresh that
// is unknown, already rotated, presented by the wrong client, or whose endpoint is revoked or
// expired is uniformly invalid_grant — refresh can never recover access to a dead endpoint.
// Governing: SPEC-0016 scenario "Refresh after endpoint death".
func (h *Handler) refreshGrant(w http.ResponseWriter, r *http.Request) {
	refresh := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")
	switch {
	case refresh == "":
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	case clientID == "":
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}

	h.issue(w, r, func(accessHash, refreshHash string) (store.OAuthToken, error) {
		return h.tokens.RotateOAuthToken(r.Context(), cred.Hash(refresh), clientID, accessHash, refreshHash,
			time.Now().Add(AccessTokenTTL))
	})
}

// issue mints the access/refresh pair, persists it through mint (which binds it to the grant's
// endpoint under the expiry clamp), and writes the RFC 6749 §5.1 success document. The plaintext
// pair exists only in this response; the store saw only hashes, and nothing here is logged.
func (h *Handler) issue(w http.ResponseWriter, r *http.Request, mint func(accessHash, refreshHash string) (store.OAuthToken, error)) {
	access, accessHash, err := MintToken()
	if err != nil {
		h.log.Error("oauth token: mint access", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}
	refresh, refreshHash, err := MintToken()
	if err != nil {
		h.log.Error("oauth token: mint refresh", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}
	tok, err := mint(accessHash, refreshHash)
	if errors.Is(err, store.ErrNotFound) {
		// The grant's endpoint is revoked, expired, or gone — or the refresh token was not
		// presentable. Either way: no new token, uniformly invalid_grant.
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "the grant is no longer valid")
		return
	}
	if err != nil {
		h.log.Error("oauth token: persist token", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}
	expiresIn := int64(time.Until(tok.ExpiresAt).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"refresh_token": refresh,
	})
}

// writeTokenError answers a rejected token request with the RFC 6749 §5.2 error document.
// Cache-Control: no-store on the error too — a token-endpoint response is never cacheable.
func writeTokenError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}
