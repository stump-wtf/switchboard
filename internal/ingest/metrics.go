package ingest

// Ingest and routing metrics seam
//
// The receivers are the only code that knows a delivery's verdict, its verification failure mode,
// and the routing decision it produced, so the SPEC-0023 REQ-4 counters are incremented here. Like
// the store, this package declares the narrow interface it needs rather than importing
// internal/metrics, so the dependency points one way. Do not import internal/metrics from this
// package: pass the documented string literals instead.
//
// This is separate from the received-lane Instrument on purpose: that one is presentation-only,
// fed client-safe free text, and may be nil in production; these are bounded counters.
//
// Governing: SPEC-0023 REQ-4 "Ingest and routing", design.md "Shape" (counters inline); ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

// Metrics receives the ingest and routing counters. *metrics.Metrics satisfies it. Implementations
// must be cheap and non-blocking and must bound their own label values.
type Metrics interface {
	// WebhookDelivery counts one delivery; verdict is "accepted" (verified and persisted,
	// idempotency dedups included), "rejected" (failed verification or refused) or "dropped" (a
	// routing decision of drop).
	WebhookDelivery(provider, trustMode, verdict string)
	// WebhookVerifyFailure counts one failed verification; reason is a bounded failure mode such as
	// bad_signature, missing_signature, stale_timestamp, bad_token, too_large or malformed — never
	// the client-safe free-text message.
	WebhookVerifyFailure(provider, reason string)
	// RoutingDecision counts one routing decision, drops included. webhookID is the server-minted
	// webhook id; ruleID is the matched rule's id, or "" when none matched (reported as "default");
	// action is "queue" or "drop".
	RoutingDecision(webhookID, ruleID, action string)
}

// SetMetrics registers the ingest metrics sink. Safe to call concurrently with the receivers;
// passing nil clears it, and an unset sink makes every increment a no-op.
func (i *Ingest) SetMetrics(m Metrics) {
	if m == nil {
		i.metricsSink.Store(nil)
		return
	}
	i.metricsSink.Store(&m)
}

// metricsOrNop returns the registered sink, or a no-op when none is set, so call sites increment
// unconditionally: i.metricsOrNop().WebhookDelivery(wh.SourceType, wh.TrustMode, "accepted").
func (i *Ingest) metricsOrNop() Metrics {
	if m := i.metricsSink.Load(); m != nil {
		return *m
	}
	return nopMetrics{}
}

// nopMetrics is the unset sink.
type nopMetrics struct{}

func (nopMetrics) WebhookDelivery(string, string, string) {}
func (nopMetrics) WebhookVerifyFailure(string, string)    {}
func (nopMetrics) RoutingDecision(string, string, string) {}
