// Stripe and Slack signed webhook receivers: HMAC-SHA256 over the raw body plus a replay window
// over the signed timestamp. Both fail closed — a missing, malformed, stale, or non-matching
// signature is a 401 and nothing is persisted (only a redacted rejection line is logged).
//
// Governing: ADR-0003 (per-provider trust model), SPEC-0001 REQ "Signed Webhook Verification",
// SPEC-0001 REQ "Replay-Window Enforcement for Timestamped Signatures".
package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// Stripe is the signed Stripe webhook receiver: POST /webhooks/stripe.
//
// Stripe signs `<t>.<raw body>` with HMAC-SHA256 and presents it as
// `Stripe-Signature: t=<unix>,v1=<hex>[,v1=<hex>...]` (multiple v1 entries during secret
// rotation). The signed `t=` timestamp is additionally checked against the replay window.
func (i *Ingest) Stripe(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if i.stripeSecret == "" {
		// Governing: SPEC-0001 scenario "Signature secret not configured" — reject without
		// comparing any signature.
		http.Error(w, "stripe adapter not configured", http.StatusServiceUnavailable)
		return
	}
	if !verifyStripe(i.stripeSecret, body, r.Header.Get("Stripe-Signature"), i.now(), i.tolerance) {
		// Reject without persisting; log a redacted line (never the signature value or secret).
		i.log.Warn("stripe signature rejected", "remote", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "signature verification failed")
		return
	}

	var p struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &p)
	// Idempotency key from the Stripe event id; body-hash fallback if absent (SPEC-0001 REQ
	// "Idempotency Key Extraction and Dedup").
	key := p.ID
	if key == "" {
		key = bodyHash(body)
	}
	// Governing: SPEC-0002/0004 REQ atomic ingestion — event + todo commit in one transaction.
	_, td, created, err := i.store.CreateEventTodo(r.Context(),
		store.EventInput{
			Source: "stripe", Family: "webhook", EventType: p.Type, ExternalID: key,
			TrustMode: "signed", Verified: true, VerifyDetail: "hmac-sha256 ok",
			ContentType: r.Header.Get("Content-Type"), Headers: sanitizeHeaders(r.Header),
			Payload: body, SourceIP: clientIP(r),
		},
		store.CreateTodoParams{
			Queue: i.stripeQueue, Source: "stripe", Kind: p.Type, Title: summarizeStripe(p.Type),
			Payload: body, IdempotencyKey: key,
		})
	if err != nil {
		i.log.Error("ingest stripe delivery", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if created {
		i.hub.Publish(td)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": td.ID, "queue": td.Queue, "verified": true})
}

// Slack is the signed Slack webhook receiver: POST /webhooks/slack.
//
// Slack signs `v0:<timestamp>:<raw body>` with HMAC-SHA256 and presents it as
// `X-Slack-Signature: v0=<hex>` alongside `X-Slack-Request-Timestamp`. The timestamp is part of
// the signed material and is additionally checked against the replay window.
func (i *Ingest) Slack(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if i.slackSecret == "" {
		// Governing: SPEC-0001 scenario "Signature secret not configured".
		http.Error(w, "slack adapter not configured", http.StatusServiceUnavailable)
		return
	}
	if !verifySlack(i.slackSecret, body, r.Header.Get("X-Slack-Request-Timestamp"),
		r.Header.Get("X-Slack-Signature"), i.now(), i.tolerance) {
		i.log.Warn("slack signature rejected", "remote", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "signature verification failed")
		return
	}

	var p struct {
		Type    string `json:"type"`
		EventID string `json:"event_id"`
		Event   struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	_ = json.Unmarshal(body, &p)
	eventType := p.Type
	if p.Event.Type != "" {
		eventType = p.Event.Type
	}
	// Slack supplies an event_id on event callbacks; fall back to a body hash otherwise
	// (SPEC-0001 REQ "Idempotency Key Extraction and Dedup" — body-hash fallback for Slack).
	key := p.EventID
	if key == "" {
		key = bodyHash(body)
	}
	// Governing: SPEC-0002/0004 REQ atomic ingestion — event + todo commit in one transaction.
	_, td, created, err := i.store.CreateEventTodo(r.Context(),
		store.EventInput{
			Source: "slack", Family: "webhook", EventType: eventType, ExternalID: key,
			TrustMode: "signed", Verified: true, VerifyDetail: "hmac-sha256 ok",
			ContentType: r.Header.Get("Content-Type"), Headers: sanitizeHeaders(r.Header),
			Payload: body, SourceIP: clientIP(r),
		},
		store.CreateTodoParams{
			Queue: i.slackQueue, Source: "slack", Kind: eventType, Title: summarizeSlack(eventType),
			Payload: body, IdempotencyKey: key,
		})
	if err != nil {
		i.log.Error("ingest slack delivery", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if created {
		i.hub.Publish(td)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": td.ID, "queue": td.Queue, "verified": true})
}

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

// summarizeStripe builds a one-line, legible todo title from a Stripe event type.
func summarizeStripe(eventType string) string {
	if eventType == "" {
		return "stripe event"
	}
	return "stripe " + eventType
}

// summarizeSlack builds a one-line, legible todo title from a Slack event type.
func summarizeSlack(eventType string) string {
	if eventType == "" {
		return "slack event"
	}
	return "slack " + eventType
}
