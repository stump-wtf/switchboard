package web

// The vend wizard: the first full-page wizard on the wizard machinery (wizard.go), replacing the
// SPEC-0013 vend modal. Vending walks agent → queues → verbs → lifetime → confirm as routed step
// pages with server-side state; the confirm step is the irreversible act (it mints the credential)
// and states so before executing; completion renders the one-time reveal. Re-vend — the SPEC-0007
// path for scope changes — starts the same wizard seeded from an existing endpoint's scope.
//
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (wizard steps incl. credential
// lifetime; one-time reveal + .mcp.json; immutable scope / re-vend doctrine), REQ "Wizard
// Interaction Pattern"; SPEC-0007 (vend semantics); SPEC-0016 REQ "Credential Lifetime".

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// vendWizard is the vend flow's wizard definition. The step order is the SPEC-0015 contract:
// agent → queues → verbs → lifetime → confirm (the confirm page is where the irreversible mint
// is stated and executed).
var vendWizard = wizardDef{
	name:  "vend",
	base:  "/endpoints/vend",
	steps: []string{"agent", "queues", "verbs", "webhooks", "lifetime", "confirm"},
}

// lifetimePresets is the lifetime step's preset vocabulary (SPEC-0016: lifetime chosen at vend
// time). Values are what parseLifetime speaks; "" (until revoked) and "custom" are rendered
// alongside these in the template.
var lifetimePresets = []lifetimePreset{
	{Value: "1h", Label: "1 hour"},
	{Value: "24h", Label: "24 hours"},
	{Value: "7d", Label: "7 days"},
	{Value: "30d", Label: "30 days"},
}

type lifetimePreset struct {
	Value string
	Label string
}

// vendChipOption is one toggle chip with its saved-state check (queues step).
type vendChipOption struct {
	Name    string
	Checked bool
}

// vendStepTab is one entry in the wizard's step tracker rail.
type vendStepTab struct {
	Slug    string
	Num     int
	Current bool
	Done    bool
}

// vendStepView is the render model for one vend-wizard step page (templates/vend.html). Every
// field prefills from the server-side wizard state, so Back navigation and revisits re-render the
// operator's entered values (SPEC-0015 value-preserving back nav).
type vendStepView struct {
	Step      string
	StepNum   int // 1-based
	StepTotal int
	Steps     []vendStepTab
	BackURL   string // "" on the first step
	ActionURL string
	Error     string // step validation error, re-rendered inline on the same page

	ReVendOf string // agent name this wizard re-vends ("" for a fresh vend)

	// persona step
	Name           string
	PersonaID      string
	PersonaOptions []vendPersonaOption

	// queues step
	QueueOptions []vendChipOption
	ExtraQueues  string // CSV of chosen queues the store did not already know

	// verbs step
	VerbOptions []vendVerbOption

	// webhooks step
	WebhookMax         string // text input for max self-managed webhooks ("" = 0 = disabled)
	WebhookSourceTypes []vendChipOption
	WebhookQueues      []vendChipOption // mirrors QueueOptions but checked independently
	WebhookQueuesExtra string

	// lifetime step
	LifetimePresets []lifetimePreset
	LifetimePreset  string // checked choice: "", one of lifetimePresets, or "custom"
	LifetimeCustom  string

	// confirm step summary
	PersonaName     string
	Queues          []string
	Verbs           []string
	WebhookMaxLabel string
	LifetimeLabel   string
}

// VendStart begins the vend wizard: it mints fresh server-side state (optionally seeded from an
// existing endpoint for the re-vend path) and redirects to the first step page. Requires human.
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (re-vend is the path for scope
// changes), REQ "Wizard Interaction Pattern".
func (h *Handler) VendStart(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	token, values, err := h.wizardBegin(w, vendWizard)
	if err != nil {
		h.fail(w, err)
		return
	}
	// Re-vend seeding: copy the source endpoint's name/persona/queues/verbs into the draft (never
	// its lifetime — expiry is re-chosen each vend). Ownership is enforced by listing only the
	// human's own cards; an unknown or foreign id simply starts an unseeded wizard.
	if from := r.URL.Query().Get("from"); from != "" {
		if eps, err := h.store.ListEndpointCards(r.Context(), human.ID); err != nil {
			h.log.Warn("vend wizard re-vend seed", "err", err)
		} else {
			for _, ep := range eps {
				if ep.ID == from {
					values.Set("name", ep.AgentName)
					if h.personasEnabled && ep.PersonaID != "" {
						values.Set("persona", ep.PersonaID)
					}
					values["queues"] = append([]string(nil), ep.ScopeQueues...)
					values["verbs"] = append([]string(nil), ep.ScopeVerbs...)
					values.Set("revend_of", ep.AgentName)
					break
				}
			}
		}
	}
	h.wizards.save(token, values)
	http.Redirect(w, r, vendWizard.stepPath(vendWizard.first()), http.StatusSeeOther)
}

// VendLegacyPersonaRedirect 303-redirects the old /endpoints/vend/persona route to /endpoints/vend/agent
// so bookmarks and back-links from before the step rename still resolve.
func (h *Handler) VendLegacyPersonaRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, vendWizard.stepPath("agent"), http.StatusSeeOther)
}

