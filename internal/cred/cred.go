// Package cred mints and hashes vended endpoint credentials (ADR-008).
//
// A credential is a high-entropy bearer token `sbk_<base64url(32 bytes)>`. Only its SHA-256 hash is
// stored; the plaintext is shown to the human exactly once at vend time. High-entropy tokens don't
// need a slow password hash (bcrypt/argon2 are for low-entropy secrets); SHA-256 + constant-time
// compare at the boundary is the right primitive, matching how API tokens are stored.
package cred

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const prefix = "sbk_"

// Mint returns a new credential: the plaintext token (shown once), its stored hash, and a
// non-secret display prefix.
func Mint() (token, hash, display string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token), Display(token)
}

// Hash returns the SHA-256 hex of a token (stored + used for lookup).
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Display returns a short, non-secret prefix for showing which credential is which.
func Display(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12] + "…"
}
