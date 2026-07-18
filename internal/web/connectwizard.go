package web

// The connect-provider wizard: full-page routed steps over the runtime provider registry
// (ADR-0020), on the shared wizard machinery (wizard.go). Webhook path: choose source → choose
// trust mode (`signed` only where a real scheme exists; `token` the default for generic senders;
// `open` only behind an explicit acknowledgement of the risk — ADR-0003's tiering made an operator
// step) → provide/generate the secret → receive the copyable ingestion URL, with a generated token
// shown exactly once in the standard reveal pattern. Queue path: choose an implemented adapter
// (Redis) → connection settings (stream/list/pubsub topology). Both paths end with the provider
// registered, enabled, and visible on the Providers view — webhook lines are live on the very next
// request (dispatch resolves the registry per request).
//
// Governing: SPEC-0017 REQ "Connect Provider Wizard" (scenarios "Homelab sender lands on token",
// "Open requires intent"), REQ "Runtime Provider Registry" (scenario "Wizard-created provider is
// live immediately"); SPEC-0015 REQ "Wizard Interaction Pattern" (full pages, server-side step
// state, value-preserving back nav, no-JS completion); ADR-0020, ADR-0003.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// connectWizard is the connect-provider flow's wizard definition. steps is the full slug
// vocabulary; the ACTIVE path through it branches on the draft (connectPathSteps): webhook kinds
// walk source → trust → secret → confirm (open skips secret — it holds none by design), the queue
// kind walks source → settings → confirm.
var connectWizard = wizardDef{
	name:  "connect",
	base:  "/providers/connect",
	steps: []string{"source", "trust", "secret", "settings", "confirm"},
}

// signedSchemeKinds are the webhook kinds with a REAL signing scheme — the only kinds the wizard
// offers `signed` for (SPEC-0017: signed only where a real scheme exists). Their route is
// /webhooks/<kind>, so their registry name IS the kind (single instance).
var signedSchemeKinds = map[string]bool{"github": true, "stripe": true, "slack": true}

// connectReservedNames are registry names the wizard refuses for generic/redis lines: "connect"
// would shadow this wizard's own routes under /providers/…, and the signed kind names would
// collide with the signed dispatch routes under /webhooks/… (a generic row named "github" would
// wedge the real GitHub line).
var connectReservedNames = map[string]bool{"connect": true, "github": true, "stripe": true, "slack": true}

// providerNameRe bounds wizard-created provider names: they become URL path segments
// (/webhooks/generic/<name>) and DOM ids (sb-pr-<name>), so the charset is deliberately narrow.
var providerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// redisModes is the queue path's transport-mode vocabulary (SPEC-0002 Redis reference modes).
var redisModes = map[string]bool{"stream": true, "list": true, "pubsub": true}

// connectKindOption is one source-step choice card, sourced from the same implementedCatalog the
// Providers view renders — the wizard can only ever offer what the backend implements (SPEC-0017
// REQ "Provider Catalog": the UI never fakes a backend).
type connectKindOption struct {
	Kind    string
	Title   string
	Family  string
	Trust   string // the trust mode connecting enforces (or defaults to, for generic)
	Desc    string
	Checked bool
}

// connectStepView is the render model for one connect-wizard step page (templates/connect.html).
// Fields prefill from the server-side draft so Back navigation re-renders entered values — except
// staged secrets, which are never echoed back into markup (SecretStaged tells the operator a value
// is held). Governing: SPEC-0015 (value-preserving back nav), SPEC-0017 (secrets never render).
type connectStepView struct {
	Step      string
	StepNum   int // 1-based position on the ACTIVE path
	StepTotal int
	Steps     []vendStepTab // tracker tabs (shared shape with the vend wizard)
	BackURL   string        // "" on the first step
	ActionURL string
	Error     string // step validation error, re-rendered inline on the same page

	// source step
	Name        string
	Kind        string
	KindOptions []connectKindOption

	// trust step
	TrustMode  string // draft choice; token is the pre-checked default for generic senders
	OpenAck    bool
	SignedKind bool // trust is fixed to signed (real scheme exists) — no choice to make

	// secret step
	SecretMode   string // generate | provide (token kinds; signed kinds always provide)
	SecretStaged bool   // a secret is already held server-side (never echoed into the input)

	// settings step (queue path)
	QueueMode string // stream | list | pubsub
	Target    string // the stream/list/channel to consume
	Queue     string // target todo queue ("" = route by source name)

	// confirm step summary
	Family        string
	TrustLabel    string
	SecretSummary string
	IngestPath    string // webhook ingestion path ("" for the queue family)
	Topology      string // queue path: "stream <target>" etc.
}

