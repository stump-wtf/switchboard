package mcp

// Webhook Routing Verbs
//
// The ADR-0022 webhook ROUTING verbs served over MCP: add_webhook_route, list_webhook_routes,
// remove_webhook_route. A webhook's deliveries always land as a todo pinned to the webhook's OWNING
// endpoint; a route adds a second (third, …) target endpoint, and the receiver mints one
// endpoint-owned todo per target. Every fan-out target is an explicit, human-authorized endpoint id
// rather than a queue name two tenants happen to share, so routing never reintroduces the
// shared-queue-string leak ADR-0022 exists to kill.
//
// Authorization, in the order it is enforced (all before any mutation):
//
//  1. VERB SCOPE — scopeGuard (tools.go) refuses a routing verb outside the endpoint's allowlist
//     with the stable "forbidden" code. These verbs are members of webhookVerbs (verbs.go).
//  2. WEBHOOK OWNERSHIP — the webhook's owning endpoint must be the CALLING endpoint itself
//     (store.WebhookOwnerEndpointFor matches endpoint_webhooks.endpoint_id against the caller).
//     Another endpoint of the same human does not own it: a friend endpoint is vended on its
//     approver's agent, so human-level ownership would let a friend re-route the approver's
//     deliveries (ADR-0038, SPEC-0033 F3/F19; TestRuleAndRouteVerbsStayOnTheirOwnWebhook). Unknown,
//     malformed, and any other endpoint's webhook ids all return "not_found".
//  3. TARGET AUTHORIZATION (add only) — the target must be an active endpoint of the calling
//     endpoint's human, or of a human that human may deliver to across an APPROVED friend edge in
//     the delivering direction (store.FriendEdgeAuthorizesDelivery). Unknown, revoked and
//     unfriended targets collapse into ONE "forbidden" code and message, so the verb is not a
//     cross-tenant existence oracle.
//
// Steps 2 and 3 live in internal/manage (routes.go), which the human API's
// /api/v1/webhooks/{id}/routes runs too, with the signed-in human as the principal. This file is the
// MCP adapter: it decodes arguments, passes the calling endpoint and its human as the principal, and
// maps manage's sentinel kinds onto the stable codes.
//
// Governing: ADR-0022 (endpoint-scoped todo ownership), SPEC-0001 REQ "Deterministic Route Fan-Out
// (Token-Free)", SPEC-0006 REQ "Webhook Route Fan-Out Under Ownership and Friendship",
// SPEC-0006 REQ "Structured Output and Stable Error Shape", ADR-0010 (human-vended friending),
// ADR-0012 (agents self-manage webhooks), ADR-0008 (human-principal vended endpoints), ADR-0038,
// SPEC-0035 REQ "Shared Implementation With MCP".
//
// @joestump-agent 10/02/2026 - The header said ownership was the calling endpoint's HUMAN; the code
// has checked the calling ENDPOINT since F19, and SPEC-0006 says so. Corrected the header and the
// input schema descriptions to match, and moved the checks into internal/manage so the human API
// shares them (#555). The verbs' behaviour is unchanged.

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/manage"
	"github.com/stump-wtf/switchboard/internal/store"
)

// --- tool input/output shapes (SDK-inferred JSON schemas) ---
//
// The output shapes live in internal/manage so the human API returns exactly the same fields; these
// aliases keep the verb code and its tests reading as before.

type addWebhookRouteIn struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the webhook to route (must be owned by this endpoint)"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the endpoint deliveries should additionally fan out to; another human's endpoint requires an approved friend edge"`
}

type removeWebhookRouteIn struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the webhook to unroute (must be owned by this endpoint)"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the target endpoint to stop delivering to"`
}

type listWebhookRoutesIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"the webhook whose routes to list (must be owned by this endpoint)"`
}

type (
	webhookRouteOut       = manage.RouteAddOut
	removeWebhookRouteOut = manage.RouteRemoveOut
	listWebhookRoutesOut  = manage.RoutesOut
)

// registerWebhookRouteTools installs the endpoint's allowlisted routing verbs on the per-session
// server, mirroring the webhook self-management registry (webhooks.go): tools/list advertises exactly
// the allowlist. Governing: SPEC-0014 REQ "Agent Tool Surface over MCP", ADR-0022.
func (h *Handler) registerWebhookRouteTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "add_webhook_route") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "add_webhook_route",
			Description: "Fan a webhook you own out to an additional target endpoint, so each delivery also creates a todo owned by that endpoint. Your own endpoints are allowed freely; another human's endpoint requires an approved friend request.",
		}, h.addWebhookRouteTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "list_webhook_routes") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "list_webhook_routes",
			Description: "List every endpoint a webhook you own delivers to: its owning endpoint (always a target) plus any explicit routes.",
		}, h.listWebhookRoutesTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "remove_webhook_route") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "remove_webhook_route",
			Description: "Stop fanning a webhook you own out to a target endpoint. The webhook's owning endpoint always remains a target.",
		}, h.removeWebhookRouteTool(ep))
	}
}

// --- verb handlers ---

// routeSvc is the route management both surfaces run (internal/manage). The MCP surface has no human
// reach: its principal is the calling endpoint, with that endpoint's human for target authorization.
func (h *Handler) routeSvc() manage.Routes { return manage.Routes{Store: h.store} }

// routePrincipal is the calling endpoint and the human it belongs to.
func routePrincipal(ep store.AuthEndpoint) manage.Principal {
	return manage.EndpointPrincipalOf(ep.ID, ep.OwnerHumanID)
}

// addWebhookRouteTool records a route after webhook ownership and target authorization
// (manage.Routes.Add). Adding a route that already exists is a success, so a retry is safe.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func (h *Handler) addWebhookRouteTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[addWebhookRouteIn, webhookRouteOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addWebhookRouteIn) (*sdk.CallToolResult, webhookRouteOut, error) {
		out, err := h.routeSvc().Add(ctx, routePrincipal(ep), in.WebhookID, in.TargetEndpointID)
		if err != nil {
			return nil, webhookRouteOut{}, h.mapRouteErr(ep, "add_webhook_route", err)
		}
		return nil, out, nil
	}
}

// listWebhookRoutesTool returns the webhook's full fan-out set: the owner endpoint plus the explicit
// routes, including any whose grant has since lapsed (the delivery path skips those), so the owner
// can see and remove them.
func (h *Handler) listWebhookRoutesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[listWebhookRoutesIn, listWebhookRoutesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in listWebhookRoutesIn) (*sdk.CallToolResult, listWebhookRoutesOut, error) {
		out, err := h.routeSvc().List(ctx, routePrincipal(ep), in.WebhookID)
		if err != nil {
			return nil, listWebhookRoutesOut{}, h.mapRouteErr(ep, "list_webhook_routes", err)
		}
		return nil, out, nil
	}
}

// removeWebhookRouteTool drops a target from a webhook the caller owns. Only webhook ownership is
// required, and removing an absent route, the owner endpoint, or a malformed id is an idempotent
// success (manage.Routes.Remove).
func (h *Handler) removeWebhookRouteTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[removeWebhookRouteIn, removeWebhookRouteOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in removeWebhookRouteIn) (*sdk.CallToolResult, removeWebhookRouteOut, error) {
		out, err := h.routeSvc().Remove(ctx, routePrincipal(ep), in.WebhookID, in.TargetEndpointID)
		if err != nil {
			return nil, removeWebhookRouteOut{}, h.mapRouteErr(ep, "remove_webhook_route", err)
		}
		return nil, out, nil
	}
}

// mapRouteErr maps a route failure onto the stable codes: manage's kinds are the codes themselves,
// and anything else is a store failure (mapWebhookErr logs it and answers internal). A refused
// webhook or target is logged with the slug and verb, never the ids, as the verbs always have been.
func (h *Handler) mapRouteErr(ep store.AuthEndpoint, tool string, err error) error {
	var me *manage.Error
	if errors.As(err, &me) {
		switch {
		case me.Kind == manage.ErrNotFound:
			h.log.Warn("mcp webhook route on unowned webhook", "slug", ep.Slug, "tool", tool)
		case me.Kind == manage.ErrForbidden && me.Reason != "":
			h.log.Warn("mcp webhook route to "+me.Reason, "slug", ep.Slug, "tool", tool)
		}
		return &toolError{me.Code(), me.Msg}
	}
	return h.mapWebhookErr(ep, tool, err)
}
