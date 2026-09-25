package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/switchboard/internal/store"
)

// fakeQueueStats stands in for *store.Store. block waits out the collector's deadline and returns
// its error, as pgx does; late ignores the deadline entirely and answers after it has passed.
type fakeQueueStats struct {
	stats []store.QueueStat
	err   error
	block bool
	late  time.Duration
	calls atomic.Int64
}

func (f *fakeQueueStats) QueueStats(ctx context.Context) ([]store.QueueStat, error) {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.late > 0 {
		time.Sleep(f.late)
	}
	return f.stats, f.err
}

// gatherQueueSeries gathers reg and flattens every counter and gauge sample into
// `name{label="value",...}` → value, labels in the registry's sorted order. A Gather error — two
// series with one label set, a sample that does not match its descriptor — fails the test.
func gatherQueueSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]float64)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var b strings.Builder
			b.WriteString(mf.GetName())
			if lps := m.GetLabel(); len(lps) > 0 {
				b.WriteByte('{')
				for i, lp := range lps {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "%s=%q", lp.GetName(), lp.GetValue())
				}
				b.WriteByte('}')
			}
			// The getters are nil-safe, so a gauge reads 0 from GetCounter and vice versa.
			out[b.String()] = m.GetGauge().GetValue() + m.GetCounter().GetValue()
		}
	}
	return out
}

// queueCollectionErrors reads switchboard_metrics_collection_errors_total{collector="queue"} on its
// own registry. The main registry gathers collectors concurrently, so a failed scrape's increment
// may land after that same scrape already read the counter; reading it after Gather has returned —
// every Collect finished — is deterministic. Returns -1 when the series does not exist.
func queueCollectionErrors(t *testing.T, m *Metrics) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.collectionErrors)
	v, ok := gatherQueueSeries(t, reg)[`switchboard_metrics_collection_errors_total{collector="queue"}`]
	if !ok {
		return -1
	}
	return v
}

// queueFamilySeries returns the series of every family the queue collector emits: switchboard_queue_*
// and switchboard_quarantine_oldest_seconds.
func queueFamilySeries(series map[string]float64) []string {
	var out []string
	for k := range series {
		if strings.HasPrefix(k, "switchboard_queue_") || strings.HasPrefix(k, "switchboard_quarantine_oldest_seconds") {
			out = append(out, k)
		}
	}
	return out
}

// wantSeries asserts each named series is present with exactly the given value.
func wantSeries(t *testing.T, series map[string]float64, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		got, ok := series[k]
		if !ok {
			t.Errorf("series %s absent, want %v", k, v)
			continue
		}
		if got != v {
			t.Errorf("series %s = %v, want %v", k, got, v)
		}
	}
}

// The incident shape — work waiting on forge and nothing holding it — produces exactly the series
// the alert `pending > 0 and ignoring(state) claimed == 0` reads, and a queue that is only scoped
// on an endpoint reports every state at zero.
// Governing: SPEC-0023 REQ-2 "Queue liveness — the mandatory pair"; design.md "Zero-value series".
func TestQueueCollectorEmitsEveryStateForEveryKnownQueue(t *testing.T) {
	m := New(Options{})
	m.RegisterQueueStats(&fakeQueueStats{stats: []store.QueueStat{
		{Queue: "deploys"},
		{Queue: "forge", Pending: 50, OldestPendingSeconds: 72000},
	}})
	series := gatherQueueSeries(t, m.Registry())
	wantSeries(t, series, map[string]float64{
		`switchboard_queue_todos{queue="forge",state="pending"}`:         50,
		`switchboard_queue_todos{queue="forge",state="claimed"}`:         0,
		`switchboard_queue_todos{queue="forge",state="done"}`:            0,
		`switchboard_queue_todos{queue="forge",state="failed"}`:          0,
		`switchboard_queue_oldest_pending_seconds{queue="forge"}`:        72000,
		`switchboard_queue_todos{queue="deploys",state="pending"}`:       0,
		`switchboard_queue_todos{queue="deploys",state="claimed"}`:       0,
		`switchboard_queue_todos{queue="deploys",state="done"}`:          0,
		`switchboard_queue_todos{queue="deploys",state="failed"}`:        0,
		`switchboard_queue_oldest_pending_seconds{queue="deploys"}`:      0,
		`switchboard_metrics_collection_errors_total{collector="queue"}`: 0,
	})
	if got := len(queueFamilySeries(series)); got != 2*(len(queueStates)+1) {
		t.Errorf("queue families carry %d series, want %d (two queues × four states + oldest)", got, 2*(len(queueStates)+1))
	}
}

