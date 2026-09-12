package mcp

// Tests for the SPEC-0005 replay_webhook_event delivery path (replay.go): target resolution and the
// hard no-target error, the SSRF rejection table across every blocked range, the happy path against
// a live httptest consumer, downstream non-2xx reported (not raised), the DNS-rebinding dial guard,
// replay-safe header projection, and the per-endpoint replay rate limit.
//
// Governing: SPEC-0005 REQ "Replay Safety", "Redirect Validation" + "Rate Limiting" security reqs.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

// replaySession vends an all-verb endpoint against f and connects a client, returning the session.
// Unlike session() it takes the fakeStore already seeded with events/settings by the caller.
func replaySession(t *testing.T, ctx context.Context, f *fakeStore) *sdk.ClientSession {
	t.Helper()
	return session(t, ctx, f, []string{"reviews"}, eventVerbNames)
}

// seedEvent seeds one replayable event with the given payload and headers.
func seedEvent(f *fakeStore, id int64, payload string, headers []byte) {
	f.putEvent(store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{ID: id, Provider: "github", EventType: "push",
			TrustMode: "signed", Verified: true, PayloadSize: len(payload)},
		ContentType: "application/json",
		Headers:     headers,
		Payload:     []byte(payload),
	})
}

// TestReplayNoTargetNoDefaultIsError: neither an explicit target_url nor a configured
// replay_default_target is a hard invalid_argument, and no outbound request is attempted.
// Governing: SPEC-0005 scenario "No target and no default is an error, not a guess".
func TestReplayNoTargetNoDefaultIsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	seedEvent(f, 1, `{"zen":"go"}`, nil)
	cs := replaySession(t, ctx, f)

	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 1}, "invalid_argument")
}

// TestReplaySSRFRejectionTable: with no trusted default/allowlist, a target_url resolving to any
// blocked range (loopback, RFC 1918 private, link-local, cloud metadata, IPv6 loopback/ULA/
// link-local) is rejected with invalid_argument before any request; a non-http(s) scheme likewise.
// Governing: SPEC-0005 scenario "Non-http scheme is rejected before any request", "Redirect Validation".
func TestReplaySSRFRejectionTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	blocked := []struct {
		name, target string
	}{
		{"loopback v4", "http://127.0.0.1:8080/hook"},
		{"loopback v4 range", "http://127.9.9.9/hook"},
		{"private 10/8", "http://10.0.0.5/hook"},
		{"private 172.16/12", "http://172.16.4.4/hook"},
		{"private 192.168/16", "http://192.168.1.10/hook"},
		{"link-local", "http://169.254.10.10/hook"},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"unspecified", "http://0.0.0.0/hook"},
		{"ipv6 loopback", "http://[::1]:9000/hook"},
		{"ipv6 ula", "http://[fc00::1]/hook"},
		{"ipv6 link-local", "http://[fe80::1]/hook"},
		{"non-http scheme file", "file:///etc/passwd"},
		{"non-http scheme gopher", "gopher://127.0.0.1/1"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStore()
			seedEvent(f, 1, `{}`, nil)
			cs := replaySession(t, ctx, f)
			callErr(t, ctx, cs, "replay_webhook_event",
				map[string]any{"id": 1, "target_url": tc.target}, "invalid_argument")
		})
	}
}

