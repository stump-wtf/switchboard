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

import (
	"fmt"
	"slices"
	"strings"
)

// DrainVerbs returns the SPEC-0006 todo drain surface in display order.
//
// get_todo is listed so the vend wizard and the consent screen offer it, though list_todos implies
// it (SPEC-0034 REQ-8, get_todo.go).
//
// release is listed so a vend can grant it; nothing implies it, because it changes state
// (SPEC-0034 REQ-9).
//
// claim_next sits beside claim rather than replacing it. claim takes an id and is what a single
// agent triaging its own board wants; claim_next takes no id and is the competing-consumer
// primitive — several workers sharing one endpoint each get a DIFFERENT todo, because the store
// scan holds FOR UPDATE SKIP LOCKED. Without it every worker must list_todos and then race to
// claim the same id, which is the pattern that does not scale past a couple of instances.
func DrainVerbs() []string {
	return []string{"list_todos", "get_todo", "claim", "claim_next", "complete", "fail", "release", "heartbeat"}
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
		"set_trusted_actors", "clear_trusted_actors",
		"add_webhook_route", "list_webhook_routes", "remove_webhook_route",
		"list_webhook_rules", "set_webhook_rules", "add_webhook_rule", "update_webhook_rule",
		"move_webhook_rule", "remove_webhook_rule", "test_webhook_rules",
	}
}

// LeaseVerbs returns the endpoint's own lease-setting surface in display order: read the default
// lease a claim on this endpoint gets, and set it. They act only on the calling endpoint and grant
// nothing a caller could not already ask for per call (lease_ttl_seconds has the same cap), so they
// sit in the default grant beside the webhook self-management verbs. They are not friend-grantable
// (store.friendGrantableVerbs): a friend endpoint is vended on the approver's agent, and its
// settings are the approver's to change. Governing: ADR-0043, SPEC-0006 REQ "Endpoint Default
// Lease Verbs".
func LeaseVerbs() []string {
	return []string{"get_default_lease", "set_default_lease"}
}

// EventVerbs returns the SPEC-0005 event-history surface in display order.
func EventVerbs() []string {
	return []string{"list_webhook_events", "get_webhook_event", "replay_webhook_event"}
}

// WebhookAdminVerbs returns the verbs that change how deliveries reach endpoints, rather than how
// one endpoint works: the webhook lifecycle, routing, rules and trusted-actor verbs, then the event
// history including replay. It is the blast-radius set issue #589 describes, the grant a worker lane
// never needs, and it stays a contiguous slice of AllVerbs so the vend default and the audit view
// can name it without carrying a copy of the verb list that could drift. Governing: ADR-0047.
func WebhookAdminVerbs() []string {
	return append(append([]string{}, WebhookVerbs()...), EventVerbs()...)
}

// NotifyHookVerbs returns the SPEC-0024 notify-hook surface in display order. These verbs make
// Switchboard dial tenant-chosen URLs, so they are grantable per endpoint but deliberately NOT part
// of AllVerbs or any default grant: the grant surfaces list them as their own "outbound HTTP calls"
// group, off by default, and an endpoint holds them only when its human ticks them.
// Governing: SPEC-0024 REQ-2 "Management Verbs", ADR-0029.
func NotifyHookVerbs() []string {
	return []string{"create_notify_hook", "list_notify_hooks", "rotate_notify_hook", "delete_notify_hook"}
}

// QueueReadVerbs returns the read half of the operator queue-supervision surface in display order:
// the one call that answers "is this queue stalled or just quiet", with the leases outstanding on
// it. It is NOT part of AllVerbs and no consumer verb implies it: an endpoint holds it only when a
// vend names it or its preset, which is what keeps a worker credential from reading the whole
// lane's health while it cannot touch its state. Governing: ADR-0056.
func QueueReadVerbs() []string { return []string{"queue_stats"} }

// QueueControlVerbs returns the state-changing half of the same surface in display order: quiesce a
// queue, wake its backlog, quiesce and wait for the leases to land, retry a dead letter, and take
// over a lease whose holder is gone. It is NOT part of AllVerbs, no default grant, and nothing
// implies it, because every verb in it changes state another endpoint's worker is relying on.
// A pause, a resume and a drain are the same ADR-0048 switch, three verbs deep; a requeue is the
// Board's "Retry now"; a lease release is the Board's "Release". Governing: ADR-0056, ADR-0048.
func QueueControlVerbs() []string {
	return []string{"pause_queue", "resume_queue", "drain_queue", "requeue_todo", "release_lease"}
}

// QueueOperatorVerbs returns the whole operator supervision surface, read verbs first, in display
// order. It is a contiguous slice of VerbOrder so the vend CLI and the audit view can name the
// family without carrying a copy of the verb list. Governing: ADR-0056, ADR-0047.
func QueueOperatorVerbs() []string {
	return append(append([]string{}, QueueReadVerbs()...), QueueControlVerbs()...)
}

// QueueOperatorVerbsIn returns the verbs of a grant that belong to the operator supervision family,
// in canonical order: what an audit view lists when it flags an endpoint that can supervise a queue
// it does not consume. Governing: ADR-0056.
func QueueOperatorVerbsIn(grant []string) []string {
	family := QueueOperatorVerbs()
	held := make([]string, 0, len(family))
	for _, v := range VerbOrder() {
		if slices.Contains(family, v) && slices.ContainsFunc(grant, func(g string) bool {
			return strings.EqualFold(g, v)
		}) {
			held = append(held, v)
		}
	}
	return held
}

// GrantHasQueueOperator reports whether a grant holds any verb from the operator supervision family,
// so the vend reveal can warn that this endpoint supervises queues rather than only working them.
// Governing: ADR-0056.
func GrantHasQueueOperator(grant []string) bool {
	return len(QueueOperatorVerbsIn(grant)) > 0
}

