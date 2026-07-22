package mcp

// This file is the ADR-0022 webhook ROUTING verb registry served over MCP: add_webhook_route,
// list_webhook_routes, remove_webhook_route. A webhook's deliveries always land as a todo pinned to
// the webhook's OWNING endpoint; a route adds a second (third, …) target endpoint, and the receiver
// mints one endpoint-owned todo per target. Routing is how an agent wires its own webhook to its own
// other endpoints, and how a friended agent's webhook reaches across humans — WITHOUT reintroducing
// the shared-queue-string leak ADR-0022 exists to kill, because every fan-out target is an explicit,
// human-authorized endpoint id rather than a queue name two tenants happen to share.
//
// AUTHORIZATION, in the order it is enforced (all at the boundary, before any mutation):
//
//  1. VERB SCOPE — scopeGuard (tools.go) refuses a routing verb outside the endpoint's allowlist
//     with the stable "forbidden" code. These verbs are members of webhookVerbs (verbs.go), so they
//     get that gate for free.
//  2. WEBHOOK OWNERSHIP — the caller MUST own the webhook, meaning the webhook's owning endpoint
//     belongs to the CALLING endpoint's human (agents.owner_human_id). Ownership is resolved by a
//     single predicate in the store; unknown, malformed, and another human's webhook ids all return
//     "not_found", so a routing verb can never confirm that some other human's webhook exists.
//     This is the check AddWebhookRoute's doc comment delegates to its caller — this layer IS that
//     caller.
//  3. TARGET AUTHORIZATION — the target endpoint is resolved to its owning human.
//     • Same human as the caller: allowed freely. An agent wiring its own webhook to its own second
//     endpoint needs nobody's permission; it is one tenant's work moving inside that tenant.
//     • A different human: REQUIRES an approved friend edge in the delivering direction
//     (from_human = caller's human, to_human = target's human, state = 'approved'). No approved
//     edge — none at all, still pending, denied, revoked, or recorded in the OPPOSITE direction —
//     means no authority. See store.FriendEdgeAuthorizesDelivery for why that direction and not
//     the mirror; inverting it would be a privilege escalation.
//
// Every target-resolution failure — unknown id, revoked endpoint, another human without an approved
// edge — collapses into ONE "forbidden" code with one message. Splitting them would turn the verb
// into a cross-tenant existence oracle: "not_found here, forbidden there" enumerates whose endpoint
// ids are real. The webhook and the target are therefore treated asymmetrically on purpose — the
// caller is entitled to know whether its OWN webhook exists (not_found), and entitled to know
// nothing about anyone else's endpoints (forbidden, uniformly).
//
// Governing: ADR-0022 (endpoint-scoped todo ownership), SPEC-0001 REQ "Deterministic Route Fan-Out
// (Token-Free)", SPEC-0006 REQ "Webhook Route Fan-Out Under Ownership and Friendship",
// SPEC-0006 REQ "Structured Output and Stable Error Shape", ADR-0010 (human-vended friending),
// ADR-0012 (agents self-manage webhooks), ADR-0008 (human-principal vended endpoints).

import (
	"context"
	"errors"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
)

// --- tool input/output shapes (SDK-inferred JSON schemas) ---

type addWebhookRouteIn struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the webhook to route (must be owned by this endpoint's human)"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the endpoint deliveries should additionally fan out to; another human's endpoint requires an approved friend edge"`
}

// webhookRouteOut is the add/remove result. Routed/Removed are always true on success: both verbs
// are idempotent, so the flag reports the resulting STATE ("this route exists" / "this route does
// not"), never whether this particular call happened to be the one that changed a row. Reporting
// "was it new" would leak nothing but would tempt callers into treating a retry as a failure.
// Governing: SPEC-0006 REQ "Structured Output and Stable Error Shape".
type webhookRouteOut struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the routed webhook id"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the target endpoint id"`
	Routed           bool   `json:"routed" jsonschema:"true once deliveries fan out to this target (idempotent: true whether the route was just added or already present)"`
}

type removeWebhookRouteIn struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the webhook to unroute (must be owned by this endpoint's human)"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the target endpoint to stop delivering to"`
}

type removeWebhookRouteOut struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the unrouted webhook id"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the removed target endpoint id"`
	Removed          bool   `json:"removed" jsonschema:"true once deliveries no longer fan out to this target (idempotent)"`
}

type listWebhookRoutesIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"the webhook whose routes to list (must be owned by this endpoint's human)"`
}

