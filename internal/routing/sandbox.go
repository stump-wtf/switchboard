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
// that stopped evaluation. Before each rule it also writes that rule's index, so the parent knows
// which rule a child was running when it died. A watchdog inside the child exits it the moment
// runtime-mapped memory passes its limit; the parent kills it on a hard wall-clock deadline and caps
// how many children run at once.
//
// A dead child is classified by what killed it (SPEC-0026 REQ-1, REQ-2):
//
//   - Killed WHILE RUNNING A RULE: a fault of that rule on this delivery. The memory limit is
//     budget_exhausted. The deadline is a timeout when the rule had been running for longer than a
//     whole event's budget, which no healthy rule does. Both are properties of the rule and the
//     payload, so the delivery is faulted (recorded, routed nowhere) exactly as an in-process timeout
//     is. A 503 would lose it to a retry loop that can never succeed, and the save-time dry-run could
//     never name the rule.
//   - Anything else is a sandbox fault (RuleIndex -1): no sandbox, no free slot, a child that failed
//     to start or died before reaching a rule, a deadline no rule overran, or garbage output. Decide
//     turns it into an Unavailable decision, and the receiver refuses the delivery with a 503 and
//     persists nothing, so the producer retries once the instance is healthy. It never routes by
//     default.
//
// Slots are shared fairly: one tenant holds at most all but one of them, so a tenant whose rules are
// slow, or whose deliveries flood in, makes only its OWN deliveries wait (ADR-0038 F5).
//
// The parent then applies the action with Decide over its OWN copy of the configuration and grant,
// so even a child that misbehaved can only ever name a rule, not a destination.
//
// Governing: SPEC-0020 Security Requirements "Expression sandboxing" (bounded by node-count and
// wall-clock limits), REQ "Isolation and Tenant Safety"; ADR-0024; SPEC-0026 REQ-1 "Faults Stop
// Evaluation", REQ-2 "Unavailable Sandbox Refuses the Delivery"; ADR-0031; ADR-0038 F5.
//
// @joestump-agent 09/11/2026 - Added after gojq's in-process limits proved unable to bound memory.
//
// @joestump-agent 09/23/2026 - A sandbox failure refuses the delivery instead of routing it by
// default (#212).
//
// @joestump-agent 09/26/2026 - A child killed mid-rule faults that rule rather than answering 503,
// and slots are shared fairly between tenants (#212 review).

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
	childMagic     = "switchboard-routing-match-v2\n"
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
	// maxOutputMarks bounds how many output arrivals the parent timestamps. The child writes one line
	// per rule plus the magic and the result, so this is ample.
	maxOutputMarks = 4*MaxRules + 8

	// After the magic line, the child writes "@<index>" before each rule it runs and finally
	// "=<MatchResult JSON>". A killed child never writes the result line.
	childLineRule   = "@"
	childLineResult = "="

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

	mu      sync.Mutex
	tenants map[string]*tenantSlots // live per-tenant shares, dropped when nobody holds or waits
}

// tenantSlots is one tenant's share of the sandbox's slots.
type tenantSlots struct {
	slots chan struct{}
	refs  int // holders and waiters; the entry is deleted when this reaches zero
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
	tenant := g.Tenant
	if tenant == "" {
		tenant = "webhook:" + in.WebhookID // no owner known: the webhook is its own tenant
	}
	return Decide(cfg, g, s.match(ctx, tenant, cfg.Rules, cfg.Params, in))
}

// tenantShare is how many slots one tenant may hold at once: all but one, so every other tenant
// always has a slot it can get. A one-slot sandbox cannot be shared and gives each tenant the lot.
func (s *Sandbox) tenantShare() int { return max(cap(s.slots)-1, 1) }

