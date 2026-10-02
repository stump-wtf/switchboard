package main

// Webhook Route Verbs
//
// `switchboard webhook route list|add|remove` drive the human API's route routes (SPEC-0035 REQ
// "Route Management") as the logged-in human, so a webhook's delivery targets can be changed from a
// terminal without the owning endpoint's MCP credential. A route is what lets a rule's `endpoints`
// name an endpoint: `rules set` refuses one outside grant.endpoints until `route add` puts it there.
//
// ENDPOINT is an endpoint id, or the slug `endpoint list` prints, resolved to its id through
// GET /api/v1/endpoints. Only your own endpoints have slugs you can resolve; a friend's endpoint is
// named by its id. Every verb takes --json, which prints the API response verbatim.
//
// Governing: SPEC-0035 REQ "CLI Parity", REQ "Route Management"; ADR-0022; SPEC-0006 REQ "Webhook
// Route Fan-Out Under Ownership and Friendship".
//
// @joestump-agent 10/02/2026 - Added webhook route list/add/remove (#555).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
)

// routesDoc is GET /api/v1/webhooks/{id}/routes (internal/manage RoutesOut).
type routesDoc struct {
	WebhookID       string     `json:"webhook_id"`
	OwnerEndpointID string     `json:"owner_endpoint_id"`
	Routes          []routeRow `json:"routes"`
}

type routeRow struct {
	TargetEndpointID string `json:"target_endpoint_id"`
	GrantedAt        string `json:"granted_at"`
}

func routesURL(id string) string { return "/api/v1/webhooks/" + url.PathEscape(id) + "/routes" }

func routeURL(id, endpointID string) string { return routesURL(id) + "/" + url.PathEscape(endpointID) }

// --- webhook route list ---

func cmdWebhookRouteList(c *cli, args []string) int {
	fs := c.flagSet("webhook route list", "WEBHOOK_ID", "Show every endpoint a webhook delivers to: its owning endpoint, which is always a\n"+
		"target, then its routes in grant order (the order an exclusive rule picks from). These are\n"+
		"the endpoints a rule's \"endpoints\" may name.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	id, exit, ok := c.webhookArg(fs, args)
	if !ok {
		return exit
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.get(routesURL(id))
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	var doc routesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return c.fail(fmt.Errorf("webhook route list: malformed response: %w", err))
	}
	// Slugs are a convenience: without them the ids still say everything, so a failed lookup is a
	// warning, not a failure.
	slugs, err := c.endpointSlugs(api)
	if err != nil {
		fmt.Fprintf(c.stderr, "switchboard: endpoint slugs unavailable (%v); showing ids only\n", err)
	}
	slug := func(id string) string {
		if s := slugs[id]; s != "" {
			return s
		}
		return "-"
	}
	// The API lists routes newest grant first. Delivery order is oldest first, ties by id, as
	// store.ResolveWebhookTargets orders them (granted_at is RFC 3339 UTC, so it sorts as text).
	routes := slices.Clone(doc.Routes)
	slices.SortFunc(routes, func(a, b routeRow) int {
		if c := strings.Compare(a.GrantedAt, b.GrantedAt); c != 0 {
			return c
		}
		return strings.Compare(a.TargetEndpointID, b.TargetEndpointID)
	})

	fmt.Fprintf(c.stdout, "Webhook %s delivers to:\n\n", doc.WebhookID)
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENDPOINT\tSLUG\tGRANTED")
	fmt.Fprintf(tw, "%s\t%s\t%s\n", doc.OwnerEndpointID, slug(doc.OwnerEndpointID), "owner (always a target)")
	for _, r := range routes {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.TargetEndpointID, slug(r.TargetEndpointID), r.GrantedAt)
	}
	_ = tw.Flush()
	if len(routes) == 0 {
		fmt.Fprintln(c.stdout, "\nNo routes: deliveries land on the owner only. Add one with `switchboard webhook route add`.")
	}
	return exitOK
}

// --- webhook route add ---

