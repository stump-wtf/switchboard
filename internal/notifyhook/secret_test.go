package notifyhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Governing: SPEC-0024 REQ-4 "Signing (Standard Webhooks)", scenarios "A receiver verifies a
// notification" and "A stock Standard Webhooks library verifies it".

// The published test vector of the Standard Webhooks reference library
// (github.com/standard-webhooks/standard-webhooks, libraries/go/webhook_test.go): this secret, id,
// timestamp and payload sign to exactly this value. Matching it byte for byte is what makes every
// conforming verifier, including Harness's (stump.wtf/harness#466), accept Switchboard's hooks.
const (
	vectorSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" // gitleaks:allow (public spec test vector)
	vectorMsgID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTimestamp = int64(1614265330)
	vectorPayload   = `{"test": 2432232314}`
	vectorSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

// referenceVerify is an independent implementation of the Standard Webhooks verification
// algorithm, written from the spec rather than shared with Switchboard's signer: strip "whsec_",
// base64-decode the rest into the key, HMAC-SHA256 "id.timestamp.body", and accept if any
// space-separated "v1," entry matches. It deliberately uses the lenient decoder a stock library
// uses, so a wrongly encoded secret yields a wrong key instead of an error, as it would in the field.
func referenceVerify(secret, msgID, timestamp string, body []byte, header string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, entry := range strings.Fields(header) {
		version, sig, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// TestSignMatchesStandardWebhooksVector: Switchboard's signer reproduces the reference library's
// published signature byte for byte.
func TestSignMatchesStandardWebhooksVector(t *testing.T) {
	key, err := DecodeSecret(vectorSecret)
	if err != nil {
		t.Fatalf("decode vector secret: %v", err)
	}
	if got := Sign(key, vectorMsgID, vectorTimestamp, []byte(vectorPayload)); got != vectorSignature {
		t.Fatalf("Sign = %q, want the published vector %q", got, vectorSignature)
	}
	// Controls: the vector must also pass the independent verifier, and must fail on any change.
	ts := strconv.FormatInt(vectorTimestamp, 10)
	if !referenceVerify(vectorSecret, vectorMsgID, ts, []byte(vectorPayload), vectorSignature) {
		t.Fatal("reference verifier rejects the published vector: the verifier itself is wrong")
	}
	if referenceVerify(vectorSecret, vectorMsgID, ts, []byte(vectorPayload+" "), vectorSignature) {
		t.Fatal("reference verifier accepted a modified body: it verifies nothing")
	}
}

// TestMintedSecretVerifiesWithReference: a secret from the hook minter, used by an independent
// verifier with no Switchboard-specific handling, verifies a signature made with the key bytes the
// minter drew. A hex-encoding minter (the inbound mintWebhookSecret's format) fails the same check.
func TestMintedSecretVerifiesWithReference(t *testing.T) {
	raw := bytes.Repeat([]byte{0xA7, 0x3C, 0x5E, 0x91}, SecretBytes/4)
	secret, err := mintFrom(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	body := []byte(`{"type":"todo.ready","todo_id":"td_1"}`)
	const msgID, ts = "msg_01J8ZQ4Y6V3N2K7T5R9W0XABCD", "1790000000"
	tsN, _ := strconv.ParseInt(ts, 10, 64)

	// Switchboard signs with the key it drew; the receiver only ever sees the secret string.
	header := Sign(raw, msgID, tsN, body)
	if !referenceVerify(secret, msgID, ts, body, header) {
		t.Fatalf("a reference verifier rejected a signature under the minted secret %q", secret)
	}

	// Negative control: the same bytes presented the way the inbound minter presents them.
	hexSecret := "whsec_" + hex.EncodeToString(raw)
	if referenceVerify(hexSecret, msgID, ts, body, header) {
		t.Fatal("a hex-encoded secret verified: the test cannot tell the encodings apart")
	}
}

func TestMintSecretShape(t *testing.T) {
	shape := regexp.MustCompile(`^whsec_[A-Za-z0-9+/]{43}=$`)
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		s, err := MintSecret()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !shape.MatchString(s) {
			t.Fatalf("minted secret %q is not whsec_ + padded standard base64 of 32 bytes", s)
		}
		key, err := DecodeSecret(s)
		if err != nil || len(key) != SecretBytes {
			t.Fatalf("DecodeSecret(minted) = %d bytes, %v", len(key), err)
		}
		if seen[s] {
			t.Fatal("MintSecret repeated a secret")
		}
		seen[s] = true
	}
}

func TestDecodeSecretRejects(t *testing.T) {
	for _, s := range []string{
		"",
		"MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", // no prefix
		"whsec_",                           // empty key
		"whsec_not base64!",                // not base64
		"whsec_" + base64.RawStdEncoding.EncodeToString(make([]byte, 32)), // unpadded
		"whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 16)),    // too short
		"whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 65)),    // too long
	} {
		if _, err := DecodeSecret(s); !errors.Is(err, ErrMalformedSecret) {
			t.Errorf("DecodeSecret(%q) err = %v, want ErrMalformedSecret", s, err)
		}
	}
}

func TestMintFromShortReader(t *testing.T) {
	if _, err := mintFrom(bytes.NewReader(make([]byte, 5))); err == nil {
		t.Fatal("mintFrom accepted a short entropy read")
	}
}
