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
//  2. WEBHOOK OWNERSHIP — the calling endpoint must be the webhook's own endpoint
//     (store.WebhookRoutingForEndpoint / UpdateWebhookRouting); unknown, malformed, and any other
//     endpoint's webhook ids, the same human's included, are uniformly not_found, exactly as the
//     ADR-0022 route verbs answer. Governing: ADR-0038, SPEC-0033 REQ "Closing the Audited
//     Surfaces" (F3, F19).
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
// @joestump-agent 09/29/2026 - The verb bodies moved to internal/manage, which the human API's rule
// routes also call (SPEC-0035 REQ "Shared Implementation With MCP"). This file is now the MCP
// adapter: it decodes arguments, passes the calling endpoint as the principal, and maps manage's
// sentinel errors onto the stable codes. The endpoint-ownership rule above is unchanged.

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/manage"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	codeRuleNotFound = string(manage.ErrRuleNotFound)
	// codeUnavailable says the rule evaluator (or another optional subsystem) could not run. It is
	// transient and names no rule. Governing: SPEC-0026 REQ-3.
	codeUnavailable = string(manage.ErrUnavailable)
)

// --- tool input/output shapes (SDK-inferred JSON schemas) ---
//
// The output shapes and the rule/action shapes live in internal/manage so the human API returns
// exactly the same fields; these aliases keep the verb code and its tests reading as before.

type (
	actionIO            = manage.ActionIO
	ruleIO              = manage.RuleIO
	webhookRulesOut     = manage.RulesOut
	testWebhookRulesOut = manage.TestOut
)

type webhookIDIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
}

type setWebhookRulesIn struct {
	WebhookID     string         `json:"webhook_id" jsonschema:"a webhook this endpoint's human owns"`
	Rules         []ruleIO       `json:"rules" jsonschema:"the complete ordered rule list; replaces the current one atomically"`
	DefaultAction *actionIO      `json:"default_action,omitempty" jsonschema:"what unmatched deliveries do; omit for the webhook's target queue"`
	Params        map[string]any `json:"params,omitempty" jsonschema:"values bound as $params in every rule; replaces the current params when present. Omit to keep the current params unchanged; pass {} to clear them"`
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

// registerWebhookRuleTools installs the endpoint's allowlisted rule verbs.
func (h *Handler) registerWebhookRuleTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "list_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "list_webhook_rules",
			Description: "List a webhook's routing rules in evaluation order, its default action, and the queues and endpoints its rules may reach."},
			h.listWebhookRulesTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "set_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "set_webhook_rules",
			Description: "Replace a webhook's whole routing configuration atomically: the ordered rules, the default action, and the params rules read as $params when params is present. Omitting params keeps the saved params; params: {} clears them. Each rule is a jq filter plus an action: {queue, endpoints?, exclusive?, once?, work_order?}, {drop: true} or {quarantine: true}. First match wins, and a rule that errors or times out stops evaluation: the delivery is held on the owner's quarantine queue as rule_fault, never handed to an agent. The save is refused, keeping the previous configuration, if a rule is invalid, if a params value is not a string, number, boolean or homogeneous list, or if any rule faults on any of the webhook's 50 most recent deliveries that its trust gate passes (found within its newest 500; the result's dry_run says how many were checked)."},
			h.setWebhookRulesTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "add_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "add_webhook_rule",
			Description: "Insert one routing rule into a webhook's rule list at a position (default: last). The expression is compiled and the action checked before saving, and the resulting rules are dry-run against the webhook's 50 most recent deliveries: a rule that faults on any of them is refused."},
			h.addWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "update_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "update_webhook_rule",
			Description: "Change a routing rule's name, expression, or action in place. Refused if the resulting rules fault on any of the webhook's 50 most recent deliveries."},
			h.updateWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "move_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "move_webhook_rule",
			Description: "Move a routing rule to a new position. Order is precedence: the first matching rule wins. Refused if the reordered rules fault on any of the webhook's 50 most recent deliveries."},
			h.moveWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "remove_webhook_rule") {
		sdk.AddTool(srv, &sdk.Tool{Name: "remove_webhook_rule",
			Description: "Remove a routing rule from a webhook. Removing a rule that is not there succeeds."},
			h.removeWebhookRuleTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "test_webhook_rules") {
		sdk.AddTool(srv, &sdk.Tool{Name: "test_webhook_rules",
			Description: "Dry-run routing: evaluate the saved rules (or candidate rules) against a sample payload or one of this webhook's stored events, and return the decision, the trace, and the envelope the rules saw. A faulted decision (faulted: true) is blocking: that delivery would be held on the owner's quarantine queue (quarantine_reason rule_fault) instead of routed. On a github, gitea or cairn webhook, a delivery whose actor the trust gate would hold (held: true) reports the gate's decision, exactly as live traffic records it (disposition quarantined, quarantine_reason untrusted_actor), and rules_would shows what the rules would do if the actor were trusted. Saves nothing."},
			h.testWebhookRulesTool(ep))
	}
}

