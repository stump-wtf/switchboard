package store

import (
	"context"
	"testing"
	"time"
)

// backdate shifts a timestamp column on a table to simulate age without waiting real time.
func backdate(t *testing.T, s *Store, ctx context.Context, table, col, where string, age time.Duration) {
	t.Helper()
	days := int(age.Hours() / 24)
	if _, err := s.pool.Exec(ctx,
		`UPDATE `+table+` SET `+col+` = now() - make_interval(days => $1) WHERE `+where, days); err != nil {
		t.Fatalf("backdate %s.%s: %v", table, col, err)
	}
}

func count(t *testing.T, s *Store, ctx context.Context, table, where string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM ` + table
	if where != "" {
		q += ` WHERE ` + where
	}
	if err := s.pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Governing: SPEC-0004 REQ "Hybrid Retention and Bounded Growth" — Prune bounds by age AND row-cap
// in one transaction, and never touches pending/claimed todos.
func TestPruneAgeDropsOldKeepsRecentAndLiveWork(t *testing.T) {
	s, ctx := testStore(t)
	setSetting(t, s, ctx, "retention_max_age_days", "7")
	setSetting(t, s, ctx, "retention_max_rows", "1000000") // cap out of the way; test age only
	ep := seedEndpoint(t, s, ctx, "prune-age")

	// Two events: one old, one fresh.
	oldEv, err := s.InsertEvent(ctx, EventInput{Source: "github", Family: "webhook", ExternalID: "old", TrustMode: "signed", Verified: true})
	if err != nil {
		t.Fatalf("insert old event: %v", err)
	}
	if _, err := s.InsertEvent(ctx, EventInput{Source: "github", Family: "webhook", ExternalID: "fresh", TrustMode: "signed", Verified: true}); err != nil {
		t.Fatalf("insert fresh event: %v", err)
	}
	backdate(t, s, ctx, "events", "received_at", "external_id = 'old'", 30*24*time.Hour)

	// A terminal (done) todo that is old, plus a pending and a claimed todo that are also old but live.
	doneTodo := seedTerminal(t, s, ctx, ep, "q", "done-old", "done")
	pendingID := seedPending(t, s, ctx, ep, "q", "pending-old")
	claimedID := seedPending(t, s, ctx, ep, "q", "claimed-old")
	if _, err := s.ClaimTodo(ctx, ep, claimedID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Age all three past the bound. Pending/claimed must survive regardless.
	backdate(t, s, ctx, "todos", "updated_at", "id IN ('"+doneTodo+"','"+pendingID+"','"+claimedID+"')", 30*24*time.Hour)
	backdate(t, s, ctx, "todos", "created_at", "id IN ('"+doneTodo+"','"+pendingID+"','"+claimedID+"')", 30*24*time.Hour)

	res, err := s.Prune(ctx)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.EventsAged != 1 {
		t.Fatalf("EventsAged=%d want 1", res.EventsAged)
	}
	if res.TodosAged != 1 {
		t.Fatalf("TodosAged=%d want 1 (only the terminal todo)", res.TodosAged)
	}
	// Old event gone, fresh event kept.
	if count(t, s, ctx, "events", "external_id = 'old'") != 0 {
		t.Fatalf("old event should be pruned")
	}
	if count(t, s, ctx, "events", "external_id = 'fresh'") != 1 {
		t.Fatalf("fresh event should survive")
	}
	// Live work survives even though it is old.
	if count(t, s, ctx, "todos", "state IN ('pending','claimed')") != 2 {
		t.Fatalf("pending/claimed todos must never be pruned")
	}
	_ = oldEv
}

func TestPruneRowCapTrimsBeyondCap(t *testing.T) {
	s, ctx := testStore(t)
	setSetting(t, s, ctx, "retention_max_age_days", "36500") // age out of the way; test cap only
	setSetting(t, s, ctx, "retention_max_rows", "2")

	// Five fresh events; cap=2 must trim the 3 oldest, keep the 2 newest.
	for i := 0; i < 5; i++ {
		ext := "e" + string(rune('a'+i))
		if _, err := s.InsertEvent(ctx, EventInput{Source: "github", Family: "webhook", ExternalID: ext, TrustMode: "signed", Verified: true}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		// Stagger received_at so ordering is deterministic (older = earlier index).
		backdate(t, s, ctx, "events", "received_at", "external_id = '"+ext+"'", time.Duration(5-i)*24*time.Hour)
	}
	res, err := s.Prune(ctx)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.EventsCapped != 3 {
		t.Fatalf("EventsCapped=%d want 3", res.EventsCapped)
	}
	if got := count(t, s, ctx, "events", ""); got != 2 {
		t.Fatalf("events remaining=%d want 2", got)
	}
	// The two newest (ed, ee) are the survivors.
	if count(t, s, ctx, "events", "external_id IN ('ed','ee')") != 2 {
		t.Fatalf("cap should keep the two newest events")
	}
}

// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY", ADR-0022 — a pending CreateTodo
// emits a todo_ready notification carrying the OWNING ENDPOINT ID as well as the queue name. The
// payload used to be the bare queue name, which is precisely what made the wakeup path leak: every
// listener on a queue called "alerts" woke for every tenant's work. The listener now scopes the
// re-scan to one endpoint, so the endpoint id must be on the wire.
func TestCreateTodoNotifiesTodoReady(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "notify-todo-ready", "alerts")

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN todo_ready"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if _, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "alerts", Title: "ping"}); err != nil {
		t.Fatalf("create todo: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("expected todo_ready notification: %v", err)
	}
	want := TodoReadyPayload(ep, "alerts")
	if n.Channel != "todo_ready" || n.Payload != want {
		t.Fatalf("notification channel=%q payload=%q, want todo_ready/%q", n.Channel, n.Payload, want)
	}
	// Guard the shape explicitly: a regression back to the bare queue name would silently restore
	// the cross-tenant wakeup, and a payload equal to just "alerts" must fail loudly.
	if n.Payload == "alerts" {
		t.Fatal("todo_ready payload is the bare queue name — the endpoint scope was dropped (ADR-0022)")
	}
}

// --- helpers ---

func setSetting(t *testing.T, s *Store, ctx context.Context, key, val string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO settings (key, value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		key, val); err != nil {
		t.Fatalf("set setting %s: %v", key, err)
	}
}

func seedPending(t *testing.T, s *Store, ctx context.Context, ep, queue, title string) string {
	t.Helper()
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: queue, Title: title})
	if err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	return td.ID
}

// seedTerminal creates a todo and drives it to a terminal state (done|failed) via the store API.
func seedTerminal(t *testing.T, s *Store, ctx context.Context, ep, queue, title, state string) string {
	t.Helper()
	id := seedPending(t, s, ctx, ep, queue, title)
	if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	switch state {
	case "done":
		if _, err := s.CompleteTodo(ctx, ep, id, "w", nil); err != nil {
			t.Fatalf("seed complete: %v", err)
		}
	case "failed":
		// Drive attempts to max so FailTodo dead-letters instead of retrying.
		if _, err := s.pool.Exec(ctx, `UPDATE todos SET attempt = max_attempts WHERE id = $1`, id); err != nil {
			t.Fatalf("seed bump attempt: %v", err)
		}
		if _, err := s.FailTodo(ctx, ep, id, "w", nil); err != nil {
			t.Fatalf("seed fail: %v", err)
		}
	}
	return id
}

// A parked retry (failed with an open next_retry_at window, SPEC-0003 scheduled backoff) is LIVE
// work awaiting re-queue — the pruner must never delete it, by age or by cap, or a scheduled retry
// silently vanishes. Only true dead-letters (failed, no window) are prunable terminal records.
func TestPruneNeverDeletesParkedRetries(t *testing.T) {
	s, ctx := testStore(t)
	setSetting(t, s, ctx, "retention_max_age_days", "7")
	setSetting(t, s, ctx, "retention_max_rows", "0") // maximally aggressive cap: everything eligible is trimmed
	ep := seedEndpoint(t, s, ctx, "prune-parked-retries")

	// A parked retry: fail below the cap so FailTodo stamps a window, then backdate it far past
	// the age bound (and keep the window open) — retention must still skip it.
	parked := seedPending(t, s, ctx, ep, "qret", "parked")
	if _, err := s.ClaimTodo(ctx, ep, parked, "w", time.Hour); err != nil {
		t.Fatalf("claim parked: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, parked, "w", nil); err != nil {
		t.Fatalf("fail parked: %v", err)
	}
	backdate(t, s, ctx, "todos", "updated_at", "id = '"+parked+"'", 30*24*time.Hour)

	// A true dead-letter, equally old — this one IS prunable.
	dead := seedTerminal(t, s, ctx, ep, "qret", "dead", "failed")
	backdate(t, s, ctx, "todos", "updated_at", "id = '"+dead+"'", 30*24*time.Hour)

	if _, err := s.Prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := count(t, s, ctx, "todos", "id = '"+parked+"'"); n != 1 {
		t.Fatalf("parked retry was pruned (age/cap), want it kept: count=%d", n)
	}
	if n := count(t, s, ctx, "todos", "id = '"+dead+"'"); n != 0 {
		t.Fatalf("dead-letter should have been pruned: count=%d", n)
	}
}