// connectDoneView is the wizard's completion page: the provider is registered and enabled, and
// this response is the ONE place a generated token ever renders (standard one-time reveal
// pattern). Governing: SPEC-0017 scenario "Homelab sender lands on token".
type connectDoneView struct {
	Name      string
	Kind      string
	Family    string
	TrustMode string
	Path      string // webhook ingestion path
	URL       string // absolute, copyable ingestion URL
	Token     string // generated shared-secret token, shown exactly once ("" = none to reveal)
	// SecretProvided: the operator supplied the secret themselves — it is sealed and never
	// re-rendered, not even here.
	SecretProvided bool
	Topology       string // queue path summary
	Queue          string // target todo queue label
}

// connectKindByName resolves a source-step kind against the implemented catalog (nil = not an
// implemented, connectable kind — the wizard refuses it; catalog-only kinds have no connect path).
func connectKindByName(kind string) *catalogCard {
	for i := range implementedCatalog {
		if implementedCatalog[i].Kind == kind {
			return &implementedCatalog[i]
		}
	}
	return nil
}

// connectPathSteps is the ACTIVE step path for a draft: the queue kind swaps the trust+secret
// steps for connection settings, and an open webhook skips the secret step (none by design).
func connectPathSteps(values url.Values) []string {
	switch {
	case values.Get("kind") == "redis":
		return []string{"source", "settings", "confirm"}
	case values.Get("trust") == "open":
		return []string{"source", "trust", "confirm"}
	default:
		return []string{"source", "trust", "secret", "confirm"}
	}
}

// connectNextStep returns the step after slug on the draft's active path (falling back to the
// first incomplete step when slug is off-path — e.g. after the kind changed under back nav).
func connectNextStep(slug string, values url.Values) string {
	path := connectPathSteps(values)
	for i, s := range path {
		if s == slug && i < len(path)-1 {
			return path[i+1]
		}
	}
	if missing := firstIncompleteConnectStep(values); missing != "" {
		return missing
	}
	return "confirm"
}

// firstIncompleteConnectStep names the earliest active-path step whose required value is missing
// from the draft ("" when the draft is ready to confirm).
func firstIncompleteConnectStep(values url.Values) string {
	kind := values.Get("kind")
	if connectKindByName(kind) == nil || values.Get("name") == "" {
		return "source"
	}
	if kind == "redis" {
		if !redisModes[values.Get("mode")] || values.Get("target") == "" {
			return "settings"
		}
		return ""
	}
	trust := values.Get("trust")
	switch trust {
	case "open":
		return ""
	case "signed":
		if values.Get("secret") == "" {
			return "secret"
		}
		return ""
	case "token":
		if values.Get("secret_mode") == "provide" && values.Get("secret") == "" {
			return "secret"
		}
		if values.Get("secret_mode") == "" {
			return "secret"
		}
		return ""
	default:
		return "trust"
	}
}

// ConnectStart begins the connect wizard: fresh server-side state (optionally seeded with a kind
// from a catalog card's connect link) and a redirect to the source step. Requires human.
// Governing: SPEC-0017 REQ "Connect Provider Wizard"; SPEC-0015 REQ "Wizard Interaction Pattern".
func (h *Handler) ConnectStart(w http.ResponseWriter, r *http.Request) {
	token, values, err := h.wizardBegin(w, connectWizard)
	if err != nil {
		h.fail(w, err)
		return
	}
	// Catalog seeding: only implemented kinds are honored — an unknown/catalog-only ?kind starts an
	// unseeded wizard rather than erroring (the source step is where the choice is made anyway).
	if kind := r.URL.Query().Get("kind"); connectKindByName(kind) != nil {
		values.Set("kind", kind)
		if signedSchemeKinds[kind] {
			// Signed kinds connect at /webhooks/<kind>: the registry name IS the kind.
			values.Set("name", kind)
			values.Set("trust", "signed")
		}
	}
	h.wizards.save(token, values)
	http.Redirect(w, r, connectWizard.stepPath(connectWizard.first()), http.StatusSeeOther)
}

