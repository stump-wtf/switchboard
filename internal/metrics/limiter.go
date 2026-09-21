package metrics

// Label cardinality limiter
//
// queue and webhook are the two label dimensions whose values the service does not control: queue
// names are operator-chosen, and webhook ids are server-minted but unbounded across the fleet. A
// labelLimiter admits the first N distinct values it is shown and every later value maps to
// "__other__". Admission is sticky for the life of the process, so an admitted value keeps its own
// series forever and never flaps into and out of the overflow bucket as traffic shifts — a series
// that disappears and reappears reads as a gap, which is exactly the misreading REQ-6 exists to
// prevent.
//
// One instance per dimension is shared by every metric that carries that label. The queue limiter
// in particular is shared between the lifecycle counters (REQ-3) and the scrape-time liveness
// gauges (REQ-2), so a queue carries the same label value in every family and a PromQL join across
// them lines up.
//
// Governing: SPEC-0023 REQ-5 "Cardinality", ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"sync"
	"unicode/utf8"
)

// Other is the overflow label value: every value past a limiter's cap, and every value that fails
// label validation, is reported under it.
const Other = "__other__"

// DefaultLabelCap is the number of distinct values a limiter admits when Options leaves the cap
// unset. SPEC-0023 design.md "Cardinality control" names 50 for queues; webhooks use the same.
const DefaultLabelCap = 50

// maxLimitedValueBytes bounds a single admitted label value. Queue names are operator free text
// with no length rule in the store, and a label value is repeated in every series that carries it,
// so an absurdly long one is folded into the overflow bucket rather than inflating every scrape.
const maxLimitedValueBytes = 128

// labelLimiter is a sticky, concurrency-safe admission set. The zero value is not usable; build one
// with newLabelLimiter.
type labelLimiter struct {
	limit    int
	mu       sync.RWMutex
	admitted map[string]struct{}
}

func newLabelLimiter(limit int) *labelLimiter {
	if limit <= 0 {
		limit = DefaultLabelCap
	}
	return &labelLimiter{limit: limit, admitted: make(map[string]struct{}, limit)}
}

// label returns v when v is (or can now be) admitted, and Other otherwise. Values that could never
// be a safe label — empty, the overflow sentinel itself, invalid UTF-8 (the client library panics
// on it), or longer than maxLimitedValueBytes — map to Other without consuming a slot.
func (l *labelLimiter) label(v string) string {
	if v == "" || v == Other || len(v) > maxLimitedValueBytes || !utf8.ValidString(v) {
		return Other
	}
	// Fast path: the steady state is every call hitting an already-admitted value, or a full set.
	l.mu.RLock()
	_, ok := l.admitted[v]
	full := len(l.admitted) >= l.limit
	l.mu.RUnlock()
	if ok {
		return v
	}
	if full {
		return Other
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-check under the write lock: another goroutine may have admitted v, or filled the last
	// slot, between the two locks.
	if _, ok := l.admitted[v]; ok {
		return v
	}
	if len(l.admitted) >= l.limit {
		return Other
	}
	l.admitted[v] = struct{}{}
	return v
}
