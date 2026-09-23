package routing

// Out-of-process rule evaluation. gojq's per-step context check bounds TIME, but not memory: a
// single builtin call can allocate gigabytes before the next check ("x" * 1e9 is one uninterruptible
// strings.Repeat), a short pipe chain of concatenations doubles a value per step faster than the
// timeout notices, and serializing or comparing a structure full of shared references is exponential
// inside one call. No jq subset that is still useful for routing closes all of those, and switchboard
// is multi-tenant — one tenant's rule must not be able to take the process down for everyone.
//
// So the jq half of routing runs in a child process: the same binary, re-executed with an empty
// environment (no inherited secrets, no DSN), fed {rules, envelope input} on stdin. The child builds
// the envelope, runs Match, and writes back only the index of the first matching rule, or the fault
// that stopped evaluation. A watchdog inside the child exits it the moment runtime-mapped memory
// passes its limit; the parent kills it on a hard wall-clock deadline and caps how many children run
// at once. Any child failure, and a busy or missing sandbox, is a sandbox fault (RuleIndex -1). Decide
// turns it into an Unavailable decision, and the receiver refuses the delivery with a 503 and
// persists nothing, so the producer retries. It never routes by default.
//
// The parent then applies the action with Decide over its OWN copy of the configuration and grant,
// so even a child that misbehaved can only ever name a rule, not a destination.
//
// Governing: SPEC-0020 Security Requirements "Expression sandboxing" (bounded by node-count and
// wall-clock limits), REQ "Isolation and Tenant Safety"; ADR-0024; SPEC-0026 REQ-1 "Faults Stop
// Evaluation", REQ-2 "Unavailable Sandbox Refuses the Delivery"; ADR-0031.
//
// @joestump-agent 09/11/2026 - Added after gojq's in-process limits proved unable to bound memory.
//
// @joestump-agent 09/23/2026 - A sandbox failure refuses the delivery instead of routing it by
// default (#212).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/metrics"
	"strconv"
	"sync"
	"time"
)

const (
	childEnv       = "SWITCHBOARD_ROUTING_CHILD"
	childMemEnv    = "SWITCHBOARD_ROUTING_CHILD_MEM_BYTES"
	childMagic     = "switchboard-routing-match-v1\n"
	childExitMem   = 3
	childExitInput = 2

	// DefaultChildMemBytes bounds everything the child maps: a 5 MiB payload decoded into a jq value
	// tree fits comfortably, a runaway allocation does not.
	DefaultChildMemBytes = 128 << 20
	// DefaultMaxChildren caps concurrent evaluations process-wide, which is what bounds aggregate
	// memory under a delivery flood.
	DefaultMaxChildren = 2
	// childStartAllowance is added to EventBudget for process start before the parent kills. A cold
	// start on a loaded host must not turn into a spurious fault, and the whole bound (2s, plus at most
	// queueWait) stays well inside producers' 5s delivery timeouts.
	childStartAllowance = 1750 * time.Millisecond
	// queueWait is how long a delivery waits for a free child slot before it is refused as
	// unavailable (the producer retries).
	queueWait = time.Second

	maxChildRequestBytes  = 16 << 20
	maxChildResponseBytes = 256 << 10

	FaultSandbox     = "sandbox_failure"
	FaultSandboxBusy = "sandbox_busy"
)

// Router routes one delivery: it evaluates rules against the envelope built from in, then decides.
type Router interface {
	Route(ctx context.Context, cfg Config, g Grant, in EnvelopeInput) Decision
}

// InProcess evaluates in the calling process. It is for tests and tooling only: it has the timeout
// but not the memory bound.
type InProcess struct{}

// Route implements Router.
func (InProcess) Route(ctx context.Context, cfg Config, g Grant, in EnvelopeInput) Decision {
	if len(cfg.Rules) == 0 {
		return Decide(cfg, g, MatchResult{})
	}
	return Evaluate(ctx, cfg, g, Envelope(in))
}

// Unavailable reports every delivery to a webhook with rules as Unavailable. It stands in when no
// Sandbox could be built, so such a webhook refuses its deliveries (503, producer retries) rather
// than routing them by default or evaluating tenant expressions
// in-process without a memory bound.
type Unavailable struct{}

// Route implements Router.
func (Unavailable) Route(_ context.Context, cfg Config, g Grant, _ EnvelopeInput) Decision {
	if len(cfg.Rules) == 0 {
		return Decide(cfg, g, MatchResult{})
	}
	return Decide(cfg, g, sandboxFault(FaultSandbox, "no rule evaluator is available"))
}

// Sandbox evaluates in child processes.
type Sandbox struct {
	path      string
	memBytes  int64
	slots     chan struct{}
	deadline  time.Duration
	queueWait time.Duration
}

// SandboxOption tunes a Sandbox (tests use these to exercise the limits quickly).
type SandboxOption func(*Sandbox)

// WithChildMemBytes sets the child's memory limit.
func WithChildMemBytes(n int64) SandboxOption { return func(s *Sandbox) { s.memBytes = n } }

// WithDeadline sets the parent's hard kill deadline for one evaluation, start-up included (tests
// running race-instrumented binaries need more than production's default).
func WithDeadline(d time.Duration) SandboxOption { return func(s *Sandbox) { s.deadline = d } }

// WithMaxChildren sets the concurrent child cap.
func WithMaxChildren(n int) SandboxOption {
	return func(s *Sandbox) { s.slots = make(chan struct{}, n) }
}

