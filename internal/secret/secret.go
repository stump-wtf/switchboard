// Package secret provides authenticated at-rest encryption (AES-256-GCM) for the small held secrets
// switchboard must keep in reversible form.
//
// Most credentials switchboard stores are one-way: vended endpoint tokens and session tokens are
// SHA-256 hashed (see internal/cred), because verification only needs an equality check. A minted
// per-webhook HMAC signing secret is different — switchboard must recompute the provider HMAC over
// every inbound body to verify a signed delivery (SPEC-0006 "Switchboard Owns Secrets, Verification,
// and Idempotency"), so it MUST hold the secret in a recoverable form; a hash cannot verify. Holding
// it as plaintext in PostgreSQL means a DB-only compromise directly yields live signing secrets.
// This package closes that gap: the secret is encrypted with a key supplied out-of-band via
// SWITCHBOARD_SECRET_KEY and never stored beside the ciphertext, so a database dump alone is inert.
//
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency" (optional
// hardening — encrypt held signing secrets at rest with a key not stored alongside the ciphertext).
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// keyLen is the AES-256 key length in bytes.
const keyLen = 32

// ErrNoKey is returned by Parse when SWITCHBOARD_SECRET_KEY is empty. Callers MUST fail startup on
// it rather than silently store plaintext.
var ErrNoKey = errors.New("secret: SWITCHBOARD_SECRET_KEY is not set")

// Cipher seals and opens small secrets with AES-256-GCM. It is safe for concurrent use — the
// underlying AEAD is stateless and a fresh random nonce is drawn per Seal.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a raw 32-byte AES-256 key. A key of any other length is rejected rather
// than silently truncated or stretched.
func New(key []byte) (*Cipher, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("secret: key must be %d bytes, got %d", keyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Parse builds a Cipher from the SWITCHBOARD_SECRET_KEY value. The key is a 32-byte AES-256 key
// encoded as either 64 hex characters or base64 (standard or raw, padded or not). An empty value
// returns ErrNoKey; a value that does not decode to exactly 32 bytes is a hard error — the caller
// MUST fail startup, never fall back to storing plaintext.
func Parse(encoded string) (*Cipher, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, ErrNoKey
	}
	key, err := decodeKey(encoded)
	if err != nil {
		return nil, err
	}
	return New(key)
}

// decodeKey turns the encoded SWITCHBOARD_SECRET_KEY into raw bytes, trying hex first (a 64-char hex
// string is unambiguous) then base64. The error never echoes the key material.
func decodeKey(encoded string) ([]byte, error) {
	if len(encoded) == 2*keyLen {
		if b, err := hex.DecodeString(encoded); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(encoded); err == nil && len(b) == keyLen {
			return b, nil
		}
	}
	return nil, fmt.Errorf("secret: SWITCHBOARD_SECRET_KEY must decode to a %d-byte key (64-char hex or base64)", keyLen)
}

// Encrypt seals plaintext, returning nonce||ciphertext (the nonce is prepended so Decrypt is
// self-describing). A crypto/rand failure is returned, never swallowed — sealing with a predictable
// nonce would be a silent security hole.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}
	// Seal appends the ciphertext to nonce, so the returned slice is nonce||ciphertext||tag.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens a nonce||ciphertext box produced by Encrypt. A box shorter than the nonce, or one
// whose authentication tag does not verify (tampered ciphertext, or a different key), is an error —
// GCM authenticates, so a wrong key or altered bytes fail closed rather than returning garbage.
func (c *Cipher) Decrypt(box []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(box) < ns {
		return nil, fmt.Errorf("secret: ciphertext too short (%d < nonce %d)", len(box), ns)
	}
	nonce, ct := box[:ns], box[ns:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("secret: open: %w", err)
	}
	return pt, nil
}
