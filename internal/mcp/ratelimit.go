package mcp

import (
	"sync"
	"time"
)

// Governing: SPEC-0014 REQ "Rate Limiting". The same dependency-free token bucket the /agent
// surface uses (internal/server/ratelimit.go), keyed here by endpoint slug rather than client IP:
// the abuse unit on this surface is a (possibly leaked) vended credential, not an address. The
// durable queue is the source of truth, so throttling only bounds abuse; it never drops work.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens refilled per second
	burst   float64 // bucket capacity
	last    time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// newRateLimiter builds a limiter allowing `burst` requests immediately and refilling at `rps`/sec.
func newRateLimiter(rps, burst float64) *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, rate: rps, burst: burst}
}

// allowAt reports whether a request under key may proceed at time now, consuming one token when it
// does. The clock is injected so the behaviour is deterministically testable.
func (rl *rateLimiter) allowAt(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.prune(now)
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: rl.burst}
		rl.buckets[key] = b
	}
	if !b.seen.IsZero() {
		b.tokens += now.Sub(b.seen).Seconds() * rl.rate
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
	}
	b.seen = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (rl *rateLimiter) allow(key string) bool { return rl.allowAt(key, time.Now()) }

// prune evicts buckets idle for more than a minute, at most once per minute, so the map stays
// bounded by recently-active endpoints. Caller holds rl.mu.
func (rl *rateLimiter) prune(now time.Time) {
	if now.Sub(rl.last) < time.Minute {
		return
	}
	rl.last = now
	for k, b := range rl.buckets {
		if now.Sub(b.seen) > time.Minute {
			delete(rl.buckets, k)
		}
	}
}
