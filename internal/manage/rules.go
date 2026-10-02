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
// @joestump-agent 10/02/2026 - Added the forbidden kind, Error.Reason and Principal.OwnerHumanID,
// which route management (routes.go) shares with this file (#555).
//
// @joestump-agent 09/29/2026 - Extracted from internal/mcp/webhook_rules.go (mutateRules,
// dryRunSave, routingGrant, the dry-run body, and the I/O shapes) and parameterized by a Principal,
// so the human API's rule routes run the MCP verbs' code rather than a copy of it.
//
// @joestump-agent 09/25/2026 - The save-time dry-run skips deliveries the trust gate would hold
// (SPEC-0026 REQ-5): live traffic never runs rules on them, so an outsider's payload could otherwise
// refuse the owner's saves. It pages back past them to find 50 that pass (#385 review).
//
// @joestump-agent 09/25/2026 - Test reports a held delivery's decision and trace as the receiver
// records them (the trust gate's), with the rules' outcome moved to rules_would.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// ErrForbidden refuses a route target. Every reason (unknown, revoked, unfriended) is this one
	// kind and one message, so the refusal enumerates nothing. Governing: SPEC-0006 REQ "Webhook Route
	// Fan-Out Under Ownership and Friendship".
	ErrForbidden Kind = "forbidden"
	// ErrUnavailable says the rule evaluator could not run, so a save could not be checked. It is
	// transient and names no rule. Governing: SPEC-0026 REQ-3.
	ErrUnavailable Kind = "unavailable"
)

// Error is a domain failure: a Kind plus a message that is safe to show the caller (it never carries
// secret material or internal error text). State is set when a write was refused because the
// webhook's owning endpoint is not active. Reason, when set, says which of several refusals that
// share one message this was; it is for the operator log only and never reaches the caller.
type Error struct {
	Kind   Kind
	Msg    string
	State  string
	Reason string
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
	WebhookEventsBefore(ctx context.Context, webhookID string, beforeAt time.Time, beforeID int64, limit int) ([]store.EventHistoryDetail, error)
}

// HumanReach resolves a webhook's owning endpoint within a human's reach.
type HumanReach interface {
	WebhookOwnerForHuman(ctx context.Context, webhookID, ownerHumanID string) (store.WebhookOwner, error)
}

// Principal is who is asking. Exactly one of EndpointID and HumanID is set; anything else resolves
// nothing.
type Principal struct {
	// EndpointID is an MCP caller: the webhook must be this endpoint's own.
	EndpointID string
	// HumanID is a human API caller: the webhook's owning endpoint must belong to this human.
	HumanID string
	// OwnerHumanID is an endpoint principal's own human (agents.owner_human_id). Route management
	// authorizes a target, and records the grant, on that human's authority (ADR-0022, ADR-0008);
	// rule management ignores it. It never resolves anything on its own.
	OwnerHumanID string
}

// EndpointPrincipal is the MCP caller.
func EndpointPrincipal(endpointID string) Principal { return Principal{EndpointID: endpointID} }

// EndpointPrincipalOf is the MCP caller together with its owning human, as route management needs it.
func EndpointPrincipalOf(endpointID, ownerHumanID string) Principal {
	return Principal{EndpointID: endpointID, OwnerHumanID: ownerHumanID}
}

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

// DryRunEvents bounds the save-time dry run: at most this many of the webhook's latest deliveries
// that the trust gate would pass, each inside the per-event evaluation budget (SPEC-0026 REQ-3,
// "Rate Limiting"). DryRunScanPages bounds how far back it reads to find them, a page of
// DryRunEvents at a time, so an outsider's flood of held deliveries costs at most that many cheap
// Go trust checks and never a sandbox evaluation. The bound means a flood of more than
// DryRunScanPages*DryRunEvents held deliveries since the owner's last trusted one leaves fewer than
// DryRunEvents to check, possibly none. The save still goes through, because refusing it would let
// an outsider block the owner's edits. Live traffic still fails closed on a fault. The result's
// dry_run.warning says so, so the owner can run a dry run against a known event before relying on
// the rules.
const (
	DryRunEvents    = 50
	DryRunScanPages = 10
)

