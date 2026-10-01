package server

// Human API: Edit an Endpoint's Settings
//
// PATCH /api/v1/endpoints/{ref} edits the settings of an endpoint the caller owns that are not
// scope. Today that is one field, default_lease_ttl_seconds: the lease a claim, claim_next or
// heartbeat on the endpoint gets when the call names no lease_ttl_seconds. Scope (queues, verbs,
// ceilings, replay targets) stays immutable (SPEC-0007), so a body naming anything else is refused
// rather than silently ignored: a caller who sent {"queues": [...]} must not believe it worked.
//
// The route follows SPEC-0035's conventions: {ref} is a slug or id among the caller's own endpoints,
// and another human's endpoint is the same 404 as one that does not exist; a write to a revoked
// endpoint is 409 with the state; errors use {"error", "code"}; the body is capped at 64 KiB (413
// past it); writes draw from the per-human write bucket. An edit applies to claims and heartbeats
// made after it, and leaves leases already granted alone.
//
// Governing: ADR-0043, SPEC-0007 REQ "Endpoint Default Lease", SPEC-0035 REQ "Reach on Every
// Route", REQ "Human API Surface" (error shape), ADR-0022.
//
// @joestump-agent 10/01/2026 - Added: the first edit route on an endpoint.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/stump-wtf/switchboard/internal/lease"
	"github.com/stump-wtf/switchboard/internal/store"
)

// editEndpointIn is PATCH's body. The field stays raw so an absent key (refused: there is nothing
// else to edit) and an explicit null (reset to the server default) can be told apart.
type editEndpointIn struct {
	DefaultLeaseTTLSeconds json.RawMessage `json:"default_lease_ttl_seconds"`
}

// leaseRangeMessage is the one refusal text for an out-of-range default, shared by vend and edit.
func leaseRangeMessage() string {
	return fmt.Sprintf("default_lease_ttl_seconds must be a whole number of seconds from %d to %d, or null for the server default (%d)",
		lease.MinDefaultSeconds, lease.MaxSeconds, lease.DefaultSeconds)
}

// EditEndpoint is PATCH /api/v1/endpoints/{ref}: {"default_lease_ttl_seconds": int|null}. It answers
// the endpoint's list shape with the new value.
func (a *apiHandler) EditEndpoint(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	found, ok := a.ownedEndpoint(w, r, "edit endpoint")
	if !ok {
		return
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in editEndpointIn
	if err := dec.Decode(&in); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_argument", "request body is too large", nil)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_argument",
			"invalid JSON body: "+err.Error()+" (only default_lease_ttl_seconds can be edited; scope is fixed at vend)", nil)
		return
	}
	raw := bytes.TrimSpace(in.DefaultLeaseTTLSeconds)
	if len(raw) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument",
			"default_lease_ttl_seconds is required: a whole number of seconds, or null for the server default", nil)
		return
	}
	var seconds *int
	if !bytes.Equal(raw, []byte("null")) {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_argument", leaseRangeMessage(), nil)
			return
		}
		seconds = &n
	}

	stored, err := a.st.SetEndpointDefaultLeaseForHuman(r.Context(), found.ID, human.ID, seconds)
	switch {
	case errors.Is(err, lease.ErrOutOfRange):
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", leaseRangeMessage(), nil)
		return
	case errors.Is(err, store.ErrConflict):
		// The caller's own endpoint, revoked: its credential is dead, so the setting can apply to
		// nothing. 409 with the state, as SPEC-0035 answers every write to a revoked endpoint.
		writeAPIError(w, http.StatusConflict, "conflict", "endpoint is "+found.State+"; its settings can no longer change",
			map[string]any{"state": found.State, "slug": found.Slug})
		return
	case errors.Is(err, store.ErrNotFound):
		// Deleted between the lookup and the write: the same answer as any endpoint out of reach.
		writeAPIError(w, http.StatusNotFound, "not_found", "no endpoint by that name or id belongs to you", nil)
		return
	case err != nil:
		a.log.Error("api edit endpoint", "human", human.ID, "slug", found.Slug, "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}

	a.log.Info("api endpoint default lease set", "human", human.ID, "slug", found.Slug, "endpoint", found.ID,
		"default_lease_ttl_seconds", leaseLogValue(stored))
	if a.endpointLeaseChanged != nil {
		a.endpointLeaseChanged(found.ID, stored)
	}
	found.DefaultLeaseTTLSeconds = stored
	writeJSON(w, http.StatusOK, endpointOutFrom(found))
}

// leaseLogValue renders a stored default for a log record: the seconds, or "default".
func leaseLogValue(def *int) any {
	if def == nil {
		return "default"
	}
	return *def
}
