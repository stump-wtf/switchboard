package store

// The quarantine default filter (ADR-0031, SPEC-0026 REQ-6): every exported todo read and lifecycle
// method in this package excludes quarantine rows, except an explicit allowlist of the owner's own
// surfaces and the quarantine verbs themselves. The test is reflective, in the pattern #371 names
// for synthetic todos: it enumerates every exported *Store method that returns todos or names one,
// and fails if a method is neither exercised below nor allowlisted. A new todo method cannot
// silently hand quarantined work to an agent.
//
// Each exercise forces the held row into the state that method would otherwise act on (claimed with
// an expired lease, failed with a due retry, input-required, …), so the only thing standing between
// the method and the row is the queue filter, not an incidental state mismatch.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// quarantineAllowlist are the exported todo methods that MAY see or act on quarantine rows, and why.
var quarantineAllowlist = map[string]string{
	"GetTodoOperatorOwned":   "the owner's todo detail on the Board",
	"TodoCounts":             "the owner's Board counts group every queue",
	"ListTodoItems":          "the owner's todo list",
	"GetTodoItem":            "the owner's todo detail",
	"QuarantinedForHuman":    "the quarantine read itself",
	"ApplyQuarantineRelease": "release: the only way out besides discard and expiry",
	"DiscardQuarantined":     "discard",
	"ExpireQuarantine":       "expiry",
	"QuarantineCounts":       "list_webhooks' count of held items",
	"CreateTodo":             "writers: a quarantine row needs a reason (CHECK), which only intake sets",
	"CreateEventTodo":        "writer",
	"CreateEventTodos":       "writer",
	"CreateRoutedEventTodos": "writer",
	"CreateIntakeEventTodos": "writer: quarantine is created here",
	"CreateForFriend":        "writer: a friend handoff targets the friend's scope queues, never quarantine",
	"SetTodoDoorbellHook":    "hook registration, no todo access (fireDoorbell refuses quarantine itself)",
	"SetTodoTransitionHook":  "hook registration, no todo access",
}

type heldFixture struct {
	s     *Store
	ctx   context.Context
	ep    string
	human string
}

