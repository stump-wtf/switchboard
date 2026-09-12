// Self-managed webhook receiver: POST /webhooks/w/{ingest_token}. This is the delivery endpoint an
// agent hands to its producer after create_webhook/rotate_webhook (SPEC-0006). Without it the
// ingest_url those verbs return would be a dead 404 and no delivery could ever become a todo.
//
// The unguessable 128-bit ingest token in the URL path routes a delivery to exactly one webhook
// (unique index). How the delivery is then verified is the webhook's switchboard-derived trust mode —
// the agent never chose it and cannot downgrade it:
//
//   - signed (github/stripe/slack): switchboard MINTED the HMAC signing secret and HOLDS the
//     plaintext, so the receiver recomputes the provider HMAC over the raw body and compares it in
//     constant time, EXACTLY as the operator-configured signed receivers do (verifyGitHub/
//     verifyStripe/verifySlack). A valid signature persists verified=true, trust_mode=signed —
//     indistinguishable from a human-configured signed webhook. A missing/invalid signature is a 401
//     and NOTHING is persisted (fail closed).
//   - token (generic): the unguessable ingest URL authenticates the caller, the body is not
//     signature-verified, so the delivery persists honestly with verified=false and an explicit
//     verify detail — never presented as `signed`, matching the generic token receiver.
//
// Duplicate deliveries collapse to a single todo: the idempotency key is scoped to this webhook (its
// id) plus the delivery id (GitHub's X-GitHub-Delivery where present) or a body hash, so a redelivery
// to the same webhook dedups while identical payloads to different webhooks stay distinct.
//
// Governing: ADR-0012 (agents self-manage webhooks within a vended ceiling),
// ADR-0003 / SPEC-0001 (per-provider trust model & signed-webhook verification),
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency" (mint/hold/verify → dedup → todo),
// SPEC-0001 REQ "Request Body Size Limits", REQ "Idempotency Key Extraction and Dedup",
// REQ "Header and Secret Sanitization Before Persist".
package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// SelfManaged is the receiver for agent self-managed webhooks: POST /webhooks/w/{token}.
func (i *Ingest) SelfManaged(w http.ResponseWriter, r *http.Request) {
	// Bound the body first (413, nothing persisted) — same contract as every other receiver.
	body, ok := i.readBody(w, r)
	if !ok {
		return
	}

	token := chi.URLParam(r, "token")
	if token == "" {
		writeErr(w, http.StatusNotFound, "unknown webhook")
		return
	}
	// Resolve the routing token to its webhook AND the signing secret switchboard holds for it. The
	// secret never travels on the metadata struct; it is read here solely to recompute the HMAC.
	wh, secret, err := i.store.GetWebhookSecretByToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		// An unknown token is indistinguishable from a guess; 404 without persisting.
		i.log.Warn("self-managed webhook unknown token", "remote", clientIP(r))
		writeErr(w, http.StatusNotFound, "unknown webhook")
		return
	}
	if err != nil {
		i.log.Error("self-managed webhook lookup", "err", err, "remote", clientIP(r))
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Idempotency key scoped to this webhook so a redelivery to the SAME webhook dedups but identical
	// payloads to different self-managed webhooks (which share a source namespace like "github") never
	// collide. Prefer a provider delivery id where one exists; fall back to a body hash otherwise.
	// Gitea uses X-Gitea-Delivery; GitHub uses X-GitHub-Delivery. Cairn's id is the event_id inside
	// its SIGNED body, never an unsigned header, so a replay cannot mint a fresh key (routing.go).
	var deliveryID string
	if wh.SourceType == routing.SourceCairn {
		deliveryID = cairnEventID(body)
	} else {
		deliveryID = r.Header.Get("X-GitHub-Delivery")
		if deliveryID == "" {
			deliveryID = r.Header.Get("X-Gitea-Delivery")
		}
	}
	key := wh.ID + ":" + idempotencyKey(deliveryID, body)
	// The routed line is in flight: surface it on the board's ephemeral received lane (SPEC-0015).
	// Unknown tokens (the 404s above) never ring the board — a guess is not a line.
	i.observeReceived(wh.SourceType, "webhook", wh.TrustMode, key)

	// Verify per the webhook's trust mode. signed → provider HMAC over the raw body against the stored
	// secret (fail closed on a bad/missing signature); token → caller authenticated by the unguessable
	// URL, body not verified. No trust downgrade is possible: the mode is switchboard's, fixed at create.
	verified := false
	verifyDetail := "self-managed " + wh.TrustMode + " webhook; caller authenticated by unguessable ingest URL; body not verified"
	if wh.TrustMode == "signed" {
		if secret == "" {
			// A signed webhook with no stored secret cannot be verified — refuse rather than fake trust.
			// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency".
			i.log.Error("self-managed signed webhook missing secret", "webhook", wh.ID, "remote", clientIP(r))
			i.observeRejected(wh.SourceType, "webhook", wh.TrustMode, key, "webhook not configured")
			writeErr(w, http.StatusServiceUnavailable, "webhook not configured")
			return
		}
		valid, err := i.verifySelfManagedSigned(r, wh.SourceType, secret, body)
		if err != nil {
			// An unsupported signed source type is a server-side misconfiguration, not a client fault.
			i.log.Error("self-managed signed webhook verify", "webhook", wh.ID, "source", wh.SourceType, "err", err)
			i.observeRejected(wh.SourceType, "webhook", wh.TrustMode, key, "verification unavailable")
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !valid {
			// Reject without persisting; log a redacted line (never the signature value or secret) —
			// exactly like the operator-configured signed receivers.
			i.log.Warn("self-managed webhook signature rejected", "webhook", wh.ID, "source", wh.SourceType,
				"delivery", deliveryID, "remote", clientIP(r))
			i.observeRejected(wh.SourceType, "webhook", wh.TrustMode, key, "signature verification failed")
			writeErr(w, http.StatusUnauthorized, "signature verification failed")
			return
		}
		// Valid HMAC: identical trust posture to a human-configured signed webhook.
		verified = true
		verifyDetail = "hmac-sha256 ok"
	}

	// Resolve the delivery's target endpoints: {owning endpoint} ∪ every explicit webhook_routes
	// target, de-duplicated, owner first. This is the token-free fan-out — routes are populated by
	// human-approved actions (friending, the MCP routing verbs), never by a per-delivery decision in
	// the payload, so a producer cannot steer a delivery at an endpoint it was not granted.
	// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
	targets, err := i.store.ResolveWebhookTargets(r.Context(), wh.ID, wh.EndpointID)
	if err != nil {
		i.log.Error("self-managed webhook resolve targets", "webhook", wh.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(targets) == 0 {
		// Per the ResolveWebhookTargets contract an empty target set means the owner endpoint is not
		// resolvable, so this delivery would produce NO work. That is a misconfiguration, not a
		// no-op: 503 (nothing persisted) so the producer retries once the webhook is coherent again,
		// rather than a 202 that silently drops the delivery on the floor.
		i.log.Error("self-managed webhook has no target endpoints; refusing delivery",
			"webhook", wh.ID, "endpoint", wh.EndpointID)
		i.observeRejected(wh.SourceType, "webhook", wh.TrustMode, key, "webhook not configured")
		writeErr(w, http.StatusServiceUnavailable, "webhook not configured")
		return
	}

	// Route (ADR-0024, SPEC-0020): after the idempotency key and the fan-out targets, before any
	// write. Rules pick a queue — optionally narrowing the targets — or drop; a webhook with no rules
	// takes its target queue on every target, exactly as before routing existed. The trace is
	// written with the event and every todo so each one can explain why it exists.
	kind := routing.EventKind(wh.SourceType, r.Header.Get, body)
	headers := sanitizeHeaders(r.Header)
	decision, envIn, err := i.routeDelivery(r.Context(), wh, targets, kind, verified, headers, r.Header.Get("Content-Type"), body)
	if err != nil {
		i.log.Error("self-managed webhook routing", "webhook", wh.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Work orders (ADR-0025). The subject — the issue or artifact this delivery is about — is parsed
	// here in Go from the verified body, never taken from a rule: it keys at-most-once delivery and
	// populates the work order, and neither may depend on tenant-written jq.
	subject := routing.SubjectOf(wh.SourceType, envIn.Headers, body)
	var onceKey string
	if decision.Once && !decision.Drop {
		onceKey = routing.OnceKey(subject, decision.Queue)
		decision.Trace.OnceKey = onceKey
	}
	var workOrder []byte
	if decision.WorkOrder && !decision.Drop {
		if workOrder, err = json.Marshal(routing.BuildWorkOrder(decision, envIn, subject)); err != nil {
			i.log.Error("self-managed webhook work order", "webhook", wh.ID, "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	trace, err := json.Marshal(decision.Trace)
	if err != nil {
		i.log.Error("self-managed webhook routing trace", "webhook", wh.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Governing: SPEC-0002/0004 REQ atomic ingestion, generalized to N targets (ADR-0022) — the
	// event and ALL of its per-target todos commit in one transaction, so a fan-out is never
	// partial. Every target's todo carries the SAME idempotency key this receiver computed,
	// "<webhook-id>:<delivery-or-body-hash>" — which is also the event's external_id, the equality
	// the Board's dedup count and received-card retirement both join on. Per-target separation
	// comes from the (endpoint_id, idempotency_key) dedup index, not from rewriting the key: a
	// redelivery collapses independently within each target, while two targets of the SAME delivery
	// never collapse onto each other. Governing: SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
	_, todos, dropped, err := i.store.CreateRoutedEventTodos(r.Context(),
		store.EventInput{
			Source: wh.SourceType, Family: "webhook", EventType: kind, ExternalID: key,
			TrustMode: wh.TrustMode, Verified: verified, VerifyDetail: verifyDetail,
			ContentType: r.Header.Get("Content-Type"), Headers: headers,
			Payload: body, SourceIP: clientIP(r),
			WebhookID: wh.ID, RoutingTrace: trace,
		}, decision.Drop, decision.Endpoints,
		store.CreateTodoParams{
			Queue: decision.Queue, Source: wh.SourceType, Kind: "webhook",
			Title:   summarizeSelfManagedTitle(wh.SourceType, selfManagedEvent(r), body),
			Payload: body, IdempotencyKey: key, RoutingTrace: trace,
			OnceKey: onceKey, WorkOrder: workOrder,
		})
	if err != nil {
		i.log.Error("ingest self-managed delivery", "webhook", wh.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if dropped {
		// Recorded, not work: the event row and its trace persist and the dedup slot is spent, but
		// there is no todo, no hub publish, and no doorbell. The in-flight card resolves without a
		// lane advance, as a deduped redelivery's does. The trace is NOT echoed to the producer — the
		// owner's rule names are the owner's business. Governing: SPEC-0020 REQ "Drop Action
		// Semantics".
		i.observeDeduped(wh.SourceType, "webhook", wh.TrustMode, key)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"todos": []map[string]any{}, "created": 0, "dropped": true,
			"verified": verified, "trust_mode": wh.TrustMode,
		})
		return
	}
	if len(todos) == 0 {
		// A repeat work order: an earlier delivery about the same subject already claimed this
		// (subject, queue), so this one is recorded (its event trace says "once":"repeat") and mints
		// nothing — the relabel that re-routes an issue does not also re-run it. Governing: ADR-0025,
		// SPEC-0020 REQ "At-Most-Once Work Orders".
		i.observeDeduped(wh.SourceType, "webhook", wh.TrustMode, key)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"todos": []map[string]any{}, "created": 0, "repeat": true,
			"verified": verified, "trust_mode": wh.TrustMode,
		})
		return
	}

	// Response body (SPEC-0001): a delivery now yields N todos, so the payload reports the set.
	//
	//	{"todos": [{"id":…, "endpoint_id":…, "queue":…, "created": true|false}, …],
	//	 "created": <count of newly-minted todos>,
	//	 "id": …, "queue": …,            // the first target's todo — retained for compatibility
	//	 "verified": …, "trust_mode": …}
	//
	// `id`/`queue` name the first target's todo. Targets keep ResolveWebhookTargets' owner-first
	// order, so that is the owner's todo unless a routing rule narrowed the owner out — and a
	// single-target webhook with no rules sees byte-identical fields to the pre-fan-out response.
	// `created` changes shape from nothing to a count; it was never in the old body, so no existing
	// reader loses a field. A dropped delivery answers {"dropped": true} with an empty todo set.
	// endpoint_id is deliberately included: the producer already knows the webhook it posted to,
	// and the fan-out is only auditable if the response says where the work actually landed.
	newTodos := 0
	items := make([]map[string]any, 0, len(todos))
	for _, ct := range todos {
		if ct.New {
			newTodos++
			// Hub fan-out is endpoint-scoped (ADR-0022); publishing per-todo hands each target's
			// subscribers only their own row. Redeliveries are skipped: an already-live todo is not
			// news, and ringing again would double-count on the board.
			i.hub.Publish(ct.Todo)
		}
		items = append(items, map[string]any{
			"id": ct.Todo.ID, "endpoint_id": ct.Todo.EndpointID,
			"queue": ct.Todo.Queue, "created": ct.New,
		})
	}
	if newTodos == 0 {
		// Every target collapsed onto an existing live todo: a wholly idempotent redelivery.
		// Resolve the in-flight card without a lane advance (SPEC-0015).
		i.observeDeduped(wh.SourceType, "webhook", wh.TrustMode, key)
	}
	owner := todos[0].Todo // owner-first target order; see the response-body note above
	writeJSON(w, http.StatusAccepted, map[string]any{
		"todos": items, "created": newTodos,
		"id": owner.ID, "queue": owner.Queue,
		"verified": verified, "trust_mode": wh.TrustMode,
	})
}

// verifySelfManagedSigned recomputes the provider HMAC for a signed self-managed webhook, dispatching
// on the switchboard-derived source type to the same verifiers the operator-configured receivers use.
// The stored signing secret is the plaintext switchboard minted and holds. An unsupported signed
// source type is an error (server-side), not a silent false. Governing: SPEC-0001 REQ "Signed Webhook
// Verification", REQ "Replay-Window Enforcement for Timestamped Signatures", ADR-0003.
func (i *Ingest) verifySelfManagedSigned(r *http.Request, sourceType, secret string, body []byte) (bool, error) {
	switch sourceType {
	case "github":
		return verifyGitHub(secret, body, r.Header.Get("X-Hub-Signature-256")), nil
	case "gitea":
		// Gitea signs the raw body with HMAC-SHA256 and presents it TWICE: natively as
		// `X-Gitea-Signature: <hex>` and, for GitHub compatibility, as
		// `X-Hub-Signature-256: sha256=<hex>`. The operator-configured receiver (Gitea, signed.go)
		// verifies the latter, so accept either here rather than leaving the two Gitea paths
		// disagreeing about which header is authoritative.
		if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
			return verifyGitea(secret, body, sig), nil
		}
		return verifyGitHub(secret, body, r.Header.Get("X-Hub-Signature-256")), nil
	case "stripe":
		return verifyStripe(secret, body, r.Header.Get("Stripe-Signature"), i.now(), i.tolerance), nil
	case "slack":
		return verifySlack(secret, body, r.Header.Get("X-Slack-Request-Timestamp"),
			r.Header.Get("X-Slack-Signature"), i.now(), i.tolerance), nil
	case routing.SourceCairn:
		// Body HMAC plus signed event_id/created_at replay defenses (routing.go verifyCairn).
		return verifyCairn(secret, body, r.Header.Get("X-Cairn-Signature"), r.Header.Get("X-Cairn-Event-Id"),
			i.now(), i.tolerance), nil
	default:
		return false, fmt.Errorf("unsupported signed source type %q", sourceType)
	}
}

// verifyGitea checks X-Gitea-Signature (bare hex HMAC-SHA256) over the raw body in constant time.
// Gitea's self-managed webhook signing uses a bare hex digest without the "sha256=" prefix that
// GitHub uses. Governing: SPEC-0001 REQ "Signed Webhook Verification", ADR-0003.
func verifyGitea(secret string, body []byte, sig string) bool {
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// summarizeSelfManaged builds a one-line, legible todo title for a self-managed webhook delivery.
func summarizeSelfManaged(sourceType string) string {
	if sourceType == "" {
		return "self-managed webhook delivery"
	}
	return "self-managed " + sourceType + " delivery"
}

// selfManagedEvent reads the forge event type off the delivery. Gitea sends X-Gitea-Event and, for
// GitHub compatibility, X-GitHub-Event; GitHub sends only the latter.
func selfManagedEvent(r *http.Request) string {
	if event := r.Header.Get("X-GitHub-Event"); event != "" {
		return event
	}
	return r.Header.Get("X-Gitea-Event")
}

// summarizeSelfManagedTitle builds a legible todo title for a self-managed webhook delivery. The
// forge sources (gitea, github) share GitHub's payload shape, so they reuse summarizeForge — the
// SAME summarizer the operator-configured receivers feed — and the board reads identically whether
// a delivery arrived on /webhooks/gitea or on a vended self-managed URL. That also buys issue
// events a real title, not just pull requests.
//
// A "generic" webhook carrying a forge event header gets the same treatment, and that case is not
// hypothetical: `endpoint vend` mints exactly one generic webhook, so the ordinary way to wire a
// forge at a vended endpoint produces one. Titling those "self-managed generic delivery" is a real
// cost, not a cosmetic one — the title is what the doorbell says, so an agent woken by an issue
// assigned to it was told only that *something* arrived, and had to claim the todo and unpack the
// payload to discover what. Every forge delivery to that endpoint looked identical.
//
// The event header is what makes this safe to infer rather than guess: X-Gitea-Event and
// X-GitHub-Event are set by the forge itself, and nothing else sends them. A body that does not
// parse as a forge payload still yields a title (summarizeForge falls back to "<label> <event>"),
// so a mislabelled delivery degrades to today's behaviour instead of erroring. Trust is untouched:
// this reads the body a token-trust delivery already stores, exactly as the gitea/github
// self-managed path does, and titles are escaped at render.
//
// The inference is deliberately limited to "generic", which means "unlabelled". An explicitly
// non-forge source type is taken at its word — a stripe webhook does not become a forge delivery
// because a header looked like one.
func summarizeSelfManagedTitle(sourceType, event string, body []byte) string {
	if sourceType == routing.SourceCairn {
		return summarizeCairn(body)
	}
	if event == "" {
		return summarizeSelfManaged(sourceType)
	}
	switch sourceType {
	case "gitea", "github":
		return summarizeForge(sourceType, event, body)
	case "generic":
		// Label the fallback with the source type so a generic webhook is still distinguishable on
		// the board from one declared as a forge.
		return summarizeForge(summarizeSelfManaged(sourceType), event, body)
	}
	// An explicitly non-forge source (stripe, slack, docker, ...) is taken at its word: its payload
	// is not a forge payload, and a header that happens to look like one does not change that.
	return summarizeSelfManaged(sourceType)
}
