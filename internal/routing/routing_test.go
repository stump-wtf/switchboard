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

// A rule that errors is no-match, recorded, and evaluation continues to the next rule.
func TestEvaluateErrorsAreRecordedNoMatch(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("boom", `error("boom")`, Action{Drop: true}),
		rule("type", `.payload.name.first`, Action{Drop: true}),
		rule("ok", `true`, Action{Queue: "forge"}),
	}}
	d := Evaluate(context.Background(), cfg, grant(), event(`{"name":"not-an-object"}`))
	if d.Queue != "forge" || len(d.Trace.Faults) != 2 {
		t.Fatalf("decision = %+v, want forge with two recorded faults", d)
	}
	for _, f := range d.Trace.Faults {
		if f.Cause != FaultError {
			t.Fatalf("fault = %+v, want cause %s", f, FaultError)
		}
	}
}

// A runaway rule times out as no-match within its budget and evaluation moves on. (Memory bombs are
// the sandbox's job and are exercised through a real child process in sandbox_test.go — running one
// in-process would allocate gigabytes inside the test binary.)
func TestEvaluateTimeoutIsBounded(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("spin", `last(range(1e12)) > 0`, Action{Drop: true}),
		rule("recurse", `def f: f; f`, Action{Drop: true}),
		rule("ok", `true`, Action{Queue: "forge"}),
	}}
	start := time.Now()
	d := Evaluate(context.Background(), cfg, grant(), event(`{}`))
	if elapsed := time.Since(start); elapsed > EventBudget+200*time.Millisecond {
		t.Fatalf("evaluation took %v, want it bounded by the %v event budget", elapsed, EventBudget)
	}
	if d.Queue != "forge" {
		t.Fatalf("decision = %+v, want the healthy rule after the runaway ones", d)
	}
	causes := map[string]string{}
	for _, f := range d.Trace.Faults {
		causes[f.RuleID] = f.Cause
	}
	if causes["spin"] != FaultTimeout || causes["recurse"] != FaultTimeout {
		t.Fatalf("faults = %+v, want spin and recurse to time out", d.Trace.Faults)
	}
}

func TestEvaluateBudgetExhaustion(t *testing.T) {
	var rules []Rule
	for i := 0; i < 8; i++ {
		rules = append(rules, rule("spin"+string(rune('a'+i)), `last(range(1e12))`, Action{Drop: true}))
	}
	d := Evaluate(context.Background(), Config{Rules: rules}, grant(), event(`{}`))
	if d.Drop || d.Trace.Cause != CauseNoMatch {
		t.Fatalf("decision = %+v, want the default once the budget is spent", d)
	}
	last := d.Trace.Faults[len(d.Trace.Faults)-1]
	if last.Cause != FaultBudgetExhausted {
		t.Fatalf("last fault = %+v, want %s", last, FaultBudgetExhausted)
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

func TestNewRuleIDIsValid(t *testing.T) {
	a, b := NewRuleID(), NewRuleID()
	if a == b || !ruleIDPattern.MatchString(a) || !strings.HasPrefix(a, "rule_") {
		t.Fatalf("NewRuleID = %q, %q; want distinct valid ids", a, b)
	}
}
