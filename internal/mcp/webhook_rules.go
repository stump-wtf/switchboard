package mcp

// This file is the ADR-0024 routing-rule verb registry served over MCP: list_webhook_rules,
// set_webhook_rules, add_webhook_rule, update_webhook_rule, move_webhook_rule, remove_webhook_rule,
// and test_webhook_rules. An agent that owns a webhook writes an ordered, first-match-wins list of jq
// rules deciding which queue a delivery lands in (optionally narrowed to some of the webhook's
// delivery targets) or whether it is dropped, plus a default for everything unmatched.
//
// AUTHORIZATION, in the order it is enforced:
//
//  1. VERB SCOPE — scopeGuard (tools.go) refuses these verbs outside the endpoint's allowlist; they
//     are members of webhookVerbs (verbs.go).
//  2. WEBHOOK OWNERSHIP — the caller's human must own the webhook (store.WebhookRoutingForHuman /
//     UpdateWebhookRouting); unknown, malformed, and another human's webhook ids are uniformly
//     not_found, exactly as the ADR-0022 route verbs answer.
//  3. REACHABILITY — every save is validated by routing.Validate against a grant built from
//     switchboard state alone: the webhook's target queue, its OWNER's allowed webhook queues (the
//     owning endpoint's ceiling united with the queues of the owner's other active, unexpired
//     endpoints — the store computes the union in the routing read, so vending an endpoint for a
//     queue lets the owner's webhook rules route to it; issue #270), and its live, authorized
//     delivery targets (ResolveWebhookTargets). A rule can narrow
//     a delivery to endpoints the webhook already reaches; it can never add one. Adding a target is
//     add_webhook_route's job, with add_webhook_route's authorization. The same grant is re-applied
//     on every delivery, so a later revocation wins over a saved rule.
//
// Every mutation is a read-modify-write under a row lock, and a failed validation leaves the previous
// configuration in force. test_webhook_rules is the authoring loop: it runs a candidate (or the saved)
// rule list against a sample payload or one of the webhook's own stored events, through the same
// router the receiver uses, and returns the decision, the trace, and the envelope the rules saw.
//
// Governing: ADR-0024, SPEC-0020 REQ "Rule Validation at Save Time", REQ "Routing Trace", REQ
// "Isolation and Tenant Safety"; SPEC-0006 REQ "Structured Output and Stable Error Shape"; ADR-0022;
// ADR-0012.
//
// @joestump-agent 09/11/2026 - Initial rule verbs and dry-run.
//
// @joestump-agent 09/11/2026 - mutateRules resolves delivery targets before the row lock; resolving
// them inside UpdateWebhookRouting's transaction took a second pooled connection and deadlocked the
// pool under concurrent rule edits.
//
// @joestump-agent 09/11/2026 - ADR-0025: params ($params), exclusive/once/work_order action flags,
// per-target scopes in the grant (read before the lock, like targets), and a dry-run that previews
// the once key and work order.
//
// @joestump-agent 09/23/2026 - Fail closed (ADR-0031, SPEC-0026 REQ-3): set/add/update/move dry-run
// the resulting rules against the webhook's 50 latest deliveries before saving, and refuse a save
// that faults on any of them. The dry-run runs before the lock, and the locked write saves the
// checked candidate only if the stored config is unchanged (conflict otherwise). test_webhook_rules
// reports faulted as blocking (#212).
//
// @joestump-agent 09/25/2026 - The save-time dry-run skips deliveries the trust gate would hold
// (SPEC-0026 REQ-5): live traffic never runs rules on them, so an outsider's payload could otherwise
// refuse the owner's saves. It pages back past them to find 50 that pass (#385 review).
//
// @joestump-agent 09/25/2026 - test_webhook_rules reports a held delivery's decision and trace as the
// receiver records them (the trust gate's), with the rules' outcome moved to rules_would.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	codeRuleNotFound = "rule_not_found"
	// codeUnavailable says the rule evaluator could not run, so a save could not be checked. It is
	// transient and names no rule. Governing: SPEC-0026 REQ-3.
	codeUnavailable = "unavailable"
)

// --- tool input/output shapes (SDK-inferred JSON schemas) ---