// acquire takes one slot of the tenant's share and then one global slot, waiting until wait is
// done. It returns the release func, or false when no slot came free in time. The tenant's share is
// taken first, so a tenant that already holds its share waits on itself without also occupying the
// global slot another tenant needs.
// Governing: ADR-0038 F5 (per-owner fair share of slots); SPEC-0026 REQ-2.
func (s *Sandbox) acquire(wait context.Context, tenant string) (func(), bool) {
	s.mu.Lock()
	if s.tenants == nil {
		s.tenants = map[string]*tenantSlots{}
	}
	ts := s.tenants[tenant]
	if ts == nil {
		ts = &tenantSlots{slots: make(chan struct{}, s.tenantShare())}
		s.tenants[tenant] = ts
	}
	ts.refs++
	s.mu.Unlock()
	unref := func() {
		s.mu.Lock()
		if ts.refs--; ts.refs == 0 {
			delete(s.tenants, tenant)
		}
		s.mu.Unlock()
	}

	select {
	case ts.slots <- struct{}{}:
	case <-wait.Done():
		unref()
		return nil, false
	}
	select {
	case s.slots <- struct{}{}:
	case <-wait.Done():
		<-ts.slots
		unref()
		return nil, false
	}
	return func() {
		<-s.slots
		<-ts.slots
		unref()
	}, true
}

func sandboxFault(cause, detail string) MatchResult {
	return MatchResult{Faults: []RuleFault{{RuleIndex: -1, Cause: cause, Detail: detail}}}
}

func (s *Sandbox) match(ctx context.Context, tenant string, rules []Rule, params map[string]any, in EnvelopeInput) MatchResult {
	wait, cancelWait := context.WithTimeout(ctx, s.queueWait)
	release, ok := s.acquire(wait, tenant)
	cancelWait()
	if !ok {
		return sandboxFault(FaultSandboxBusy, "no evaluation slot free")
	}
	defer release()

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
	out := &childOutput{limit: maxChildResponseBytes}
	cmd.Stdout = out
	cmd.Stderr = io.Discard // a panic trace could quote payload text; never surface it
	cmd.WaitDelay = 100 * time.Millisecond

	runErr := cmd.Run()
	var end childEnd
	switch {
	case runCtx.Err() != nil && ctx.Err() == nil:
		end.deadline, _ = runCtx.Deadline()
	case runErr != nil:
		var exit *exec.ExitError
		end.memory = errors.As(runErr, &exit) && exit.ExitCode() == childExitMem
		end.failed = !end.memory
	}
	return classifyChild(out, end, len(rules))
}

// ruleKilled is a child killed while it ran rule i: a fault of that rule on this delivery.
func ruleKilled(i int, cause, detail string) MatchResult {
	return MatchResult{Faults: []RuleFault{{RuleIndex: i, Cause: cause, Detail: detail}}}
}

// childEnd is how a child's run ended: killed at the parent's deadline (deadline set), exited at its
// memory limit, failed some other way, or (the zero value) exited cleanly.
type childEnd struct {
	deadline time.Time
	memory   bool
	failed   bool
}

// classifyChild turns what a child wrote and how it ended into a MatchResult. It is the one place
// that decides whether a dead child is a fault of the rule it was running (the delivery is faulted)
// or of the sandbox (Unavailable, 503). The file comment says why.
// Governing: SPEC-0026 REQ-1 "Faults Stop Evaluation", REQ-2 "Unavailable Sandbox Refuses the
// Delivery".
func classifyChild(out *childOutput, end childEnd, nRules int) MatchResult {
	rep, bad := readChild(out, nRules)
	switch {
	case !end.deadline.IsZero():
		// A deadline is the rule's doing only if that rule had been running for longer than a whole
		// event's budget. Otherwise the child was slow to start or starved by the host, which says
		// nothing about the delivery.
		if bad == "" && rep.running >= 0 && !rep.runningAt.IsZero() && end.deadline.Sub(rep.runningAt) >= EventBudget {
			return ruleKilled(rep.running, FaultTimeout, "evaluation exceeded its deadline")
		}
		return sandboxFault(FaultSandbox, "evaluation exceeded its deadline")
	case end.memory:
		if bad == "" && rep.running >= 0 {
			return ruleKilled(rep.running, FaultBudgetExhausted, "evaluation exceeded its memory limit")
		}
		return sandboxFault(FaultSandbox, "evaluation exceeded its memory limit")
	case end.failed:
		return sandboxFault(FaultSandbox, "evaluation process failed")
	case bad != "":
		return sandboxFault(FaultSandbox, bad)
	case rep.result == nil:
		return sandboxFault(FaultSandbox, "evaluation process returned no result")
	}
	m := *rep.result
	if m.RuleIndex != nil && (*m.RuleIndex < 0 || *m.RuleIndex >= nRules) {
		return sandboxFault(FaultSandbox, "evaluation process named a rule that does not exist")
	}
	return m
}

