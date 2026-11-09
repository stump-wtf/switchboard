package ingest

// Throttle Reject Attribution Tests
//
// A 429 from the inbound webhook throttle is the one refusal no receiver runs, so the only thing
// that can attribute it to a source is the pairing the receiver recorded when it last resolved that
// URL (rejects.go, ADR-0051). These tests pin that pairing: the index is bounded, the path token is
// read without a route match, a resolved URL attributes its throttled deliveries, and anything the
// index has never resolved is counted with no webhook rather than dropped. No database: nothing on
// this path reads one.

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/store"
)

// The reason literal this package passes must be the one the metrics side reads back off the
// ledger for the card. ingest never imports internal/metrics in production code, so the check lives
// here, where it already imports both.
func TestThrottleReasonMatchesMetricsContract(t *testing.T) {
	if reasonRateLimited != metrics.RejectReasonRateLimited {
		t.Errorf("reasonRateLimited = %q, want %q", reasonRateLimited, metrics.RejectReasonRateLimited)
	}
}

func TestIngestTokenFromPath(t *testing.T) {
	for _, tc := range []struct {
		path, want string
	}{
		{"/webhooks/w/tok_1a2b", "tok_1a2b"},
		{"/webhooks/w/", ""},
		{"/webhooks/w/tok/extra", ""},
		{"/webhooks/w", ""},
		{"/mcp/abc", ""},
	} {
		if got := ingestTokenFromPath(tc.path); got != tc.want {
			t.Errorf("ingestTokenFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestTokenIndexAttributesResolvedURLs(t *testing.T) {
	rec := &recordingMetrics{}
	ing := &Ingest{tokenIndex: newTokenIndex(8)}
	ing.SetMetrics(rec)

	// The receiver resolves the token on an ordinary delivery; that is what teaches the pairing.
	ing.noteIngestToken("tok_live", "wh-1")

	ing.ObserveThrottled(httptest.NewRequest("POST", "/webhooks/w/tok_live", nil))
	ing.ObserveThrottled(httptest.NewRequest("POST", "/webhooks/w/tok-never-seen", nil))
	ing.ObserveThrottled(httptest.NewRequest("POST", "/dev/todos", nil))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	want := []recordedReject{
		{"wh-1", reasonRateLimited},
		{"", reasonRateLimited},
		{"", reasonRateLimited},
	}
	if len(rec.preIntake) != len(want) {
		t.Fatalf("recorded %v, want %v", rec.preIntake, want)
	}
	for i := range want {
		if rec.preIntake[i] != want[i] {
			t.Errorf("reject %d = %+v, want %+v", i, rec.preIntake[i], want[i])
		}
	}
}

// A hand-built Ingest (several suites do this) must not panic on the throttle hook.
func TestObserveThrottledWithoutIndexIsSafe(t *testing.T) {
	rec := &recordingMetrics{}
	ing := &Ingest{}
	ing.SetMetrics(rec)
	ing.ObserveThrottled(httptest.NewRequest("POST", "/webhooks/w/tok_x", nil))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.preIntake) != 1 || rec.preIntake[0].webhookID != "" || rec.preIntake[0].reason != reasonRateLimited {
		t.Fatalf("an unattributable throttle must still be counted, got %v", rec.preIntake)
	}
	var nilIngest *Ingest
	nilIngest.ObserveThrottled(httptest.NewRequest("POST", "/webhooks/w/tok_x", nil))
}

func TestTokenIndexIsBounded(t *testing.T) {
	idx := newTokenIndex(2)
	idx.note(tokenHash("a"), "wh-a")
	idx.note(tokenHash("b"), "wh-b")
	idx.note(tokenHash("c"), "wh-c")
	if _, ok := idx.lookup(tokenHash("a")); ok {
		t.Error("the oldest entry survived past the cap: the index is unbounded")
	}
	for _, tok := range []string{"b", "c"} {
		if id, ok := idx.lookup(tokenHash(tok)); !ok || id != "wh-"+tok {
			t.Errorf("lookup(%q) = %q, %v; want its own webhook", tok, id, ok)
		}
	}
	// Re-noting an address refreshes the webhook it names, and neither evicts nor duplicates.
	idx.note(tokenHash("c"), "wh-c2")
	if id, _ := idx.lookup(tokenHash("c")); id != "wh-c2" {
		t.Errorf("re-note left %q", id)
	}
	idx.note("", "wh-x")
	idx.note(tokenHash("d"), "")
}

// A refusal the receiver itself makes is a pre-intake reject too, and it must carry the webhook the
// delivery resolved to — otherwise the card can say only that something was refused, not where.
func TestVerifyFailureAttributesWebhook(t *testing.T) {
	rec := &recordingMetrics{}

	unresolved := newDeliveryCount()
	unresolved.verifyFailed(rec, reasonUnknownWebhook)

	resolved := newDeliveryCount()
	resolved.resolved(store.Webhook{ID: "wh-9", SourceType: "gitea", TrustMode: "signed"})
	resolved.verifyFailed(rec, reasonBadSignature)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	want := []recordedReject{{"", reasonUnknownWebhook}, {"wh-9", reasonBadSignature}}
	if fmt.Sprint(rec.preIntake) != fmt.Sprint(want) {
		t.Errorf("pre-intake rejects = %v, want %v", rec.preIntake, want)
	}
	if len(rec.failures) != 2 || rec.failures[1] != (recordedFailure{"gitea", reasonBadSignature}) {
		t.Errorf("verify-failure counter lost its provider attribution: %v", rec.failures)
	}
}
