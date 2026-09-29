// Package notifyhook holds SPEC-0024's outbound notify hooks: the Standard Webhooks secret format
// and signature primitive here, and (in later stories) the delivery dispatcher.
//
// The secret format is an interop trap, so it is spelled out once, here. Standard Webhooks secrets
// are "whsec_" followed by the STANDARD, PADDED base64 of the key bytes, and the HMAC key is the
// decoded bytes, never the literal string. Switchboard's inbound webhook minter
// (internal/mcp.mintWebhookSecret) returns "whsec_" followed by HEX, and hex is a valid base64
// alphabet: a conforming verifier decodes a hex secret without error, derives a different key, and
// rejects every delivery, silently on both sides. Notify hooks therefore never reuse that minter.
//
// Governing: ADR-0029, SPEC-0024 REQ-4 "Signing (Standard Webhooks)"; design.md "Standard Webhooks,
// with dual signatures on rotation".
package notifyhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// SecretPrefix marks a Standard Webhooks symmetric secret.
	SecretPrefix = "whsec_"
	// SecretBytes is the minted key length: 32 CSPRNG bytes (SPEC-0024 REQ-4).
	SecretBytes = 32
	// SignatureVersion is the Standard Webhooks symmetric-signature scheme tag.
	SignatureVersion = "v1"
)

// ErrMalformedSecret is returned by DecodeSecret for a value that is not "whsec_" plus padded
// standard base64 of 24 to 64 bytes, the Standard Webhooks key-size range.
var ErrMalformedSecret = errors.New("notifyhook: malformed signing secret")

// MintSecret returns a fresh hook signing secret, "whsec_" + padded standard base64 of 32 bytes
// from crypto/rand. Callers never supply their own secret (SPEC-0024 REQ-4).
func MintSecret() (string, error) {
	return mintFrom(rand.Reader)
}

// mintFrom is MintSecret over an injectable byte source, so a test can pin the key bytes and prove
// the encoding round-trips through an independent verifier.
func mintFrom(r io.Reader) (string, error) {
	key := make([]byte, SecretBytes)
	if _, err := io.ReadFull(r, key); err != nil {
		return "", fmt.Errorf("notifyhook: mint secret: %w", err)
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(key), nil
}

// DecodeSecret returns the HMAC key a secret denotes: the base64-decoded bytes after "whsec_". It is
// strict (padded standard base64, 24 to 64 bytes) because a lenient decoder is exactly what lets a
// wrong encoding produce a wrong key without an error.
func DecodeSecret(secret string) ([]byte, error) {
	enc, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok {
		return nil, ErrMalformedSecret
	}
	key, err := base64.StdEncoding.Strict().DecodeString(enc)
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, ErrMalformedSecret
	}
	return key, nil
}

// Sign returns one Standard Webhooks signature entry, "v1,<base64 HMAC-SHA256>", over
// "<msgID>.<timestamp>.<body>" keyed with the decoded secret key. The body is the exact raw bytes
// sent; any re-serialisation between signing and sending breaks every receiver.
func Sign(key []byte, msgID string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID))
	mac.Write([]byte{'.'})
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return SignatureVersion + "," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
