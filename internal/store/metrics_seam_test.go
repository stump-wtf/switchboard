package store

// Lifecycle metrics seam tests
//
// The seam is what Story 3's increments call unconditionally, so its contract is pinned here
// without a database: unset is a no-op, SetMetrics routes to the sink, nil clears it, and a
// concurrent SetMetrics never races an in-flight increment. This file deliberately does not import
// internal/metrics (that package reads the store; see metrics.go).
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"sync"
	"sync/atomic"
	"testing"
)

type countingMetrics struct{ created, claimed, finished, expired atomic.Int64 }

func (c *countingMetrics) TodoCreated(string, string)  { c.created.Add(1) }
func (c *countingMetrics) TodoClaimed(string, int)     { c.claimed.Add(1) }
func (c *countingMetrics) TodoFinished(string, string) { c.finished.Add(1) }
func (c *countingMetrics) LeaseExpired(string)         { c.expired.Add(1) }

func TestStoreMetricsSeamUnsetIsNoOp(t *testing.T) {
	s := New(nil)
	m := s.metricsOrNop()
	if m == nil {
		t.Fatal("metricsOrNop returned nil; call sites increment unconditionally")
	}
	m.TodoCreated("q", "github")
	m.TodoClaimed("q", 1)
	m.TodoFinished("q", "complete")
	m.LeaseExpired("q")
}

func TestStoreMetricsSeamRoutesAndClears(t *testing.T) {
	s := New(nil)
	c := &countingMetrics{}
	s.SetMetrics(c)
	s.metricsOrNop().TodoCreated("q", "github")
	s.metricsOrNop().TodoClaimed("q", 2)
	s.metricsOrNop().TodoFinished("q", "fail")
	s.metricsOrNop().LeaseExpired("q")
	if c.created.Load() != 1 || c.claimed.Load() != 1 || c.finished.Load() != 1 || c.expired.Load() != 1 {
		t.Fatalf("sink saw created=%d claimed=%d finished=%d expired=%d, want 1 each",
			c.created.Load(), c.claimed.Load(), c.finished.Load(), c.expired.Load())
	}
	s.SetMetrics(nil)
	s.metricsOrNop().TodoCreated("q", "github")
	if c.created.Load() != 1 {
		t.Fatal("SetMetrics(nil) did not clear the sink")
	}
}

// TestStoreMetricsSeamConcurrentSet: wiring may race traffic; the atomic slot must make that safe.
// Run with -race.
func TestStoreMetricsSeamConcurrentSet(t *testing.T) {
	s := New(nil)
	c := &countingMetrics{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 200 {
				if i == 0 {
					s.SetMetrics(c)
				}
				s.metricsOrNop().TodoClaimed("q", 1)
			}
		}(i)
	}
	wg.Wait()
}
