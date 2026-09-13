// Generic webhook receiver: POST /webhooks/generic/{name} for providers with no signing scheme
// (Docker Hub, homelab/self-hosted senders). Two trust modes, both honest and both weaker than
// `signed`:
//
//   - `token` — a configured shared-secret token authenticates the CALLER (not the body), compared
//     in constant time. The provider is disabled until a token is set; a missing/wrong token is a
//     403 and nothing is persisted. Accepted deliveries persist trust_mode='token', verified=false.
//   - `open`  — no verification at all. Exists only by explicit operator opt-in; an unconfigured
//     provider name is a 404, never a silent fall-through to open. Accepted deliveries persist
//     trust_mode='open', verified=false.
//
// Governing: ADR-0003 (per-provider trust model), SPEC-0001 REQ "Shared-Secret Token
// Authentication for Unsigned Webhooks", SPEC-0001 REQ "Explicit Open Trust Mode".
package ingest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Trust modes a generic provider may be explicitly configured with (ADR-0003).
const (
	trustModeToken = "token"
	trustModeOpen  = "open"
)

// tokenHeader is the dedicated shared-secret header for generic providers (preferred over the URL
// fallback). It is redacted before headers are persisted (see sensitiveHeaders in ingest.go).
const tokenHeader = "X-Webhook-Token"

// verifyDetail strings persisted on accepted generic deliveries. `token` explicitly states the body
// is NOT verified so it can never be misread as `signed`; `open` states no verification happened.
// Governing: SPEC-0001 REQ "Shared-Secret Token Authentication for Unsigned Webhooks" (never
// presented as signed), REQ "Explicit Open Trust Mode".
const (
	tokenVerifyDetail = "token ok (caller authenticated; body not verified)"
	openVerifyDetail  = "open — no verification"
)

// GenericProvider configures one generic webhook provider served at /webhooks/generic/{name}.
type GenericProvider struct {
	// Mode is the explicit trust mode: "token" or "open". There is no default — a provider with an
	// unknown or empty mode is rejected at parse time so nothing ever falls through to open.
	Mode string `json:"mode"`
	// Token is the shared secret for mode=token. A token provider with an empty token is DISABLED:
	// every request is rejected 403 until a token is configured.
	Token string `json:"token"`
	// Queue is the target todo queue; defaults to the provider name.
	Queue string `json:"queue"`
}

// ParseGenericProviders parses the SWITCHBOARD_GENERIC_PROVIDERS env value: a JSON object mapping
// provider name → {mode, token, queue}, e.g.
//
//	{"dockerhub":{"mode":"token","token":"s3cret","queue":"builds"},"lan":{"mode":"open"}}
//
// It fails loudly on malformed JSON, an empty provider name, or a mode outside {token, open} —
// misconfiguration must never silently become an open endpoint. An empty input yields an empty map
// (no generic providers configured). Governing: SPEC-0001 REQ "Explicit Open Trust Mode" (the
// system MUST NOT default any provider to open).
func ParseGenericProviders(raw string) (map[string]GenericProvider, error) {
	out := map[string]GenericProvider{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse generic providers config: %w", err)
	}
	for name, p := range out {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("parse generic providers config: empty provider name")
		}
		if p.Mode != trustModeToken && p.Mode != trustModeOpen {
			return nil, fmt.Errorf("parse generic providers config: provider %q has invalid mode %q (must be %q or %q)",
				name, p.Mode, trustModeToken, trustModeOpen)
		}
		if p.Queue == "" {
			p.Queue = name
			out[name] = p
		}
	}
	return out, nil
}