// When the read fails or overruns its deadline, NEITHER gauge family appears — no zeros, no cached
// values — and the collector's error counter increments once per failed scrape. The rest of the
// scrape (here the Go collectors) is unaffected.
// Governing: SPEC-0023 REQ-6 "Honest absence"; design.md "Shape" (timeout, omit on failure).
func TestQueueCollectorFailureOmitsBothFamilies(t *testing.T) {
	cases := map[string]*fakeQueueStats{
		"error":   {err: errors.New("connection refused")},
		"timeout": {block: true},
		// Rows that arrive after the deadline are a timeout, not a reading.
		"late answer": {late: 60 * time.Millisecond, stats: []store.QueueStat{{Queue: "forge", Pending: 1}}},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			m := New(Options{})
			m.RegisterQueueStats(src, WithQueueStatsTimeout(20*time.Millisecond))
			for scrape := 1; scrape <= 2; scrape++ {
				start := time.Now()
				series := gatherQueueSeries(t, m.Registry())
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Fatalf("scrape %d took %v; the deadline did not bound it", scrape, elapsed)
				}
				if got := queueFamilySeries(series); len(got) != 0 {
					t.Fatalf("scrape %d: failed collector still emitted %v; want both families omitted", scrape, got)
				}
				if got := queueCollectionErrors(t, m); got != float64(scrape) {
					t.Errorf("scrape %d: collection errors = %v, want %d", scrape, got, scrape)
				}
				if _, ok := series["go_goroutines"]; !ok {
					t.Errorf("scrape %d: go_goroutines missing; one failed collector must not blank the scrape", scrape)
				}
			}
		})
	}
}

// Recovery after a failure serves fresh readings again; nothing from the failed scrape lingers.
// Governing: SPEC-0023 REQ-6 "Honest absence".
func TestQueueCollectorRecoversAfterFailure(t *testing.T) {
	src := &fakeQueueStats{err: errors.New("db down")}
	m := New(Options{})
	m.RegisterQueueStats(src)
	if got := queueFamilySeries(gatherQueueSeries(t, m.Registry())); len(got) != 0 {
		t.Fatalf("failed scrape emitted %v", got)
	}
	src.err, src.stats = nil, []store.QueueStat{{Queue: "forge", Claimed: 2}}
	wantSeries(t, gatherQueueSeries(t, m.Registry()), map[string]float64{
		`switchboard_queue_todos{queue="forge",state="claimed"}`: 2,
		`switchboard_queue_todos{queue="forge",state="pending"}`: 0,
	})
	if got := queueCollectionErrors(t, m); got != 1 {
		t.Errorf("collection errors = %v after one failed and one good scrape, want 1", got)
	}
}

