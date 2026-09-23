package routing

// Unit tests for the deterministic routing stage. No database: every property here is a pure
// function of (config, grant, event). The DB-backed guarantees — drop persists the event and its
// dedup slot, todos carry the trace, tenant isolation across humans — live in the ingest, store,
// and mcp packages.
//
// Governing: SPEC-0020 REQ "Deterministic Rule Evaluation", REQ "Rule Validation at Save Time",
// REQ "Routing Trace", REQ "Isolation and Tenant Safety"; ADR-0024.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	owner = "11111111-1111-1111-1111-111111111111"
	epB   = "22222222-2222-2222-2222-222222222222"
	epC   = "33333333-3333-3333-3333-333333333333"
)

func grant() Grant {
	return Grant{TargetQueue: "inbox", Queues: []string{"inbox", "forge", "handoff"}, Endpoints: []string{owner, epB, epC}}
}

func event(payload string) map[string]any {
	return Envelope(EnvelopeInput{Source: "generic", TrustMode: "token", WebhookID: "wh", Body: []byte(payload)})
}

func rule(id, expr string, a Action) Rule { return Rule{ID: id, Name: id, Expr: expr, Action: a} }

func validationCode(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	return ve
}

func TestCompileRejectsMalformedExpressions(t *testing.T) {
	for _, expr := range []string{"", "   ", ".foo ==", "if . then", strings.Repeat("a", MaxExprBytes+1)} {
		_, err := Compile(expr)
		if err == nil {
			t.Fatalf("Compile(%.20q) accepted a malformed expression", expr)
		}
		if ve := validationCode(t, err); ve.Code != CodeInvalidExpression {
			t.Fatalf("Compile(%.20q) code = %s, want %s", expr, ve.Code, CodeInvalidExpression)
		}
	}
}

// The sandbox: host access, host control, and non-determinism are refused at compile time, including
// when smuggled through a function definition or a nested pipeline.
func TestCompileRejectsSandboxEscapes(t *testing.T) {
	for _, expr := range []string{
		`env`, `$ENV.HOME`, `env.PATH != null`, `[.[] | env]`,
		`input`, `inputs`, `[inputs] | length`, `input_filename`,
		`now > 0`, `def f: now; f`, `localtime`, `0 | strflocaltime("%Y")`,
		`halt`, `halt_error`, `"x" | halt_error(1)`, `debug`, `debug("x")`, `stderr`,
		`import "a" as a; .`, `include "a"; .`,
	} {
		_, err := Compile(expr)
		if err == nil {
			t.Fatalf("Compile(%q) accepted a sandbox escape", expr)
		}
		if ve := validationCode(t, err); ve.Code != CodeForbiddenFunction {
			t.Fatalf("Compile(%q) code = %s (%s), want %s", expr, ve.Code, ve.Msg, CodeForbiddenFunction)
		}
	}
}

// $__loc__ and undefined variables ($HOME, $__prog_args) do not exist in the sandbox at all — they
// fail to compile rather than resolve to anything from the host — while pure data access that merely
// looks like a forbidden name ("env" as a key) is ordinary routing.
func TestCompileAllowsHarmlessLookalikes(t *testing.T) {
	for _, expr := range []string{`$__loc__`, `$HOME`, `$__prog_args`} {
		_, err := Compile(expr)
		if err == nil {
			t.Fatalf("Compile(%q) resolved a variable the sandbox does not define", expr)
		}
		if ve := validationCode(t, err); ve.Code != CodeInvalidExpression {
			t.Fatalf("Compile(%q) code = %s, want %s", expr, ve.Code, CodeInvalidExpression)
		}
	}
	for _, expr := range []string{`.payload.env == "prod"`, `.payload | getpath(["now"])`, `.headers["x-input"]`} {
		if _, err := Compile(expr); err != nil {
			t.Fatalf("Compile(%q) = %v, want data access to compile", expr, err)
		}
	}
}

