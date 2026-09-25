package store

// Lifecycle metrics seam
//
// The store is where every todo transition commits, so it is where the SPEC-0023 REQ-3 lifecycle
// counters are incremented: after the commit, next to the fireTodoHook calls, never before — a
// counter must not count a transition the database rolled back. The store declares the narrow
// interface it needs rather than importing internal/metrics, which keeps the dependency pointing
// one way (the metrics package's scrape-time collectors read the store, never the reverse). Do not
// import internal/metrics from this package: pass the documented string literals instead.
//
// This is deliberately its own slot, not a second subscriber multiplexed through
// SetTodoTransitionHook: that hook is single-slot and owned by the web layer's SSE fan-out.
//
// Governing: SPEC-0023 REQ-3 "Lifecycle counters", design.md "Shape" (counters inline); ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

// Metrics receives the todo lifecycle counters. *metrics.Metrics satisfies it. Implementations must
// be cheap and non-blocking — they run on the transition's own goroutine — and must bound their own
// label values; the store passes queue names as they are.
type Metrics interface {
	// TodoCreated counts one genuinely new todo row (never an idempotency dedup). source is a
	// bounded origin such as the webhook source type or push/api/dev.
	TodoCreated(queue, source string)
	// TodoClaimed counts one successful claim; attempt is the todo's attempt number after it.
	TodoClaimed(queue string, attempt int)
	// TodoFinished counts one claimant completion; outcome is "complete" or "fail".
	TodoFinished(queue, outcome string)
	// LeaseExpired counts one lapsed lease, reaped or taken over by a new claimant.
	LeaseExpired(queue string)
	// QuarantineHeld counts one delivery newly held in quarantine (never a redelivery collapsing
	// onto its item); reason is untrusted_actor, rule_fault or rule_action. Governing: SPEC-0026
	// REQ-11.
	QuarantineHeld(reason string)
	// QuarantineResolved counts one held todo leaving quarantine: outcome is released, discarded or
	// expired, and by is the resolver as recorded on the row ("human:<id>", "classifier:<slug>" or
	// "system"). The sink folds by onto its kind; the store passes it as recorded.
	QuarantineResolved(outcome, by string)
}

// SetMetrics registers the lifecycle metrics sink. Safe to call concurrently with store use;
// passing nil clears it, and an unset sink makes every increment a no-op.
func (s *Store) SetMetrics(m Metrics) {
	if m == nil {
		s.metricsSink.Store(nil)
		return
	}
	s.metricsSink.Store(&m)
}

// metricsOrNop returns the registered sink, or a no-op when none is set, so call sites increment
// unconditionally: s.metricsOrNop().TodoClaimed(t.Queue, t.Attempt).
func (s *Store) metricsOrNop() Metrics {
	if m := s.metricsSink.Load(); m != nil {
		return *m
	}
	return nopMetrics{}
}

// nopMetrics is the unset sink.
type nopMetrics struct{}

func (nopMetrics) TodoCreated(string, string)        {}
func (nopMetrics) TodoClaimed(string, int)           {}
func (nopMetrics) TodoFinished(string, string)       {}
func (nopMetrics) LeaseExpired(string)               {}
func (nopMetrics) QuarantineHeld(string)             {}
func (nopMetrics) QuarantineResolved(string, string) {}
