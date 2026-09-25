package notifyhook

// The delivery dispatcher: when a push-eligible todo becomes ready, POST one signed notification to
// each matching hook of its endpoint. Best effort by design. The queue is the ledger, so a dropped,
// failed or lost notification costs latency, never work: the todo stays pending and claimable.
//
//   - Trigger: the store's ready hook (store.SetTodoReadyHook), fired after commit and only for
//     todos that pass the SPEC-0011 sender gate, on creation and on requeue. Enqueue never blocks:
//     a full queue drops the notification and logs it.
//   - Match: the hook's endpoint must be live, the todo's queue must be in the endpoint's CURRENT
//     scope, and in the hook's queue filter when it has one.
//   - Dial: the URL is re-validated before every attempt, and the connection goes to an address from
//     that same resolution (no second lookup), with TLS verified against the URL's host, no proxy
//     and no redirects.
//   - Sign: Standard Webhooks, dual-signed during a rotation grace. The webhook-id is stable across
//     a notification's attempts; each attempt has its own timestamp and signature.
//   - Retry: at most 3 attempts, about 1s then 5s apart with jitter, for network errors, timeouts,
//     408, 429 and 5xx only.
//
// Nothing is persisted: a restart loses in-flight notifications and nothing else. No log line
// carries a secret, a signature or a URL's query string (URLs are logged only through RedactURL,
// and in practice only hook ids are).
//
// Governing: ADR-0029, SPEC-0024 REQ-1, REQ-3, REQ-4, REQ-5, REQ-6, REQ-7, REQ-12, REQ-13;
// design.md "Validation runs twice, and the dial is pinned", "A per-instance bounded queue, with no
// persistence".

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stump-wtf/switchboard/internal/buildinfo"
	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Fixed delivery constants (SPEC-0024 design.md "Fixed values").
const (
	DefaultQueueSize     = 1024
	DefaultWorkers       = 8
	DefaultAttemptTTL    = 5 * time.Second
	DefaultRatePerMinute = 120
	MaxAttempts          = 3
	// DisableAfter is how many failed deliveries in a row disable a hook (SPEC-0024 REQ-8).
	DisableAfter        = 10
	maxResponseRead     = 64 << 10
	secretSweepInterval = 10 * time.Minute
)

// DefaultBackoff is the wait before the second and third attempts; each is jittered ±20%.
var DefaultBackoff = []time.Duration{time.Second, 5 * time.Second}

// Sentinel errors classify a failed attempt (SPEC-0024 REQ-12), for health and metrics.
var (
	ErrSSRF      = errors.New("notifyhook: target rejected by the SSRF guard")
	ErrRedirect  = errors.New("notifyhook: receiver answered with a redirect")
	ErrTimeout   = errors.New("notifyhook: attempt timed out")
	ErrPermanent = errors.New("notifyhook: receiver refused the notification")
	ErrRetryable = errors.New("notifyhook: receiver failed; retryable")
)

// Attempt results, the bounded label set of SPEC-0024 REQ-11.
const (
	Result2xx          = "2xx"
	Result3xx          = "3xx"
	Result4xx          = "4xx"
	Result5xx          = "5xx"
	ResultTimeout      = "timeout"
	ResultNetwork      = "network"
	ResultTLS          = "tls"
	ResultRejectedSSRF = "rejected_ssrf"
)

// Store is the slice of the store the dispatcher reads. Every hook read is endpoint-scoped.
type Store interface {
	NotifyEndpointForDispatch(ctx context.Context, endpointID string) (store.NotifyEndpoint, error)
	ListNotifyHooks(ctx context.Context, endpointID string) ([]store.NotifyHook, error)
	GetNotifyHook(ctx context.Context, id, endpointID string) (store.NotifyHook, error)
	NotifyHookSigningSecrets(ctx context.Context, id, endpointID string) (store.NotifyHookSecrets, error)
	DestroyExpiredNotifyHookSecrets(ctx context.Context) (int64, error)
	// RecordNotifyHookDelivery writes a finished delivery's health in one statement and reports
	// whether that statement disabled the hook (SPEC-0024 REQ-8).
	RecordNotifyHookDelivery(ctx context.Context, id string, delivered bool, status *int, lastError string, disableAfter int) (bool, error)
}

