// Package metrics owns switchboard's Prometheus surface: a dedicated registry, the counter vectors
// SPEC-0023 names, the label limiters that keep them bounded, and the authenticated /metrics
// handler.
//
// Counters are incremented inline by the code that already knows the outcome (the store for the
// todo lifecycle, ingest for deliveries and routing), through narrow interfaces those packages
// declare themselves (store.Metrics, ingest.Metrics) — so neither imports this package, and this
// package never imports them. The queue-liveness gauges are computed at scrape time by a collector
// registered on Registry(), never maintained inline.
//
// Every method is nil-receiver safe: a nil *Metrics is a complete no-op, so a caller never has to
// guard an increment and a test can build any component without a metrics surface.
//
// Governing: SPEC-0023 REQ-1 "The endpoint", REQ-3 "Lifecycle counters", REQ-4 "Ingest and
// routing", REQ-5 "Cardinality", REQ-6 "Honest absence"; design.md "Shape"; ADR-0028.
package metrics

import (
	"log/slog"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Enumerated label values. Anything a caller passes outside these sets is reported as Other, never
// passed through, so a caller bug can widen a label by exactly one value and no further.
const (
	OutcomeComplete = "complete"
	OutcomeFail     = "fail"

	VerdictAccepted = "accepted"
	VerdictRejected = "rejected"
	VerdictDropped  = "dropped"

	ActionQueue = "queue"
	ActionDrop  = "drop"

	// RuleDefault is the rule_id a routing decision carries when no rule matched and the webhook's
	// default target applied.
	RuleDefault = "default"

	// SPEC-0024 REQ-11 notify-hook label values.
	NotifyTypeReady      = "todo.ready"
	NotifyTypeBacklog    = "todos.backlog"
	NotifyDelivered      = "delivered"
	NotifyFailed         = "failed"
	NotifyDropped        = "dropped"
	NotifyDisabledFailed = "consecutive_failures"
	NotifyDisabledByOp   = "operator"
)

// notifyAttemptResults is the bounded result label of switchboard_notify_hook_attempts_total.
var notifyAttemptResults = []string{"2xx", "3xx", "4xx", "5xx", "timeout", "network", "tls", "rejected_ssrf"}

var (
	// tokenLabel is the last-resort bound on free-ish label values (source, provider, trust_mode,
	// reason, collector). Callers are expected to map to their own small enums first; this only
	// guarantees that whatever reaches a label is short, lowercase, and from a finite alphabet.
	tokenLabel = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// ruleIDLabel admits only server-minted rule ids (routing.NewRuleID: rule_<24 hex>). Rule names
	// are operator free text and must never reach a label (SPEC-0023 REQ-4).
	ruleIDLabel = regexp.MustCompile(`^rule_[0-9a-f]{24}$`)
)

// Options configures New. The zero value is valid and applies the defaults.
type Options struct {
	// QueueCap is how many distinct queue label values are admitted before the rest report as
	// queue="__other__". Zero means DefaultLabelCap.
	QueueCap int
	// WebhookCap is the same bound for the webhook label on routing decisions. Zero means
	// DefaultLabelCap.
	WebhookCap int
	// Log receives exposition errors from the scrape handler. Nil discards them.
	Log *slog.Logger
}

// Metrics is the process's metric surface. Build it once with New and share the pointer.
type Metrics struct {
	reg *prometheus.Registry
	log *slog.Logger

	queues   *labelLimiter
	webhooks *labelLimiter

	// REQ-3 lifecycle counters.
	todosCreated   *prometheus.CounterVec
	todosClaimed   *prometheus.CounterVec
	todosCompleted *prometheus.CounterVec
	leasesExpired  *prometheus.CounterVec
	todoAttempts   *prometheus.CounterVec

	// REQ-4 ingest and routing counters.
	deliveries       *prometheus.CounterVec
	routingDecisions *prometheus.CounterVec
	verifyFailures   *prometheus.CounterVec

	// REQ-6: a collector that could not compute its families says so here.
	collectionErrors *prometheus.CounterVec

	// SPEC-0024 REQ-11 notify-hook counters. No hook, endpoint, URL or host label, ever.
	notifyNotifications *prometheus.CounterVec
	notifyAttempts      *prometheus.CounterVec
	notifyDisabled      *prometheus.CounterVec
}

// New builds the metric surface on a dedicated registry — never the global default, so a stray
// prometheus.MustRegister anywhere in the dependency tree can neither collide with these names nor
// leak its own series onto the scrape (design.md "Shape"). The Go runtime and process collectors
// are registered, because restarts and memory growth are part of reading everything else
// (SPEC-0023 REQ-1).
func New(opts Options) *Metrics {
	m := &Metrics{
		reg:      prometheus.NewRegistry(),
		log:      opts.Log,
		queues:   newLabelLimiter(opts.QueueCap),
		webhooks: newLabelLimiter(opts.WebhookCap),

		todosCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_todos_created_total",
			Help: "Todos created, by queue and bounded source.",
		}, []string{"queue", "source"}),
		todosClaimed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_todos_claimed_total",
			Help: "Successful todo claims, by queue.",
		}, []string{"queue"}),
		todosCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_todos_completed_total",
			Help: "Todos finished by their claimant, by queue and outcome (complete|fail).",
		}, []string{"queue", "outcome"}),
		leasesExpired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_todo_leases_expired_total",
			Help: "Claims whose lease lapsed before completion — each one is potential duplicate work.",
		}, []string{"queue"}),
		todoAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_todo_attempts_total",
			Help: "Claims by the attempt number they started (1|2|3+).",
		}, []string{"queue", "attempt_bucket"}),

		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_webhook_deliveries_total",
			Help: "Inbound webhook deliveries, by provider, trust mode and verdict (accepted|rejected|dropped).",
		}, []string{"provider", "trust_mode", "verdict"}),
		routingDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_routing_decisions_total",
			Help: "Routing decisions, drops included, by webhook, rule id and action (queue|drop).",
		}, []string{"webhook", "rule_id", "action"}),
		verifyFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_webhook_verify_failures_total",
			Help: "Webhook deliveries that failed verification, by provider and bounded reason.",
		}, []string{"provider", "reason"}),

		collectionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_metrics_collection_errors_total",
			Help: "Scrape-time collectors that failed and omitted their families, by collector.",
		}, []string{"collector"}),

		notifyNotifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_notify_hook_notifications_total",
			Help: "Outbound notify-hook notifications, by type (todo.ready|todos.backlog) and outcome (delivered|failed|dropped). delivered, failed and rate-limited drops count one per hook; a full delivery queue drops before hooks are matched, so it counts one per ready todo, whatever its hook count (zero included).",
		}, []string{"type", "outcome"}),
		notifyAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_notify_hook_attempts_total",
			Help: "Outbound notify-hook HTTP attempts, by result (2xx|3xx|4xx|5xx|timeout|network|tls|rejected_ssrf).",
		}, []string{"result"}),
		notifyDisabled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_notify_hooks_disabled_total",
			Help: "Notify hooks disabled, by reason (consecutive_failures|operator).",
		}, []string{"reason"}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.todosCreated, m.todosClaimed, m.todosCompleted, m.leasesExpired, m.todoAttempts,
		m.deliveries, m.routingDecisions, m.verifyFailures,
		m.collectionErrors,
		m.notifyNotifications, m.notifyAttempts, m.notifyDisabled,
	)
	return m
}

