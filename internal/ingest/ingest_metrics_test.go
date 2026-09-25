package ingest

// Ingest And Routing Counter Tests
//
// Tests for the SPEC-0023 REQ-4 counters the self-managed receiver increments: one delivery verdict
// per delivery on every return path, one bounded verify-failure reason per refusal the delivery's
// own inputs caused, and one routing decision per persisted delivery, drops included. The table
// tests (verifyFailureReason, decisionLabels) need no database; the receiver tests are DB-backed and
// skip cleanly without SWITCHBOARD_TEST_DATABASE_URL, like the rest of this package's accept-path
// tests.
//
// The receiver tests install recordingMetrics through SetMetrics and assert the raw calls. One test
// wires the real *metrics.Metrics instead and reads the gathered series, so the literals this
// package passes are proven to survive the metrics side's label coercion. That import is test-only;
// internal/metrics imports nothing from switchboard, so there is no cycle, and production code in
// this package still never imports it.
//
// Governing: SPEC-0023 REQ-4 "Ingest and routing" (scenario "a routing rule that matches nothing"),
// REQ-5 "Cardinality"; ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for SPEC-0023 story 4 (#273).

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

type recordedDelivery struct{ provider, trustMode, verdict string }

type recordedFailure struct{ provider, reason string }

type recordedDecision struct{ webhookID, ruleID, action string }

// recordingMetrics is an ingest.Metrics that keeps every call, in order, for assertion.
type recordingMetrics struct {
	mu         sync.Mutex
	deliveries []recordedDelivery
	failures   []recordedFailure
	decisions  []recordedDecision
}

func (r *recordingMetrics) WebhookDelivery(provider, trustMode, verdict string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = append(r.deliveries, recordedDelivery{provider, trustMode, verdict})
}

func (r *recordingMetrics) WebhookVerifyFailure(provider, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, recordedFailure{provider, reason})
}

func (r *recordingMetrics) RoutingDecision(webhookID, ruleID, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, recordedDecision{webhookID, ruleID, action})
}

// snapshot copies the calls so far and clears them, so a test can assert one step at a time.
func (r *recordingMetrics) snapshot() ([]recordedDelivery, []recordedFailure, []recordedDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, f, c := r.deliveries, r.failures, r.decisions
	r.deliveries, r.failures, r.decisions = nil, nil, nil
	return d, f, c
}

// expectCalls asserts exactly these calls were recorded since the last snapshot. nil and empty are
// the same expectation.
func expectCalls(t *testing.T, rec *recordingMetrics, wantD []recordedDelivery, wantF []recordedFailure, wantC []recordedDecision) {
	t.Helper()
	d, f, c := rec.snapshot()
	if !slices.Equal(d, wantD) {
		t.Errorf("deliveries = %+v, want %+v", d, wantD)
	}
	if !slices.Equal(f, wantF) {
		t.Errorf("verify failures = %+v, want %+v", f, wantF)
	}
	if !slices.Equal(c, wantC) {
		t.Errorf("routing decisions = %+v, want %+v", c, wantC)
	}
}

// repeatDelivery is n copies of one delivery verdict.
func repeatDelivery(n int, d recordedDelivery) []recordedDelivery {
	out := make([]recordedDelivery, n)
	for i := range out {
		out[i] = d
	}
	return out
}

func slackSig(secret, ts, body string) string {
	return "v0=" + hmacHex(secret, "v0:"+ts+":"+body)
}

func stripeSig(secret string, ts int64, body string) string {
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hmacHex(secret, strconv.FormatInt(ts, 10)+"."+body)
}

// hmacHex is the bare hex HMAC-SHA256 of msg (giteaSig's shape, over an arbitrary string).
func hmacHex(secret, msg string) string {
	return giteaSig(secret, msg)
}

