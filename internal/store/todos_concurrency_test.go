package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Governing: SPEC-0003 REQ "Concurrency Safety of Queue Workers" — concurrent claimants get distinct
// todos with no double-claim and no serialization behind a global lock (FOR UPDATE SKIP LOCKED).
// Run under -race in CI; correctness comes from DB atomic transitions, not in-process locks.
func TestConcurrentClaimsAreDistinct(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "concurrent-claims-are-distinct")

	const n = 12
	for i := 0; i < n; i++ {
		if _, _, err := s.CreateTodo(ctx, CreateTodoParams{
			EndpointID: ep,
			Queue:      "race", Title: fmt.Sprintf("t%d", i), IdempotencyKey: fmt.Sprintf("rk%d", i),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  = map[string]int{} // todo id -> how many workers claimed it
		errN int
	)
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			td, err := s.ClaimNext(ctx, ep, []string{"race"}, fmt.Sprintf("w%d", worker), time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errN++
				return
			}
			got[td.ID]++
		}(w)
	}
	wg.Wait()

	if errN != 0 {
		t.Fatalf("%d claimants errored though %d todos were available", errN, n)
	}
	if len(got) != n {
		t.Fatalf("claimed %d distinct todos, want %d", len(got), n)
	}
	for id, c := range got {
		if c != 1 {
			t.Fatalf("todo %s claimed %d times (double-claim)", id, c)
		}
	}
	// SPEC-0034 REQ-20: each committed claim opened exactly one attempt, and no todo holds two.
	if open := count(t, s, ctx, "todo_attempts", "ended_at IS NULL"); open != n {
		t.Fatalf("%d open attempts after %d claims, want %d", open, n, n)
	}
	assertAttemptInvariant(t, s, ctx)
}

// Governing: SPEC-0003 REQ "Error Handling Standards" — a conditional update that matches no row is
// classified as ErrConflict (row exists, wrong state/owner) or ErrNotFound (absent), never a nil
// error with an empty todo.
func TestZeroRowUpdatesAreClassified(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "zero-row-updates-are-classified")

	// Absent id → ErrNotFound for every conditional transition, and the returned todo is the zero value.
	for name, call := range map[string]func() (Todo, error){
		"ClaimTodo": func() (Todo, error) { return s.ClaimTodo(ctx, ep, "td_absent", "w", time.Minute) },
		"Complete":  func() (Todo, error) { return s.CompleteTodo(ctx, ep, "td_absent", "w", nil) },
		"Fail":      func() (Todo, error) { return s.FailTodo(ctx, ep, "td_absent", "w", nil) },
		"Heartbeat": func() (Todo, error) { return s.HeartbeatTodo(ctx, ep, "td_absent", "w", time.Minute) },
		"Retry":     func() (Todo, error) { return s.RetryTodo(ctx, ep, "td_absent") },
	} {
		td, err := call()
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s(absent) = %v, want ErrNotFound", name, err)
		}
		if td.ID != "" {
			t.Fatalf("%s(absent) returned a non-empty todo: %+v", name, td)
		}
	}

	// Existing but wrong-state (pending, never claimed) → ErrConflict, never a silent success.
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "cls"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for name, call := range map[string]func() (Todo, error){
		"Complete":  func() (Todo, error) { return s.CompleteTodo(ctx, ep, td.ID, "w", nil) },
		"Fail":      func() (Todo, error) { return s.FailTodo(ctx, ep, td.ID, "w", nil) },
		"Heartbeat": func() (Todo, error) { return s.HeartbeatTodo(ctx, ep, td.ID, "w", time.Minute) },
		"Retry":     func() (Todo, error) { return s.RetryTodo(ctx, ep, td.ID) },
	} {
		if _, err := call(); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s(pending) = %v, want ErrConflict", name, err)
		}
	}
}
