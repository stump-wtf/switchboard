package oauthsrv

// Authorization-code plumbing for GET/POST /oauth/authorize (SPEC-0016 REQ "Authorization Code
// Flow With Consent"): PKCE (S256-only) request validation, endpoint binding from the RFC 8707
// resource indicator, and single-use code minting. The HTTP surface itself lives in internal/web
// (the consent screen renders in the charm-web shell behind auth.RequireHuman); these helpers keep
// the OAuth rules in the OAuth package so the web handler orchestrates but never re-invents them.
// Governing: ADR-0019.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
)

// CodeTTL is how long an issued authorization code may sit unredeemed. RFC 6749 §4.1.2 recommends
// a maximum of ten minutes; MCP clients redeem within seconds, so five is generous without leaving
// a long replay window. Governing: SPEC-0016 ("single-use, expiring ... authorization code").
const CodeTTL = 5 * time.Minute

// PKCE code_challenge bounds: base64url(SHA-256) is exactly 43 characters, but RFC 7636 §4.1
// bounds the VERIFIER at 43–128, and the challenge grammar shares the alphabet — so accept that
// range and reject anything outside it as malformed rather than guessing.
const (
	minChallengeLen = 43
	maxChallengeLen = 128
)

// ValidateCodeChallenge enforces the PKCE profile ADR-0019 commits to: method S256 only ("plain"
// would let a leaked code be replayed by anyone who saw the challenge), and a challenge shaped
// like base64url with no padding. The empty method is rejected rather than defaulted — OAuth 2.1
// makes PKCE mandatory, so an MCP client that sends no method has not done PKCE.
// Governing: SPEC-0016 REQ "Authorization Code Flow With Consent" (PKCE-bound (S256)).
func ValidateCodeChallenge(challenge, method string) error {
	if method != "S256" {
		return fmt.Errorf("code_challenge_method must be S256 (got %s)", quote(method))
	}
	if len(challenge) < minChallengeLen || len(challenge) > maxChallengeLen {
		return fmt.Errorf("code_challenge must be %d–%d characters", minChallengeLen, maxChallengeLen)
	}
	for _, r := range challenge {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return fmt.Errorf("code_challenge is not base64url")
		}
	}
	return nil
}

// SlugFromResource extracts the endpoint slug from an RFC 8707 resource indicator — the MCP mount
// URL an MCP client discovered via the RFC 9728 metadata (base + "/mcp/{slug}"). Everything is
// exact-prefix matched against the deployed base, so a resource on any other origin (or any other
// path) resolves to nothing: the authorization server only ever grants onto its own mounts.
// Returns "" when the indicator does not name a well-formed local mount.
// Governing: SPEC-0016 REQ "Authorization Code Flow With Consent" (bind the request to one vended
// endpoint), ADR-0019 (tokens are credentials onto existing vended endpoints).
func SlugFromResource(base, resource string) string {
	prefix := strings.TrimRight(base, "/") + "/mcp/"
	slug, ok := strings.CutPrefix(resource, prefix)
	if !ok {
		return ""
	}
	slug = strings.TrimSuffix(slug, "/")
	if !SlugOK(slug) {
		return ""
	}
	return slug
}

// MintCode mints a single-use authorization code: 32 random bytes, base64url — the same entropy
// class as internal/cred's endpoint credentials — plus the SHA-256 hex the store persists. The
// plaintext exists only in the redirect back to the client; only the hash is ever stored.
// Governing: SPEC-0016 ("Codes SHALL be stored hashed"), design.md "Opaque tokens, hashed at rest".
func MintCode() (code, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("oauthsrv: read random: %w", err)
	}
	code = base64.RawURLEncoding.EncodeToString(b)
	return code, cred.Hash(code), nil
}