// webhookRouteMetaOut is one explicit route row. granted_by_human_id is deliberately NOT surfaced:
// only a human who owns the webhook can grant a route, so it is always the caller's own human id and
// echoing a bare principal identifier back buys nothing.
type webhookRouteMetaOut struct {
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"an endpoint this webhook's deliveries fan out to"`
	GrantedAt        string `json:"granted_at" jsonschema:"RFC 3339 time the route was granted"`
}

// listWebhookRoutesOut reports the webhook's FULL fan-out set, not just the routes table: the owner
// endpoint is an implicit, unremovable target (ResolveWebhookTargets seeds it, and RemoveWebhookRoute
// on it is a no-op), so listing only the explicit rows would misrepresent where deliveries actually
// land. Governing: SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
type listWebhookRoutesOut struct {
	WebhookID       string                `json:"webhook_id" jsonschema:"the webhook these routes belong to"`
	OwnerEndpointID string                `json:"owner_endpoint_id" jsonschema:"the webhook's owning endpoint — always a delivery target, implicitly and unremovably"`
	Routes          []webhookRouteMetaOut `json:"routes" jsonschema:"explicit additional targets, newest grant first"`
}

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

// addWebhookRouteTool enforces webhook ownership and then target authorization before recording the
// route. Adding a route that already exists is a success, not a conflict — the store's insert is
// ON CONFLICT DO NOTHING, and a fan-out target set is a SET, so "make this target routed" is
// naturally idempotent and safe for an agent to retry. Adding the owner endpoint as an explicit
// route is likewise accepted and redundant: ResolveWebhookTargets de-duplicates it away.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func (h *Handler) addWebhookRouteTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[addWebhookRouteIn, webhookRouteOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addWebhookRouteIn) (*sdk.CallToolResult, webhookRouteOut, error) {
		webhookID := strings.TrimSpace(in.WebhookID)
		target := strings.TrimSpace(in.TargetEndpointID)
		if webhookID == "" {
			return nil, webhookRouteOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		if target == "" {
			return nil, webhookRouteOut{}, &toolError{codeInvalidArgument, "target_endpoint_id is required"}
		}
		if _, err := h.ownedWebhook(ctx, ep, "add_webhook_route", webhookID); err != nil {
			return nil, webhookRouteOut{}, err
		}
		if err := h.authorizeRouteTarget(ctx, ep, "add_webhook_route", target); err != nil {
			return nil, webhookRouteOut{}, err
		}
		// The grantor recorded on the row is the caller's human — the principal whose authority (own
		// endpoint, or an approved friend edge) this route rests on (ADR-0008).
		if err := h.store.AddWebhookRoute(ctx, webhookID, target, ep.OwnerHumanID); err != nil {
			return nil, webhookRouteOut{}, h.mapWebhookErr(ep, "add_webhook_route", err)
		}
		return nil, webhookRouteOut{WebhookID: webhookID, TargetEndpointID: target, Routed: true}, nil
	}
}

// listWebhookRoutesTool returns the webhook's full fan-out set. Ownership alone gates it: a route
// list names endpoint ids that a friend consented to receive work at, so it stays inside the webhook
// owner's tenant. Target authorization is NOT re-checked per row — a friendship revoked after the
// fact should be visible here so the owner can see and remove the now-dead route, and the delivery
// path is where it stops mattering: ResolveWebhookTargets re-evaluates endpoint liveness and the
// friend edge on EVERY delivery, so a route whose grant has lapsed is listed but never delivered to.
func (h *Handler) listWebhookRoutesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[listWebhookRoutesIn, listWebhookRoutesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in listWebhookRoutesIn) (*sdk.CallToolResult, listWebhookRoutesOut, error) {
		webhookID := strings.TrimSpace(in.WebhookID)
		if webhookID == "" {
			return nil, listWebhookRoutesOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		ownerEndpointID, err := h.ownedWebhook(ctx, ep, "list_webhook_routes", webhookID)
		if err != nil {
			return nil, listWebhookRoutesOut{}, err
		}
		routes, err := h.store.ListWebhookRoutes(ctx, webhookID)
		if err != nil {
			return nil, listWebhookRoutesOut{}, h.mapWebhookErr(ep, "list_webhook_routes", err)
		}
		out := listWebhookRoutesOut{
			WebhookID:       webhookID,
			OwnerEndpointID: ownerEndpointID,
			Routes:          make([]webhookRouteMetaOut, 0, len(routes)),
		}
		for _, r := range routes {
			out.Routes = append(out.Routes, webhookRouteMetaOut{
				TargetEndpointID: r.TargetEndpointID,
				GrantedAt:        r.GrantedAt.UTC().Format(time.RFC3339),
			})
		}
		return nil, out, nil
	}
}

// removeWebhookRouteTool drops a target from a webhook the caller owns. Only WEBHOOK ownership is
// required, deliberately: withdrawing delivery is always safe, and re-checking target authorization
// would make a route unremovable exactly when it most needs removing (the friendship that authorized
// it has been revoked). Removing a route that is not there is a success — the store's DELETE is a
// no-op and the verb states an end condition, so a retry is idempotent. Removing the owner endpoint
// is likewise a no-op: it is an implicit, unremovable target (ResolveWebhookTargets seeds it).
func (h *Handler) removeWebhookRouteTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[removeWebhookRouteIn, removeWebhookRouteOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in removeWebhookRouteIn) (*sdk.CallToolResult, removeWebhookRouteOut, error) {
		webhookID := strings.TrimSpace(in.WebhookID)
		target := strings.TrimSpace(in.TargetEndpointID)
		if webhookID == "" {
			return nil, removeWebhookRouteOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		if target == "" {
			return nil, removeWebhookRouteOut{}, &toolError{codeInvalidArgument, "target_endpoint_id is required"}
		}
		if _, err := h.ownedWebhook(ctx, ep, "remove_webhook_route", webhookID); err != nil {
			return nil, removeWebhookRouteOut{}, err
		}
		// A malformed target id is an absent route, not a server fault. This verb deliberately skips
		// authorizeRouteTarget (see above), which is where the isUUID guard otherwise lives, so
		// without this check a non-uuid string reaches a uuid-typed predicate as a 22P02 and surfaces
		// as codeInternal + an Error log line — agent-triggerable log spam on every call. Removing a
		// route that cannot exist satisfies the verb's stated end condition, so report the same
		// idempotent success the store's no-op DELETE reports for an unknown-but-well-formed id.
		if !store.IsUUID(target) {
			return nil, removeWebhookRouteOut{WebhookID: webhookID, TargetEndpointID: target, Removed: true}, nil
		}
		if err := h.store.RemoveWebhookRoute(ctx, webhookID, target); err != nil {
			return nil, removeWebhookRouteOut{}, h.mapWebhookErr(ep, "remove_webhook_route", err)
		}
		return nil, removeWebhookRouteOut{WebhookID: webhookID, TargetEndpointID: target, Removed: true}, nil
	}
}

// --- authorization helpers ---

// ownedWebhook resolves a webhook the CALLING endpoint's human owns, returning the webhook's owning
// endpoint id (its implicit delivery target). A webhook that does not exist, is malformed, or belongs
// to another human is uniformly "not_found" — the same shape rotate_webhook/delete_webhook use, so
// probing ids reveals nothing about another human's webhooks.
//
// Ownership is checked at the HUMAN, not the endpoint: an agent may hold several endpoints under one
// human (ADR-0008), and a human's own webhook is theirs to route from whichever of their endpoints is
// driving the session. The tenant boundary ADR-0022 defends is between HUMANS; within one human,
// endpoint scoping is the todo-visibility mechanism, not an ownership wall.
func (h *Handler) ownedWebhook(ctx context.Context, ep store.AuthEndpoint, tool, webhookID string) (string, error) {
	ownerEndpointID, err := h.store.WebhookOwnerEndpointForHuman(ctx, webhookID, ep.OwnerHumanID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.log.Warn("mcp webhook route on unowned webhook", "slug", ep.Slug, "tool", tool)
			return "", &toolError{codeNotFound, "webhook not found"}
		}
		return "", h.mapWebhookErr(ep, tool, err)
	}
	return ownerEndpointID, nil
}

// authorizeRouteTarget decides whether the calling endpoint's human may make targetEndpointID a
// delivery target: freely for an endpoint their own human owns, and only across an APPROVED friend
// edge in the delivering direction otherwise (store.FriendEdgeAuthorizesDelivery carries the
// direction argument). Pending, denied, revoked, and opposite-direction edges all fail.
//
// Unknown ids, revoked endpoints, and unfriended humans return the SAME code and message on purpose.
// Distinguishing them would let an agent enumerate which endpoint ids exist and which humans it is
// friends with, one probe at a time — the exact cross-tenant oracle ADR-0022 is closing.
// Governing: ADR-0022, ADR-0010, SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges".
func (h *Handler) authorizeRouteTarget(ctx context.Context, ep store.AuthEndpoint, tool, targetEndpointID string) error {
	refuse := &toolError{codeForbidden, "target endpoint is not routable from this endpoint"}

	targetHumanID, err := h.store.EndpointOwnerHuman(ctx, targetEndpointID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.log.Warn("mcp webhook route to unresolvable target", "slug", ep.Slug, "tool", tool)
			return refuse
		}
		return h.mapWebhookErr(ep, tool, err)
	}
	if targetHumanID == ep.OwnerHumanID {
		return nil // one human's work moving inside their own tenant
	}
	ok, err := h.store.FriendEdgeAuthorizesDelivery(ctx, ep.OwnerHumanID, targetHumanID)
	if err != nil {
		return h.mapWebhookErr(ep, tool, err)
	}
	if !ok {
		h.log.Warn("mcp webhook route to unfriended human", "slug", ep.Slug, "tool", tool)
		return refuse
	}
	return nil
}
