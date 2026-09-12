package web

// The OAuth consent surface (SPEC-0016 REQ "Authorization Code Flow With Consent"): GET
// /oauth/authorize renders the charm-web "authorize access" screen behind the human session, and
// its POST records the decision — approval mints a single-use, expiring, PKCE-bound authorization
// code (stored hashed) and redirects the MCP client back; denial returns the standard
// access_denied error. The request is bound to exactly ONE vended endpoint owned by the signed-in
// human, and every scope bullet on the screen derives from that endpoint's stored
// scope_queues/scope_verbs — the same columns the MCP scope guard enforces, so consent cannot
// advertise anything the guard would refuse. OAuth mechanics (PKCE rules, redirect matching, code
// minting) come from internal/oauthsrv; this file only orchestrates them into the page pattern.
// Governing: ADR-0019; SPEC-0015 REQ "Wizard Interaction Pattern" (full-page flow, plain forms).

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Authorize-request parameter bounds (SPEC-0016 "Security Requirements → Input Validation"): a
// legitimate request is a handful of short identifiers; anything past these bounds is abuse.
const (
	maxOAuthParamLen = 512  // client_id, state, endpoint slug, code_challenge
	maxOAuthURILen   = 2000 // redirect_uri, resource — matches the registration bound
)

// authorizeView is the render model for templates/authorize.html — the consent screen (design
// package "authorize access" surface) or, when the request is unsafe to bounce back to the
// client, its dead-end error state.
type authorizeView struct {
	Error string // non-redirectable request error; renders the error state instead of consent

	ClientName string   // the registered client_name ("Claude Desktop")
	Workspace  string   // the deployed workspace host the client wants into
	Principal  string   // the accountable principal (display name · email)
	Initials   string   // principal avatar initials
	Operator   bool     // true = operator grant (acts AS the signed-in human); false = endpoint grant
	AgentName  string   // the bound endpoint's agent (endpoint grants only)
	Slug       string   // the bound endpoint's public slug (endpoint grants only)
	Resource   string   // the RFC 8707 resource indicator, round-tripped through the form
	Bullets    []string // scope bullets DERIVED from the endpoint's stored scope (or the operator scope)

	// Hidden form fields: the validated request round-trips through the consent form and is
	// re-validated on POST, so the decision handler never trusts the rendered page.
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
}

// oauthAuthzRequest is a fully validated authorize request: a registered client, an exact-match
// redirect URI, S256 PKCE material, and the one grant principal — either the live vended endpoint
// (owned by the signed-in human) an MCP client connects through, or the signed-in human themselves
// for an operator grant (the CLI/API shape, ADR-0023).
type oauthAuthzRequest struct {
	Client        store.OAuthClient
	Endpoint      store.EndpointCard // zero value on an operator grant
	Operator      bool               // true = grant binds to the signed-in human, not an endpoint
	Resource      string             // the RFC 8707 resource indicator ("" when absent)
	RedirectURI   string
	State         string
	CodeChallenge string
}

// principalBinding returns the (endpointID, humanID) pair the grant stores: the endpoint's id for
// an endpoint grant, or the consenting human's id for an operator grant. Exactly one is non-empty,
// mirroring the schema's num_nonnulls constraint.
func (req oauthAuthzRequest) principalBinding(human store.Human) (string, string) {
	if req.Operator {
		return "", human.ID
	}
	return req.Endpoint.ID, ""
}

// oauthAuthzError is a validation failure that is SAFE to return to the client via its (already
// validated) redirect URI, as the standard error/error_description query parameters.
type oauthAuthzError struct {
	code, description string
}

func (e *oauthAuthzError) Error() string { return e.code + ": " + e.description }

// OAuthAuthorize renders the consent screen for a valid authorize request. Requires human — the
// router wraps this in auth.RequireHuman, so an anonymous hit is bounced to /login before any of
// this runs (SPEC-0016 scenario "Human absent"). Requests that cannot be safely bounced back to
// the client (unknown client_id, unregistered redirect_uri) render the dead-end error state;
// everything else invalid redirects with the standard OAuth error. Governing: SPEC-0016 REQ
// "Authorization Code Flow With Consent".
func (h *Handler) OAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	h.oauthConsent(w, r, &human, r.URL.Query())
}

