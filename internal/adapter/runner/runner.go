// Package runner owns the poll-loop lifecycle for pull adapters (ADR-0014, SPEC-0002): each
// registered adapter runs as an explicitly-managed worker goroutine with a clean startup and a
// graceful shutdown. The runner registers every adapter in the adapters table on start, honors the
// operator's runtime enabled flag (a disabled adapter does not consume; flipping the flag off
// cancels an in-flight consume), backs off with jitter on transient broker errors instead of
// crashing the process, and stamps per-attempt health (last poll time + last error) both in memory
// and on the adapter's registry row.
//
// The runner sits on the lifecycle side of the adapter seam: transports (internal/adapter/redis)
// own consume + store-then-ack, the Sink (the enqueue coupling) owns the shared back half, and this
// package owns starting, supervising, and stopping the loops around them.
//
// Governing: ADR-0014 (adapter lifecycle), SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety".
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/joestump/switchboard/internal/adapter"
	"github.com/joestump/switchboard/internal/store"
)

// Registry is the narrow slice of the store the runner needs: adapter registration, the runtime
// enabled flag, and per-attempt health stamping. *store.Store satisfies it; tests fake it in memory.
type Registry interface {
	RegisterAdapter(ctx context.Context, name, family, trustMode string, config []byte) (store.Adapter, error)
	AdapterEnabled(ctx context.Context, name string) (bool, error)
	RecordAdapterPoll(ctx context.Context, name string, pollErr string) error
}

// Health is a point-in-time snapshot of one adapter's poll-loop health.
type Health struct {
	Name string
	// LastPollAt is when the loop last attempted a consume (zero until the first attempt).
	LastPollAt time.Time
	// LastError is the error text of the most recent failed attempt, empty while healthy.
	LastError string
	// ConsecutiveFailures counts consume attempts that have failed since the last healthy one; the
	// loop is degraded (backing off) while this is non-zero.
	ConsecutiveFailures int
}

// Options tunes the runner's supervision loops. The zero value gets production defaults; tests
// shrink the intervals to keep runs fast.
type Options struct {
	// BackoffMin is the first retry delay after a consume error. Default 1s.
	BackoffMin time.Duration
	// BackoffMax caps the exponential backoff. Default 30s. (SPEC-0002 leaves the policy specifics
	// to the implementation; exponential-with-jitter between these bounds is the choice here.)
	BackoffMax time.Duration
	// EnabledInterval is how often the loop re-checks the adapters.enabled flag — both while parked
	// disabled and, via a watcher, while a consume is in flight (so disabling an adapter cancels its
	// consume within this interval). Default 15s.
	EnabledInterval time.Duration
	// HealthTimeout bounds each RecordAdapterPoll write so a slow database cannot stall the poll
	// loop. Default 5s.
	HealthTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.BackoffMin <= 0 {
		o.BackoffMin = time.Second
	}
	if o.BackoffMax < o.BackoffMin {
		o.BackoffMax = 30 * time.Second
	}
	if o.EnabledInterval <= 0 {
		o.EnabledInterval = 15 * time.Second
	}
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = 5 * time.Second
	}
	return o
}

// worker pairs an adapter with the sink its consume loop feeds, plus the loop's health state.
type worker struct {
	adapter adapter.Adapter
	sink    adapter.Sink
	config  []byte // non-secret registry config (jsonb), may be nil

	mu     sync.Mutex
	health Health
}

// Runner supervises one worker goroutine per registered adapter. The registry is concurrency-safe:
// Add may be called before or after Run (a late Add starts its worker immediately under Run's
// context), and Health snapshots may be read from any goroutine.
type Runner struct {
	reg  Registry
	log  *slog.Logger
	opts Options

	mu      sync.Mutex
	workers map[string]*worker
	runCtx  context.Context // non-nil once Run has started; late Adds start under it
	wg      sync.WaitGroup
}

// New builds a Runner over the given registry.
func New(reg Registry, log *slog.Logger, opts Options) *Runner {
	return &Runner{reg: reg, log: log, opts: opts.withDefaults(), workers: map[string]*worker{}}
}

