package server

// Governing: SPEC-0004 REQ "Hybrid Retention and Bounded Growth" — a periodic pruning task MUST
// invoke the hybrid age + row-cap retention. These tests cover the LOOP WIRING against a fake
// store (no database): the pruner runs at startup and on ticks, logs structured counts, survives
// store errors, and exits on context cancellation (graceful shutdown, ADR-0002). The age/row-cap
// DELETE semantics themselves are covered by the DB-backed tests in internal/store/retention_test.go.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// fakePruneStore counts Prune invocations and returns a scripted result/error per call.
type fakePruneStore struct {
	mu    sync.Mutex
	calls int
	res   store.PruneResult
	errs  []error // errs[i] returned on call i (nil past the end)
}

func (f *fakePruneStore) Prune(ctx context.Context) (store.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return store.PruneResult{}, f.errs[i]
	}
	return f.res, nil
}

func (f *fakePruneStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// startPruner runs pruner in a goroutine and returns a channel closed when it exits.
func startPruner(ctx context.Context, st pruneStore, log *slog.Logger, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		pruner(ctx, st, log, interval)
	}()
	return done
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}

// The pruner invokes Prune once immediately at startup, then again on each tick, and exits
// promptly when the context is cancelled — no goroutine outlives shutdown.
func TestPrunerRunsAtStartupAndOnTicksThenStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakePruneStore{}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	done := startPruner(ctx, fake, log, 2*time.Millisecond)
	// >= 3 calls proves both the startup prune and the periodic ticks fire.
	waitFor(t, func() bool { return fake.count() >= 3 }, "pruner never reached 3 Prune calls (startup + ticks)")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pruner did not exit after context cancellation")
	}
}

// A Prune error is logged and does NOT kill the loop: retention keeps being enforced on later
// ticks despite a transient store failure.
func TestPrunerSurvivesPruneErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakePruneStore{errs: []error{errors.New("db hiccup")}} // first call fails, rest succeed
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	done := startPruner(ctx, fake, log, 2*time.Millisecond)
	waitFor(t, func() bool { return fake.count() >= 3 }, "pruner stopped looping after a Prune error")
	cancel()
	<-done

	if !strings.Contains(buf.String(), "retention prune failed") {
		t.Fatalf("expected a warn log for the failed prune; got: %q", buf.String())
	}
}

// A non-empty PruneResult is logged with structured per-surface counts; an all-zero (no-op) result
// stays silent so an idle deployment does not emit an hourly log line.
func TestPrunerLogsStructuredCountsOnlyWhenWorkWasDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakePruneStore{res: store.PruneResult{EventsAged: 3, EventsCapped: 2, TodosAged: 1, TodosCapped: 4}}
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	done := startPruner(ctx, fake, log, 2*time.Millisecond)
	waitFor(t, func() bool { return fake.count() >= 1 }, "pruner never invoked Prune")
	cancel()
	<-done

	out := buf.String()
	for _, want := range []string{"retention pruned", "events_aged=3", "events_capped=2", "todos_aged=1", "todos_capped=4", "total=10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q; got: %q", want, out)
		}
	}

	// No-op path: zero result must not log.
	buf2 := syncBuffer{}
	quiet := &fakePruneStore{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := startPruner(ctx2, quiet, slog.New(slog.NewTextHandler(&buf2, nil)), 2*time.Millisecond)
	waitFor(t, func() bool { return quiet.count() >= 1 }, "no-op pruner never invoked Prune")
	cancel2()
	<-done2
	if got := buf2.String(); strings.Contains(got, "retention pruned") {
		t.Fatalf("no-op prune must not log a pruned line; got: %q", got)
	}
}

// A context cancelled mid-Prune (the shutdown race) exits without logging the cancellation as a
// store failure.
func TestPrunerDoesNotLogShutdownCancellationAsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the startup Prune sees ctx.Err() != nil
	fake := &fakePruneStore{errs: []error{context.Canceled}}
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	done := startPruner(ctx, fake, log, time.Hour)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pruner did not exit on pre-cancelled context")
	}
	if strings.Contains(buf.String(), "retention prune failed") {
		t.Fatalf("shutdown cancellation must not be logged as a failure; got: %q", buf.String())
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer: the pruner goroutine writes logs while the test
// goroutine reads them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