// InitNotifyHookSeries pre-creates every notify-hook series at zero. The label sets are small fixed
// enums, so a dashboard or an increase() alert has a baseline from the first scrape instead of a
// family that appears only after the first failure (#362). Call it once where the dispatcher is
// wired; a registry with no dispatcher stays honestly empty.
func (m *Metrics) InitNotifyHookSeries() {
	if m == nil {
		return
	}
	for _, typ := range []string{NotifyTypeReady, NotifyTypeBacklog} {
		for _, outcome := range []string{NotifyDelivered, NotifyFailed, NotifyDropped} {
			m.notifyNotifications.WithLabelValues(typ, outcome)
		}
	}
	for _, r := range notifyAttemptResults {
		m.notifyAttempts.WithLabelValues(r)
	}
	for _, reason := range []string{NotifyDisabledFailed, NotifyDisabledByOp} {
		m.notifyDisabled.WithLabelValues(reason)
	}
}

// NotifyHookNotification counts one notification's final outcome: NotifyDelivered, NotifyFailed
// (after its attempts), or NotifyDropped (queue full or over the per-hook rate limit).
//
// The unit is one hook's notification, except for a queue_full drop. The bounded queue sits on the
// ingest path and holds ready todos, not per-hook notifications, because matching hooks needs a
// store read that SPEC-0024 REQ-7 keeps off that path. A queue_full drop is therefore counted once
// per dropped todo: once for a todo whose endpoint has three matching hooks, and once for one with
// none. Read a dropped rate as "the dispatcher is shedding load", not as a count of receivers that
// missed a notification.
func (m *Metrics) NotifyHookNotification(typ, outcome string) {
	if m == nil {
		return
	}
	m.notifyNotifications.WithLabelValues(oneOf(typ, NotifyTypeReady, NotifyTypeBacklog),
		oneOf(outcome, NotifyDelivered, NotifyFailed, NotifyDropped)).Inc()
}

