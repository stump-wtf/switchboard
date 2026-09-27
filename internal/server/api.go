package server

// The ADR-0023 operator API: registration vends the whole happy path in one call, over the SAME
// OAuth model as everything else.
//
// POST /api/v1/endpoints creates the agent, the vended endpoint (bearer credential + /mcp/{slug}
// URL), a scoped default queue, and a self-managed ingestion webhook bound to that queue — and
// returns every credential the operator needs in one response. The mint path reuses exactly the
// pieces the web vend wizard and the MCP create_webhook verb use (cred.Mint,
// store.VendAgentEndpoint, store.CreateWebhook), so the API is the same product surface without
// the session-gated UI.
//
// Auth is OAuth, full stop (ADR-0019/ADR-0023): every request presents an OAuth access token minted
// by an OPERATOR grant — the same authorization-code + PKCE flow the MCP surface uses, with
// resource = <base>/api — resolved to the signed-in human via store.HumanByOAuthToken. There is no
// static shared token: the CLI (cmd/switchboard) performs the flow gh-style and vends its own
// credentials, so the API's principal is always a real, accountable human.
//
// Governing: ADR-0023 REQ "Registration Vends the Whole Happy Path"; SPEC-0007 (credential hash at
// rest, one-time reveal, accountable principal); SPEC-0006 REQ "Switchboard Owns Secrets,
// Verification, and Idempotency"; ADR-0022 (endpoint-scoped ownership).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

// apiHandler serves the /api/v1 operator surface.
type apiHandler struct {
	st   *store.Store
	base string // cfg.BaseURL, no trailing slash
	log  *slog.Logger
	// endpointRevoked tears down a revoked endpoint's live MCP sessions. It is the SAME hook the
	// web UI's revoke and the expiry reaper ring: revoking over the API must not leave an agent
	// holding an open stream on a credential that no longer exists. Nil is tolerated (tests that
	// build the API without an MCP handler) and simply skips the teardown.
	endpointRevoked func(endpointID string)
	// replayGuard validates the replay targets a vend names (api_replay_targets.go): the shared SSRF
	// guard's defaults, the same rules replay_webhook_event applies. Governing: SPEC-0033 REQ "Owned
	// Replay Targets".
	replayGuard *push.Validator
}

func newAPIHandler(st *store.Store, baseURL string, log *slog.Logger, onEndpointRevoked func(string)) *apiHandler {
	return &apiHandler{st: st, base: strings.TrimRight(baseURL, "/"), log: log, endpointRevoked: onEndpointRevoked,
		replayGuard: push.New()}
}

// operatorKey is the API context key carrying the resolved operator principal.
type operatorKey struct{}

// operatorFromContext returns the authenticated operator human, if any.
func operatorFromContext(ctx context.Context) (store.Human, bool) {
	h, ok := ctx.Value(operatorKey{}).(store.Human)
	return h, ok
}

// oauthGuard resolves the presented OAuth access token to its human principal (an operator grant,
// minted with resource = <base>/api). Endpoint-bound tokens and static sbk_ bearers do not resolve
// here: the operator API is a human surface, never an agent one. Every failure is a plain 401 with
// no distinction between a missing, unknown, expired, or wrong-shape credential.
func (a *apiHandler) oauthGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		raw = strings.TrimSpace(raw)
		if !strings.EqualFold(scheme, "Bearer") || raw == "" || strings.HasPrefix(raw, "sbk_") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard-api"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		human, err := a.st.HumanByOAuthToken(r.Context(), cred.Hash(raw))
		if err != nil {
			// Unknown, expired, revoked, or wrong-shape credential — and a store failure fails
			// closed as the same plain 401 rather than an authenticated-by-accident 500. Only the
			// store failure is an operator-visible error; a rejected bearer is routine traffic.
			if errors.Is(err, store.ErrNotFound) {
				a.log.Debug("api bearer rejected")
			} else {
				a.log.Error("api bearer resolve", "err", err)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard-api"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operatorKey{}, human)))
	})
}

// Routes mounts the operator API. Every route requires a live operator OAuth bearer.
func (a *apiHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(a.oauthGuard, maxBytes(64<<10))
	r.Post("/endpoints", a.VendEndpoint)
	r.Get("/endpoints", a.ListEndpoints)
	r.Post("/endpoints/{ref}/revoke", a.RevokeEndpoint)
	r.Post("/endpoints/{ref}/todos", a.PushTodo)
	r.Get("/agents", a.ListAgents)
	return r
}

