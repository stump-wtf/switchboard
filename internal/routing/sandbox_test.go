package routing

// Tests for out-of-process evaluation. TestMain installs the child hook, so the sandbox re-executes
// THIS test binary as its child — the production code path, end to end, on any OS. The memory and
// deadline limits are what the in-process tests cannot show: a rule that would allocate gigabytes is
// contained in a child that dies, and the delivery still gets a decision.
//
// Governing: SPEC-0020 Security Requirements "Expression sandboxing", REQ "Isolation and Tenant
// Safety"; ADR-0024.

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	RunChildIfRequested()
	os.Exit(m.Run())
}

// testSandbox re-executes this test binary with a generous deadline: a race-instrumented test binary
// starts far slower than the production binary the default deadline is sized for.
func testSandbox(t *testing.T, opts ...SandboxOption) *Sandbox {
	t.Helper()
	s, err := NewSandbox("", append([]SandboxOption{WithDeadline(20 * time.Second)}, opts...)...)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	return s
}

func cairnInput(body string) EnvelopeInput {
	return EnvelopeInput{Source: SourceCairn, TrustMode: "signed", Verified: true, WebhookID: "wh-1", Body: []byte(body)}
}

func onlySandboxFault(t *testing.T, d Decision) RuleFault {
	t.Helper()
	if len(d.Trace.Faults) != 1 || d.Trace.Faults[0].RuleIndex != -1 {
		t.Fatalf("faults = %+v, want exactly one sandbox-level fault", d.Trace.Faults)
	}
	return d.Trace.Faults[0]
}

// The child is the production path: its decision must be identical to in-process evaluation.
func TestSandboxAgreesWithInProcess(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("err", `.artifact.title | error`, Action{Drop: true}),
		rule("handoff", `.artifact.title | startswith("[handoff:")`, Action{Queue: "handoff", Endpoints: []string{epC}}),
	}, Default: &Action{Drop: true}}
	in := cairnInput(cairnBody)
	got := testSandbox(t).Route(context.Background(), cfg, grant(), in)
	want := InProcess{}.Route(context.Background(), cfg, grant(), in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox decision differs from in-process:\n got %+v\nwant %+v", got, want)
	}
	if got.Queue != "handoff" || !slices.Equal(got.Endpoints, []string{epC}) {
		t.Fatalf("decision = %+v, want handoff on %s", got, epC)
	}
}

func TestSandboxWithoutRulesNeverStartsAChild(t *testing.T) {
	s, err := NewSandbox("/nonexistent/switchboard")
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	d := s.Route(context.Background(), Config{}, grant(), cairnInput(`{}`))
	if d.Queue != "inbox" || len(d.Trace.Faults) != 0 {
		t.Fatalf("no-rule route = %+v, want the target queue with no faults (and no process)", d)
	}
}

// A single allocation of a gigabyte, and a doubling pipe chain, are both contained: the child dies
// at its memory limit and the delivery takes the default, recorded as a sandbox fault.
func TestSandboxContainsMemoryBombs(t *testing.T) {
	for name, expr := range map[string]string{
		"repeat":   `("x" * 1e9) | length > 0`,
		"doubling": `.artifact.title` + strings.Repeat(` | (. + .)`, 40) + ` | length > 0`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{Rules: []Rule{rule("bomb", expr, Action{Drop: true}), rule("ok", `true`, Action{Queue: "forge"})}}
			start := time.Now()
			d := testSandbox(t, WithChildMemBytes(64<<20)).Route(context.Background(), cfg, grant(), cairnInput(cairnBody))
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("bomb took %v to contain", elapsed)
			}
			// Two containment outcomes are both correct, and which one wins is a scheduling race inside
			// the child: the watchdog kills the child (a sandbox-level fault, default routing), or the
			// bomb rule's own timeout fires first and it is recorded as a faulted no-match before the
			// next rule matches. What must never happen is the bomb rule matching (its drop) or the
			// evaluation escaping the child's bounds.
			if d.Drop {
				t.Fatalf("decision = %+v, the bomb rule must never match", d)
			}
			if len(d.Trace.Faults) == 0 {
				t.Fatalf("decision = %+v, want the bomb recorded as a fault", d)
			}
			switch f := d.Trace.Faults[0]; {
			case f.RuleIndex == -1 && f.Cause == FaultSandbox:
				if d.Queue != "inbox" || d.Trace.Cause != CauseNoMatch {
					t.Fatalf("killed child decision = %+v, want default routing", d)
				}
			case f.RuleID == "bomb" && (f.Cause == FaultTimeout || f.Cause == FaultError):
				if d.Queue != "forge" || d.Trace.RuleID != "ok" {
					t.Fatalf("faulted bomb decision = %+v, want the next rule to match", d)
				}
			default:
				t.Fatalf("faults = %+v, want a sandbox failure or a faulted bomb rule", d.Trace.Faults)
			}
		})
	}
}