// NotifyHookAttempt counts one outbound attempt by its bounded result.
func (m *Metrics) NotifyHookAttempt(result string) {
	if m == nil {
		return
	}
	m.notifyAttempts.WithLabelValues(oneOf(result, notifyAttemptResults...)).Inc()
}

// NotifyHookDisabled counts one hook being disabled, automatically or by its owning human.
func (m *Metrics) NotifyHookDisabled(reason string) {
	if m == nil {
		return
	}
	m.notifyDisabled.WithLabelValues(oneOf(reason, NotifyDisabledFailed, NotifyDisabledByOp)).Inc()
}

// Registry is the dedicated registry the handler serves. Scrape-time collectors (the queue
// liveness gauges) register here; nothing registers on the global default. Nil on a nil receiver.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// QueueLabel maps a queue name to the label value it reports under: its own name while it holds
// one of the admitted slots, Other once the cap is reached. Every family that carries a queue label
// MUST route it through here so the same queue reads the same everywhere (SPEC-0023 REQ-5). A nil
// receiver returns Other.
func (m *Metrics) QueueLabel(queue string) string {
	if m == nil {
		return Other
	}
	return m.queues.label(queue)
}

// webhookLabel is QueueLabel's twin for the webhook dimension of routing decisions.
func (m *Metrics) webhookLabel(id string) string {
	return m.webhooks.label(id)
}

// CollectionError records that a scrape-time collector failed and omitted its families
// (SPEC-0023 REQ-6: omit, never report zero, and make the omission itself visible).
func (m *Metrics) CollectionError(collector string) {
	if m == nil {
		return
	}
	m.collectionErrors.WithLabelValues(token(collector)).Inc()
}

// InitCollectionErrors pre-creates the collection-error series for collector at zero, so an
// increase() alert over it has a baseline from the first scrape rather than only after the first
// failure. Call it once when registering a scrape-time collector.
func (m *Metrics) InitCollectionErrors(collector string) {
	if m == nil {
		return
	}
	m.collectionErrors.WithLabelValues(token(collector))
}

// TodoCreated counts one newly persisted todo. Idempotency dedups are not creations and must not
// be reported. source is a bounded origin (webhook source type, push, api, dev…).
func (m *Metrics) TodoCreated(queue, source string) {
	if m == nil {
		return
	}
	m.todosCreated.WithLabelValues(m.QueueLabel(queue), token(source)).Inc()
}

