package ingest

// Routing fails closed at the receiver (ADR-0031, SPEC-0026). A delivery whose rules fault is
// recorded with its trace and disposition=faulted, mints no todo, rings nothing, spends its dedup
// slot, is counted once in switchboard_routing_faults_total, and is logged with exactly one warning
// naming the webhook, rule, index and cause. A webhook with rules whose evaluator cannot run at all
// answers 503 with nothing persisted, so the producer retries. A webhook without rules never needs
// the evaluator and is unaffected.
//
// Before #212 a fault was treated as no-match: the delivery fell through to the next rule or the
// default, so a broken drop rule stopped dropping and a mistyped trust rule admitted everyone.
//
// Governing: ADR-0031, SPEC-0026 REQ-1 "Faults Stop Evaluation", REQ-2 "Unavailable Sandbox Refuses
// the Delivery", REQ-13; SPEC-0020 REQ "Routing Trace".

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// lockedBuffer collects log output safely. The doorbell hub publishes from another goroutine, so an
// unguarded bytes.Buffer here would be a data race under `go test -race` — which CI runs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ingestWithLogCapture mirrors testIngestDeps but keeps the log instead of discarding it. It builds
// Ingest directly rather than adding a parameter to the shared helper, so every other ingest test
// keeps its io.Discard logger untouched.
func ingestWithLogCapture(t *testing.T, cfg Config) (*Ingest, *pgxpool.Pool, context.Context, *lockedBuffer) {
	t.Helper()
	pool, ctx := ingestTestPool(t)
	st := store.New(pool)
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(st, NewHub(), log, cfg), pool, ctx, logs
}

func countWhere(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// SPEC-0026 REQ-1 scenario "A mistyped trust rule no longer admits everyone", end to end through the
// receiver: r1 faults on a string-typed allowlist, r2 is never evaluated, nothing lands on inbox, and
// the delivery is recorded as faulted, counted and warned once. A redelivery after the owner fixes
// the rules stays faulted, because the fault spent the dedup slot exactly as a drop does.
func TestSelfManagedFaultedDeliveryIsRecordedNotRouted(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-fault", cairnSecret)

	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{
		Rules: []routing.Rule{
			{ID: "trust", Expr: `.artifact.actor_id as $a | any($params.trusted[]; . == $a) | not`, Action: routing.Action{Drop: true}},
			{ID: "ok", Expr: `true`, Action: routing.Action{Queue: "inbox"}},
		},
		Params: map[string]any{"trusted": "alice"}, // a string, not a list
	})

	body := cairnBody("evt-fault", time.Now(), "from mallory")
	resp := postSelfManaged(ing, "cairn-fault", body, cairnHeaders(body, "evt-fault"))
	if resp.Code != http.StatusAccepted || !strings.Contains(resp.Body.String(), `"faulted":true`) ||
		!strings.Contains(resp.Body.String(), `"created":0`) {
		t.Fatalf("faulted delivery = %d %s, want 202 faulted with nothing created", resp.Code, resp.Body.String())
	}
	if b := resp.Body.String(); strings.Contains(b, `"trust"`) || strings.Contains(b, "rule") {
		t.Fatalf("response %s names the owner's rule; the fault is not the producer's business", resp.Body.String())
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 0 {
		t.Fatalf("todos = %d, want none: a faulted delivery routes nowhere", n)
	}
	var disp string
	if err := pool.QueryRow(ctx, `SELECT disposition FROM events WHERE webhook_id = $1`, wh.ID).Scan(&disp); err != nil || disp != store.DispositionFaulted {
		t.Fatalf("event disposition = %q (%v), want faulted", disp, err)
	}
	tr := traceOf(t, ctx, pool, `SELECT routing_trace FROM events WHERE webhook_id = $1`, wh.ID)
	if tr.Stage != routing.StageFault || tr.RuleID != "trust" || tr.RuleIndex == nil || *tr.RuleIndex != 0 || tr.Cause != routing.FaultError {
		t.Fatalf("trace = %+v, want the fault at rule trust (0), cause error", tr)
	}

	out := logs.String()
	if got := strings.Count(out, "routing rule faulted"); got != 1 {
		t.Fatalf("log = %q, want exactly one fault warning, got %d", out, got)
	}
	for _, want := range []string{"level=WARN", "webhook=" + wh.ID, "rule_id=trust", "rule_index=0", "cause=" + routing.FaultError} {
		if !strings.Contains(out, want) {
			t.Fatalf("log = %q, want it to contain %q", out, want)
		}
	}
	if f := rec.takeFaults(); !slices.Equal(f, []string{routing.FaultError}) {
		t.Fatalf("routing faults = %v, want one %s", f, routing.FaultError)
	}
	expectCalls(t, rec, []recordedDelivery{{"cairn", "signed", "accepted"}}, nil, nil)

	// The producer retries while the rules still fault. It is the same delivery, already recorded
	// and counted, so it is neither counted nor warned about again.
	retry := postSelfManaged(ing, "cairn-fault", body, cairnHeaders(body, "evt-fault"))
	if retry.Code != http.StatusAccepted || !strings.Contains(retry.Body.String(), `"faulted":true`) {
		t.Fatalf("retry = %d %s, want it still faulted", retry.Code, retry.Body.String())
	}
	if f := rec.takeFaults(); len(f) != 0 {
		t.Fatalf("routing faults after a retry = %v, want none: one delivery counts once", f)
	}
	if got := strings.Count(logs.String(), "routing rule faulted"); got != 1 {
		t.Fatalf("fault warnings after a retry = %d, want still 1", got)
	}

	// The owner fixes the rules; the producer redelivers. The slot is spent, so it stays faulted.
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "ok", Expr: `true`, Action: routing.Action{Queue: "inbox"}},
	}})
	again := postSelfManaged(ing, "cairn-fault", body, cairnHeaders(body, "evt-fault"))
	if again.Code != http.StatusAccepted || !strings.Contains(again.Body.String(), `"faulted":true`) {
		t.Fatalf("redelivery = %d %s, want it still faulted", again.Code, again.Body.String())
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 0 {
		t.Fatalf("todos after redelivery = %d, want none", n)
	}
	// Today's rules would queue it, but nothing was queued: no routing decision is counted, and no
	// fault either (it was counted when it was recorded).
	if f := rec.takeFaults(); len(f) != 0 {
		t.Fatalf("routing faults after the fixed redelivery = %v, want none", f)
	}
	expectCalls(t, rec, repeatDelivery(2, recordedDelivery{"cairn", "signed", "accepted"}), nil, nil) // the retry and the redelivery
}

