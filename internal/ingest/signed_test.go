package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// signStripe computes a real Stripe v1 signature: HMAC-SHA256(secret, "<t>.<body>").
func signStripe(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// signSlack computes a real Slack v0 signature: HMAC-SHA256(secret, "v0:<ts>:<body>").
func signSlack(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + strconv.FormatInt(ts, 10) + ":"))
	m.Write(body)
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifyStripe(t *testing.T) {
	const secret = "whsec_test_secret"
	body := []byte(`{"id":"evt_123","type":"invoice.paid"}`)
	now := time.Unix(1_700_000_000, 0)
	fresh := now.Unix() - 60   // inside the 300s window
	stale := now.Unix() - 400  // outside the window (past)
	future := now.Unix() + 400 // outside the window (future skew)
	tolerance := 300 * time.Second

	header := func(ts int64, sigs ...string) string {
		parts := []string{"t=" + strconv.FormatInt(ts, 10)}
		for _, s := range sigs {
			parts = append(parts, "v1="+s)
		}
		return strings.Join(parts, ",")
	}

	tests := []struct {
		name   string
		body   []byte
		header string
		want   bool
	}{
		{"valid fresh signature", body, header(fresh, signStripe(secret, fresh, body)), true},
		{"valid HMAC but stale timestamp", body, header(stale, signStripe(secret, stale, body)), false},
		{"valid HMAC but future timestamp", body, header(future, signStripe(secret, future, body)), false},
		{"wrong secret", body, header(fresh, signStripe("whsec_other", fresh, body)), false},
		{"tampered body", []byte(`{"id":"evt_666"}`), header(fresh, signStripe(secret, fresh, body)), false},
		{"timestamp not covered by HMAC", body, header(fresh+1, signStripe(secret, fresh, body)), false},
		{"rotation: second v1 matches", body, header(fresh, signStripe("whsec_old", fresh, body), signStripe(secret, fresh, body)), true},
		{"missing header", body, "", false},
		{"missing v1", body, "t=" + strconv.FormatInt(fresh, 10), false},
		{"missing t", body, "v1=" + signStripe(secret, fresh, body), false},
		{"non-numeric t", body, "t=notanumber,v1=" + signStripe(secret, fresh, body), false},
		{"bad hex v1", body, header(fresh, "zzzz"), false},
		{"garbage header", body, "sha256=deadbeef", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyStripe(secret, tc.body, tc.header, now, tolerance); got != tc.want {
				t.Fatalf("verifyStripe(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestVerifySlack(t *testing.T) {
	const secret = "slack_signing_secret"
	body := []byte(`{"type":"event_callback","event_id":"Ev123","event":{"type":"app_mention"}}`)
	now := time.Unix(1_700_000_000, 0)
	fresh := now.Unix() - 60
	stale := now.Unix() - 400
	future := now.Unix() + 400
	tolerance := 300 * time.Second
	tsStr := func(ts int64) string { return strconv.FormatInt(ts, 10) }

	tests := []struct {
		name      string
		body      []byte
		timestamp string
		sig       string
		want      bool
	}{
		{"valid fresh signature", body, tsStr(fresh), signSlack(secret, fresh, body), true},
		{"valid HMAC but stale timestamp", body, tsStr(stale), signSlack(secret, stale, body), false},
		{"valid HMAC but future timestamp", body, tsStr(future), signSlack(secret, future, body), false},
		{"wrong secret", body, tsStr(fresh), signSlack("other", fresh, body), false},
		{"tampered body", []byte(`{"evil":true}`), tsStr(fresh), signSlack(secret, fresh, body), false},
		{"timestamp not covered by HMAC", body, tsStr(fresh + 1), signSlack(secret, fresh, body), false},
		{"missing signature", body, tsStr(fresh), "", false},
		{"missing v0= prefix", body, tsStr(fresh), strings.TrimPrefix(signSlack(secret, fresh, body), "v0="), false},
		{"bad hex signature", body, tsStr(fresh), "v0=zzzz", false},
		{"missing timestamp", body, "", signSlack(secret, fresh, body), false},
		{"non-numeric timestamp", body, "notanumber", signSlack(secret, fresh, body), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifySlack(secret, tc.body, tc.timestamp, tc.sig, now, tolerance); got != tc.want {
				t.Fatalf("verifySlack(ts=%q, sig=%q) = %v, want %v", tc.timestamp, tc.sig, got, tc.want)
			}
		})
	}
}

// GitHub's scheme signs no timestamp, so an arbitrarily old delivery with a valid signature still
// verifies — no fabricated replay window. Governing: SPEC-0001 REQ "Replay-Window Enforcement for
// Timestamped Signatures" (GitHub scenario).
func TestVerifyGitHubHasNoReplayWindow(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"action":"opened"}`)
	// There is simply no timestamp input to verifyGitHub — assert the signature alone decides.
	if !verifyGitHub(secret, body, sign(secret, body)) {
		t.Fatal("valid GitHub signature must verify regardless of delivery age")
	}
}

// The rejection paths (503 unconfigured, 401 bad signature, 401 stale timestamp) must never touch
// the store — handlers are built with a nil store, so any persist attempt would panic.
// Governing: SPEC-0001 REQ "Signed Webhook Verification" (fail closed, no persist).
func TestSignedHandlersRejectWithoutPersisting(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const stripeSecret = "whsec_test"
	const slackSecret = "slack_test"
	body := `{"id":"evt_1","type":"invoice.paid"}`
	now := time.Unix(1_700_000_000, 0)
	fresh := now.Unix() - 10
	stale := now.Unix() - 4000

	newIngest := func(cfg Config) *Ingest {
		i := New(nil, nil, log, cfg)
		i.now = func() time.Time { return now }
		return i
	}

	tests := []struct {
		name     string
		cfg      Config
		handler  string // "stripe" or "slack"
		headers  map[string]string
		wantCode int
	}{
		{
			name: "stripe unconfigured secret is 503", cfg: Config{}, handler: "stripe",
			headers:  map[string]string{"Stripe-Signature": "t=1,v1=deadbeef"},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "stripe missing signature is 401", cfg: Config{StripeSecret: stripeSecret}, handler: "stripe",
			headers:  map[string]string{},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "stripe invalid signature is 401", cfg: Config{StripeSecret: stripeSecret}, handler: "stripe",
			headers:  map[string]string{"Stripe-Signature": "t=" + strconv.FormatInt(fresh, 10) + ",v1=deadbeef"},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "stripe valid HMAC outside replay window is 401", cfg: Config{StripeSecret: stripeSecret}, handler: "stripe",
			headers: map[string]string{
				"Stripe-Signature": "t=" + strconv.FormatInt(stale, 10) + ",v1=" + signStripe(stripeSecret, stale, []byte(body)),
			},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "slack unconfigured secret is 503", cfg: Config{}, handler: "slack",
			headers:  map[string]string{},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "slack missing signature is 401", cfg: Config{SlackSecret: slackSecret}, handler: "slack",
			headers:  map[string]string{"X-Slack-Request-Timestamp": strconv.FormatInt(fresh, 10)},
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "slack valid HMAC outside replay window is 401", cfg: Config{SlackSecret: slackSecret}, handler: "slack",
			headers: map[string]string{
				"X-Slack-Request-Timestamp": strconv.FormatInt(stale, 10),
				"X-Slack-Signature":         signSlack(slackSecret, stale, []byte(body)),
			},
			wantCode: http.StatusUnauthorized,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			i := newIngest(tc.cfg)
			req := httptest.NewRequest(http.MethodPost, "/webhooks/"+tc.handler, strings.NewReader(body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			switch tc.handler {
			case "stripe":
				i.Stripe(rec, req)
			case "slack":
				i.Slack(rec, req)
			}
			if rec.Code != tc.wantCode {
				t.Fatalf("got %d, want %d (body: %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

func TestParseStripeSignature(t *testing.T) {
	ts, sigs := parseStripeSignature("t=1700000000,v1=abc,v0=ignored,v1=def")
	if ts != 1700000000 {
		t.Fatalf("ts = %d, want 1700000000", ts)
	}
	if len(sigs) != 2 || sigs[0] != "abc" || sigs[1] != "def" {
		t.Fatalf("sigs = %v, want [abc def]", sigs)
	}
	if ts, sigs := parseStripeSignature("t=-5,v1=abc"); ts != 0 || sigs != nil {
		t.Fatalf("negative timestamp must be rejected, got ts=%d sigs=%v", ts, sigs)
	}
}
