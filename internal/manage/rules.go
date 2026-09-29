// Package manage holds the management logic that the MCP verbs and the human API (/api/v1) both
// run, so the two surfaces cannot drift: one grant computation, one validation path, one save-time
// dry run, one conflict check. Each surface is a thin adapter that decodes its own input, calls in
// here with its principal, and maps the sentinel errors below onto its own envelope.
package manage

// Routing-Rule Management
//
// A webhook's routing configuration (ADR-0024) is an ordered, first-match-wins list of jq rules, a
// default action, and the params rules read as $params. This file reads it, replaces or edits it,
// and dry-runs candidates against a sample payload or a stored delivery, for either principal:
//
//   - an ENDPOINT (MCP): the webhook must be the calling endpoint's own. Unknown, malformed and any
//     other endpoint's webhook ids, the same human's included, are uniformly not_found (ADR-0038,
//     SPEC-0033 F3/F19). Nothing here widens that: an endpoint principal is used as the owning
//     endpoint directly, and the store's endpoint-scoped reads enforce the match.
//   - a HUMAN (the human API): the webhook's owning endpoint must hang off one of the human's
//     agents (store.WebhookOwnerForHuman). That endpoint is then used exactly as an endpoint
//     principal would be, so both surfaces share every read and the row-locked write. A write to a
//     webhook whose owning endpoint is not active is refused with conflict and the state.
//
// Every save is validated by routing.Validate against a grant built from switchboard state alone
// (the webhook's target queue, its owner's allowed webhook queues, its live delivery targets and
// their scopes), dry-run against the webhook's latest deliveries, and written under a row lock only
// if the stored configuration is still the one the candidate was built from. A failed save leaves
// the previous configuration in force.
//
// Governing: ADR-0024, SPEC-0020 REQ "Rule Validation at Save Time", REQ "Routing Trace", REQ
// "Isolation and Tenant Safety"; ADR-0031, SPEC-0026 REQ-3 "Save-Time Fault Refusal and Param
// Typing"; ADR-0038, SPEC-0033 REQ "Closing the Audited Surfaces"; SPEC-0035 REQ "Shared
// Implementation With MCP", REQ "Rule Management", REQ "Reach on Every Route".
//
// @joestump-agent 09/29/2026 - Extracted from internal/mcp/webhook_rules.go (mutateRules,
// dryRunSave, routingGrant, the dry-run body, and the I/O shapes) and parameterized by a Principal,
// so the human API's rule routes run the MCP verbs' code rather than a copy of it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Kind is a domain failure both surfaces must tell apart. Its value is the stable machine code the
// MCP surface already answers with (SPEC-0006 REQ "Structured Output and Stable Error Shape"), and
// the human API maps each one to an HTTP status (SPEC-0035 REQ "Human API Surface").
type Kind string

func (k Kind) Error() string { return string(k) }

// The sentinel kinds. Surfaces match them with errors.Is, never by message text.
const (
	ErrInvalidArgument Kind = "invalid_argument"
	ErrNotFound        Kind = "not_found"
	ErrRuleNotFound    Kind = "rule_not_found"
	ErrConflict        Kind = "conflict"
	// ErrUnavailable says the rule evaluator could not run, so a save could not be checked. It is
	// transient and names no rule. Governing: SPEC-0026 REQ-3.
	ErrUnavailable Kind = "unavailable"
)

// Error is a domain failure: a Kind plus a message that is safe to show the caller (it never carries
// secret material or internal error text). State is set when a write was refused because the
// webhook's owning endpoint is not active.
type Error struct {
	Kind  Kind
	Msg   string
	State string
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Msg }

// Unwrap makes errors.Is(err, manage.ErrConflict) and its siblings work.
func (e *Error) Unwrap() error { return e.Kind }

// Code is the stable machine code for the failure.
func (e *Error) Code() string { return string(e.Kind) }

func fail(k Kind, msg string) *Error { return &Error{Kind: k, Msg: msg} }

