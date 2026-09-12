package web

// The one-step quick vend: the whole grant on a single page — agent name, queue chips, verb
// chips, lifetime — submitted in one POST that mints and renders the one-time reveal inline, no
// wizard steps. It reuses the vend wizard's exact components (queue/verb chipsets, lifetime
// choices) and the same single mint path (executeVendOn), so the one-step page and the stepped
// wizard can never disagree about what a vend is. The stepped wizard remains the guided path; the
// quick page is the ADR-0023 basics path for operators who know the vocabulary.
// Governing: ADR-0023 (MVP basics); SPEC-0015 (component reuse, value-preserving re-render);
// SPEC-0007 (one-time credential reveal).

import (
	"net/http"
	"slices"
	"strings"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/store"
)

// quickVendView is the render model for templates/quickvend.html — one page, one form.
type quickVendView struct {
	ActionURL string
	Error     string // validation error, re-rendered inline with the entered values preserved

	Name            string
	QueueOptions    []vendChipOption
	LifetimePresets []lifetimePreset
	ExtraQueues     string
	VerbOptions     []vendVerbOption
	LifetimePreset  string
	LifetimeCustom  string
}

// QuickVendStart renders the one-step vend page. Requires human (the router's RequireHuman group).
func (h *Handler) QuickVendStart(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	h.renderQuickVend(w, r, &human, quickVendView{}, http.StatusOK)
}

// QuickVendSubmit validates the single form and mints through the shared vend path. A rejected
// submission re-renders the page with the error inline and every entered value preserved — the
// same value-preservation contract the wizard steps keep (SPEC-0015).
func (h *Handler) QuickVendSubmit(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	queues := dedupe(multiValues(r, "queues"))
	queues = append(queues, splitCSV(r.FormValue("queues_extra"))...)
	queues = dedupe(queues)
	verbs := dedupe(multiValues(r, "verbs"))
	lifetime := strings.TrimSpace(r.FormValue("lifetime"))
	if lifetime == "custom" {
		lifetime = strings.TrimSpace(r.FormValue("lifetime_custom"))
	}

	// Value preservation on a rejected submission (SPEC-0015): the known-queue chips re-render
	// with the operator's choices still checked, and only the queues the store does not know go
	// back into the free-text field.
	known := h.vendQueueOptions(r)
	v := quickVendView{Name: name, ExtraQueues: strings.Join(extraQueues(known, queues), ", ")}
	for _, q := range known {
		v.QueueOptions = append(v.QueueOptions, vendChipOption{Name: q, Checked: slices.Contains(queues, q)})
	}
	v.VerbOptions = vendVerbOptions()
	if chosen := multiValues(r, "verbs"); len(chosen) > 0 {
		for i := range v.VerbOptions {
			v.VerbOptions[i].Checked = slices.Contains(chosen, v.VerbOptions[i].Name)
		}
	}
	v.LifetimePreset = strings.TrimSpace(r.FormValue("lifetime"))
	v.LifetimeCustom = strings.TrimSpace(r.FormValue("lifetime_custom"))
	if v.LifetimePreset != "" && !slices.ContainsFunc(lifetimePresets, func(p lifetimePreset) bool { return p.Value == v.LifetimePreset }) {
		v.LifetimePreset = "custom"
	}

	fail := func(msg string) {
		v.Error = msg
		h.renderQuickVend(w, r, &human, v, http.StatusBadRequest)
	}
	switch {
	case name == "":
		fail("an agent name is required")
		return
	case len(queues) == 0:
		fail("at least one queue is required")
		return
	case len(verbs) == 0:
		fail("at least one verb is required")
		return
	}
	if lifetime != "" {
		if _, err := parseLifetime(lifetime); err != nil {
			fail("invalid lifetime — use a duration like 90m, 24h, 7d, or 4w")
			return
		}
	}

	h.executeVendOn(w, r, &human, vendSubmission{
		Name: name, Queues: queues, Verbs: verbs, Lifetime: lifetime,
	}, "quickvend")
}

// renderQuickVend renders the one-step page (with any reveal already set on the view by the mint).
func (h *Handler) renderQuickVend(w http.ResponseWriter, r *http.Request, human *store.Human, v quickVendView, status int) {
	v.ActionURL = "/endpoints/quick"
	v.LifetimePresets = lifetimePresets
	if v.VerbOptions == nil {
		v.VerbOptions = vendVerbOptions()
	}
	if v.QueueOptions == nil {
		for _, q := range h.vendQueueOptions(r) {
			v.QueueOptions = append(v.QueueOptions, vendChipOption{Name: q})
		}
	}
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	h.renderStatus(w, status, "quickvend", view{
		Title: "Quick vend", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, Quick: &v,
	})
}

// extraQueues returns the chosen queues the store does not already know (for the free-text field).
func extraQueues(known, chosen []string) []string {
	var out []string
	for _, q := range chosen {
		if !slices.Contains(known, q) {
			out = append(out, q)
		}
	}
	return out
}
