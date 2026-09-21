package metrics

// Metric surface tests
//
// Pins the declarations (names and label sets exactly as SPEC-0023 REQ-3/REQ-4/REQ-6 spell them),
// enum coercion, the shared queue limiter, honest absence for never-incremented vectors, and
// nil-receiver safety.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// series is one gathered sample: its family name, its labels, and its value.
type series struct {
	name   string
	labels map[string]string
	value  float64
}

// gather flattens the registry into series, failing the test on a gather error.
func gather(t *testing.T, m *Metrics) []series {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []series
	for _, mf := range mfs {
		for _, mt := range mf.GetMetric() {
			s := series{name: mf.GetName(), labels: map[string]string{}}
			for _, lp := range mt.GetLabel() {
				s.labels[lp.GetName()] = lp.GetValue()
			}
			s.value = sampleValue(mf, mt)
			out = append(out, s)
		}
	}
	return out
}

func sampleValue(mf *dto.MetricFamily, mt *dto.Metric) float64 {
	switch mf.GetType() {
	case dto.MetricType_COUNTER:
		return mt.GetCounter().GetValue()
	case dto.MetricType_GAUGE:
		return mt.GetGauge().GetValue()
	default:
		return 0
	}
}

// value returns the value of the series with exactly these labels, and whether it exists.
func value(t *testing.T, m *Metrics, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	for _, s := range gather(t, m) {
		if s.name != name || len(s.labels) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if s.labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s.value, true
		}
	}
	return 0, false
}

func mustValue(t *testing.T, m *Metrics, name string, labels map[string]string, want float64) {
	t.Helper()
	got, ok := value(t, m, name, labels)
	if !ok {
		t.Fatalf("%s%v: series absent, want %v", name, labels, want)
	}
	if got != want {
		t.Fatalf("%s%v = %v, want %v", name, labels, got, want)
	}
}

// familyLabels returns the label names each switchboard_ family carries, from gathered series.
func familyLabels(t *testing.T, m *Metrics) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, s := range gather(t, m) {
		if !strings.HasPrefix(s.name, "switchboard_") {
			continue
		}
		var names []string
		for k := range s.labels {
			names = append(names, k)
		}
		sort.Strings(names)
		out[s.name] = strings.Join(names, ",")
	}
	return out
}

// TestDeclaredFamiliesMatchSpec drives every increment once and asserts each family's name and
// label set exactly as SPEC-0023 spells them — a typo in a metric name is an alert that never fires.
func TestDeclaredFamiliesMatchSpec(t *testing.T) {
	m := New(Options{})
	m.TodoCreated("forge", "github")
	m.TodoClaimed("forge", 1)
	m.TodoFinished("forge", OutcomeComplete)
	m.LeaseExpired("forge")
	m.WebhookDelivery("github", "signed", VerdictAccepted)
	m.WebhookVerifyFailure("github", "bad_signature")
	m.RoutingDecision("wh-1", "", ActionQueue)
	m.CollectionError("queue")

	want := map[string]string{
		"switchboard_todos_created_total":             "queue,source",
		"switchboard_todos_claimed_total":             "queue",
		"switchboard_todos_completed_total":           "outcome,queue",
		"switchboard_todo_leases_expired_total":       "queue",
		"switchboard_todo_attempts_total":             "attempt_bucket,queue",
		"switchboard_webhook_deliveries_total":        "provider,trust_mode,verdict",
		"switchboard_routing_decisions_total":         "action,rule_id,webhook",
		"switchboard_webhook_verify_failures_total":   "provider,reason",
		"switchboard_metrics_collection_errors_total": "collector",
	}
	got := familyLabels(t, m)
	for name, labels := range want {
		if got[name] != labels {
			t.Errorf("%s: labels %q, want %q", name, got[name], labels)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected switchboard_ family %s — SPEC-0023 names every family", name)
		}
	}
}

// TestRuntimeCollectorsRegistered: go_* and process_* are part of reading everything else
// (SPEC-0023 REQ-1).
func TestRuntimeCollectorsRegistered(t *testing.T) {
	m := New(Options{})
	var goroutines, process bool
	for _, s := range gather(t, m) {
		if s.name == "go_goroutines" {
			goroutines = true
		}
		if strings.HasPrefix(s.name, "process_") {
			process = true
		}
	}
	if !goroutines {
		t.Error("go_goroutines missing from the registry")
	}
	if !process {
		t.Error("no process_ series in the registry")
	}
}

