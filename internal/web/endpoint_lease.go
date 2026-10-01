package web

// Endpoint Default Lease in the Operator UI
//
// The vend paths (quick vend, the wizard's lifetime step, the direct POST) take an optional default
// claim lease, and every active endpoint card carries a small form that edits it in place: the lease
// is not scope (SPEC-0007), so changing it needs no re-vend. The input speaks the vocabulary the CLI
// speaks, bare seconds or a Go duration ("3600", "45m", "1h"); blank or "default" means the server
// default. The server validates the bounds; a refused edit re-renders the Endpoints page with the
// error and the entered value on that card's form, never a dead-end page.
//
// Governing: ADR-0043, SPEC-0015 REQ "Endpoints View And Vend Wizard", SPEC-0007 REQ "Endpoint
// Default Lease", SPEC-0012 (CSRF on every session mutation; same-origin redirects only).
//
// @joestump-agent 10/01/2026 - Added.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/lease"
	"github.com/stump-wtf/switchboard/internal/store"
)

// leaseHelp is the range the lease inputs state under the field, built from the bounds.
var leaseHelp = fmt.Sprintf("blank = server default (%ds) · %ds to %ds (24h) · seconds or a duration like 45m, 1h",
	lease.DefaultSeconds, lease.MinDefaultSeconds, lease.MaxSeconds)

// leaseCardLabel states an endpoint's effective default lease and whose it is.
func leaseCardLabel(def *int) string {
	if def == nil {
		return lease.Label(lease.DefaultSeconds) + " · server default"
	}
	return lease.Label(*def) + " · set for this endpoint"
}

// parseLeaseInput reads an operator's default-lease input into the stored form: nil for the server
// default (blank or "default"), else whole seconds within the bounds. A refusal is returned as the
// message the form shows; "" means the input is good.
func parseLeaseInput(in string) (*int, string) {
	in = strings.TrimSpace(in)
	if in == "" || strings.EqualFold(in, "default") {
		return nil, ""
	}
	seconds, err := lease.ParseSeconds(in)
	if err != nil {
		return nil, "invalid default lease — use seconds or a duration like 45m or 1h, or leave it blank for the server default"
	}
	if err := lease.ValidateDefault(seconds); err != nil {
		return nil, fmt.Sprintf("default lease must be from %ds to %ds (24h); %q is %ds",
			lease.MinDefaultSeconds, lease.MaxSeconds, in, seconds)
	}
	return &seconds, ""
}

// SetEndpointLease is POST /endpoints/{id}/lease, the card's lease form. The store binds ownership in
// the write: another human's endpoint, or an unknown id, is a plain 404 and changes nothing; the
// owner's revoked endpoint (whose card shows no form) is 409. On success it returns to the card.
func (h *Handler) SetEndpointLease(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	input := strings.TrimSpace(r.FormValue("default_lease"))
	seconds, msg := parseLeaseInput(input)
	if msg != "" {
		h.renderLeaseError(w, r, &human, id, input, msg)
		return
	}
	stored, err := h.store.SetEndpointDefaultLeaseForHuman(r.Context(), id, human.ID, seconds)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "endpoint is revoked; its settings can no longer change", http.StatusConflict)
		return
	case errors.Is(err, lease.ErrOutOfRange):
		// parseLeaseInput already checked the bounds; this is the store's own guard answering.
		h.renderLeaseError(w, r, &human, id, input, "default lease out of range")
		return
	case err != nil:
		h.fail(w, err)
		return
	}
	value := "default"
	if stored != nil {
		value = strconv.Itoa(*stored)
	}
	h.log.Info("web endpoint default lease set", "human", human.ID, "endpoint", id, "default_lease_ttl_seconds", value)
	if h.endpointLeaseChanged != nil {
		h.endpointLeaseChanged(id, stored)
	}
	http.Redirect(w, r, "/endpoints#sb-ep-"+id, http.StatusSeeOther)
}

// renderLeaseError re-renders the Endpoints view with 400, the refusal and the entered value on the
// edited card's lease form. An id that is not one of the human's cards is a plain 404, so a refused
// edit cannot be used to probe for other humans' endpoints.
func (h *Handler) renderLeaseError(w http.ResponseWriter, r *http.Request, human *store.Human, id, input, msg string) {
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	cards := h.endpointCards(r, human)
	found := false
	for i := range cards {
		if cards[i].ID == id && cards[i].State == "active" {
			cards[i].LeaseInput, cards[i].LeaseError = input, msg
			found = true
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.renderStatus(w, http.StatusBadRequest, "endpoints", view{
		Title: "Endpoints", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, EndpointCards: cards, PersonasEnabled: h.personasEnabled,
	})
}
