package metrics

// Queue liveness collector
//
// switchboard_queue_todos{queue,state} and switchboard_queue_oldest_pending_seconds{queue} are the
// mandatory pair: together they make "work is waiting and nothing holds it" a series an alert can
// read. They are computed at scrape time from one aggregate read of the store, never maintained
// inline — an inline gauge drifts the first time a transition path forgets to adjust it, and a
// permanently wrong graph that looks plausible is worse than none (design.md "Shape").
//
// Every known queue reports all four states, zeros included, because the alert condition is
// literally claimed == 0 and PromQL reads a zero far more cleanly than an absent series. Queue
// labels pass through the shared, sticky queue limiter, so a queue reads the same here as on the
// lifecycle counters; queues past the cap are summed into queue="__other__", whose oldest-pending
// age is the maximum across the queues folded into it (REQ-5).
//
// The read runs under a deadline shorter than any sane scrape interval. On error or timeout the
// collector emits NEITHER family — no zeros, no cached values — and increments
// switchboard_metrics_collection_errors_total{collector="queue"}, so a broken collector is itself
// visible instead of flattening the graph to the exact reading this surface exists to prevent
// (REQ-6).
//
// Governing: SPEC-0023 REQ-2 "Queue liveness — the mandatory pair", REQ-5 "Cardinality", REQ-6
// "Honest absence"; design.md "Shape", "Zero-value series", "Cardinality control"; ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 2).

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/switchboard/internal/store"
)

// DefaultQueueStatsTimeout bounds one scrape's queue-liveness read. It sits well under the 15–60s
// scrape intervals design.md assumes, so a slow database costs one omitted reading, not a pile of
// overlapping scrapes.
const DefaultQueueStatsTimeout = 5 * time.Second

// queueCollectorName is the collector label the queue-liveness collector reports failures under.
const queueCollectorName = "queue"

// queueStates is the state label domain REQ-2 enumerates, in exposition order. The A2A states the
// todos table also admits are deliberately absent.
var queueStates = [...]string{"pending", "claimed", "done", "failed"}

// QueueStatsSource is the one read the collector needs. *store.Store satisfies it; tests fake it.
type QueueStatsSource interface {
	// QueueStats returns a zero-filled row per known queue. It must honour ctx's deadline.
	QueueStats(ctx context.Context) ([]store.QueueStat, error)
}

// QueueOption configures RegisterQueueStats.
type QueueOption func(*queueCollector)

// WithQueueStatsTimeout overrides DefaultQueueStatsTimeout. Non-positive values keep the default.
func WithQueueStatsTimeout(d time.Duration) QueueOption {
	return func(c *queueCollector) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// RegisterQueueStats registers the queue-liveness collector over src on the dedicated registry and
// pre-creates its collection-error series at zero, so an increase() alert over it has a baseline
// from the first scrape. Call it once per *Metrics: a second call panics on the duplicate
// descriptors, like any MustRegister. A nil receiver or nil src is a no-op.
func (m *Metrics) RegisterQueueStats(src QueueStatsSource, opts ...QueueOption) {
	if m == nil || src == nil {
		return
	}
	c := &queueCollector{
		src:     src,
		m:       m,
		timeout: DefaultQueueStatsTimeout,
		todos: prometheus.NewDesc("switchboard_queue_todos",
			"Todos on each known queue by state (pending|claimed|done|failed), computed at scrape time; every known queue reports every state, zeros included.",
			[]string{"queue", "state"}, nil),
		oldest: prometheus.NewDesc("switchboard_queue_oldest_pending_seconds",
			"Age in seconds of the oldest pending todo on each known queue, computed at scrape time; 0 when none is pending.",
			[]string{"queue"}, nil),
	}
	for _, opt := range opts {
		opt(c)
	}
	m.InitCollectionErrors(queueCollectorName)
	m.reg.MustRegister(c)
}

// queueCollector is the scrape-time prometheus.Collector behind the REQ-2 gauges. It holds no
// mutable state, so concurrent scrapes are safe; the shared queue limiter is itself concurrency-safe.
type queueCollector struct {
	src     QueueStatsSource
	m       *Metrics
	timeout time.Duration
	todos   *prometheus.Desc
	oldest  *prometheus.Desc
}

// Describe implements prometheus.Collector. Both families are described up front so the registry
// checks every emitted series against them.
func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.todos
	ch <- c.oldest
}

// Collect implements prometheus.Collector: one bounded read, then either every series or none.
func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	// Collect carries no context, so the deadline is rooted here.
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	stats, err := c.src.QueueStats(ctx)
	if err == nil {
		// A source that ignores its deadline and answers late has still overrun the scrape budget;
		// treat it as the timeout it is rather than serving a reading the scraper may have dropped.
		err = ctx.Err()
	}
	var out []prometheus.Metric
	if err == nil {
		out, err = c.build(stats)
	}
	if err != nil {
		// All or nothing: out is only sent once every series built, so a failure never leaves a
		// partial family that reads as a subset of queues sitting at zero. The registry gathers
		// collectors concurrently, so this increment shows on this scrape or, at the latest, the next.
		c.m.CollectionError(queueCollectorName)
		if c.m.log != nil {
			c.m.log.Warn("metrics: queue liveness collector failed; gauges omitted", "err", err)
		}
		return
	}
	for _, metric := range out {
		ch <- metric
	}
}

// queueAgg accumulates one label value's reading: a single queue, or every queue folded into
// Other. counts is indexed like queueStates.
type queueAgg struct {
	counts [len(queueStates)]int64
	oldest float64
}

// build folds stats onto their limited labels and renders every series. Each label value appears
// exactly once per family however many queues map onto it, so the registry never sees two series
// with identical label sets.
func (c *queueCollector) build(stats []store.QueueStat) ([]prometheus.Metric, error) {
	byLabel := make(map[string]*queueAgg, len(stats))
	order := make([]string, 0, len(stats))
	for _, s := range stats {
		label := c.m.QueueLabel(s.Queue)
		a, ok := byLabel[label]
		if !ok {
			a = &queueAgg{}
			byLabel[label] = a
			order = append(order, label)
		}
		for i, n := range [len(queueStates)]int64{s.Pending, s.Claimed, s.Done, s.Failed} { // queueStates order
			a.counts[i] += n
		}
		a.oldest = max(a.oldest, s.OldestPendingSeconds)
	}
	out := make([]prometheus.Metric, 0, len(order)*(len(queueStates)+1))
	for _, label := range order {
		a := byLabel[label]
		for i, state := range queueStates {
			metric, err := prometheus.NewConstMetric(c.todos, prometheus.GaugeValue, float64(a.counts[i]), label, state)
			if err != nil {
				return nil, err
			}
			out = append(out, metric)
		}
		metric, err := prometheus.NewConstMetric(c.oldest, prometheus.GaugeValue, a.oldest, label)
		if err != nil {
			return nil, err
		}
		out = append(out, metric)
	}
	return out, nil
}
