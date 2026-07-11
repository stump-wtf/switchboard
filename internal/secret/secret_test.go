package secret

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return k
}

// Round-trip: Encrypt then Decrypt returns the original plaintext, and the ciphertext does not
// contain the plaintext (the at-rest bytes are opaque).
func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := New(newKey(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	pt := []byte("whsec_roundtripsecret")
	box, err := c.Encrypt(pt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Contains(box, pt) {
		t.Fatalf("ciphertext contains plaintext — not encrypted")
	}
	got, err := c.Decrypt(box)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip = %q, want %q", got, pt)
	}
}

// Two Encrypts of the same plaintext produce different boxes (fresh random nonce each time).
func TestEncryptUsesFreshNonce(t *testing.T) {
	c, _ := New(newKey(t))
	a, _ := c.Encrypt([]byte("same"))
	b, _ := c.Encrypt([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatalf("two seals of the same plaintext are identical — nonce reuse")
	}
}

// GCM authenticates: a box sealed under one key does not open under another, and a flipped byte
// fails closed rather than returning garbage.
func TestDecryptWrongKeyAndTamperFailClosed(t *testing.T) {
	c1, _ := New(newKey(t))
	c2, _ := New(newKey(t))
	box, _ := c1.Encrypt([]byte("secret"))

	if _, err := c2.Decrypt(box); err == nil {
		t.Fatalf("decrypt with wrong key succeeded, want error")
	}
	tampered := bytes.Clone(box)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := c1.Decrypt(tampered); err == nil {
		t.Fatalf("decrypt of tampered ciphertext succeeded, want error")
	}
	if _, err := c1.Decrypt([]byte("x")); err == nil {
		t.Fatalf("decrypt of too-short box succeeded, want error")
	}
}

func TestNewRejectsBadKeyLength(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatalf("New accepted a 16-byte key, want error")
	}
}

// Parse accepts a 32-byte key in hex or base64 and round-trips through it; an empty value is
// ErrNoKey and a malformed value is a hard error.
func TestParse(t *testing.T) {
	raw := newKey(t)
	pt := []byte("whsec_parsed")

	for name, encoded := range map[string]string{
		"hex":       hex.EncodeToString(raw),
		"base64std": base64.StdEncoding.EncodeToString(raw),
		"base64raw": base64.RawStdEncoding.EncodeToString(raw),
		"base64url": base64.URLEncoding.EncodeToString(raw),
	} {
		c, err := Parse(encoded)
		if err != nil {
			t.Fatalf("Parse(%s): %v", name, err)
		}
		box, err := c.Encrypt(pt)
		if err != nil {
			t.Fatalf("Parse(%s) encrypt: %v", name, err)
		}
		// Decrypt with a Cipher built from the same raw key — proves Parse recovered the exact key.
		ref, _ := New(raw)
		got, err := ref.Decrypt(box)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("Parse(%s) key mismatch: got %q err %v", name, got, err)
		}
	}

	if _, err := Parse(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Parse(empty) = %v, want ErrNoKey", err)
	}
	if _, err := Parse("not-a-valid-key"); err == nil {
		t.Fatalf("Parse(malformed) succeeded, want error")
	}
	// A well-formed encoding of the wrong length is rejected (16 bytes, not 32).
	if _, err := Parse(hex.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatalf("Parse(16-byte key) succeeded, want error")
	}
}