func TestEvaluateFirstMatchWins(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("miss", `.payload.kind == "nope"`, Action{Queue: "handoff"}),
		rule("first", `.payload.kind == "review"`, Action{Queue: "forge"}),
		rule("second", `.payload.kind == "review"`, Action{Drop: true}),
	}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{"kind":"review"}`))
	if d.Drop || d.Queue != "forge" {
		t.Fatalf("decision = %+v, want the first matching rule (forge)", d)
	}
	if d.Trace.Stage != StageRule || d.Trace.RuleIndex == nil || *d.Trace.RuleIndex != 1 || d.Trace.RuleID != "first" {
		t.Fatalf("trace = %+v, want rule_index 1 id first", d.Trace)
	}
	if !slices.Equal(d.Endpoints, grant().Endpoints) {
		t.Fatalf("endpoints = %v, want every fan-out target when the action names none", d.Endpoints)
	}
}

// jq truthiness: only false and null fail, and a filter that emits nothing does not match.
func TestEvaluateTruthiness(t *testing.T) {
	for expr, want := range map[string]bool{
		`null`: false, `false`: false, `empty`: false, `.payload.missing`: false,
		`true`: true, `0`: true, `""`: true, `[]`: true, `{}`: true, `(false, true)`: false,
	} {
		cfg := Config{Rules: []Rule{rule("r", expr, Action{Queue: "forge"})}}
		d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
		if got := d.Trace.Stage == StageRule; got != want {
			t.Fatalf("%s matched = %v, want %v (first output decides)", expr, got, want)
		}
	}
}

func TestEvaluateDropAndDefaults(t *testing.T) {
	ctx := context.Background()
	drop := Evaluate(ctx, Config{Rules: []Rule{rule("noise", `.kind == null`, Action{Drop: true})}}, grant(), event(`{}`))
	if !drop.Drop || !drop.Trace.Action.Drop || drop.Trace.Action.Queue != "" {
		t.Fatalf("drop rule = %+v, want a drop decision with the drop action traced", drop)
	}

	none := Evaluate(ctx, Config{}, grant(), event(`{}`))
	if none.Drop || none.Queue != "inbox" || none.Trace.Cause != CauseNoMatch || none.Trace.Stage != StageDefault {
		t.Fatalf("no rules = %+v, want the webhook target queue with cause %s", none, CauseNoMatch)
	}

	dropDefault := Evaluate(ctx, Config{Default: &Action{Drop: true}}, grant(), event(`{}`))
	if !dropDefault.Drop || dropDefault.Trace.Cause != CauseNoMatch {
		t.Fatalf("drop default = %+v, want drop", dropDefault)
	}
}

// A rule that errors stops evaluation there: no later rule runs, the default does not apply, and the
// decision is faulted with the fault on the trace (SPEC-0026 REQ-1).
func TestEvaluateErrorStopsEvaluation(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("boom", `error("boom")`, Action{Drop: true}),
		rule("ok", `true`, Action{Queue: "forge"}),
	}, Default: &Action{Queue: "inbox"}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
	assertFaulted(t, d, "boom", 0, FaultError)
	if d.Fault.Detail == "" {
		t.Fatalf("fault = %+v, want the error detail recorded", d.Fault)
	}
}

// A type error in a later rule faults even though an earlier rule was evaluated and did not match.
func TestEvaluateTypeErrorAfterNoMatchFaults(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("miss", `.kind == "nope"`, Action{Drop: true}),
		rule("type", `.payload.name.first`, Action{Drop: true}),
		rule("ok", `true`, Action{Queue: "forge"}),
	}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{"name":"not-an-object"}`))
	assertFaulted(t, d, "type", 1, FaultError)
}

// A runaway rule times out within its budget, and the timeout faults the delivery rather than
// letting the healthy rule after it route. (Memory bombs are the sandbox's job and are exercised
// through a real child process in sandbox_test.go — running one in-process would allocate gigabytes
// inside the test binary.)
func TestEvaluateTimeoutIsBoundedAndFaults(t *testing.T) {
	for name, expr := range map[string]string{"spin": `last(range(1e12)) > 0`, "recurse": `def f: f; f`} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{Rules: []Rule{
				rule(name, expr, Action{Drop: true}),
				rule("ok", `true`, Action{Queue: "forge"}),
			}}
			start := time.Now()
			d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
			if elapsed := time.Since(start); elapsed > EventBudget+200*time.Millisecond {
				t.Fatalf("evaluation took %v, want it bounded by the %v event budget", elapsed, EventBudget)
			}
			assertFaulted(t, d, name, 0, FaultTimeout)
		})
	}
}