// verifyFailureReason names the bounded failure mode for every refusal each signed scheme can
// produce, and every case is cross-checked against the real verifier: the classifier's premise is
// that it only ever sees a delivery the verifier refused, so a case the verifier would ACCEPT is
// itself a test bug. The one accepted case pins that a fully valid cairn delivery is not blamed on
// any named mode. Governing: SPEC-0023 REQ-4 (reason is a bounded enum), REQ-5.
func TestVerifyFailureReason(t *testing.T) {
	const secret = "whsec_reason"
	now := time.Unix(1_800_000_000, 0)
	fresh := now.Unix()
	stale := now.Add(-2 * defaultReplayTolerance).Unix()
	ing := &Ingest{now: func() time.Time { return now }, tolerance: defaultReplayTolerance}

	const body = `{"action":"opened"}`
	cairn := func(eventID, createdAt string) string {
		return fmt.Sprintf(`{"event_id":%q,"kind":"artifact.created","created_at":%q}`, eventID, createdAt)
	}
	freshAt := now.UTC().Format(time.RFC3339Nano)
	staleAt := now.Add(-2 * defaultReplayTolerance).UTC().Format(time.RFC3339Nano)
	cairnOK := cairn("evt-1", freshAt)

	cases := []struct {
		name, source, body string
		hdr                map[string]string
		want               string
	}{
		{"github missing", "github", body, nil, reasonMissingSignature},
		{"github unprefixed", "github", body, map[string]string{"X-Hub-Signature-256": "deadbeef"}, reasonMalformedSignature},
		{"github mismatch", "github", body, map[string]string{"X-Hub-Signature-256": githubSig("wrong", body)}, reasonBadSignature},

		{"gitea native mismatch", "gitea", body, map[string]string{"X-Gitea-Signature": giteaSig("wrong", body)}, reasonBadSignature},
		{"gitea missing both", "gitea", body, nil, reasonMissingSignature},
		{"gitea hub unprefixed", "gitea", body, map[string]string{"X-Hub-Signature-256": "deadbeef"}, reasonMalformedSignature},
		{"gitea hub mismatch", "gitea", body, map[string]string{"X-Hub-Signature-256": githubSig("wrong", body)}, reasonBadSignature},

		{"stripe missing", "stripe", body, nil, reasonMissingSignature},
		{"stripe garbage", "stripe", body, map[string]string{"Stripe-Signature": "garbage"}, reasonMalformedSignature},
		{"stripe no v1", "stripe", body, map[string]string{"Stripe-Signature": "t=" + strconv.FormatInt(fresh, 10)}, reasonMalformedSignature},
		{"stripe stale", "stripe", body, map[string]string{"Stripe-Signature": stripeSig(secret, stale, body)}, reasonStaleTimestamp},
		{"stripe mismatch", "stripe", body, map[string]string{"Stripe-Signature": stripeSig("wrong", fresh, body)}, reasonBadSignature},

		{"slack missing", "slack", body, nil, reasonMissingSignature},
		{"slack unprefixed", "slack", body, map[string]string{"X-Slack-Signature": "abc",
			"X-Slack-Request-Timestamp": strconv.FormatInt(fresh, 10)}, reasonMalformedSignature},
		{"slack no timestamp", "slack", body, map[string]string{"X-Slack-Signature": slackSig(secret, "0", body)}, reasonMalformedSignature},
		{"slack bad timestamp", "slack", body, map[string]string{"X-Slack-Signature": slackSig(secret, "x", body),
			"X-Slack-Request-Timestamp": "x"}, reasonMalformedSignature},
		{"slack stale", "slack", body, map[string]string{"X-Slack-Signature": slackSig(secret, strconv.FormatInt(stale, 10), body),
			"X-Slack-Request-Timestamp": strconv.FormatInt(stale, 10)}, reasonStaleTimestamp},
		{"slack non-hex", "slack", body, map[string]string{"X-Slack-Signature": "v0=zz",
			"X-Slack-Request-Timestamp": strconv.FormatInt(fresh, 10)}, reasonMalformedSignature},
		{"slack mismatch", "slack", body, map[string]string{"X-Slack-Signature": slackSig("wrong", strconv.FormatInt(fresh, 10), body),
			"X-Slack-Request-Timestamp": strconv.FormatInt(fresh, 10)}, reasonBadSignature},

		{"cairn missing", routing.SourceCairn, cairnOK, nil, reasonMissingSignature},
		{"cairn unprefixed", routing.SourceCairn, cairnOK, map[string]string{"X-Cairn-Signature": "deadbeef"}, reasonMalformedSignature},
		{"cairn mismatch", routing.SourceCairn, cairnOK, map[string]string{"X-Cairn-Signature": cairnSig("wrong", cairnOK)}, reasonBadSignature},
		{"cairn no event id", routing.SourceCairn, cairn("", freshAt),
			map[string]string{"X-Cairn-Signature": cairnSig(secret, cairn("", freshAt))}, reasonMalformedBody},
		{"cairn not json", routing.SourceCairn, "not json",
			map[string]string{"X-Cairn-Signature": cairnSig(secret, "not json")}, reasonMalformedBody},
		{"cairn event id mismatch", routing.SourceCairn, cairnOK,
			map[string]string{"X-Cairn-Signature": cairnSig(secret, cairnOK), "X-Cairn-Event-Id": "evt-other"}, reasonEventIDMismatch},
		{"cairn bad created_at", routing.SourceCairn, cairn("evt-1", "yesterday"),
			map[string]string{"X-Cairn-Signature": cairnSig(secret, cairn("evt-1", "yesterday"))}, reasonMalformedBody},
		{"cairn stale", routing.SourceCairn, cairn("evt-1", staleAt),
			map[string]string{"X-Cairn-Signature": cairnSig(secret, cairn("evt-1", staleAt))}, reasonStaleTimestamp},
		{"cairn valid", routing.SourceCairn, cairnOK,
			map[string]string{"X-Cairn-Signature": cairnSig(secret, cairnOK), "X-Cairn-Event-Id": "evt-1"}, reasonOther},

		{"unknown source", "docker", body, nil, reasonOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhooks/w/x", strings.NewReader(tc.body))
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			valid, err := ing.verifySelfManagedSigned(req, tc.source, secret, []byte(tc.body))
			switch {
			case tc.source == "docker":
				if err == nil {
					t.Fatalf("verifier accepted unknown source %q", tc.source)
				}
			case err != nil:
				t.Fatalf("verifier error: %v", err)
			case valid != (tc.name == "cairn valid"):
				t.Fatalf("verifier valid = %v; the case does not exercise the refusal it names", valid)
			}
			if got := verifyFailureReason(tc.source, secret, req.Header, []byte(tc.body), now, defaultReplayTolerance); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// decisionLabels reads the decision routing.Decide actually builds, so a change to the trace's shape
// breaks here rather than silently relabelling the counter. A matched rule that can no longer reach
// its queue is decided by the default, and reports as the default even though the trace names it.
// Governing: SPEC-0023 REQ-4 (rule_id from the matched rule, "default" when none).
func TestDecisionLabels(t *testing.T) {
	rid := routing.NewRuleID()
	g := routing.Grant{TargetQueue: "inbox", Endpoints: []string{"ep-1"}}
	idx := func(i int) *int { return &i }
	dropRule := routing.Rule{ID: rid, Name: "noise", Expr: "true", Action: routing.Action{Drop: true}}
	queueRule := routing.Rule{ID: rid, Name: "triage", Expr: "true", Action: routing.Action{Queue: "inbox"}}
	lostRule := routing.Rule{ID: rid, Name: "gone", Expr: "true", Action: routing.Action{Queue: "revoked"}}

	cases := []struct {
		name       string
		cfg        routing.Config
		m          routing.MatchResult
		wantRuleID string
		wantAction string
	}{
		{"rule drops", routing.Config{Rules: []routing.Rule{dropRule}}, routing.MatchResult{RuleIndex: idx(0)}, rid, actionDrop},
		{"rule queues", routing.Config{Rules: []routing.Rule{queueRule}}, routing.MatchResult{RuleIndex: idx(0)}, rid, actionQueue},
		{"no match takes the target queue", routing.Config{Rules: []routing.Rule{dropRule}}, routing.MatchResult{}, "", actionQueue},
		{"no rules at all", routing.Config{}, routing.MatchResult{}, "", actionQueue},
		{"default drops", routing.Config{Default: &routing.Action{Drop: true}}, routing.MatchResult{}, "", actionDrop},
		{"matched rule not granted falls to the default", routing.Config{Rules: []routing.Rule{lostRule}},
			routing.MatchResult{RuleIndex: idx(0)}, "", actionQueue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := routing.Decide(tc.cfg, g, tc.m)
			if gotRule, gotAction := decisionLabels(d); gotRule != tc.wantRuleID || gotAction != tc.wantAction {
				t.Fatalf("labels = (%q, %q), want (%q, %q); trace %+v", gotRule, gotAction, tc.wantRuleID, tc.wantAction, d.Trace)
			}
		})
	}
}

// An oversized body is refused before any webhook resolves, so it is counted against the unknown
// provider — and still counted exactly once. The nil store proves nothing is looked up.
func TestSelfManagedMetricsOversizedBody(t *testing.T) {
	ing := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)
	if got := postSelfManaged(ing, "any-token", strings.Repeat("x", maxBody+1), nil); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", got.Code)
	}
	expectCalls(t, rec,
		[]recordedDelivery{{labelUnknown, labelUnknown, verdictRejected}},
		[]recordedFailure{{labelUnknown, reasonTooLarge}},
		nil)
}