type actionIO struct {
	Queue      string   `json:"queue,omitempty" jsonschema:"deliver to this queue (the webhook's target queue or one in its owner's allowed webhook queues)"`
	Drop       bool     `json:"drop,omitempty" jsonschema:"true to record the delivery without creating a todo or ringing a doorbell"`
	Quarantine bool     `json:"quarantine,omitempty" jsonschema:"true to hold the delivery on the owner's quarantine queue for a human or classifier to release or discard (no agent is handed it)"`
	Endpoints  []string `json:"endpoints,omitempty" jsonschema:"with queue only: deliver to just these of the webhook's delivery targets (see grant.endpoints); omitted means every target"`
	Exclusive  bool     `json:"exclusive,omitempty" jsonschema:"with queue only: deliver to exactly one target, the first (owner first, then routes by grant time) whose endpoint scope includes the queue"`
	Once       bool     `json:"once,omitempty" jsonschema:"with queue only: at most one work order per subject (issue or cairn artifact) per queue; later deliveries about it are recorded but mint nothing"`
	WorkOrder  bool     `json:"work_order,omitempty" jsonschema:"with queue only: attach a switchboard-authored work order (lane, verified provenance, authorizing rule, subject) to each todo"`
}

type ruleIO struct {
	ID     string   `json:"id,omitempty" jsonschema:"stable rule id; minted when omitted on create"`
	Name   string   `json:"name,omitempty" jsonschema:"label recorded on the routing trace"`
	Expr   string   `json:"expr" jsonschema:"jq filter over the routing envelope; its first output decides (anything but false or null matches)"`
	Action actionIO `json:"action" jsonschema:"where a matching delivery goes: exactly one of queue or drop"`
}

type grantOut struct {
	Queues    []string `json:"queues" jsonschema:"queues an action may name: the webhook's target queue plus its owner's allowed webhook queues"`
	Endpoints []string `json:"endpoints" jsonschema:"endpoints an action may narrow to: the webhook's live delivery targets, owner first"`
}

type webhookRulesOut struct {
	WebhookID     string         `json:"webhook_id" jsonschema:"the webhook"`
	SourceType    string         `json:"source_type" jsonschema:"the webhook's source type (the envelope's .source)"`
	TargetQueue   string         `json:"target_queue" jsonschema:"the webhook's target queue"`
	DefaultAction *actionIO      `json:"default_action,omitempty" jsonschema:"what unmatched deliveries do; omitted means the target queue on every target"`
	Rules         []ruleIO       `json:"rules" jsonschema:"the rules, in evaluation order (first match wins)"`
	Params        map[string]any `json:"params,omitempty" jsonschema:"owner-set values rules read as $params (e.g. trusted-actor allowlists)"`
	Grant         grantOut       `json:"grant" jsonschema:"what actions may reach right now"`
}

type webhookIDIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
}

type setWebhookRulesIn struct {
	WebhookID     string         `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	Rules         []ruleIO       `json:"rules" jsonschema:"the complete ordered rule list; replaces the current one atomically"`
	DefaultAction *actionIO      `json:"default_action,omitempty" jsonschema:"what unmatched deliveries do; omit for the webhook's target queue"`
	Params        map[string]any `json:"params,omitempty" jsonschema:"values bound as $params in every rule; replaces the current params (omit to clear)"`
}

type addWebhookRuleIn struct {
	WebhookID string   `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	ID        string   `json:"id,omitempty" jsonschema:"stable rule id; minted when omitted"`
	Name      string   `json:"name,omitempty" jsonschema:"label recorded on the routing trace"`
	Expr      string   `json:"expr" jsonschema:"jq filter over the routing envelope"`
	Action    actionIO `json:"action" jsonschema:"exactly one of queue or drop"`
	Position  *int     `json:"position,omitempty" jsonschema:"0-based index to insert at; omitted appends (lowest precedence)"`
}

type updateWebhookRuleIn struct {
	WebhookID string    `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	RuleID    string    `json:"rule_id" jsonschema:"the rule to change"`
	Name      *string   `json:"name,omitempty" jsonschema:"new label"`
	Expr      *string   `json:"expr,omitempty" jsonschema:"new jq filter"`
	Action    *actionIO `json:"action,omitempty" jsonschema:"new action"`
}

type moveWebhookRuleIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	RuleID    string `json:"rule_id" jsonschema:"the rule to move"`
	Position  int    `json:"position" jsonschema:"0-based index the rule should end up at; clamped to the list"`
}

type removeWebhookRuleIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	RuleID    string `json:"rule_id" jsonschema:"the rule to remove (removing an absent rule succeeds)"`
}

type testWebhookRulesIn struct {
	WebhookID     string            `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	EventID       int64             `json:"event_id,omitempty" jsonschema:"route one of THIS webhook's stored deliveries (exactly one of event_id or payload)"`
	Payload       any               `json:"payload,omitempty" jsonschema:"a sample delivery body (any JSON) to route as if it had just arrived and verified"`
	Headers       map[string]string `json:"headers,omitempty" jsonschema:"sample request headers for a payload test (e.g. X-Gitea-Event)"`
	Rules         []ruleIO          `json:"rules,omitempty" jsonschema:"candidate rules to try instead of the saved list (not saved)"`
	DefaultAction *actionIO         `json:"default_action,omitempty" jsonschema:"candidate default, used only with rules"`
	Params        map[string]any    `json:"params,omitempty" jsonschema:"candidate params to try instead of the saved ones (not saved)"`
	OmitEnvelope  bool              `json:"omit_envelope,omitempty" jsonschema:"true to leave the evaluated envelope out of the result"`
}