// A delivery that routed, redelivered after the owner's rules started to fault on it, keeps its
// recorded outcome: its todo is reported back as an idempotent redelivery, and nothing claims a fault
// the event row does not record (no faulted response, no fault counted, no warning).
func TestSelfManagedRoutedRedeliveryIgnoresTodaysFault(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-refault", cairnSecret)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "ok", Expr: `true`, Action: routing.Action{Queue: "inbox"}},
	}})
	body := cairnBody("evt-refault", time.Now(), "routed first")
	if first := postSelfManaged(ing, "cairn-refault", body, cairnHeaders(body, "evt-refault")); first.Code != http.StatusAccepted ||
		!strings.Contains(first.Body.String(), `"created":1`) {
		t.Fatalf("first delivery = %d %s, want one todo created", first.Code, first.Body.String())
	}

	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "broken", Expr: `.artifact.title + 1 > 1`, Action: routing.Action{Queue: "inbox"}},
	}})
	again := postSelfManaged(ing, "cairn-refault", body, cairnHeaders(body, "evt-refault"))
	if again.Code != http.StatusAccepted || strings.Contains(again.Body.String(), "faulted") ||
		!strings.Contains(again.Body.String(), `"created":0`) || !strings.Contains(again.Body.String(), `"todos":[{`) {
		t.Fatalf("redelivery = %d %s, want the existing todo reported, not a fault", again.Code, again.Body.String())
	}
	var disp string
	if err := pool.QueryRow(ctx, `SELECT disposition FROM events WHERE webhook_id = $1`, wh.ID).Scan(&disp); err != nil || disp != store.DispositionRouted {
		t.Fatalf("event disposition = %q (%v), want routed", disp, err)
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 1 {
		t.Fatalf("todos = %d, want the one the original delivery minted", n)
	}
	if f := rec.takeFaults(); len(f) != 0 {
		t.Fatalf("routing faults = %v, want none for a delivery recorded as routed", f)
	}
	if strings.Contains(logs.String(), "routing rule faulted") {
		t.Fatalf("log = %q, want no fault warning for a delivery recorded as routed", logs.String())
	}
	expectCalls(t, rec, repeatDelivery(2, recordedDelivery{"cairn", "signed", "accepted"}), nil,
		[]recordedDecision{{wh.ID, "ok", "queue"}})
}