// OAuthDecision executes the human's consent decision. The POST re-validates the entire request
// from its form fields — identically to the GET, never trusting the rendered page — then either
// mints the single-use code (approve) or returns access_denied (deny). CSRF is enforced by the
// route group's RequireCSRF. Requires human. Governing: SPEC-0016 REQ "Authorization Code Flow
// With Consent" ("Approval SHALL issue a single-use, expiring, PKCE-bound (S256) authorization
// code; denial SHALL return the standard error").
func (h *Handler) OAuthDecision(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req, authzErr, renderErr := h.parseAuthorizeRequest(r, &human, r.PostForm)
	if renderErr != "" {
		h.renderAuthorizeError(w, r, &human, renderErr)
		return
	}
	if authzErr != nil {
		h.redirectOAuthError(w, r, req, authzErr)
		return
	}

	if r.PostFormValue("decision") != "approve" {
		// Any non-approval is a denial: the standard error, nothing minted, nothing recorded.
		h.redirectOAuthError(w, r, req, &oauthAuthzError{"access_denied", "the operator denied the request"})
		return
	}

	code, codeHash, err := oauthsrv.MintCode()
	if err != nil {
		h.fail(w, err)
		return
	}
	// The stored row is the consent record: this client, onto this principal (the endpoint or, for
	// an operator grant, the signed-in human themselves), under this PKCE
	// challenge, redeemable at exactly this redirect URI, expiring on a clock consent cannot
	// extend. Only the hash is persisted; the plaintext rides the redirect and dies there.
	endpointID, humanID := req.principalBinding(human)
	if _, err := h.store.CreateOAuthCode(r.Context(), codeHash, req.Client.ClientID, endpointID, humanID,
		req.CodeChallenge, req.RedirectURI, time.Now().Add(oauthsrv.CodeTTL)); err != nil {
		h.fail(w, err)
		return
	}
	h.redirectOAuth(w, r, req.RedirectURI, url.Values{"code": {code}}, req.State)
}

// oauthConsent validates the request and renders the consent screen (or the appropriate error
// path). Shared shape with OAuthDecision so GET and POST can never diverge on what "valid" means.
func (h *Handler) oauthConsent(w http.ResponseWriter, r *http.Request, human *store.Human, params url.Values) {
	req, authzErr, renderErr := h.parseAuthorizeRequest(r, human, params)
	if renderErr != "" {
		h.renderAuthorizeError(w, r, human, renderErr)
		return
	}
	if authzErr != nil {
		h.redirectOAuthError(w, r, req, authzErr)
		return
	}

	// Approving this form 302s to the client's registered callback — another origin — and
	// form-action governs the redirect, not just the POST. Without this the browser refuses the
	// callback navigation and the minted code is never delivered. See CSPAllowingFormActionTo.
	w.Header().Set("Content-Security-Policy", CSPAllowingFormActionTo(req.RedirectURI))

	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	name := req.Client.Name
	if name == "" {
		name = "an MCP client"
	}
	h.render(w, "authorize", view{
		Title: "Authorize access", Human: human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		Authorize: &authorizeView{
			ClientName: name,
			Workspace:  h.workspaceHost(),
			Principal:  principalLabel(human),
			Initials:   initials(human),
			Operator:   req.Operator,
			AgentName:  req.Endpoint.AgentName,
			Slug:       req.Endpoint.Slug,
			Resource:   req.Resource,
			Bullets:    authorizeScopeBullets(req),

			ClientID:      req.Client.ClientID,
			RedirectURI:   req.RedirectURI,
			State:         req.State,
			CodeChallenge: req.CodeChallenge,
		},
	})
}

