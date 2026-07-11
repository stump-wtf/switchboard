package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/adapter"
	"github.com/joestump/switchboard/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fastOpts keeps supervision intervals tiny so tests run in milliseconds. Backoff and the enabled
// re-check are behaviors under test, not real waits.
func fastOpts() Options {
	return Options{
		BackoffMin:      2 * time.Millisecond,
		BackoffMax:      10 * time.Millisecond,
		EnabledInterval: 2 * time.Millisecond,
		HealthTimeout:   time.Second,
	}
}

// fakeRegistry is an in-memory, mutex-guarded Registry (the runner's slice of *store.Store):
// registration rows, the runtime enabled flag, and recorded poll outcomes.
type fakeRegistry struct {
	mu      sync.Mutex
	rows    map[string]store.Adapter
	polls   map[string][]string // name → recorded pollErr strings ("" = healthy)
	regErrs int                 // scripted RegisterAdapter failures remaining
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{rows: map[string]store.Adapter{}, polls: map[string][]string{}}
}

func (f *fakeRegistry) RegisterAdapter(_ context.Context, name, family, trustMode string, config []byte) (store.Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.regErrs > 0 {
		f.regErrs--
		return store.Adapter{}, errors.New("db down")
	}
	row, ok := f.rows[name]
	if !ok {
		row = store.Adapter{Name: name, Enabled: true} // new rows default to enabled
	}
	row.Family, row.TrustMode, row.Config = family, trustMode, config // preserves Enabled
	f.rows[name] = row
	return row, nil
}

func (f *fakeRegistry) AdapterEnabled(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[name]
	if !ok {
		return false, store.ErrNotFound
	}
	return row.Enabled, nil
}

func (f *fakeRegistry) RecordAdapterPoll(_ context.Context, name, pollErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[name]; !ok {
		return store.ErrNotFound
	}
	f.polls[name] = append(f.polls[name], pollErr)
	return nil
}

func (f *fakeRegistry) setEnabled(name string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[name]
	row.Name, row.Enabled = name, enabled
	f.rows[name] = row
}

func (f *fakeRegistry) pollLog(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.polls[name]...)
}

// fakeAdapter is an integration-test transport (no real Redis): Consume fails failures times, then
// blocks until its context is cancelled — the shape of a healthy long-poll loop.
type fakeAdapter struct {
	name     string
	consumes atomic.Int64 // total Consume calls
	failures atomic.Int64 // scripted transient errors remaining
	blocked  atomic.Int64 // 1 while a Consume is blocked healthy
}

func (a *fakeAdapter) Name() string        { return a.name }
func (a *fakeAdapter) TrustDetail() string { return "fake broker" }

func (a *fakeAdapter) Consume(ctx context.Context, _ adapter.Sink) error {
	a.consumes.Add(1)
	if a.failures.Load() > 0 {
		a.failures.Add(-1)
		return errors.New("broker connection refused")
	}
	a.blocked.Store(1)
	defer a.blocked.Store(0)
	<-ctx.Done()
	// Contract: on cancellation the transport stops consuming, leaves in-flight-unstored messages
	// un-acked, and returns the context error (see internal/adapter/redis for the real ones).
	return ctx.Err()
}

// nopSink stands in for the shared back half (the enqueue coupling story owns the real one).
type nopSink struct{}

func (nopSink) Deliver(_ context.Context, env adapter.Envelope) error { return env.Validate() }

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startRunner runs r.Run in a goroutine and returns a channel that closes when Run has returned —
// i.e. when every worker goroutine has been joined.
func startRunner(t *testing.T, r *Runner, ctx context.Context) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	return done
}

// SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety", scenario "Graceful shutdown stops
// consuming and abandons in-flight safely": cancelling the runner's context stops every consume
// loop and Run returns only after all workers are joined — no leaked goroutines.
func TestGracefulShutdown(t *testing.T) {
	reg := newFakeRegistry()
	r := New(reg, testLogger(), fastOpts())
	a1 := &fakeAdapter{name: "fake-one"}
	a2 := &fakeAdapter{name: "fake-two"}
	if err := r.Add(a1, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(a2, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startRunner(t, r, ctx)

	// Both adapters reach a blocked (healthy, consuming) state.
	waitFor(t, "both adapters consuming", func() bool {
		return a1.blocked.Load() == 1 && a2.blocked.Load() == 1
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel — leaked worker goroutine")
	}
	if a1.blocked.Load() != 0 || a2.blocked.Load() != 0 {
		t.Fatal("a consume loop is still running after shutdown")
	}
}

// SPEC-0002 REQ "Adapter Interface and Trust Mode", scenario "Disabled adapter does not consume":
// a disabled registry row parks the poll loop, re-enabling resumes it, and disabling mid-consume
// cancels the in-flight consume via the flag watcher.
func TestEnabledFlagGatesConsume(t *testing.T) {
	reg := newFakeRegistry()
	// Pre-register disabled: the operator's kill switch is set before the process starts, and
	// runner registration must preserve it.
	reg.rows["fake"] = store.Adapter{Name: "fake", Enabled: false}

	r := New(reg, testLogger(), fastOpts())
	a := &fakeAdapter{name: "fake"}
	if err := r.Add(a, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startRunner(t, r, ctx)

	// Parked: several enabled-check intervals pass without a single consume.
	time.Sleep(20 * time.Millisecond)
	if n := a.consumes.Load(); n != 0 {
		t.Fatalf("disabled adapter consumed %d times; must not consume", n)
	}

	// Flip on: the loop resumes and consumes.
	reg.setEnabled("fake", true)
	waitFor(t, "consume after enable", func() bool { return a.blocked.Load() == 1 })

	// Flip off mid-consume: the watcher cancels the in-flight consume within EnabledInterval and
	// the loop parks again instead of re-consuming.
	reg.setEnabled("fake", false)
	waitFor(t, "in-flight consume cancelled by disable", func() bool { return a.blocked.Load() == 0 })
	settled := a.consumes.Load()
	time.Sleep(20 * time.Millisecond)
	if n := a.consumes.Load(); n != settled {
		t.Fatalf("disabled adapter consumed again (%d → %d)", settled, n)
	}

	cancel()
	<-done
}

// SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety", scenario "Broker error backs off, not
// crashes": transient consume errors are retried with backoff, health records the degraded run
// (consecutive failures, last error), and recovery clears it.
func TestBrokerErrorBacksOffAndRecovers(t *testing.T) {
	reg := newFakeRegistry()
	r := New(reg, testLogger(), fastOpts())
	a := &fakeAdapter{name: "fake"}
	a.failures.Store(3)
	if err := r.Add(a, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startRunner(t, r, ctx)

	// The loop survives the failures and reaches a healthy blocked consume.
	waitFor(t, "recovery after transient errors", func() bool { return a.blocked.Load() == 1 })
	if n := a.consumes.Load(); n != 4 {
		t.Fatalf("consume attempts = %d, want 4 (3 failures + 1 recovery)", n)
	}

	// Health reflects the degraded run: three failing polls recorded, failures counted up.
	polls := reg.pollLog("fake")
	if len(polls) != 3 {
		t.Fatalf("recorded polls = %v, want 3 failing attempts", polls)
	}
	for _, p := range polls {
		if p == "" {
			t.Fatalf("failing attempt recorded as healthy: %v", polls)
		}
	}
	hs := r.Health()
	if len(hs) != 1 || hs[0].Name != "fake" {
		t.Fatalf("health snapshot: %+v", hs)
	}
	if hs[0].ConsecutiveFailures != 3 || hs[0].LastError == "" || hs[0].LastPollAt.IsZero() {
		t.Fatalf("degraded health not tracked: %+v", hs[0])
	}

	// Shutdown ends the healthy consume; that final attempt records as healthy (context.Canceled
	// is a shutdown, not a broker failure) and resets the failure count.
	cancel()
	<-done
	hs = r.Health()
	if hs[0].ConsecutiveFailures != 0 || hs[0].LastError != "" {
		t.Fatalf("healthy consume must clear degraded state: %+v", hs[0])
	}
}

// A transiently-failing registry (database blip during startup registration) backs off and
// retries rather than killing the worker.
func TestRegistrationRetries(t *testing.T) {
	reg := newFakeRegistry()
	reg.regErrs = 2
	r := New(reg, testLogger(), fastOpts())
	a := &fakeAdapter{name: "fake"}
	if err := r.Add(a, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startRunner(t, r, ctx)
	waitFor(t, "consume after registration retries", func() bool { return a.blocked.Load() == 1 })

	cancel()
	<-done
}

// The adapter registry is concurrency-safe: Add works before and after Run, duplicates are
// rejected, and Health may be read concurrently with running workers (exercised under -race).
func TestRegistryConcurrencySafety(t *testing.T) {
	reg := newFakeRegistry()
	r := New(reg, testLogger(), fastOpts())
	early := &fakeAdapter{name: "early"}
	if err := r.Add(early, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(&fakeAdapter{name: "early"}, nopSink{}, nil); err == nil {
		t.Fatal("duplicate Add must error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startRunner(t, r, ctx)
	waitFor(t, "early adapter consuming", func() bool { return early.blocked.Load() == 1 })

	// Concurrent late Adds + Health reads while workers run.
	late := &fakeAdapter{name: "late"}
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 50 {
				for _, h := range r.Health() {
					_ = h.ConsecutiveFailures
				}
			}
		}()
	}
	if err := r.Add(late, nopSink{}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "late-added adapter consuming", func() bool { return late.blocked.Load() == 1 })
	readers.Wait()

	if err := r.Run(ctx); err == nil {
		t.Fatal("second Run must error")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel with late-added worker")
	}
}