// Metrics is the SPEC-0024 REQ-11 counter seam; *metrics.Metrics satisfies it and is nil-safe.
type Metrics interface {
	NotifyHookNotification(typ, outcome string)
	NotifyHookAttempt(result string)
	NotifyHookDisabled(reason string)
}

type nopMetrics struct{}

func (nopMetrics) NotifyHookNotification(string, string) {}
func (nopMetrics) NotifyHookAttempt(string)              {}
func (nopMetrics) NotifyHookDisabled(string)             {}

// Delivery is the outcome of one notification to one hook, after all its attempts.
type Delivery struct {
	HookID         string
	EndpointID     string
	NotificationID string
	Delivered      bool
	Attempts       int
	Status         *int   // the last HTTP status, nil for a network-level failure
	Result         string // the last attempt's result (Result*)
	Err            error  // the last attempt's classified error; nil when delivered
}

// Options configure a Dispatcher. Store and Validator are required; the rest default to the
// SPEC-0024 fixed values and exist so tests can run fast and trust a test CA.
type Options struct {
	Store     Store
	Validator *push.Validator
	Log       *slog.Logger
	// Max is the operator ceiling (SWITCHBOARD_NOTIFY_HOOK_MAX). 0 is the kill switch: nothing is
	// ever dispatched, even to hooks that already exist.
	Max           int
	QueueSize     int
	Workers       int
	AttemptTTL    time.Duration
	Backoff       []time.Duration
	RatePerMinute int
	// TLSConfig, when set, is cloned for every dial (tests add a RootCAs pool). ServerName is always
	// overwritten with the URL's host.
	TLSConfig *tls.Config
	// Metrics receives the REQ-11 counters; nil counts nothing.
	Metrics Metrics
	// OnDelivery observes every finished delivery, after its health is recorded. It must not block.
	OnDelivery func(Delivery)
	// OnDrop observes a notification dropped before any attempt: "queue_full" or "rate_limited".
	OnDrop func(reason string)
}

type job struct {
	todo   store.Todo
	reason string
}

// Dispatcher owns the bounded queue and its workers.
type Dispatcher struct {
	opts    Options
	log     *slog.Logger
	queue   chan job
	limiter *hookLimiter
	dropped atomic.Int64
	started atomic.Bool
}

// NewDispatcher builds a dispatcher; Run starts it.
func NewDispatcher(o Options) *Dispatcher {
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.AttemptTTL <= 0 {
		o.AttemptTTL = DefaultAttemptTTL
	}
	if o.Backoff == nil {
		o.Backoff = DefaultBackoff
	}
	if o.RatePerMinute <= 0 {
		o.RatePerMinute = DefaultRatePerMinute
	}
	if o.Metrics == nil {
		o.Metrics = nopMetrics{}
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Dispatcher{
		opts:    o,
		log:     log,
		queue:   make(chan job, o.QueueSize),
		limiter: newHookLimiter(o.RatePerMinute),
	}
}

// Dropped reports how many notifications were dropped before any attempt.
func (d *Dispatcher) Dropped() int64 { return d.dropped.Load() }

// Enqueue hands a ready todo to the workers. It never blocks and never fails the caller: this runs
// on the ingest and reaper paths (SPEC-0024 REQ-7). Its signature matches store.TodoReadyHook.
func (d *Dispatcher) Enqueue(t store.Todo, reason string) {
	if d.opts.Max <= 0 || t.EndpointID == "" {
		return // the kill switch, and a system todo no endpoint owns (REQ-1)
	}
	select {
	case d.queue <- job{todo: t, reason: reason}:
	default:
		d.drop("queue_full", "todo", t.ID)
	}
}

func (d *Dispatcher) drop(reason string, attrs ...any) {
	d.dropped.Add(1)
	d.opts.Metrics.NotifyHookNotification(TypeTodoReady, "dropped")
	d.log.Warn("notify hook notification dropped", append([]any{"reason", reason}, attrs...)...)
	if d.opts.OnDrop != nil {
		d.opts.OnDrop(reason)
	}
}

// Run starts the workers and the expired-secret sweep, and blocks until ctx is done and every
// worker has returned. Cancelling ctx aborts in-flight attempts, so shutdown is bounded by one
// attempt's teardown, not by a receiver (REQ-13). Run may be called once.
func (d *Dispatcher) Run(ctx context.Context) {
	if !d.started.CompareAndSwap(false, true) {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < d.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-d.queue:
					d.dispatch(ctx, j)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(secretSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := d.opts.Store.DestroyExpiredNotifyHookSecrets(ctx); err != nil {
					d.log.Warn("notify hook secret sweep", "err", err)
				} else if n > 0 {
					d.log.Info("destroyed expired notify hook secrets", "count", n)
				}
			}
		}
	}()
	wg.Wait()
}

