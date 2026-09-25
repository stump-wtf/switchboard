package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Governing: ADR-0010 (A2A discovery + human-vended friending), ADR-0011 (OIDC provenance; passkey
// step-up deferred), SPEC-0010 REQ "Verifiable OIDC-Signed Provenance", REQ "Friend-Request
// Lifecycle" (pending edge grants nothing), REQ "Anti-Spam — Bounded Discovery and Quotas".
//
// This is the INWARD A2A friend-request intake: a remote board's agent, on behalf of a human, asks a
// LOCAL published persona for scoped access. Intake verifies the requesting human's OIDC-signed
// provenance and records a PENDING friend edge that grants nothing — the target human approves later
// (#63), and approval is the sole vend (ADR-0008). No endpoint is minted here.

// intakeMaxScopeItems bounds how many queues/verbs a single request may name — a requested_scope is a
// short list, not a blob (defense-in-depth alongside the 64 KiB body cap).
const intakeMaxScopeItems = 32

// intakeMaxScopeItemLen bounds a single queue/verb string; intakeMaxReasonLen bounds the reason.
const (
	intakeMaxScopeItemLen = 128
	intakeMaxReasonLen    = 1024
	intakeMaxPersonaLen   = 512
)

// intakeLiveRequestQuota is the per-requester ceiling on LIVE (pending+approved) friend edges. A
// requester at or over this cap is refused (429) without creating a pending edge — the anti-flood
// quota (SPEC-0010). The per-IP token bucket at the route handles burst rate; this bounds the
// standing backlog one attested human can accumulate.
const intakeLiveRequestQuota = 16

// friendIntakeStore is the slice of the data layer the intake needs. The narrow interface keeps the
// handler unit-testable without PostgreSQL. *store.Store satisfies it.
type friendIntakeStore interface {
	PublishedPersonaByID(ctx context.Context, id string) (store.Persona, error)
	UpsertHuman(ctx context.Context, subject, displayName, email string) (store.Human, error)
	CountLiveFriendRequestsFrom(ctx context.Context, fromHuman string) (int, error)
	CreateFriendRequest(ctx context.Context, p store.CreateFriendRequestParams) (store.FriendEdge, error)
}

// provenanceVerifier verifies a friend request's OIDC-signed human provenance and returns the
// attested subject (plus display name/email for the legible pending-request row). *auth.Authenticator
// satisfies it; a fake satisfies it in tests. A non-nil error MUST be surfaced as unauthenticated.
type provenanceVerifier interface {
	VerifyProvenance(ctx context.Context, rawToken string) (subject, name, email string, err error)
}

// friendIntake is the A2A friend-request intake handler.
type friendIntake struct {
	store    friendIntakeStore
	verifier provenanceVerifier
	log      *slog.Logger
}

// newFriendIntake wires the intake handler. *store.Store and *auth.Authenticator are the production
// implementations; tests pass fakes.
func newFriendIntake(st friendIntakeStore, v provenanceVerifier, log *slog.Logger) *friendIntake {
	return &friendIntake{store: st, verifier: v, log: log}
}

// friendRequestBody is the JSON an inbound A2A friend request carries. `provenance` is the requesting
// human's OIDC-signed ID token (the credential); the rest is the scoped ask against the target
// persona. Field names are snake_case to match the JSON surface elsewhere.
type friendRequestBody struct {
	ToPersona       string   `json:"to_persona"`       // target: a LOCAL published persona id (the card being friended)
	FromPersona     string   `json:"from_persona"`     // requesting persona handle/URL (opaque; carried on the edge)
	RequestedQueues []string `json:"requested_queues"` // ceiling the requester asks for; approval narrows
	RequestedVerbs  []string `json:"requested_verbs"`  // ceiling the requester asks for; approval narrows
	Reason          string   `json:"reason"`           // legible who/why, persisted on the friend edge
	Provenance      string   `json:"provenance"`       // OIDC-signed ID token attesting the requesting human
}

// Intake handles POST /a2a/friend-requests. It is not session-authenticated: the request carries its
// own OIDC-signed human provenance in-band (the justified non-session route — the provenance token IS
// the credential). Order matters: bound + parse the body, then VERIFY provenance before any lookup or
// write, so an unauthenticated caller never creates a pending edge nor probes persona existence.
func (h *friendIntake) Intake(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeIntakeErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "content-type must be application/json")
		return
	}

	// Decode within the route's http.MaxBytesReader (64 KiB). DisallowUnknownFields rejects stray
	// fields (typo'd or smuggled) rather than silently ignoring them. A body over the cap surfaces as
	// a *http.MaxBytesError → 413; any other decode failure is a 400.
	var body friendRequestBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeIntakeErr(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds limit")
			return
		}
		writeIntakeErr(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	// Provenance is the credential: verify it BEFORE any store access. Missing/invalid → unauthenticated,
	// no edge, no persona probe (SPEC-0010 "Missing or invalid provenance is rejected").
	if strings.TrimSpace(body.Provenance) == "" {
		writeIntakeErr(w, http.StatusUnauthorized, "unauthenticated", "missing OIDC-signed provenance")
		return
	}
	subject, name, email, err := h.verifier.VerifyProvenance(r.Context(), body.Provenance)
	if err != nil {
		if errors.Is(err, auth.ErrProvenanceUnavailable) {
			// No trusted issuer configured: we cannot attest anyone, so we refuse rather than record an
			// unverified edge. This is a server-capability gap, not a caller error → 503.
			h.log.Warn("friend intake: provenance verification unavailable (OIDC not configured)")
			writeIntakeErr(w, http.StatusServiceUnavailable, "provenance_unavailable", "server cannot verify provenance")
			return
		}
		h.log.Warn("friend intake rejected", "reason", "invalid provenance", "err", err, "remote", r.RemoteAddr)
		writeIntakeErr(w, http.StatusUnauthorized, "unauthenticated", "invalid OIDC-signed provenance")
		return
	}

	// Shape/size validation (the 64 KiB cap already bounds the whole body; this bounds the parts).
	if !validIntakeFields(&body) {
		writeIntakeErr(w, http.StatusBadRequest, "invalid_request", "invalid or missing required fields")
		return
	}

	// Resolve the target: a PUBLISHED persona only. Unknown/unpublished → 404, bounding discovery to
	// advertised cards (SPEC-0010 "discovery bounded to known directories"). This yields the owning
	// human who will approve/deny.
	persona, err := h.store.PublishedPersonaByID(r.Context(), body.ToPersona)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeIntakeErr(w, http.StatusNotFound, "not_found", "target persona not found")
			return
		}
		h.log.Error("friend intake: resolve persona", "err", err)
		writeIntakeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// Map the attested OIDC subject to its canonical local human row: from_human is a humans.id FK
	// (the requesting human's signed identity, not the agent's self-assertion). Idempotent upsert.
	requester, err := h.store.UpsertHuman(r.Context(), subject, name, email)
	if err != nil {
		h.log.Error("friend intake: upsert requesting human", "err", err)
		writeIntakeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// Per-requester quota: an attested human may hold only so many live edges. Over the cap → refuse
	// with no pending edge (SPEC-0010 anti-flood). Checked after upsert so the count is keyed on the
	// canonical requester id.
	if n, err := h.store.CountLiveFriendRequestsFrom(r.Context(), requester.ID); err != nil {
		h.log.Error("friend intake: quota count", "err", err)
		writeIntakeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	} else if n >= intakeLiveRequestQuota {
		h.log.Warn("friend intake refused", "reason", "quota exceeded", "from_human", requester.ID, "live", n)
		w.Header().Set("Retry-After", "3600")
		writeIntakeErr(w, http.StatusTooManyRequests, "quota_exceeded", "friend-request quota exceeded")
		return
	}

	edge, err := h.store.CreateFriendRequest(r.Context(), store.CreateFriendRequestParams{
		FromPersona:        body.FromPersona,
		ToPersona:          persona.ID,
		FromHuman:          requester.ID,
		ToHuman:            persona.OwnerHumanID,
		RequestedQueues:    body.RequestedQueues,
		RequestedVerbs:     body.RequestedVerbs,
		Reason:             body.Reason,
		ProvenanceVerified: true,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// A live (pending/approved) edge for this directional pair already exists — a duplicate
			// request, not a new one (anti-flood unique index). No new edge is created.
			writeIntakeErr(w, http.StatusConflict, "conflict", "a live friend request for this pair already exists")
			return
		}
		h.log.Error("friend intake: create friend request", "err", err)
		writeIntakeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// The pending edge IS the approval surface — no companion todo is minted. The row persisted
	// above already carries the legible who/why (from_human, from_persona, to_persona,
	// requested_queues, requested_verbs, reason, provenance_verified) that the target human's Friends
	// view renders straight out of friend_edges. Duplicate suppression is the idx_friend_edges_live
	// partial unique index, whose collision surfaced as the ErrConflict branch above — so a
	// re-delivered intake cannot double-list. Governing: SPEC-0010 REQ "Approval Surfaced from the
	// Friend Edge", ADR-0022 (todos are endpoint-owned; a human approval has no owning endpoint).
	h.log.Info("friend request accepted", "request_id", edge.ID, "to_persona", persona.ID,
		"from_human", requester.ID, "provenance_verified", edge.ProvenanceVerified)
	writeIntakeJSON(w, http.StatusAccepted, map[string]any{
		"request_id":          edge.ID,
		"state":               edge.State,
		"provenance_verified": edge.ProvenanceVerified,
	})
}

// validIntakeFields enforces required-field presence and shape/size bounds on the parsed body. The
// store still enforces the substantive invariants (subset-at-approval, live-pair uniqueness); this is
// input hygiene so malformed asks are 400s, not deep failures.
func validIntakeFields(b *friendRequestBody) bool {
	b.ToPersona = strings.TrimSpace(b.ToPersona)
	b.FromPersona = strings.TrimSpace(b.FromPersona)
	b.Reason = strings.TrimSpace(b.Reason)
	if b.ToPersona == "" || len(b.ToPersona) > intakeMaxPersonaLen {
		return false
	}
	if b.FromPersona == "" || len(b.FromPersona) > intakeMaxPersonaLen {
		return false
	}
	if len(b.Reason) > intakeMaxReasonLen {
		return false
	}
	if !validScopeList(b.RequestedVerbs) || !validScopeList(b.RequestedQueues) {
		return false
	}
	// A friend edge can only ever grant create_for and the drain verbs: any other requested verb
	// would act with the approver's authority, so it is dropped before the request is stored (the
	// store drops it again, for every other caller). Governing: ADR-0038, SPEC-0033 REQ "Closing
	// the Audited Surfaces" (F3).
	b.RequestedVerbs = store.FriendGrantable(b.RequestedVerbs)
	// A request must ask for SOMETHING to hand off; an empty scope grants nothing to approve.
	return len(b.RequestedVerbs) > 0
}

// validScopeList bounds the count and per-item length of a queue/verb list and rejects empty items.
func validScopeList(items []string) bool {
	if len(items) > intakeMaxScopeItems {
		return false
	}
	for _, it := range items {
		if it == "" || len(it) > intakeMaxScopeItemLen {
			return false
		}
	}
	return true
}

// writeIntakeJSON / writeIntakeErr mirror the JSON envelope used by the vended agent API so error
// shapes are uniform across switchboard's machine surfaces ({"error":{"code","message"}}).
func writeIntakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeIntakeErr(w http.ResponseWriter, status int, code, msg string) {
	writeIntakeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// compile-time assertions that the production types satisfy the intake's narrow interfaces.
var (
	_ friendIntakeStore  = (*store.Store)(nil)
	_ provenanceVerifier = (*auth.Authenticator)(nil)
)