type decisionOut struct {
	Drop        bool               `json:"drop" jsonschema:"true when a drop action would record the delivery without a todo"`
	Queue       string             `json:"queue,omitempty" jsonschema:"the queue todos would land in"`
	Endpoints   []string           `json:"endpoints,omitempty" jsonschema:"the endpoints todos would be created on"`
	Disposition string             `json:"disposition,omitempty" jsonschema:"the intake outcome: routed, dropped, quarantined or faulted"`
	Faulted     bool               `json:"faulted" jsonschema:"BLOCKING: a rule faulted, so evaluation stopped and the delivery would be held on the owner's quarantine queue as rule_fault instead of routed; a save whose rules fault on recent deliveries is refused"`
	Quarantine  bool               `json:"quarantine,omitempty" jsonschema:"a rule's {quarantine: true} matched: the delivery would be held on the owner's quarantine queue"`
	Unavailable bool               `json:"unavailable,omitempty" jsonschema:"the rule evaluator could not run, so this dry-run says nothing about the rules; retry"`
	Fault       *routing.RuleFault `json:"fault,omitempty" jsonschema:"the fault that stopped evaluation: rule id, index, cause and detail"`
}

type testWebhookRulesOut struct {
	Decision  decisionOut        `json:"decision" jsonschema:"where the delivery would go"`
	Trace     routing.Trace      `json:"trace" jsonschema:"the routing trace that would be recorded"`
	OnceKey   string             `json:"once_key,omitempty" jsonschema:"the at-most-once key a once action would claim (whether it is already claimed is only known at delivery)"`
	WorkOrder *routing.WorkOrder `json:"work_order,omitempty" jsonschema:"the work order a work_order action would attach"`
	Envelope  map[string]any     `json:"envelope,omitempty" jsonschema:"the JSON document the rules evaluated (write expressions against these paths)"`
	// Governing: SPEC-0026 REQ-5, REQ-10.
	Actor *routing.ActorTrust `json:"actor,omitempty" jsonschema:"github/gitea/cairn: who acted and the trust gate's verdict against the webhook's trusted_actors (.actor)"`
	// Held reports the receiver's outcome, not the rules': live traffic records a held delivery with
	// the trust gate's decision and trace and never runs the rules, so decision and trace say exactly
	// that, and rules_would carries what the rules would have done had the actor been trusted.
	Held       bool           `json:"held,omitempty" jsonschema:"true when the trust gate would hold this delivery before any rule ran: decision and trace are then the trust gate's (what live traffic records), and rules_would shows what the rules would do if the actor were trusted"`
	RulesWould *rulesWouldOut `json:"rules_would,omitempty" jsonschema:"only when held: the decision and trace the rules would produce if the actor were trusted; live traffic never evaluates them for this delivery"`
}

// rulesWouldOut is what the rules would do with a held delivery, had the trust gate passed it.
type rulesWouldOut struct {
	Decision decisionOut   `json:"decision" jsonschema:"where the delivery would go if its actor were trusted"`
	Trace    routing.Trace `json:"trace" jsonschema:"the routing trace the rules would produce"`
}

