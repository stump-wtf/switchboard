package oauthsrv

// Conformance tests for the authorize-flow primitives (SPEC-0016 REQ "Authorization Code Flow
// With Consent"): the S256-only PKCE profile and its failure modes, the RFC 8707 resource →
// endpoint-slug binding, and single-use code minting (high-entropy plaintext, SHA-256 stored
// form). Governing: ADR-0019.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/cred"
)

func TestValidateCodeChallenge(t *testing.T) {
	valid := strings.Repeat("a", 43)
	if err := ValidateCodeChallenge(valid, "S256"); err != nil {
		t.Fatalf("valid S256 challenge rejected: %v", err)
	}
	if err := ValidateCodeChallenge(strings.Repeat("A0-_", 32), "S256"); err != nil {
		t.Fatalf("128-char base64url challenge rejected: %v", err)
	}

	// PKCE failure modes: every one must be refused, none silently accepted or downgraded.
	for name, tc := range map[string]struct{ challenge, method string }{
		"plain method":       {valid, "plain"},
		"missing method":     {valid, ""}, // OAuth 2.1: no PKCE is no flow
		"unknown method":     {valid, "S512"},
		"too short":          {strings.Repeat("a", 42), "S256"},
		"too long":           {strings.Repeat("a", 129), "S256"},
		"empty challenge":    {"", "S256"},
		"padding character":  {strings.Repeat("a", 42) + "=", "S256"},
		"plus character":     {strings.Repeat("a", 42) + "+", "S256"},
		"space in challenge": {strings.Repeat("a", 42) + " ", "S256"},
	} {
		if err := ValidateCodeChallenge(tc.challenge, tc.method); err == nil {
			t.Errorf("%s: challenge accepted, must be rejected", name)
		}
	}
}

// TestSlugFromResource: the RFC 8707 resource indicator resolves ONLY for a well-formed mount URL
// on this deployment's own base — the AS never grants onto another origin's resource.
func TestSlugFromResource(t *testing.T) {
	const base = "https://sb.example.com"
	for resource, want := range map[string]string{
		"https://sb.example.com/mcp/ci-bot-ab12cd":  "ci-bot-ab12cd",
		"https://sb.example.com/mcp/ci-bot-ab12cd/": "ci-bot-ab12cd", // tolerated trailing slash
		"https://evil.example.com/mcp/ci-bot":       "",              // foreign origin
		"https://sb.example.com/other/ci-bot":       "",              // not an MCP mount
		"https://sb.example.com/mcp/":               "",              // empty slug
		"https://sb.example.com/mcp/Bad_Slug":       "",              // not slug-shaped
		"https://sb.example.com/mcp/a/b":            "",              // nested path is no slug
		"":                                          "",
	} {
		if got := SlugFromResource(base, resource); got != want {
			t.Errorf("SlugFromResource(%q) = %q, want %q", resource, got, want)
		}
	}
	// A base with a trailing slash derives the same mount prefix.
	if got := SlugFromResource(base+"/", base+"/mcp/x1"); got != "x1" {
		t.Errorf("trailing-slash base: got %q, want %q", got, "x1")
	}
}

// TestMintCode: codes are unique high-entropy base64url strings and the returned hash is the
// SHA-256 the store persists — the plaintext never being derivable from what is stored.
func TestMintCode(t *testing.T) {
	code, hash, err := MintCode()
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}
	if len(code) != 43 { // base64url of 32 bytes, unpadded
		t.Errorf("code length = %d, want 43", len(code))
	}
	if hash != cred.Hash(code) {
		t.Error("returned hash is not the SHA-256 of the code")
	}
	if code == hash {
		t.Error("plaintext equals stored form")
	}
	code2, _, err := MintCode()
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}
	if code == code2 {
		t.Error("two minted codes collided")
	}
}