// --- verb handlers ---

// ruleSvc is the shared rule-management code, driven by this surface's endpoint principal. The
// router is read on every call, so SetRouter takes effect for live sessions.
func (h *Handler) ruleSvc() manage.Rules {
	return manage.Rules{Store: h.store, Router: h.rulesRouter}
}

func (h *Handler) listWebhookRulesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[webhookIDIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in webhookIDIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		out, err := h.ruleSvc().Get(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID)
		if err != nil {
			return nil, webhookRulesOut{}, h.mapRuleErr(ep, "list_webhook_rules", err)
		}
		return nil, out, nil
	}
}

// setWebhookRulesTool keeps REQ-4's keep-on-omit semantics (SPEC-0026): an omitted params keeps
// the saved ones and only an explicit {} clears them, the same answer the human API gives an
// omitted params.
func (h *Handler) setWebhookRulesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[setWebhookRulesIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in setWebhookRulesIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		out, _, err := h.ruleSvc().Replace(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID,
			manage.Replacement{Rules: in.Rules, DefaultAction: in.DefaultAction, Params: in.Params, KeepParams: in.Params == nil})
		return h.ruleResult(ep, "set_webhook_rules", out, err)
	}
}

func (h *Handler) addWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[addWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in addWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		change := manage.Insert(ruleIO{ID: in.ID, Name: in.Name, Expr: in.Expr, Action: in.Action}, in.Position)
		out, _, err := h.ruleSvc().Mutate(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID, true, change)
		return h.ruleResult(ep, "add_webhook_rule", out, err)
	}
}

func (h *Handler) updateWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[updateWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in updateWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		change := manage.Update(in.RuleID, in.Name, in.Expr, in.Action)
		out, _, err := h.ruleSvc().Mutate(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID, true, change)
		return h.ruleResult(ep, "update_webhook_rule", out, err)
	}
}

func (h *Handler) moveWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[moveWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in moveWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		change := manage.Move(in.RuleID, in.Position)
		out, _, err := h.ruleSvc().Mutate(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID, true, change)
		return h.ruleResult(ep, "move_webhook_rule", out, err)
	}
}

// removeWebhookRuleTool skips the save-time dry run (see manage.Remove).
func (h *Handler) removeWebhookRuleTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[removeWebhookRuleIn, webhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in removeWebhookRuleIn) (*sdk.CallToolResult, webhookRulesOut, error) {
		out, _, err := h.ruleSvc().Mutate(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID, false, manage.Remove(in.RuleID))
		return h.ruleResult(ep, "remove_webhook_rule", out, err)
	}
}

// testWebhookRulesTool routes without persisting anything (manage.Rules.Test).
func (h *Handler) testWebhookRulesTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[testWebhookRulesIn, testWebhookRulesOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in testWebhookRulesIn) (*sdk.CallToolResult, testWebhookRulesOut, error) {
		out, err := h.ruleSvc().Test(ctx, manage.EndpointPrincipal(ep.ID), in.WebhookID, manage.TestIn{
			EventID: in.EventID, Payload: in.Payload, Headers: in.Headers, Rules: in.Rules,
			DefaultAction: in.DefaultAction, Params: in.Params, OmitEnvelope: in.OmitEnvelope,
		})
		if err != nil {
			return nil, testWebhookRulesOut{}, h.mapRuleErr(ep, "test_webhook_rules", err)
		}
		return nil, out, nil
	}
}

// --- helpers ---

func (h *Handler) ruleResult(ep store.AuthEndpoint, tool string, out webhookRulesOut, err error) (*sdk.CallToolResult, webhookRulesOut, error) {
	if err != nil {
		return nil, webhookRulesOut{}, h.mapRuleErr(ep, tool, err)
	}
	return nil, out, nil
}

// rulesRouter is the router dry-runs use: the same sandbox the receiver uses unless a test replaced it.
func (h *Handler) rulesRouter() routing.Router {
	if p := h.router.Load(); p != nil {
		return *p
	}
	return routing.Unavailable{}
}

// RulesRouter is the evaluator this handler's dry runs use. The human API's rule routes share it, so
// both surfaces run candidates through the same sandbox.
func (h *Handler) RulesRouter() routing.Router { return h.rulesRouter() }

// SetRouter replaces the router used by test_webhook_rules (tests install routing.InProcess{}).
func (h *Handler) SetRouter(r routing.Router) { h.router.Store(&r) }

// mapRuleErr maps rule-verb failures onto stable codes. manage's sentinel kinds are the codes
// themselves. Validation failures carry routing's own code and a message naming the offending rule —
// except not_granted, which is the house forbidden code.
func (h *Handler) mapRuleErr(ep store.AuthEndpoint, tool string, err error) error {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	var me *manage.Error
	if errors.As(err, &me) {
		return &toolError{me.Code(), me.Msg}
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