// parseAuthorizeRequest validates one authorize request (query on GET, form on POST) against the
// registered client, the PKCE profile, and the endpoint binding. Returns exactly one of:
//
//   - req with neither error: valid — render consent / execute the decision.
//   - renderErr != "": the request must dead-end on our own page (unknown client, or a redirect
//     URI that is not the registered exact match — redirecting there would BE the open redirect).
//   - authzErr != nil (req still carries the validated client/redirect): bounce the standard OAuth
//     error back to the client.
//
// Governing: SPEC-0016 REQ "Dynamic Client Registration" (redirect URIs "validated exactly ...
// again at authorization time"), REQ "Authorization Code Flow With Consent".
func (h *Handler) parseAuthorizeRequest(r *http.Request, human *store.Human, params url.Values) (oauthAuthzRequest, *oauthAuthzError, string) {
	var req oauthAuthzRequest
	for _, p := range []struct {
		name, val string
		max       int
	}{
		{"client_id", params.Get("client_id"), maxOAuthParamLen},
		{"state", params.Get("state"), maxOAuthParamLen},
		{"code_challenge", params.Get("code_challenge"), maxOAuthParamLen},
		{"endpoint", params.Get("endpoint"), maxOAuthParamLen},
		{"redirect_uri", params.Get("redirect_uri"), maxOAuthURILen},
		{"resource", params.Get("resource"), maxOAuthURILen},
	} {
		if len(p.val) > p.max {
			return req, nil, "the authorization request is malformed (" + p.name + " is too long)"
		}
	}

	clientID := params.Get("client_id")
	if clientID == "" {
		return req, nil, "the authorization request names no client_id"
	}
	client, err := h.store.OAuthClientByClientID(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		return req, nil, "unknown client — the client_id was never registered with this switchboard"
	}
	if err != nil {
		h.log.Error("oauth authorize: client lookup", "err", err)
		return req, nil, "the authorization request could not be validated — try again"
	}
	req.Client = client

	// Redirect URI: exact string match against the registered allowlist, nothing else. Absent is
	// tolerated only for a client with exactly one registered URI (RFC 6749 §3.1.2.3); any
	// mismatch dead-ends HERE — bouncing an error to an unregistered URI is an open redirect.
	redirectURI := params.Get("redirect_uri")
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	if !oauthsrv.RedirectAllowed(client, redirectURI) {
		return req, nil, "the redirect_uri does not exactly match one registered for this client"
	}
	req.RedirectURI = redirectURI
	req.State = params.Get("state")

	// From here every failure is safe to return to the client at its registered redirect URI.
	if rt := params.Get("response_type"); rt != "code" {
		return req, &oauthAuthzError{"unsupported_response_type", "only response_type=code is supported"}, ""
	}
	challenge := params.Get("code_challenge")
	if err := oauthsrv.ValidateCodeChallenge(challenge, params.Get("code_challenge_method")); err != nil {
		return req, &oauthAuthzError{"invalid_request", err.Error()}, ""
	}
	req.CodeChallenge = challenge

	// Grant binding: an operator grant (resource = base + "/api") binds to the signed-in human
	// themselves; anything else binds to the RFC 8707 resource indicator's (or bare endpoint
	// slug's) one live vended endpoint owned by that human. The operator resource is exact-matched
	// against the deployed base so a foreign origin can never be mistaken for it.
	res := params.Get("resource")
	req.Resource = res
	if res != "" && res == strings.TrimRight(h.cfg.BaseURL, "/")+OperatorResourcePath {
		req.Operator = true
	} else {
		slug := params.Get("endpoint")
		if slug == "" && res != "" {
			slug = oauthsrv.SlugFromResource(h.cfg.BaseURL, res)
		}
		if slug == "" || !oauthsrv.SlugOK(slug) {
			return req, &oauthAuthzError{"invalid_target",
				"the request must name a vended endpoint on this switchboard (resource or endpoint parameter)"}, ""
		}
		ep, err := h.store.EndpointBySlugOwned(r.Context(), slug, human.ID)
		if errors.Is(err, store.ErrNotFound) {
			// Unknown, revoked, expired, or another principal's endpoint — uniformly the same error, so
			// the response leaks nothing about which. Consent is the owner's alone (SPEC-0007).
			return req, &oauthAuthzError{"invalid_target",
				"no live vended endpoint by that name belongs to the signed-in operator"}, ""
		}
		if err != nil {
			h.log.Error("oauth authorize: endpoint lookup", "err", err)
			return req, nil, "the authorization request could not be validated — try again"
		}
		req.Endpoint = ep
	}
	return req, nil, ""
}

// renderAuthorizeError renders the dead-end consent error state (400): the request was too broken
// to safely return to the client. The message is one of our own fixed strings, never echoed input.
func (h *Handler) renderAuthorizeError(w http.ResponseWriter, r *http.Request, human *store.Human, msg string) {
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	h.renderStatus(w, http.StatusBadRequest, "authorize", view{
		Title: "Authorize access", Human: human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		Authorize: &authorizeView{Error: msg},
	})
}

