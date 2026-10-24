package metrics

// Pre-intake reject ledger tests
//
// Pins the family the endpoint card and /metrics both read (ADR-0051, issue #612): counts are
// attributed to the webhook a refusal was addressed to, an unidentified refusal lands in the
// overflow bucket instead of inventing a webhook, a webhook past the cardinality cap reports no
// per-webhook number rather than the overflow bucket's, and the exposition and the card read the
// same counter.
//
// Governing: SPEC-0023 REQ-4 "Ingest and routing", REQ-5 "Cardinality", REQ-6 "Honest absence".

import (
	"testing"
)

func TestPreIntakeRejectCountsPerWebhook(t *testing.T) {
	m := New(Options{})
	m.WebhookPreIntakeReject("wh-1", RejectReasonRateLimited)
	m.WebhookPreIntakeReject("wh-1", "bad_signature")
	m.WebhookPreIntakeReject("wh-2", RejectReasonRateLimited)

	got := m.PreIntakeRejects("wh-1")
	if !got.Tracked {
		t.Fatal("an admitted webhook must report its own numbers")
	}
	if got.Total != 2 {
		t.Errorf("total = %d, want 2", got.Total)
	}
	if got.ByReason[RejectReasonRateLimited] != 1 || got.ByReason["bad_signature"] != 1 {
		t.Errorf("byReason = %v, want one rate_limited and one bad_signature", got.ByReason)
	}
	if other := m.PreIntakeRejects("wh-2"); other.Total != 1 || other.ByReason["bad_signature"] != 0 {
		t.Errorf("a second webhook's count leaked: %+v", other)
	}

	for _, want := range []struct {
		reason string
		value  float64
	}{
		{RejectReasonRateLimited, 1},
		{"bad_signature", 1},
	} {
		v, ok := value(t, m, "switchboard_webhook_preintake_rejects_total",
			map[string]string{"webhook": "wh-1", "reason": want.reason})
		if !ok || v != want.value {
			t.Errorf("exposition wh-1/%s = %v (present %v), want %v", want.reason, v, ok, want.value)
		}
	}
}

func TestPreIntakeRejectUnattributedGoesToOverflow(t *testing.T) {
	m := New(Options{})
	m.WebhookPreIntakeReject("", RejectReasonRateLimited)
	if got := m.PreIntakeRejects(""); got.Tracked || got.Total != 0 {
		t.Errorf("no webhook was identified, but a card number came back: %+v", got)
	}
	if v, ok := value(t, m, "switchboard_webhook_preintake_rejects_total",
		map[string]string{"webhook": Other, "reason": RejectReasonRateLimited}); !ok || v != 1 {
		t.Errorf("overflow series = %v (present %v), want 1", v, ok)
	}
}

func TestPreIntakeRejectOverflowWebhookReportsNothing(t *testing.T) {
	m := New(Options{WebhookCap: 1})
	m.WebhookPreIntakeReject("wh-admitted", RejectReasonRateLimited)
	m.WebhookPreIntakeReject("wh-folded", RejectReasonRateLimited)

	if got := m.PreIntakeRejects("wh-folded"); got.Tracked || got.Total != 0 {
		t.Errorf("a webhook folded into __other__ must quote no number, got %+v", got)
	}
	if got := m.PreIntakeRejects("wh-admitted"); !got.Tracked || got.Total != 1 {
		t.Errorf("the admitted webhook lost its own count: %+v", got)
	}
	// Reading a card must not spend a cardinality slot: a webhook nobody ever rejected here stays
	// out of the limiter, so real traffic keeps its series.
	if got := m.PreIntakeRejects("wh-never-seen"); got.Tracked {
		t.Error("reading a card admitted a webhook into the limiter, displacing a counted one")
	}
	m.WebhookPreIntakeReject("wh-never-seen", RejectReasonRateLimited)
	// Both folded webhooks are in the one overflow series: that shared number is exactly why the
	// card quotes none of it for either of them.
	if v, ok := value(t, m, "switchboard_webhook_preintake_rejects_total",
		map[string]string{"webhook": Other, "reason": RejectReasonRateLimited}); !ok || v != 2 {
		t.Errorf("overflow series = %v (present %v), want 2", v, ok)
	}
}

func TestPreIntakeRejectReasonCoercion(t *testing.T) {
	m := New(Options{})
	m.WebhookPreIntakeReject("wh-1", "HMAC mismatch for secret abc123")
	if _, ok := value(t, m, "switchboard_webhook_preintake_rejects_total",
		map[string]string{"webhook": "wh-1", "reason": Other}); !ok {
		t.Error("free-text reason minted its own series instead of folding into __other__")
	}
}

func TestPreIntakeRejectAbsentUntilFirstRefusal(t *testing.T) {
	m := New(Options{})
	for _, s := range gather(t, m) {
		if s.name == "switchboard_webhook_preintake_rejects_total" {
			t.Fatal("the family reported a series before anything was refused")
		}
	}
}

func TestPreIntakeRejectNilReceiverIsSafe(t *testing.T) {
	var m *Metrics
	m.WebhookPreIntakeReject("wh-1", RejectReasonRateLimited)
	if got := m.PreIntakeRejects("wh-1"); got.Tracked || got.Total != 0 {
		t.Errorf("nil receiver returned numbers: %+v", got)
	}
}