// VendStep renders one wizard step page from the server-side state. A request with no live wizard
// state (expired, cleared, or deep-linked cold) restarts the flow rather than rendering a
// half-broken page. Requires human.
func (h *Handler) VendStep(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	slug := chi.URLParam(r, "step")
	if vendWizard.index(slug) < 0 {
		http.NotFound(w, r)
		return
	}
	_, values, ok := h.wizardValues(r, vendWizard)
	if !ok {
		http.Redirect(w, r, vendWizard.base, http.StatusSeeOther)
		return
	}
	h.renderVendStep(w, r, &human, slug, values, "", http.StatusOK)
}

// VendStepSubmit validates and saves one step's fields into the server-side state, then advances
// (POST-redirect-GET, so Back/refresh never re-posts). The confirm step is the irreversible one:
// its POST executes the mint. A validation failure re-renders the SAME step with the entered
// values and an inline error — never a dead end. Requires human.
func (h *Handler) VendStepSubmit(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	slug := chi.URLParam(r, "step")
	if vendWizard.index(slug) < 0 {
		http.NotFound(w, r)
		return
	}
	token, values, ok := h.wizardValues(r, vendWizard)
	if !ok {
		http.Redirect(w, r, vendWizard.base, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	switch slug {
	case "agent":
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			h.renderVendStep(w, r, &human, slug, values, "an agent name is required", http.StatusBadRequest)
			return
		}
		values.Set("name", name)
		// The persona binding is only honored while personas are enabled — mirroring Vend, which
		// ignores a submitted persona when the capability is off.
		if h.personasEnabled {
			values.Set("persona", strings.TrimSpace(r.FormValue("persona")))
		}
	case "queues":
		queues := multiValues(r, "queues")
		queues = append(queues, splitCSV(r.FormValue("queues_extra"))...)
		queues = dedupe(queues)
		if len(queues) == 0 {
			h.renderVendStep(w, r, &human, slug, values, "at least one queue is required", http.StatusBadRequest)
			return
		}
		values["queues"] = queues
	case "verbs":
		verbs := dedupe(multiValues(r, "verbs"))
		if len(verbs) == 0 {
			h.renderVendStep(w, r, &human, slug, values, "at least one verb is required", http.StatusBadRequest)
			return
		}
		values["verbs"] = verbs
	case "webhooks":
		// The webhooks step is OPTIONAL — leaving it empty vends with webhook_max=0 (self-managed
		// webhooks disabled). The operator only fills it when they want the agent to create its
		// own webhooks (ADR-0012).
		webhookMax := strings.TrimSpace(r.FormValue("webhook_max"))
		if webhookMax != "" {
			if n, err := strconv.Atoi(webhookMax); err != nil || n < 0 {
				h.renderVendStep(w, r, &human, slug, values, "max webhooks must be a non-negative integer", http.StatusBadRequest)
				return
			}
		}
		sourceTypes := dedupe(multiValues(r, "webhook_source_types"))
		whQueues := multiValues(r, "webhook_queues")
		whQueues = append(whQueues, splitCSV(r.FormValue("webhook_queues_extra"))...)
		whQueues = dedupe(whQueues)
		values.Set("webhook_max", webhookMax)
		values["webhook_source_types"] = sourceTypes
		values["webhook_queues"] = whQueues
	case "lifetime":
		lifetime := strings.TrimSpace(r.FormValue("lifetime"))
		if lifetime == "custom" {
			lifetime = strings.TrimSpace(r.FormValue("lifetime_custom"))
		}
		if lifetime != "" {
			if _, err := parseLifetime(lifetime); err != nil {
				h.renderVendStep(w, r, &human, slug, values,
					"invalid lifetime — use a duration like 90m, 24h, 7d, or 4w", http.StatusBadRequest)
				return
			}
		}
		values.Set("lifetime", lifetime)
	case "confirm":
		// The irreversible step: mint from the SERVER-SIDE state (the confirm form carries only the
		// CSRF token). An incomplete draft bounces to its first unfinished step instead of 400ing.
		if missing := firstIncompleteVendStep(values); missing != "" {
			http.Redirect(w, r, vendWizard.stepPath(missing), http.StatusSeeOther)
			return
		}
		minted := h.executeVendOn(w, r, &human, vendSubmission{
			Name:               values.Get("name"),
			PersonaID:          values.Get("persona"),
			Queues:             values["queues"],
			Verbs:              values["verbs"],
			Lifetime:           values.Get("lifetime"),
			WebhookMax:         webhookMaxFromValues(values),
			WebhookSourceTypes: values["webhook_source_types"],
			WebhookQueues:      values["webhook_queues"],
		}, "endpoints")
		if minted {
			// The reveal is already on the wire, so only the server-side draft is dropped here (no
			// cookie header can follow the body). The orphaned cookie is harmless: any later wizard
			// request finds no state and restarts the flow — the one-time reveal stays one-time.
			h.wizards.drop(token)
		}
		return
	}

	h.wizards.save(token, values)
	http.Redirect(w, r, vendWizard.stepPath(vendWizard.next(slug)), http.StatusSeeOther)
}

// firstIncompleteVendStep names the earliest step whose required value is missing from the draft
// ("" when the draft is complete). The lifetime step is complete once visited or skipped — its
// empty value is the deliberate "until revoked" choice.
func firstIncompleteVendStep(values url.Values) string {
	if strings.TrimSpace(values.Get("name")) == "" {
		return "agent"
	}
	if len(values["queues"]) == 0 {
		return "queues"
	}
	if len(values["verbs"]) == 0 {
		return "verbs"
	}
	return ""
}

// renderVendStep builds the step view from the saved draft and renders the full step page.
func (h *Handler) renderVendStep(w http.ResponseWriter, r *http.Request, human *store.Human, slug string, values url.Values, errMsg string, status int) {
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	idx := vendWizard.index(slug)
	v := vendStepView{
		Step: slug, StepNum: idx + 1, StepTotal: len(vendWizard.steps),
		ActionURL: vendWizard.stepPath(slug),
		Error:     errMsg,
		ReVendOf:  values.Get("revend_of"),
	}
	if prev := vendWizard.prev(slug); prev != "" {
		v.BackURL = vendWizard.stepPath(prev)
	}
	for i, s := range vendWizard.steps {
		v.Steps = append(v.Steps, vendStepTab{Slug: s, Num: i + 1, Current: i == idx, Done: i < idx})
	}

	switch slug {
	case "agent":
		v.Name = values.Get("name")
		v.PersonaID = values.Get("persona")
		v.PersonaOptions = h.vendPersonaOptions(r, human.ID)
	case "queues":
		known := h.vendQueueOptions(r)
		chosen := values["queues"]
		for _, q := range known {
			v.QueueOptions = append(v.QueueOptions, vendChipOption{Name: q, Checked: slices.Contains(chosen, q)})
		}
		var extra []string
		for _, q := range chosen {
			if !slices.Contains(known, q) {
				extra = append(extra, q)
			}
		}
		v.ExtraQueues = strings.Join(extra, ", ")
	case "verbs":
		v.VerbOptions = vendVerbOptions()
		// Once the operator has made a verb choice, the saved set replaces the drain-verb defaults —
		// back navigation must show what was chosen, not re-check the defaults.
		if chosen := values["verbs"]; len(chosen) > 0 {
			for i := range v.VerbOptions {
				v.VerbOptions[i].Checked = slices.Contains(chosen, v.VerbOptions[i].Name)
			}
		}
	case "webhooks":
		v.WebhookMax = values.Get("webhook_max")
		knownSources := []string{"gitea", "github", "stripe", "slack", "cairn", "generic"}
		chosenSources := values["webhook_source_types"]
		for _, s := range knownSources {
			v.WebhookSourceTypes = append(v.WebhookSourceTypes, vendChipOption{Name: s, Checked: slices.Contains(chosenSources, s)})
		}
		knownQueues := h.vendQueueOptions(r)
		chosenWhQueues := values["webhook_queues"]
		for _, q := range knownQueues {
			v.WebhookQueues = append(v.WebhookQueues, vendChipOption{Name: q, Checked: slices.Contains(chosenWhQueues, q)})
		}
		var extra []string
		for _, q := range chosenWhQueues {
			if !slices.Contains(knownQueues, q) {
				extra = append(extra, q)
			}
		}
		v.WebhookQueuesExtra = strings.Join(extra, ", ")
	case "lifetime":
		v.LifetimePresets = lifetimePresets
		lifetime := values.Get("lifetime")
		v.LifetimePreset = lifetime
		if lifetime != "" && !slices.ContainsFunc(lifetimePresets, func(p lifetimePreset) bool { return p.Value == lifetime }) {
			v.LifetimePreset = "custom"
			v.LifetimeCustom = lifetime
		}
	case "confirm":
		v.Name = values.Get("name")
		v.Queues = values["queues"]
		v.Verbs = values["verbs"]
		v.PersonaName = h.personaNameByID(r, human.ID, values.Get("persona"))
		if wm := values.Get("webhook_max"); wm != "" {
			v.WebhookMaxLabel = wm
		} else {
			v.WebhookMaxLabel = "disabled"
		}
		if lifetime := values.Get("lifetime"); lifetime == "" {
			v.LifetimeLabel = "until revoked"
		} else {
			v.LifetimeLabel = lifetime
		}
	}

	h.renderStatus(w, status, "vend", view{
		Title: "Vend endpoint", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, PersonasEnabled: h.personasEnabled, Vend: &v,
	})
}

// personaNameByID resolves a persona id from the human's own personas to its display name for the
// confirm summary ("" = agent-level endpoint, or personas disabled). Resolution failures degrade
// to the id-less label; the vend transaction re-validates ownership regardless.
func (h *Handler) personaNameByID(r *http.Request, humanID, personaID string) string {
	if personaID == "" {
		return ""
	}
	for _, p := range h.vendPersonaOptions(r, humanID) {
		if p.ID == personaID {
			return p.Name
		}
	}
	return ""
}

// dedupe returns items with duplicates removed, first occurrence order preserved (checkbox chips
// plus the free-text field can name the same queue twice).
func dedupe(items []string) []string {
	var out []string
	for _, it := range items {
		if !slices.Contains(out, it) {
			out = append(out, it)
		}
	}
	return out
}

// webhookMaxFromValues parses the wizard state's webhook_max string into an int. Empty or invalid
// values return 0 (webhook self-management disabled).
func webhookMaxFromValues(values url.Values) int {
	if v := values.Get("webhook_max"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}