// A spent per-event budget is a fault at the rule it stopped in front of, never the default.
func TestEvaluateBudgetExhaustionFaults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the budget derives from ctx, so it is spent before the first rule
	cfg := Config{Rules: []Rule{rule("first", `true`, Action{Queue: "forge"})}, Default: &Action{Queue: "inbox"}}
	d := Evaluate(ctx, cfg, grant(), event(`{}`))
	assertFaulted(t, d, "first", 0, FaultBudgetExhausted)
}

// A stored rule that no longer compiles faults the delivery.
func TestEvaluateCompileFaultStops(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("stale", `env.HOME`, Action{Drop: true}),
		rule("ok", `true`, Action{Queue: "forge"}),
	}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
	assertFaulted(t, d, "stale", 0, FaultCompile)
}

// SPEC-0026 REQ-1 scenario "A mistyped trust rule no longer admits everyone", with NO `| arrays`
// guard in the rule: the engine itself fails closed.
func TestMistypedTrustRuleFailsClosedWithoutGuards(t *testing.T) {
	cfg := Config{
		Rules: []Rule{
			rule("r1", `.payload.issue.user.login as $a | any($params.trusted[]; . == $a) | not`, Action{Drop: true}),
			rule("r2", `true`, Action{Queue: "forge"}),
		},
		Params: map[string]any{"trusted": "alice"}, // a string, not a list
	}
	d := Evaluate(context.Background(), cfg, grant(), event(`{"issue":{"user":{"login":"mallory"}}}`))
	assertFaulted(t, d, "r1", 0, FaultError)
	if d.Disposition() != DispositionFaulted {
		t.Fatalf("disposition = %q, want %q", d.Disposition(), DispositionFaulted)
	}
}

