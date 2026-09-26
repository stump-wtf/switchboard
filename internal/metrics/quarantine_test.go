package metrics

// Quarantine and routing-fault metrics, and the documented alerts
//
// Pins SPEC-0026 REQ-11: the quarantine counters' bounded label sets (a resolver's id or slug never
// reaches a label), the unlabeled switchboard_quarantine_oldest_seconds gauge the queue collector
// derives from the quarantine queue's row, and the alert rules published in the self-hosting guide.
// The alerts test reads the rules block straight out of docs/guides/14-self-hosting.md and evaluates
// the queue alerts against the real collector's output, so the documented liveness alert cannot
// drift back into firing on quarantine.
//
// Governing: SPEC-0026 REQ-11 "Metrics" (scenario "Liveness alert ignores quarantine"); SPEC-0023
// REQ-2, REQ-5; ADR-0028, ADR-0031.
//
// @joestump-agent 09/25/2026 - Added for #392.

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

func TestQuarantineCounters(t *testing.T) {
	m := New(Options{})
	for _, r := range []string{QuarantineUntrustedActor, QuarantineUntrustedActor, QuarantineRuleFault, QuarantineRuleAction, "because"} {
		m.QuarantineHeld(r)
	}
	for label, want := range map[string]float64{
		QuarantineUntrustedActor: 2, QuarantineRuleFault: 1, QuarantineRuleAction: 1, Other: 1,
	} {
		mustValue(t, m, "switchboard_quarantine_items_total", map[string]string{"reason": label}, want)
	}

	resolutions := []struct{ outcome, by string }{
		{ResolvedReleased, "human:3f8a2c1e-0000-4000-8000-000000000001"},
		{ResolvedReleased, "human:another-human"},
		{ResolvedReleased, "classifier:triage-bot"},
		{ResolvedDiscarded, "human:3f8a2c1e-0000-4000-8000-000000000001"},
		{ResolvedDiscarded, "classifier:triage-bot"},
		{ResolvedExpired, "system"},
		{"forgotten", "system"},
		{ResolvedDiscarded, "mallory"},
		{ResolvedDiscarded, ""},
	}
	for _, r := range resolutions {
		m.QuarantineResolved(r.outcome, r.by)
	}
	want := map[[2]string]float64{
		{ResolvedReleased, ResolvedByHuman}:       2,
		{ResolvedReleased, ResolvedByClassifier}:  1,
		{ResolvedDiscarded, ResolvedByHuman}:      1,
		{ResolvedDiscarded, ResolvedByClassifier}: 1,
		{ResolvedExpired, ResolvedBySystem}:       1,
		{Other, ResolvedBySystem}:                 1,
		{ResolvedDiscarded, Other}:                2,
	}
	for k, v := range want {
		mustValue(t, m, "switchboard_quarantine_resolved_total", map[string]string{"outcome": k[0], "by": k[1]}, v)
	}
	// Bounded: the resolver's id and slug never become label values.
	for _, s := range gather(t, m) {
		if s.name != "switchboard_quarantine_resolved_total" {
			continue
		}
		if strings.Contains(s.labels["by"], ":") || strings.Contains(s.labels["by"], "triage-bot") {
			t.Errorf("resolver identity reached a label: %v", s.labels)
		}
	}
}

// The gauge is the quarantine queue's oldest-pending age, taken before label limiting, so it still
// reads correctly when "quarantine" itself has folded into __other__ on the queue families.
func TestQuarantineOldestGauge(t *testing.T) {
	cases := map[string]struct {
		stats []store.QueueStat
		cap   int
		want  float64
	}{
		"held items": {stats: []store.QueueStat{
			{Queue: "forge", Pending: 3, OldestPendingSeconds: 90000},
			{Queue: store.QueueQuarantine, Pending: 5, OldestPendingSeconds: 3600},
		}, want: 3600},
		"nothing held reports zero": {stats: []store.QueueStat{{Queue: "forge", Pending: 1, OldestPendingSeconds: 50}}, want: 0},
		"quarantine past the queue cap": {stats: []store.QueueStat{
			{Queue: "alpha"}, {Queue: store.QueueQuarantine, Pending: 1, OldestPendingSeconds: 7200},
		}, cap: 1, want: 7200},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := New(Options{QueueCap: c.cap})
			m.RegisterQueueStats(&fakeQueueStats{stats: c.stats})
			series := gatherQueueSeries(t, m.Registry())
			got, ok := series["switchboard_quarantine_oldest_seconds"]
			if !ok || got != c.want {
				t.Fatalf("switchboard_quarantine_oldest_seconds = %v (present %v), want %v", got, ok, c.want)
			}
		})
	}
}

