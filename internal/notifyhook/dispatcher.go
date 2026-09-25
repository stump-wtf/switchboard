package notifyhook

// The delivery dispatcher: when a push-eligible todo becomes ready, POST one signed notification to
// each matching hook of its endpoint. Best effort by design. The queue is the ledger, so a dropped,
// failed or lost notification costs latency, never work: the todo stays pending and claimable.
//
//   - Trigger: the store's ready hook (store.SetTodoReadyHook), fired after commit and only for
//     todos that pass the SPEC-0011 sender gate, on creation and on requeue. Enqueue never blocks:
//     a full queue drops the notification and logs it. A queued notification keeps only the few
//     todo fields its body needs (notice), never the payload, so the bound is on memory too.
//   - Match: the hook's endpoint must be live, the todo's queue must be in the endpoint's CURRENT
//     scope, and in the hook's queue filter when it has one. Each matching hook becomes its own
//     delivery job on a second bounded queue, so one hook's retries never delay a sibling's first
//     attempt; the endpoint, scope and hook are re-checked before every attempt.
//   - Dial: the URL is re-validated before every attempt, and the connection goes to an address from
//     that same resolution (no second lookup), with TLS verified against the URL's host, no proxy
//     and no redirects.
//   - Sign: Standard Webhooks, dual-signed during a rotation grace. The webhook-id is stable across
//     a notification's attempts; each attempt has its own timestamp and signature.
//   - Retry: at most 3 attempts, about 1s then 5s apart with jitter, for network errors (a host
//     that fails to resolve is one), timeouts, 408, 429 and 5xx only. An address the SSRF guard
//     rejects is never retried.
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
	maxResponseRead      = 64 << 10
	secretSweepInterval  = 10 * time.Minute
	// dropLogEvery coalesces drop warnings: at most one line per reason in this window, carrying
	// how many drops it stood for, so a sustained overflow cannot flood the log. Every drop is
	// still counted (Dropped, OnDrop).
	dropLogEvery = 10 * time.Second
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
}

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
	// OnDelivery observes every finished delivery (health and metrics hang off it). It must not block.
	OnDelivery func(Delivery)
	// OnDrop observes a notification dropped before any attempt: "queue_full" or "rate_limited".
	OnDrop func(reason string)
}

// delivery is one notification bound for one hook: the unit a worker runs, retries included.
type delivery struct {
	n      notice
	hookID string
}

// Dispatcher owns the two bounded queues and their workers: ready todos waiting to be matched, and
// (notification, hook) deliveries waiting to be sent.
type Dispatcher struct {
	opts       Options
	log        *slog.Logger
	queue      chan notice
	deliveries chan delivery
	limiter    *hookLimiter
	dropped    atomic.Int64
	started    atomic.Bool
	dropLog    dropLogger
}

// dropLogger coalesces drop warnings per reason (see dropLogEvery).
type dropLogger struct {
	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int64
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
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Dispatcher{
		opts:       o,
		log:        log,
		queue:      make(chan notice, o.QueueSize),
		deliveries: make(chan delivery, o.QueueSize),
		limiter:    newHookLimiter(o.RatePerMinute),
		dropLog:    dropLogger{last: map[string]time.Time{}, suppressed: map[string]int64{}},
	}
}

// Dropped reports how many notifications were dropped before any attempt.
func (d *Dispatcher) Dropped() int64 { return d.dropped.Load() }

// Enqueue hands a ready todo to the workers. It never blocks and never fails the caller: this runs
// on the ingest and reaper paths (SPEC-0024 REQ-7). Its signature matches store.TodoReadyHook.
// Only a slim notice is queued, so the todo's payload (up to the 5 MiB body cap), routing trace and
// work order are collectable as soon as the store hook returns.
func (d *Dispatcher) Enqueue(t store.Todo, reason string) {
	if d.opts.Max <= 0 || t.EndpointID == "" {
		return // the kill switch, and a system todo no endpoint owns (REQ-1)
	}
	select {
	case d.queue <- newNotice(t, reason):
	default:
		// No hook is matched yet, so the endpoint id stands in for the hook id (never the URL).
		d.drop("queue_full", "endpoint", t.EndpointID, "todo", t.ID)
	}
}