// RuleStore is the store surface rule management needs. Every read and the write are scoped to the
// webhook's owning endpoint.
type RuleStore interface {
	WebhookRoutingForEndpoint(ctx context.Context, webhookID, endpointID string) (store.WebhookRouting, error)
	UpdateWebhookRouting(ctx context.Context, webhookID, endpointID string, mutate func(store.WebhookRouting) (routing.Config, error)) (store.WebhookRouting, error)
	ResolveWebhookTargets(ctx context.Context, webhookID, ownerEndpointID string) ([]string, error)
	EndpointScopeQueues(ctx context.Context, endpointIDs []string) (map[string][]string, error)
	EventForWebhook(ctx context.Context, eventID int64, webhookID string) (store.EventHistoryDetail, error)
	RecentWebhookEvents(ctx context.Context, webhookID string, limit int) ([]store.EventHistoryDetail, error)
}

// HumanReach resolves a webhook's owning endpoint within a human's reach.
type HumanReach interface {
	WebhookOwnerForHuman(ctx context.Context, webhookID, ownerHumanID string) (store.WebhookOwner, error)
}

// Principal is who is asking. Exactly one field is set; anything else resolves nothing.
type Principal struct {
	// EndpointID is an MCP caller: the webhook must be this endpoint's own.
	EndpointID string
	// HumanID is a human API caller: the webhook's owning endpoint must belong to this human.
	HumanID string
}

// EndpointPrincipal is the MCP caller.
func EndpointPrincipal(endpointID string) Principal { return Principal{EndpointID: endpointID} }

// HumanPrincipal is the human API caller.
func HumanPrincipal(humanID string) Principal { return Principal{HumanID: humanID} }

// Rules runs rule management. Humans may be nil (the MCP surface never needs it), in which case a
// human principal resolves nothing. Router supplies the evaluator for dry runs; nil, or a nil
// result, means none is available.
type Rules struct {
	Store  RuleStore
	Humans HumanReach
	Router func() routing.Router
}

// DryRunEvents bounds the save-time dry run: at most this many of the webhook's latest deliveries,
// each inside the per-event evaluation budget (SPEC-0026 REQ-3, "Rate Limiting").
const DryRunEvents = 50

// trustModeSigned is the trust mode of an HMAC-verified webhook: a sample payload is routed as a
// delivery that passed verification.
const trustModeSigned = "signed"

// --- I/O shapes, shared by both surfaces so their fields cannot drift ---

// ActionIO is where a matching delivery goes.
type ActionIO struct {
	Queue     string   `json:"queue,omitempty" jsonschema:"deliver to this queue (the webhook's target queue or one in its owner's allowed webhook queues)"`
	Drop      bool     `json:"drop,omitempty" jsonschema:"true to record the delivery without creating a todo or ringing a doorbell"`
	Endpoints []string `json:"endpoints,omitempty" jsonschema:"with queue only: deliver to just these of the webhook's delivery targets (see grant.endpoints); omitted means every target"`
	Exclusive bool     `json:"exclusive,omitempty" jsonschema:"with queue only: deliver to exactly one target, the first (owner first, then routes by grant time) whose endpoint scope includes the queue"`
	Once      bool     `json:"once,omitempty" jsonschema:"with queue only: at most one work order per subject (issue or cairn artifact) per queue; later deliveries about it are recorded but mint nothing"`
	WorkOrder bool     `json:"work_order,omitempty" jsonschema:"with queue only: attach a switchboard-authored work order (lane, verified provenance, authorizing rule, subject) to each todo"`
}

// RuleIO is one ordered rule.
type RuleIO struct {
	ID     string   `json:"id,omitempty" jsonschema:"stable rule id; minted when omitted on create"`
	Name   string   `json:"name,omitempty" jsonschema:"label recorded on the routing trace"`
	Expr   string   `json:"expr" jsonschema:"jq filter over the routing envelope; its first output decides (anything but false or null matches)"`
	Action ActionIO `json:"action" jsonschema:"where a matching delivery goes: exactly one of queue or drop"`
}

// GrantOut is what actions may reach right now.
type GrantOut struct {
	Queues    []string `json:"queues" jsonschema:"queues an action may name: the webhook's target queue plus its owner's allowed webhook queues"`
	Endpoints []string `json:"endpoints" jsonschema:"endpoints an action may narrow to: the webhook's live delivery targets, owner first"`
}