// registerWebhookRuleTools installs the endpoint's allowlisted rule verbs.
func (h *Handler) registerWebhookRuleTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "list_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "list_webhook_rules",
			Description: "List a webhook's routing rules in evaluation order, its default action, and the queues and endpoints its rules may reach."},
			h.listWebhookRulesTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "set_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "set_webhook_rules",
			Description: "Replace a webhook's whole routing configuration atomically: the ordered rules, the default action, and the params rules read as $params. Each rule is a jq filter plus an action: {queue, endpoints?, exclusive?, once?, work_order?} or {drop: true}. First match wins. First match wins, and a rule that errors or times out stops evaluation: the delivery is recorded and routed nowhere. The save is refused, keeping the previous configuration, if a rule is invalid, if a params value is not a string, number, boolean or homogeneous list, or if any rule faults on any of the webhook's 50 most recent deliveries that its trust gate passes."},
			h.setWebhookRulesTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "add_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "add_webhook_rule",
			Description: "Insert one routing rule into a webhook's rule list at a position (default: last). The expression is compiled and the action checked before saving, and the resulting rules are dry-run against the webhook's 50 most recent deliveries that its trust gate passes: a rule that faults on any of them is refused."},
			h.addWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "update_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "update_webhook_rule",
			Description: "Change a routing rule's name, expression, or action in place. Refused if the resulting rules fault on any of the webhook's 50 most recent deliveries that its trust gate passes."},
			h.updateWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "move_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "move_webhook_rule",
			Description: "Move a routing rule to a new position. Order is precedence: the first matching rule wins. Refused if the reordered rules fault on any of the webhook's 50 most recent deliveries that its trust gate passes."},
			h.moveWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "remove_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "remove_webhook_rule",
			Description: "Remove a routing rule from a webhook. Removing a rule that is not there succeeds."},
			h.removeWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "test_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "test_webhook_rules",
			Description: "Dry-run routing: evaluate the saved rules (or candidate rules) against a sample payload or one of this webhook's stored events, and return the decision, the trace, and the envelope the rules saw. A faulted decision (faulted: true) is blocking: that delivery would be recorded and routed nowhere. On a github, gitea or cairn webhook, a delivery whose actor the trust gate would hold (held: true) reports the gate's decision, exactly as live traffic records it, and rules_would shows what the rules would do if the actor were trusted. Saves nothing."},
			h.testWebhookRulesTool(ep))
	}
}

// --- verb handlers ---

func (h *Handler) listWebhookRulesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[webhookIDIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in webhookIDIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		id := strings.TrimSpace(in.WebhookID)
		if id == "" {
			return nil, webhookRulesOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		wr, err := h.store.WebhookRoutingForHuman(ctx, id, ep.OwnerHumanID)
		if err != nil {
			return nil, webhookRulesOut{}, h.mapRuleErr(ep, "list_webhook_rules", err)
		}
		g, err := h.routingGrant(ctx, wr)
		if err != nil {
			return nil, webhookRulesOut{}, h.mapRuleErr(ep, "list_webhook_rules", err)
		}
		return nil, rulesOut(wr, g), nil
	}
}

func (h *Handler) setWebhookRulesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[setWebhookRulesIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in setWebhookRulesIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		rules := make([]routing.Rule, 0, len(in.Rules))
		for _, r := range in.Rules {
			rules = append(rules, fromRuleIO(r))
		}
		return h.mutateRules(ctx, ep, "set_webhook_rules", in.WebhookID, func(routing.Config) (routing.Config, error) {
			return routing.Config{Rules: rules, Default: fromActionIO(in.DefaultAction), Params: in.Params}, nil
		})
	}
}

func (h *Handler) addWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[addWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		r := fromRuleIO(ruleIO{ID: in.ID, Name: in.Name, Expr: in.Expr, Action: in.Action})
		return h.mutateRules(ctx, ep, "add_webhook_rule", in.WebhookID, func(cur routing.Config) (routing.Config, error) {
			pos := len(cur.Rules)
			if in.Position != nil {
				pos = min(max(*in.Position, 0), len(cur.Rules))
			}
			cur.Rules = slices.Insert(slices.Clone(cur.Rules), pos, r)
			return cur, nil
		})
	}
}

func (h *Handler) updateWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[updateWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in updateWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		return h.mutateRules(ctx, ep, "update_webhook_rule", in.WebhookID, func(cur routing.Config) (routing.Config, error) {
			i := ruleIndex(cur, in.RuleID)
			if i < 0 {
				return cur, &toolError{codeRuleNotFound, "rule not found"}
			}
			cur.Rules = slices.Clone(cur.Rules)
			if in.Name != nil {
				cur.Rules[i].Name = *in.Name
			}
			if in.Expr != nil {
				cur.Rules[i].Expr = *in.Expr
			}
			if in.Action != nil {
				cur.Rules[i].Action = *fromActionIO(in.Action)
			}
			return cur, nil
		})
	}
}

func (h *Handler) moveWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[moveWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in moveWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		return h.mutateRules(ctx, ep, "move_webhook_rule", in.WebhookID, func(cur routing.Config) (routing.Config, error) {
			i := ruleIndex(cur, in.RuleID)
			if i < 0 {
				return cur, &toolError{codeRuleNotFound, "rule not found"}
			}
			r := cur.Rules[i]
			rest := slices.Delete(slices.Clone(cur.Rules), i, i+1)
			cur.Rules = slices.Insert(rest, min(max(in.Position, 0), len(rest)), r)
			return cur, nil
		})
	}
}