// A body that could not be read at all is the other half of readBody's failure contract: refused
// 400 before any lookup (the nil store proves that) and counted as rejected with reasonUnreadable,
// never too_large. The two share a return path and are told apart only by the error, so this is the
// one branch that needs a reader which fails rather than one that is too big.
func TestSelfManagedMetricsUnreadableBody(t *testing.T) {
	ing := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/w/any-token", failingReader{})
	ing.SelfManaged(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", recorder.Code)
	}
	expectCalls(t, rec,
		[]recordedDelivery{{labelUnknown, labelUnknown, verdictRejected}},
		[]recordedFailure{{labelUnknown, reasonUnreadable}},
		nil)
}

// failingReader fails on its first Read, standing in for a client that disconnects mid-body. Its
// error is not an *http.MaxBytesError, which is exactly what separates reasonUnreadable from
// reasonTooLarge in readBody.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// An unknown token names no webhook: rejected, unknown_webhook, no routing decision.
func TestSelfManagedMetricsUnknownToken(t *testing.T) {
	ing, _, _, _, _ := testIngestDeps(t, Config{})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)
	if got := postSelfManaged(ing, "no-such-token", `{}`, nil); got.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", got.Code)
	}
	expectCalls(t, rec,
		[]recordedDelivery{{labelUnknown, labelUnknown, verdictRejected}},
		[]recordedFailure{{labelUnknown, reasonUnknownWebhook}},
		nil)
}

