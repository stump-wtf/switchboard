package manage

// Webhook Route Management
//
// A webhook's deliveries always land on its owning endpoint; a route adds another target endpoint,
// and the receiver mints one endpoint-owned todo per target (ADR-0022). This file lists, adds and
// removes those routes for either principal, so add_webhook_route and its siblings over MCP and
// /api/v1/webhooks/{id}/routes over the human API run one ownership check and one target check:
//
//   - WEBHOOK OWNERSHIP. An ENDPOINT principal (MCP) owns only the webhooks whose owning endpoint is
//     itself; a sibling endpoint of the same human does not (ADR-0038, SPEC-0033 F3/F19). A HUMAN
//     principal (the human API) owns every webhook whose owning endpoint hangs off one of their
//     agents (store.WebhookOwnerForHuman). Unknown, malformed and out-of-reach webhook ids are all
//     one not_found. Adding a route to a webhook whose owning endpoint is not active is conflict with
//     its state; listing and removing still work, because a revoked endpoint's routes still deliver
//     and withdrawing delivery is always safe.
//   - TARGET AUTHORIZATION, on add only. The target must be an active endpoint of the human who owns
//     the webhook, or of a human that human may deliver to across an approved friend edge in the
//     delivering direction (store.FriendEdgeAuthorizesDelivery). Unknown, malformed, revoked and
//     unfriended targets are all one forbidden code and message, so the verb is not an existence
//     oracle for anyone's endpoints. Remove skips it, so a route whose friendship lapsed stays
//     removable; the delivery path re-checks every route anyway (store.ResolveWebhookTargets).
//
// Governing: ADR-0022 (endpoint-scoped todo ownership), ADR-0010, ADR-0008, ADR-0038; SPEC-0006 REQ
// "Webhook Route Fan-Out Under Ownership and Friendship"; SPEC-0001 REQ "Deterministic Route Fan-Out
// (Token-Free)"; SPEC-0033 REQ "Closing the Audited Surfaces"; SPEC-0035 REQ "Shared Implementation
// With MCP", REQ "Route Management", REQ "Reach on Every Route".
//
// @joestump-agent 10/02/2026 - Extracted from internal/mcp/webhook_routes.go (ownedWebhook,
// authorizeRouteTarget, the three verb bodies and their output shapes) and parameterized by a
// Principal, so the human API's route routes run the MCP verbs' checks rather than a copy (#555).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// RouteStore is the store surface route management needs: who owns a webhook (endpoint-scoped), who
// owns a target endpoint, whether a friend edge bridges two humans, and the route rows themselves.
type RouteStore interface {
	WebhookOwnerEndpointFor(ctx context.Context, webhookID, callerEndpointID string) (string, error)
	EndpointOwnerHuman(ctx context.Context, endpointID string) (string, error)
	FriendEdgeAuthorizesDelivery(ctx context.Context, fromHumanID, toHumanID string) (bool, error)
	AddWebhookRoute(ctx context.Context, webhookID, targetEndpointID, grantedByHumanID string) error
	RemoveWebhookRoute(ctx context.Context, webhookID, targetEndpointID string) error
	ListWebhookRoutes(ctx context.Context, webhookID string) ([]store.WebhookRoute, error)
}

// Routes runs route management. Humans may be nil (the MCP surface never needs it), in which case a
// human principal resolves nothing.
type Routes struct {
	Store  RouteStore
	Humans HumanReach
}

// RouteRefused is the one message every target-authorization failure carries, whatever the reason.
const RouteRefused = "target endpoint is not routable from this endpoint"

// --- I/O shapes, shared by both surfaces so their fields cannot drift ---