// TodoClaimed counts one successful claim and the attempt it began: attempt is the todo's attempt
// number after the claim (1 for a first claim). It feeds both switchboard_todos_claimed_total and
// switchboard_todo_attempts_total{attempt_bucket="1"|"2"|"3+"}.
func (m *Metrics) TodoClaimed(queue string, attempt int) {
	if m == nil {
		return
	}
	q := m.QueueLabel(queue)
	m.todosClaimed.WithLabelValues(q).Inc()
	m.todoAttempts.WithLabelValues(q, attemptBucket(attempt)).Inc()
}

// TodoFinished counts one claimant-reported completion. outcome is OutcomeComplete or OutcomeFail.
func (m *Metrics) TodoFinished(queue, outcome string) {
	if m == nil {
		return
	}
	m.todosCompleted.WithLabelValues(m.QueueLabel(queue), oneOf(outcome, OutcomeComplete, OutcomeFail)).Inc()
}

// LeaseExpired counts one lapsed lease, whether the reaper found it or a claimant took it over.
// Any non-zero rate is a finding: it is the duplicate-work signal (SPEC-0023 REQ-3).
func (m *Metrics) LeaseExpired(queue string) {
	if m == nil {
		return
	}
	m.leasesExpired.WithLabelValues(m.QueueLabel(queue)).Inc()
}

// WebhookDelivery counts one inbound delivery by its verdict: VerdictAccepted (verified and
// persisted, idempotency dedups included), VerdictRejected (failed verification or refused), or
// VerdictDropped (a routing decision of drop).
func (m *Metrics) WebhookDelivery(provider, trustMode, verdict string) {
	if m == nil {
		return
	}
	m.deliveries.WithLabelValues(token(provider), token(trustMode),
		oneOf(verdict, VerdictAccepted, VerdictRejected, VerdictDropped)).Inc()
}

// WebhookVerifyFailure counts one failed verification. reason must be a bounded failure mode
// (bad_signature, missing_signature, stale_timestamp, …), never the free-text rejection message.
func (m *Metrics) WebhookVerifyFailure(provider, reason string) {
	if m == nil {
		return
	}
	m.verifyFailures.WithLabelValues(token(provider), token(reason)).Inc()
}

// RoutingDecision counts one routing decision, drops included — a counter stuck at zero is how a
// rule that matches nothing becomes visible (SPEC-0023 REQ-4). webhookID is the server-minted id,
// held to the webhook limiter; ruleID is the matched rule's id, or "" (or RuleDefault) when no rule
// matched. action is ActionQueue or ActionDrop.
func (m *Metrics) RoutingDecision(webhookID, ruleID, action string) {
	if m == nil {
		return
	}
	m.routingDecisions.WithLabelValues(m.webhookLabel(webhookID), ruleLabel(ruleID),
		oneOf(action, ActionQueue, ActionDrop)).Inc()
}

// attemptBucket folds an attempt number into the three buckets REQ-3 names. A claim is always at
// least a first attempt, so anything below 2 is "1".
func attemptBucket(attempt int) string {
	switch {
	case attempt >= 3:
		return "3+"
	case attempt == 2:
		return "2"
	default:
		return "1"
	}
}

// oneOf returns v when it is one of allowed, and Other otherwise.
func oneOf(v string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return Other
}

// token returns v when it matches the bounded token alphabet, and Other otherwise.
func token(v string) string {
	if tokenLabel.MatchString(v) {
		return v
	}
	return Other
}

// ruleLabel maps a routing decision's rule id to its label: RuleDefault when no rule matched, the
// id itself when it is server-minted, Other for anything else.
func ruleLabel(id string) string {
	switch {
	case id == "" || id == RuleDefault:
		return RuleDefault
	case ruleIDLabel.MatchString(id):
		return id
	default:
		return Other
	}
}