// A delivery that fails signature verification is rejected and counts its bounded reason against
// the webhook's provider; it is refused before routing, so it counts no decision.
func TestSelfManagedMetricsSignatureFailureIsRejected(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_metrics_sig"
	seedWebhook(t, st, ctx, "github", "signed", "reviews", "metrics-sig", secret)
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	body := `{"action":"opened"}`
	if got := postSelfManaged(ing, "metrics-sig", body, map[string]string{
		"X-GitHub-Delivery": "d-bad", "X-Hub-Signature-256": githubSig("not-the-secret", body)}); got.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d, want 401", got.Code)
	}
	if got := postSelfManaged(ing, "metrics-sig", body, map[string]string{"X-GitHub-Delivery": "d-none"}); got.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: got %d, want 401", got.Code)
	}
	expectCalls(t, rec,
		repeatDelivery(2, recordedDelivery{"github", "signed", verdictRejected}),
		[]recordedFailure{{"github", reasonBadSignature}, {"github", reasonMissingSignature}},
		nil)
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("events after rejected deliveries = %d, want 0", n)
	}
}

// The silent-rule scenario (SPEC-0023 REQ-4): a drop rule whose event-kind string has a typo matches
// nothing, so deliveries it was meant to drop are queued by the default, and its rule_id never
// appears with action "drop". Fixing the typo makes the same rule id start dropping — the positive
// control that proves the assertion above is not vacuous — and the drop is the delivery's verdict.
func TestSelfManagedMetricsSilentRuleNeverFires(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	_, _, wh := seedWebhook(t, st, ctx, "generic", "token", "forge", "metrics-silent", "")
	rid := routing.NewRuleID()
	setRules(t, ctx, st, wh.ID, routing.Config{Rules: []routing.Rule{
		{ID: rid, Name: "drop ci", Expr: `.kind == "workflow_runs"`, Action: routing.Action{Drop: true}},
	}})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	deliver := func(delivery string) *httptest.ResponseRecorder {
		return postSelfManaged(ing, "metrics-silent", `{"action":"completed"}`,
			map[string]string{"X-Gitea-Event": "workflow_run", "X-Gitea-Delivery": delivery})
	}
	for n := range 3 {
		if r := routed(t, deliver(fmt.Sprintf("d-ci-%d", n))); r.Dropped || r.Created != 1 {
			t.Fatalf("delivery %d = %+v, want queued by the default", n, r)
		}
	}
	d, f, c := rec.snapshot()
	for _, dec := range c {
		if dec.ruleID == rid {
			t.Fatalf("the silent rule %s produced decision %+v, want none", rid, dec)
		}
	}
	if want := []recordedDecision{{wh.ID, "", actionQueue}, {wh.ID, "", actionQueue}, {wh.ID, "", actionQueue}}; !slices.Equal(c, want) {
		t.Fatalf("decisions = %+v, want three default queue decisions %+v", c, want)
	}
	if want := repeatDelivery(3, recordedDelivery{"generic", "token", verdictAccepted}); !slices.Equal(d, want) {
		t.Fatalf("deliveries = %+v, want %+v", d, want)
	}
	if len(f) != 0 {
		t.Fatalf("verify failures = %+v, want none", f)
	}

	setRules(t, ctx, st, wh.ID, routing.Config{Rules: []routing.Rule{
		{ID: rid, Name: "drop ci", Expr: `.kind == "workflow_run"`, Action: routing.Action{Drop: true}},
	}})
	if r := routed(t, deliver("d-ci-fixed")); !r.Dropped {
		t.Fatalf("delivery after the fix = %+v, want dropped", r)
	}
	expectCalls(t, rec,
		[]recordedDelivery{{"generic", "token", verdictDropped}},
		nil,
		[]recordedDecision{{wh.ID, rid, actionDrop}})
}

