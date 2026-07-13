package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/joestump/switchboard/internal/auth"
)

// Governing: SPEC-0006 REQ webhook self-management rate ceiling, SPEC-0009 persona card. A small
// dependency-free token-bucket limiter keyed by client IP. The durable queue is the source of truth,
// so throttling only bounds abuse; it never drops accepted work.
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

// allow reports whether a request from key may proceed, consuming one token when it does. The clock
// is injected so the behaviour is deterministically testable.
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

// prune evicts buckets idle for more than a minute, at most once per minute, so the map can't grow
// unboundedly with unique client IPs. Caller holds rl.mu.
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

// middleware throttles by client IP, answering 429 with a Retry-After when the bucket is empty.
func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(rateKey(r)) {
			rl.reject(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// postMiddleware is the shared human-surface mutation throttle: it meters ONLY state-changing
// POSTs (vend/revoke/persona/friend/todo actions and every other authenticated form), keyed by
// the authenticated human so one operator's burst can neither starve nor be hidden behind
// another's. GETs — including the long-lived SSE stream at /events, which carries its own
// per-session cap — pass untouched. Mounted after auth.RequireHuman, so the principal is always
// in context in production; the client-IP fallback only covers mis-mounting. An empty bucket
// answers 429 with a Retry-After, matching the limiter's other surfaces.
// Governing: SPEC-0013 "Rate Limiting" ("State-changing POST routes SHOULD share the
// human-surface rate limiter; the SSE endpoint MUST cap concurrent streams per session").
func (rl *rateLimiter) postMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r) // read path (incl. SSE): exempt by design
			return
		}
		key := rateKey(r)
		if h, ok := auth.FromContext(r.Context()); ok {
			key = h.ID
		}
		if !rl.allow(key) {
			rl.reject(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// reject answers an empty bucket: 429 plus a Retry-After derived from the refill rate, so
// well-behaved clients (and HTMX retries) know when a token will exist again.
func (rl *rateLimiter) reject(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(1/rl.rate)+1))
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// rateKey is the throttle key: the client IP (host portion of RemoteAddr).
func rateKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