// RouteOut is one explicit route. granted_by_human_id is deliberately not surfaced: only a human who
// owns the webhook can grant a route, so it is always the caller's own human.
type RouteOut struct {
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"an endpoint this webhook's deliveries fan out to"`
	GrantedAt        string `json:"granted_at" jsonschema:"RFC 3339 time the route was granted"`
}

// RoutesOut is a webhook's FULL fan-out set, not just the routes table: the owner endpoint is an
// implicit, unremovable target (ResolveWebhookTargets seeds it), so listing only the explicit rows
// would misrepresent where deliveries land. It is what list_webhook_routes and GET
// /api/v1/webhooks/{id}/routes return. Governing: SPEC-0001 REQ "Deterministic Route Fan-Out
// (Token-Free)".
type RoutesOut struct {
	WebhookID       string     `json:"webhook_id" jsonschema:"the webhook these routes belong to"`
	OwnerEndpointID string     `json:"owner_endpoint_id" jsonschema:"the webhook's owning endpoint — always a delivery target, implicitly and unremovably"`
	Routes          []RouteOut `json:"routes" jsonschema:"explicit additional targets, newest grant first"`
}

// RouteAddOut is an add's result. Routed is always true on success: the add is idempotent, so the
// flag reports the resulting state ("this route exists"), never whether this call created the row.
// Governing: SPEC-0006 REQ "Structured Output and Stable Error Shape".
type RouteAddOut struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the routed webhook id"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the target endpoint id"`
	Routed           bool   `json:"routed" jsonschema:"true once deliveries fan out to this target (idempotent: true whether the route was just added or already present)"`
}

// RouteRemoveOut is a remove's result; Removed is the resulting state, as Routed is for an add.
type RouteRemoveOut struct {
	WebhookID        string `json:"webhook_id" jsonschema:"the unrouted webhook id"`
	TargetEndpointID string `json:"target_endpoint_id" jsonschema:"the removed target endpoint id"`
	Removed          bool   `json:"removed" jsonschema:"true once deliveries no longer fan out to this target (idempotent)"`
}

// --- operations ---