func (h *Handler) removeWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[removeWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in removeWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		return h.mutateRules(ctx, ep, "remove_webhook_rule", in.WebhookID, func(cur routing.Config) (routing.Config, error) {
			if i := ruleIndex(cur, in.RuleID); i >= 0 {
				cur.Rules = slices.Delete(slices.Clone(cur.Rules), i, i+1)
			}
			return cur, nil
		})
	}
}

// testWebhookRulesTool routes without persisting anything. A stored event is only reachable through
// the webhook it arrived on (store.EventForWebhook), after webhook ownership has been established, so
// a dry-run cannot read another tenant's deliveries.
func (h *Handler) testWebhookRulesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[testWebhookRulesIn, testWebhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in testWebhookRulesIn) (*sdk.CallToolResult, testWebhookRulesOut, error) {
		id := strings.TrimSpace(in.WebhookID)
		if id == "" {
			return nil, testWebhookRulesOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		if (in.EventID > 0) == (in.Payload != nil) {
			return nil, testWebhookRulesOut{}, &toolError{codeInvalidArgument, "exactly one of event_id or payload is required"}
		}
		wr, err := h.store.WebhookRoutingForHuman(ctx, id, ep.OwnerHumanID)
		if err != nil {
			return nil, testWebhookRulesOut{}, h.mapRuleErr(ep, "test_webhook_rules", err)
		}
		g, err := h.routingGrant(ctx, wr)
		if err != nil {
			return nil, testWebhookRulesOut{}, h.mapRuleErr(ep, "test_webhook_rules", err)
		}
		cfg := wr.Config
		if in.Rules != nil {
			cfg = routing.Config{Default: fromActionIO(in.DefaultAction), Params: wr.Config.Params}
			for _, r := range in.Rules {
				cfg.Rules = append(cfg.Rules, fromRuleIO(r))
			}
		}
		if in.Params != nil {
			cfg.Params = in.Params
		}
		if in.Rules != nil || in.Params != nil {
			// Candidates are held to the same standard as a save, so a dry-run that passes is a save
			// that will pass.
			if err := routing.Validate(cfg, g); err != nil {
				return nil, testWebhookRulesOut{}, h.mapRuleErr(ep, "test_webhook_rules", err)
			}
		}

		env := routing.EnvelopeInput{Source: wr.SourceType, WebhookID: wr.WebhookID, TrustMode: wr.TrustMode}
		if in.EventID > 0 {
			ev, err := h.store.EventForWebhook(ctx, in.EventID, wr.WebhookID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, testWebhookRulesOut{}, &toolError{codeNotFound, "event not found on this webhook"}
				}
				return nil, testWebhookRulesOut{}, h.mapRuleErr(ep, "test_webhook_rules", err)
			}
			env = storedEnvelope(wr, ev)
		} else {
			body, err := json.Marshal(in.Payload)
			if err != nil {
				return nil, testWebhookRulesOut{}, &toolError{codeInvalidArgument, "payload is not JSON-encodable"}
			}
			env.Body, env.Headers = body, in.Headers
			env.Verified = wr.TrustMode == trustModeSigned // as a delivery that passed verification
			env.ContentType = "application/json"
			header := func(k string) string {
				for hk, v := range in.Headers {
					if strings.EqualFold(hk, k) {
						return v
					}
				}
				return ""
			}
			env.Kind = routing.EventKind(wr.SourceType, header, body)
			env.Actor = dryRunActor(wr, env.Verified, body)
		}

		d := h.rulesRouter().Route(ctx, cfg, g, env)
		out := testWebhookRulesOut{
			Decision: decisionOut{Drop: d.Drop, Queue: d.Queue, Endpoints: d.Endpoints,
				Disposition: d.Disposition(), Faulted: d.Faulted, Quarantine: d.Quarantine, Unavailable: d.Unavailable, Fault: d.Fault},
			Trace: d.Trace,
			Actor: env.Actor, Held: env.Actor != nil && !env.Actor.IsTrusted(),
		}
		if d.Unavailable {
			// A dry-run that could not run says nothing about the rules. It is reported, but no
			// disposition is claimed for it.
			out.Decision.Disposition = ""
		}
		if out.Held {
			// The receiver never runs the rules on a held delivery: it records the trust gate's
			// decision and trace (ingest selfmanaged.go). Report exactly that, so an agent reading
			// decision.disposition sees what live traffic records, and move the rules' hypothetical
			// outcome aside. No once key or work order: a held delivery claims and mints neither.
			// Governing: SPEC-0026 REQ-5, REQ-13 "trust gate".
			held := routing.UntrustedDecision(env.Actor)
			out.RulesWould = &rulesWouldOut{Decision: out.Decision, Trace: out.Trace}
			out.Decision = decisionOut{Disposition: held.Disposition(), Faulted: held.Faulted, Fault: held.Fault}
			out.Trace = held.Trace
		}
		subject := routing.SubjectOf(wr.SourceType, env.Headers, env.Body)
		if d.Once && !d.Drop && !d.Faulted && !out.Held {
			out.OnceKey = routing.OnceKey(subject, d.Queue)
			out.Trace.OnceKey = out.OnceKey
		}
		if d.WorkOrder && !d.Drop && !d.Faulted && !out.Held {
			wo := routing.BuildWorkOrder(d, env, subject)
			out.WorkOrder = &wo
		}
		if !in.OmitEnvelope {
			out.Envelope = routing.Envelope(env)
		}
		return nil, out, nil
	}
}