func cmdWebhookRouteAdd(c *cli, args []string) int {
	fs := c.flagSet("webhook route add", "WEBHOOK_ID ENDPOINT", "Make ENDPOINT one of a webhook's delivery targets, beside its owning endpoint, so the\n"+
		"webhook's rules may name it in \"endpoints\". ENDPOINT is your own endpoint (slug or id), or\n"+
		"by id another human's that you may deliver to through an approved friend request. Adding a\n"+
		"route that exists changes nothing.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	id, ref, exit, ok := c.webhookEndpointArgs(fs, args)
	if !ok {
		return exit
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	target, err := c.resolveEndpointRef(api, ref)
	if err != nil {
		return c.fail(err)
	}
	body, err := api.put(routeURL(id, target), nil)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	fmt.Fprintf(c.stdout, "Webhook %s now also delivers to %s.\n", id, describeEndpoint(target, ref))
	fmt.Fprintf(c.stdout, "Its rules may name it: `switchboard webhook rules get %s` lists it under the grant's endpoints.\n", id)
	return exitOK
}

// --- webhook route remove ---

func cmdWebhookRouteRemove(c *cli, args []string) int {
	fs := c.flagSet("webhook route remove", "WEBHOOK_ID ENDPOINT", "Stop delivering a webhook to ENDPOINT (slug or id). Removing a route that is not there\n"+
		"succeeds, and the webhook's owning endpoint always stays a target. A rule that still names\n"+
		"the endpoint falls back to the default action until you change it.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	id, ref, exit, ok := c.webhookEndpointArgs(fs, args)
	if !ok {
		return exit
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	target, err := c.resolveEndpointRef(api, ref)
	if err != nil {
		return c.fail(err)
	}
	body, err := api.del(routeURL(id, target))
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	fmt.Fprintf(c.stdout, "Webhook %s no longer delivers to %s.\n", id, describeEndpoint(target, ref))
	return exitOK
}

// --- helpers ---

// webhookEndpointArgs parses a route verb's flags and its WEBHOOK_ID ENDPOINT arguments.
func (c *cli) webhookEndpointArgs(fs *flag.FlagSet, args []string) (webhookID, endpoint string, exit int, ok bool) {
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return "", "", exit, false
	}
	switch {
	case len(pos) == 0 || strings.TrimSpace(pos[0]) == "":
		return "", "", c.usageError(fs, "a webhook id is required (see `switchboard webhook list`)"), false
	case len(pos) == 1 || strings.TrimSpace(pos[1]) == "":
		return "", "", c.usageError(fs, "an endpoint is required: a slug from `switchboard endpoint list`, or an endpoint id"), false
	case len(pos) > 2:
		return "", "", c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[2])), false
	}
	return strings.TrimSpace(pos[0]), strings.TrimSpace(pos[1]), exitOK, true
}

// resolveEndpointRef turns ENDPOINT into an endpoint id: an id is used as given, and a slug is looked
// up among your endpoints, the list `endpoint list` prints. Another human's endpoint has no slug you
// can resolve, so it is named by id.
func (c *cli) resolveEndpointRef(api *apiClient, ref string) (string, error) {
	if isEndpointID(ref) {
		return ref, nil
	}
	rows, err := c.endpointRows(api)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Slug == ref {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("no endpoint of yours has the slug %q (see `switchboard endpoint list`); name another human's endpoint by its id", ref)
}

type endpointRow struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// endpointRows is GET /api/v1/endpoints, reduced to what slug resolution needs.
func (c *cli) endpointRows(api *apiClient) ([]endpointRow, error) {
	body, err := api.get("/api/v1/endpoints")
	if err != nil {
		return nil, err
	}
	var rows []endpointRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("endpoints: malformed response: %w", err)
	}
	return rows, nil
}

// endpointSlugs maps your endpoints' ids to their slugs.
func (c *cli) endpointSlugs(api *apiClient) (map[string]string, error) {
	rows, err := c.endpointRows(api)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Slug
	}
	return out, nil
}

// isEndpointID reports whether ref is an endpoint id: a UUID in its canonical 36-character form, the
// only form the API prints.
func isEndpointID(ref string) bool {
	if len(ref) != 36 {
		return false
	}
	_, err := uuid.Parse(ref)
	return err == nil
}

// describeEndpoint names an endpoint the way the operator did, adding the id when they gave a slug.
func describeEndpoint(id, ref string) string {
	if ref == id {
		return id
	}
	return ref + " (" + id + ")"
}