// TestReplayHappyPathDefaultTarget: a configured replay_default_target (trusted, so exempt from the
// loopback blocklist) is used when target_url is omitted; the stored payload is delivered to a live
// consumer and the response reports id, resolved target_url, delivered, response_status, response_ms.
// Governing: SPEC-0005 REQ "Replay Safety".
func TestReplayHappyPathDefaultTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var gotBody atomic.Value
	var gotCT atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotCT.Store(r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusAccepted) // 202
	}))
	t.Cleanup(ts.Close)

	f := newFakeStore()
	f.settings[settingReplayDefaultTarget] = ts.URL + "/consume"
	seedEvent(f, 42, `{"action":"opened"}`, []byte(`{"Content-Type":"application/json","X-GitHub-Event":"pull_request"}`))
	cs := replaySession(t, ctx, f)

	var out struct {
		ID             int64  `json:"id"`
		TargetURL      string `json:"target_url"`
		Delivered      bool   `json:"delivered"`
		ResponseStatus *int   `json:"response_status"`
		ResponseMs     int64  `json:"response_ms"`
	}
	callOK(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 42}, &out)

	if out.ID != 42 {
		t.Fatalf("id = %d, want 42", out.ID)
	}
	if out.TargetURL != ts.URL+"/consume" {
		t.Fatalf("target_url = %q, want the configured default", out.TargetURL)
	}
	if !out.Delivered {
		t.Fatalf("delivered = false, want true")
	}
	if out.ResponseStatus == nil || *out.ResponseStatus != http.StatusAccepted {
		t.Fatalf("response_status = %v, want 202", out.ResponseStatus)
	}
	if out.ResponseMs < 0 {
		t.Fatalf("response_ms = %d, want >= 0", out.ResponseMs)
	}
	if b, _ := gotBody.Load().(string); b != `{"action":"opened"}` {
		t.Fatalf("downstream body = %q, want the stored raw payload", b)
	}
	if ct, _ := gotCT.Load().(string); ct != "application/json" {
		t.Fatalf("downstream Content-Type = %q, want the replay-safe stored header", ct)
	}
}

// TestReplayExplicitAllowlistedTarget: an explicit target_url that is not the default but is on the
// operator allowlist (replay_allowed_targets) is trusted and delivered even though it is loopback.
func TestReplayExplicitAllowlistedTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	host := ts.Listener.Addr().String() // 127.0.0.1:port
	f := newFakeStore()
	f.settings[settingReplayAllowedTargets] = "example.test, " + host
	seedEvent(f, 7, `{}`, nil)
	cs := replaySession(t, ctx, f)

	var out struct {
		Delivered      bool `json:"delivered"`
		ResponseStatus *int `json:"response_status"`
	}
	callOK(t, ctx, cs, "replay_webhook_event",
		map[string]any{"id": 7, "target_url": ts.URL + "/x"}, &out)
	if !out.Delivered || out.ResponseStatus == nil || *out.ResponseStatus != 200 {
		t.Fatalf("allowlisted replay = %+v, want delivered 200", out)
	}
}

// TestReplayDownstreamNon2xxReported: a downstream non-2xx is reported as delivered:true with the
// status, not raised as replay_failed (which is reserved for transport failures).
// Governing: SPEC-0005 scenario "Downstream non-2xx is reported, not raised".
func TestReplayDownstreamNon2xxReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500
	}))
	t.Cleanup(ts.Close)

	f := newFakeStore()
	f.settings[settingReplayDefaultTarget] = ts.URL
	seedEvent(f, 5, `{}`, nil)
	cs := replaySession(t, ctx, f)

	var out struct {
		Delivered      bool `json:"delivered"`
		ResponseStatus *int `json:"response_status"`
	}
	callOK(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 5}, &out)
	if !out.Delivered {
		t.Fatalf("delivered = false, want true even on downstream 500")
	}
	if out.ResponseStatus == nil || *out.ResponseStatus != http.StatusInternalServerError {
		t.Fatalf("response_status = %v, want 500", out.ResponseStatus)
	}
}

// TestReplayTransportFailureRaisesReplayFailed: a target that accepts nothing (connection refused)
// is a transport failure — replay_failed, with no leaked internal detail.
// Governing: SPEC-0005 REQ "Stable Error Shape" (replay_failed reserved for transport failures).
func TestReplayTransportFailureRaisesReplayFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Bind then immediately close a listener to obtain a port nothing is listening on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := l.Addr().String()
	_ = l.Close()

	f := newFakeStore()
	f.settings[settingReplayDefaultTarget] = "http://" + dead + "/x" // trusted → passes SSRF, fails to connect
	seedEvent(f, 9, `{}`, nil)
	cs := replaySession(t, ctx, f)

	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 9}, "replay_failed")
}

