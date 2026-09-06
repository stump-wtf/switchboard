package ingest

// Received-lane instrumentation contract (SPEC-0015 REQ "Patch Panel Board"), exercised through the
// self-managed receiver — the only delivery surface left after the shared receivers were stripped
// (issue #181): the receiver reports arrival BEFORE verification and rejection at the verification
// boundary, with REDACTED facts only (provider, event type, trust mode, idempotency key,
// client-safe reason) — never the body, headers, signature, or token.
//
// The rejection path persists nothing, but the token lookup must resolve for a delivery to reach
// the verification boundary at all, so the rejection case runs DB-gated like the rest of the
// accept-path suite.

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/store"
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

// TestSelfManagedRejectionInstrumentsReceivedThenRejected: a signed delivery reports arrival before
// verification, and a bad signature resolves it with the client-safe reason. The redacted facts
// never include the presented signature or the body.
func TestSelfManagedRejectionInstrumentsReceivedThenRejected(t *testing.T) {
	ins := &captureInstrument{}
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	seedWebhook(t, st, ctx, "github", "signed", "reviews", "inst-reject", "whsec_inst")
	ing.SetInstrument(ins)

	body := `{"action":"opened"}`
	rec := postSelfManaged(ing, "inst-reject", body, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "gh-d-77",
		"X-Hub-Signature-256": "sha256=deadbeef",
	})

	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(ins.calls) != 2 {
		t.Fatalf("instrument calls = %+v, want received then rejected", ins.calls)
	}
	// The key is webhook-id-scoped (wh.ID + ":" + delivery id), not the bare delivery id.
	if ins.calls[0].kind != "received" || ins.calls[0].provider != "github" || ins.calls[0].eventType != "webhook" || ins.calls[0].trust != "signed" || !strings.HasSuffix(ins.calls[0].key, ":gh-d-77") {
		t.Errorf("arrival call = %+v, want received/webhook/signed keyed on the scoped delivery id", ins.calls[0])
	}
	rej := ins.calls[1]
	if rej.kind != "rejected" || rej.reason != "signature verification failed" || !strings.HasSuffix(rej.key, ":gh-d-77") {
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

// TestSelfManagedDedupedInstrumentsDedupedLane: a redelivery resolves the in-flight card with the
// deduped lane, keyed by the same idempotency key as the first delivery.
func TestSelfManagedDedupedInstrumentsDedupedLane(t *testing.T) {
	ins := &captureInstrument{}
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	seedWebhook(t, st, ctx, "github", "signed", "reviews", "inst-dedup", "whsec_inst")
	ing.SetInstrument(ins)

	body := `{"action":"opened","pull_request":{"number":1}}`
	hdr := map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "gh-d-1",
		"X-Hub-Signature-256": githubSig("whsec_inst", body),
	}
	if rec := postSelfManaged(ing, "inst-dedup", body, hdr); rec.Code != 202 {
		t.Fatalf("first delivery: got %d, want 202", rec.Code)
	}
	if rec := postSelfManaged(ing, "inst-dedup", body, hdr); rec.Code != 202 {
		t.Fatalf("redelivery: got %d, want 202", rec.Code)
	}

	var deduped []instrumentCall
	for _, c := range ins.calls {
		if c.kind == "deduped" {
			deduped = append(deduped, c)
		}
	}
	if len(deduped) != 1 {
		t.Fatalf("deduped calls = %+v, want exactly one", deduped)
	}
	if deduped[0].key == "" || deduped[0].provider != "github" || deduped[0].trust != "signed" {
		t.Errorf("deduped call = %+v", deduped[0])
	}
}

// TestNilInstrumentIsSafe: the receiver runs identically with no instrument wired (the default) —
// the oversize path fires before anything else and must not panic on the nil instrument.
func TestNilInstrumentIsSafe(t *testing.T) {
	ing := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	rec := postSelfManaged(ing, "any-token", strings.Repeat("x", maxBody+1), nil)
	if rec.Code != 413 {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}
