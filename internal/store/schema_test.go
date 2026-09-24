package store

import (
	"context"
	"strings"
	"testing"
)

func indexDef(t *testing.T, s *Store, ctx context.Context, name string) string {
	t.Helper()
	var def string
	err := s.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&def)
	if err != nil {
		t.Fatalf("index %s not found: %v", name, err)
	}
	return def
}

// Governing: SPEC-0004 REQ "Schema, Partial Indexes, and Rich Column Types" — the hot-path indexes
// exist with the right partial predicates and uniqueness.
func TestSchemaHotPathIndexes(t *testing.T) {
	s, ctx := testStore(t)

	pending := indexDef(t, s, ctx, "idx_todos_pending")
	if !strings.Contains(pending, "state = 'pending'") {
		t.Fatalf("idx_todos_pending must be partial on pending rows: %s", pending)
	}

	dedupe := indexDef(t, s, ctx, "idx_todos_dedupe")
	if !strings.Contains(strings.ToUpper(dedupe), "UNIQUE") || !strings.Contains(dedupe, "idempotency_key") {
		t.Fatalf("idx_todos_dedupe must be a unique partial index on idempotency_key: %s", dedupe)
	}
	// The 0008 predicate must keep a parked retry (failed with an open next_retry_at window) LIVE
	// in dedup — the 0001 predicate dropped it, letting a redelivery mint a duplicate that the
	// re-queue transition then collided with (23505). SPEC-0003 REQ "Idempotent Enqueue and Dedup".
	if !strings.Contains(dedupe, "next_retry_at") {
		t.Fatalf("idx_todos_dedupe must keep parked retries (open next_retry_at) in the dedup predicate: %s", dedupe)
	}

	evDedupe := indexDef(t, s, ctx, "idx_events_dedupe")
	// The owner is part of the key, so one owner's delivery is never answered with another's event.
	// Governing: SPEC-0033 REQ "Closing the Audited Surfaces" (F14).
	if !strings.Contains(strings.ToUpper(evDedupe), "UNIQUE") || !strings.Contains(evDedupe, "external_id") ||
		!strings.Contains(evDedupe, "endpoint_id") {
		t.Fatalf("idx_events_dedupe must be a unique index on (endpoint_id, source, external_id): %s", evDedupe)
	}

	// Present-and-usable is enough for the read surface index.
	_ = indexDef(t, s, ctx, "idx_events_source_time")
}

// Governing: SPEC-0004 scenario "Hot claim scan hits the partial index" — with many terminal rows
// present, the claim scan is served by idx_todos_pending and never sequentially scans terminal rows.
func TestHotClaimScanUsesPartialIndex(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "hot-claim-scan-uses-partial-index")

	// Seed skew: terminal todos plus a few pending. The partial index holds only the pending rows,
	// so serving the claim scan through it physically cannot touch the terminal backlog.
	mkTodo := func(title string) string {
		td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: title})
		if err != nil {
			t.Fatalf("create todo: %v", err)
		}
		return td.ID
	}
	for i := 0; i < 50; i++ {
		id := mkTodo("term")
		if _, err := s.ClaimTodo(ctx, ep, id, "w", 0); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := s.CompleteTodo(ctx, ep, id, "w", nil); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		mkTodo("live")
	}

	// At test scale the whole table fits in one page, so the cost planner would pick a seq scan on
	// cost alone regardless of the index. Disabling seq scans for this EXPLAIN forces the planner to
	// reveal the serving path: if idx_todos_pending is a valid access path for the claim predicate,
	// it is chosen — proving the hot scan is served by the partial index (which, being partial, holds
	// only pending rows and so never scans the terminal backlog). At production scale the planner
	// picks the same index on cost. EXPLAIN without ANALYZE does not execute, so FOR UPDATE is inert.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	var plan string
	err = tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT id FROM todos
		WHERE queue = ANY($1) AND state='pending' AND (assignee IS NULL OR assignee=$2)
		ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, []string{"q"}, "w").Scan(&plan)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(plan, "idx_todos_pending") {
		t.Fatalf("claim scan should be served by idx_todos_pending, plan was: %s", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Fatalf("claim scan must not sequentially scan the terminal backlog, plan was: %s", plan)
	}
}

// Governing: SPEC-0004 scenario "No plaintext secrets at rest" — the endpoints table stores only a
// credential hash and a non-secret display prefix; no column can hold a recoverable plaintext secret.
func TestNoPlaintextCredentialColumns(t *testing.T) {
	s, ctx := testStore(t)

	rows, err := s.pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'endpoints'`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan col: %v", err)
		}
		cols = append(cols, c)
	}
	has := func(name string) bool {
		for _, c := range cols {
			if c == name {
				return true
			}
		}
		return false
	}
	if !has("credential_hash") || !has("credential_prefix") {
		t.Fatalf("endpoints must store credential_hash + credential_prefix, got %v", cols)
	}
	// No column that would imply a recoverable secret at rest.
	for _, c := range cols {
		if c == "credential_hash" || c == "credential_prefix" {
			continue
		}
		low := strings.ToLower(c)
		if strings.Contains(low, "secret") || strings.Contains(low, "plaintext") ||
			low == "credential" || low == "token" || low == "password" {
			t.Fatalf("endpoints column %q could hold a plaintext secret at rest", c)
		}
	}
}

// Governing: SPEC-0004 REQ "Schema ... Rich Column Types" — first-class jsonb / text[] / inet / bytea
// are used rather than everything-as-text.
func TestRichColumnTypes(t *testing.T) {
	s, ctx := testStore(t)
	want := map[[2]string]string{
		{"events", "payload"}:         "bytea",
		{"events", "headers"}:         "jsonb",
		{"events", "source_ip"}:       "inet",
		{"todos", "payload"}:          "jsonb",
		{"endpoints", "scope_queues"}: "ARRAY",
		{"humans", "created_at"}:      "timestamp with time zone",
	}
	for key, typ := range want {
		var got string
		err := s.pool.QueryRow(ctx,
			`SELECT data_type FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
			key[0], key[1]).Scan(&got)
		if err != nil {
			t.Fatalf("%s.%s: %v", key[0], key[1], err)
		}
		if got != typ {
			t.Fatalf("%s.%s data_type=%q want %q", key[0], key[1], got, typ)
		}
	}
}