// drop counts a notification dropped before any attempt and logs it, coalescing the warning to one
// line per reason per dropLogEvery. Attrs name hook, endpoint and todo ids only, never a URL.
func (d *Dispatcher) drop(reason string, attrs ...any) {
	d.dropped.Add(1)
	if d.opts.OnDrop != nil {
		d.opts.OnDrop(reason)
	}
	now := time.Now()
	d.dropLog.mu.Lock()
	if last, ok := d.dropLog.last[reason]; ok && now.Sub(last) < dropLogEvery {
		d.dropLog.suppressed[reason]++
		d.dropLog.mu.Unlock()
		return
	}
	suppressed := d.dropLog.suppressed[reason]
	d.dropLog.last[reason], d.dropLog.suppressed[reason] = now, 0
	d.dropLog.mu.Unlock()
	d.log.Warn("notify hook notification dropped",
		append([]any{"reason", reason, "suppressed_since_last_warning", suppressed}, attrs...)...)
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
				case n := <-d.queue:
					d.dispatch(ctx, n)
				case dj := <-d.deliveries:
					d.deliver(ctx, dj)
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

// dispatch matches one ready todo against its endpoint's hooks and queues one delivery per matching
// hook, so each hook's attempts and retries run independently of its siblings'.
func (d *Dispatcher) dispatch(ctx context.Context, n notice) {
	ep, err := d.opts.Store.NotifyEndpointForDispatch(ctx, n.endpointID)
	if errors.Is(err, store.ErrNotFound) {
		return // revoked, expired or deleted: its hooks do not fire
	}
	if err != nil {
		d.log.Warn("notify hook dispatch: endpoint lookup", "todo", n.todoID, "err", fmt.Errorf("dispatch %s: %w", n.todoID, err))
		return
	}
	// The scope is read at fire time, so a queue removed from the scope stops every hook at once.
	if !slices.Contains(ep.ScopeQueues, n.queue) {
		return
	}
	hooks, err := d.opts.Store.ListNotifyHooks(ctx, n.endpointID)
	if err != nil {
		d.log.Warn("notify hook dispatch: list hooks", "todo", n.todoID, "err", fmt.Errorf("dispatch %s: %w", n.todoID, err))
		return
	}
	for _, h := range hooks {
		if !h.Enabled || !hookWants(h, n.queue) {
			continue
		}
		if !d.limiter.allow(h.ID, time.Now()) {
			d.drop("rate_limited", "hook", h.ID, "todo", n.todoID)
			continue
		}
		select {
		case d.deliveries <- delivery{n: n, hookID: h.ID}:
		default:
			d.drop("queue_full", "hook", h.ID, "todo", n.todoID)
		}
	}
}

// hookWants reports whether a hook's queue filter admits queue (no filter admits every queue).
func hookWants(h store.NotifyHook, queue string) bool {
	return len(h.Queues) == 0 || slices.Contains(h.Queues, queue)
}

// deliver runs one notification's attempts against one hook.
func (d *Dispatcher) deliver(ctx context.Context, dj delivery) {
	n, hookID := dj.n, dj.hookID
	now := time.Now()
	msgID, err := newMessageID(now)
	if err != nil {
		d.log.Error("notify hook: message id", "hook", hookID, "err", err)
		return
	}
	var body []byte
	out := Delivery{HookID: hookID, EndpointID: n.endpointID, NotificationID: msgID}
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 && !sleepCtx(ctx, jitter(d.opts.Backoff[min(attempt-2, len(d.opts.Backoff)-1)])) {
			return // shutting down: abandon, the todo is still pending
		}
		cur, slug, secrets, ok := d.reload(ctx, n, hookID, msgID)
		if !ok {
			return
		}
		if body == nil {
			if body, err = buildReadyBody(n, slug, now); err != nil {
				d.log.Error("notify hook: body", "hook", hookID, "notification", msgID, "err", err)
				return
			}
		}
		start := time.Now()
		status, result, aerr := d.attempt(ctx, cur.URL, msgID, body, secrets)
		out.Attempts, out.Status, out.Result, out.Err = attempt, status, result, aerr
		d.log.Info("notify hook attempt", "hook", hookID, "notification", msgID, "attempt", attempt,
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
	if !out.Delivered {
		d.log.Warn("notify hook delivery failed", "hook", hookID, "notification", msgID, "attempts", out.Attempts,
			"result", out.Result, "err", fmt.Errorf("hook %s: %w", hookID, out.Err))
	}
	if d.opts.OnDelivery != nil {
		d.opts.OnDelivery(out)
	}
}

// reload re-reads, before every attempt, everything that decides whether the attempt may run: the
// endpoint (a revoke stops the next attempt), its scope (a queue removed from it stops the next
// attempt), the hook (a delete, a disable or a narrowed filter stops the next attempt) and its
// secrets (a rotation mid-notification signs with the current pair). ok is false when the attempt
// must not run; a lookup failure other than not-found is logged.
func (d *Dispatcher) reload(ctx context.Context, n notice, hookID, msgID string) (hook store.NotifyHook, slug string, secrets store.NotifyHookSecrets, ok bool) {
	ep, err := d.opts.Store.NotifyEndpointForDispatch(ctx, n.endpointID)
	if errors.Is(err, store.ErrNotFound) {
		return hook, "", secrets, false
	}
	if err != nil {
		d.log.Warn("notify hook: endpoint reload", "hook", hookID, "notification", msgID, "err", fmt.Errorf("hook %s endpoint reload: %w", hookID, err))
		return hook, "", secrets, false
	}
	if !slices.Contains(ep.ScopeQueues, n.queue) {
		return hook, "", secrets, false
	}
	hook, err = d.opts.Store.GetNotifyHook(ctx, hookID, n.endpointID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!hook.Enabled || !hookWants(hook, n.queue))) {
		return hook, "", secrets, false
	}
	if err != nil {
		d.log.Warn("notify hook: reload", "hook", hookID, "notification", msgID, "err", fmt.Errorf("hook %s reload: %w", hookID, err))
		return hook, "", secrets, false
	}
	secrets, err = d.opts.Store.NotifyHookSigningSecrets(ctx, hookID, n.endpointID)
	if err != nil {
		d.log.Warn("notify hook: secrets", "hook", hookID, "notification", msgID, "err", fmt.Errorf("hook %s secrets: %w", hookID, err))
		return hook, "", secrets, false
	}
	return hook, ep.Slug, secrets, true
}

// attempt sends one signed request. It returns the HTTP status (nil without a response), the
// REQ-11 result label, and nil or a classified error.
func (d *Dispatcher) attempt(ctx context.Context, rawURL, msgID string, body []byte, secrets store.NotifyHookSecrets) (*int, string, error) {
	actx, cancel := context.WithTimeout(ctx, d.opts.AttemptTTL)
	defer cancel()

	target, err := ValidateURL(actx, d.opts.Validator, rawURL)
	if err != nil {
		result, cerr := classifyValidateErr(actx, err)
		return nil, result, cerr
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

// classifyValidateErr maps a dial-time ValidateURL failure. A host that did not resolve (a resolver
// outage, SERVFAIL, NXDOMAIN, or the lookup hitting the attempt deadline) is a network error or a
// timeout, which REQ-7 retries; nothing was dialled either way, so no connection is ever opened on an
// unknown answer. Only an address-policy rejection (a disallowed or rebound address, a bad scheme or
// URL) is rejected_ssrf, and that is never retried.
func classifyValidateErr(ctx context.Context, err error) (string, error) {
	if !errors.Is(err, push.ErrResolve) {
		return ResultRejectedSSRF, fmt.Errorf("%w: %v", ErrSSRF, err)
	}
	var dnsErr *net.DNSError
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded ||
		(errors.As(err, &dnsErr) && dnsErr.IsTimeout) {
		return ResultTimeout, fmt.Errorf("%w: resolving the hook host", ErrTimeout)
	}
	return ResultNetwork, fmt.Errorf("%w: resolving the hook host", ErrRetryable)
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