// childReport is what a child wrote: the last rule it said it was starting (-1 for none) and when
// that line reached the parent, and its result if it got that far.
type childReport struct {
	running   int
	runningAt time.Time
	result    *MatchResult
}

// readChild parses a child's output. A non-empty second value means the output cannot be trusted at
// all (no magic, an overflow, an unknown line, a rule index out of range or out of order, or a
// malformed result), and the caller treats the child as a sandbox failure. A trailing partial line
// is what a child killed mid-write leaves behind, and is ignored.
func readChild(out *childOutput, nRules int) (childReport, string) {
	rep := childReport{running: -1}
	body, ok := bytes.CutPrefix(out.Bytes(), []byte(childMagic))
	if !ok || out.overflow {
		return rep, "evaluation process returned no result"
	}
	offset := len(childMagic)
	for {
		line, rest, complete := bytes.Cut(body, []byte("\n"))
		if !complete {
			return rep, ""
		}
		offset += len(line) + 1
		body = rest
		switch {
		case rep.result != nil:
			return rep, "evaluation process wrote past its result"
		case bytes.HasPrefix(line, []byte(childLineRule)):
			i, err := strconv.Atoi(string(line[len(childLineRule):]))
			if err != nil || i < 0 || i >= nRules || i <= rep.running {
				return rep, "evaluation process reported a rule that does not exist"
			}
			rep.running, rep.runningAt = i, out.arrivedAt(offset)
		case bytes.HasPrefix(line, []byte(childLineResult)):
			var m MatchResult
			if err := json.Unmarshal(line[len(childLineResult):], &m); err != nil {
				return rep, "evaluation process returned a malformed result"
			}
			rep.result = &m
		default:
			return rep, "evaluation process returned a malformed result"
		}
	}
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
	if _, err := io.WriteString(w, childMagic); err != nil {
		return childExitInput
	}
	// Each progress line is its own write, so it reaches the parent before the rule starts: if the
	// rule then kills the child, the parent knows which rule did it.
	m := match(context.Background(), req.Rules, req.Params, Envelope(req.Input), func(i int) {
		_, _ = io.WriteString(w, childLineRule+strconv.Itoa(i)+"\n")
	})
	body, err := json.Marshal(m)
	if err != nil {
		return childExitInput
	}
	if _, err := io.WriteString(w, childLineResult+string(body)+"\n"); err != nil {
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

// childOutput collects a child's stdout. It stops accepting output past limit and remembers that it
// did, and it timestamps each arrival so the parent can tell how long the child's current rule has
// been running.
type childOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
	marks    []outputMark
}

// outputMark is one arrival: the buffer length after it, and when it came.
type outputMark struct {
	end int
	at  time.Time
}

func (b *childOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return len(p), nil
	}
	n, err := b.Buffer.Write(p)
	if len(b.marks) < maxOutputMarks {
		b.marks = append(b.marks, outputMark{end: b.Len(), at: time.Now()})
	}
	return n, err
}

// arrivedAt is when the output up to offset had arrived, or the zero time when that is not known.
func (b *childOutput) arrivedAt(offset int) time.Time {
	for _, m := range b.marks {
		if m.end >= offset {
			return m.at
		}
	}
	return time.Time{}
}