// Add registers an adapter (and the sink its messages feed) with the runner. config is the
// adapter's NON-SECRET registry config (jsonb; never connection secrets). Adding a duplicate name
// is an error. If the runner is already running, the worker starts immediately.
func (r *Runner) Add(a adapter.Adapter, sink adapter.Sink, config []byte) error {
	if a == nil || sink == nil {
		return errors.New("runner: adapter and sink are required")
	}
	name := a.Name()
	if name == "" {
		return errors.New("runner: adapter has no name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.workers[name]; dup {
		return fmt.Errorf("runner: adapter %q already registered", name)
	}
	w := &worker{adapter: a, sink: sink, config: config, health: Health{Name: name}}
	r.workers[name] = w
	if r.runCtx != nil && r.runCtx.Err() == nil {
		r.start(r.runCtx, w)
	}
	return nil
}

// Health returns a snapshot of every registered adapter's poll-loop health.
func (r *Runner) Health() []Health {
	r.mu.Lock()
	workers := make([]*worker, 0, len(r.workers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	r.mu.Unlock()
	out := make([]Health, 0, len(workers))
	for _, w := range workers {
		w.mu.Lock()
		out = append(out, w.health)
		w.mu.Unlock()
	}
	return out
}

// Run starts one supervised poll loop per registered adapter and blocks until ctx is cancelled AND
// every worker has returned — so the caller observes a shutdown with no leaked goroutines. Each
// adapter is upserted into the adapters registry before its loop starts (registration preserves the
// operator's enabled flag). Run may be called once.
//
// Governing: SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety" (scenario "Graceful shutdown
// stops consuming and abandons in-flight safely").
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.runCtx != nil {
		r.mu.Unlock()
		return errors.New("runner: Run called twice")
	}
	r.runCtx = ctx
	for _, w := range r.workers {
		r.start(ctx, w)
	}
	r.mu.Unlock()

	<-ctx.Done()
	r.wg.Wait()
	return ctx.Err()
}

// start launches one worker goroutine. Callers hold r.mu.
func (r *Runner) start(ctx context.Context, w *worker) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.work(ctx, w)
	}()
}

// work is one adapter's supervision loop: register → (enabled? consume : park) → record health →
// back off on error → repeat, until ctx is cancelled. Shared state (health, the registry map) is
// mutex-guarded; everything else is loop-local, so the loop is race-safe by construction.
func (r *Runner) work(ctx context.Context, w *worker) {
	name := w.adapter.Name()
	log := r.log.With("adapter", name)

	// Register the adapter's row so the enabled flag and health columns exist. Registration
	// preserves an operator's disabled flag (SPEC-0002 REQ "Adapter Interface and Trust Mode").
	// Transient failures back off and retry: a briefly-unreachable database must not kill the
	// worker (the same posture as broker errors).
	backoff := r.opts.BackoffMin
	for {
		if _, err := r.reg.RegisterAdapter(ctx, name, adapter.Family, adapter.TrustMode, w.config); err == nil {
			break
		} else if ctx.Err() != nil {
			return
		} else {
			log.Warn("adapter registration failed; backing off", "err", err, "backoff", backoff)
			if !r.sleep(ctx, backoff) {
				return
			}
			backoff = r.nextBackoff(backoff)
		}
	}

	backoff = r.opts.BackoffMin
	wasDisabled := false
	for {
		if ctx.Err() != nil {
			return
		}

		enabled, err := r.reg.AdapterEnabled(ctx, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			if ctx.Err() != nil {
				return
			}
			log.Warn("adapter enabled check failed; backing off", "err", err, "backoff", backoff)
			if !r.sleep(ctx, backoff) {
				return
			}
			backoff = r.nextBackoff(backoff)
			continue
		}
		if !enabled {
			// Disabled adapter MUST NOT consume (SPEC-0002 scenario "Disabled adapter does not
			// consume"). Park and re-check; log the transition once, not every interval.
			if !wasDisabled {
				log.Info("adapter disabled; poll loop parked", "recheck", r.opts.EnabledInterval)
				wasDisabled = true
			}
			if !r.sleep(ctx, r.opts.EnabledInterval) {
				return
			}
			continue
		}
		if wasDisabled {
			log.Info("adapter re-enabled; poll loop resuming")
			wasDisabled = false
		}

		// Consume under a sub-context a flag watcher can cancel, so disabling the adapter stops an
		// in-flight consume within EnabledInterval. The transport's own contract guarantees the
		// cancellation leaves in-flight-unstored messages un-acked for redelivery.
		start := time.Now()
		err = r.consume(ctx, w)
		r.recordPoll(ctx, w, log, err)

		switch {
		case ctx.Err() != nil:
			// Shutdown: the consume returned because the runner is stopping. Done.
			return
		case err == nil || errors.Is(err, context.Canceled):
			// A clean return or a disable-cancelled consume: loop back to the enabled check with
			// backoff reset. Transports block until cancellation or error, so a nil return that
			// comes back instantly is a misbehaving adapter — pace it at BackoffMin rather than
			// letting it spin a hot loop against the enabled check.
			backoff = r.opts.BackoffMin
			if time.Since(start) < r.opts.BackoffMin {
				if !r.sleep(ctx, r.opts.BackoffMin) {
					return
				}
			}
		default:
			// Transient broker/transport error: degraded mode. Back off and retry — never crash
			// the process (SPEC-0002 scenario "Broker error backs off, not crashes"). A consume
			// that ran longer than BackoffMax was healthy before this failure, so its retry starts
			// from BackoffMin again rather than compounding old failures.
			if time.Since(start) > r.opts.BackoffMax {
				backoff = r.opts.BackoffMin
			}
			w.mu.Lock()
			failures := w.health.ConsecutiveFailures
			w.mu.Unlock()
			log.Warn("adapter consume failed; backing off (degraded)",
				"err", err, "backoff", backoff, "consecutive_failures", failures)
			if !r.sleep(ctx, backoff) {
				return
			}
			backoff = r.nextBackoff(backoff)
		}
	}
}