// TestReplayUnknownIdIsNotFound: an unknown event id resolves to not_found before any outbound work.
// Governing: SPEC-0005 scenario "Unknown id raises not_found".
func TestReplayUnknownIdIsNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.settings[settingReplayDefaultTarget] = "http://192.0.2.1/x"
	cs := replaySession(t, ctx, f)

	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 999}, "not_found")
}

// TestReplayRateLimited: once the per-endpoint replay budget is exhausted, further replays return
// the stable rate_limited code. The budget is shrunk for the test via the handler's replayRL.
// Governing: SPEC-0005 "Rate Limiting" security requirement.
func TestReplayRateLimited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	f := newFakeStore()
	f.settings[settingReplayDefaultTarget] = ts.URL
	seedEvent(f, 1, `{}`, nil)

	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, eventVerbNames)
	h := New(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(h.Close)
	// A tiny bucket: 2 immediate replays, then throttled (refill rate negligible for the test).
	h.replayRL = newRateLimiter(0.0001, 2)
	srv := httptest.NewServer(routesFor(h))
	t.Cleanup(srv.Close)
	cs, err := connect(t, ctx, srv.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	var out struct {
		Delivered bool `json:"delivered"`
	}
	callOK(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 1}, &out)
	callOK(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 1}, &out)
	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 1}, "rate_limited")
}

// TestGuardedDialRefusesBlockedIP is the DNS-rebinding second-layer guard: even bypassing pre-flight
// validation, the guarded dialer refuses to connect to a blocked (loopback) address at dial time.
// This is what catches a hostname that passed pre-flight as public but rebinds to a private IP.
func TestGuardedDialRefusesBlockedIP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	// Untrusted client (guarded dialer) dialing the loopback httptest server must be refused at
	// connect time — the dial guard, not pre-flight, is what fails here.
	client := replayClient(false)
	req, err := http.NewRequest(http.MethodPost, ts.URL, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := client.Do(req); err == nil {
		t.Fatal("guarded dial to loopback succeeded, want refusal")
	}

	// A trusted client (blocklist bypassed) reaches the same loopback server.
	if resp, err := replayClient(true).Do(mustReq(t, ts.URL)); err != nil {
		t.Fatalf("trusted dial to loopback failed: %v", err)
	} else {
		_ = resp.Body.Close()
	}
}

func mustReq(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// TestReplaySafeHeadersDropsSecretsAndHopByHop: the replay-safe header projection drops hop-by-hop
// and credential headers and any header still carrying the ingest redaction sentinel, while keeping
// benign metadata headers.
func TestReplaySafeHeadersDropsSecretsAndHopByHop(t *testing.T) {
	in := map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": redactionMarker, // redacted secret → dropped
		"Authorization":       "Bearer sekret", // credential → dropped
		"Cookie":              "sid=abc",       // credential → dropped
		"Connection":          "keep-alive",    // hop-by-hop → dropped
		"Host":                "evil.example",  // set by client → dropped
	}
	out := replaySafeHeaders(in)
	if out.Get("Content-Type") != "application/json" || out.Get("X-GitHub-Event") != "push" {
		t.Fatalf("benign headers dropped: %v", out)
	}
	for _, drop := range []string{"X-Hub-Signature-256", "Authorization", "Cookie", "Connection", "Host"} {
		if out.Get(drop) != "" {
			t.Fatalf("header %q was forwarded, want dropped", drop)
		}
	}
}

// TestIsBlockedIPTable is a direct unit table over the SSRF classifier.
func TestIsBlockedIPTable(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"169.254.169.254", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"fc00::1", true},
		{"fe80::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false}, // just outside 172.16/12
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		if got := isBlockedIP(net.ParseIP(c.ip)); got != c.blocked {
			t.Errorf("isBlockedIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}
