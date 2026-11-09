package metrics

// Pre-intake reject ledger
//
// A delivery that never reaches intake leaves no row anywhere: the rejection doctrine forbids
// storing a refused payload, and the senders that lose work this way — a 429 to a producer that
// does not retry, a signature that fails verification — do not come back to say they were refused.
// So the loss is invisible twice over: nothing in the queue, nothing in the database. This ledger
// is the visible half: one in-process counter per (webhook, bounded reason) for every delivery
// refused before intake, read by the Prometheus exposition and by the webhook row on the endpoint
// card, so an operator can see WHICH source is leaking and roughly how much.
//
// It is deliberately not persisted. A rejected delivery has no row to hang a count on, and adding a
// writes-on-reject table would put a database write on the one path that exists to shed load, and
// would store metadata about payloads the doctrine says switchboard must not keep. The cost is
// honest and stated on the surface that shows it: the counts begin at zero when the process starts.
//
// Cardinality follows the routing-decision family: webhook ids pass through the shared sticky
// webhook limiter, so a webhook past the cap folds into the overflow bucket and the card says
// nothing about it rather than quoting another webhook's number (REQ-5). Reasons pass through
// `token`, so a caller that names a reason outside the bounded alphabet lands in the overflow
// bucket instead of minting a series.
//
// Governing: SPEC-0023 REQ-4 "Ingest and routing", REQ-5 "Cardinality"; ADR-0051; issue #612.

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// RejectReasonRateLimited is the reason label of a delivery the inbound webhook throttle refused
// with 429 before any receiver ran. Every other reason the ledger carries is one of the bounded
// verification failure modes the receivers already name (bad_signature, too_large, …).
const RejectReasonRateLimited = "rate_limited"

// PreIntakeRejectTotals is one webhook's pre-intake reject counts since this process started.
type PreIntakeRejectTotals struct {
	// Tracked reports whether these numbers belong to this webhook: false when the webhook folded
	// into the cardinality overflow bucket, where no per-webhook number can be honest.
	Tracked bool
	Total   uint64
	// ByReason is the same total split by bounded reason; empty when nothing has been refused.
	ByReason map[string]uint64
}

// rejectLedger holds the counts. Keys are already label-limited webhook values, so the map is
// bounded by the webhook cap plus the overflow bucket.
type rejectLedger struct {
	mu     sync.Mutex
	counts map[string]map[string]uint64
}

func newRejectLedger() *rejectLedger {
	return &rejectLedger{counts: make(map[string]map[string]uint64)}
}

func (l *rejectLedger) add(webhookLabel, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	byReason := l.counts[webhookLabel]
	if byReason == nil {
		byReason = make(map[string]uint64)
		l.counts[webhookLabel] = byReason
	}
	byReason[reason]++
}

func (l *rejectLedger) totals(webhookLabel string) map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.counts[webhookLabel]))
	for reason, n := range l.counts[webhookLabel] {
		out[reason] = n
	}
	return out
}

func (l *rejectLedger) snapshot() map[string]map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]map[string]uint64, len(l.counts))
	for webhook, byReason := range l.counts {
		cp := make(map[string]uint64, len(byReason))
		for reason, n := range byReason {
			cp[reason] = n
		}
		out[webhook] = cp
	}
	return out
}

// rejectCollector exposes the ledger as a counter family at scrape time, the same way the queue
// liveness gauges read their source at scrape time: one source of truth, so the card and /metrics
// can never disagree. The family is absent until the first refusal, which is the honest reading —
// a source that has never rejected a delivery has no number to report (SPEC-0023 REQ-6).
type rejectCollector struct {
	ledger *rejectLedger
	desc   *prometheus.Desc
}

func newRejectCollector(ledger *rejectLedger) *rejectCollector {
	return &rejectCollector{ledger: ledger, desc: prometheus.NewDesc(
		"switchboard_webhook_preintake_rejects_total",
		"Deliveries refused before intake, by webhook and bounded reason; counted in-process, so a restart resets them.",
		[]string{"webhook", "reason"}, nil,
	)}
}

func (c *rejectCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *rejectCollector) Collect(ch chan<- prometheus.Metric) {
	for webhook, byReason := range c.ledger.snapshot() {
		for reason, n := range byReason {
			ch <- prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue, float64(n), webhook, reason)
		}
	}
}

// WebhookPreIntakeReject counts one delivery refused before intake. webhookID is the server-minted
// webhook id the delivery was addressed to, or "" when the refusal happened before any webhook was
// identified (an unknown path token, a throttle that never reached a receiver); both report under
// the overflow bucket. reason is a bounded failure mode, never the client-safe free text.
func (m *Metrics) WebhookPreIntakeReject(webhookID, reason string) {
	if m == nil {
		return
	}
	m.preIntakeRejects.add(m.webhookLabel(webhookID), token(reason))
}

// PreIntakeRejects reads one webhook's totals for the endpoint card. A webhook folded into the
// overflow bucket reports Tracked false: its refusals are counted, but under a label that names
// many webhooks, so quoting them on one card would be a lie.
func (m *Metrics) PreIntakeRejects(webhookID string) PreIntakeRejectTotals {
	if m == nil || !m.webhooks.admits(webhookID) {
		return PreIntakeRejectTotals{}
	}
	byReason := m.preIntakeRejects.totals(webhookID)
	out := PreIntakeRejectTotals{Tracked: true, ByReason: byReason}
	for _, n := range byReason {
		out.Total += n
	}
	return out
}
