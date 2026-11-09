// Pre-intake reject attribution for the inbound webhook throttle
//
// Every refusal a RECEIVER makes is already counted (deliveryCount.verifyFailed). The one refusal
// no receiver sees is the throttle: the 429 is written by the limiter's middleware, before the
// request reaches a handler, so nothing on that path knows which webhook the delivery was
// addressed to — and a producer that does not retry a 429 loses the event silently, which is the
// whole complaint this answers (issue #612).
//
// The gap is closed with an in-memory index rather than a read: when the receiver resolves a path
// token it records the pair (hash of the token → webhook id) it already looked up, and the throttle
// hook consults that index. A throttled request therefore costs one SHA-256 and a map read, never a
// database query — the reject path is exactly the path that must not become expensive. The index is
// bounded and self-healing: it is repopulated by ordinary traffic, so the only window that
// attributes a 429 to no webhook is one where the source has not delivered successfully since this
// process started. Those refusals are still counted, under the metrics overflow bucket.
//
// The index keys on the SHA-256 of the ingest token, never the token: the URL is a credential, and
// keeping it in a long-lived map would put every live ingest URL in a heap dump.
//
// Governing: ADR-0051; SPEC-0001 REQ "Rate Limiting"; SPEC-0023 REQ-4 "Ingest and routing".
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// reasonRateLimited is the bounded reason a throttled delivery is counted under. It is the value
// metrics.RejectReasonRateLimited documents; this package does not import internal/metrics, so it
// is spelled out here (same convention as the verdict literals in ingest.go).
const reasonRateLimited = "rate_limited"

// selfManagedPathPrefix is the path the self-managed receiver is mounted on, with its trailing
// slash. The throttle rejects before chi resolves the route, so the token is read off the path.
const selfManagedPathPrefix = "/webhooks/w/"

// defaultTokenIndexCap bounds the attribution index. One entry is a token hash and a uuid: at the
// cap the index is a few hundred KB, and the webhook surface an instance holds is far below it.
// Past the cap the oldest entry is dropped, which costs attribution, never correctness.
const defaultTokenIndexCap = 4096

// tokenIndex is the bounded (token hash → webhook id) index. The zero value is not usable; New
// builds one.
type tokenIndex struct {
	mu     sync.Mutex
	byHash map[string]string
	// order is insertion order, used only to drop the oldest entry past the cap.
	order []string
	limit int
}

func newTokenIndex(limit int) *tokenIndex {
	if limit <= 0 {
		limit = defaultTokenIndexCap
	}
	return &tokenIndex{byHash: make(map[string]string), limit: limit}
}

// note records that hash addresses webhookID. Re-noting an existing hash refreshes the webhook it
// names (a rotated token is a new hash; the old one simply ages out), and a note past the cap
// evicts the oldest entry.
func (t *tokenIndex) note(hash, webhookID string) {
	if hash == "" || webhookID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byHash[hash]; ok {
		t.byHash[hash] = webhookID
		return
	}
	if len(t.byHash) >= t.limit {
		if len(t.order) > 0 {
			oldest := t.order[0]
			t.order = t.order[1:]
			delete(t.byHash, oldest)
		}
	}
	t.byHash[hash] = webhookID
	t.order = append(t.order, hash)
}

// lookup reports the webhook a token hash addresses, if this process has resolved it.
func (t *tokenIndex) lookup(hash string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.byHash[hash]
	return id, ok
}

// tokenHash is the index key for a raw ingest token.
func tokenHash(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// ingestTokenFromPath is the path token of a self-managed webhook delivery. Anything that is not
// exactly one path segment under the receiver's prefix (a probe, a scanner, a different route) has
// no token to name.
func ingestTokenFromPath(path string) string {
	token, ok := strings.CutPrefix(path, selfManagedPathPrefix)
	if !ok || token == "" || strings.Contains(token, "/") {
		return ""
	}
	return token
}

// noteIngestToken records the token a receiver just resolved, so a later throttle of the same URL
// can be attributed to the webhook it belongs to.
func (i *Ingest) noteIngestToken(rawToken, webhookID string) {
	if i == nil || i.tokenIndex == nil || rawToken == "" || webhookID == "" {
		return
	}
	i.tokenIndex.note(tokenHash(rawToken), webhookID)
}

// ObserveThrottled counts one delivery the inbound webhook throttle refused with 429 before any
// receiver ran. The server wires it as the webhook limiter's reject hook; it never blocks, never
// reads the database, and never touches the body.
func (i *Ingest) ObserveThrottled(r *http.Request) {
	if i == nil {
		return
	}
	webhookID := ""
	if r != nil && r.URL != nil && i.tokenIndex != nil {
		if token := ingestTokenFromPath(r.URL.Path); token != "" {
			webhookID, _ = i.tokenIndex.lookup(tokenHash(token))
		}
	}
	i.metricsOrNop().WebhookPreIntakeReject(webhookID, reasonRateLimited)
}
