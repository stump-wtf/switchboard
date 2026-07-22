package store

import (
	"testing"
	"time"
)

// Governing: SPEC-0012 REQ "Live Updates via SSE" — the SSE retry interval is a settings row
// (sse_retry_ms, seeded by 0001_init) with a safe default when absent or malformed.
func TestSettingInt(t *testing.T) {
	s, ctx := testStore(t)

	if v, err := s.SettingInt(ctx, "sse_retry_ms", 999); err != nil || v != 3000 {
		t.Fatalf("seeded sse_retry_ms = %d, %v; want 3000, nil", v, err)
	}
	if v, err := s.SettingInt(ctx, "no_such_setting", 42); err != nil || v != 42 {
		t.Fatalf("absent key = %d, %v; want default 42, nil", v, err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO settings (key, value) VALUES ('bad_int', 'nope')
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("seed bad_int: %v", err)
	}
	if v, err := s.SettingInt(ctx, "bad_int", 7); err == nil || v != 7 {
		t.Fatalf("malformed value = %d, %v; want default 7 with error", v, err)
	}
}

// Governing: SPEC-0012 REQ "Live Updates via SSE" — committed todo lifecycle transitions reach the
// registered process-local hook with the transition verb, so the web SSE hub can fan them out.
func TestTodoTransitionHookFires(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "todo-transition-hook-fires")

	var verbs []string
	s.SetTodoTransitionHook(func(verb string, td Todo) { verbs = append(verbs, verb) })
	defer s.SetTodoTransitionHook(nil)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "hooked", IdempotencyKey: "hk1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, td.ID, "w", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}
	// A below-cap fail parks in 'failed' with a scheduled retry window (SPEC-0003 scheduled
	// backoff); rewind it so the re-claim is due.
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("rewind retry window: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, ep, td.ID, "w", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	want := []string{"created", "claimed", "failed", "claimed", "done"}
	if len(verbs) != len(want) {
		t.Fatalf("hook verbs = %v, want %v", verbs, want)
	}
	for i := range want {
		if verbs[i] != want[i] {
			t.Fatalf("hook verbs = %v, want %v", verbs, want)
		}
	}
}