// seedHeld creates a fresh quarantine todo, on a verified event, then forces it into the given state.
func (f heldFixture) seedHeld(t *testing.T, state string) string {
	t.Helper()
	key := "held-" + time.Now().Format("150405.000000000")
	evID, out, _, err := f.s.CreateIntakeEventTodos(f.ctx, EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed",
		Verified: true, Payload: []byte(`{}`), Disposition: DispositionQuarantined,
	}, []string{f.ep}, CreateTodoParams{Source: "github", Kind: "webhook", Title: "held", IdempotencyKey: key,
		QuarantineReason: "untrusted_actor", QuarantineDetail: []byte(`{}`)})
	if err != nil || len(out) != 1 || evID == 0 {
		t.Fatalf("seed held: %v (%d todos)", err, len(out))
	}
	id := out[0].Todo.ID
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE todos SET state = $2,
		owner = CASE WHEN $2 IN ('claimed','input-required') THEN 'w' END,
		lease_expires_at = CASE WHEN $2 = 'claimed' THEN now() - interval '1 minute' END,
		next_retry_at = CASE WHEN $2 = 'failed' THEN now() - interval '1 minute' END
		WHERE id = $1`, id, state); err != nil {
		t.Fatalf("force state %s: %v", state, err)
	}
	return id
}

func (f heldFixture) rowState(t *testing.T, id string) string {
	t.Helper()
	var queue, state, owner string
	if err := f.s.pool.QueryRow(f.ctx, `SELECT queue, state, COALESCE(owner,'') FROM todos WHERE id = $1`, id).
		Scan(&queue, &state, &owner); err != nil {
		t.Fatalf("read held row: %v", err)
	}
	return queue + "/" + state + "/" + owner
}

func containsTodo(ts []Todo, id string) bool {
	return slices.ContainsFunc(ts, func(t Todo) bool { return t.ID == id })
}

func TestQuarantineDefaultFilterCoversEveryTodoMethod(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "held-filter", "q")
	var human string
	if err := s.pool.QueryRow(ctx, `SELECT ag.owner_human_id::text FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE e.id = $1`, ep).Scan(&human); err != nil {
		t.Fatalf("owner: %v", err)
	}
	f := heldFixture{s: s, ctx: ctx, ep: ep, human: human}
	ttl := time.Minute
	q := []string{QueueQuarantine}
	var rang []Todo
	s.SetTodoDoorbellHook(func(t Todo) { rang = append(rang, t) })
	t.Cleanup(func() { s.SetTodoDoorbellHook(nil) })

	// Each exercise returns an error when the method saw or changed the held row.
	exercises := map[string]func(t *testing.T) error{
		"ClaimTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			if _, err := s.ClaimTodo(ctx, ep, id, "w", ttl); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("ClaimTodo = %v, want not found", err)
			}
			return unchanged(t, f, id, "quarantine/pending/")
		},
		"ClaimNext": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			if got, err := s.ClaimNext(ctx, ep, q, "w", ttl); err == nil && got.ID == id {
				return fmt.Errorf("ClaimNext handed out the held todo")
			}
			return unchanged(t, f, id, "quarantine/pending/")
		},
		"ClaimTodoOperatorOwned": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			if _, err := s.ClaimTodoOperatorOwned(ctx, human, id, "w", ttl); err == nil {
				return fmt.Errorf("ClaimTodoOperatorOwned claimed the held todo")
			}
			return unchanged(t, f, id, "quarantine/pending/")
		},
		"HeartbeatTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.HeartbeatTodo(ctx, ep, id, "w", ttl); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("HeartbeatTodo = %v, want not found", err)
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"HeartbeatTodoOperatorOwned": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.HeartbeatTodoOperatorOwned(ctx, human, id, "w", ttl); err == nil {
				return fmt.Errorf("HeartbeatTodoOperatorOwned extended the held todo")
			}
			return nil
		},
		"CompleteTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.CompleteTodo(ctx, ep, id, "w", nil); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("CompleteTodo = %v, want not found", err)
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"CompleteTodoOperatorOwned": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.CompleteTodoOperatorOwned(ctx, human, id, "w", nil); err == nil {
				return fmt.Errorf("CompleteTodoOperatorOwned completed the held todo")
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"FailTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.FailTodo(ctx, ep, id, "w", nil); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("FailTodo = %v, want not found", err)
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"FailTodoOperatorOwned": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.FailTodoOperatorOwned(ctx, human, id, "w", nil); err == nil {
				return fmt.Errorf("FailTodoOperatorOwned failed the held todo")
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"ReleaseTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.ReleaseTodo(ctx, ep, id, "w"); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("ReleaseTodo = %v, want not found", err)
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"ReleaseTodoOperatorOwned": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.ReleaseTodoOperatorOwned(ctx, human, id, "w"); err == nil {
				return fmt.Errorf("ReleaseTodoOperatorOwned released the held todo")
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"RetryTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "failed")
			if _, err := s.RetryTodo(ctx, ep, id); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("RetryTodo = %v, want not found", err)
			}
			return unchanged(t, f, id, "quarantine/failed/")
		},
		"RetryTodoOperatorOwned": func(t *testing.T) error {
			id := f.seedHeld(t, "failed")
			if _, err := s.RetryTodoOperatorOwned(ctx, human, id); err == nil {
				return fmt.Errorf("RetryTodoOperatorOwned re-queued the held todo")
			}
			return unchanged(t, f, id, "quarantine/failed/")
		},
		"RequeueDueRetries": func(t *testing.T) error {
			id := f.seedHeld(t, "failed")
			if _, err := s.RequeueDueRetries(ctx); err != nil {
				return err
			}
			return unchanged(t, f, id, "quarantine/failed/")
		},
		"ReapExpired": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.ReapExpired(ctx); err != nil {
				return err
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"ListTodos": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			got, err := s.ListTodos(ctx, ep, append(q, "q"), "", 200)
			if err != nil || containsTodo(got, id) {
				return fmt.Errorf("ListTodos = %v (%v), listed the held todo", len(got), err)
			}
			return nil
		},
		"GetTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			if _, err := s.GetTodo(ctx, ep, id); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("GetTodo = %v, want not found", err)
			}
			return nil
		},
		"PendingDoorbellTodos": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			got, err := s.PendingDoorbellTodos(ctx, ep, QueueQuarantine, 200)
			if err != nil || containsTodo(got, id) {
				return fmt.Errorf("PendingDoorbellTodos offered the held todo (%v)", err)
			}
			return nil
		},
		"PendingDoorbellTodosByQueue": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			got, err := s.PendingDoorbellTodosByQueue(ctx, QueueQuarantine, 200)
			if err != nil || containsTodo(got, id) {
				return fmt.Errorf("PendingDoorbellTodosByQueue offered the held todo (%v)", err)
			}
			return nil
		},
		"RingUnclaimed": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			got, err := s.RingUnclaimed(ctx)
			if err != nil || containsTodo(got, id) {
				return fmt.Errorf("RingUnclaimed rang the held todo (%v)", err)
			}
			return nil
		},
		"RingOnAttach": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			got, err := s.RingOnAttach(ctx, ep, append(q, "q"))
			if err != nil || containsTodo(got, id) {
				return fmt.Errorf("RingOnAttach rang the held todo (%v)", err)
			}
			return nil
		},
		"CancelTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			if _, err := s.CancelTodo(ctx, id, nil); err == nil {
				return fmt.Errorf("CancelTodo cancelled the held todo")
			}
			return unchanged(t, f, id, "quarantine/pending/")
		},
		"RejectTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "pending")
			if _, err := s.RejectTodo(ctx, id, nil); err == nil {
				return fmt.Errorf("RejectTodo rejected the held todo")
			}
			return unchanged(t, f, id, "quarantine/pending/")
		},
		"InterruptTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "claimed")
			if _, err := s.InterruptTodo(ctx, id, "w", "input-required", nil); err == nil {
				return fmt.Errorf("InterruptTodo interrupted the held todo")
			}
			return unchanged(t, f, id, "quarantine/claimed/w")
		},
		"ResumeTodo": func(t *testing.T) error {
			id := f.seedHeld(t, "input-required")
			if _, err := s.ResumeTodo(ctx, id, "w", nil); err == nil {
				return fmt.Errorf("ResumeTodo resumed the held todo")
			}
			return unchanged(t, f, id, "quarantine/input-required/w")
		},
	}

	// Reflective coverage: every exported method that returns todos, or names one, is exercised or
	// allowlisted. The two sweeps return counts, so they are listed explicitly.
	todoTypes := []reflect.Type{reflect.TypeOf(Todo{}), reflect.TypeOf([]Todo{}), reflect.TypeOf(TodoItem{}),
		reflect.TypeOf([]TodoItem{}), reflect.TypeOf([]CreatedTodo{}), reflect.TypeOf(TodoCounts{})}
	st := reflect.TypeOf(s)
	for i := 0; i < st.NumMethod(); i++ {
		m := st.Method(i)
		touches := strings.Contains(m.Name, "Todo")
		for o := 0; o < m.Type.NumOut(); o++ {
			if slices.Contains(todoTypes, m.Type.Out(o)) {
				touches = true
			}
		}
		if !touches {
			continue
		}
		if _, ok := quarantineAllowlist[m.Name]; ok {
			continue
		}
		if _, ok := exercises[m.Name]; !ok {
			t.Errorf("store method %s reads or changes todos but is neither exercised against a quarantine row "+
				"here nor allowlisted: apply the queue <> 'quarantine' filter and add it to the exercises", m.Name)
		}
	}
	for _, name := range []string{"RequeueDueRetries", "ReapExpired"} {
		if _, ok := exercises[name]; !ok {
			t.Errorf("sweep %s is not exercised", name)
		}
	}

	for name, run := range exercises {
		t.Run(name, func(t *testing.T) {
			if err := run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
	if len(rang) != 0 {
		t.Fatalf("doorbells rang for %d quarantine todos, want none", len(rang))
	}
}

func unchanged(t *testing.T, f heldFixture, id, want string) error {
	t.Helper()
	if got := f.rowState(t, id); got != want {
		return fmt.Errorf("held row is now %s, want %s untouched", got, want)
	}
	return nil
}
