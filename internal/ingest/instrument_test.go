package ingest

// Received-lane instrumentation contract (SPEC-0015 REQ "Patch Panel Board"): the receivers report
// arrival BEFORE verification and rejection at the verification boundary, with REDACTED facts only
// (provider, event type, trust mode, idempotency key, client-safe reason) — never the body,
// headers, signature, or token. These run store-less: the rejection paths return before any
// persist, so a nil store doubles as proof that nothing was stored (SPEC-0001 unchanged).

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureInstrument records instrument callbacks in order.
type captureInstrument struct {
	calls []instrumentCall
}

type instrumentCall struct {
	kind, provider, eventType, trust, key, reason string
}

func (c *captureInstrument) DeliveryReceived(provider, eventType, trust, key string) {
	c.calls = append(c.calls, instrumentCall{kind: "received", provider: provider, eventType: eventType, trust: trust, key: key})
}

func (c *captureInstrument) DeliveryRejected(provider, eventType, trust, key, reason string) {
	c.calls = append(c.calls, instrumentCall{kind: "rejected", provider: provider, eventType: eventType, trust: trust, key: key, reason: reason})
}

func (c *captureInstrument) DeliveryDeduped(provider, eventType, trust, key string) {
	c.calls = append(c.calls, instrumentCall{kind: "deduped", provider: provider, eventType: eventType, trust: trust, key: key})
}

// TestGitHubRejectionInstrumentsReceivedThenRejected: a signed delivery reports arrival before
// verification, and a bad signature resolves it with the client-safe reason. The nil store proves
// the payload was never persisted, and the redacted facts never include the signature or body.
func TestGitHubRejectionInstrumentsReceivedThenRejected(t *testing.T) {
	ins := &captureInstrument{}
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{GitHubSecret: "s3cr3t"})
	i.SetInstrument(ins)

	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "gh-d-77")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	i.GitHub(rec, req)

	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(ins.calls) != 2 {
		t.Fatalf("instrument calls = %+v, want received then rejected", ins.calls)
	}
	want := instrumentCall{kind: "received", provider: "github", eventType: "pull_request", trust: "signed", key: "gh-d-77"}
	if ins.calls[0] != want {
		t.Errorf("arrival call = %+v, want %+v", ins.calls[0], want)
	}
	rej := ins.calls[1]
	if rej.kind != "rejected" || rej.reason != "signature verification failed" || rej.key != "gh-d-77" {
		t.Errorf("rejection call = %+v", rej)
	}
	// Redaction: no reported fact carries the presented signature or the body.
	for _, c := range ins.calls {
		for _, field := range []string{c.provider, c.eventType, c.trust, c.key, c.reason} {
			if strings.Contains(field, "deadbeef") || strings.Contains(field, "opened") {
				t.Errorf("instrument leaked request material: %+v", c)
			}
		}
	}
}

// TestGitHubUnconfiguredInstrumentsProviderUnavailable: the pre-verification rejections
// (unconfigured/disabled provider) also resolve the in-flight card, without comparing anything.
func TestGitHubUnconfiguredInstrumentsProviderUnavailable(t *testing.T) {
	ins := &captureInstrument{}
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	i.SetInstrument(ins)

	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	i.GitHub(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if len(ins.calls) != 2 || ins.calls[0].kind != "received" || ins.calls[1].kind != "rejected" {
		t.Fatalf("instrument calls = %+v, want received then rejected", ins.calls)
	}
	if ins.calls[1].reason != "provider unavailable" {
		t.Errorf("reason = %q, want provider unavailable", ins.calls[1].reason)
	}
}

// TestGenericTokenRejectionInstruments: a configured token provider reports arrival (trust mode
// token) and resolves a wrong token with the client-safe reason; the token value itself never
// reaches the instrument.
func TestGenericTokenRejectionInstruments(t *testing.T) {
	ins := &captureInstrument{}
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		Generic: map[string]GenericProvider{"dockerhub": {Mode: "token", Token: "sekret"}},
	})
	i.SetInstrument(ins)

	req := genericRequest("dockerhub", `{"push":true}`, map[string]string{"X-Webhook-Token": "wrong"}, "")
	rec := httptest.NewRecorder()
	i.Generic(rec, req)

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(ins.calls) != 2 || ins.calls[0].kind != "received" || ins.calls[1].kind != "rejected" {
		t.Fatalf("instrument calls = %+v, want received then rejected", ins.calls)
	}
	if ins.calls[0].trust != "token" || ins.calls[1].reason != "invalid token" {
		t.Errorf("calls = %+v", ins.calls)
	}
	for _, c := range ins.calls {
		for _, field := range []string{c.provider, c.eventType, c.trust, c.key, c.reason} {
			if strings.Contains(field, "sekret") || strings.Contains(field, "wrong") {
				t.Errorf("instrument leaked token material: %+v", c)
			}
		}
	}
}

// TestUnknownGenericProviderNeverRingsTheBoard: an unconfigured provider name is a 404 probe, not
// a line — no instrument call fires, so scan noise cannot spam the received lane.
func TestUnknownGenericProviderNeverRingsTheBoard(t *testing.T) {
	ins := &captureInstrument{}
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	i.SetInstrument(ins)

	req := genericRequest("ghost", `{}`, nil, "")
	rec := httptest.NewRecorder()
	i.Generic(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(ins.calls) != 0 {
		t.Fatalf("unknown provider must not instrument, got %+v", ins.calls)
	}
}

// TestNilInstrumentIsSafe: receivers run identically with no instrument wired (the default).
func TestNilInstrumentIsSafe(t *testing.T) {
	i := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{GitHubSecret: "s3cr3t"})
	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	i.GitHub(rec, req) // must not panic
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
