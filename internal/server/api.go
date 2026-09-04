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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/store"
)

// apiHandler serves the /api/v1 operator surface.
type apiHandler struct {
	st   *store.Store
	base string // cfg.BaseURL, no trailing slash
	log  *slog.Logger
}

func newAPIHandler(st *store.Store, baseURL string, log *slog.Logger) *apiHandler {
	return &apiHandler{st: st, base: strings.TrimRight(baseURL, "/"), log: log}
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
}

type vendWebhookOut struct {
	WebhookID string `json:"webhook_id"`
	IngestURL string `json:"ingest_url"`
	// TrustMode is always "token" for the vended webhook: the unguessable ingest URL is the
	// credential. Governing: ADR-0003 (per-source trust model).
	TrustMode string `json:"trust_mode"`
}

type vendEndpointOut struct {
	AgentName string         `json:"agent_name"`
	Slug      string         `json:"slug"`
	MCPURL    string         `json:"mcp_url"`
	Token     string         `json:"token"`
	Queue     string         `json:"queue"`
	Verbs     []string       `json:"verbs"`
	Webhook   vendWebhookOut `json:"webhook"`
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
	Slug      string   `json:"slug"`
	AgentName string   `json:"agent_name"`
	State     string   `json:"state"`
	Queues    []string `json:"queues"`
	Verbs     []string `json:"verbs"`
	ExpiresAt *string  `json:"expires_at,omitempty"`
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

	// The basics scope: the full todo-drain surface (the core scope an endpoint exists to carry)
	// plus webhook self-management and the event history, so the vended loop needs no second call.
	verbs := append(append([]string{}, mcp.DrainVerbs()...), mcp.WebhookVerbs()...)
	verbs = append(verbs, mcp.EventVerbs()...)

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

	res, err := a.st.VendAgentEndpoint(r.Context(), store.VendParams{
		OwnerHumanID: human.ID, Name: in.Name,
		CredHash: hash, CredPrefix: prefix, Slug: slug,
		Queues: []string{in.Queue}, Verbs: verbs,
		WebhookMax:         1,
		WebhookSourceTypes: []string{"generic"},
		WebhookQueues:      []string{in.Queue},
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
			Token:  token, Queue: in.Queue, Verbs: ep.ScopeVerbs,
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
		Token:  token, Queue: in.Queue, Verbs: ep.ScopeVerbs,
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
			Slug: c.Slug, AgentName: c.AgentName, State: c.State,
			Queues: c.ScopeQueues, Verbs: c.ScopeVerbs,
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