// dispatch fans one ready todo out to its endpoint's matching hooks.
func (d *Dispatcher) dispatch(ctx context.Context, j job) {
	t := j.todo
	ep, err := d.opts.Store.NotifyEndpointForDispatch(ctx, t.EndpointID)
	if errors.Is(err, store.ErrNotFound) {
		return // revoked, expired or deleted: its hooks do not fire
	}
	if err != nil {
		d.log.Warn("notify hook dispatch: endpoint lookup", "todo", t.ID, "err", fmt.Errorf("dispatch %s: %w", t.ID, err))
		return
	}
	// The scope is read at fire time, so a queue removed from the scope stops every hook at once.
	if !slices.Contains(ep.ScopeQueues, t.Queue) {
		return
	}
	hooks, err := d.opts.Store.ListNotifyHooks(ctx, t.EndpointID)
	if err != nil {
		d.log.Warn("notify hook dispatch: list hooks", "todo", t.ID, "err", fmt.Errorf("dispatch %s: %w", t.ID, err))
		return
	}
	for _, h := range hooks {
		if !h.Enabled || (len(h.Queues) > 0 && !slices.Contains(h.Queues, t.Queue)) {
			continue
		}
		if !d.limiter.allow(h.ID, time.Now()) {
			d.drop("rate_limited", "hook", h.ID, "todo", t.ID)
			continue
		}
		d.deliver(ctx, ep, h, t, j.reason)
	}
}

// deliver runs one notification's attempts against one hook.
func (d *Dispatcher) deliver(ctx context.Context, ep store.NotifyEndpoint, h store.NotifyHook, t store.Todo, reason string) {
	now := time.Now()
	msgID, err := newMessageID(now)
	if err != nil {
		d.log.Error("notify hook: message id", "hook", h.ID, "err", err)
		return
	}
	body, err := buildReadyBody(t, reason, ep.Slug, now)
	if err != nil {
		d.log.Error("notify hook: body", "hook", h.ID, "notification", msgID, "err", err)
		return
	}
	out := Delivery{HookID: h.ID, EndpointID: h.EndpointID, NotificationID: msgID}
	// stopped says why the loop ended before a retry it would otherwise have made. Once an attempt
	// has been sent, stopping early still finishes the notification as failed with the last attempt's
	// result: its attempts are already counted, so it gets exactly one outcome too, and a real
	// receiver failure (a 503 on attempt 1, then a store error reloading for attempt 2) still reaches
	// consecutive_failures (REQ-8, REQ-11, REQ-12). Before any attempt there is nothing to count or
	// record, so the notification just ends (logged). Shutdown is the exception: it abandons
	// in-flight notifications outright (REQ-7, nothing is persisted), and a health write on a
	// cancelled context could not land anyway.
	var stopped string
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 && !sleepCtx(ctx, jitter(d.opts.Backoff[min(attempt-2, len(d.opts.Backoff)-1)])) {
			return // shutting down: abandon, the todo is still pending
		}
		// Re-read the hook before every attempt so a delete or disable takes effect at once, and
		// the secrets so a rotation mid-notification signs with the current pair.
		cur, err := d.opts.Store.GetNotifyHook(ctx, h.ID, h.EndpointID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !cur.Enabled) {
			// No further attempt; the health write below is a no-op on a deleted or disabled row.
			stopped = "hook deleted or disabled"
			break
		}
		if err != nil {
			d.log.Warn("notify hook: reload", "hook", h.ID, "notification", msgID, "err", fmt.Errorf("hook %s reload: %w", h.ID, err))
			stopped = "hook reload failed"
			break
		}
		secrets, err := d.opts.Store.NotifyHookSigningSecrets(ctx, h.ID, h.EndpointID)
		if err != nil {
			d.log.Warn("notify hook: secrets", "hook", h.ID, "notification", msgID, "err", fmt.Errorf("hook %s secrets: %w", h.ID, err))
			stopped = "hook secrets load failed"
			break
		}
		start := time.Now()
		status, result, aerr := d.attempt(ctx, cur.URL, msgID, body, secrets)
		out.Attempts, out.Status, out.Result, out.Err = attempt, status, result, aerr
		d.opts.Metrics.NotifyHookAttempt(result)
		d.log.Info("notify hook attempt", "hook", h.ID, "notification", msgID, "attempt", attempt,
			"result", result, "duration_ms", time.Since(start).Milliseconds())
		if aerr == nil {
			out.Delivered = true
			break
		}
		if !errors.Is(aerr, ErrRetryable) && !errors.Is(aerr, ErrTimeout) {
			break
		}
		if ctx.Err() != nil {
			return
		}
	}
	if out.Attempts == 0 {
		return // stopped before anything was sent: no attempt, so no outcome and no health
	}
	if out.Delivered {
		d.opts.Metrics.NotifyHookNotification(TypeTodoReady, "delivered")
	} else {
		d.opts.Metrics.NotifyHookNotification(TypeTodoReady, "failed")
		attrs := []any{"hook", h.ID, "notification", msgID, "attempts", out.Attempts, "result", out.Result}
		if stopped != "" {
			attrs = append(attrs, "stopped", stopped)
		}
		d.log.Warn("notify hook delivery failed", append(attrs, "err", fmt.Errorf("hook %s: %w", h.ID, out.Err))...)
	}
	d.recordHealth(ctx, out)
	if d.opts.OnDelivery != nil {
		d.opts.OnDelivery(out)
	}
}