// --- helpers ---

// mutateRules is the shared read-modify-validate-write for every rule mutation. change receives the
// current configuration; the result is validated against a freshly computed grant inside the row
// lock, so validation and write cannot be separated by a concurrent edit.
func (h *Handler) mutateRules(ctx context.Context, ep store.AuthEndpoint, tool, webhookID string,
	change func(routing.Config) (routing.Config, error)) (*sdk.CallToolResult, webhookRulesOut, error) {
	id := strings.TrimSpace(webhookID)
	if id == "" {
		return nil, webhookRulesOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
	}
	// Resolve the grant's delivery targets BEFORE UpdateWebhookRouting takes its row lock. That
	// transaction holds a pooled connection until it commits, so resolving targets from inside mutate
	// needs a second one: N concurrent rule edits on an N-connection pool then each hold one and wait
	// forever on another, and ingest wedges with them. Targets read a moment before the lock are as
	// sound as targets read under it — the lock never covered webhook_routes or endpoint state — and
	// every delivery re-applies the grant anyway. The queue ceiling still comes from the locked row.
	pre, err := h.store.WebhookRoutingForHuman(ctx, id, ep.OwnerHumanID)
	if err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}
	targets, err := h.store.ResolveWebhookTargets(ctx, pre.WebhookID, pre.EndpointID)
	if err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}
	scopes, err := h.store.EndpointScopeQueues(ctx, targets) // also before the lock, for the same reason
	if err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}

	// Build and check the candidate from the configuration read above, then dry-run it against the
	// webhook's recent traffic, all BEFORE the lock (SPEC-0026 REQ-3). The dry-run reads up to
	// dryRunEvents stored events and evaluates each one in the sandbox. Doing that under the row lock
	// would hold it for as long as the evaluations take, and the event read would take a second
	// pooled connection, which is the deadlock above. Instead, the locked mutation below saves this
	// exact candidate only if the stored configuration is still the one it was built from. A
	// concurrent edit in between answers conflict rather than saving something that was never
	// dry-run.
	next, err := change(pre.Config)
	if err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}
	for i := range next.Rules {
		if next.Rules[i].ID == "" {
			next.Rules[i].ID = routing.NewRuleID()
		}
	}
	g := routing.Grant{TargetQueue: pre.TargetQueue, Queues: pre.WebhookQueues, Endpoints: targets, EndpointQueues: scopes}
	if err := routing.Validate(next, g); err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}
	// remove_webhook_rule is not dry-run (SPEC-0026 REQ-3 names the four verbs that add or change
	// a rule). Removing a rule adds no new expression, and refusing it would block an owner from
	// deleting the very rule that faults.
	if tool != "remove_webhook_rule" {
		if err := h.dryRunSave(ctx, pre, next, g); err != nil {
			return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
		}
	}

	wr, err := h.store.UpdateWebhookRouting(ctx, id, ep.OwnerHumanID, func(cur store.WebhookRouting) (routing.Config, error) {
		if !reflect.DeepEqual(cur.Config, pre.Config) {
			return routing.Config{}, &toolError{codeConflict, "the webhook's rules changed while this edit was being checked; read them again and retry"}
		}
		// The queue ceiling still comes from the locked row.
		g = routing.Grant{TargetQueue: cur.TargetQueue, Queues: cur.WebhookQueues, Endpoints: targets, EndpointQueues: scopes}
		if err := routing.Validate(next, g); err != nil {
			return routing.Config{}, err
		}
		return next, nil
	})
	if err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}
	return nil, rulesOut(wr, g), nil
}