// NewSandbox builds a Sandbox that re-executes path, or this executable when path is "". The binary
// must call RunChildIfRequested before doing anything else.
func NewSandbox(path string, opts ...SandboxOption) (*Sandbox, error) {
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("routing: resolve executable for sandbox: %w", err)
		}
		path = exe
	}
	s := &Sandbox{path: path, memBytes: DefaultChildMemBytes, slots: make(chan struct{}, DefaultMaxChildren),
		deadline: EventBudget + childStartAllowance, queueWait: queueWait}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Route implements Router. A webhook with no rules never starts a child.
func (s *Sandbox) Route(ctx context.Context, cfg Config, g Grant, in EnvelopeInput) Decision {
	if len(cfg.Rules) == 0 {
		return Decide(cfg, g, MatchResult{})
	}
	return Decide(cfg, g, s.match(ctx, cfg.Rules, cfg.Params, in))
}

func sandboxFault(cause, detail string) MatchResult {
	return MatchResult{Faults: []RuleFault{{RuleIndex: -1, Cause: cause, Detail: detail}}}
}

func (s *Sandbox) match(ctx context.Context, rules []Rule, params map[string]any, in EnvelopeInput) MatchResult {
	wait, cancelWait := context.WithTimeout(ctx, s.queueWait)
	defer cancelWait()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-wait.Done():
		return sandboxFault(FaultSandboxBusy, "no evaluation slot free")
	}

	req, err := json.Marshal(childRequest{Rules: rules, Params: params, Input: in})
	if err != nil || len(req) > maxChildRequestBytes {
		return sandboxFault(FaultSandbox, "request too large")
	}
	runCtx, cancel := context.WithTimeout(ctx, s.deadline)
	defer cancel()
	// -test.run=^$ makes a Go test binary that lacks the child hook run no tests and exit, rather
	// than recursing into its own suite; the real binary enters child mode before parsing arguments.
	cmd := exec.CommandContext(runCtx, s.path, "-test.run=^$")
	cmd.Env = childEnviron(s.memBytes)
	cmd.Stdin = bytes.NewReader(req)
	var out limitedBuffer
	out.limit = maxChildResponseBytes
	cmd.Stdout = &out
	cmd.Stderr = io.Discard // a panic trace could quote payload text; never surface it
	cmd.WaitDelay = 100 * time.Millisecond

	runErr := cmd.Run()
	if runCtx.Err() != nil && ctx.Err() == nil {
		return sandboxFault(FaultSandbox, "evaluation exceeded its deadline")
	}
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) && exit.ExitCode() == childExitMem {
			return sandboxFault(FaultSandbox, "evaluation exceeded its memory limit")
		}
		return sandboxFault(FaultSandbox, "evaluation process failed")
	}
	body, ok := bytes.CutPrefix(out.Bytes(), []byte(childMagic))
	if !ok || out.overflow {
		return sandboxFault(FaultSandbox, "evaluation process returned no result")
	}
	var m MatchResult
	if err := json.Unmarshal(body, &m); err != nil {
		return sandboxFault(FaultSandbox, "evaluation process returned a malformed result")
	}
	if m.RuleIndex != nil && (*m.RuleIndex < 0 || *m.RuleIndex >= len(rules)) {
		return sandboxFault(FaultSandbox, "evaluation process named a rule that does not exist")
	}
	return m
}

// childEnviron is the child's ENTIRE environment. Nothing is inherited: the parent's environment
// holds the database DSN, OAuth client secrets, and the secret-box key, and the child runs expressions
// a tenant wrote.
func childEnviron(memBytes int64) []string {
	return []string{childEnv + "=1", childMemEnv + "=" + strconv.FormatInt(memBytes, 10), "GOMAXPROCS=2"}
}

type childRequest struct {
	Rules  []Rule         `json:"rules"`
	Params map[string]any `json:"params,omitempty"`
	Input  EnvelopeInput  `json:"input"`
}

// RunChildIfRequested turns this process into a routing child when the parent asked for one, and
// never returns in that case. Call it first in main (and in TestMain of packages that exercise the
// sandbox).
func RunChildIfRequested() {
	if os.Getenv(childEnv) != "1" {
		return
	}
	limit, err := strconv.ParseInt(os.Getenv(childMemEnv), 10, 64)
	if err != nil || limit <= 0 {
		limit = DefaultChildMemBytes
	}
	os.Exit(runChild(os.Stdin, os.Stdout, uint64(limit)))
}

var watchdogOnce sync.Once

func runChild(r io.Reader, w io.Writer, memLimit uint64) int {
	watchdogOnce.Do(func() { go watchdog(memLimit) })
	var req childRequest
	if err := json.NewDecoder(io.LimitReader(r, maxChildRequestBytes)).Decode(&req); err != nil {
		return childExitInput
	}
	m := Match(context.Background(), req.Rules, req.Params, Envelope(req.Input))
	body, err := json.Marshal(m)
	if err != nil {
		return childExitInput
	}
	if _, err := io.WriteString(w, childMagic); err != nil {
		return childExitInput
	}
	if _, err := w.Write(body); err != nil {
		return childExitInput
	}
	return 0
}

// exitProcess is how the watchdog ends the child. It is os.Exit everywhere except the watchdog's
// own unit test, which cannot otherwise observe the loop tripping without a race against the child
// finishing first.
var exitProcess = os.Exit

// watchdog exits the child once the runtime has mapped more than limit bytes. It reads the total
// the runtime has obtained from the OS, which grows the moment a large allocation is made — before
// the pages are written — so a runaway allocation is caught at reservation, not after it is filled.
func watchdog(limit uint64) {
	sample := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	for {
		metrics.Read(sample)
		if sample[0].Value.Kind() == metrics.KindUint64 && sample[0].Value.Uint64() > limit {
			exitProcess(childExitMem)
		}
		time.Sleep(time.Millisecond)
	}
}

// limitedBuffer stops accepting output past limit and remembers that it did.
type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