// recordHealth writes the delivery's outcome on the hook row. A store failure is logged with the
// hook and notification ids and the worker moves on: health is bookkeeping, never a reason to stall
// deliveries (SPEC-0024 REQ-12 "Store outage during health update").
func (d *Dispatcher) recordHealth(ctx context.Context, out Delivery) {
	disabled, err := d.opts.Store.RecordNotifyHookDelivery(ctx, out.HookID, out.Delivered, out.Status, lastErrorClass(out), DisableAfter)
	if err != nil {
		d.log.Warn("notify hook health update failed", "hook", out.HookID, "notification", out.NotificationID,
			"err", fmt.Errorf("hook %s health: %w", out.HookID, err))
		return
	}
	if disabled {
		d.opts.Metrics.NotifyHookDisabled("consecutive_failures")
		d.log.Warn("notify hook disabled after consecutive failures", "hook", out.HookID, "endpoint", out.EndpointID,
			"failures", DisableAfter, "result", out.Result,
			"fix", "the owner re-enables it with rotate_notify_hook or from the endpoint card")
	}
}

// lastErrorClass is the short classified reason stored as last_error: never response text.
func lastErrorClass(out Delivery) string {
	if out.Delivered {
		return ""
	}
	switch out.Result {
	case Result3xx:
		return "redirect"
	case Result4xx:
		return "client_error"
	case Result5xx:
		return "server_error"
	default:
		return out.Result // timeout, network, tls, rejected_ssrf
	}
}

// attempt sends one signed request. It returns the HTTP status (nil without a response), the
// REQ-11 result label, and nil or a classified error.
func (d *Dispatcher) attempt(ctx context.Context, rawURL, msgID string, body []byte, secrets store.NotifyHookSecrets) (*int, string, error) {
	actx, cancel := context.WithTimeout(ctx, d.opts.AttemptTTL)
	defer cancel()

	target, err := ValidateURL(actx, d.opts.Validator, rawURL)
	if err != nil {
		return nil, ResultRejectedSSRF, fmt.Errorf("%w: %v", ErrSSRF, err)
	}
	sig, err := signatureHeader(secrets, msgID, time.Now().Unix(), body)
	if err != nil {
		return nil, ResultNetwork, fmt.Errorf("%w: signing: %v", ErrPermanent, err)
	}
	ts := sig.timestamp
	req, err := http.NewRequestWithContext(actx, http.MethodPost, target.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ResultNetwork, fmt.Errorf("%w: build request", ErrPermanent)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "switchboard/"+buildinfo.Get().Version)
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("webhook-signature", sig.value)

	client := &http.Client{
		Transport:     d.pinnedTransport(target),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		result, cerr := classifyNetErr(actx, err)
		return nil, result, cerr
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseRead))
	_ = resp.Body.Close()
	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		return &status, Result2xx, nil
	case status >= 300 && status < 400:
		return &status, Result3xx, fmt.Errorf("%w: status %d", ErrRedirect, status)
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		return &status, Result4xx, fmt.Errorf("%w: status %d", ErrRetryable, status)
	case status >= 500:
		return &status, Result5xx, fmt.Errorf("%w: status %d", ErrRetryable, status)
	default:
		return &status, Result4xx, fmt.Errorf("%w: status %d", ErrPermanent, status)
	}
}