// SPEC-0026 REQ-1 scenario "A faulting drop rule does not route its delivery": the default of queue
// inbox is not taken.
func TestFaultingDropRuleDoesNotTakeDefault(t *testing.T) {
	cfg := Config{Rules: []Rule{rule("big", `last(range(1e12)) > 0`, Action{Drop: true})}, Default: &Action{Queue: "inbox"}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
	assertFaulted(t, d, "big", 0, FaultTimeout)
}

// Decide never lets a fault become a route, even when a misbehaving evaluator also names a rule.
func TestDecideFaultWinsOverMatch(t *testing.T) {
	cfg := Config{Rules: []Rule{rule("a", `true`, Action{Queue: "forge"}), rule("b", `true`, Action{Queue: "forge"})}}
	idx := 1
	d := Decide(cfg, grant(), MatchResult{RuleIndex: &idx, Faults: []RuleFault{{RuleIndex: 0, RuleID: "forged", Cause: FaultError}}})
	assertFaulted(t, d, "a", 0, FaultError) // the rule id comes from the caller's config, not the result
}

// A sandbox-level fault (RuleIndex -1), or a fault naming a rule that does not exist, is
// Unavailable: the receiver refuses the delivery rather than recording anything about it.
func TestDecideSandboxFaultIsUnavailable(t *testing.T) {
	cfg := Config{Rules: []Rule{rule("a", `true`, Action{Queue: "forge"})}}
	for _, f := range []RuleFault{{RuleIndex: -1, Cause: FaultSandbox}, {RuleIndex: 5, Cause: FaultError}} {
		d := Decide(cfg, grant(), MatchResult{Faults: []RuleFault{f}})
		if !d.Unavailable || d.Faulted || d.Queue != "" || len(d.Endpoints) != 0 || d.Drop {
			t.Fatalf("fault %+v: decision = %+v, want Unavailable with no route", f, d)
		}
		if d.Fault == nil || d.Fault.RuleIndex != -1 || d.Trace.Stage != StageFault {
			t.Fatalf("fault %+v: decision = %+v, want the sandbox fault on a fault-stage trace", f, d)
		}
	}
}

func assertFaulted(t *testing.T, d Decision, ruleID string, index int, cause string) {
	t.Helper()
	if !d.Faulted || d.Unavailable || d.Drop || d.Queue != "" || len(d.Endpoints) != 0 {
		t.Fatalf("decision = %+v, want faulted with no route", d)
	}
	if d.Fault == nil || d.Fault.RuleID != ruleID || d.Fault.RuleIndex != index || d.Fault.Cause != cause {
		t.Fatalf("fault = %+v, want rule %s at %d with cause %s", d.Fault, ruleID, index, cause)
	}
	tr := d.Trace
	if tr.Stage != StageFault || tr.Cause != cause || tr.RuleID != ruleID || tr.RuleIndex == nil ||
		*tr.RuleIndex != index || len(tr.Faults) != 1 || tr.Faults[0].Cause != cause {
		t.Fatalf("trace = %+v, want a fault-stage trace naming rule %s at %d (%s)", tr, ruleID, index, cause)
	}
}

// Evaluation-time tenant safety: a rule saved against a grant that has since shrunk cannot reach
// the revoked queue or endpoint; it takes the default and says why.
func TestEvaluateReappliesGrantAtDelivery(t *testing.T) {
	cfg := Config{Rules: []Rule{rule("to-handoff", `true`, Action{Queue: "handoff", Endpoints: []string{epC}})}}

	shrunkQueues := grant()
	shrunkQueues.Queues = []string{"inbox"}
	d := Evaluate(context.Background(), cfg, shrunkQueues, event(`{}`))
	if d.Queue != "inbox" || d.Trace.Cause != CauseRuleNotGranted || d.Trace.RuleID != "to-handoff" {
		t.Fatalf("revoked queue = %+v, want default with cause %s naming the rule", d, CauseRuleNotGranted)
	}

	revokedRoute := grant()
	revokedRoute.Endpoints = []string{owner, epB}
	d = Evaluate(context.Background(), cfg, revokedRoute, event(`{}`))
	if d.Queue != "inbox" || d.Trace.Cause != CauseRuleNotGranted || !slices.Equal(d.Endpoints, []string{owner, epB}) {
		t.Fatalf("revoked endpoint = %+v, want default across the live targets", d)
	}

	cfg.Default = &Action{Queue: "handoff"}
	d = Evaluate(context.Background(), Config{Default: cfg.Default}, shrunkQueues, event(`{}`))
	if d.Queue != "inbox" || d.Trace.Cause != CauseDefaultNotGranted {
		t.Fatalf("revoked default = %+v, want the target queue with cause %s", d, CauseDefaultNotGranted)
	}
}

// Endpoints narrow fan-out to a subset, in grant (owner-first) order, never widen it.
func TestEvaluateEndpointsNarrowFanOut(t *testing.T) {
	cfg := Config{Rules: []Rule{rule("pick", `true`, Action{Queue: "forge", Endpoints: []string{epC, epB, "44444444-4444-4444-4444-444444444444"}})}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
	if !slices.Equal(d.Endpoints, []string{epB, epC}) {
		t.Fatalf("endpoints = %v, want [%s %s] (intersection, grant order)", d.Endpoints, epB, epC)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("err", `error("x")`, Action{Drop: true}),
		rule("big", `.payload.n > 10`, Action{Queue: "forge"}),
	}}
	a := Evaluate(context.Background(), cfg, grant(), event(`{"n":11}`))
	b := Evaluate(context.Background(), cfg, grant(), event(`{"n":11}`))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same event and rules routed differently:\n%+v\n%+v", a, b)
	}
}