// ---- The documented alerts -------------------------------------------------------------------

const selfHostingGuide = "../../docs/guides/14-self-hosting.md"

var alertsBlock = regexp.MustCompile("(?s)```yaml title=\"switchboard-alerts.yaml\"\\n(.*?)\\n```")

// documentedAlerts reads the alert name → expression pairs from the guide's rules block. It handles
// the two shapes the block uses: `expr: <inline>` and a `expr: |` literal block.
func documentedAlerts(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(selfHostingGuide))
	if err != nil {
		t.Fatalf("read guide: %v", err)
	}
	m := alertsBlock.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal(`no "switchboard-alerts.yaml" block in the self-hosting guide; the title changed`)
	}
	out := map[string]string{}
	lines := strings.Split(m[1], "\n")
	var name string
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(l, "- alert:"):
			name = strings.TrimSpace(strings.TrimPrefix(l, "- alert:"))
		case l == "expr: |":
			indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			var parts []string
			for i+1 < len(lines) && len(lines[i+1])-len(strings.TrimLeft(lines[i+1], " ")) > indent {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			out[name] = strings.Join(parts, " ")
		case strings.HasPrefix(l, "expr:"):
			out[name] = strings.TrimSpace(strings.TrimPrefix(l, "expr:"))
		}
	}
	return out
}

// A deliberately small PromQL subset: `SEL CMP NUM`, optionally joined by `and ignoring(L, …)`.
// Anything else fails the test loudly rather than being evaluated wrongly.
var (
	comparisonExpr = regexp.MustCompile(`^([a-z_:][a-z0-9_:]*)(?:\{([^}]*)\})?\s*(>|<|==|!=|>=|<=)\s*([0-9.]+)$`)
	matcherExpr    = regexp.MustCompile(`^([a-z_]+)(=|!=)"([^"]*)"$`)
	andIgnoring    = regexp.MustCompile(`^(.*?)\s+and\s+ignoring\(([a-z_, ]*)\)\s+(.*)$`)
	metricNameRef  = regexp.MustCompile(`switchboard_[a-z_]+`)
)

// evalAlert returns the label sets the expression fires for over samples.
func evalAlert(t *testing.T, expr string, samples []series) []map[string]string {
	t.Helper()
	if m := andIgnoring.FindStringSubmatch(expr); m != nil {
		var ignored []string
		for _, l := range strings.Split(m[2], ",") {
			ignored = append(ignored, strings.TrimSpace(l))
		}
		key := func(labels map[string]string) string {
			var b strings.Builder
			keys := slices.Sorted(maps.Keys(labels))
			for _, k := range keys {
				if !slices.Contains(ignored, k) {
					b.WriteString(k + "=" + labels[k] + ",")
				}
			}
			return b.String()
		}
		right := map[string]bool{}
		for _, r := range evalAlert(t, m[3], samples) {
			right[key(r)] = true
		}
		var out []map[string]string
		for _, l := range evalAlert(t, m[1], samples) {
			if right[key(l)] {
				out = append(out, l)
			}
		}
		return out
	}
	m := comparisonExpr.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		t.Fatalf("documented expression %q is outside the subset this test evaluates; extend the test", expr)
	}
	type matcher struct{ name, op, value string }
	var matchers []matcher
	if m[2] != "" {
		for _, part := range strings.Split(m[2], ",") {
			mm := matcherExpr.FindStringSubmatch(strings.TrimSpace(part))
			if mm == nil {
				t.Fatalf("matcher %q in %q is outside the evaluated subset", part, expr)
			}
			matchers = append(matchers, matcher{mm[1], mm[2], mm[3]})
		}
	}
	threshold, err := strconv.ParseFloat(m[4], 64)
	if err != nil {
		t.Fatalf("threshold in %q: %v", expr, err)
	}
	var out []map[string]string
	for _, s := range samples {
		if s.name != m[1] {
			continue
		}
		ok := true
		for _, mt := range matchers {
			if (s.labels[mt.name] == mt.value) != (mt.op == "=") {
				ok = false
				break
			}
		}
		if ok && compare(s.value, m[3], threshold) {
			out = append(out, s.labels)
		}
	}
	return out
}

func compare(v float64, op string, th float64) bool {
	switch op {
	case ">":
		return v > th
	case "<":
		return v < th
	case ">=":
		return v >= th
	case "<=":
		return v <= th
	case "==":
		return v == th
	default:
		return v != th
	}
}