type signature struct {
	value     string
	timestamp int64
}

// signatureHeader builds webhook-signature: one "v1," entry for the current secret, plus one for
// the previous secret while its rotation grace runs (space-separated, per Standard Webhooks).
func signatureHeader(secrets store.NotifyHookSecrets, msgID string, ts int64, body []byte) (signature, error) {
	key, err := DecodeSecret(secrets.Current)
	if err != nil {
		return signature{}, err
	}
	value := Sign(key, msgID, ts, body)
	if secrets.Previous != "" {
		prev, err := DecodeSecret(secrets.Previous)
		if err != nil {
			return signature{}, err
		}
		value += " " + Sign(prev, msgID, ts, body)
	}
	return signature{value: value, timestamp: ts}, nil
}

// pinnedTransport dials only the addresses the validator just approved, never re-resolving the
// host, verifies TLS against the URL's host, uses no proxy and keeps no connection.
func (d *Dispatcher) pinnedTransport(target push.Target) *http.Transport {
	var tlsCfg *tls.Config
	if d.opts.TLSConfig != nil {
		tlsCfg = d.opts.TLSConfig.Clone()
	} else {
		tlsCfg = &tls.Config{}
	}
	tlsCfg.ServerName = target.Host
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	ips, port := target.IPs, target.Port
	return &http.Transport{
		Proxy: nil, // an environment proxy would resolve the host itself and unpin the dial
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("no validated address to dial")
			}
			return nil, lastErr
		},
		TLSClientConfig:        tlsCfg,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    d.opts.AttemptTTL,
		ResponseHeaderTimeout:  d.opts.AttemptTTL,
		MaxResponseHeaderBytes: maxResponseRead,
	}
}

// classifyNetErr maps a transport error to a REQ-11 result and a sentinel. The message is the
// class only: net/http errors quote the request URL, query string included.
func classifyNetErr(ctx context.Context, err error) (string, error) {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded ||
		(errors.As(err, &netErr) && netErr.Timeout()):
		return ResultTimeout, fmt.Errorf("%w", ErrTimeout)
	case isTLSError(err):
		return ResultTLS, fmt.Errorf("%w: tls", ErrRetryable)
	default:
		return ResultNetwork, fmt.Errorf("%w: network", ErrRetryable)
	}
}

func isTLSError(err error) bool {
	var (
		verr  *tls.CertificateVerificationError
		rherr tls.RecordHeaderError
		alert tls.AlertError
		uaerr x509.UnknownAuthorityError
		hnerr x509.HostnameError
		cierr x509.CertificateInvalidError
		ech   *tls.ECHRejectionError
	)
	return errors.As(err, &verr) || errors.As(err, &rherr) || errors.As(err, &alert) || errors.As(err, &uaerr) ||
		errors.As(err, &hnerr) || errors.As(err, &cierr) || errors.As(err, &ech)
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// sleepCtx waits d or until ctx is done, reporting whether the full wait elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// hookLimiter is a per-hook token bucket: RatePerMinute notifications a minute, bursting to the same
// number, so a runaway producer cannot turn Switchboard into an amplifier against a receiver.
type hookLimiter struct {
	mu      sync.Mutex
	perSec  float64
	burst   float64
	buckets map[string]*hookBucket
}

type hookBucket struct {
	tokens float64
	last   time.Time
}

func newHookLimiter(perMinute int) *hookLimiter {
	return &hookLimiter{perSec: float64(perMinute) / 60, burst: float64(perMinute), buckets: map[string]*hookBucket{}}
}

func (l *hookLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &hookBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	// A full, idle bucket carries no state worth keeping; prune them so the map tracks busy hooks.
	if len(l.buckets) > 4096 {
		for k, v := range l.buckets {
			if now.Sub(v.last) > 2*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
