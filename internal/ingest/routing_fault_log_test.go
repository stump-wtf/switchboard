package ingest

// A faulting routing rule is treated as no-match, so it stops restricting rather than failing the
// delivery. That is fail-open by design (routing.Match), and it is silent: the fault is recorded on
// the trace and nothing ever said so out loud, so an owner whose trust rule stopped matching had no
// way to learn it except by opening the right event and reading its routing trace.
//
// These tests pin the warning that closes that gap, and — just as importantly — pin that the
// warning changes nothing about where the delivery lands. Governing: #212, ADR-0024,
// SPEC-0020 REQ "Deterministic Rule Evaluation", REQ "Routing Trace".

import (
	"bytes"
	"context"
	"log/slog"
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

// A rule that errors is skipped and the delivery routes on. Before #212 that was entirely silent.
func TestSelfManagedRoutingFaultIsWarned(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, _, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-fault", cairnSecret)

	// "trust" stands in for an allowlist that has stopped working — the shape that matters, because
	// a trust rule that faults admits nobody at that rule and the delivery continues past it.
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "trust", Expr: `error("boom")`, Action: routing.Action{Drop: true}},
		{ID: "ok", Expr: `true`, Action: routing.Action{Queue: "inbox"}},
	}})

	body := cairnBody("evt-fault", time.Now(), "routed despite the faulting rule")
	rec := postSelfManaged(ing, "cairn-fault", body, cairnHeaders(body, "evt-fault"))

	// The outcome must be unchanged: warning about a fault must never alter routing.
	if _, q := accepted202(t, rec); q != "inbox" {
		t.Fatalf("queue = %q, want inbox — a faulting rule must not change where the delivery lands", q)
	}

	out := logs.String()
	if !strings.Contains(out, "routing rule faulted") {
		t.Fatalf("log = %q, want a warning that the rule faulted", out)
	}
	for _, want := range []string{"rule_id=trust", "cause=" + routing.FaultError, "level=WARN"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log = %q, want it to contain %q", out, want)
		}
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
	if out := logs.String(); strings.Contains(out, "faulted") || strings.Contains(out, "wholesale") {
		t.Fatalf("log = %q, want no fault warning when every rule evaluates", out)
	}
}
