// Signature Verifiers And Delivery Keys
//
// The pure verification and idempotency helpers the self-managed webhook path (selfmanaged.go) and
// the routing rules (routing.go) share: Stripe and Slack signature checks, the replay-window test,
// the body-hash fallback key, and the delivery id a generic sender may stamp on its own requests.
//
// Every function body below is MOVED verbatim from signed.go and generic.go, which held them
// alongside the operator-configured receivers (/webhooks/{github,gitea,stripe,slack,generic/*}).
// Those receivers are gone — instance-wide ingestion belongs to no tenant — but a self-managed signed
// webhook verifies exactly as they did, so the verifiers stay and only change address. Nothing here
// was re-authored: signature checking is not code to rewrite in passing. The one edit is prose —
// genericDeliveryID's comment described the shared endpoint that no longer exists.
//
// @joestump-agent 09/06/2026 - First salvage during the shared-receiver teardown (#181).
//
// @joestump 09/21/2026 - Replaced that salvage with a verbatim move from main. It had rewritten
// verifyStripe/verifySlack inline rather than relocating them, and predated genericDeliveryID
// (#285), which selfmanaged.go calls and which lived in the deleted generic.go.
//
// Governing: SPEC-0001 REQ "Replay-Window Enforcement for Timestamped Signatures", REQ
// "Idempotency Key Extraction and Dedup"; SPEC-0006 "Signed Webhook Verification"; ADR-0012.

package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// verifyStripe checks a `Stripe-Signature: t=<unix>,v1=<hex>[,v1=...]` header over the raw body.
// The HMAC-SHA256 is computed over `<t>.<body>` and compared constant-time against every v1
// candidate (Stripe sends several during secret rotation). The signed timestamp must be within
// tolerance of now — a valid HMAC with a stale timestamp is still rejected.
// Governing: SPEC-0001 REQ "Signed Webhook Verification", REQ "Replay-Window Enforcement for
// Timestamped Signatures".
func verifyStripe(secret string, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	ts, sigs := parseStripeSignature(header)
	if ts == 0 || len(sigs) == 0 {
		return false
	}
	if !freshTimestamp(ts, now, tolerance) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err != nil {
			continue
		}
		if hmac.Equal(expected, got) {
			return true
		}
	}
	return false
}

// parseStripeSignature splits `t=<unix>,v1=<hex>[,v1=...]` into the signed timestamp and the v1
// signature candidates. Unknown elements (v0=, future schemes) are ignored per Stripe's contract.
func parseStripeSignature(header string) (ts int64, sigs []string) {
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return 0, nil
			}
			ts = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	return ts, sigs
}

// verifySlack checks `X-Slack-Signature: v0=<hex>` over `v0:<timestamp>:<raw body>` in constant
// time. The signed X-Slack-Request-Timestamp must be within tolerance of now — a valid HMAC with
// a stale timestamp is still rejected.
// Governing: SPEC-0001 REQ "Signed Webhook Verification", REQ "Replay-Window Enforcement for
// Timestamped Signatures".
func verifySlack(secret string, body []byte, timestamp, sig string, now time.Time, tolerance time.Duration) bool {
	sigHex, ok := strings.CutPrefix(sig, "v0=")
	if !ok {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil || ts <= 0 {
		return false
	}
	if !freshTimestamp(ts, now, tolerance) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte(":"))
	mac.Write(body)
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	return hmac.Equal(mac.Sum(nil), got)
}

// freshTimestamp reports whether a signed unix timestamp is within tolerance of now, in either
// direction (stale replays and future clock skew both fail closed).
// Governing: SPEC-0001 REQ "Replay-Window Enforcement for Timestamped Signatures".
func freshTimestamp(ts int64, now time.Time, tolerance time.Duration) bool {
	d := now.Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}

// bodyHash derives the idempotency-key fallback for providers that supply no delivery id
// (SPEC-0001 REQ "Idempotency Key Extraction and Dedup").
func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// idempotencyKey derives the dedup key for an accepted delivery: the provider delivery id where one
// exists (GitHub X-GitHub-Delivery, Stripe event id, Slack event_id), with a sha256(body) fallback
// where the provider supplies none — a key is ALWAYS derived, so a redelivery can never bypass
// dedup with an empty key. Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup".
func idempotencyKey(deliveryID string, body []byte) string {
	if deliveryID != "" {
		return deliveryID
	}
	return bodyHash(body)
}

// Generic delivery ids. A generic sender has no signing scheme and so no signed delivery id, but a
// producer that retries can still stamp every attempt with the same id — the plain `X-Delivery-Id`,
// or the Standard Webhooks `Webhook-Id` — and a retry whose body differs (a fresh timestamp, a
// re-serialized payload) then collapses onto the original todo instead of minting a second one.
// Without it the body hash is the only key a generic sender can get, and every byte-different retry
// is a duplicate.
//
// The id is trusted exactly as much as the body it travels with: it is caller-asserted, and the
// key it feeds is scoped to the webhook it arrived on (selfmanaged.go prefixes wh.ID), so a caller
// can only ever collapse ITS OWN deliveries. An id over
// maxGenericDeliveryID bytes is ignored — the body hash applies — so a hostile sender cannot grow
// the dedup index with the header. Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup"
// (scenario "Generic redelivery with the same delivery id dedups").
//
// @justinabrahms 09/13/2026 - Added: a homelab producer retrying with a fresh timestamp in the
// body minted one todo per attempt; the forge sources already keyed on the provider's delivery id.
const maxGenericDeliveryID = 256

// genericDeliveryIDHeaders are consulted in order; the first non-empty value wins. Header names are
// canonicalized by net/http, so the Standard Webhooks lowercase spelling matches too.
var genericDeliveryIDHeaders = []string{"X-Delivery-Id", "Webhook-Id"}

// genericDeliveryID returns the delivery id a generic sender stamped on the request, or "" when it
// sent none (or one too long to be a key), in which case the caller falls back to the body hash.
func genericDeliveryID(h http.Header) string {
	for _, name := range genericDeliveryIDHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			if len(v) > maxGenericDeliveryID {
				return ""
			}
			return v
		}
	}
	return ""
}
