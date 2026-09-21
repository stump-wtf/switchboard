package metrics

// The /metrics scrape handler
//
// /metrics is an operator surface, not an agent one, and it carries operational data: which
// queues exist, how much work sits on each, who is falling behind. So it authenticates with its
// own credential class — a dedicated scrape token presented as a bearer — and deliberately shares
// nothing with the vended-endpoint grant model, OAuth, or the human session. A vended endpoint's
// token authorizes queue work; fleet-wide counts are not queue work.
//
// The comparison hashes both sides to SHA-256 first and compares the digests in constant time, so
// neither the token's content nor its length leaks through timing. Every failure is the same
// 401 with an empty body: no metric names, no hint of which queues exist (queue names are
// operator-chosen and can be descriptive). With no token configured the endpoint is closed, not
// open — every request is a 401.
//
// The exposition is pinned to the Prometheus text format (text/plain; version=0.0.4): the request's
// Accept header is not consulted, so neither OpenMetrics nor protobuf is ever negotiated and every
// scraper reads the same bytes.
//
// Governing: SPEC-0023 REQ-1 "The endpoint"; design.md "Auth"; ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// maxScrapesInFlight bounds concurrent authenticated scrapes. A scrape runs the queue-liveness
// aggregate against PostgreSQL, so an authenticated client (or a misconfigured HA pair) looping
// scrapes must queue behind a small constant rather than fan out onto the database. promhttp
// answers the excess with 503.
const maxScrapesInFlight = 4

// Handler returns the /metrics handler guarded by token. An empty token — or a nil receiver —
// yields a handler that rejects every request: the endpoint is closed by default, never open.
func (m *Metrics) Handler(token string) http.Handler {
	if m == nil || token == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { unauthorized(w) })
	}
	want := sha256.Sum256([]byte(token))
	expose := promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		// One broken collector must not blank the whole scrape: serve every family that could be
		// gathered. The collector itself reports its failure through CollectionError (REQ-6).
		ErrorHandling:       promhttp.ContinueOnError,
		ErrorLog:            slogPrintln{m},
		MaxRequestsInFlight: maxScrapesInFlight,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r, want) {
			unauthorized(w)
			return
		}
		pinned := r.Clone(r.Context())
		pinned.Header.Del("Accept")
		w.Header().Set("Cache-Control", "no-store")
		expose.ServeHTTP(w, pinned)
	})
}

// bearerMatches reports whether r presents exactly the scrape token as a bearer credential. Only the
// Authorization header is read; any other scheme, a missing header, or an empty credential fails.
func bearerMatches(r *http.Request, want [sha256.Size]byte) bool {
	scheme, raw, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	got := sha256.Sum256([]byte(raw))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// unauthorized is the single rejection shape: 401, a bearer challenge, and no body at all.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard-metrics"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// slogPrintln adapts the metrics logger to promhttp's Println-shaped error log.
type slogPrintln struct{ m *Metrics }

func (l slogPrintln) Println(v ...any) {
	if l.m.log != nil {
		l.m.log.Warn("metrics exposition", "err", strings.TrimSpace(fmt.Sprintln(v...)))
	}
}
