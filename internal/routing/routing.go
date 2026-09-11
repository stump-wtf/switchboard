// Package routing is the deterministic event-routing stage between "delivery verified and
// normalized" and "todo created": an ordered, first-match-wins list of jq rules per webhook, each
// naming an action of `queue` (optionally narrowed to a subset of the webhook's fan-out endpoints)
// or `drop`, terminated by an explicit default. It is a leaf package — pure functions over values,
// no store, no HTTP — so the ingest receiver, the MCP rule verbs, and the dry-run tool all decide
// routes with exactly the same code.
//
// Expressions run in gojq with the host shut out: no environment (gojq's env is empty unless a
// loader is supplied, and `env`/`$ENV` are refused outright anyway), no `input`/`inputs`, no module
// imports, and no functions whose result depends on the clock or that write to stderr or halt the
// host. A per-rule timeout and a per-event budget bound CPU; a rule that errors or times out is
// treated as no-match and recorded on the trace rather than failing the delivery.
//
// Tenant safety is enforced twice. Validate refuses, at save time, any action whose queue is
// outside the owning endpoint's webhook-queue ceiling or whose endpoints are not already live,
// authorized fan-out targets of the webhook. Evaluate re-applies the same Grant at delivery time, so
// a ceiling that shrank or a route that was revoked after the rule was saved cannot be reached:
// a rule can only ever NARROW where a delivery lands, never widen it.
//
// Governing: ADR-0024 (deterministic jq rules; LLM triage phased as a follow-up), SPEC-0020 REQ
// "Deterministic Rule Evaluation", REQ "Rule Validation at Save Time", REQ "Drop Action Semantics",
// REQ "Routing Trace", REQ "Isolation and Tenant Safety"; ADR-0022 (endpoint-scoped todos).
//
// @joestump-agent 09/11/2026 - Initial deterministic stage: rules, validation, sandbox, trace.
package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/itchyny/gojq"
)

// Limits. They bound what one webhook's configuration can cost per delivery: at most MaxRules
// compiles and evaluations, each capped at RuleTimeout, all inside EventBudget. The budget is what
// keeps a pathological rule list from holding a delivery open for MaxRules × RuleTimeout.
const (
	MaxRules          = 32
	MaxExprBytes      = 4096
	MaxNameBytes      = 128
	MaxActionEndpoint = 16
	RuleTimeout       = 50 * time.Millisecond
	EventBudget       = 250 * time.Millisecond
)

// Validation error codes, stable for the MCP layer to surface verbatim.
const (
	CodeInvalidRule       = "invalid_rule"
	CodeInvalidExpression = "invalid_expression"
	CodeForbiddenFunction = "forbidden_function"
	CodeNotGranted        = "not_granted"
	CodeTooManyRules      = "too_many_rules"
	CodeInvalidParams     = "invalid_params"

	// MaxParamsBytes bounds a webhook's encoded params (allowlists and other owner-set values).
	MaxParamsBytes = 16 << 10
)

// paramsVariable is the one variable rule expressions can see: the webhook owner's params, bound as
// $params. It is how an allowlist lives next to the rules without being spliced into every expression
// — and, because only the owner can set it, how a rule's trust decision stays out of the payload's
// reach. Governing: ADR-0025.
var paramsVariable = []string{"$params"}

// Trace stages and causes (SPEC-0020 REQ "Routing Trace").
const (
	StageRule    = "rule"
	StageDefault = "default"

	CauseNoMatch            = "no_match_default"
	CauseRuleNotGranted     = "rule_not_granted"
	CauseDefaultNotGranted  = "default_not_granted"
	FaultTimeout            = "timeout"
	FaultError              = "error"
	FaultCompile            = "compile_error"
	FaultBudgetExhausted    = "budget_exhausted"
	maxFaultDetailBytes     = 200
	ruleIDPrefix            = "rule_"
	defaultIndexForMessages = -1
	paramsIndexForMessages  = -2
)