// The parent's hard deadline kills a child regardless of what it is doing.
func TestSandboxDeadlineKillsTheChild(t *testing.T) {
	s := testSandbox(t)
	s.deadline = time.Millisecond
	d := s.Route(context.Background(), Config{Rules: []Rule{rule("ok", `true`, Action{Queue: "forge"})}}, grant(), cairnInput(`{}`))
	if f := onlySandboxFault(t, d); f.Cause != FaultSandbox || !strings.Contains(f.Detail, "deadline") {
		t.Fatalf("fault = %+v, want a deadline sandbox failure", f)
	}
	if d.Queue != "inbox" {
		t.Fatalf("decision = %+v, want default routing", d)
	}
}

// With every slot taken, a delivery waits briefly and then routes by default instead of queueing
// unboundedly behind other tenants' evaluations.
func TestSandboxBusyRoutesByDefault(t *testing.T) {
	s := testSandbox(t, WithMaxChildren(1))
	s.queueWait = 20 * time.Millisecond
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	d := s.Route(context.Background(), Config{Rules: []Rule{rule("ok", `true`, Action{Queue: "forge"})}}, grant(), cairnInput(`{}`))
	if f := onlySandboxFault(t, d); f.Cause != FaultSandboxBusy {
		t.Fatalf("fault = %+v, want %s", f, FaultSandboxBusy)
	}
}

// A binary that does not speak the child protocol (here: cat, which echoes the request) is a
// failure, never a result — the magic prefix is required.
func TestSandboxRejectsForeignOutput(t *testing.T) {
	s, err := NewSandbox("/bin/cat")
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	d := s.Route(context.Background(), Config{Rules: []Rule{rule("ok", `true`, Action{Queue: "forge"})}}, grant(), cairnInput(`{}`))
	if f := onlySandboxFault(t, d); f.Cause != FaultSandbox {
		t.Fatalf("fault = %+v, want %s", f, FaultSandbox)
	}
}

// The child inherits nothing: the parent's environment carries the DSN and signing keys.
func TestChildEnvironIsMinimal(t *testing.T) {
	t.Setenv("SWITCHBOARD_DATABASE_URL", "postgres://secret")
	env := childEnviron(1 << 20)
	if len(env) != 3 {
		t.Fatalf("child environment = %v, want exactly the three sandbox variables", env)
	}
	for _, kv := range env {
		if strings.Contains(kv, "DATABASE") || strings.Contains(kv, "secret") {
			t.Fatalf("child environment leaked %q", kv)
		}
	}
}

// Decide trusts only the caller's config: an out-of-range index from a broken evaluator is ignored.
func TestDecideIgnoresOutOfRangeIndex(t *testing.T) {
	bad := 7
	d := Decide(Config{Rules: []Rule{rule("only", `true`, Action{Drop: true})}}, grant(), MatchResult{RuleIndex: &bad})
	if d.Drop || d.Queue != "inbox" {
		t.Fatalf("decision = %+v, want the default for an out-of-range index", d)
	}
}