// --- request/response shapes ---

type vendEndpointIn struct {
	// Name is the agent name; the endpoint slug derives from it.
	Name string `json:"name"`
	// Queue is the single scoped queue the endpoint drains and the webhook routes to.
	// Defaults to "inbox".
	Queue string `json:"queue,omitempty"`
	// ReplayTargets are the replay destinations the endpoint owns (first = default for a replay that
	// names none). Optional; each must pass the SSRF guard. Governing: SPEC-0033 REQ "Owned Replay
	// Targets".
	ReplayTargets []string `json:"replay_targets,omitempty"`
}

type vendWebhookOut struct {
	WebhookID string `json:"webhook_id"`
	IngestURL string `json:"ingest_url"`
	// TrustMode is always "token" for the vended webhook: the unguessable ingest URL is the
	// credential. Governing: ADR-0003 (per-source trust model).
	TrustMode string `json:"trust_mode"`
}

type vendEndpointOut struct {
	AgentName     string         `json:"agent_name"`
	Slug          string         `json:"slug"`
	MCPURL        string         `json:"mcp_url"`
	Token         string         `json:"token"`
	Queue         string         `json:"queue"`
	Verbs         []string       `json:"verbs"`
	ReplayTargets []string       `json:"replay_targets"`
	Webhook       vendWebhookOut `json:"webhook"`
	// MCPJSON is the ready-to-paste .mcp.json stanza wiring an MCP client to the endpoint with the
	// credential embedded — byte-identical to the web reveal's wiring (internal/mcp).
	MCPJSON   json.RawMessage `json:"mcp_json,omitempty"`
	ExpiresAt *string         `json:"expires_at,omitempty"`
}

type agentOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type endpointOut struct {
	// ID is the endpoint's stable identifier. Slug is the friendly handle an operator reads and
	// types; ID is what the web UI's own routes use. Both address an endpoint on the revoke route.
	ID        string   `json:"id"`
	Slug      string   `json:"slug"`
	AgentName string   `json:"agent_name"`
	State     string   `json:"state"`
	Queues    []string `json:"queues"`
	Verbs     []string `json:"verbs"`
	// ReplayTargets are the endpoint's owned replay destinations, shown only to its owner.
	ReplayTargets []string `json:"replay_targets"`
	ExpiresAt     *string  `json:"expires_at,omitempty"`
}

