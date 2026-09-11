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
//     switchboard state alone: the webhook's target queue, its OWNER endpoint's webhook-queue
//     ceiling, and its live, authorized delivery targets (ResolveWebhookTargets). A rule can narrow
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

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/routing"
	"github.com/joestump/switchboard/internal/store"
)

const codeRuleNotFound = "rule_not_found"

// --- tool input/output shapes (SDK-inferred JSON schemas) ---

type actionIO struct {
	Queue     string   `json:"queue,omitempty" jsonschema:"deliver to this queue (the webhook's target queue or one in its owner's allowed webhook queues)"`
	Drop      bool     `json:"drop,omitempty" jsonschema:"true to record the delivery without creating a todo or ringing a doorbell"`
	Endpoints []string `json:"endpoints,omitempty" jsonschema:"with queue only: deliver to just these of the webhook's delivery targets (see grant.endpoints); omitted means every target"`
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
	WebhookID     string    `json:"webhook_id" jsonschema:"the webhook"`
	SourceType    string    `json:"source_type" jsonschema:"the webhook's source type (the envelope's .source)"`
	TargetQueue   string    `json:"target_queue" jsonschema:"the webhook's target queue"`
	DefaultAction *actionIO `json:"default_action,omitempty" jsonschema:"what unmatched deliveries do; omitted means the target queue on every target"`
	Rules         []ruleIO  `json:"rules" jsonschema:"the rules, in evaluation order (first match wins)"`
	Grant         grantOut  `json:"grant" jsonschema:"what actions may reach right now"`
}

type webhookIDIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
}

type setWebhookRulesIn struct {
	WebhookID     string    `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	Rules         []ruleIO  `json:"rules" jsonschema:"the complete ordered rule list; replaces the current one atomically"`
	DefaultAction *actionIO `json:"default_action,omitempty" jsonschema:"what unmatched deliveries do; omit for the webhook's target queue"`
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
	OmitEnvelope  bool              `json:"omit_envelope,omitempty" jsonschema:"true to leave the evaluated envelope out of the result"`
}

type decisionOut struct {
	Drop      bool     `json:"drop" jsonschema:"true when the delivery would be recorded without a todo"`
	Queue     string   `json:"queue,omitempty" jsonschema:"the queue todos would land in"`
	Endpoints []string `json:"endpoints,omitempty" jsonschema:"the endpoints todos would be created on"`
}

type testWebhookRulesOut struct {
	Decision decisionOut    `json:"decision" jsonschema:"where the delivery would go"`
	Trace    routing.Trace  `json:"trace" jsonschema:"the routing trace that would be recorded"`
	Envelope map[string]any `json:"envelope,omitempty" jsonschema:"the JSON document the rules evaluated (write expressions against these paths)"`
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
			Description: "Replace a webhook's whole routing rule list (and default action) atomically. Each rule is a jq filter plus an action: {queue, endpoints?} or {drop: true}. First match wins. An invalid rule rejects the save and keeps the previous rules."},
			h.setWebhookRulesTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "add_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "add_webhook_rule",
			Description: "Insert one routing rule into a webhook's rule list at a position (default: last). The expression is compiled and the action checked before saving."},
			h.addWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "update_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "update_webhook_rule",
			Description: "Change a routing rule's name, expression, or action in place."},
			h.updateWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "move_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "move_webhook_rule",
			Description: "Move a routing rule to a new position. Order is precedence: the first matching rule wins."},
			h.moveWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "remove_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "remove_webhook_rule",
			Description: "Remove a routing rule from a webhook. Removing a rule that is not there succeeds."},
			h.removeWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "test_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "test_webhook_rules",
			Description: "Dry-run routing: evaluate the saved rules (or candidate rules) against a sample payload or one of this webhook's stored events, and return the decision, the trace, and the envelope the rules saw. Saves nothing."},
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
			return routing.Config{Rules: rules, Default: fromActionIO(in.DefaultAction)}, nil
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
			cfg = routing.Config{Default: fromActionIO(in.DefaultAction)}
			for _, r := range in.Rules {
				cfg.Rules = append(cfg.Rules, fromRuleIO(r))
			}
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
			_ = json.Unmarshal(ev.Headers, &env.Headers)
			env.Body, env.Kind, env.Verified, env.ContentType = ev.Payload, ev.EventType, ev.Verified, ev.ContentType
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
		}

		d := h.rulesRouter().Route(ctx, cfg, g, env)
		out := testWebhookRulesOut{
			Decision: decisionOut{Drop: d.Drop, Queue: d.Queue, Endpoints: d.Endpoints},
			Trace:    d.Trace,
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
	var g routing.Grant
	wr, err := h.store.UpdateWebhookRouting(ctx, id, ep.OwnerHumanID, func(cur store.WebhookRouting) (routing.Config, error) {
		next, err := change(cur.Config)
		if err != nil {
			return routing.Config{}, err
		}
		for i := range next.Rules {
			if next.Rules[i].ID == "" {
				next.Rules[i].ID = routing.NewRuleID()
			}
		}
		if g, err = h.routingGrant(ctx, cur); err != nil {
			return routing.Config{}, err
		}
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

// routingGrant builds a webhook's grant from switchboard state: its target queue, its OWNER
// endpoint's webhook-queue ceiling, and its live, authorized delivery targets.
func (h *Handler) routingGrant(ctx context.Context, wr store.WebhookRouting) (routing.Grant, error) {
	targets, err := h.store.ResolveWebhookTargets(ctx, wr.WebhookID, wr.EndpointID)
	if err != nil {
		return routing.Grant{}, err
	}
	return routing.Grant{TargetQueue: wr.TargetQueue, Queues: wr.WebhookQueues, Endpoints: targets}, nil
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
	return &routing.Action{Queue: strings.TrimSpace(a.Queue), Drop: a.Drop, Endpoints: a.Endpoints}
}

func fromRuleIO(r ruleIO) routing.Rule {
	return routing.Rule{ID: strings.TrimSpace(r.ID), Name: r.Name, Expr: r.Expr, Action: *fromActionIO(&r.Action)}
}

func toActionIO(a routing.Action) actionIO {
	return actionIO{Queue: a.Queue, Drop: a.Drop, Endpoints: a.Endpoints}
}

func rulesOut(wr store.WebhookRouting, g routing.Grant) webhookRulesOut {
	out := webhookRulesOut{
		WebhookID: wr.WebhookID, SourceType: wr.SourceType, TargetQueue: wr.TargetQueue,
		Rules: make([]ruleIO, 0, len(wr.Config.Rules)),
		Grant: grantOut{Queues: []string{g.TargetQueue}, Endpoints: nonNil(g.Endpoints)},
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