// TestHonestAbsence: a vector nobody incremented emits no series at all — never a zero that reads
// as a measurement (SPEC-0023 REQ-6). InitCollectionErrors is the one deliberate zero: a baseline
// for increase().
func TestHonestAbsence(t *testing.T) {
	m := New(Options{})
	if got := familyLabels(t, m); len(got) != 0 {
		t.Fatalf("fresh registry emits switchboard_ families %v, want none", got)
	}
	m.InitCollectionErrors("queue")
	mustValue(t, m, "switchboard_metrics_collection_errors_total", map[string]string{"collector": "queue"}, 0)
	m.CollectionError("queue")
	mustValue(t, m, "switchboard_metrics_collection_errors_total", map[string]string{"collector": "queue"}, 1)
}

func TestAttemptBuckets(t *testing.T) {
	m := New(Options{})
	for _, a := range []int{0, 1, 2, 3, 7} {
		m.TodoClaimed("forge", a)
	}
	mustValue(t, m, "switchboard_todos_claimed_total", map[string]string{"queue": "forge"}, 5)
	mustValue(t, m, "switchboard_todo_attempts_total", map[string]string{"queue": "forge", "attempt_bucket": "1"}, 2)
	mustValue(t, m, "switchboard_todo_attempts_total", map[string]string{"queue": "forge", "attempt_bucket": "2"}, 1)
	mustValue(t, m, "switchboard_todo_attempts_total", map[string]string{"queue": "forge", "attempt_bucket": "3+"}, 2)
}

// TestEnumCoercion: a label value outside its documented set is reported as __other__, never passed
// through, so a caller bug widens a label by exactly one value.
func TestEnumCoercion(t *testing.T) {
	m := New(Options{})

	m.TodoFinished("forge", OutcomeComplete)
	m.TodoFinished("forge", OutcomeFail)
	m.TodoFinished("forge", "done") // not an outcome
	mustValue(t, m, "switchboard_todos_completed_total", map[string]string{"queue": "forge", "outcome": "complete"}, 1)
	mustValue(t, m, "switchboard_todos_completed_total", map[string]string{"queue": "forge", "outcome": "fail"}, 1)
	mustValue(t, m, "switchboard_todos_completed_total", map[string]string{"queue": "forge", "outcome": Other}, 1)

	m.WebhookDelivery("github", "signed", VerdictRejected)
	m.WebhookDelivery("github", "signed", "maybe")
	mustValue(t, m, "switchboard_webhook_deliveries_total", map[string]string{"provider": "github", "trust_mode": "signed", "verdict": "rejected"}, 1)
	mustValue(t, m, "switchboard_webhook_deliveries_total", map[string]string{"provider": "github", "trust_mode": "signed", "verdict": Other}, 1)

	m.RoutingDecision("wh-1", "", ActionDrop)
	m.RoutingDecision("wh-1", "", "reroute")
	mustValue(t, m, "switchboard_routing_decisions_total", map[string]string{"webhook": "wh-1", "rule_id": "default", "action": "drop"}, 1)
	mustValue(t, m, "switchboard_routing_decisions_total", map[string]string{"webhook": "wh-1", "rule_id": "default", "action": Other}, 1)
}

