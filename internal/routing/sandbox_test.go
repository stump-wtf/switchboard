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
		rule("miss", `.artifact.title == "nope"`, Action{Drop: true}),
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

// A rule fault inside the child faults the delivery exactly as in-process evaluation does: the
// child reports the fault, and the parent never routes past it (SPEC-0026 REQ-1).
func TestSandboxFaultAgreesWithInProcess(t *testing.T) {
	cfg := Config{Rules: []Rule{
		rule("err", `.artifact.title | error`, Action{Drop: true}),
		rule("handoff", `.artifact.title | startswith("[handoff:")`, Action{Queue: "handoff", Endpoints: []string{epC}}),
	}, Default: &Action{Queue: "inbox"}}
	in := cairnInput(cairnBody)
	got := testSandbox(t).Route(context.Background(), cfg, grant(), in)
	want := InProcess{}.Route(context.Background(), cfg, grant(), in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox decision differs from in-process:\n got %+v\nwant %+v", got, want)
	}
	assertFaulted(t, got, "err", 0, FaultError)
}

// Without a sandbox, a webhook with rules is Unavailable (the receiver answers 503) and one without
// rules routes as it always did (SPEC-0026 REQ-2).
func TestUnavailableRouter(t *testing.T) {
	d := Unavailable{}.Route(context.Background(), Config{Rules: []Rule{rule("ok", `true`, Action{Queue: "forge"})}}, grant(), cairnInput(`{}`))
	if !d.Unavailable || d.Queue != "" || len(d.Endpoints) != 0 {
		t.Fatalf("decision = %+v, want Unavailable with no route", d)
	}
	none := Unavailable{}.Route(context.Background(), Config{}, grant(), cairnInput(`{}`))
	if none.Unavailable || none.Queue != "inbox" {
		t.Fatalf("no-rules decision = %+v, want the target queue", none)
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
			// A 5s deadline (rather than testSandbox's 20s) keeps "contained" meaning contained: a child
			// stalled by cgroup memory pressure (seen at 10.7s in a 768 MiB container) is killed by the
			// parent well inside the 10s bound below, and lands as a sandbox fault.
			d := testSandbox(t, WithChildMemBytes(64<<20), WithDeadline(5*time.Second)).Route(context.Background(), cfg, grant(), cairnInput(cairnBody))
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("bomb took %v to contain", elapsed)
			}
			// Two containment outcomes are both correct, and which one wins is a scheduling race inside
			// the child. Either the watchdog kills the child (a sandbox-level fault: Unavailable, the
			// receiver answers 503), or the bomb rule's own timeout fires first and the delivery is
			// faulted at the bomb rule. Either way nothing routes: not the bomb's drop, not the next
			// rule, not the default (SPEC-0026 REQ-1, REQ-2).
			if d.Drop || d.Queue != "" || len(d.Endpoints) != 0 {
				t.Fatalf("decision = %+v, a contained bomb must route nowhere", d)
			}
			if d.Fault == nil || len(d.Trace.Faults) != 1 {
				t.Fatalf("decision = %+v, want the bomb recorded as exactly one fault", d)
			}
			switch f := *d.Fault; {
			case f.RuleIndex == -1 && f.Cause == FaultSandbox:
				if !d.Unavailable {
					t.Fatalf("killed child decision = %+v, want Unavailable", d)
				}
			case f.RuleID == "bomb" && (f.Cause == FaultTimeout || f.Cause == FaultError || f.Cause == FaultBudgetExhausted):
				if !d.Faulted {
					t.Fatalf("faulted bomb decision = %+v, want Faulted", d)
				}
			default:
				t.Fatalf("fault = %+v, want a sandbox failure or a faulted bomb rule", f)
			}
		})
	}
}

// The watchdog trips once mapped memory passes its limit, and exits with the code the parent reads
// as "exceeded its memory limit". The bomb test above cannot show this on its own — a rule timeout is
// also an acceptable outcome there, so a no-op watchdog passes it — and an end-to-end child with a
// tiny limit is no better: on Linux the child routinely finishes before the watchdog's first sample.
// So the loop is tested directly, with the exit captured. A 1 MiB limit sits below the runtime's own
// baseline, so the first sample must trip it.
func TestWatchdogExitsOverItsMemoryLimit(t *testing.T) {
	codes := make(chan int, 1)
	orig := exitProcess
	exitProcess = func(code int) {
		codes <- code
		select {} // a real exit never returns; park the loop so it cannot report twice
	}
	t.Cleanup(func() { exitProcess = orig })

	go watchdog(1 << 20)
	select {
	case code := <-codes:
		if code != childExitMem {
			t.Fatalf("watchdog exit code = %d, want %d", code, childExitMem)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog never tripped with a limit below the runtime baseline")
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
	if !d.Unavailable || d.Queue != "" {
		t.Fatalf("decision = %+v, want Unavailable with no route", d)
	}
}

// With every slot taken, a delivery waits briefly and is then refused as Unavailable (the producer
// retries), instead of queueing unboundedly behind other tenants' evaluations or routing by default.
func TestSandboxBusyIsUnavailable(t *testing.T) {
	s := testSandbox(t, WithMaxChildren(1))
	s.queueWait = 20 * time.Millisecond
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	d := s.Route(context.Background(), Config{Rules: []Rule{rule("ok", `true`, Action{Queue: "forge"})}}, grant(), cairnInput(`{}`))
	if f := onlySandboxFault(t, d); f.Cause != FaultSandboxBusy {
		t.Fatalf("fault = %+v, want %s", f, FaultSandboxBusy)
	}
	if !d.Unavailable || d.Queue != "" {
		t.Fatalf("decision = %+v, want Unavailable with no route", d)
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

// Decide trusts only the caller's config: an out-of-range index from a broken evaluator is never
// followed, and never falls back to the default either. It is Unavailable.
func TestDecideRefusesOutOfRangeIndex(t *testing.T) {
	bad := 7
	d := Decide(Config{Rules: []Rule{rule("only", `true`, Action{Drop: true})}}, grant(), MatchResult{RuleIndex: &bad})
	if d.Drop || d.Queue != "" || !d.Unavailable {
		t.Fatalf("decision = %+v, want Unavailable for an out-of-range index", d)
	}
}