// ConnectStep renders one wizard step page from the server-side draft. No live state (expired,
// cleared, cold deep link) restarts the flow; an off-path slug (the draft branched away from it)
// bounces to the first incomplete step instead of rendering a half-truth. Requires human.
func (h *Handler) ConnectStep(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	slug := chi.URLParam(r, "step")
	if connectWizard.index(slug) < 0 {
		http.NotFound(w, r)
		return
	}
	_, values, ok := h.wizardValues(r, connectWizard)
	if !ok {
		http.Redirect(w, r, connectWizard.base, http.StatusSeeOther)
		return
	}
	if !onConnectPath(slug, values) {
		next := firstIncompleteConnectStep(values)
		if next == "" {
			next = "confirm"
		}
		http.Redirect(w, r, connectWizard.stepPath(next), http.StatusSeeOther)
		return
	}
	h.renderConnectStep(w, r, &human, slug, values, "", http.StatusOK)
}

// onConnectPath reports whether slug belongs to the draft's active step path.
func onConnectPath(slug string, values url.Values) bool {
	for _, s := range connectPathSteps(values) {
		if s == slug {
			return true
		}
	}
	return false
}

// ConnectStepSubmit validates and saves one step's fields into the server-side draft, then
// advances along the active path (POST-redirect-GET). The confirm POST executes the registration.
// A validation failure re-renders the SAME step with an inline error — never a dead end.
// Requires human + CSRF (the enclosing route group). Governing: SPEC-0017 REQ "Connect Provider
// Wizard"; SPEC-0015 REQ "Wizard Interaction Pattern".
func (h *Handler) ConnectStepSubmit(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	slug := chi.URLParam(r, "step")
	if connectWizard.index(slug) < 0 {
		http.NotFound(w, r)
		return
	}
	token, values, ok := h.wizardValues(r, connectWizard)
	if !ok {
		http.Redirect(w, r, connectWizard.base, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	switch slug {
	case "source":
		if msg := h.submitConnectSource(r, values); msg != "" {
			h.renderConnectStep(w, r, &human, slug, values, msg, http.StatusBadRequest)
			return
		}
	case "trust":
		if values.Get("kind") == "redis" {
			// The queue path has no trust step — the broker connection IS the trust (ADR-0003).
			http.Redirect(w, r, connectWizard.stepPath(connectNextStep("source", values)), http.StatusSeeOther)
			return
		}
		if msg := submitConnectTrust(r, values); msg != "" {
			h.renderConnectStep(w, r, &human, slug, values, msg, http.StatusBadRequest)
			return
		}
	case "secret":
		if !onConnectPath("secret", values) {
			http.Redirect(w, r, connectWizard.stepPath(connectNextStep("trust", values)), http.StatusSeeOther)
			return
		}
		if msg := submitConnectSecret(r, values); msg != "" {
			h.renderConnectStep(w, r, &human, slug, values, msg, http.StatusBadRequest)
			return
		}
	case "settings":
		if values.Get("kind") != "redis" {
			http.Redirect(w, r, connectWizard.stepPath(connectNextStep("source", values)), http.StatusSeeOther)
			return
		}
		if msg := submitConnectSettings(r, values); msg != "" {
			h.renderConnectStep(w, r, &human, slug, values, msg, http.StatusBadRequest)
			return
		}
	case "confirm":
		// The execute step: register from the SERVER-SIDE draft (the confirm form carries only the
		// CSRF token). An incomplete draft bounces to its first unfinished step instead of 400ing.
		if missing := firstIncompleteConnectStep(values); missing != "" {
			http.Redirect(w, r, connectWizard.stepPath(missing), http.StatusSeeOther)
			return
		}
		h.executeConnect(w, r, &human, token, values)
		return
	}

	h.wizards.save(token, values)
	http.Redirect(w, r, connectWizard.stepPath(connectNextStep(slug, values)), http.StatusSeeOther)
}

// submitConnectSource validates the source step (kind + registry name), mutating the draft on
// success and returning an inline error message otherwise. Changing the kind clears every
// downstream choice — a stale trust/secret/topology from another kind must never survive into the
// confirm summary.
func (h *Handler) submitConnectSource(r *http.Request, values url.Values) string {
	kind := strings.TrimSpace(r.FormValue("kind"))
	if connectKindByName(kind) == nil {
		return "choose a source — only implemented kinds connect (catalog-only kinds have no wizard path)"
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if signedSchemeKinds[kind] {
		// The signed route is /webhooks/<kind>: the name is the kind, whatever was typed.
		name = kind
	}
	if name == "" {
		return "a provider name is required — it becomes the line's registry name and URL segment"
	}
	if !providerNameRe.MatchString(name) {
		return "provider names are 1–64 chars: lowercase letters, digits, - and _, starting with a letter or digit"
	}
	if !signedSchemeKinds[kind] && connectReservedNames[name] {
		return "that name is reserved — it would collide with an existing route"
	}
	// Best-effort duplicate check for early feedback; the create itself is the race-safe guard
	// (SeedProvider is create-if-absent, so a collision can never clobber an existing line).
	if _, err := h.store.GetAdapter(r.Context(), name); err == nil {
		return "a provider named " + name + " is already connected — remove it first or pick another name"
	} else if !errors.Is(err, store.ErrNotFound) {
		h.log.Warn("connect wizard duplicate check", "err", err)
	}
	if values.Get("kind") != kind {
		for _, k := range []string{"trust", "open_ack", "secret", "secret_mode", "mode", "target", "queue"} {
			values.Del(k)
		}
		if signedSchemeKinds[kind] {
			values.Set("trust", "signed")
		}
	}
	values.Set("kind", kind)
	values.Set("name", name)
	return ""
}

// submitConnectTrust validates the trust step. Signed kinds are fixed to signed (the only real
// scheme); generic senders choose token (the default) or open — and open REQUIRES the explicit
// acknowledgement checkbox before the wizard proceeds. Governing: SPEC-0017 scenario "Open
// requires intent"; ADR-0003 (token as the tier between signed and open).
func submitConnectTrust(r *http.Request, values url.Values) string {
	if signedSchemeKinds[values.Get("kind")] {
		values.Set("trust", "signed")
		values.Del("open_ack")
		return ""
	}
	mode := r.FormValue("trust")
	switch mode {
	case "token":
		values.Del("open_ack")
	case "open":
		if r.FormValue("open_ack") != "1" {
			return "open means no verification at all — anyone who can reach the URL can inject events. Acknowledge the risk to proceed."
		}
		values.Set("open_ack", "1")
		// Open holds no secret by design: drop any staged one so the summary stays truthful.
		values.Del("secret")
		values.Del("secret_mode")
	default:
		return "choose a trust mode"
	}
	values.Set("trust", mode)
	return ""
}

// submitConnectSecret validates the secret step. Token kinds default to a server-generated secret
// (minted at the confirm execute, revealed once); providing one requires a non-empty value unless
// a secret is already staged from a previous visit (a blank re-submit keeps it — the input never
// echoes staged material). Signed kinds always provide the external scheme's signing secret.
func submitConnectSecret(r *http.Request, values url.Values) string {
	secret := strings.TrimSpace(r.FormValue("secret"))
	if values.Get("trust") == "signed" {
		if secret == "" && values.Get("secret") == "" {
			return "the signing secret is required — it is what deliveries are verified against"
		}
		if secret != "" {
			values.Set("secret", secret)
		}
		values.Set("secret_mode", "provide")
		return ""
	}
	mode := r.FormValue("secret_mode")
	if mode != "provide" {
		mode = "generate"
	}
	if mode == "generate" {
		values.Del("secret")
	} else if secret == "" && values.Get("secret") == "" {
		return "provide the shared-secret token, or choose generate"
	} else if secret != "" {
		values.Set("secret", secret)
	}
	values.Set("secret_mode", mode)
	return ""
}

// submitConnectSettings validates the queue path's Redis connection settings: transport mode plus
// the stream/list/channel to consume; the target todo queue is optional (empty routes by source
// name — StoreSink semantics). The broker DSN is deliberately NOT collected: it is a secret and
// lives in SWITCHBOARD_REDIS_URL, never the registry (ADR-0014).
func submitConnectSettings(r *http.Request, values url.Values) string {
	mode := r.FormValue("mode")
	if !redisModes[mode] {
		return "choose a consume mode — stream (recommended), list, or pub/sub"
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if target == "" {
		return "name the " + redisTargetNoun(mode) + " to consume"
	}
	if len(target) > 128 {
		return "the " + redisTargetNoun(mode) + " name is too long (128 chars max)"
	}
	queue := strings.TrimSpace(r.FormValue("queue"))
	if len(queue) > 128 {
		return "the todo queue name is too long (128 chars max)"
	}
	values.Set("mode", mode)
	values.Set("target", target)
	values.Set("queue", queue)
	return ""
}

// redisTargetNoun names the consume target per mode, for labels and errors.
func redisTargetNoun(mode string) string {
	switch mode {
	case "list":
		return "list"
	case "pubsub":
		return "channel"
	default:
		return "stream"
	}
}

// executeConnect performs the registration from a complete draft: seal-and-create the registry row
// (create-if-absent — a name collision re-renders the source step and creates nothing), then
// render the completion page with the copyable ingestion URL and, for a generated token, the
// one-time reveal. The row is created ENABLED, so a webhook line accepts (and trust-checks) calls
// on the very next request. Governing: SPEC-0017 REQ "Connect Provider Wizard" (ends registered,
// enabled, visible), REQ "Runtime Provider Registry"; ADR-0020 (secrets through the envelope).
func (h *Handler) executeConnect(w http.ResponseWriter, r *http.Request, human *store.Human, token string, values url.Values) {
	kind := values.Get("kind")
	name := values.Get("name")
	seed := store.ProviderSeed{Name: name, Kind: kind}
	var generated string

	if kind == "redis" {
		seed.Family, seed.TrustMode = "queue", "queue"
		cfg := map[string]string{"transport": "redis", "mode": values.Get("mode")}
		cfg[redisTargetKey(values.Get("mode"))] = values.Get("target")
		if q := values.Get("queue"); q != "" {
			cfg["queue"] = q
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			h.fail(w, fmt.Errorf("connect wizard: marshal config: %w", err))
			return
		}
		seed.Config = b
	} else {
		seed.Family = "webhook"
		seed.TrustMode = values.Get("trust")
		switch {
		case seed.TrustMode == "open":
			// none by design
		case seed.TrustMode == "token" && values.Get("secret_mode") != "provide":
			// The default path: mint here, at the execute — never staged in wizard state.
			s, err := mintProviderSecret()
			if err != nil {
				h.fail(w, err)
				return
			}
			generated, seed.Secret = s, s
		default:
			seed.Secret = values.Get("secret")
		}
		b, err := json.Marshal(map[string]string{"queue": name})
		if err != nil {
			h.fail(w, fmt.Errorf("connect wizard: marshal config: %w", err))
			return
		}
		seed.Config = b
	}

	created, err := h.store.SeedProvider(r.Context(), seed)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !created {
		// Lost the race (or the early check was skipped): nothing was clobbered — re-render the
		// source step so the operator picks another name.
		h.renderConnectStep(w, r, human, "source", values,
			"a provider named "+name+" already exists — pick another name", http.StatusConflict)
		return
	}

	done := connectDoneView{
		Name: name, Kind: kind, Family: seed.Family, TrustMode: seed.TrustMode,
		Token:          generated,
		SecretProvided: seed.Secret != "" && generated == "",
	}
	if seed.Family == "webhook" {
		done.Path = webhookIngestPath(name, seed.TrustMode)
		done.URL = strings.TrimRight(h.cfg.BaseURL, "/") + done.Path
	} else {
		done.Topology = values.Get("mode") + " " + values.Get("target")
		done.Queue = values.Get("queue")
		if done.Queue == "" {
			done.Queue = values.Get("target")
		}
	}

	sh, _ := h.buildShell(r.Context(), "providers", human)
	h.render(w, "connect", view{
		Title: "Connect provider", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, ConnectDone: &done,
	})
	// The completion (and its one-time token) is already on the wire; drop the server-side draft so
	// nothing can re-render it. The orphaned cookie is harmless — the next wizard request finds no
	// state and restarts the flow.
	h.wizards.drop(token)
}

// redisTargetKey maps a consume mode to its RegistryConfig field name.
func redisTargetKey(mode string) string {
	switch mode {
	case "list":
		return "list"
	case "pubsub":
		return "channel"
	default:
		return "stream"
	}
}

// webhookIngestPath is the ingestion path a webhook trust mode dispatches on — the same mapping
// providerLineFrom renders on the view.
func webhookIngestPath(name, trustMode string) string {
	if trustMode == "signed" {
		return "/webhooks/" + name
	}
	return "/webhooks/generic/" + name
}

// connectTrustLabel is the confirm summary's honest one-line description of a trust mode
// (ADR-0003 vocabulary — token is never presented as signed).
func connectTrustLabel(trust string) string {
	switch trust {
	case "signed":
		return "signed — HMAC-verified per delivery"
	case "token":
		return "token — caller authenticated; body not verified"
	case "open":
		return "open — no verification at all"
	default:
		return "queue — the broker connection is the trust"
	}
}

// renderConnectStep builds the step view from the saved draft and renders the full step page.
func (h *Handler) renderConnectStep(w http.ResponseWriter, r *http.Request, human *store.Human, slug string, values url.Values, errMsg string, status int) {
	sh, _ := h.buildShell(r.Context(), "providers", human)
	path := connectPathSteps(values)
	idx := 0
	for i, s := range path {
		if s == slug {
			idx = i
		}
	}
	v := connectStepView{
		Step: slug, StepNum: idx + 1, StepTotal: len(path),
		ActionURL: connectWizard.stepPath(slug),
		Error:     errMsg,
		Kind:      values.Get("kind"),
		Name:      values.Get("name"),
	}
	if idx > 0 {
		v.BackURL = connectWizard.stepPath(path[idx-1])
	}
	for i, s := range path {
		v.Steps = append(v.Steps, vendStepTab{Slug: s, Num: i + 1, Current: i == idx, Done: i < idx})
	}

	switch slug {
	case "source":
		for _, c := range implementedCatalog {
			v.KindOptions = append(v.KindOptions, connectKindOption{
				Kind: c.Kind, Title: c.Title, Family: c.Family, Trust: c.Trust, Desc: c.Desc,
				// Generic is the pre-checked default: the pit of success for homelab senders.
				Checked: c.Kind == v.Kind || (v.Kind == "" && c.Kind == "generic"),
			})
		}
	case "trust":
		v.SignedKind = signedSchemeKinds[v.Kind]
		v.TrustMode = values.Get("trust")
		if v.TrustMode == "" {
			v.TrustMode = "token" // the default for generic senders (SPEC-0017)
		}
		v.OpenAck = values.Get("open_ack") == "1"
	case "secret":
		v.TrustMode = values.Get("trust")
		v.SecretMode = values.Get("secret_mode")
		if v.SecretMode == "" && v.TrustMode != "signed" {
			v.SecretMode = "generate" // the default: minted server-side, revealed once
		}
		v.SecretStaged = values.Get("secret") != ""
	case "settings":
		v.QueueMode = values.Get("mode")
		if v.QueueMode == "" {
			v.QueueMode = "stream" // the RECOMMENDED durable mode (SPEC-0002)
		}
		v.Target = values.Get("target")
		v.Queue = values.Get("queue")
	case "confirm":
		if v.Kind == "redis" {
			v.Family, v.TrustMode = "queue", "queue"
			v.Topology = values.Get("mode") + " " + values.Get("target")
			v.Queue = values.Get("queue")
			if v.Queue == "" {
				v.Queue = values.Get("target")
			}
			v.SecretSummary = "none — the broker DSN lives in the environment, never the registry"
		} else {
			v.Family = "webhook"
			v.TrustMode = values.Get("trust")
			v.IngestPath = webhookIngestPath(v.Name, v.TrustMode)
			v.Queue = v.Name
			switch {
			case v.TrustMode == "open":
				v.SecretSummary = "none by design — open verifies nothing"
			case v.TrustMode == "token" && values.Get("secret_mode") != "provide":
				v.SecretSummary = "generated on connect · shown exactly once on the next page"
			default:
				v.SecretSummary = "provided · sealed through the envelope · never shown again"
			}
		}
		v.TrustLabel = connectTrustLabel(v.TrustMode)
	}

	h.renderStatus(w, status, "connect", view{
		Title: "Connect provider", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, Connect: &v,
	})
}
