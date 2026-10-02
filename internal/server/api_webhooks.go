package server

// Human API: Webhooks, Their Routing Rules And Their Routes
//
// The first slice of SPEC-0035 on /api/v1: list the webhooks in the signed-in human's reach; read,
// replace and dry-run a webhook's routing rules; and list, add and remove its routes (the extra
// endpoints its deliveries fan out to). Both could previously be managed only over MCP, by the
// endpoint that owns the webhook and only when that endpoint's scope carried the verbs, so a webhook
// owned by an endpoint vended before those verbs existed, or whose credential nobody kept, had rules
// and routes no credential could edit. These routes are authorized by the human instead (ADR-0022:
// the endpoint's human is the authorization principal): a webhook is in reach when its owning
// endpoint belongs to the caller, and every other webhook is the same 404 as one that does not exist.
//
// The route routes run internal/manage's route checks with the same human principal: any webhook in
// reach, and a target that is the human's own active endpoint or one an approved friend edge lets
// them deliver to. Every refused target is one 403 forbidden. Adding a route to a webhook whose
// endpoint is revoked is 409; listing and removing still work (SPEC-0035 REQ "Route Management").
//
// The rule routes run internal/manage, the code the MCP rule verbs run, with a human principal:
// the same grant, validation, save-time dry run and row-locked compare-and-write, and the same
// response shape as list_webhook_rules. The one difference is PUT without a params key, which keeps
// the stored params (SPEC-0026 REQ-4's rule) where set_webhook_rules still clears them today;
// "params": null or {} clears.
// The MCP verbs' endpoint-to-endpoint isolation (ADR-0038 F19) is untouched: this is a separate,
// human-scoped surface, and an endpoint credential never reaches it (oauthGuard refuses sbk_).
//
// Governing: SPEC-0035 REQ "Human API Surface", REQ "Reach on Every Route", REQ "Shared
// Implementation With MCP", REQ "Rule Management", REQ "Route Management", REQ "Work Orders on the
// Human API", REQ "Error Handling Standards"; ADR-0023; ADR-0022; ADR-0024; ADR-0038.
//
// @joestump-agent 09/29/2026 - Added GET /webhooks and GET/PUT /webhooks/{id}/rules plus POST
// /webhooks/{id}/rules/test, with a per-human rate limit and SPEC-0035's JSON error shape.
//
// @joestump-agent 10/02/2026 - Added GET /webhooks/{id}/routes, PUT and DELETE
// /webhooks/{id}/routes/{endpoint_id}, and POST /webhooks/{id}/routes with a target_endpoint_id
// body (#555). ruleErr became manageErr, which also maps forbidden.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/manage"
	"github.com/stump-wtf/switchboard/internal/routing"
)

// rulesBodyLimit is the body cap for the rule write and dry-run routes: 32 rules of 4 KiB each plus
// 16 KiB of params exceed the API's usual 64 KiB (SPEC-0035 "Request Body Size Limits").
const rulesBodyLimit = 256 << 10

// webhookRoutes mounts the webhook routes. It is called from Routes after oauthGuard, so every
// request here carries an operator human. Reads and writes draw from separate per-human buckets
// (SPEC-0035 "Rate Limiting"): a dry run and a save both evaluate tenant rules in the sandbox.
func (a *apiHandler) webhookRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(maxBytes(64<<10), a.perHuman(a.readRL))
		r.Get("/webhooks", a.ListWebhooks)
		r.Get("/webhooks/{webhook_id}/rules", a.GetWebhookRules)
		r.Get("/webhooks/{webhook_id}/routes", a.ListWebhookRoutes)
	})
	r.Group(func(r chi.Router) {
		r.Use(maxBytes(rulesBodyLimit), a.perHuman(a.writeRL))
		r.Put("/webhooks/{webhook_id}/rules", a.SetWebhookRules)
		r.Post("/webhooks/{webhook_id}/rules/test", a.TestWebhookRules)
	})
	// Route writes carry at most a target id, so they keep the API's usual 64 KiB cap.
	r.Group(func(r chi.Router) {
		r.Use(maxBytes(64<<10), a.perHuman(a.writeRL))
		r.Put("/webhooks/{webhook_id}/routes/{endpoint_id}", a.AddWebhookRoute)
		r.Post("/webhooks/{webhook_id}/routes", a.AddWebhookRoute)
		r.Delete("/webhooks/{webhook_id}/routes/{endpoint_id}", a.RemoveWebhookRoute)
	})
}