// Generic is the token/open generic webhook receiver: POST /webhooks/generic/{name}.
//
// Dispatch resolves the provider from the REGISTRY at request time (ADR-0020): a registry row is
// authoritative — its trust mode, secret, enabled flag, and queue decide the request, so a
// wizard-created provider is live (and an operator's disable bites) without a restart. The
// boot-time env map is only the fallback for names the registry does not hold; after the boot seed
// (SPEC-0017 REQ "Environment Config Import") every env provider has a row, so the fallback covers
// only store-less test wiring and the pre-seed window. Governing: SPEC-0017 REQ "Runtime Provider
// Registry" (scenario "Wizard-created provider is live immediately").
func (i *Ingest) Generic(w http.ResponseWriter, r *http.Request) {
	body, ok := i.readBody(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	p, exists := i.generic[name]
	reg, secret, err := i.resolveRegistry(r.Context(), name)
	switch {
	case err == nil:
		// Registry row wins over any env/boot map entry — the registry is authoritative after
		// import (SPEC-0017 REQ "Environment Config Import": env changes never silently override).
		if reg.Family != "webhook" || (reg.TrustMode != trustModeToken && reg.TrustMode != trustModeOpen) {
			// The name belongs to some other kind of provider (signed webhook, queue adapter) — it
			// is not a generic endpoint, and 404 must not leak what it is.
			writeErr(w, http.StatusNotFound, "unknown provider")
			return
		}
		if !reg.Enabled {
			// Disabled stops the line while history stays queryable (SPEC-0017 REQ "Provider
			// Lifecycle" — scenario "Disable stops the line"). Nothing is persisted.
			i.log.Warn("generic webhook rejected: provider disabled", "provider", name, "remote", clientIP(r))
			writeErr(w, http.StatusForbidden, "provider disabled")
			return
		}
		p, exists = GenericProvider{Mode: reg.TrustMode, Token: secret, Queue: providerQueue(reg.Config, name)}, true
	case errors.Is(err, store.ErrNotFound):
		// No registry row: fall through to the boot map (or 404 below).
	default:
		// Registry unavailable: fail closed, never fall back to possibly-stale env trust decisions.
		// Governing: SPEC-0001 REQ "Error Handling Standards" (generic to the client, structured log).
		i.log.Error("generic provider registry lookup", "provider", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !exists {
		// Governing: SPEC-0001 scenario "Open provider only exists when explicitly created" — an
		// unconfigured provider name is 404, never an implicit open endpoint.
		writeErr(w, http.StatusNotFound, "unknown provider")
		return
	}

	// The idempotency key: the sender's own delivery id when it stamped one (genericDeliveryID),
	// else the body hash. Derived here (before verification) ONLY to correlate the ephemeral
	// received-lane card (SPEC-0015); nothing is persisted until the trust check below passes.
	key := idempotencyKey(genericDeliveryID(r.Header), body)
	// The line is in flight for a CONFIGURED provider: surface it on the received lane. An
	// unconfigured name (the 404 above) never rings the board — probe noise is not a line.
	i.observeReceived(name, "webhook", p.Mode, key)

	var trustMode, verifyDetail string
	switch p.Mode {
	case trustModeToken:
		if p.Token == "" {
			// Governing: SPEC-0001 scenario "Generic provider disabled until token set".
			i.log.Warn("generic webhook rejected: provider disabled (no token configured)",
				"provider", name, "remote", clientIP(r))
			i.observeRejected(name, "webhook", p.Mode, key, "provider disabled")
			writeErr(w, http.StatusForbidden, "provider disabled")
			return
		}
		if !tokenEqual(p.Token, presentedToken(r)) {
			// Reject without persisting; log a redacted line (never the token value).
			i.log.Warn("generic webhook token rejected", "provider", name, "remote", clientIP(r))
			i.observeRejected(name, "webhook", p.Mode, key, "invalid token")
			writeErr(w, http.StatusForbidden, "invalid token")
			return
		}
		trustMode, verifyDetail = trustModeToken, tokenVerifyDetail
	case trustModeOpen:
		// Explicit operator opt-in only — reaching here required a configured mode of "open"
		// (ParseGenericProviders rejects everything else). Governing: SPEC-0001 REQ "Explicit Open
		// Trust Mode".
		trustMode, verifyDetail = trustModeOpen, openVerifyDetail
	default:
		// Defense in depth: an Ingest constructed with an unvalidated map still fails closed.
		i.log.Warn("generic webhook rejected: invalid trust mode", "provider", name, "mode", p.Mode)
		i.observeRejected(name, "webhook", p.Mode, key, "provider disabled")
		writeErr(w, http.StatusForbidden, "provider disabled")
		return
	}

	// INTERIM (PR 2): operator-configured receiver, no vended endpoint of its own — the todo is
	// owned by the operator-designated legacy endpoint; unconfigured → 503, nothing persisted
	// (legacyEndpoint). ADR-0022.
	endpointID, ok := i.legacyEndpoint(w, name)
	if !ok {
		i.observeRejected(name, "webhook", trustMode, key, "receiver not configured")
		return
	}
	// key (derived above) is the idempotency key: the sender's delivery id when it stamped one,
	// else the body hash (SPEC-0001 REQ "Idempotency Key Extraction and Dedup" — generic delivery id).
	// Governing: SPEC-0002/0004 REQ atomic ingestion — event + todo commit in one transaction.
	_, td, created, err := i.store.CreateEventTodo(r.Context(),
		store.EventInput{
			Source: name, Family: "webhook", ExternalID: key,
			TrustMode: trustMode, Verified: false, VerifyDetail: verifyDetail,
			ContentType: r.Header.Get("Content-Type"), Headers: sanitizeHeaders(r.Header),
			Payload: body, SourceIP: clientIP(r),
		},
		store.CreateTodoParams{
			EndpointID: endpointID,
			Queue:      p.Queue, Source: name, Kind: "webhook", Title: summarizeGeneric(name),
			Payload: body, IdempotencyKey: key,
		})
	if err != nil {
		i.log.Error("ingest generic delivery", "provider", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if created {
		i.hub.Publish(td)
	} else {
		// Idempotent redelivery: resolve the in-flight card without a lane advance (SPEC-0015).
		i.observeDeduped(name, "webhook", trustMode, key)
	}
	// verified is always false here — token authenticates the caller, not the body, and open
	// verifies nothing. `token`/`open` MUST never be presented as `signed` (SPEC-0001).
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": td.ID, "queue": td.Queue, "verified": false, "trust_mode": trustMode,
	})
}

// presentedToken extracts the shared-secret token from the request: `Authorization: Bearer <token>`
// or the dedicated X-Webhook-Token header (preferred), with a `?token=` URL fallback for senders
// that can only be configured with a URL. The `Bearer` auth-scheme is matched case-insensitively —
// RFC 7235 §2.1 makes scheme names case-insensitive, so `bearer <token>` must authenticate too.
// Governing: SPEC-0001 REQ "Shared-Secret Token Authentication for Unsigned Webhooks" (header
// SHOULD, URL MAY).
func presentedToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const scheme = "Bearer "
		if len(auth) > len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
			return auth[len(scheme):]
		}
	}
	if tok := r.Header.Get(tokenHeader); tok != "" {
		return tok
	}
	return r.URL.Query().Get("token")
}