// Action is where a matched delivery goes. Exactly one of Queue or Drop is set. Endpoints, valid
// only with Queue, narrows the delivery to that subset of the webhook's fan-out targets (its owning
// endpoint plus explicit webhook routes); empty means every target, which is today's behavior.
//
// The three work-order flags are also queue-only (ADR-0025):
//   - Exclusive delivers to exactly ONE target — the first, in delivery order, whose endpoint scope
//     grants Queue — instead of fanning out, so a lane served by one pool endpoint gets one todo.
//   - Once makes the delivery a work order at most once per (subject, queue): a second delivery about
//     the same issue or artifact routed to the same queue records its event but mints no todo.
//   - WorkOrder attaches a switchboard-authored work order (subject, provenance, authorizing rule) to
//     each todo it mints.
type Action struct {
	Queue     string   `json:"queue,omitempty"`
	Drop      bool     `json:"drop,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
	Exclusive bool     `json:"exclusive,omitempty"`
	Once      bool     `json:"once,omitempty"`
	WorkOrder bool     `json:"work_order,omitempty"`
}

// Rule is one ordered routing rule. Expr is a jq filter whose FIRST output decides the match with
// jq truthiness (anything but false and null matches; no output does not).
type Rule struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Expr   string `json:"expr"`
	Action Action `json:"action"`
}

// Config is a webhook's routing configuration. A nil Default means "the webhook's target queue",
// which is also what an empty rule list yields — so a webhook nobody configured routes exactly as it
// did before routing existed.
type Config struct {
	Rules   []Rule  `json:"rules"`
	Default *Action `json:"default_action,omitempty"`
	// Params is bound as $params in every rule expression. Owner-set, never payload-derived.
	Params map[string]any `json:"params,omitempty"`
}

// Grant is what a webhook's rules may reach, computed by the caller from switchboard state — never
// from the payload. TargetQueue is the webhook's own queue (always routable: routing to it grants
// nothing the webhook did not already have). Queues is the owning endpoint's webhook-queue ceiling.
// Endpoints is the live, authorized fan-out set in delivery order, owner first. EndpointQueues is
// each of those endpoints' scope queues — what exclusive delivery selects on.
type Grant struct {
	TargetQueue    string
	Queues         []string
	Endpoints      []string
	EndpointQueues map[string][]string
}

// ValidationError names the offending rule (Index -1 is the default action) so a save failure is
// actionable rather than a bare "invalid".
type ValidationError struct {
	Index  int
	RuleID string
	Name   string
	Code   string
	Msg    string
}

func (e *ValidationError) Error() string {
	if e.Index == defaultIndexForMessages {
		return "default_action: " + e.Msg
	}
	if e.Index == paramsIndexForMessages {
		return "params: " + e.Msg
	}
	label := fmt.Sprintf("rule %d", e.Index)
	if e.RuleID != "" {
		label += " (" + e.RuleID + ")"
	}
	if e.Name != "" {
		label += " " + fmt.Sprintf("%q", e.Name)
	}
	return label + ": " + e.Msg
}

// ruleIDPattern keeps ids URL- and log-safe; minted ids are rule_<24 hex>.
var ruleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewRuleID mints a stable, opaque rule id.
func NewRuleID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a time-derived id is still unique
		// enough for a list capped at MaxRules.
		return ruleIDPrefix + fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return ruleIDPrefix + hex.EncodeToString(b)
}

// forbiddenFuncs are refused at compile time. Each is either host access (env, input, stderr),
// host control (halt), or non-determinism (now, local time zone) — the three things SPEC-0020 says a
// routing expression must not have. Variables need no entry: the only one gojq is given is $params
// (paramsVariable), so $__loc__, $HOME and friends fail to compile on their own.
var forbiddenFuncs = map[string]string{
	"env":            "environment access is not available to routing rules",
	"$ENV":           "environment access is not available to routing rules",
	"input":          "rules see only the event; input/inputs are not available",
	"inputs":         "rules see only the event; input/inputs are not available",
	"input_filename": "rules see only the event; input_filename is not available",
	"debug":          "debug writes to the host and is not available",
	"stderr":         "stderr writes to the host and is not available",
	"halt":           "halt stops the host and is not available",
	"halt_error":     "halt_error stops the host and is not available",
	"now":            "now is non-deterministic; route on timestamps carried by the event instead",
	"localtime":      "localtime depends on the host time zone; use gmtime",
	"strflocaltime":  "strflocaltime depends on the host time zone; use strftime",
}

// Compile parses and compiles a rule expression inside the sandbox. It is the single compatibility
// contract: if Compile accepts it, it routes.
func Compile(expr string) (*gojq.Code, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, &ValidationError{Code: CodeInvalidExpression, Msg: "expr is required"}
	}
	if len(expr) > MaxExprBytes {
		return nil, &ValidationError{Code: CodeInvalidExpression,
			Msg: fmt.Sprintf("expr is %d bytes; the limit is %d", len(expr), MaxExprBytes)}
	}
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, &ValidationError{Code: CodeInvalidExpression, Msg: "expr does not parse: " + err.Error()}
	}
	if len(q.Imports) > 0 {
		return nil, &ValidationError{Code: CodeForbiddenFunction, Msg: "import/include is not available to routing rules"}
	}
	if name, why, found := findForbidden(reflect.ValueOf(q)); found {
		return nil, &ValidationError{Code: CodeForbiddenFunction, Msg: name + ": " + why}
	}
	// No WithEnvironLoader (env stays empty), no WithInputIter (input is refused), no module loader
	// (import fails), no custom functions, and one variable: the owner's $params. This is the whole
	// sandbox surface gojq exposes.
	code, err := gojq.Compile(q, gojq.WithVariables(paramsVariable))
	if err != nil {
		return nil, &ValidationError{Code: CodeInvalidExpression, Msg: "expr does not compile: " + err.Error()}
	}
	return code, nil
}

// findForbidden walks the parsed AST looking for a call to a forbidden function or variable. The
// walk is reflective so it follows every node type gojq has (and any it adds) without a hand-kept
// visitor that could silently miss one.
func findForbidden(v reflect.Value) (string, string, bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "", "", false
		}
		return findForbidden(v.Elem())
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(gojq.Func{}) {
			name := v.FieldByName("Name").String()
			if why, bad := forbiddenFuncs[name]; bad {
				return name, why, true
			}
		}
		for i := 0; i < v.NumField(); i++ {
			if name, why, found := findForbidden(v.Field(i)); found {
				return name, why, true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if name, why, found := findForbidden(v.Index(i)); found {
				return name, why, true
			}
		}
	}
	return "", "", false
}

// Validate checks a whole configuration against a grant: shape, count, compile, and reachability.
// It returns the first *ValidationError; nil means the configuration is safe to store.
func Validate(cfg Config, g Grant) error {
	if len(cfg.Params) > 0 {
		raw, err := json.Marshal(cfg.Params)
		if err != nil {
			return &ValidationError{Index: paramsIndexForMessages, Code: CodeInvalidParams, Msg: "params are not JSON-encodable"}
		}
		if len(raw) > MaxParamsBytes {
			return &ValidationError{Index: paramsIndexForMessages, Code: CodeInvalidParams,
				Msg: fmt.Sprintf("params encode to %d bytes; the limit is %d", len(raw), MaxParamsBytes)}
		}
	}
	if len(cfg.Rules) > MaxRules {
		return &ValidationError{Index: len(cfg.Rules) - 1, Code: CodeTooManyRules,
			Msg: fmt.Sprintf("%d rules; the limit is %d", len(cfg.Rules), MaxRules)}
	}
	seen := make(map[string]bool, len(cfg.Rules))
	for i, r := range cfg.Rules {
		fail := func(code, msg string) error {
			return &ValidationError{Index: i, RuleID: r.ID, Name: r.Name, Code: code, Msg: msg}
		}
		if !ruleIDPattern.MatchString(r.ID) {
			return fail(CodeInvalidRule, "id must be 1-64 characters of [A-Za-z0-9_-]")
		}
		if seen[r.ID] {
			return fail(CodeInvalidRule, "duplicate rule id")
		}
		seen[r.ID] = true
		if len(r.Name) > MaxNameBytes {
			return fail(CodeInvalidRule, fmt.Sprintf("name is longer than %d bytes", MaxNameBytes))
		}
		if _, err := Compile(r.Expr); err != nil {
			var ve *ValidationError
			if errors.As(err, &ve) {
				return fail(ve.Code, ve.Msg)
			}
			return fail(CodeInvalidExpression, err.Error())
		}
		if code, msg := checkAction(r.Action, g); code != "" {
			return fail(code, msg)
		}
	}
	if cfg.Default != nil {
		if code, msg := checkAction(*cfg.Default, g); code != "" {
			return &ValidationError{Index: defaultIndexForMessages, Code: code, Msg: msg}
		}
	}
	return nil
}

// checkAction validates one action's shape and reachability, returning ("", "") when it is fine.
func checkAction(a Action, g Grant) (string, string) {
	switch {
	case a.Drop && a.Queue != "":
		return CodeInvalidRule, "action must set exactly one of queue or drop"
	case !a.Drop && strings.TrimSpace(a.Queue) == "":
		return CodeInvalidRule, "action must set exactly one of queue or drop"
	case a.Drop && len(a.Endpoints) > 0:
		return CodeInvalidRule, "endpoints is only valid with a queue action"
	case a.Drop && (a.Exclusive || a.Once || a.WorkOrder):
		return CodeInvalidRule, "exclusive, once and work_order are only valid with a queue action"
	case len(a.Endpoints) > MaxActionEndpoint:
		return CodeInvalidRule, fmt.Sprintf("at most %d endpoints per action", MaxActionEndpoint)
	}
	if a.Drop {
		return "", ""
	}
	if !queueGranted(a.Queue, g) {
		return CodeNotGranted, "queue " + a.Queue + " is not in the webhook owner's allowed webhook queues"
	}
	seen := make(map[string]bool, len(a.Endpoints))
	for _, ep := range a.Endpoints {
		if seen[ep] {
			return CodeInvalidRule, "duplicate endpoint in action"
		}
		seen[ep] = true
		// One message for every miss, so a rule save cannot be used to learn which endpoint ids
		// exist elsewhere: only this webhook's own authorized targets are ever acceptable.
		if !slices.Contains(g.Endpoints, ep) {
			return CodeNotGranted, "endpoint " + ep + " is not a delivery target of this webhook; add it with add_webhook_route first"
		}
	}
	if a.Exclusive {
		if _, ok := exclusiveTarget(candidates(a, g), a.Queue, g); !ok {
			return CodeNotGranted, "no delivery target is scoped to queue " + a.Queue +
				"; exclusive delivery needs one (route an endpoint whose scope includes it)"
		}
	}
	return "", ""
}

// candidates is the action's target set before exclusivity: its endpoints narrowed to the grant, in
// grant (owner-first, then route) order, or every target when it names none.
func candidates(a Action, g Grant) []string {
	if len(a.Endpoints) == 0 {
		return slices.Clone(g.Endpoints)
	}
	var eps []string
	for _, ep := range g.Endpoints {
		if slices.Contains(a.Endpoints, ep) {
			eps = append(eps, ep)
		}
	}
	return eps
}

// exclusiveTarget picks the one endpoint an exclusive delivery lands on: the first candidate whose
// scope grants the queue. Order is the grant's, which the store makes deterministic (owner first,
// then routes by grant time), so the same delivery always picks the same executing identity.
func exclusiveTarget(eps []string, queue string, g Grant) (string, bool) {
	for _, ep := range eps {
		if slices.Contains(g.EndpointQueues[ep], queue) {
			return ep, true
		}
	}
	return "", false
}

func queueGranted(q string, g Grant) bool {
	return q != "" && (q == g.TargetQueue || slices.Contains(g.Queues, q))
}

// Decision is the evaluated route for one delivery.
type Decision struct {
	Drop      bool
	Queue     string
	Endpoints []string // the endpoints to mint todos on, a subset of Grant.Endpoints in its order
	Once      bool     // the action asked for at-most-once per (subject, queue)
	WorkOrder bool     // the action asked for a work order on each todo
	Trace     Trace
}

// Trace records how a delivery was routed. It is persisted on the event and on every todo the
// delivery produced, so a todo can always explain why it exists.
type Trace struct {
	Stage     string      `json:"stage"`
	Cause     string      `json:"cause,omitempty"`
	RuleIndex *int        `json:"rule_index,omitempty"`
	RuleID    string      `json:"rule_id,omitempty"`
	RuleName  string      `json:"rule_name,omitempty"`
	Action    Action      `json:"action"`
	Faults    []RuleFault `json:"faults,omitempty"`
	// OnceKey is the (subject, queue) key a Once action claimed, set by the receiver; "once":"repeat"
	// is merged into the stored event trace when the key had already been claimed.
	OnceKey string `json:"once_key,omitempty"`
}

// RuleFault records a rule that could not be evaluated and was therefore treated as no-match.
type RuleFault struct {
	RuleIndex int    `json:"rule_index"`
	RuleID    string `json:"rule_id,omitempty"`
	Cause     string `json:"cause"`
	Detail    string `json:"detail,omitempty"`
}

// MatchResult is what the jq half of routing reports: the index of the first matching rule, if
// any, and every rule that faulted on the way. It deliberately carries no action — the action is
// looked up by Decide from the caller's own configuration, so whatever evaluates the expressions
// (in particular the sandbox child process) can say WHICH rule matched but never WHERE it goes.
type MatchResult struct {
	RuleIndex *int        `json:"rule_index,omitempty"`
	Faults    []RuleFault `json:"faults,omitempty"`
}

// Evaluate matches and decides in-process. Production delivery routes through a Router (see
// sandbox.go) so jq runs out of process; Evaluate is the same logic for tests and for callers that
// have already established isolation.
func Evaluate(ctx context.Context, cfg Config, g Grant, event map[string]any) Decision {
	return Decide(cfg, g, Match(ctx, cfg.Rules, cfg.Params, event))
}

// Match runs the rules, in order, against one normalized event (see Envelope) with params bound as
// $params, and stops at the first match. It never fails: every fault degrades to no-match and is
// recorded.
func Match(ctx context.Context, rules []Rule, params map[string]any, event map[string]any) MatchResult {
	budget, cancel := context.WithTimeout(ctx, EventBudget)
	defer cancel()

	vars := paramsValue(params)
	var res MatchResult
	for i, r := range rules {
		if budget.Err() != nil {
			res.Faults = append(res.Faults, RuleFault{RuleIndex: i, RuleID: r.ID, Cause: FaultBudgetExhausted})
			break
		}
		code, err := Compile(r.Expr)
		if err != nil {
			// A stored rule that no longer compiles (e.g. the sandbox tightened after it was saved)
			// must not route anything, and must say so.
			res.Faults = append(res.Faults, RuleFault{RuleIndex: i, RuleID: r.ID, Cause: FaultCompile, Detail: clip(err.Error())})
			continue
		}
		matched, cause, detail := run(budget, code, event, vars)
		if cause != "" {
			res.Faults = append(res.Faults, RuleFault{RuleIndex: i, RuleID: r.ID, Cause: cause, Detail: detail})
			continue
		}
		if matched {
			idx := i
			res.RuleIndex = &idx
			return res
		}
	}
	return res
}

// Decide turns a MatchResult into a route under the grant. It is pure Go over the caller's own
// configuration: a RuleIndex out of range (which only a broken evaluator could produce) is ignored
// rather than trusted.
func Decide(cfg Config, g Grant, m MatchResult) Decision {
	if m.RuleIndex == nil || *m.RuleIndex < 0 || *m.RuleIndex >= len(cfg.Rules) {
		return defaultDecision(cfg, g, CauseNoMatch, m.Faults)
	}
	idx := *m.RuleIndex
	r := cfg.Rules[idx]
	if d, ok := apply(r.Action, g); ok {
		d.Trace = Trace{Stage: StageRule, RuleIndex: &idx, RuleID: r.ID, RuleName: r.Name, Action: r.Action, Faults: m.Faults}
		return d
	}
	// Matched, but the rule now names something the webhook can no longer reach. Taking the default
	// (rather than trying later rules) keeps the outcome predictable: a revoked target never silently
	// promotes a broader rule further down the list.
	d := defaultDecision(cfg, g, CauseRuleNotGranted, m.Faults)
	d.Trace.RuleIndex, d.Trace.RuleID, d.Trace.RuleName = &idx, r.ID, r.Name
	return d
}

// run evaluates one compiled rule under RuleTimeout. gojq checks the context at every step, so a
// looping or recursing filter stops promptly once the timeout passes and reports it as an error
// value. A single long builtin call cannot be interrupted in-process at all; bounding that is the
// sandbox's job (sandbox.go) — the child's memory watchdog and the parent's hard deadline. That is
// also why evaluation is synchronous: abandoning a still-running call on a goroutine would let the
// next rule, or the child's result write, race an allocation that is still in flight.
func run(ctx context.Context, code *gojq.Code, event map[string]any, vars any) (bool, string, string) {
	rctx, cancel := context.WithTimeout(ctx, RuleTimeout)
	defer cancel()
	v, ok := code.RunWithContext(rctx, event, vars).Next()
	if !ok {
		return false, "", "" // no output: no match
	}
	if err, isErr := v.(error); isErr {
		if rctx.Err() != nil {
			return false, FaultTimeout, ""
		}
		return false, FaultError, clip(err.Error())
	}
	return v != nil && v != false, "", ""
}

// apply resolves an action against the grant at delivery time.
func apply(a Action, g Grant) (Decision, bool) {
	if a.Drop {
		return Decision{Drop: true}, true
	}
	if !queueGranted(a.Queue, g) {
		return Decision{}, false
	}
	eps := candidates(a, g) // grant order keeps the owner first, as fan-out does
	if a.Exclusive {
		ep, ok := exclusiveTarget(eps, a.Queue, g)
		if !ok {
			return Decision{}, false
		}
		eps = []string{ep}
	}
	return Decision{Queue: a.Queue, Endpoints: eps, Once: a.Once, WorkOrder: a.WorkOrder}, len(eps) > 0
}

// paramsValue renders params as the gojq value bound to $params: always an object (empty when unset,
// so `$params.x` is null rather than a compile-time surprise), with numbers normalized the same way
// payloads are.
func paramsValue(params map[string]any) any {
	if len(params) == 0 {
		return map[string]any{}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return map[string]any{}
	}
	if v, ok := DecodePayload(raw).(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// defaultDecision applies the configured default, falling back to the webhook's target queue across
// every target — the pre-routing behavior — when no default is set or the default is unreachable.
func defaultDecision(cfg Config, g Grant, cause string, faults []RuleFault) Decision {
	if cfg.Default != nil {
		if d, ok := apply(*cfg.Default, g); ok {
			d.Trace = Trace{Stage: StageDefault, Cause: cause, Action: *cfg.Default, Faults: faults}
			return d
		}
		cause = CauseDefaultNotGranted
	}
	a := Action{Queue: g.TargetQueue}
	return Decision{
		Queue: g.TargetQueue, Endpoints: slices.Clone(g.Endpoints),
		Trace: Trace{Stage: StageDefault, Cause: cause, Action: a, Faults: faults},
	}
}

func clip(s string) string {
	if len(s) <= maxFaultDetailBytes {
		return s
	}
	return s[:maxFaultDetailBytes] + "…"
}
