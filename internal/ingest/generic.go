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
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/switchboard/internal/store"
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
func (i *Ingest) Generic(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	p, exists := i.generic[name]
	if !exists {
		// Governing: SPEC-0001 scenario "Open provider only exists when explicitly created" — an
		// unconfigured provider name is 404, never an implicit open endpoint.
		writeErr(w, http.StatusNotFound, "unknown provider")
		return
	}

	var trustMode, verifyDetail string
	switch p.Mode {
	case trustModeToken:
		if p.Token == "" {
			// Governing: SPEC-0001 scenario "Generic provider disabled until token set".
			i.log.Warn("generic webhook rejected: provider disabled (no token configured)",
				"provider", name, "remote", clientIP(r))
			writeErr(w, http.StatusForbidden, "provider disabled")
			return
		}
		if !tokenEqual(p.Token, presentedToken(r)) {
			// Reject without persisting; log a redacted line (never the token value).
			i.log.Warn("generic webhook token rejected", "provider", name, "remote", clientIP(r))
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
		writeErr(w, http.StatusForbidden, "provider disabled")
		return
	}

	// Generic providers supply no delivery id, so the idempotency key is the body hash
	// (SPEC-0001 REQ "Idempotency Key Extraction and Dedup" — body-hash fallback).
	key := bodyHash(body)
	// Governing: SPEC-0002/0004 REQ atomic ingestion — event + todo commit in one transaction.
	_, td, created, err := i.store.CreateEventTodo(r.Context(),
		store.EventInput{
			Source: name, Family: "webhook", ExternalID: key,
			TrustMode: trustMode, Verified: false, VerifyDetail: verifyDetail,
			ContentType: r.Header.Get("Content-Type"), Headers: sanitizeHeaders(r.Header),
			Payload: body, SourceIP: clientIP(r),
		},
		store.CreateTodoParams{
			Queue: p.Queue, Source: name, Kind: "webhook", Title: summarizeGeneric(name),
			Payload: body, IdempotencyKey: key,
		})
	if err != nil {
		i.log.Error("ingest generic delivery", "provider", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if created {
		i.hub.Publish(td)
	}
	// verified is always false here — token authenticates the caller, not the body, and open
	// verifies nothing. `token`/`open` MUST never be presented as `signed` (SPEC-0001).
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": td.ID, "queue": td.Queue, "verified": false, "trust_mode": trustMode,
	})
}

// presentedToken extracts the shared-secret token from the request: `Authorization: Bearer <token>`
// or the dedicated X-Webhook-Token header (preferred), with a `?token=` URL fallback for senders
// that can only be configured with a URL. Governing: SPEC-0001 REQ "Shared-Secret Token
// Authentication for Unsigned Webhooks" (header SHOULD, URL MAY).
func presentedToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return tok
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

// summarizeGeneric builds a one-line, legible todo title for a generic provider delivery.
func summarizeGeneric(name string) string {
	return "webhook " + name + " delivery"
}