// List returns the webhook's full fan-out set. Ownership alone gates it: target authorization is not
// re-checked per row, so a route whose friendship lapsed stays visible for its owner to remove.
func (r Routes) List(ctx context.Context, p Principal, webhookID string) (RoutesOut, error) {
	id, err := requireID(webhookID)
	if err != nil {
		return RoutesOut{}, err
	}
	ownerEndpointID, _, err := r.owner(ctx, p, id, false)
	if err != nil {
		return RoutesOut{}, err
	}
	rows, err := r.Store.ListWebhookRoutes(ctx, id)
	if err != nil {
		return RoutesOut{}, fmt.Errorf("manage: list webhook routes: %w", err)
	}
	out := RoutesOut{WebhookID: id, OwnerEndpointID: ownerEndpointID, Routes: make([]RouteOut, 0, len(rows))}
	for _, row := range rows {
		out.Routes = append(out.Routes, RouteOut{TargetEndpointID: row.TargetEndpointID, GrantedAt: row.GrantedAt.UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// Add makes targetEndpointID a delivery target of the webhook, after webhook ownership and then
// target authorization. Adding a route that exists, or the owner endpoint itself, succeeds and
// changes nothing that matters: the store's insert is ON CONFLICT DO NOTHING, and
// ResolveWebhookTargets de-duplicates the owner. The grantor recorded on the row is the human who
// owns the webhook, the principal whose authority the route rests on (ADR-0008).
func (r Routes) Add(ctx context.Context, p Principal, webhookID, targetEndpointID string) (RouteAddOut, error) {
	id, target, err := requireRouteArgs(webhookID, targetEndpointID)
	if err != nil {
		return RouteAddOut{}, err
	}
	_, humanID, err := r.owner(ctx, p, id, true)
	if err != nil {
		return RouteAddOut{}, err
	}
	if err := r.authorizeTarget(ctx, humanID, target); err != nil {
		return RouteAddOut{}, err
	}
	if err := r.Store.AddWebhookRoute(ctx, id, target, humanID); err != nil {
		return RouteAddOut{}, fmt.Errorf("manage: add webhook route: %w", err)
	}
	return RouteAddOut{WebhookID: id, TargetEndpointID: target, Routed: true}, nil
}

// Remove drops a target from the webhook. Only webhook ownership is required: withdrawing delivery
// is always safe, and re-checking the target would make a route unremovable exactly when its
// friendship has been revoked. A route that is not there, the owner endpoint (an implicit target)
// and a malformed target id all succeed, since the verb states an end condition that already holds.
func (r Routes) Remove(ctx context.Context, p Principal, webhookID, targetEndpointID string) (RouteRemoveOut, error) {
	id, target, err := requireRouteArgs(webhookID, targetEndpointID)
	if err != nil {
		return RouteRemoveOut{}, err
	}
	if _, _, err := r.owner(ctx, p, id, false); err != nil {
		return RouteRemoveOut{}, err
	}
	// A malformed target cannot be a route. Checking here keeps it from reaching the uuid-typed
	// DELETE as a 22P02, which would surface as internal and an error log on every such call.
	if store.IsUUID(target) {
		if err := r.Store.RemoveWebhookRoute(ctx, id, target); err != nil {
			return RouteRemoveOut{}, fmt.Errorf("manage: remove webhook route: %w", err)
		}
	}
	return RouteRemoveOut{WebhookID: id, TargetEndpointID: target, Removed: true}, nil
}

// --- helpers ---

// owner resolves the webhook's owning endpoint and the human who owns it. An endpoint principal must
// BE the owning endpoint, and its human comes from the principal (OwnerHumanID, set by the MCP
// adapter from the authenticated endpoint). A human principal must own the owning endpoint's agent;
// for an add, that endpoint must also be active.
func (r Routes) owner(ctx context.Context, p Principal, webhookID string, add bool) (endpointID, humanID string, err error) {
	switch {
	case p.EndpointID != "" && p.HumanID == "":
		endpointID, err := r.Store.WebhookOwnerEndpointFor(ctx, webhookID, p.EndpointID)
		if err != nil {
			return "", "", notFoundAs(err, "webhook not found")
		}
		return endpointID, p.OwnerHumanID, nil
	case p.HumanID != "" && p.EndpointID == "" && r.Humans != nil:
		o, err := r.Humans.WebhookOwnerForHuman(ctx, webhookID, p.HumanID)
		if err != nil {
			return "", "", notFoundAs(err, "webhook not found")
		}
		if add && o.EndpointState != "active" {
			return "", "", &Error{Kind: ErrConflict, State: o.EndpointState,
				Msg: "the webhook's endpoint " + o.EndpointSlug + " is " + o.EndpointState + ", so no route can be added to it (removing one still works)"}
		}
		return o.EndpointID, p.HumanID, nil
	default:
		// No principal, both, or a human with no way to resolve reach: nothing is in reach.
		return "", "", fail(ErrNotFound, "webhook not found")
	}
}

// authorizeTarget decides whether humanID, the webhook's owner, may make targetEndpointID a delivery
// target: freely for an endpoint of their own, and otherwise only across an approved friend edge from
// humanID to the target's human (pending, denied, revoked and opposite-direction edges all fail).
// Every refusal is the same code and message; Reason tells the operator log which one it was.
// Governing: ADR-0022, ADR-0010, SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges".
func (r Routes) authorizeTarget(ctx context.Context, humanID, targetEndpointID string) error {
	targetHumanID, err := r.Store.EndpointOwnerHuman(ctx, targetEndpointID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &Error{Kind: ErrForbidden, Msg: RouteRefused, Reason: "unresolvable target"}
		}
		return fmt.Errorf("manage: endpoint owner human: %w", err)
	}
	if humanID != "" && targetHumanID == humanID {
		return nil // one human's work moving inside their own tenant
	}
	ok, err := r.Store.FriendEdgeAuthorizesDelivery(ctx, humanID, targetHumanID)
	if err != nil {
		return fmt.Errorf("manage: friend edge authorizes delivery: %w", err)
	}
	if !ok {
		return &Error{Kind: ErrForbidden, Msg: RouteRefused, Reason: "unfriended human"}
	}
	return nil
}

func requireRouteArgs(webhookID, targetEndpointID string) (string, string, error) {
	id, err := requireID(webhookID)
	if err != nil {
		return "", "", err
	}
	target := strings.TrimSpace(targetEndpointID)
	if target == "" {
		return "", "", fail(ErrInvalidArgument, "target_endpoint_id is required")
	}
	return id, target, nil
}