// RulesOut is a webhook's routing configuration and grant: what list_webhook_rules and every rule
// save return, and what GET /api/v1/webhooks/{id}/rules returns. It is also a valid body for PUT
// /api/v1/webhooks/{id}/rules, which ignores the read-only fields.
type RulesOut struct {
	WebhookID     string         `json:"webhook_id" jsonschema:"the webhook"`
	SourceType    string         `json:"source_type" jsonschema:"the webhook's source type (the envelope's .source)"`
	TargetQueue   string         `json:"target_queue" jsonschema:"the webhook's target queue"`
	DefaultAction *ActionIO      `json:"default_action,omitempty" jsonschema:"what unmatched deliveries do; omitted means the target queue on every target"`
	Rules         []RuleIO       `json:"rules" jsonschema:"the rules, in evaluation order (first match wins)"`
	Params        map[string]any `json:"params,omitempty" jsonschema:"owner-set values rules read as $params (e.g. trusted-actor allowlists)"`
	Grant         GrantOut       `json:"grant" jsonschema:"what actions may reach right now"`
}

// TestIn is a dry run: exactly one of EventID or Payload, optionally with candidate rules, default
// and params that are tried instead of the saved ones and never saved.
type TestIn struct {
	EventID       int64             `json:"event_id,omitempty"`
	Payload       any               `json:"payload,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Rules         []RuleIO          `json:"rules,omitempty"`
	DefaultAction *ActionIO         `json:"default_action,omitempty"`
	Params        map[string]any    `json:"params,omitempty"`
	OmitEnvelope  bool              `json:"omit_envelope,omitempty"`
}

// DecisionOut is where a dry-run delivery would go.
type DecisionOut struct {
	Drop        bool               `json:"drop" jsonschema:"true when a drop action would record the delivery without a todo"`
	Queue       string             `json:"queue,omitempty" jsonschema:"the queue todos would land in"`
	Endpoints   []string           `json:"endpoints,omitempty" jsonschema:"the endpoints todos would be created on"`
	Disposition string             `json:"disposition,omitempty" jsonschema:"the intake outcome: routed, dropped or faulted"`
	Faulted     bool               `json:"faulted" jsonschema:"BLOCKING: a rule faulted, so evaluation stopped and the delivery would be recorded and routed nowhere; a save whose rules fault on recent deliveries is refused"`
	Unavailable bool               `json:"unavailable,omitempty" jsonschema:"the rule evaluator could not run, so this dry-run says nothing about the rules; retry"`
	Fault       *routing.RuleFault `json:"fault,omitempty" jsonschema:"the fault that stopped evaluation: rule id, index, cause and detail"`
}

// TestOut is a dry run's result.
type TestOut struct {
	Decision  DecisionOut        `json:"decision" jsonschema:"where the delivery would go"`
	Trace     routing.Trace      `json:"trace" jsonschema:"the routing trace that would be recorded"`
	OnceKey   string             `json:"once_key,omitempty" jsonschema:"the at-most-once key a once action would claim (whether it is already claimed is only known at delivery)"`
	WorkOrder *routing.WorkOrder `json:"work_order,omitempty" jsonschema:"the work order a work_order action would attach"`
	Envelope  map[string]any     `json:"envelope,omitempty" jsonschema:"the JSON document the rules evaluated (write expressions against these paths)"`
}

// Change is what a save replaced and what it stored, for a surface that records the difference.
type Change struct {
	Previous routing.Config
	Saved    routing.Config
}

// Replacement is a whole-configuration replace (set_webhook_rules, PUT /api/v1/webhooks/{id}/rules).
type Replacement struct {
	Rules         []RuleIO
	DefaultAction *ActionIO
	Params        map[string]any
	// KeepParams carries the stored params forward and ignores Params. It is how the human API
	// answers a body that omits params: clearing a router's params silently drops every handoff
	// (ADR-0025), so only an explicit clear may do it there.
	KeepParams bool
}

// --- operations ---

// Get reads a webhook's rules, default, params and grant.
func (r Rules) Get(ctx context.Context, p Principal, webhookID string) (RulesOut, error) {
	id, err := requireID(webhookID)
	if err != nil {
		return RulesOut{}, err
	}
	endpointID, err := r.resolve(ctx, p, id, false)
	if err != nil {
		return RulesOut{}, err
	}
	wr, err := r.routing(ctx, id, endpointID)
	if err != nil {
		return RulesOut{}, err
	}
	g, err := r.grant(ctx, wr)
	if err != nil {
		return RulesOut{}, err
	}
	return Out(wr, g), nil
}

// Replace swaps the whole configuration atomically, through the full save path.
func (r Rules) Replace(ctx context.Context, p Principal, webhookID string, rep Replacement) (RulesOut, Change, error) {
	rules := make([]routing.Rule, 0, len(rep.Rules))
	for _, ru := range rep.Rules {
		rules = append(rules, FromRuleIO(ru))
	}
	return r.Mutate(ctx, p, webhookID, true, func(cur routing.Config) (routing.Config, error) {
		next := routing.Config{Rules: rules, Default: FromActionIO(rep.DefaultAction), Params: rep.Params}
		if rep.KeepParams {
			next.Params = cur.Params
		}
		return next, nil
	})
}

// Insert returns a change that inserts rule at pos (nil appends; clamped to the list).
func Insert(rule RuleIO, pos *int) func(routing.Config) (routing.Config, error) {
	ru := FromRuleIO(rule)
	return func(cur routing.Config) (routing.Config, error) {
		at := len(cur.Rules)
		if pos != nil {
			at = min(max(*pos, 0), len(cur.Rules))
		}
		cur.Rules = slices.Insert(slices.Clone(cur.Rules), at, ru)
		return cur, nil
	}
}

// Update returns a change that edits one rule in place; nil fields are left as they are.
func Update(ruleID string, name, expr *string, action *ActionIO) func(routing.Config) (routing.Config, error) {
	return func(cur routing.Config) (routing.Config, error) {
		i := RuleIndex(cur, ruleID)
		if i < 0 {
			return cur, fail(ErrRuleNotFound, "rule not found")
		}
		cur.Rules = slices.Clone(cur.Rules)
		if name != nil {
			cur.Rules[i].Name = *name
		}
		if expr != nil {
			cur.Rules[i].Expr = *expr
		}
		if action != nil {
			cur.Rules[i].Action = *FromActionIO(action)
		}
		return cur, nil
	}
}

// Move returns a change that moves one rule to pos (clamped to the list). Order is precedence.
func Move(ruleID string, pos int) func(routing.Config) (routing.Config, error) {
	return func(cur routing.Config) (routing.Config, error) {
		i := RuleIndex(cur, ruleID)
		if i < 0 {
			return cur, fail(ErrRuleNotFound, "rule not found")
		}
		ru := cur.Rules[i]
		rest := slices.Delete(slices.Clone(cur.Rules), i, i+1)
		cur.Rules = slices.Insert(rest, min(max(pos, 0), len(rest)), ru)
		return cur, nil
	}
}

// Remove returns a change that removes one rule; an absent rule is not an error. Save it with
// dryRun false: removing a rule adds no expression, and refusing it would block an owner from
// deleting the very rule that faults (SPEC-0026 REQ-3 names the four verbs that add or change one).
func Remove(ruleID string) func(routing.Config) (routing.Config, error) {
	return func(cur routing.Config) (routing.Config, error) {
		if i := RuleIndex(cur, ruleID); i >= 0 {
			cur.Rules = slices.Delete(slices.Clone(cur.Rules), i, i+1)
		}
		return cur, nil
	}
}

// Mutate is the shared read-modify-validate-write for every rule save. change receives the current
// configuration and returns the candidate. The candidate is validated against a freshly computed
// grant, dry-run against recent deliveries when dryRun is set, and written under the row lock only
// if the stored configuration is still the one change saw. It returns the saved configuration's view,
// and the configurations before and after.
func (r Rules) Mutate(ctx context.Context, p Principal, webhookID string, dryRun bool,
	change func(routing.Config) (routing.Config, error)) (RulesOut, Change, error) {
	id, err := requireID(webhookID)
	if err != nil {
		return RulesOut{}, Change{}, err
	}
	endpointID, err := r.resolve(ctx, p, id, true)
	if err != nil {
		return RulesOut{}, Change{}, err
	}
	// Resolve the grant's delivery targets BEFORE UpdateWebhookRouting takes its row lock. That
	// transaction holds a pooled connection until it commits, so resolving targets from inside mutate
	// needs a second one: N concurrent rule edits on an N-connection pool then each hold one and wait
	// forever on another, and ingest wedges with them. Targets read a moment before the lock are as
	// sound as targets read under it — the lock never covered webhook_routes or endpoint state — and
	// every delivery re-applies the grant anyway. The queue ceiling still comes from the locked row.
	pre, err := r.routing(ctx, id, endpointID)
	if err != nil {
		return RulesOut{}, Change{}, err
	}
	targets, err := r.Store.ResolveWebhookTargets(ctx, pre.WebhookID, pre.EndpointID)
	if err != nil {
		return RulesOut{}, Change{}, fmt.Errorf("manage: resolve webhook targets: %w", err)
	}
	scopes, err := r.Store.EndpointScopeQueues(ctx, targets) // also before the lock, for the same reason
	if err != nil {
		return RulesOut{}, Change{}, fmt.Errorf("manage: endpoint scope queues: %w", err)
	}

	// Build and check the candidate from the configuration read above, then dry-run it against the
	// webhook's recent traffic, all BEFORE the lock (SPEC-0026 REQ-3). The dry run reads up to
	// DryRunEvents stored events and evaluates each one in the sandbox. Doing that under the row lock
	// would hold it for as long as the evaluations take, and the event read would take a second
	// pooled connection, which is the deadlock above. Instead, the locked mutation below saves this
	// exact candidate only if the stored configuration is still the one it was built from. A
	// concurrent edit in between answers conflict rather than saving something that was never
	// dry-run.
	next, err := change(pre.Config)
	if err != nil {
		return RulesOut{}, Change{}, err
	}
	for i := range next.Rules {
		if next.Rules[i].ID == "" {
			next.Rules[i].ID = routing.NewRuleID()
		}
	}
	g := routing.Grant{TargetQueue: pre.TargetQueue, Queues: pre.WebhookQueues, Endpoints: targets, EndpointQueues: scopes,
		Tenant: pre.OwnerHumanID}
	// Param shapes are checked only when this save changes the params (SPEC-0026 REQ-3). Params saved
	// before shapes were checked are carried forward as they are, so they never stop the owner from
	// editing or removing the rules that read them. Such a rule faults, and fails closed, until the
	// params are rewritten.
	validate := routing.Validate
	if reflect.DeepEqual(next.Params, pre.Config.Params) {
		validate = routing.ValidateKeepingParams
	}
	if err := validate(next, g); err != nil {
		return RulesOut{}, Change{}, err
	}
	if dryRun {
		if err := r.dryRunSave(ctx, pre, next, g); err != nil {
			return RulesOut{}, Change{}, err
		}
	}

	wr, err := r.Store.UpdateWebhookRouting(ctx, id, endpointID, func(cur store.WebhookRouting) (routing.Config, error) {
		if !reflect.DeepEqual(cur.Config, pre.Config) {
			return routing.Config{}, fail(ErrConflict, "the webhook's rules changed while this edit was being checked; read them again and retry")
		}
		// The queue ceiling still comes from the locked row.
		g = routing.Grant{TargetQueue: cur.TargetQueue, Queues: cur.WebhookQueues, Endpoints: targets, EndpointQueues: scopes,
			Tenant: cur.OwnerHumanID}
		if err := validate(next, g); err != nil { // cur.Config is pre.Config, so the same check applies
			return routing.Config{}, err
		}
		return next, nil
	})
	if err != nil {
		return RulesOut{}, Change{}, notFoundAs(err, "webhook not found")
	}
	return Out(wr, g), Change{Previous: pre.Config, Saved: wr.Config}, nil
}

// Test routes without persisting anything. A stored event is only reachable through the webhook it
// arrived on (store.EventForWebhook), after webhook ownership has been established, so a dry run
// cannot read another tenant's deliveries.
func (r Rules) Test(ctx context.Context, p Principal, webhookID string, in TestIn) (TestOut, error) {
	id, err := requireID(webhookID)
	if err != nil {
		return TestOut{}, err
	}
	if (in.EventID > 0) == (in.Payload != nil) {
		return TestOut{}, fail(ErrInvalidArgument, "exactly one of event_id or payload is required")
	}
	endpointID, err := r.resolve(ctx, p, id, false)
	if err != nil {
		return TestOut{}, err
	}
	wr, err := r.routing(ctx, id, endpointID)
	if err != nil {
		return TestOut{}, err
	}
	g, err := r.grant(ctx, wr)
	if err != nil {
		return TestOut{}, err
	}
	cfg := wr.Config
	if in.Rules != nil {
		cfg = routing.Config{Default: FromActionIO(in.DefaultAction), Params: wr.Config.Params}
		for _, ru := range in.Rules {
			cfg.Rules = append(cfg.Rules, FromRuleIO(ru))
		}
	}
	if in.Params != nil {
		cfg.Params = in.Params
	}
	if in.Rules != nil || in.Params != nil {
		// Candidates are held to the same standard as a save, so a dry run that passes is a save
		// that will pass.
		if err := routing.Validate(cfg, g); err != nil {
			return TestOut{}, err
		}
	}

	env := routing.EnvelopeInput{Source: wr.SourceType, WebhookID: wr.WebhookID, TrustMode: wr.TrustMode}
	if in.EventID > 0 {
		ev, err := r.Store.EventForWebhook(ctx, in.EventID, wr.WebhookID)
		if err != nil {
			return TestOut{}, notFoundAs(err, "event not found on this webhook")
		}
		env = StoredEnvelope(wr, ev)
	} else {
		body, err := json.Marshal(in.Payload)
		if err != nil {
			return TestOut{}, fail(ErrInvalidArgument, "payload is not JSON-encodable")
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

	d := r.router().Route(ctx, cfg, g, env)
	out := TestOut{
		Decision: DecisionOut{Drop: d.Drop, Queue: d.Queue, Endpoints: d.Endpoints,
			Disposition: d.Disposition(), Faulted: d.Faulted, Unavailable: d.Unavailable, Fault: d.Fault},
		Trace: d.Trace,
	}
	if d.Unavailable {
		// A dry run that could not run says nothing about the rules. It is reported, but no
		// disposition is claimed for it.
		out.Decision.Disposition = ""
	}
	subject := routing.SubjectOf(wr.SourceType, env.Headers, env.Body)
	if d.Once && !d.Drop && !d.Faulted {
		out.OnceKey = routing.OnceKey(subject, d.Queue)
		out.Trace.OnceKey = out.OnceKey
	}
	if d.WorkOrder && !d.Drop && !d.Faulted {
		wo := routing.BuildWorkOrder(d, env, subject)
		out.WorkOrder = &wo
	}
	if !in.OmitEnvelope {
		out.Envelope = routing.Envelope(env)
	}
	return out, nil
}

// --- helpers ---

// resolve returns the endpoint whose endpoint-scoped reads and row-locked write serve this request.
// An endpoint principal is its own answer: the store then refuses any webhook that is not that
// endpoint's. A human principal is answered by the webhook's owning endpoint when it is in the
// human's reach; for a write that endpoint must also be active.
func (r Rules) resolve(ctx context.Context, p Principal, webhookID string, write bool) (string, error) {
	switch {
	case p.EndpointID != "" && p.HumanID == "":
		return p.EndpointID, nil
	case p.HumanID != "" && p.EndpointID == "" && r.Humans != nil:
		o, err := r.Humans.WebhookOwnerForHuman(ctx, webhookID, p.HumanID)
		if err != nil {
			return "", notFoundAs(err, "webhook not found")
		}
		if write && o.EndpointState != "active" {
			return "", &Error{Kind: ErrConflict, State: o.EndpointState,
				Msg: "the webhook's endpoint " + o.EndpointSlug + " is " + o.EndpointState + ", so its rules can no longer be changed"}
		}
		return o.EndpointID, nil
	default:
		// No principal, both, or a human with no way to resolve reach: nothing is in reach.
		return "", fail(ErrNotFound, "webhook not found")
	}
}

// routing reads the webhook's routing through its owning endpoint.
func (r Rules) routing(ctx context.Context, webhookID, endpointID string) (store.WebhookRouting, error) {
	wr, err := r.Store.WebhookRoutingForEndpoint(ctx, webhookID, endpointID)
	if err != nil {
		return store.WebhookRouting{}, notFoundAs(err, "webhook not found")
	}
	return wr, nil
}

// grant builds a webhook's grant from switchboard state: its target queue, its OWNER's allowed
// webhook queues (the owner-wide union the routing read computes — see store.WebhookRouting), and
// its live, authorized delivery targets with their scopes.
func (r Rules) grant(ctx context.Context, wr store.WebhookRouting) (routing.Grant, error) {
	targets, err := r.Store.ResolveWebhookTargets(ctx, wr.WebhookID, wr.EndpointID)
	if err != nil {
		return routing.Grant{}, fmt.Errorf("manage: resolve webhook targets: %w", err)
	}
	scopes, err := r.Store.EndpointScopeQueues(ctx, targets)
	if err != nil {
		return routing.Grant{}, fmt.Errorf("manage: endpoint scope queues: %w", err)
	}
	return routing.Grant{TargetQueue: wr.TargetQueue, Queues: wr.WebhookQueues, Endpoints: targets, EndpointQueues: scopes,
		Tenant: wr.OwnerHumanID}, nil
}

// router is the evaluator dry runs use: the same sandbox the receiver uses unless a test replaced it.
func (r Rules) router() routing.Router {
	if r.Router != nil {
		if rt := r.Router(); rt != nil {
			return rt
		}
	}
	return routing.Unavailable{}
}

// dryRunSave evaluates a candidate configuration against the webhook's most recent stored
// deliveries and refuses it with invalid_argument if any rule faults on any of them. The error names
// every faulting rule id, with the event ids and the cause. Most faults are type errors that real
// traffic exposes at once, so this turns "installs green, then faults every delivery" into a refused
// save that says why. A webhook with no stored events skips the dry run. So does a candidate with
// no rules, which cannot fault.
//
// The sandbox being unavailable is not the candidate's fault. It refuses the save with
// "unavailable", so the caller can retry, rather than blaming a rule.
// Governing: SPEC-0026 REQ-3 "Save-Time Fault Refusal and Param Typing"; ADR-0031.
func (r Rules) dryRunSave(ctx context.Context, wr store.WebhookRouting, cfg routing.Config, g routing.Grant) error {
	if len(cfg.Rules) == 0 {
		return nil
	}
	events, err := r.Store.RecentWebhookEvents(ctx, wr.WebhookID, DryRunEvents)
	if err != nil {
		return fmt.Errorf("manage: recent webhook events: %w", err)
	}
	router := r.router()
	faults := map[string][]int64{} // "rule_id (cause)" -> event ids, newest first
	var order []string
	for _, ev := range events {
		d := router.Route(ctx, cfg, g, StoredEnvelope(wr, ev))
		if d.Unavailable {
			return fail(ErrUnavailable, "routing is unavailable, so the rules could not be checked against recent deliveries; retry shortly")
		}
		if !d.Faulted {
			continue
		}
		k := d.Fault.RuleID + " (" + d.Fault.Cause + ")"
		if _, seen := faults[k]; !seen {
			order = append(order, k)
		}
		faults[k] = append(faults[k], ev.ID)
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
	return fail(ErrInvalidArgument, "refused: the rules fault on recent deliveries, which would record them and route them nowhere: "+
		strings.Join(parts, "; ")+". Fix the rules (a dry run with that event_id reproduces each one) and save again.")
}

// StoredEnvelope rebuilds the envelope input a stored delivery was routed on, exactly as the
// receiver saw it: the sanitized headers, the body, the kind and the verification result.
func StoredEnvelope(wr store.WebhookRouting, ev store.EventHistoryDetail) routing.EnvelopeInput {
	env := routing.EnvelopeInput{Source: wr.SourceType, WebhookID: wr.WebhookID, TrustMode: wr.TrustMode,
		Body: ev.Payload, Kind: ev.EventType, Verified: ev.Verified, ContentType: ev.ContentType}
	// The receiver stores headers as a flat string map (sanitizeHeaders); a row that does not decode
	// simply routes without headers, exactly as the delivery path does.
	_ = json.Unmarshal(ev.Headers, &env.Headers)
	return env
}

// notFoundAs turns the store's not-found into the ErrNotFound kind with msg, and wraps anything else
// with context for the caller to log.
func notFoundAs(err error, msg string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fail(ErrNotFound, msg)
	}
	var me *Error
	var ve *routing.ValidationError
	if errors.As(err, &me) || errors.As(err, &ve) {
		return err
	}
	return fmt.Errorf("manage: %w", err)
}

func requireID(webhookID string) (string, error) {
	id := strings.TrimSpace(webhookID)
	if id == "" {
		return "", fail(ErrInvalidArgument, "webhook_id is required")
	}
	return id, nil
}

// RuleIndex is the index of the rule with id in cfg, or -1.
func RuleIndex(cfg routing.Config, id string) int {
	id = strings.TrimSpace(id)
	return slices.IndexFunc(cfg.Rules, func(r routing.Rule) bool { return id != "" && r.ID == id })
}

// FromActionIO converts a wire action to the router's; nil stays nil.
func FromActionIO(a *ActionIO) *routing.Action {
	if a == nil {
		return nil
	}
	return &routing.Action{Queue: strings.TrimSpace(a.Queue), Drop: a.Drop, Endpoints: a.Endpoints,
		Exclusive: a.Exclusive, Once: a.Once, WorkOrder: a.WorkOrder}
}

// FromRuleIO converts a wire rule to the router's.
func FromRuleIO(r RuleIO) routing.Rule {
	return routing.Rule{ID: strings.TrimSpace(r.ID), Name: r.Name, Expr: r.Expr, Action: *FromActionIO(&r.Action)}
}

func toActionIO(a routing.Action) ActionIO {
	return ActionIO{Queue: a.Queue, Drop: a.Drop, Endpoints: a.Endpoints,
		Exclusive: a.Exclusive, Once: a.Once, WorkOrder: a.WorkOrder}
}

// Out renders a webhook's routing and grant in the shared wire shape.
func Out(wr store.WebhookRouting, g routing.Grant) RulesOut {
	out := RulesOut{
		WebhookID: wr.WebhookID, SourceType: wr.SourceType, TargetQueue: wr.TargetQueue,
		Rules:  make([]RuleIO, 0, len(wr.Config.Rules)),
		Params: wr.Config.Params,
		Grant:  GrantOut{Queues: []string{g.TargetQueue}, Endpoints: nonNil(g.Endpoints)},
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
		out.Rules = append(out.Rules, RuleIO{ID: r.ID, Name: r.Name, Expr: r.Expr, Action: toActionIO(r.Action)})
	}
	return out
}

// WorkOrderDelta compares the rules carrying work_order before and after a save: the ids whose flag
// was added, removed, or kept. SPEC-0035 REQ "Work Orders on the Human API" logs it on every save
// that touches a work-order rule, since a work order is switchboard vouching for a delivery.
func WorkOrderDelta(prev, next routing.Config) (added, removed, kept []string) {
	carries := func(cfg routing.Config) map[string]bool {
		m := map[string]bool{}
		for _, r := range cfg.Rules {
			if r.Action.WorkOrder {
				m[r.ID] = true
			}
		}
		if cfg.Default != nil && cfg.Default.WorkOrder {
			m["(default)"] = true
		}
		return m
	}
	before, after := carries(prev), carries(next)
	for id := range after {
		if before[id] {
			kept = append(kept, id)
		} else {
			added = append(added, id)
		}
	}
	for id := range before {
		if !after[id] {
			removed = append(removed, id)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(kept)
	return added, removed, kept
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
