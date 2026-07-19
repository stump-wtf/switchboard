package web

// Full-page wizard machinery: routed step pages backed by SERVER-SIDE step state, shared by every
// create/vend/connect flow (the vend wizard is the first adopter; personas/providers wizards reuse
// this helper). A wizard is an ordered list of step slugs under one base path; the operator's
// entered values live server-side in an in-memory, TTL'd state table keyed by an opaque cookie
// token — never in hidden form fields — so back navigation re-renders any step with its values
// preserved and a no-JS visitor completes the identical flow with plain forms + redirects.
//
// Governing: SPEC-0015 REQ "Wizard Interaction Pattern" (full pages, server-side step state,
// value-preserving back nav, no-JS completion, confirm on irreversible steps), ADR-0018 (wizards
// are pages with server-side step state).

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wizardTTL bounds how long an abandoned wizard's server-side state lives. Long enough for a human
// to think through a scope choice mid-flow; short enough that abandoned starts cannot accumulate.
const wizardTTL = 30 * time.Minute

// maxWizardStates caps the in-memory state table so a hostile (but authenticated and rate-limited)
// client cannot grow it without bound; at the cap, beginning a new wizard evicts the entry closest
// to expiry — an active wizard is the last to go.
const maxWizardStates = 4096

// wizardDef describes one full-page wizard: its name (state cookie identity), base path, and
// ordered step slugs. Definitions are static package values; all mutable state lives in
// wizardStates.
type wizardDef struct {
	name  string   // cookie: "sb_wiz_" + name
	base  string   // e.g. "/endpoints/vend" — steps route as base + "/" + slug
	steps []string // ordered step slugs; the last step is the confirm/execute step
}

func (d wizardDef) cookieName() string { return "sb_wiz_" + d.name }

// index returns the 0-based position of slug, or -1 when the wizard has no such step.
func (d wizardDef) index(slug string) int {
	for i, s := range d.steps {
		if s == slug {
			return i
		}
	}
	return -1
}

func (d wizardDef) first() string { return d.steps[0] }

// prev returns the step before slug ("" on the first step — the template renders no Back link).
func (d wizardDef) prev(slug string) string {
	if i := d.index(slug); i > 0 {
		return d.steps[i-1]
	}
	return ""
}

// next returns the step after slug ("" on the last step — advancing past confirm is the execute).
func (d wizardDef) next(slug string) string {
	if i := d.index(slug); i >= 0 && i < len(d.steps)-1 {
		return d.steps[i+1]
	}
	return ""
}

func (d wizardDef) stepPath(slug string) string { return d.base + "/" + slug }

// wizardEntry is one wizard-in-progress: the operator's entered values plus its expiry.
type wizardEntry struct {
	values  url.Values
	expires time.Time
}

// wizardStates is the server-side step-state table: opaque token → entered values, in-memory with
// a TTL. In-memory is deliberate — wizard state is a draft, not a record; a restart merely asks
// the operator to restart an unfinished flow, and nothing secret or durable is ever staged here
// (the credential is minted only at the final confirm POST).
type wizardStates struct {
	mu  sync.Mutex
	m   map[string]wizardEntry
	ttl time.Duration
	now func() time.Time // injectable clock for expiry tests
}

func newWizardStates(ttl time.Duration) *wizardStates {
	return &wizardStates{m: make(map[string]wizardEntry), ttl: ttl, now: time.Now}
}

// begin mints a new empty wizard state and returns its opaque token. A crypto/rand failure is
// returned, never swallowed — a guessable token would let one session read another's draft scope.
func (s *wizardStates) begin() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("wizard: read random: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.m[token] = wizardEntry{values: url.Values{}, expires: s.now().Add(s.ttl)}
	return token, nil
}

// get returns a COPY of the state's values (callers mutate freely, then save), or ok=false when
// the token is unknown or expired.
func (s *wizardStates) get(token string) (url.Values, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[token]
	if !ok || s.now().After(e.expires) {
		return nil, false
	}
	cp := make(url.Values, len(e.values))
	for k, v := range e.values {
		cp[k] = append([]string(nil), v...)
	}
	return cp, true
}

// save stores values under an existing token and refreshes its TTL (each completed step buys the
// operator another full window). Saving to an unknown or already-expired token is a no-op — the
// caller already treats a missing state as "start over", and a save must never resurrect an
// expired draft.
func (s *wizardStates) save(token string, values url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[token]
	if !ok || s.now().After(e.expires) {
		return
	}
	s.m[token] = wizardEntry{values: values, expires: s.now().Add(s.ttl)}
}

// drop deletes a state (wizard completed or cancelled).
func (s *wizardStates) drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, token)
}

// pruneLocked evicts expired entries, then — only if the table is still at cap — the entry
// closest to expiry. Callers hold s.mu.
func (s *wizardStates) pruneLocked() {
	now := s.now()
	for tok, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, tok)
		}
	}
	for len(s.m) >= maxWizardStates {
		var oldest string
		var oldestAt time.Time
		for tok, e := range s.m {
			if oldest == "" || e.expires.Before(oldestAt) {
				oldest, oldestAt = tok, e.expires
			}
		}
		delete(s.m, oldest)
	}
}

// wizardBegin mints fresh server-side state for def and sets its token cookie (HttpOnly, Lax,
// path-scoped to the wizard so the token never rides along on unrelated requests).
func (h *Handler) wizardBegin(w http.ResponseWriter, def wizardDef) (string, url.Values, error) {
	token, err := h.wizards.begin()
	if err != nil {
		return "", nil, err
	}
	http.SetCookie(w, h.wizardCookie(def, token, wizardTTL))
	values, _ := h.wizards.get(token)
	return token, values, nil
}

// wizardValues resolves the request's wizard state for def. ok=false means no cookie, or an
// unknown/expired token — the caller redirects to the wizard start.
func (h *Handler) wizardValues(r *http.Request, def wizardDef) (string, url.Values, bool) {
	c, err := r.Cookie(def.cookieName())
	if err != nil {
		return "", nil, false
	}
	values, ok := h.wizards.get(c.Value)
	if !ok {
		return "", nil, false
	}
	return c.Value, values, true
}

// wizardClear drops the request's wizard state and expires its cookie (flow completed or
// cancelled; the draft must not survive to seed an unrelated later vend).
func (h *Handler) wizardClear(w http.ResponseWriter, r *http.Request, def wizardDef) {
	if c, err := r.Cookie(def.cookieName()); err == nil {
		h.wizards.drop(c.Value)
	}
	http.SetCookie(w, h.wizardCookie(def, "", -1))
}

// wizardCookie builds the state-token cookie for def: HttpOnly (no script access), SameSite=Lax,
// Secure when the deployment origin is https (mirroring the auth session cookie), and path-scoped
// to the wizard's base so the token is only presented to wizard routes.
func (h *Handler) wizardCookie(def wizardDef, value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     def.cookieName(),
		Value:    value,
		Path:     def.base,
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	}
}
