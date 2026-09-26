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
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/stump-wtf/switchboard/internal/store"
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

	// Routing fault causes (SPEC-0026 REQ-11).
	FaultCauseTimeout = "timeout"
	FaultCauseError   = "error"
	FaultCauseCompile = "compile"
	FaultCauseBudget  = "budget"

	// Quarantine reasons, resolution outcomes and resolvers (SPEC-0026 REQ-11).
	QuarantineUntrustedActor = "untrusted_actor"
	QuarantineRuleFault      = "rule_fault"
	QuarantineRuleAction     = "rule_action"

	ResolvedReleased  = "released"
	ResolvedDiscarded = "discarded"
	ResolvedExpired   = "expired"

	ResolvedByHuman      = "human"
	ResolvedByClassifier = "classifier"
	ResolvedBySystem     = "system"
)

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
	// SPEC-0026 REQ-1 / REQ-11: deliveries whose routing stopped at a rule fault.
	routingFaults *prometheus.CounterVec
	// SPEC-0026 REQ-11: deliveries held in quarantine, and how held deliveries left it.
	quarantineItems    *prometheus.CounterVec
	quarantineResolved *prometheus.CounterVec

	// REQ-6: a collector that could not compute its families says so here.
	collectionErrors *prometheus.CounterVec
}

// New builds the metric surface on a dedicated registry — never the global default, so a stray
// prometheus.MustRegister anywhere in the dependency tree can neither collide with these names nor
// leak its own series onto the scrape (design.md "Shape"). The Go runtime and process collectors
// are registered, because restarts and memory growth are part of reading everything else
// (SPEC-0023 REQ-1).
func New(opts Options) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		log: opts.Log,
		// The reserved quarantine queue never folds into Other: the liveness alert excludes it by
		// name (SPEC-0026 REQ-11).
		queues:   newLabelLimiter(opts.QueueCap, store.QueueQuarantine),
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
		routingFaults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_routing_faults_total",
			Help: "Deliveries whose routing stopped at a rule fault and routed nowhere, by cause (timeout|error|compile|budget).",
		}, []string{"cause"}),
		quarantineItems: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_quarantine_items_total",
			Help: "Deliveries held in the owner's quarantine, by reason (untrusted_actor|rule_fault|rule_action).",
		}, []string{"reason"}),
		quarantineResolved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_quarantine_resolved_total",
			Help: "Held deliveries that left quarantine, by outcome (released|discarded|expired) and resolver (human|classifier|system).",
		}, []string{"outcome", "by"}),

		collectionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchboard_metrics_collection_errors_total",
			Help: "Scrape-time collectors that failed and omitted their families, by collector.",
		}, []string{"collector"}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.todosCreated, m.todosClaimed, m.todosCompleted, m.leasesExpired, m.todoAttempts,
		m.deliveries, m.routingDecisions, m.verifyFailures, m.routingFaults,
		m.quarantineItems, m.quarantineResolved,
		m.collectionErrors,
	)
	return m
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

// RoutingFault counts one delivery whose routing stopped at a rule fault. cause is the routing
// package's fault cause; it is folded into the four label values SPEC-0026 REQ-11 names, and
// anything else reports as Other.
func (m *Metrics) RoutingFault(cause string) {
	if m == nil {
		return
	}
	m.routingFaults.WithLabelValues(faultCauseLabel(cause)).Inc()
}

// faultCauseLabel maps routing's fault causes (routing.FaultTimeout and friends, spelled out here
// because this package imports neither routing nor ingest) onto the REQ-11 label set.
func faultCauseLabel(cause string) string {
	switch cause {
	case "timeout":
		return FaultCauseTimeout
	case "error":
		return FaultCauseError
	case "compile_error":
		return FaultCauseCompile
	case "budget_exhausted":
		return FaultCauseBudget
	default:
		return Other
	}
}

// QuarantineHeld counts one delivery newly held in quarantine (a redelivery collapsing onto its
// existing item is not counted). reason is untrusted_actor, rule_fault or rule_action; anything else
// reports as Other. Governing: SPEC-0026 REQ-11.
func (m *Metrics) QuarantineHeld(reason string) {
	if m == nil {
		return
	}
	m.quarantineItems.WithLabelValues(oneOf(reason, QuarantineUntrustedActor, QuarantineRuleFault, QuarantineRuleAction)).Inc()
}

// QuarantineResolved counts one held delivery leaving quarantine. outcome is released, discarded
// or expired. by is the resolver exactly as the store records it ("human:<id>",
// "classifier:<slug>" or "system"); only its kind reaches the label, never the id or slug, so the
// series stays bounded however many humans and classifiers there are. Governing: SPEC-0026 REQ-11,
// SPEC-0023 REQ-5.
func (m *Metrics) QuarantineResolved(outcome, by string) {
	if m == nil {
		return
	}
	m.quarantineResolved.WithLabelValues(oneOf(outcome, ResolvedReleased, ResolvedDiscarded, ResolvedExpired),
		resolverLabel(by)).Inc()
}

// resolverLabel folds a recorded resolver onto its kind: human, classifier or system.
func resolverLabel(by string) string {
	kind, _, _ := strings.Cut(by, ":")
	return oneOf(kind, ResolvedByHuman, ResolvedByClassifier, ResolvedBySystem)
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