// trustModeSigned is the trust mode of an HMAC-verified webhook: a sample payload is routed as a
// delivery that passed verification.
const trustModeSigned = "signed"

// --- I/O shapes, shared by both surfaces so their fields cannot drift ---

// ActionIO is where a matching delivery goes.
type ActionIO struct {
	Queue      string   `json:"queue,omitempty" jsonschema:"deliver to this queue (the webhook's target queue or one in its owner's allowed webhook queues)"`
	Drop       bool     `json:"drop,omitempty" jsonschema:"true to record the delivery without creating a todo or ringing a doorbell"`
	Quarantine bool     `json:"quarantine,omitempty" jsonschema:"true to hold the delivery on the owner's quarantine queue for a human or classifier to release or discard (no agent is handed it)"`
	Endpoints  []string `json:"endpoints,omitempty" jsonschema:"with queue only: deliver to just these of the webhook's delivery targets (see grant.endpoints); omitted means every target"`
	Exclusive  bool     `json:"exclusive,omitempty" jsonschema:"with queue only: deliver to exactly one target, the first (owner first, then routes by grant time) whose endpoint scope includes the queue"`
	Once       bool     `json:"once,omitempty" jsonschema:"with queue only: at most one work order per subject (issue or cairn artifact) per queue; later deliveries about it are recorded but mint nothing"`
	WorkOrder  bool     `json:"work_order,omitempty" jsonschema:"with queue only: attach a switchboard-authored work order (lane, verified provenance, authorizing rule, subject) to each todo"`
}

// RuleIO is one ordered rule.
type RuleIO struct {
	ID     string   `json:"id,omitempty" jsonschema:"stable rule id; minted when omitted on create"`
	Name   string   `json:"name,omitempty" jsonschema:"label recorded on the routing trace"`
	Expr   string   `json:"expr" jsonschema:"jq filter over the routing envelope; its first output decides (anything but false or null matches)"`
	Action ActionIO `json:"action" jsonschema:"where a matching delivery goes: exactly one of queue, drop or quarantine"`
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
	Params        map[string]any `json:"params" jsonschema:"owner-set values rules read as $params (e.g. trusted-actor allowlists); {} when none are set"`
	Grant         GrantOut       `json:"grant" jsonschema:"what actions may reach right now"`
	DryRun        *DryRunOut     `json:"dry_run,omitempty" jsonschema:"what the save-time dry-run checked; omitted when no dry-run ran"`
}

// DryRunOut reports a save-time dry-run's coverage, so an owner can tell when a flood of held
// deliveries left fewer than DryRunEvents to check (SPEC-0026 REQ-3).
type DryRunOut struct {
	Checked     int    `json:"checked" jsonschema:"deliveries the trust gate passes that the new rules were evaluated against"`
	SkippedHeld int    `json:"skipped_held" jsonschema:"deliveries skipped because the trust gate would hold them"`
	Warning     string `json:"warning,omitempty" jsonschema:"set when the scan bound stopped the dry-run before it found 50 deliveries to check"`
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
	Drop        bool     `json:"drop" jsonschema:"true when a drop action would record the delivery without a todo"`
	Queue       string   `json:"queue,omitempty" jsonschema:"the queue todos would land in"`
	Endpoints   []string `json:"endpoints,omitempty" jsonschema:"the endpoints todos would be created on"`
	Disposition string   `json:"disposition,omitempty" jsonschema:"the intake outcome: routed, dropped, quarantined or faulted"`
	Faulted     bool     `json:"faulted" jsonschema:"BLOCKING: a rule faulted, so evaluation stopped and the delivery would be held on the owner's quarantine queue as rule_fault instead of routed; a save whose rules fault on recent deliveries is refused"`
	Quarantine  bool     `json:"quarantine,omitempty" jsonschema:"a rule's {quarantine: true} matched: the delivery would be held on the owner's quarantine queue"`
	// Governing: SPEC-0026 REQ-6 (quarantine triggers).
	QuarantineReason string             `json:"quarantine_reason,omitempty" jsonschema:"why the delivery would be held on the owner's quarantine queue: untrusted_actor, rule_fault or rule_action; empty when it would not be held"`
	Unavailable      bool               `json:"unavailable,omitempty" jsonschema:"the rule evaluator could not run, so this dry-run says nothing about the rules; retry"`
	Fault            *routing.RuleFault `json:"fault,omitempty" jsonschema:"the fault that stopped evaluation: rule id, index, cause and detail"`
}