// Known queues past the cap collapse into queue="__other__": counts summed per state, the oldest
// age the maximum across the folded queues, and never two series with one label set — including
// when an operator named a queue "__other__" outright.
// Governing: SPEC-0023 REQ-5 "Cardinality"; design.md "Cardinality control".
func TestQueueCollectorOverflowSumsIntoOther(t *testing.T) {
	m := New(Options{QueueCap: 2})
	m.RegisterQueueStats(&fakeQueueStats{stats: []store.QueueStat{
		{Queue: "__other__", Pending: 1, OldestPendingSeconds: 40},
		{Queue: "alpha", Pending: 1, Claimed: 2, Done: 3, Failed: 4, OldestPendingSeconds: 10},
		{Queue: "beta", Pending: 1, OldestPendingSeconds: 5},
		{Queue: "delta", Done: 5, Failed: 1},
		{Queue: "gamma", Pending: 2, Claimed: 1, OldestPendingSeconds: 30},
	}})
	series := gatherQueueSeries(t, m.Registry())
	wantSeries(t, series, map[string]float64{
		`switchboard_queue_todos{queue="alpha",state="failed"}`:       4,
		`switchboard_queue_oldest_pending_seconds{queue="alpha"}`:     10,
		`switchboard_queue_todos{queue="beta",state="pending"}`:       1,
		`switchboard_queue_todos{queue="__other__",state="pending"}`:  3, // "__other__" 1 + gamma 2
		`switchboard_queue_todos{queue="__other__",state="claimed"}`:  1,
		`switchboard_queue_todos{queue="__other__",state="done"}`:     5,
		`switchboard_queue_todos{queue="__other__",state="failed"}`:   1,
		`switchboard_queue_oldest_pending_seconds{queue="__other__"}`: 40,
	})
	for _, k := range []string{"gamma", "delta"} {
		if _, ok := series[`switchboard_queue_oldest_pending_seconds{queue="`+k+`"}`]; ok {
			t.Errorf("queue %q past the cap kept its own series", k)
		}
	}
	if got := len(queueFamilySeries(series)); got != 3*(len(queueStates)+1) {
		t.Errorf("queue families carry %d series, want %d (alpha, beta, __other__)", got, 3*(len(queueStates)+1))
	}
}

// The gauges label through the SAME sticky limiter as the lifecycle counters, so a queue the
// counters admitted first keeps its own label here and a queue that arrived after the cap reads
// "__other__" in both — a PromQL join across the families lines up.
// Governing: SPEC-0023 REQ-5 "Cardinality".
func TestQueueCollectorSharesQueueLimiterWithCounters(t *testing.T) {
	m := New(Options{QueueCap: 1})
	m.TodoClaimed("zeta", 1) // the counters admit zeta first and fill the cap
	m.RegisterQueueStats(&fakeQueueStats{stats: []store.QueueStat{
		{Queue: "alpha", Pending: 1},
		{Queue: "zeta", Pending: 2},
	}})
	wantSeries(t, gatherQueueSeries(t, m.Registry()), map[string]float64{
		`switchboard_queue_todos{queue="zeta",state="pending"}`:      2,
		`switchboard_queue_todos{queue="__other__",state="pending"}`: 1,
		`switchboard_todos_claimed_total{queue="zeta"}`:              1,
	})
}

// A nil *Metrics or a nil source registers nothing and does not panic.
func TestRegisterQueueStatsNilSafe(t *testing.T) {
	var nilM *Metrics
	nilM.RegisterQueueStats(&fakeQueueStats{})

	m := New(Options{})
	m.RegisterQueueStats(nil)
	series := gatherQueueSeries(t, m.Registry())
	if got := queueFamilySeries(series); len(got) != 0 {
		t.Errorf("nil source emitted %v", got)
	}
	if _, ok := series[`switchboard_metrics_collection_errors_total{collector="queue"}`]; ok {
		t.Error("nil source initialised the queue error series; nothing was registered")
	}
}

// Concurrent scrapes share the collector and the limiter; run under -race.
func TestQueueCollectorConcurrentScrapes(t *testing.T) {
	m := New(Options{QueueCap: 3})
	stats := make([]store.QueueStat, 0, 8)
	for i := range 8 {
		stats = append(stats, store.QueueStat{Queue: fmt.Sprintf("q%d", i), Pending: int64(i)})
	}
	src := &fakeQueueStats{stats: stats}
	m.RegisterQueueStats(src)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				m.TodoCreated("q7", "api")
				if _, err := m.Registry().Gather(); err != nil {
					t.Errorf("gather: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := src.calls.Load(); got != 80 {
		t.Errorf("source read %d times, want once per scrape (80)", got)
	}
	series := gatherQueueSeries(t, m.Registry())
	if got := len(queueFamilySeries(series)); got != 4*(len(queueStates)+1) {
		t.Errorf("queue families carry %d series, want %d (three admitted + __other__)", got, 4*(len(queueStates)+1))
	}
}