// tokenEqual compares a presented token against the configured one in constant time. Both sides are
// SHA-256 hashed first so subtle.ConstantTimeCompare always runs over equal-length inputs — a
// length mismatch neither short-circuits nor leaks the configured token's length. An empty
// presented token never matches. Governing: SPEC-0001 REQ "Shared-Secret Token Authentication for
// Unsigned Webhooks" (constant-time compare).
func tokenEqual(want, got string) bool {
	if got == "" {
		return false
	}
	w := sha256.Sum256([]byte(want))
	g := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(w[:], g[:]) == 1
}

// providerQueue extracts the target todo queue from a registry row's non-secret config jsonb
// ({"queue":…}), defaulting to the provider name — the same default ParseGenericProviders applies
// to env config. Governing: ADR-0020 (registry config carries the routing).
func providerQueue(config []byte, name string) string {
	var c struct {
		Queue string `json:"queue"`
	}
	_ = json.Unmarshal(config, &c)
	if c.Queue == "" {
		return name
	}
	return c.Queue
}

// summarizeGeneric builds a one-line, legible todo title for a generic provider delivery.
func summarizeGeneric(name string) string {
	return "webhook " + name + " delivery"
}

// Generic delivery ids. A generic sender has no signing scheme and so no signed delivery id, but a
// producer that retries can still stamp every attempt with the same id — the plain `X-Delivery-Id`,
// or the Standard Webhooks `Webhook-Id` — and a retry whose body differs (a fresh timestamp, a
// re-serialized payload) then collapses onto the original todo instead of minting a second one.
// Without it the body hash is the only key a generic sender can get, and every byte-different retry
// is a duplicate.
//
// The id is trusted exactly as much as the body it travels with: it is caller-asserted, and the
// key it feeds is scoped to the provider (events dedup on (source, external_id), todos on the
// owning endpoint), so a caller can only ever collapse ITS OWN deliveries. An id over
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
