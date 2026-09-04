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
// rather than carrying a copy that could drift.
func WebhookVerbs() []string {
	return []string{
		"create_webhook", "list_webhooks", "rotate_webhook", "delete_webhook",
		"add_webhook_route", "list_webhook_routes", "remove_webhook_route",
	}
}

// EventVerbs returns the SPEC-0005 event-history surface in display order.
func EventVerbs() []string {
	return []string{"list_webhook_events", "get_webhook_event", "replay_webhook_event", "list_providers"}
}

// verbSet builds the membership map the scope guard consults from an ordered verb list.
func verbSet(verbs []string) map[string]bool {
	m := make(map[string]bool, len(verbs))
	for _, v := range verbs {
		m[v] = true
	}
	return m
}