// An idempotent redelivery is still an accepted delivery, and a delivery that fans out to two
// targets is one routing decision, not one per todo.
func TestSelfManagedMetricsDedupIsAcceptedOncePerDelivery(t *testing.T) {
	ing, _, st, _, ctx := perTargetDeps(t)
	const secret = "whsec_metrics_dedup"
	_, _, wh := seedRoutedWebhook(t, st, ctx, "metrics-dedup", secret)
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	body := `{"action":"opened","pull_request":{"number":7,"title":"x"}}`
	hdr := signedHeaders(secret, body, "guid-metrics-dedup")
	for n, wantCreated := range []int{2, 0} {
		todos, created := fanOut202(t, postSelfManaged(ing, "metrics-dedup", body, hdr))
		if len(todos) != 2 || created != wantCreated {
			t.Fatalf("delivery %d: %d todos, %d created; want 2 todos, %d created", n, len(todos), created, wantCreated)
		}
	}
	expectCalls(t, rec,
		repeatDelivery(2, recordedDelivery{"github", "signed", verdictAccepted}),
		nil,
		[]recordedDecision{{wh.ID, "", actionQueue}, {wh.ID, "", actionQueue}})
}

// A refusal the server causes after verification passed is rejected but counts no verify failure:
// rejected minus verify failures is the server's own share. A webhook whose only target was revoked
// resolves no targets and is refused 503 before routing, so it counts no decision either.
func TestSelfManagedMetricsServerRefusalCountsNoVerifyFailure(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_metrics_revoked"
	h, owner, _ := seedWebhook(t, st, ctx, "github", "signed", "reviews", "metrics-revoked", secret)
	if err := st.RevokeEndpoint(ctx, owner.ID, h.ID); err != nil {
		t.Fatalf("revoke owner: %v", err)
	}
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)

	body := `{"action":"opened"}`
	if got := postSelfManaged(ing, "metrics-revoked", body, signedHeaders(secret, body, "guid-revoked")); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (body %s)", got.Code, got.Body.String())
	}
	expectCalls(t, rec,
		[]recordedDelivery{{"github", "signed", verdictRejected}},
		nil,
		nil)
}