// dryRunEvents bounds the save-time dry-run: at most this many of the webhook's latest deliveries
// that the trust gate would pass, each inside the per-event evaluation budget (SPEC-0026 REQ-3,
// "Rate Limiting"). dryRunScanPages bounds how far back it reads to find them, a page of
// dryRunEvents at a time, so an outsider's flood of held deliveries costs at most that many cheap
// Go trust checks and never a sandbox evaluation.
const (
	dryRunEvents    = 50
	dryRunScanPages = 10
)

// dryRunSave evaluates a candidate configuration against the webhook's most recent stored
// deliveries and refuses it with invalid_argument if any rule faults on any of them. The error names
// every faulting rule id, with the event ids and the cause. Most faults are type errors that real
// traffic exposes at once, so this turns "installs green, then faults every delivery" into a refused
// save that says why. A webhook with no stored events skips the dry-run. So does a candidate with
// no rules, which cannot fault.
//
// The sandbox being unavailable is not the candidate's fault. It refuses the save with
// "unavailable", so the agent can retry, rather than blaming a rule.
//
// Only deliveries the trust gate would pass TODAY are checked, because only those ever reach the
// rules on live traffic (the receiver holds the rest before any rule runs). Checking a held one
// would let anyone who can make the producer send (on a public repository, anyone who can open an
// issue) refuse the owner's saves with a payload built to fault, and a flood of them would push the
// owner's real traffic out of the window. Held deliveries are skipped and the scan pages further
// back, up to dryRunScanPages, to find dryRunEvents that pass.
// Governing: SPEC-0026 REQ-3 "Save-Time Fault Refusal and Param Typing", REQ-5; ADR-0031.
func (h *Handler) dryRunSave(ctx context.Context, wr store.WebhookRouting, cfg routing.Config, g routing.Grant) error {
	if len(cfg.Rules) == 0 {
		return nil
	}
	router := h.rulesRouter()
	faults := map[string][]int64{} // "rule_id (cause)" -> event ids, newest first
	var order []string
	checked := 0
	var cursor store.EventHistoryDetail // zero: start at the newest
scan:
	for page := 0; page < dryRunScanPages; page++ {
		events, err := h.store.WebhookEventsBefore(ctx, wr.WebhookID, cursor.ReceivedAt, cursor.ID, dryRunEvents)
		if err != nil {
			return err
		}
		for _, ev := range events {
			env := storedEnvelope(wr, ev)
			if env.Actor != nil && !env.Actor.IsTrusted() {
				continue // held by the trust gate: live traffic never runs the rules on it
			}
			d := router.Route(ctx, cfg, g, env)
			if d.Unavailable {
				return &toolError{codeUnavailable, "routing is unavailable, so the rules could not be checked against recent deliveries; retry shortly"}
			}
			if d.Faulted {
				k := d.Fault.RuleID + " (" + d.Fault.Cause + ")"
				if _, seen := faults[k]; !seen {
					order = append(order, k)
				}
				faults[k] = append(faults[k], ev.ID)
			}
			if checked++; checked == dryRunEvents {
				break scan
			}
		}
		if len(events) < dryRunEvents {
			break // no older deliveries
		}
		cursor = events[len(events)-1]
	}
	if len(order) == 0 {
		return nil
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		ids := make([]string, 0, len(faults[k]))
		for _, id := range faults[k] {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		parts = append(parts, "rule "+k+" on events "+strings.Join(ids, ", "))
	}
	return &toolError{codeInvalidArgument, "refused: the rules fault on recent deliveries, which would record them and route them nowhere: " +
		strings.Join(parts, "; ") + ". Fix the rules (test_webhook_rules with event_id reproduces each one) and save again."}
}

// storedEnvelope rebuilds the envelope input a stored delivery was routed on, exactly as the
// receiver saw it: the sanitized headers, the body, the kind and the verification result.
func storedEnvelope(wr store.WebhookRouting, ev store.EventHistoryDetail) routing.EnvelopeInput {
	env := routing.EnvelopeInput{Source: wr.SourceType, WebhookID: wr.WebhookID, TrustMode: wr.TrustMode,
		Body: ev.Payload, Kind: ev.EventType, Verified: ev.Verified, ContentType: ev.ContentType,
		Actor: dryRunActor(wr, ev.Verified, ev.Payload)}
	_ = json.Unmarshal(ev.Headers, &env.Headers)
	return env
}

// dryRunActor is the trust gate's verdict exactly as the receiver would compute it (ingest
// trustGate), so a dry-run's .actor matches live traffic: a missing or unreadable list, or an
// unverified body, trusts no one. nil for a source with no gate. Governing: SPEC-0026 REQ-5, REQ-10.
func dryRunActor(wr store.WebhookRouting, verified bool, body []byte) *routing.ActorTrust {
	if !routing.HasActorProjection(wr.SourceType) {
		return nil
	}
	ta, _ := routing.DecodeTrustedActors(wr.SourceType, wr.TrustedActors)
	if !verified {
		ta, _ = routing.DefaultTrustedActors(wr.SourceType)
	}
	return routing.EvaluateTrust(wr.SourceType, ta, body)
}

// routingGrant builds a webhook's grant from switchboard state: its target queue, its OWNER's
// allowed webhook queues (the owner-wide union the routing read computes — see store.WebhookRouting),
// and its live, authorized delivery targets.
func (h *Handler) routingGrant(ctx context.Context, wr store.WebhookRouting) (routing.Grant, error) {
	targets, err := h.store.ResolveWebhookTargets(ctx, wr.WebhookID, wr.EndpointID)
	if err != nil {
		return routing.Grant{}, err
	}
	scopes, err := h.store.EndpointScopeQueues(ctx, targets)
	if err != nil {
		return routing.Grant{}, err
	}
	return routing.Grant{TargetQueue: wr.TargetQueue, Queues: wr.WebhookQueues, Endpoints: targets, EndpointQueues: scopes}, nil
}

// rulesRouter is the router dry-runs use: the same sandbox the receiver uses unless a test replaced it.
func (h *Handler) rulesRouter() routing.Router {
	if p := h.router.Load(); p != nil {
		return *p
	}
	return routing.Unavailable{}
}

// SetRouter replaces the router used by test_webhook_rules (tests install routing.InProcess{}).
func (h *Handler) SetRouter(r routing.Router) { h.router.Store(&r) }

// mapRuleErr maps rule-verb failures onto stable codes. Validation failures carry routing's own code
// and a message naming the offending rule — except not_granted, which is the house forbidden code.
func (h *Handler) mapRuleErr(ep store.AuthEndpoint, tool string, err error) error {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	var ve *routing.ValidationError
	if errors.As(err, &ve) {
		code := ve.Code
		if code == routing.CodeNotGranted {
			code = codeForbidden
		}
		return &toolError{code, ve.Error()}
	}
	return h.mapWebhookErr(ep, tool, err)
}

func ruleIndex(cfg routing.Config, id string) int {
	id = strings.TrimSpace(id)
	return slices.IndexFunc(cfg.Rules, func(r routing.Rule) bool { return id != "" && r.ID == id })
}

func fromActionIO(a *actionIO) *routing.Action {
	if a == nil {
		return nil
	}
	return &routing.Action{Queue: strings.TrimSpace(a.Queue), Drop: a.Drop, Quarantine: a.Quarantine, Endpoints: a.Endpoints,
		Exclusive: a.Exclusive, Once: a.Once, WorkOrder: a.WorkOrder}
}

func fromRuleIO(r ruleIO) routing.Rule {
	return routing.Rule{ID: strings.TrimSpace(r.ID), Name: r.Name, Expr: r.Expr, Action: *fromActionIO(&r.Action)}
}

func toActionIO(a routing.Action) actionIO {
	return actionIO{Queue: a.Queue, Drop: a.Drop, Quarantine: a.Quarantine, Endpoints: a.Endpoints,
		Exclusive: a.Exclusive, Once: a.Once, WorkOrder: a.WorkOrder}
}

func rulesOut(wr store.WebhookRouting, g routing.Grant) webhookRulesOut {
	out := webhookRulesOut{
		WebhookID: wr.WebhookID, SourceType: wr.SourceType, TargetQueue: wr.TargetQueue,
		Rules:  make([]ruleIO, 0, len(wr.Config.Rules)),
		Params: wr.Config.Params,
		Grant:  grantOut{Queues: []string{g.TargetQueue}, Endpoints: nonNil(g.Endpoints)},
	}
	for _, q := range g.Queues {
		if !slices.Contains(out.Grant.Queues, q) {
			out.Grant.Queues = append(out.Grant.Queues, q)
		}
	}
	if wr.Config.Default != nil {
		a := toActionIO(*wr.Config.Default)
		out.DefaultAction = &a
	}
	for _, r := range wr.Config.Rules {
		out.Rules = append(out.Rules, ruleIO{ID: r.ID, Name: r.Name, Expr: r.Expr, Action: toActionIO(r.Action)})
	}
	return out
}