// TestRuleIDLabel: only server-minted ids (rule_<24 hex>) reach the label; no match is "default";
// operator-chosen ids and rule names (free text) collapse to __other__ (SPEC-0023 REQ-4).
func TestRuleIDLabel(t *testing.T) {
	minted := "rule_0123456789abcdef01234567"
	for in, want := range map[string]string{
		"":                              RuleDefault,
		RuleDefault:                     RuleDefault,
		minted:                          minted,
		"rule_0123456789ABCDEF01234567": Other, // uppercase hex is not what NewRuleID mints
		"rule_0123":                     Other,
		"drop noisy workflow_run":       Other, // a rule NAME
		"my-custom-id":                  Other,
	} {
		if got := ruleLabel(in); got != want {
			t.Errorf("ruleLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTokenLabelBound: source/provider/trust_mode/reason/collector values outside the bounded token
// alphabet collapse to __other__ — the last-resort bound behind the callers' own enums.
func TestTokenLabelBound(t *testing.T) {
	for in, want := range map[string]string{
		"github":                      "github",
		"bad_signature":               "bad_signature",
		"self-managed":                "self-managed",
		"":                            Other,
		"GitHub":                      Other,
		"signature mismatch: got abc": Other,
		strings.Repeat("a", 33):       Other,
		"https://example.com/hook":    Other,
	} {
		if got := token(in); got != want {
			t.Errorf("token(%q) = %q, want %q", in, got, want)
		}
	}
	m := New(Options{})
	m.TodoCreated("forge", "GitHub Push")
	m.WebhookVerifyFailure("github", "HMAC mismatch for secret abc")
	mustValue(t, m, "switchboard_todos_created_total", map[string]string{"queue": "forge", "source": Other}, 1)
	mustValue(t, m, "switchboard_webhook_verify_failures_total", map[string]string{"provider": "github", "reason": Other}, 1)
}

// TestQueueCardinalityOverflow is the REQ-5 acceptance criterion: with more than 50 distinct queues
// the 51st and later report as queue="__other__" and the admitted ones keep their labels — and the
// SAME limiter answers QueueLabel, so Story 2's gauges agree with the counters.
func TestQueueCardinalityOverflow(t *testing.T) {
	m := New(Options{})
	for i := range 60 {
		m.TodoCreated(fmt.Sprintf("queue-%02d", i), "github")
	}
	for i := range 50 {
		q := fmt.Sprintf("queue-%02d", i)
		mustValue(t, m, "switchboard_todos_created_total", map[string]string{"queue": q, "source": "github"}, 1)
		if got := m.QueueLabel(q); got != q {
			t.Errorf("QueueLabel(%q) = %q after the counters admitted it; the limiter must be shared", q, got)
		}
	}
	mustValue(t, m, "switchboard_todos_created_total", map[string]string{"queue": Other, "source": "github"}, 10)
	if got := m.QueueLabel("queue-55"); got != Other {
		t.Errorf("QueueLabel(queue-55) = %q, want %q", got, Other)
	}
	queues := map[string]bool{}
	for _, s := range gather(t, m) {
		if s.name == "switchboard_todos_created_total" {
			queues[s.labels["queue"]] = true
		}
	}
	if len(queues) != 51 {
		t.Fatalf("created_total carries %d distinct queue labels, want 51 (50 admitted + __other__)", len(queues))
	}
}

func TestQueueCapOption(t *testing.T) {
	m := New(Options{QueueCap: 2})
	m.LeaseExpired("a")
	m.LeaseExpired("b")
	m.LeaseExpired("c")
	mustValue(t, m, "switchboard_todo_leases_expired_total", map[string]string{"queue": "a"}, 1)
	mustValue(t, m, "switchboard_todo_leases_expired_total", map[string]string{"queue": "b"}, 1)
	mustValue(t, m, "switchboard_todo_leases_expired_total", map[string]string{"queue": Other}, 1)
}

// TestWebhookCardinalityOverflow: webhook ids are server-minted but unbounded across the fleet, so
// they get their own limiter (SPEC-0023 REQ-4/REQ-5), independent of the queue one.
func TestWebhookCardinalityOverflow(t *testing.T) {
	m := New(Options{})
	for i := range 55 {
		m.RoutingDecision(fmt.Sprintf("wh-%02d", i), "", ActionQueue)
	}
	mustValue(t, m, "switchboard_routing_decisions_total", map[string]string{"webhook": "wh-00", "rule_id": "default", "action": "queue"}, 1)
	mustValue(t, m, "switchboard_routing_decisions_total", map[string]string{"webhook": Other, "rule_id": "default", "action": "queue"}, 5)
	// The queue limiter is untouched by webhook traffic.
	if got := m.QueueLabel("forge"); got != "forge" {
		t.Errorf("QueueLabel after webhook overflow = %q, want forge (limiters are independent)", got)
	}
}

// TestNilReceiverIsNoOp: every method on a nil *Metrics is safe, so no caller has to guard an
// increment.
func TestNilReceiverIsNoOp(t *testing.T) {
	var m *Metrics
	m.TodoCreated("q", "github")
	m.TodoClaimed("q", 1)
	m.TodoFinished("q", OutcomeComplete)
	m.LeaseExpired("q")
	m.WebhookDelivery("github", "signed", VerdictAccepted)
	m.WebhookVerifyFailure("github", "bad_signature")
	m.RoutingDecision("wh", "", ActionQueue)
	m.CollectionError("queue")
	m.InitCollectionErrors("queue")
	if m.Registry() != nil {
		t.Error("nil receiver Registry() should be nil")
	}
	if got := m.QueueLabel("q"); got != Other {
		t.Errorf("nil receiver QueueLabel = %q, want %q", got, Other)
	}
	if m.Handler("some-token-that-is-long-enough-000") == nil {
		t.Fatal("nil receiver Handler must still return a (closed) handler")
	}
}