// VendEndpoint is the one-call mint: agent + endpoint + queue + webhook. Every default is chosen so
// an operator POSTs a name and gets a working webhook→todo→doorbell loop back. Any failure after
// the endpoint mint but before the webhook mint leaves a working MCP-only endpoint (the operator
// can create the webhook over MCP), so the error is reported honestly rather than pretending the
// whole mint failed.
func (a *apiHandler) VendEndpoint(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	var in vendEndpointIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Queue = strings.TrimSpace(in.Queue)
	if in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if in.Queue == "" {
		in.Queue = "inbox"
	}
	// Owned replay targets are checked by the SSRF guard BEFORE anything is minted, so a refused
	// target leaves no agent and no endpoint. Governing: SPEC-0033 REQ "Owned Replay Targets".
	replayTargets, err := normalizeReplayTargets(r.Context(), a.replayGuard, in.ReplayTargets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The basics scope: mcp.AllVerbs() — the full todo-drain surface (the core scope an endpoint
	// exists to carry) plus webhook self-management and the event history — so the vended loop
	// needs no second call. Governing: SPEC-0006 REQ "Todo Drain Verbs".
	verbs := mcp.AllVerbs()

	token, hash, prefix, err := cred.Mint()
	if err != nil {
		a.fail(w, "mint credential", err)
		return
	}
	slug, err := store.MintSlug(in.Name)
	if err != nil {
		a.fail(w, "mint slug", err)
		return
	}

	// The basics webhook ceiling, shared with the one-step quick vend so the two basics paths
	// grant the same usable scope. Governing: ADR-0023, SPEC-0006.
	whMax, whSources, whQueues := mcp.BasicWebhookCeiling(verbs, []string{in.Queue})
	res, err := a.st.VendAgentEndpoint(r.Context(), store.VendParams{
		OwnerHumanID: human.ID, Name: in.Name,
		CredHash: hash, CredPrefix: prefix, Slug: slug,
		Queues: []string{in.Queue}, Verbs: verbs,
		WebhookMax:         whMax,
		WebhookSourceTypes: whSources,
		WebhookQueues:      whQueues,
		ReplayTargets:      replayTargets,
	})
	if err != nil {
		a.fail(w, "vend endpoint", err)
		return
	}
	ep := res.Endpoint

	// The ingestion webhook, minted through the same store path the MCP create_webhook verb uses.
	// Generic source type → token trust mode → no signing secret; the unguessable URL is the
	// credential. Governing: SPEC-0006, ADR-0012.
	ingestToken, err := mintAPIIngestToken()
	if err != nil {
		a.fail(w, "mint ingest token", err)
		return
	}
	webhook, err := a.st.CreateWebhook(r.Context(), ep.ID, "generic", in.Queue, "token", ingestToken, "", 1)
	if err != nil {
		// The endpoint exists and works over MCP; say so instead of a bare 500.
		a.log.Error("api vend webhook mint", "slug", ep.Slug, "err", err)
		writeJSON(w, http.StatusCreated, vendEndpointOut{
			AgentName: res.AgentName, Slug: ep.Slug,
			MCPURL: mcp.EndpointURL(a.base, ep.Slug),
			Token:  token, Queue: in.Queue, Verbs: ep.ScopeVerbs, ReplayTargets: replayTargets,
			MCPJSON: json.RawMessage(mcp.ClientConfigJSON(a.base, ep.Slug, token)),
		})
		return
	}

	var expiresAt *string
	if ep.ExpiresAt != nil {
		s := ep.ExpiresAt.UTC().Format(time.RFC3339)
		expiresAt = &s
	}
	writeJSON(w, http.StatusCreated, vendEndpointOut{
		AgentName: res.AgentName, Slug: ep.Slug,
		MCPURL: mcp.EndpointURL(a.base, ep.Slug),
		Token:  token, Queue: in.Queue, Verbs: ep.ScopeVerbs, ReplayTargets: replayTargets,
		Webhook:   vendWebhookOut{WebhookID: webhook.ID, IngestURL: a.base + "/webhooks/w/" + ingestToken, TrustMode: "token"},
		MCPJSON:   json.RawMessage(mcp.ClientConfigJSON(a.base, ep.Slug, token)),
		ExpiresAt: expiresAt,
	})
}

// ListEndpoints returns the authenticated operator's vended endpoints — the machine twin of the
// Endpoints view, WITHOUT credentials: tokens are revealed exactly once at mint (SPEC-0007).
func (a *apiHandler) ListEndpoints(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	cards, err := a.st.ListEndpointCards(r.Context(), human.ID)
	if err != nil {
		a.fail(w, "list endpoints", err)
		return
	}
	out := make([]endpointOut, 0, len(cards))
	for _, c := range cards {
		e := endpointOut{
			ID: c.ID, Slug: c.Slug, AgentName: c.AgentName, State: c.State,
			Queues: c.ScopeQueues, Verbs: c.ScopeVerbs, ReplayTargets: nonNilStrings(c.ReplayTargets),
		}
		if c.ExpiresAt != nil {
			s := c.ExpiresAt.UTC().Format(time.RFC3339)
			e.ExpiresAt = &s
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
}

// ListAgents returns the authenticated operator's registered agents.
func (a *apiHandler) ListAgents(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	agents, err := a.st.ListAgents(r.Context(), human.ID)
	if err != nil {
		a.fail(w, "list agents", err)
		return
	}
	out := make([]agentOut, 0, len(agents))
	for _, ag := range agents {
		out = append(out, agentOut{ID: ag.ID, Name: ag.Name, Description: ag.Description})
	}
	writeJSON(w, http.StatusOK, out)
}

// RevokeEndpoint kills one endpoint: its stored credential stops authenticating and its live MCP
// sessions are torn down immediately. {ref} is the slug an operator reads off `endpoint list`, or
// the endpoint id — both are accepted because the web UI addresses endpoints by id and a human
// addresses them by name, and making the caller convert between the two is a papercut with no
// security value (the lookup is scoped to the operator's own endpoints either way).
//
// Revocation is terminal and there is no un-revoke: SPEC-0007 says a changed scope means a new
// endpoint, not an edited one. Re-revoking an already-revoked endpoint is therefore reported as a
// conflict rather than a success, so a script cannot mistake "it was already dead" for "I killed
// it just now" — the two mean different things when you are rotating a leaked credential.
//
// Governing: SPEC-0007 REQ "Revoke = Kill the Endpoint"; ADR-0008.
func (a *apiHandler) RevokeEndpoint(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	found, ok := a.ownedEndpoint(w, r, "revoke endpoint")
	if !ok {
		return
	}
	if found.State != "active" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "endpoint is already " + found.State, "slug": found.Slug, "state": found.State})
		return
	}
	if err := a.st.RevokeEndpoint(r.Context(), found.ID, human.ID); err != nil {
		a.fail(w, "revoke endpoint", err)
		return
	}
	if a.endpointRevoked != nil {
		a.endpointRevoked(found.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": found.ID, "slug": found.Slug, "agent_name": found.AgentName, "state": "revoked"})
}

// ownedEndpoint resolves {ref} — the slug an operator reads, or the id — among the caller's OWN
// endpoints. Unknown and another operator's endpoints are the same answer, already written to w:
// the caller learns nothing about endpoints that are not theirs. Governing: ADR-0022, SPEC-0007
// REQ "Human as Accountable Principal".
func (a *apiHandler) ownedEndpoint(w http.ResponseWriter, r *http.Request, what string) (store.EndpointCard, bool) {
	human, _ := operatorFromContext(r.Context())
	ref := chi.URLParam(r, "ref")
	cards, err := a.st.ListEndpointCards(r.Context(), human.ID)
	if err != nil {
		a.fail(w, what, err)
		return store.EndpointCard{}, false
	}
	for _, c := range cards {
		if c.Slug == ref || c.ID == ref {
			return c, true
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": "no endpoint by that name or id belongs to you"})
	return store.EndpointCard{}, false
}

// --- operator hand-off (ADR-0026) ---

const (
	// pushSource is the source, family and trust mode of an operator hand-off's delivery event.
	pushSource = "operator"
	// maxPushTitle bounds a hand-off's title: it is the doorbell's one line, and a paragraph there
	// is a payload, not a title.
	maxPushTitle = 200
	// maxPushKey bounds the operator-supplied idempotency key — the ceiling the generic receivers
	// put on a sender-supplied delivery id.
	maxPushKey = 256
)

type pushTodoIn struct {
	// Queue must be one of the endpoint's vended queues; an endpoint that drains exactly one
	// needs none.
	Queue string `json:"queue,omitempty"`
	Title string `json:"title"`
	// Kind is what list_todos reports; defaults to "operator".
	Kind string `json:"kind,omitempty"`
	// Payload is optional JSON handed to the agent verbatim. The body decoder has already
	// validated it as JSON by the time it lands here.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Key makes the push idempotent within the endpoint: the same key returns the existing live
	// todo instead of minting another.
	Key string `json:"key,omitempty"`
}

type pushTodoOut struct {
	ID         string `json:"id"`
	EndpointID string `json:"endpoint_id"`
	Slug       string `json:"slug"`
	Queue      string `json:"queue"`
	State      string `json:"state"`
	// Created is false when Key matched a live todo: nothing was minted and nothing was rung.
	Created bool  `json:"created"`
	EventID int64 `json:"event_id"`
}

// PushTodo is the operator's hand: POST /api/v1/endpoints/{ref}/todos mints one todo on an
// endpoint the caller owns and rings its doorbell. It records the hand-off as a delivery event —
// source, family and trust mode "operator", verified, verify_detail naming the human — and fans it
// out through the same CreateEventTodos path an ingest receiver uses, so nothing downstream is
// special-cased: the store's doorbell hook fires because the event is verified (SPEC-0011 sender
// gate), the heartbeat sweep and the pull path apply their existing predicates, the board shows an
// operator badge, and history shows who handed the work over. The title and payload are the
// agent's untrusted input like any todo's. Governing: ADR-0026; SPEC-0011 scenario
// "Operator-authored todo is pushed"; ADR-0022 (owner-only, queue inside the vended scope).
//
// @justinabrahms 09/13/2026 - Added: the only way for a human to hand their own agent a todo was
// to impersonate a producer on its ingest URL, which recorded the hand-off as anonymous and
// unverified — exactly what the trust model exists to flag.
func (a *apiHandler) PushTodo(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	found, ok := a.ownedEndpoint(w, r, "push todo")
	if !ok {
		return
	}
	var in pushTodoIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		// The route's maxBytes(64 KiB) wraps the body, so a read past the cap surfaces as a
		// *http.MaxBytesError — a 413, told apart from a mistyped body's 400 the way the
		// friend-intake route splits them. Governing: SPEC-0012 REQ "Request Body Size Limits".
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body exceeds 64 KiB; put the detail in a smaller payload", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in.Title, in.Queue = strings.TrimSpace(in.Title), strings.TrimSpace(in.Queue)
	in.Kind, in.Key = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Key)
	switch {
	case in.Title == "":
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	case len(in.Title) > maxPushTitle:
		http.Error(w, fmt.Sprintf("title is over %d bytes; put the detail in payload", maxPushTitle), http.StatusBadRequest)
		return
	case len(in.Key) > maxPushKey:
		http.Error(w, fmt.Sprintf("key is over %d bytes", maxPushKey), http.StatusBadRequest)
		return
	}
	if in.Queue == "" {
		if len(found.ScopeQueues) != 1 {
			http.Error(w, "queue is required: the endpoint drains "+strings.Join(found.ScopeQueues, ", "), http.StatusBadRequest)
			return
		}
		in.Queue = found.ScopeQueues[0]
	}
	if !slices.Contains(found.ScopeQueues, in.Queue) {
		http.Error(w, fmt.Sprintf("queue %q is outside the endpoint's scope (%s)", in.Queue, strings.Join(found.ScopeQueues, ", ")), http.StatusBadRequest)
		return
	}
	if found.State != "active" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "endpoint is " + found.State, "slug": found.Slug, "state": found.State})
		return
	}
	if in.Kind == "" {
		in.Kind = pushSource
	}
	payload := []byte(in.Payload)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	// Without a key every push is new work. With one, the key is scoped to the endpoint so two
	// operators' keys — or one operator's keys on two endpoints — never collide (ADR-0022).
	key := in.Key
	if key == "" {
		key = rand.Text()
	}
	key = found.ID + ":" + key

	eventID, out, err := a.st.CreateEventTodos(r.Context(), store.EventInput{
		Source: pushSource, Family: pushSource, EventType: in.Kind, ExternalID: key,
		TrustMode: pushSource, Verified: true,
		VerifyDetail: "operator-authored over /api/v1 by " + operatorName(human) + " (" + human.ID + ")",
		ContentType:  "application/json", Headers: []byte(`{}`), Payload: payload, SourceIP: remoteIP(r),
		EndpointID: found.ID,
	}, []string{found.ID}, store.CreateTodoParams{
		Queue: in.Queue, Source: pushSource, Kind: in.Kind, Title: in.Title,
		Payload: payload, IdempotencyKey: key,
	})
	if err != nil {
		a.fail(w, "push todo", err)
		return
	}
	if len(out) != 1 {
		a.fail(w, "push todo", fmt.Errorf("fan-out minted %d todos for one target", len(out)))
		return
	}
	td := out[0]
	a.log.Info("api todo pushed", "human", human.ID, "slug", found.Slug, "todo_id", td.Todo.ID,
		"queue", td.Todo.Queue, "created", td.New)
	writeJSON(w, http.StatusCreated, pushTodoOut{
		ID: td.Todo.ID, EndpointID: found.ID, Slug: found.Slug, Queue: td.Todo.Queue,
		State: td.Todo.State, Created: td.New, EventID: eventID,
	})
}

// remoteIP is the caller's address without the port, the way the ingest receivers record it:
// events.source_ip is inet, and "host:port" is not an inet.
func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// operatorName is the readable half of a hand-off's attribution: display name, else email, else
// the OIDC subject. The human id follows it in verify_detail, so the name is a courtesy, not the key.
func operatorName(h store.Human) string {
	for _, s := range []string{h.DisplayName, h.Email, h.OIDCSubject} {
		if s != "" {
			return s
		}
	}
	return pushSource
}

func (a *apiHandler) fail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusBadRequest)
		return
	}
	a.log.Error("api "+what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// mintAPIIngestToken mirrors the MCP verb's ingest-token mint: 128 bits of CSPRNG hex, non-secret
// routing token for a token-trust webhook.
func mintAPIIngestToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: mint ingest token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