// redirectOAuthError bounces the standard OAuth error document back to the client's validated
// redirect URI (error + error_description, state echoed). Only ever called with req.RedirectURI
// already exact-matched against the registration. Governing: SPEC-0016 ("denial SHALL return the
// standard error to the client").
func (h *Handler) redirectOAuthError(w http.ResponseWriter, r *http.Request, req oauthAuthzRequest, e *oauthAuthzError) {
	h.redirectOAuth(w, r, req.RedirectURI, url.Values{
		"error":             {e.code},
		"error_description": {e.description},
	}, req.State)
}

// redirectOAuth sends the client back to its validated redirect URI with the given parameters
// merged into the existing query (RFC 6749 §3.1.2: the URI's own components are retained). state
// is echoed exactly when present.
func (h *Handler) redirectOAuth(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Registered URIs are validated at registration; a parse failure here means stored data is
		// corrupt, not that the client erred.
		h.fail(w, err)
		return
	}
	q := u.Query()
	for k, vs := range params {
		q.Set(k, vs[0])
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// workspaceHost is the human-readable workspace identity on the consent screen — the deployed
// host the client is asking into ("connect to your switchboard workspace").
func (h *Handler) workspaceHost() string {
	if u, err := url.Parse(h.cfg.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimRight(h.cfg.BaseURL, "/")
}

// principalLabel renders the accountable principal line: display name and email when both exist,
// otherwise whichever is present.
func principalLabel(human *store.Human) string {
	switch {
	case human == nil:
		return ""
	case human.DisplayName != "" && human.Email != "":
		return human.DisplayName + " · " + human.Email
	case human.Email != "":
		return human.Email
	default:
		return human.DisplayName
	}
}

// OperatorResourcePath is the path suffix of the RFC 8707 resource indicator that requests an
// OPERATOR grant: an OAuth token that acts AS the signed-in human (register agents, vend
// endpoints) rather than a credential onto one vended endpoint. The CLI and the /api/v1 surface
// are its only consumers (ADR-0023).
const OperatorResourcePath = "/api"

// authorizeScopeBullets derives the consent screen's capability bullets: the operator scope for an
// operator grant, the bound endpoint's stored scope otherwise (via scopeBullets).
func authorizeScopeBullets(req oauthAuthzRequest) []string {
	if req.Operator {
		return []string{
			"register agents on this switchboard",
			"vend endpoints and see each credential exactly once",
			"list the endpoints you own",
		}
	}
	return scopeBullets(req.Endpoint.ScopeQueues, req.Endpoint.ScopeVerbs)
}

// scopeBullets derives the consent screen's capability bullets from an endpoint's STORED
// scope_queues/scope_verbs — the exact columns the MCP scope guard enforces — so the screen can
// never advertise anything the guard would refuse. Verbs are grouped into the tool families the
// agent surface serves (internal/mcp verbs.go), each family named only when at least one of its
// verbs is in scope and describing only the verbs that are; a verb outside every known family
// falls through verbatim rather than being dressed up. Governing: SPEC-0016 scenario "Consent
// reflects real enforcement" ("every bullet corresponds to that stored scope — nothing advertised
// that the scope guard would not actually allow").
func scopeBullets(queues, verbs []string) []string {
	has := func(v string) bool { return slices.Contains(verbs, v) }
	inScope := func(family []string) []string {
		var out []string
		for _, v := range family {
			if has(v) {
				out = append(out, v)
			}
		}
		return out
	}

	var bullets []string
	if has("list_todos") {
		bullets = append(bullets, "read todos on "+strings.Join(queues, " · "))
	}
	// The lease-bound lifecycle verbs (everything on the drain surface past reading).
	var lease []string
	for _, v := range []string{"claim", "complete", "fail", "heartbeat"} {
		if has(v) {
			lease = append(lease, v)
		}
	}
	if len(lease) > 0 {
		bullets = append(bullets, strings.Join(lease, " & ")+" under a lease")
	}
	if hooks := inScope(mcp.WebhookVerbs()); len(hooks) > 0 {
		bullets = append(bullets, "self-manage webhooks ("+strings.Join(hooks, " · ")+")")
	}
	if events := inScope(mcp.EventVerbs()); len(events) > 0 {
		bullets = append(bullets, "inspect event history & providers ("+strings.Join(events, " · ")+")")
	}
	// Anything outside the known families renders as itself — truthful, never embellished.
	known := mcp.AllVerbs()
	for _, v := range verbs {
		if !slices.Contains(known, v) {
			bullets = append(bullets, v)
		}
	}
	return bullets
}
