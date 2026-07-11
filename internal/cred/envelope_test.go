package cred

import (
	"encoding/base64"
	"strings"
	"testing"
)

func newTestBox(t *testing.T) *SecretBox {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatalf("new secret box: %v", err)
	}
	return box
}

func TestSecretBoxRoundTrip(t *testing.T) {
	box := newTestBox(t)
	const plaintext = "whsec_deadbeefcafe"

	ct, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Stored form must be the self-describing ciphertext, never the plaintext.
	if !IsEncrypted(ct) {
		t.Fatalf("ciphertext %q missing enc:v1: prefix", ct)
	}
	if strings.Contains(ct, plaintext) {
		t.Fatalf("ciphertext %q must not contain the plaintext", ct)
	}

	got, err := box.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round-trip = %q, want %q", got, plaintext)
	}
}

func TestSecretBoxNonceIsRandom(t *testing.T) {
	box := newTestBox(t)
	a, _ := box.Encrypt("same-secret")
	b, _ := box.Encrypt("same-secret")
	if a == b {
		t.Fatal("encrypting the same secret twice must yield distinct ciphertexts (random nonce)")
	}
}

// A value without the enc:v1: prefix is legacy plaintext and must pass through Decrypt unchanged, so
// enabling encryption does not strand rows written before it was on.
func TestSecretBoxDecryptLegacyPlaintext(t *testing.T) {
	box := newTestBox(t)
	got, err := box.Decrypt("whsec_legacy_plaintext")
	if err != nil {
		t.Fatalf("decrypt legacy: %v", err)
	}
	if got != "whsec_legacy_plaintext" {
		t.Fatalf("legacy passthrough = %q", got)
	}
}

// GCM authentication: a tampered ciphertext (or a wrong key) must fail to open, never return garbage.
func TestSecretBoxTamperAndWrongKeyFail(t *testing.T) {
	box := newTestBox(t)
	ct, err := box.Encrypt("whsec_x")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// Flip a byte inside the raw sealed bytes (nonce‖ct‖tag), then re-encode, so the tamper survives
	// base64 round-tripping (flipping a trailing base64 char can land on ignored padding bits).
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ct, encPrefix))
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	raw[len(raw)-1] ^= 0x01 // corrupt the last byte of the GCM tag
	tampered := encPrefix + base64.RawStdEncoding.EncodeToString(raw)
	if _, err := box.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext must fail to decrypt")
	}

	// A different key must not open ciphertext sealed under box's key.
	otherKey := make([]byte, 32)
	otherKey[0] = 0xAA
	other, err := NewSecretBox(otherKey)
	if err != nil {
		t.Fatalf("new other box: %v", err)
	}
	if _, err := other.Decrypt(ct); err == nil {
		t.Fatal("wrong key must fail to decrypt")
	}
}

func TestNewSecretBoxRejectsBadKeyLen(t *testing.T) {
	if _, err := NewSecretBox(make([]byte, 16)); err == nil {
		t.Fatal("16-byte key must be rejected (AES-256 needs 32)")
	}
}

func TestParseSecretBoxKey(t *testing.T) {
	// Empty is a non-error "encryption disabled" signal.
	if b, err := ParseSecretBoxKey(""); err != nil || b != nil {
		t.Fatalf("empty key = (%v, %v), want (nil, nil)", b, err)
	}
	// A 32-byte base64 key parses; a wrong-length or garbage one does not.
	valid := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes, std base64
	if b, err := ParseSecretBoxKey(valid); err != nil || len(b) != 32 {
		t.Fatalf("valid base64 key = (len %d, %v), want (32, nil)", len(b), err)
	}
	if _, err := ParseSecretBoxKey("too-short"); err == nil {
		t.Fatal("garbage key must be rejected")
	}
	// Hex form is also accepted.
	hexKey := strings.Repeat("ab", 32) // 32 bytes of 0xab
	if b, err := ParseSecretBoxKey(hexKey); err != nil || len(b) != 32 {
		t.Fatalf("hex key = (len %d, %v), want (32, nil)", len(b), err)
	}
}
