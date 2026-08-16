package store

import (
	"strings"
	"testing"
)

// Governing: SPEC-0018 REQ "Task State Machine Extension" / "Database Operation Standards" — the
// 0012 migration stamps a CHECK enumerating the full valid state domain (the four original plus the
// four A2A states); existing pending/claimed/done/failed rows are unaffected and the four new states
// are now insertable, while an out-of-domain state is rejected by the database.
func TestTodoStateCheckConstraint(t *testing.T) {
	s, ctx := testStore(t)
	// todos.endpoint_id is NOT NULL (ADR-0022), so even raw-SQL fixtures must name an owner.
	ep := seedEndpoint(t, s, ctx, "state-check", "q")

	// Every valid state must be insertable directly.
	valid := []string{
		StatePending, StateClaimed, StateDone, StateFailed,
		StateCanceled, StateRejected, StateInputRequired, StateAuthRequired,
	}
	for _, st := range valid {
		id := "td_check_" + st
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO todos (id, endpoint_id, queue, title, state) VALUES ($1, $2, 'q', 't', $3)`, id, ep, st); err != nil {
			t.Fatalf("insert state %q rejected but should be valid: %v", st, err)
		}
	}

	// An out-of-domain state must be rejected by the CHECK (SQLSTATE 23514).
	_, err := s.pool.Exec(ctx,
		`INSERT INTO todos (id, endpoint_id, queue, title, state) VALUES ('td_check_bad', $1, 'q', 't', 'bogus')`, ep)
	if err == nil {
		t.Fatalf("insert of an out-of-domain state succeeded; the CHECK constraint must reject it")
	}
	if !strings.Contains(err.Error(), "todos_state_check") && !strings.Contains(err.Error(), "23514") {
		t.Fatalf("expected a todos_state_check constraint violation, got: %v", err)
	}
}