// gatherCounters scrapes the real metric surface into "name{label="value",...}" → value.
func gatherCounters(t *testing.T, m *metrics.Metrics) map[string]float64 {
	t.Helper()
	fams, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		if !strings.HasPrefix(f.GetName(), "switchboard_") {
			continue
		}
		for _, s := range f.GetMetric() {
			pairs := make([]string, 0, len(s.GetLabel()))
			for _, l := range s.GetLabel() {
				pairs = append(pairs, l.GetName()+"="+strconv.Quote(l.GetValue()))
			}
			sort.Strings(pairs)
			out[f.GetName()+"{"+strings.Join(pairs, ",")+"}"] = s.GetCounter().GetValue()
		}
	}
	return out
}

// End to end against the real metric surface: every literal this package passes reaches the scrape
// as itself rather than being coerced to __other__, and a webhook past the cap reports as
// webhook="__other__" (the cap is 1 here so two webhooks overflow it, the same limiter the default
// cap of 50 uses). Governing: SPEC-0023 REQ-4, REQ-5 (webhook overflow).
func TestSelfManagedMetricsExposition(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	_, _, first := seedWebhook(t, st, ctx, "generic", "token", "forge", "metrics-first", "")
	seedWebhook(t, st, ctx, "generic", "token", "forge", "metrics-second", "")
	const secret = "whsec_metrics_expo"
	seedWebhook(t, st, ctx, "github", "signed", "reviews", "metrics-signed", secret)
	m := metrics.New(metrics.Options{WebhookCap: 1})
	ing.SetMetrics(m)

	accepted202(t, postSelfManaged(ing, "metrics-first", `{"n":1}`, nil))
	accepted202(t, postSelfManaged(ing, "metrics-second", `{"n":2}`, nil))
	body := `{"action":"opened"}`
	postSelfManaged(ing, "metrics-signed", body, map[string]string{"X-Hub-Signature-256": githubSig("wrong", body)})
	postSelfManaged(ing, "metrics-signed", body, map[string]string{"X-Hub-Signature-256": "deadbeef"})
	postSelfManaged(ing, "no-such-token", body, nil)

	got := gatherCounters(t, m)
	want := map[string]float64{
		`switchboard_webhook_deliveries_total{provider="generic",trust_mode="token",verdict="accepted"}`:   2,
		`switchboard_webhook_deliveries_total{provider="github",trust_mode="signed",verdict="rejected"}`:   2,
		`switchboard_webhook_deliveries_total{provider="unknown",trust_mode="unknown",verdict="rejected"}`: 1,
		`switchboard_webhook_verify_failures_total{provider="github",reason="bad_signature"}`:              1,
		`switchboard_webhook_verify_failures_total{provider="github",reason="malformed_signature"}`:        1,
		`switchboard_webhook_verify_failures_total{provider="unknown",reason="unknown_webhook"}`:           1,
		`switchboard_routing_decisions_total{action="queue",rule_id="default",webhook="` + first.ID + `"}`: 1,
		`switchboard_routing_decisions_total{action="queue",rule_id="default",webhook="__other__"}`:        1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok && (strings.Contains(k, "webhook") || strings.Contains(k, "routing")) {
			t.Errorf("unexpected series %s = %v", k, got[k])
		}
	}
}
