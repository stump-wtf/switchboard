package server

// Loop-wiring tests for the dependency recheck sweep (ADR-0055, stump.wtf/switchboard#583), against a
// fake rechecker (no database, no provider): the first pass runs immediately so a restart does not
// leave holds unread for a whole interval, ticks keep passing, a Recheck error is logged without
// killing the loop, and the loop exits on context cancellation. The pass semantics themselves are
// covered by internal/deps.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/deps"
)

type fakeRechecker struct {
	mu    sync.Mutex
	calls int
	errs  []error // errs[i] returned on call i (nil past the end)
}

func (f *fakeRechecker) Recheck(_ context.Context) (deps.RecheckReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return deps.RecheckReport{}, f.errs[i]
	}
	return deps.RecheckReport{}, nil
}

func (f *fakeRechecker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The immediate first pass plus at least one tick, and cancellation ends the loop.
func TestRecheckDependentsTicksAndStops(t *testing.T) {
	f := &fakeRechecker{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		recheckDependents(ctx, f, 10*time.Millisecond, quietLog())
	}()
	deadline := time.After(2 * time.Second)
	for f.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d passes in 2s at a 10ms interval", f.count())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit on cancellation")
	}
}

// A store or provider error mid-sweep is a warning, not a death: the next tick still passes.
func TestRecheckDependentsSurvivesErrors(t *testing.T) {
	f := &fakeRechecker{errs: []error{errors.New("boom")}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		recheckDependents(ctx, f, 10*time.Millisecond, quietLog())
	}()
	deadline := time.After(2 * time.Second)
	for f.count() < 2 {
		select {
		case <-deadline:
			t.Fatalf("loop died after the first error: %d passes", f.count())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}