// consume runs one Consume attempt under a cancellable sub-context, with a watcher goroutine that
// cancels it if the operator disables the adapter mid-flight. The watcher is joined before consume
// returns, so no goroutine outlives the attempt.
func (r *Runner) consume(ctx context.Context, w *worker) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		t := time.NewTicker(r.opts.EnabledInterval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				enabled, err := r.reg.AdapterEnabled(cctx, w.adapter.Name())
				if err == nil && !enabled {
					cancel() // runtime kill switch: stop consuming; un-stored messages stay un-acked
					return
				}
			}
		}
	}()

	err := w.adapter.Consume(cctx, w.sink)
	cancel()
	watch.Wait()
	return err
}

// recordPoll stamps the attempt's outcome on the in-memory health snapshot and, best-effort, on the
// adapter's registry row. The row write runs under its own bounded timeout, detached from ctx's
// cancellation so a shutdown-time attempt still records — but a dead database only costs
// HealthTimeout and a warning, never the loop.
func (r *Runner) recordPoll(ctx context.Context, w *worker, log *slog.Logger, pollErr error) {
	errText := ""
	if pollErr != nil && !errors.Is(pollErr, context.Canceled) {
		errText = pollErr.Error()
	}

	w.mu.Lock()
	w.health.LastPollAt = time.Now()
	w.health.LastError = errText
	if errText == "" {
		w.health.ConsecutiveFailures = 0
	} else {
		w.health.ConsecutiveFailures++
	}
	w.mu.Unlock()

	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.HealthTimeout)
	defer cancel()
	if err := r.reg.RecordAdapterPoll(hctx, w.adapter.Name(), errText); err != nil {
		log.Warn("recording adapter poll health failed", "err", err)
	}
}

// nextBackoff doubles the delay up to BackoffMax, with ±25% jitter so a fleet of adapters that
// failed together does not retry in lockstep.
func (r *Runner) nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > r.opts.BackoffMax {
		next = r.opts.BackoffMax
	}
	jitter := 1 + (rand.Float64()-0.5)/2 // 0.75 .. 1.25
	next = time.Duration(float64(next) * jitter)
	if next < r.opts.BackoffMin {
		next = r.opts.BackoffMin
	}
	if next > r.opts.BackoffMax {
		next = r.opts.BackoffMax
	}
	return next
}

// sleep waits for d or until ctx is cancelled, reporting whether the full wait elapsed.
func (r *Runner) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
