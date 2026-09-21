package mcp

// Ordered, display-ready enumerations of the agent-facing verb surface. These slices are the
// single source of truth for the per-family verb sets (tools.go agentVerbs, webhooks.go
// webhookVerbs, events.go eventVerbs are derived from them via verbSet), and they are exported so
// the operator UI can offer exactly the verbs this surface serves — the vend modal's toggle chips
// (SPEC-0013 REQ "Endpoints View and Vend Modal") enumerate them instead of hardcoding a copy
// that could drift.
//
// Governing: SPEC-0006 REQ "Todo Drain Verbs", REQ "Webhook Self-Management"; SPEC-0005 (event
// history tools); SPEC-0014 REQ "Agent Tool Surface over MCP".

import "slices"

// DrainVerbs returns the SPEC-0006 todo drain surface in display order.
//
// claim_next sits beside claim rather than replacing it. claim takes an id and is what a single
// agent triaging its own board wants; claim_next takes no id and is the competing-consumer
// primitive — several workers sharing one endpoint each get a DIFFERENT todo, because the store
// scan holds FOR UPDATE SKIP LOCKED. Without it every worker must list_todos and then race to
// claim the same id, which is the pattern that does not scale past a couple of instances.
func DrainVerbs() []string {
	return []string{"list_todos", "claim", "claim_next", "complete", "fail", "heartbeat"}
}

// WebhookVerbs returns the SPEC-0006 webhook self-management surface in display order: the four
// lifecycle verbs an agent uses on its OWN webhooks, then the three ADR-0022 routing verbs that
// decide which endpoints a webhook's deliveries fan out to. Routing lives in this family because it
// is webhook self-management — the routing verbs are gated by webhook ownership, and grouping them
// here means the vend wizard and the OAuth consent screen (internal/web) enumerate them for free
// rather than carrying a copy that could drift. The ADR-0024 rule verbs follow for the same reason:
// rules are webhook configuration, gated by webhook ownership.
func WebhookVerbs() []string {
	return []string{
		"create_webhook", "list_webhooks", "rotate_webhook", "delete_webhook",
		"add_webhook_route", "list_webhook_routes", "remove_webhook_route",
		"list_webhook_rules", "set_webhook_rules", "add_webhook_rule", "update_webhook_rule",
		"move_webhook_rule", "remove_webhook_rule", "test_webhook_rules",
	}
}

// EventVerbs returns the SPEC-0005 event-history surface in display order.
func EventVerbs() []string {
	return []string{"list_webhook_events", "get_webhook_event", "replay_webhook_event"}
}

// AllVerbs returns the full verb set a vend can grant: every drain, webhook, and event verb
// concatenated in display order. Call sites that need the composed grant (the vend wizard's
// endpoint API, the OAuth consent screen) enumerate this instead of concatenating the three
// families by hand, so the composition cannot drift between them. Governing: SPEC-0006 REQ
// "Todo Drain Verbs", REQ "Webhook Self-Management"; SPEC-0014 REQ "Agent Tool Surface over MCP".
func AllVerbs() []string {
	return append(append(append([]string{}, DrainVerbs()...), WebhookVerbs()...), EventVerbs()...)
}

// verbSet builds the membership map the scope guard consults from an ordered verb list.
func verbSet(verbs []string) map[string]bool {
	m := make(map[string]bool, len(verbs))
	for _, v := range verbs {
		m[v] = true
	}
	return m
}

// BasicWebhookMax is the self-managed webhook ceiling the basics vend paths grant: one webhook, the
// single generic ingestion URL an MVP loop needs. The operator API's basics vend and the one-step
// quick vend both grant it, so the two paths cannot drift; the full wizard keeps its own explicit
// ceiling step. Governing: ADR-0023 (MVP basics), ADR-0012, SPEC-0006 REQ "Webhook Self-Management
// Within a Vended Ceiling".
const BasicWebhookMax = 1

// BasicWebhookCeiling returns the webhook ceiling a basics vend grants for the given verbs and
// queues. When the grant carries create_webhook the ceiling is usable end to end — BasicWebhookMax
// webhooks of the generic source type, routed only to the vended queues — because a create_webhook
// grant with no allowed source types or a zero max can never succeed (issue #293). Without
// create_webhook it grants no webhook scope at all: max 0 and nil slices, which disables webhook
// self-management. Governing: ADR-0023, ADR-0012, SPEC-0006 REQ "Webhook Self-Management Within a
// Vended Ceiling".
func BasicWebhookCeiling(verbs, queues []string) (maxWebhooks int, sourceTypes, webhookQueues []string) {
	if !slices.Contains(verbs, "create_webhook") {
		return 0, nil, nil
	}
	return BasicWebhookMax, []string{"generic"}, slices.Clone(queues)
}
