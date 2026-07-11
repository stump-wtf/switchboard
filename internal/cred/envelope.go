package cred

// Envelope encryption for secrets switchboard must HOLD in plaintext to do its job (as opposed to
// vended credentials, which are hashed — see Mint/Hash above). The canonical case is a self-managed
// webhook's HMAC signing secret: switchboard mints it, reveals it once, and then must recompute the
// provider HMAC over every inbound body to verify a signed delivery (SPEC-0003), so it cannot be
// hashed. Encrypting it at rest with a key held OUTSIDE the database (injected via env/deploy config)
// means a DB-only compromise yields ciphertext, not live signing secrets.
//
// Primitive: AES-256-GCM (authenticated) with a random 96-bit nonce per encryption, prepended to the
// ciphertext. The stored form is a self-describing string `enc:v1:<base64(nonce||ciphertext||tag)>`
// so a reader can tell an encrypted value from a legacy plaintext one and decrypt transparently.
//
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency" (switchboard
// holds the secret and verifies), ADR-0002 (PostgreSQL is the trust boundary).

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

// encPrefix marks a stored value as v1 AES-256-GCM ciphertext. A value without this prefix is a
// legacy plaintext secret (from before at-rest encryption was enabled) and is returned verbatim by
// Decrypt, so turning encryption on does not strand existing rows.
const encPrefix = "enc:v1:"

// SecretBox seals and opens held secrets with AES-256-GCM under a single symmetric key. It is
// safe for concurrent use: cipher.AEAD is stateless across Seal/Open calls and each Encrypt draws a
// fresh random nonce.
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox builds a SecretBox from a 32-byte (AES-256) key. Any other length is rejected rather
// than silently truncated/padded — a wrong-length key is a deployment error, not something to paper
// over into a weak cipher.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cred: secret encryption key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cred: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cred: new gcm: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

// ParseSecretBoxKey decodes a configured key string into 32 raw bytes. It accepts standard base64,
// raw-URL base64, or hex — whichever decodes to exactly 32 bytes — so operators can supply the key
// in whatever form their secret store hands them. An empty string is a distinct, non-error signal
// that no key is configured (encryption disabled): callers check for len==0.
func ParseSecretBoxKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// Try the common encodings; keep the first that yields exactly 32 bytes.
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("cred: secret encryption key must decode (base64 or hex) to exactly 32 bytes")
}

// Encrypt seals plaintext and returns the self-describing stored form `enc:v1:<base64(nonce||ct)>`.
// A fresh random nonce is drawn per call, so encrypting the same secret twice yields distinct
// ciphertexts. A crypto/rand failure is surfaced, never swallowed (encrypting under a zero nonce
// would be a silent break).
func (b *SecretBox) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cred: read nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a stored value. A value WITHOUT the enc:v1: prefix is treated as legacy plaintext and
// returned unchanged, so a SecretBox can transparently read rows written before encryption was
// enabled. A prefixed value that fails authentication (wrong key or tampered ciphertext) is a hard
// error — GCM's tag is the integrity guarantee.
func (b *SecretBox) Decrypt(stored string) (string, error) {
	if !IsEncrypted(stored) {
		return stored, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("cred: decode ciphertext: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("cred: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := b.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("cred: open (wrong key or tampered ciphertext): %w", err)
	}
	return string(pt), nil
}

// IsEncrypted reports whether a stored value is in the enc:v1: ciphertext form (vs. legacy plaintext).
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, encPrefix)
}