// webhookListOut is one webhook in reach. It never carries the ingest URL or a secret: both were
// revealed once, to the endpoint that owns the webhook, at create or rotate.
type webhookListOut struct {
	WebhookID        string `json:"webhook_id"`
	EndpointID       string `json:"endpoint_id"`
	EndpointSlug     string `json:"endpoint_slug"`
	EndpointState    string `json:"endpoint_state"`
	AgentName        string `json:"agent_name"`
	SourceType       string `json:"source_type"`
	TargetQueue      string `json:"target_queue"`
	TrustMode        string `json:"trust_mode"`
	RuleCount        int    `json:"rule_count"`
	HasDefaultAction bool   `json:"has_default_action"`
	HasParams        bool   `json:"has_params"`
	CreatedAt        string `json:"created_at"`
	RotatedAt        string `json:"rotated_at,omitempty"`
}

// ListWebhooks is GET /api/v1/webhooks: every webhook whose owning endpoint belongs to the caller,
// active endpoints first.
func (a *apiHandler) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	rows, err := a.st.ListWebhooksForHuman(r.Context(), human.ID)
	if err != nil {
		a.log.Error("api list webhooks", "human", human.ID, "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	out := make([]webhookListOut, 0, len(rows))
	for _, wh := range rows {
		row := webhookListOut{
			WebhookID: wh.ID, EndpointID: wh.EndpointID, EndpointSlug: wh.EndpointSlug,
			EndpointState: wh.EndpointState, AgentName: wh.AgentName, SourceType: wh.SourceType,
			TargetQueue: wh.TargetQueue, TrustMode: wh.TrustMode, RuleCount: wh.RuleCount,
			HasDefaultAction: wh.HasDefault, HasParams: wh.HasParams,
			CreatedAt: wh.CreatedAt.UTC().Format(time.RFC3339),
		}
		if wh.RotatedAt != nil {
			row.RotatedAt = wh.RotatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

// GetWebhookRules is GET /api/v1/webhooks/{webhook_id}/rules: list_webhook_rules' shape.
func (a *apiHandler) GetWebhookRules(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := chi.URLParam(r, "webhook_id")
	out, err := a.rules.Get(r.Context(), manage.HumanPrincipal(human.ID), id)
	if err != nil {
		a.manageErr(w, r, "get webhook rules", id, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setRulesIn is PUT's body. Rules is a pointer so an absent or null "rules" is refused rather than
// read as "remove every rule". Params stays raw so "absent" (keep the stored params) and null (clear
// them) can be told apart. Every other field of a GET response (webhook_id, grant, …) is ignored, so
// `switchboard webhook rules get --json` output is a valid body as it stands.
type setRulesIn struct {
	Rules         *[]manage.RuleIO `json:"rules"`
	DefaultAction *manage.ActionIO `json:"default_action"`
	Params        json.RawMessage  `json:"params"`
}

// SetWebhookRules is PUT /api/v1/webhooks/{webhook_id}/rules: set_webhook_rules through the full
// save path. A body without a params key keeps the stored params; "params": null (or {}) clears them.
func (a *apiHandler) SetWebhookRules(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := chi.URLParam(r, "webhook_id")
	var in setRulesIn
	if !decodeAPIBody(w, r, &in) {
		return
	}
	if in.Rules == nil {
		writeAPIError(w, http.StatusBadRequest, string(manage.ErrInvalidArgument),
			"rules is required: send the complete ordered list, or [] to remove every rule", nil)
		return
	}
	rep := manage.Replacement{Rules: *in.Rules, DefaultAction: in.DefaultAction}
	switch strings.TrimSpace(string(in.Params)) {
	case "":
		rep.KeepParams = true
	case "null":
		// An explicit clear.
	default:
		if err := json.Unmarshal(in.Params, &rep.Params); err != nil {
			writeAPIError(w, http.StatusBadRequest, string(manage.ErrInvalidArgument),
				"params must be a JSON object, or null to clear them", nil)
			return
		}
	}
	out, change, err := a.rules.Replace(r.Context(), manage.HumanPrincipal(human.ID), id, rep)
	if err != nil {
		a.manageErr(w, r, "set webhook rules", id, err)
		return
	}
	// One record per save. A save that adds, removes or keeps a work-order rule says which: a work
	// order is switchboard vouching for a delivery's provenance (SPEC-0035 REQ "Work Orders on the
	// Human API").
	attrs := []any{"human", human.ID, "webhook", out.WebhookID, "surface", "api",
		"rules", len(out.Rules), "params_kept", rep.KeepParams}
	if added, removed, kept := manage.WorkOrderDelta(change.Previous, change.Saved); len(added)+len(removed)+len(kept) > 0 {
		attrs = append(attrs, "work_order_added", added, "work_order_removed", removed, "work_order_kept", kept)
	}
	a.log.Info("api webhook rules replaced", attrs...)
	writeJSON(w, http.StatusOK, out)
}

// TestWebhookRules is POST /api/v1/webhooks/{webhook_id}/rules/test: test_webhook_rules. It saves
// nothing, and an event_id must be one of this webhook's own deliveries.
func (a *apiHandler) TestWebhookRules(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := chi.URLParam(r, "webhook_id")
	var in manage.TestIn
	if !decodeAPIBody(w, r, &in) {
		return
	}
	out, err := a.rules.Test(r.Context(), manage.HumanPrincipal(human.ID), id, in)
	if err != nil {
		a.manageErr(w, r, "test webhook rules", id, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListWebhookRoutes is GET /api/v1/webhooks/{webhook_id}/routes: list_webhook_routes' shape, the
// owner endpoint plus the explicit routes.
func (a *apiHandler) ListWebhookRoutes(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := chi.URLParam(r, "webhook_id")
	out, err := a.routes.List(r.Context(), manage.HumanPrincipal(human.ID), id)
	if err != nil {
		a.manageErr(w, r, "list webhook routes", id, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// addRouteIn is POST /api/v1/webhooks/{webhook_id}/routes' body, add_webhook_route's argument.
type addRouteIn struct {
	TargetEndpointID string `json:"target_endpoint_id"`
}

// AddWebhookRoute is add_webhook_route, idempotent either way it is spelled: PUT
// /api/v1/webhooks/{webhook_id}/routes/{endpoint_id} (SPEC-0035's route), or POST
// /api/v1/webhooks/{webhook_id}/routes with {"target_endpoint_id": …} (#555's). A target the human
// may not route to is 403 forbidden, one answer for an unknown, revoked or unfriended endpoint.
func (a *apiHandler) AddWebhookRoute(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := chi.URLParam(r, "webhook_id")
	target := chi.URLParam(r, "endpoint_id")
	if r.Method == http.MethodPost {
		var in addRouteIn
		if !decodeAPIBody(w, r, &in) {
			return
		}
		target = in.TargetEndpointID
	}
	out, err := a.routes.Add(r.Context(), manage.HumanPrincipal(human.ID), id, target)
	if err != nil {
		a.manageErr(w, r, "add webhook route", id, err)
		return
	}
	// A route hands this webhook's deliveries to another endpoint, possibly another human's, so every
	// grant is on the record (an existing route re-granted included: the call is idempotent).
	a.log.Info("api webhook route added", "human", human.ID, "webhook", out.WebhookID,
		"target_endpoint", out.TargetEndpointID, "surface", "api")
	writeJSON(w, http.StatusOK, out)
}

// RemoveWebhookRoute is DELETE /api/v1/webhooks/{webhook_id}/routes/{endpoint_id}:
// remove_webhook_route. It succeeds whether or not the route existed, and for the owner endpoint
// (always a target) it changes nothing.
func (a *apiHandler) RemoveWebhookRoute(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := chi.URLParam(r, "webhook_id")
	out, err := a.routes.Remove(r.Context(), manage.HumanPrincipal(human.ID), id, chi.URLParam(r, "endpoint_id"))
	if err != nil {
		a.manageErr(w, r, "remove webhook route", id, err)
		return
	}
	a.log.Info("api webhook route removed", "human", human.ID, "webhook", out.WebhookID,
		"target_endpoint", out.TargetEndpointID, "surface", "api")
	writeJSON(w, http.StatusOK, out)
}

// decodeAPIBody decodes a JSON body, answering 413 past the route's cap and 400 for anything that
// does not decode (a work_order given as an object included: it is computed at ingest, never input).
func decodeAPIBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, string(manage.ErrInvalidArgument), "request body is too large", nil)
			return false
		}
		writeAPIError(w, http.StatusBadRequest, string(manage.ErrInvalidArgument), "invalid JSON body: "+err.Error(), nil)
		return false
	}
	return true
}

// manageErr maps a rule or route failure onto SPEC-0035's status table and error shape. The code is
// the one the MCP verb answers with for the same input, so a script can treat both surfaces alike.
func (a *apiHandler) manageErr(w http.ResponseWriter, r *http.Request, what, webhookID string, err error) {
	human, _ := operatorFromContext(r.Context())
	var me *manage.Error
	if errors.As(err, &me) {
		status := http.StatusInternalServerError
		switch me.Kind {
		case manage.ErrInvalidArgument:
			status = http.StatusBadRequest
		case manage.ErrNotFound, manage.ErrRuleNotFound:
			status = http.StatusNotFound
		case manage.ErrForbidden:
			status = http.StatusForbidden
			// One answer for every refused target; which one it was goes to the log only.
			a.log.Warn("api "+what+" refused", "human", human.ID, "webhook", webhookID, "reason", me.Reason)
		case manage.ErrConflict:
			status = http.StatusConflict
		case manage.ErrUnavailable:
			status = http.StatusServiceUnavailable
			a.log.Warn("api "+what+" unavailable", "human", human.ID, "webhook", webhookID, "cause", me.Msg)
		}
		var extra map[string]any
		if me.State != "" {
			extra = map[string]any{"state": me.State}
		}
		writeAPIError(w, status, me.Code(), me.Msg, extra)
		return
	}
	var ve *routing.ValidationError
	if errors.As(err, &ve) {
		// A rule the router refuses names itself; not_granted is the house forbidden code, exactly
		// as the MCP verbs answer it.
		if ve.Code == routing.CodeNotGranted {
			writeAPIError(w, http.StatusForbidden, "forbidden", ve.Error(), nil)
			return
		}
		writeAPIError(w, http.StatusBadRequest, ve.Code, ve.Error(), nil)
		return
	}
	a.log.Error("api "+what, "human", human.ID, "webhook", webhookID, "err", err)
	writeAPIError(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

// writeAPIError is SPEC-0035's one error shape: {"error": <human-readable>, "code": <MCP code>},
// plus any extra fields a route names.
func writeAPIError(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// perHuman throttles a route group by the authenticated human, answering 429 with a Retry-After in
// the API's error shape. It runs after oauthGuard, so the human is always present.
func (a *apiHandler) perHuman(rl *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			human, _ := operatorFromContext(r.Context())
			if !rl.allow("human:" + human.ID) {
				w.Header().Set("Retry-After", rl.retryAfter())
				writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded; retry after the Retry-After interval", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
