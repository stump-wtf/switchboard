package store

// Tests for the plaintext-signing-secret warning.
//
// These deliberately need NO database. Every DB-backed test in this package skips unless
// SWITCHBOARD_TEST_DATABASE_URL is set, and CI does not run Postgres (issue #141), so a test that
// could only observe this behaviour through a live write would report ok while asserting nothing —
// the exact failure mode the warning itself exists to fix.
//
// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency".
//
// @joestump-agent 09/12/2026 - Added alongside the warning.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// noopCipher is a SecretCipher that is present but does nothing; it stands for "encryption is
// configured" without pulling in the real AES envelope.
type noopCipher struct{}

func (noopCipher) Encrypt(plaintext string) (string, error) { return plaintext, nil }
func (noopCipher) Decrypt(stored string) (string, error)    { return stored, nil }

// captureLogs returns a logger writing JSON records into buf, so a test can assert that a record
// was emitted and inspect its level and message. The rest of this repo's test helpers send logs to
// io.Discard, which cannot answer "did it fire?" — the question this test exists to ask.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// records decodes the captured JSON log lines.
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestStoresPlaintextSigningSecret(t *testing.T) {
	cases := []struct {
		name      string
		cipher    SecretCipher
		trustMode string
		secret    string
		want      bool
	}{
		{"no cipher, signed, secret present", nil, "signed", "s3cret", true},
		{"cipher configured", noopCipher{}, "signed", "s3cret", false},
		{"token webhook stores no secret", nil, "token", "s3cret", false},
		{"open webhook stores no secret", nil, "open", "s3cret", false},
		{"signed but no secret supplied", nil, "signed", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storesPlaintextSigningSecret(tc.cipher, tc.trustMode, tc.secret); got != tc.want {
				t.Fatalf("storesPlaintextSigningSecret(%v, %q, secret) = %v, want %v",
					tc.cipher != nil, tc.trustMode, got, tc.want)
			}
		})
	}
}

// TestWarnPlaintextSigningSecretFires is the assertion that matters: the warning must actually be
// emitted, at WARN, naming the row — not merely compile.
func TestWarnPlaintextSigningSecretFires(t *testing.T) {
	log, buf := captureLogs()
	s := &Store{log: log}

	s.warnPlaintextSigningSecret("signed", "s3cret", "wh-1", "ep-1")

	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 log record, got %d: %s", len(recs), buf.String())
	}
	rec := recs[0]
	if lvl, _ := rec["level"].(string); lvl != "WARN" {
		t.Errorf("level = %q, want WARN", lvl)
	}
	if msg, _ := rec["msg"].(string); msg != "webhook signing secret stored without at-rest encryption" {
		t.Errorf("msg = %q, want the plaintext-secret warning", msg)
	}
	if wh, _ := rec["webhook"].(string); wh != "wh-1" {
		t.Errorf("webhook = %q, want wh-1", wh)
	}
	if ep, _ := rec["endpoint"].(string); ep != "ep-1" {
		t.Errorf("endpoint = %q, want ep-1", ep)
	}
	// The secret must never reach the log.
	if strings.Contains(buf.String(), "s3cret") {
		t.Fatal("the signing secret leaked into the log record")
	}
}

func TestWarnPlaintextSigningSecretStaysQuiet(t *testing.T) {
	cases := []struct {
		name      string
		store     func(*slog.Logger) *Store
		trustMode string
		secret    string
	}{
		{"cipher configured", func(l *slog.Logger) *Store {
			return &Store{log: l, secretCipher: noopCipher{}}
		}, "signed", "s3cret"},
		{"token webhook", func(l *slog.Logger) *Store { return &Store{log: l} }, "token", "s3cret"},
		{"no secret", func(l *slog.Logger) *Store { return &Store{log: l} }, "signed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLogs()
			tc.store(log).warnPlaintextSigningSecret(tc.trustMode, tc.secret, "wh-1", "ep-1")
			if n := len(records(t, buf)); n != 0 {
				t.Fatalf("expected no log records, got %d: %s", n, buf.String())
			}
		})
	}
}

// A store with no logger attached must not panic: WithLogger is optional.
func TestWarnPlaintextSigningSecretNilLogger(t *testing.T) {
	s := &Store{}
	s.warnPlaintextSigningSecret("signed", "s3cret", "wh-1", "ep-1")
}