// SPEC-0026 REQ-11 "Liveness alert ignores quarantine": five open quarantine items and no claims do
// not fire the documented liveness alert, while a genuinely stalled queue beside them still does;
// the quarantine age alert fires on the old item instead. Every family any documented alert names is
// one the registry actually serves, so a renamed metric cannot leave an alert that never fires.
func TestDocumentedAlerts(t *testing.T) {
	alerts := documentedAlerts(t)
	liveness, age := alerts["SwitchboardQueueStalled"], alerts["SwitchboardQuarantineWaiting"]
	if liveness == "" || age == "" {
		t.Fatalf("the guide no longer documents SwitchboardQueueStalled and SwitchboardQuarantineWaiting: %v", alerts)
	}

	m := New(Options{})
	m.RegisterQueueStats(&fakeQueueStats{stats: []store.QueueStat{
		{Queue: "forge", Pending: 50, OldestPendingSeconds: 72000},
		{Queue: "reviews", Pending: 2, Claimed: 1, OldestPendingSeconds: 30},
		{Queue: store.QueueQuarantine, Pending: 5, OldestPendingSeconds: 2 * 86400},
	}})
	// Touch every counter family so the name check below sees the full surface.
	m.RoutingFault("timeout")
	m.QuarantineHeld(QuarantineUntrustedActor)
	m.QuarantineResolved(ResolvedExpired, "system")
	m.CollectionError("probe")
	samples := gather(t, m)

	var fired []string
	for _, l := range evalAlert(t, liveness, samples) {
		fired = append(fired, l["queue"])
	}
	if len(fired) != 1 || fired[0] != "forge" {
		t.Errorf("liveness alert fired for %v, want exactly [forge]: never quarantine, never a queue being drained", fired)
	}
	if got := evalAlert(t, age, samples); len(got) != 1 {
		t.Errorf("quarantine age alert fired %d times on a two-day-old held item, want 1", len(got))
	}

	// Without the exclusion the liveness alert would fire on quarantine: the test is not vacuous.
	unguarded := strings.ReplaceAll(liveness, `,queue!="quarantine"`, "")
	if got := evalAlert(t, unguarded, samples); len(got) != 2 {
		t.Errorf("unguarded liveness alert fired %d times, want 2 (forge and quarantine)", len(got))
	}

	// Quarantine is a reserved queue label: however many operator queues fill the cap first, it is
	// reported as itself, so the exclusion above still matches it. Were it folded into __other__,
	// its never-claimed rows would fire the liveness alert as queue="__other__". QueueStats sorts
	// by name, so "quarantine" really does arrive after most operator queues on a busy instance.
	capped := New(Options{QueueCap: 2})
	capped.RegisterQueueStats(&fakeQueueStats{stats: []store.QueueStat{
		{Queue: "alpha", Pending: 1, Claimed: 1},
		{Queue: "beta", Claimed: 1},
		{Queue: store.QueueQuarantine, Pending: 5, OldestPendingSeconds: 3600},
	}})
	if got := evalAlert(t, liveness, gather(t, capped)); len(got) != 0 {
		t.Errorf("liveness alert fired for %v with quarantine past the queue cap, want nothing", got)
	}

	families := map[string]bool{}
	for _, s := range samples {
		families[s.name] = true
	}
	for name, expr := range alerts {
		for _, ref := range metricNameRef.FindAllString(expr, -1) {
			if !families[ref] {
				t.Errorf("alert %s reads %s, which the registry does not serve", name, ref)
			}
		}
	}
}

// SwitchboardRoutingFaults is increase(...[15m]) > 0. A series that first appears at 1 has no
// earlier sample, so increase() over it is 0 and the first fault after every restart would go
// unalerted. New therefore pre-creates every bounded cause at zero, the same baseline
// InitCollectionErrors gives the collector-failure alert.
func TestRoutingFaultSeriesHaveABaseline(t *testing.T) {
	m := New(Options{})
	got := map[string]float64{}
	for _, s := range gather(t, m) {
		if s.name == "switchboard_routing_faults_total" {
			got[s.labels["cause"]] = s.value
		}
	}
	for _, cause := range []string{FaultCauseTimeout, FaultCauseError, FaultCauseCompile, FaultCauseBudget} {
		if v, ok := got[cause]; !ok || v != 0 {
			t.Errorf("switchboard_routing_faults_total{cause=%q} = %v (present %v) before any fault, want 0", cause, v, ok)
		}
	}
	if _, ok := got[Other]; ok {
		t.Errorf("the overflow cause %q is pre-created; only the four named causes need a baseline", Other)
	}
}
