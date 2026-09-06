package server

// Reaper loop-wiring tests for endpoint credential-lifetime enforcement, against a fake store (no
// database): every tick sweeps expired endpoints, each flipped endpoint's live MCP sessions are
// closed through the closeSessions seam (the same CloseEndpointSessions path a web-UI revoke
// rings), a store error is logged without killing the loop, and the loop exits on context
// cancellation. The UPDATE semantics themselves (state flip, revoked_at, idempotency) are covered
// by the DB-backed tests in internal/store/endpoint_expiry_test.go.
// Governing: SPEC-0016 REQ "Credential Lifetime" (scenario "Expiry enforcement"), ADR-0019,
// SPEC-0007 revocation semantics reused.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// fakeReapStore scripts ExpireEndpoints results per call and records everything; the todo-lease
// verbs are inert so these tests bind only to the expiry wiring.
type fakeReapStore struct {
	mu      sync.Mutex
	calls   int
	expired [][]string     // expired[i] returned on call i (nil past the end)
	ring    [][]store.Todo // ring[i] returned by RingUnclaimed on call i
	errs    []error        // errs[i] returned on call i (nil past the end)
}

func (f *fakeReapStore) ReapExpired(ctx context.Context) (int64, error)       { return 0, nil }
func (f *fakeReapStore) RequeueDueRetries(ctx context.Context) (int64, error) { return 0, nil }

// rung is what the heartbeat sweep hands back for re-publishing, scripted per call.
func (f *fakeReapStore) RingUnclaimed(ctx context.Context) ([]store.Todo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ring) == 0 {
		return nil, nil
	}
	out := f.ring[0]
	f.ring = f.ring[1:]
	return out, nil
}

func (f *fakeReapStore) ExpireEndpoints(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.expired) {
		return f.expired[i], nil
	}
	return nil, nil
}

func (f *fakeReapStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// closeRecorder captures the endpoint ids the reaper asked to tear sessions down for.
type closeRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (c *closeRecorder) close(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
}

func (c *closeRecorder) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

// startReaper runs reaper in a goroutine and returns a channel closed when it exits.
func startReaper(ctx context.Context, st reapStore, log *slog.Logger, closeSessions func(string), ringFn func(store.Todo), interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		reaper(ctx, st, log, closeSessions, ringFn, interval)
	}()
	return done
}

// TestReaperClosesSessionsForExpiredEndpoints: ids returned by the expiry sweep are each handed to
// closeSessions — expiry tears down live sessions through the same path as a revoke — and the loop
// keeps ticking afterwards, then exits promptly on cancellation.
func TestReaperClosesSessionsForExpiredEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeReapStore{expired: [][]string{{"ep-1", "ep-2"}}}
	rec := &closeRecorder{}
	log := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))

	done := startReaper(ctx, fake, log, rec.close, nil, 2*time.Millisecond)
	waitFor(t, func() bool { return len(rec.snapshot()) >= 2 }, "reaper never closed sessions for the expired endpoints")
	waitFor(t, func() bool { return fake.count() >= 3 }, "reaper stopped ticking after an expiry sweep")

	got := rec.snapshot()
	if got[0] != "ep-1" || got[1] != "ep-2" {
		t.Fatalf("closed sessions for %v; want [ep-1 ep-2] in sweep order", got[:2])
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not exit after context cancellation")
	}
}

// TestReaperSurvivesExpirySweepErrors: a failed sweep is logged and does NOT kill the loop —
// enforcement resumes on later ticks, so a transient DB failure can never disable credential
// lifetimes for the life of the process (mirroring the pruner's contract).
func TestReaperSurvivesExpirySweepErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeReapStore{
		errs:    []error{errors.New("db hiccup")},
		expired: [][]string{nil, {"ep-late"}}, // first call errors; the id lands on a later tick
	}
	rec := &closeRecorder{}
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	done := startReaper(ctx, fake, log, rec.close, nil, 2*time.Millisecond)
	waitFor(t, func() bool { return len(rec.snapshot()) >= 1 }, "reaper never recovered after a failed expiry sweep")
	cancel()
	<-done

	if !strings.Contains(buf.String(), "endpoint expiry") {
		t.Fatalf("expected a warn log for the failed expiry sweep; got: %q", buf.String())
	}
	if got := rec.snapshot(); got[0] != "ep-late" {
		t.Fatalf("post-error sweep closed %v; want [ep-late]", got)
	}
}

// TestReaperNilCloseSessionsIsSafe: wiring without a session-closer (as a future caller might) must
// not panic — the store flip alone already kills the credential at auth.
func TestReaperNilCloseSessionsIsSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeReapStore{expired: [][]string{{"ep-1"}}}
	log := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))

	done := startReaper(ctx, fake, log, nil, nil, 2*time.Millisecond)
	waitFor(t, func() bool { return fake.count() >= 2 }, "reaper with nil closeSessions stopped ticking")
	cancel()
	<-done
}