func TestValidate(t *testing.T) {
	good := rule("good", `.kind == "x"`, Action{Queue: "forge"})
	tooMany := make([]Rule, MaxRules+1)
	for i := range tooMany {
		tooMany[i] = rule("r"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26)), `true`, Action{Drop: true})
	}
	cases := []struct {
		name  string
		cfg   Config
		code  string
		index int
	}{
		{"too many", Config{Rules: tooMany}, CodeTooManyRules, MaxRules},
		{"bad id", Config{Rules: []Rule{rule("has space", `true`, Action{Drop: true})}}, CodeInvalidRule, 0},
		{"dup id", Config{Rules: []Rule{good, good}}, CodeInvalidRule, 1},
		{"long name", Config{Rules: []Rule{{ID: "n", Name: strings.Repeat("n", MaxNameBytes+1), Expr: "true", Action: Action{Drop: true}}}}, CodeInvalidRule, 0},
		{"typo", Config{Rules: []Rule{good, rule("typo", `.kind ==`, Action{Drop: true})}}, CodeInvalidExpression, 1},
		{"forbidden", Config{Rules: []Rule{rule("env", `env.HOME`, Action{Drop: true})}}, CodeForbiddenFunction, 0},
		{"queue and drop", Config{Rules: []Rule{rule("both", `true`, Action{Queue: "forge", Drop: true})}}, CodeInvalidRule, 0},
		{"neither", Config{Rules: []Rule{rule("none", `true`, Action{})}}, CodeInvalidRule, 0},
		{"endpoints on drop", Config{Rules: []Rule{rule("ed", `true`, Action{Drop: true, Endpoints: []string{owner}})}}, CodeInvalidRule, 0},
		{"queue not granted", Config{Rules: []Rule{rule("q", `true`, Action{Queue: "someone-elses"})}}, CodeNotGranted, 0},
		{"endpoint not a target", Config{Rules: []Rule{rule("e", `true`, Action{Queue: "forge", Endpoints: []string{"44444444-4444-4444-4444-444444444444"}})}}, CodeNotGranted, 0},
		{"dup endpoint", Config{Rules: []Rule{rule("d", `true`, Action{Queue: "forge", Endpoints: []string{epB, epB}})}}, CodeInvalidRule, 0},
		{"default not granted", Config{Default: &Action{Queue: "nope"}}, CodeNotGranted, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg, grant())
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			ve := validationCode(t, err)
			if ve.Code != tc.code || ve.Index != tc.index {
				t.Fatalf("Validate = (code %s, index %d: %s), want (code %s, index %d)", ve.Code, ve.Index, ve.Msg, tc.code, tc.index)
			}
		})
	}

	// The error names the rule so a failed save is actionable.
	err := Validate(Config{Rules: []Rule{good, rule("typo", `.kind ==`, Action{Drop: true})}}, grant())
	if !strings.Contains(err.Error(), "rule 1 (typo)") {
		t.Fatalf("error %q does not name the offending rule", err)
	}

	// The webhook's own target queue is always routable even when the ceiling no longer lists it.
	narrow := Grant{TargetQueue: "legacy", Endpoints: []string{owner}}
	if err := Validate(Config{Rules: []Rule{rule("t", `true`, Action{Queue: "legacy"})}, Default: &Action{Queue: "legacy"}}, narrow); err != nil {
		t.Fatalf("target queue rejected: %v", err)
	}
	if err := Validate(Config{Rules: []Rule{good, rule("pick", `true`, Action{Queue: "handoff", Endpoints: []string{epC}})}, Default: &Action{Drop: true}}, grant()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// SPEC-0026 REQ-3: params are scalars or homogeneous lists of strings or numbers. Anything else is
// refused naming the key, including the scenario "Nested params refused".
func TestValidateParamTypes(t *testing.T) {
	ok := []map[string]any{
		{"trusted": []any{"alice", "bob"}},
		{"ids": []any{float64(1), float64(2)}},
		{"name": "x", "n": float64(3), "on": true, "empty": []any{}},
		{"go": []string{"a"}, "i": 7},
	}
	for _, p := range ok {
		if err := Validate(Config{Params: p}, grant()); err != nil {
			t.Fatalf("params %v rejected: %v", p, err)
		}
	}
	bad := map[string]map[string]any{
		"trusted": {"trusted": map[string]any{"alice": true}},
		"mixed":   {"mixed": []any{"a", float64(1)}},
		"nested":  {"nested": []any{[]any{"a"}}},
		"bools":   {"bools": []any{true}},
		"nil":     {"nil": nil},
	}
	for key, p := range bad {
		err := Validate(Config{Params: p}, grant())
		if err == nil {
			t.Fatalf("params %v accepted", p)
		}
		ve := validationCode(t, err)
		if ve.Code != CodeInvalidParams || !strings.Contains(err.Error(), `"`+key+`"`) {
			t.Fatalf("params %v: error %q (code %s), want %s naming %q", p, err, ve.Code, CodeInvalidParams, key)
		}
	}
}

func TestNewRuleIDIsValid(t *testing.T) {
	a, b := NewRuleID(), NewRuleID()
	if a == b || !ruleIDPattern.MatchString(a) || !strings.HasPrefix(a, "rule_") {
		t.Fatalf("NewRuleID = %q, %q; want distinct valid ids", a, b)
	}
}
