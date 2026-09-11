// Routing stage and the cairn signed source for self-managed deliveries.
//
// A verified delivery is routed after its idempotency key is derived and its fan-out targets are
// resolved, and before anything is persisted: the webhook's jq rules pick a queue (optionally
// narrowed to a subset of the targets) or drop, and the decision's trace is written with the event
// and every todo. Rules run through a routing.Router — in production the out-of-process sandbox — and
// see the SAME sanitized headers the event row stores, so a dry-run against a stored event reproduces
// exactly what the live delivery saw.
//
// Cairn (cairn ADR-0017 / SPEC-0012) signs artifact.created deliveries with
// `X-Cairn-Signature: sha256=<hex>` — HMAC-SHA256 over the raw body — and signs no timestamp header.
// The body itself carries a unique event_id and a created_at, both covered by the signature, so the
// receiver takes its replay defenses from there: event_id is the idempotency key (a replayed body
// collapses onto the original delivery) and created_at must fall inside the replay window (a
// replay after that slot has been reaped is refused). The unsigned X-Cairn-Event-Id header must
// agree with the signed event_id when present, so it cannot be used to mint a fresh dedup key.
//
// Governing: ADR-0024, SPEC-0020 REQ "Deterministic Rule Evaluation", REQ "Routing Trace";
// SPEC-0001 REQ "Signed Webhook Verification", REQ "Replay-Window Enforcement for Timestamped
// Signatures", REQ "Idempotency Key Extraction and Dedup"; ADR-0012.
//
// @joestump-agent 09/11/2026 - Routing stage + cairn as a first-class signed source.
package ingest

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/joestump/switchboard/internal/routing"
	"github.com/joestump/switchboard/internal/store"
)

// SetRouter replaces the router used for webhooks that have routing rules. New installs the
// out-of-process sandbox; tests install routing.InProcess{}.
func (i *Ingest) SetRouter(r routing.Router) { i.router = r }

// routeDelivery decides where a verified self-managed delivery lands. targets is the live, authorized
// fan-out set (ResolveWebhookTargets); the grant is built from switchboard state only.
func (i *Ingest) routeDelivery(ctx context.Context, wh store.Webhook, targets []string, kind string,
	verified bool, headers []byte, contentType string, body []byte) (routing.Decision, error) {
	rt, err := i.store.WebhookRoutingByID(ctx, wh.ID)
	if err != nil {
		return routing.Decision{}, err
	}
	var hdr map[string]string
	_ = json.Unmarshal(headers, &hdr) // sanitizeHeaders always emits a flat string map
	router := i.router
	if router == nil {
		router = routing.Unavailable{}
	}
	return router.Route(ctx, rt.Config,
		routing.Grant{TargetQueue: wh.TargetQueue, Queues: rt.WebhookQueues, Endpoints: targets},
		routing.EnvelopeInput{
			Source: wh.SourceType, Kind: kind, WebhookID: wh.ID, TrustMode: wh.TrustMode,
			Verified: verified, ContentType: contentType, Headers: hdr, Body: body,
		}), nil
}

// cairnDelivery is the part of cairn's signed body the receiver itself depends on.
type cairnDelivery struct {
	EventID   string `json:"event_id"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
	Data      struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		ShareType string `json:"share_type"`
	} `json:"data"`
}

func parseCairn(body []byte) (cairnDelivery, bool) {
	var d cairnDelivery
	if err := json.Unmarshal(body, &d); err != nil {
		return cairnDelivery{}, false
	}
	return d, true
}

// cairnEventID is the delivery id for a cairn body: its signed event_id, or "" (body-hash fallback).
func cairnEventID(body []byte) string {
	d, _ := parseCairn(body)
	return d.EventID
}

// verifyCairn checks X-Cairn-Signature over the raw body in constant time (the same `sha256=<hex>`
// format GitHub uses), then the replay defenses cairn's scheme leaves to the consumer: a signed
// event_id and a signed created_at inside the replay window, and an X-Cairn-Event-Id header that, if
// present, matches. Every failure is a plain false — the caller answers 401 and persists nothing.
func verifyCairn(secret string, body []byte, sig, eventIDHeader string, now time.Time, tolerance time.Duration) bool {
	if !verifyGitHub(secret, body, sig) {
		return false
	}
	d, ok := parseCairn(body)
	if !ok || d.EventID == "" || d.CreatedAt == "" {
		return false
	}
	if eventIDHeader != "" && eventIDHeader != d.EventID {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, d.CreatedAt)
	if err != nil {
		return false
	}
	return freshTimestamp(created.Unix(), now, tolerance)
}

// summarizeCairn titles a cairn delivery the way the doorbell should read it: what arrived and what
// it is called.
func summarizeCairn(body []byte) string {
	d, _ := parseCairn(body)
	kind := d.Kind
	if kind == "" {
		kind = "delivery"
	}
	title := strings.TrimSpace(d.Data.Title)
	switch {
	case title != "" && d.Data.ShareType != "":
		return "cairn " + kind + " — " + title + " (" + d.Data.ShareType + ")"
	case title != "":
		return "cairn " + kind + " — " + title
	case d.Data.ID != "":
		return "cairn " + kind + " " + d.Data.ID
	default:
		return "cairn " + kind
	}
}