// TestOut is a dry run's result.
type TestOut struct {
	Decision  DecisionOut        `json:"decision" jsonschema:"where the delivery would go"`
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
	RulesWould *RulesWouldOut `json:"rules_would,omitempty" jsonschema:"only when held: the decision and trace the rules would produce if the actor were trusted; live traffic never evaluates them for this delivery"`
}

// RulesWouldOut is what the rules would do with a held delivery, had the trust gate passed it.
type RulesWouldOut struct {
	Decision DecisionOut   `json:"decision" jsonschema:"where the delivery would go if its actor were trusted"`
	Trace    routing.Trace `json:"trace" jsonschema:"the routing trace the rules would produce"`
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
	var report *DryRunOut
	if dryRun {
		if report, err = r.dryRunSave(ctx, pre, next, g); err != nil {
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
	out := Out(wr, g)
	out.DryRun = report
	return out, Change{Previous: pre.Config, Saved: wr.Config}, nil
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
		env.Actor = dryRunActor(wr, env.Verified, body)
	}

	d := r.router().Route(ctx, cfg, g, env)
	out := TestOut{
		Decision: DecisionOut{Drop: d.Drop, Queue: d.Queue, Endpoints: d.Endpoints,
			Disposition: d.Disposition(), Faulted: d.Faulted, Quarantine: d.Quarantine, QuarantineReason: d.QuarantineReason(),
			Unavailable: d.Unavailable, Fault: d.Fault},
		Trace: d.Trace,
		Actor: env.Actor, Held: env.Actor != nil && !env.Actor.IsTrusted(),
	}
	if d.Unavailable {
		// A dry run that could not run says nothing about the rules. It is reported, but no
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
		out.RulesWould = &RulesWouldOut{Decision: out.Decision, Trace: out.Trace}
		out.Decision = DecisionOut{Disposition: held.Disposition(), QuarantineReason: held.QuarantineReason(),
			Faulted: held.Faulted, Fault: held.Fault}
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
// deliveries and refuses it with invalid_argument if any rule faults on any of them. It pages back
// until it finds DryRunEvents that the trust gate would pass (live traffic only evaluates those).
//
// Only deliveries the trust gate would pass TODAY are checked, because only those ever reach the
// rules on live traffic (the receiver holds the rest before any rule runs). Checking a held one
// would let anyone who can make the producer send (on a public repository, anyone who can open an
// issue) refuse the owner's saves with a payload built to fault, and a flood of them would push the
// owner's real traffic out of the window. Held deliveries are skipped and the scan pages further
// back, up to DryRunScanPages, to find DryRunEvents that pass.
//
// On success it reports what it checked: nil when nothing ran (no rules, or no stored events), and
// a warning when the scan bound stopped it short of DryRunEvents while older deliveries remained.
// Governing: SPEC-0026 REQ-3 "Save-Time Fault Refusal and Param Typing", REQ-5; ADR-0031.
func (r Rules) dryRunSave(ctx context.Context, wr store.WebhookRouting, cfg routing.Config, g routing.Grant) (*DryRunOut, error) {
	if len(cfg.Rules) == 0 {
		return nil, nil
	}
	router := r.router()
	faults := map[string][]int64{}
	var order []string
	checked, skipped := 0, 0
	bounded := true
	var cursor store.EventHistoryDetail
scan:
	for page := 0; page < DryRunScanPages; page++ {
		events, err := r.Store.WebhookEventsBefore(ctx, wr.WebhookID, cursor.ReceivedAt, cursor.ID, DryRunEvents)
		if err != nil {
			return nil, fmt.Errorf("manage: webhook events before: %w", err)
		}
		for _, ev := range events {
			env := StoredEnvelope(wr, ev)
			if env.Actor != nil && !env.Actor.IsTrusted() {
				skipped++
				continue
			}
			d := router.Route(ctx, cfg, g, env)
			if d.Unavailable {
				return nil, fail(ErrUnavailable, "routing is unavailable, so the rules could not be checked against recent deliveries; retry shortly")
			}
			if d.Faulted {
				k := d.Fault.RuleID + " (" + d.Fault.Cause + ")"
				if _, seen := faults[k]; !seen {
					order = append(order, k)
				}
				faults[k] = append(faults[k], ev.ID)
			}
			if checked++; checked == DryRunEvents {
				bounded = false
				break scan
			}
		}
		if len(events) < DryRunEvents {
			bounded = false
			break
		}
		cursor = events[len(events)-1]
	}
	dryRun := &DryRunOut{Checked: checked, SkippedHeld: skipped}
	if len(order) == 0 {
		if checked == 0 && skipped == 0 {
			return nil, nil
		}
		if bounded {
			dryRun.Warning = fmt.Sprintf("the dry-run read the newest %d deliveries and the trust gate would hold %d of them, so the rules were checked against only %d of the %d it aims for; older trusted deliveries were not checked. Run test_webhook_rules with a known event_id before relying on these rules.", DryRunScanPages*DryRunEvents, skipped, checked, DryRunEvents)
		}
		return dryRun, nil
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		ids := make([]string, 0, len(faults[k]))
		for _, id := range faults[k] {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		parts = append(parts, "rule "+k+" on events "+strings.Join(ids, ", "))
	}
	return nil, fail(ErrInvalidArgument, "refused: the rules fault on recent deliveries, which would record them and route them nowhere: "+
		strings.Join(parts, "; ")+". Fix the rules (test_webhook_rules with event_id reproduces each one) and save again.")
}

// StoredEnvelope rebuilds the envelope input a stored delivery was routed on, exactly as the
// receiver saw it: the sanitized headers, the body, the kind and the verification result.
func StoredEnvelope(wr store.WebhookRouting, ev store.EventHistoryDetail) routing.EnvelopeInput {
	env := routing.EnvelopeInput{Source: wr.SourceType, WebhookID: wr.WebhookID, TrustMode: wr.TrustMode,
		Body: ev.Payload, Kind: ev.EventType, Verified: ev.Verified, ContentType: ev.ContentType,
		Actor: dryRunActor(wr, ev.Verified, ev.Payload)}
	// The receiver stores headers as a flat string map (sanitizeHeaders); a row that does not decode
	// simply routes without headers, exactly as the delivery path does.
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
	return &routing.Action{Queue: strings.TrimSpace(a.Queue), Drop: a.Drop, Quarantine: a.Quarantine, Endpoints: a.Endpoints,
		Exclusive: a.Exclusive, Once: a.Once, WorkOrder: a.WorkOrder}
}

// FromRuleIO converts a wire rule to the router's.
func FromRuleIO(r RuleIO) routing.Rule {
	return routing.Rule{ID: strings.TrimSpace(r.ID), Name: r.Name, Expr: r.Expr, Action: *FromActionIO(&r.Action)}
}

func toActionIO(a routing.Action) ActionIO {
	return ActionIO{Queue: a.Queue, Drop: a.Drop, Quarantine: a.Quarantine, Endpoints: a.Endpoints,
		Exclusive: a.Exclusive, Once: a.Once, WorkOrder: a.WorkOrder}
}

// Out renders a webhook's routing and grant in the shared wire shape.
func Out(wr store.WebhookRouting, g routing.Grant) RulesOut {
	out := RulesOut{
		WebhookID: wr.WebhookID, SourceType: wr.SourceType, TargetQueue: wr.TargetQueue,
		Rules:  make([]RuleIO, 0, len(wr.Config.Rules)),
		Params: wr.Config.Params,
		Grant:  GrantOut{Queues: []string{g.TargetQueue}, Endpoints: nonNil(g.Endpoints)},
		DryRun: nil,
	}
	for _, q := range g.Queues {
		if !slices.Contains(out.Grant.Queues, q) {
			out.Grant.Queues = append(out.Grant.Queues, q)
		}
	}
	if out.Params == nil {
		out.Params = map[string]any{} // echo {} rather than omit, so a caller sees the params it left in force
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