// SPEC-0026 REQ-1 scenario "A faulting drop rule does not route its delivery": the default queue is
// not taken.
func TestSelfManagedFaultingDropRuleSkipsDefault(t *testing.T) {
	ing, pool, ctx, _ := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-fault-drop", cairnSecret)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{
		Rules:   []routing.Rule{{ID: "noise", Expr: `last(range(1e12)) > 0`, Action: routing.Action{Drop: true}}},
		Default: &routing.Action{Queue: "inbox"},
	})
	body := cairnBody("evt-fault-drop", time.Now(), "big")
	resp := postSelfManaged(ing, "cairn-fault-drop", body, cairnHeaders(body, "evt-fault-drop"))
	if resp.Code != http.StatusAccepted || !strings.Contains(resp.Body.String(), `"faulted":true`) {
		t.Fatalf("delivery = %d %s, want 202 faulted", resp.Code, resp.Body.String())
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 0 {
		t.Fatalf("todos = %d, want none: the default must not apply after a fault", n)
	}
}

// SPEC-0026 REQ-2 scenario "Sandbox down": a webhook with rules on an instance with no evaluator
// answers 503 routing unavailable and persists nothing; the producer's retry lands once the sandbox is
// back. A webhook without rules is unaffected.
func TestSelfManagedUnavailableSandboxRefusesDelivery(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.Unavailable{})
	rec := &recordingMetrics{}
	ing.SetMetrics(rec)
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-down", cairnSecret)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "ok", Expr: `true`, Action: routing.Action{Queue: "inbox"}},
	}})

	body := cairnBody("evt-down", time.Now(), "while the sandbox is down")
	resp := postSelfManaged(ing, "cairn-down", body, cairnHeaders(body, "evt-down"))
	if resp.Code != http.StatusServiceUnavailable || !strings.Contains(resp.Body.String(), "routing unavailable") {
		t.Fatalf("delivery = %d %s, want 503 routing unavailable", resp.Code, resp.Body.String())
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM events WHERE webhook_id = $1`, wh.ID); n != 0 {
		t.Fatalf("events = %d, want nothing persisted", n)
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "routing unavailable") ||
		!strings.Contains(out, "webhook="+wh.ID) {
		t.Fatalf("log = %q, want one error naming the webhook", out)
	}
	if f := rec.takeFaults(); len(f) != 0 {
		t.Fatalf("routing faults = %v, want none: an outage is not a faulted delivery", f)
	}
	expectCalls(t, rec, []recordedDelivery{{"cairn", "signed", "rejected"}}, nil, nil)

	// The sandbox recovers and the producer retries the same delivery: it routes normally.
	ing.SetRouter(routing.InProcess{})
	retry := postSelfManaged(ing, "cairn-down", body, cairnHeaders(body, "evt-down"))
	if _, q := accepted202(t, retry); q != "inbox" {
		t.Fatalf("retry queue = %q, want inbox", q)
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 1 {
		t.Fatalf("todos after retry = %d, want 1", n)
	}

	// A webhook without rules never needs the evaluator.
	ing.SetRouter(routing.Unavailable{})
	_, _, _ = seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-norules", cairnSecret)
	plain := cairnBody("evt-norules", time.Now(), "no rules")
	if _, q := accepted202(t, postSelfManaged(ing, "cairn-norules", plain, cairnHeaders(plain, "evt-norules"))); q != "inbox" {
		t.Fatalf("no-rules queue = %q, want inbox", q)
	}
}

// A healthy rule set logs nothing. Without this, a warning that fired on every delivery would look
// exactly like a correct implementation.
func TestSelfManagedRoutingWithoutFaultsIsQuiet(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, _, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-quiet", cairnSecret)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "ok", Expr: `true`, Action: routing.Action{Queue: "inbox"}},
	}})

	body := cairnBody("evt-quiet", time.Now(), "nothing wrong here")
	if _, q := accepted202(t, postSelfManaged(ing, "cairn-quiet", body, cairnHeaders(body, "evt-quiet"))); q != "inbox" {
		t.Fatalf("queue = %q, want inbox", q)
	}
	if out := logs.String(); strings.Contains(out, "faulted") || strings.Contains(out, "unavailable") {
		t.Fatalf("log = %q, want no fault warning when every rule evaluates", out)
	}
}