// AllVerbs returns the default-grantable verb set: every drain, webhook, event and lease verb
// concatenated in display order. The notify-hook verbs are grantable too but never by default, so
// they are listed separately (NotifyHookVerbs) and never folded in here. Call sites that need the composed grant (the vend wizard's
// endpoint API, the OAuth consent screen) enumerate this instead of concatenating the three
// families by hand, so the composition cannot drift between them. Governing: SPEC-0006 REQ
// "Todo Drain Verbs", REQ "Webhook Self-Management"; SPEC-0014 REQ "Agent Tool Surface over MCP".
func AllVerbs() []string {
	out := append(append([]string{}, DrainVerbs()...), WebhookAdminVerbs()...)
	return append(out, LeaseVerbs()...)
}

// VendPresets are the named verb groups an operator may name in a vend verb spec instead of listing
// verbs one at a time. The order here is the order help text and refusal messages list them in.
var VendPresets = []struct {
	Name  string
	Verbs []string
}{
	{"todo", DrainVerbs()},
	{"webhook-admin", WebhookAdminVerbs()},
	{"queue-operator", QueueOperatorVerbs()},
	{"all", AllVerbs()},
}

// DefaultVendVerbs is the grant a vend hands out when its spec names none: the eight todo verbs, plus
// the endpoint's own default-lease verbs. Narrow-by-default is the posture ADR-0047 proposes, and this
// is its single source, so the CLI and the API cannot disagree about what "no verbs named" means.
//
// The lease verbs belong here because SPEC-0006 requires them in the default grant, and they add
// nothing a caller could not already ask for on a single call: lease_ttl_seconds carries the same
// ceiling either way. The webhook, routing, rule, trusted-actor and event verbs are the ones a lane
// has to ask for by name. Governing: ADR-0047, SPEC-0006 REQ "Endpoint Default Lease Verbs".
func DefaultVendVerbs() []string {
	return append(append([]string{}, DrainVerbs()...), LeaseVerbs()...)
}

// VerbOrder returns every grantable verb in canonical display order, operator supervision verbs
// before the notify-hook verbs last: the order a resolved grant is stored and printed in, whatever
// order the operator typed them in.
func VerbOrder() []string {
	out := append(append([]string{}, AllVerbs()...), QueueOperatorVerbs()...)
	return append(out, NotifyHookVerbs()...)
}

// VendVerbGrant resolves a vend verb spec into the grant to store. A spec that names nothing — absent,
// empty, or blank entries a script left behind — is the narrow default (DefaultVendVerbs); anything
// else is expanded by ResolveVerbSpec. Presets resolve here too, so one spec works as a CLI flag and
// as a scripted POST against the operator API.
func VendVerbGrant(spec []string) ([]string, error) {
	named := make([]string, 0, len(spec))
	for _, entry := range spec {
		if strings.TrimSpace(entry) != "" {
			named = append(named, entry)
		}
	}
	if len(named) == 0 {
		return DefaultVendVerbs(), nil
	}
	return ResolveVerbSpec(named)
}

// ResolveVerbSpec expands entries that are verb names and/or preset names (case-insensitive, commas
// and spaces allowed between them) into one de-duplicated grant in canonical order. An entry naming
// neither a verb nor a preset is refused: a mistyped verb would otherwise silently grant less than
// the operator meant, which is the one failure this surface must not have. A spec that resolves to
// nothing is refused for the same reason.
func ResolveVerbSpec(spec []string) ([]string, error) {
	known := verbSet(VerbOrder())
	wanted := map[string]bool{}
	var unknown []string
	for _, entry := range spec {
		for _, part := range strings.Split(entry, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if preset, ok := presetVerbs(name); ok {
				for _, v := range preset {
					wanted[v] = true
				}
				continue
			}
			if !known[name] {
				unknown = append(unknown, name)
				continue
			}
			wanted[name] = true
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown verb %q: not a verb or a preset (%s)",
			strings.Join(unknown, ", "), strings.Join(presetNames(), ", "))
	}
	grant := make([]string, 0, len(wanted))
	for _, v := range VerbOrder() {
		if wanted[v] {
			grant = append(grant, v)
		}
	}
	if len(grant) == 0 {
		return nil, fmt.Errorf("the verb spec grants no verbs")
	}
	return grant, nil
}

// WebhookAdminVerbsIn returns the verbs of a grant that belong to the webhook-admin family, in
// canonical order: what an audit view lists when it flags an endpoint that can rewrite routing.
func WebhookAdminVerbsIn(grant []string) []string {
	admin := WebhookAdminVerbs()
	held := make([]string, 0, len(admin))
	for _, v := range VerbOrder() {
		if slices.Contains(admin, v) && slices.ContainsFunc(grant, func(g string) bool {
			return strings.EqualFold(g, v)
		}) {
			held = append(held, v)
		}
	}
	return held
}

// GrantHasWebhookAdmin reports whether a grant holds any verb from the webhook-admin family, so the
// vend reveal and the audit view can flag the endpoints that can rewrite routing without printing a
// verb list. Governing: ADR-0047.
func GrantHasWebhookAdmin(grant []string) bool {
	return len(WebhookAdminVerbsIn(grant)) > 0
}

func presetVerbs(name string) ([]string, bool) {
	for _, p := range VendPresets {
		if p.Name == name {
			return p.Verbs, true
		}
	}
	return nil, false
}

func presetNames() []string {
	names := make([]string, 0, len(VendPresets))
	for _, p := range VendPresets {
		names = append(names, p.Name)
	}
	return names
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
