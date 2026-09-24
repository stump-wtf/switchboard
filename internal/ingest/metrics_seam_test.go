package ingest

// Ingest metrics seam tests
//
// The seam is what Story 4's increments call unconditionally, so its contract is pinned here: unset
// is a no-op, SetMetrics routes to the sink, nil clears it, and a concurrent SetMetrics never races
// an in-flight increment. This file deliberately does not import internal/metrics (see metrics.go).
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"sync"
	"sync/atomic"
	"testing"
)

type countingMetrics struct{ deliveries, failures, decisions atomic.Int64 }

func (c *countingMetrics) WebhookDelivery(string, string, string) { c.deliveries.Add(1) }
func (c *countingMetrics) WebhookVerifyFailure(string, string)    { c.failures.Add(1) }
func (c *countingMetrics) RoutingDecision(string, string, string) { c.decisions.Add(1) }
func (c *countingMetrics) RoutingFault(string)                    {}

func TestIngestMetricsSeamUnsetIsNoOp(t *testing.T) {
	var i Ingest // the zero value must be safe too: some tests build Ingest by hand
	m := i.metricsOrNop()
	if m == nil {
		t.Fatal("metricsOrNop returned nil; call sites increment unconditionally")
	}
	m.WebhookDelivery("github", "signed", "accepted")
	m.WebhookVerifyFailure("github", "bad_signature")
	m.RoutingDecision("wh", "", "queue")
}

func TestIngestMetricsSeamRoutesAndClears(t *testing.T) {
	i := &Ingest{}
	c := &countingMetrics{}
	i.SetMetrics(c)
	i.metricsOrNop().WebhookDelivery("github", "signed", "rejected")
	i.metricsOrNop().WebhookVerifyFailure("github", "bad_signature")
	i.metricsOrNop().RoutingDecision("wh", "", "drop")
	if c.deliveries.Load() != 1 || c.failures.Load() != 1 || c.decisions.Load() != 1 {
		t.Fatalf("sink saw deliveries=%d failures=%d decisions=%d, want 1 each",
			c.deliveries.Load(), c.failures.Load(), c.decisions.Load())
	}
	i.SetMetrics(nil)
	i.metricsOrNop().WebhookDelivery("github", "signed", "accepted")
	if c.deliveries.Load() != 1 {
		t.Fatal("SetMetrics(nil) did not clear the sink")
	}
}

// TestIngestMetricsSeamConcurrentSet: run with -race.
func TestIngestMetricsSeamConcurrentSet(t *testing.T) {
	i := &Ingest{}
	c := &countingMetrics{}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for range 200 {
				if g == 0 {
					i.SetMetrics(c)
				}
				i.metricsOrNop().RoutingDecision("wh", "", "queue")
			}
		}(g)
	}
	wg.Wait()
}
