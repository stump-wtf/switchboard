package store

import (
	"context"
	"errors"
	"testing"
)

// newTask creates a todo and returns its id, for use as a push config's task_id FK target.
// The todo is pinned to a freshly seeded endpoint — todos.endpoint_id is NOT NULL (ADR-0022), so
// a tenantless fixture cannot exist.
func newTask(t *testing.T, s *Store, ctx context.Context, key string) string {
	t.Helper()
	ep := seedEndpoint(t, s, ctx, "push-config", "reviews")
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "reviews", Title: "task", IdempotencyKey: key})
	if err != nil {
		t.Fatalf("create todo: %v", err)
	}
	return td.ID
}

// TestPushConfigCRUD exercises create → get → list → delete against a real task, and the optional
// token/auth columns. Governing: SPEC-0019 REQ "PushNotificationConfig CRUD".
func TestPushConfigCRUD(t *testing.T) {
	s, ctx := testStore(t)
	taskID := newTask(t, s, ctx, "pc-crud")

	auth := []byte(`{"scheme":"bearer"}`)
	c, err := s.CreatePushConfig(ctx, taskID, "https://hook.example/a2a", "corr-123", auth)
	if err != nil {
		t.Fatalf("create push config: %v", err)
	}
	if c.ID == "" || c.TaskID != taskID || c.URL != "https://hook.example/a2a" {
		t.Fatalf("unexpected stored config: %+v", c)
	}
	if c.Token == nil || *c.Token != "corr-123" {
		t.Fatalf("token not persisted: %+v", c.Token)
	}
	if string(c.Authentication) != `{"scheme": "bearer"}` && string(c.Authentication) != `{"scheme":"bearer"}` {
		t.Fatalf("authentication not round-tripped: %s", c.Authentication)
	}

	got, err := s.GetPushConfig(ctx, c.ID, taskID)
	if err != nil {
		t.Fatalf("get push config: %v", err)
	}
	if got.ID != c.ID || got.URL != c.URL {
		t.Fatalf("get returned wrong row: %+v", got)
	}

	list, err := s.ListPushConfigs(ctx, taskID)
	if err != nil {
		t.Fatalf("list push configs: %v", err)
	}
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list returned %d rows, want 1 with id %s", len(list), c.ID)
	}
}

// TestPushConfigOptionalFieldsNull asserts an empty token and nil auth persist as NULL.
func TestPushConfigOptionalFieldsNull(t *testing.T) {
	s, ctx := testStore(t)
	taskID := newTask(t, s, ctx, "pc-null")

	c, err := s.CreatePushConfig(ctx, taskID, "https://hook.example/b", "", nil)
	if err != nil {
		t.Fatalf("create push config: %v", err)
	}
	if c.Token != nil {
		t.Fatalf("empty token must persist NULL, got %q", *c.Token)
	}
	if c.Authentication != nil {
		t.Fatalf("nil auth must persist NULL, got %s", c.Authentication)
	}
}

// TestPushConfigCreateUnknownTask asserts a config against a nonexistent task is rejected as
// ErrNotFound (FK violation mapped), not persisted. Governing: SPEC-0019 REQ "PushNotificationConfig
// CRUD" (a config requires an existing task).
func TestPushConfigCreateUnknownTask(t *testing.T) {
	s, ctx := testStore(t)
	_, err := s.CreatePushConfig(ctx, "no-such-todo", "https://hook.example/c", "", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("create against unknown task must be ErrNotFound, got %v", err)
	}
}

// TestPushConfigGetScoping asserts Get is scoped by task_id: a config's id read under a different
// task returns ErrNotFound with no leak. Governing: SPEC-0019 REQ "PushNotificationConfig CRUD"
// (scoped to the caller's own tasks).
func TestPushConfigGetScoping(t *testing.T) {
	s, ctx := testStore(t)
	taskA := newTask(t, s, ctx, "pc-a")
	taskB := newTask(t, s, ctx, "pc-b")

	c, err := s.CreatePushConfig(ctx, taskA, "https://hook.example/a", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.GetPushConfig(ctx, c.ID, taskB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get for a different task must be ErrNotFound, got %v", err)
	}
}

// TestPushConfigDeleteIdempotent asserts delete succeeds twice for the same id. Governing:
// SPEC-0019 REQ "PushNotificationConfig CRUD" (Delete is idempotent).
func TestPushConfigDeleteIdempotent(t *testing.T) {
	s, ctx := testStore(t)
	taskID := newTask(t, s, ctx, "pc-del")

	c, err := s.CreatePushConfig(ctx, taskID, "https://hook.example/d", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeletePushConfig(ctx, c.ID, taskID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Second delete of the same (now-absent) config must still succeed.
	if err := s.DeletePushConfig(ctx, c.ID, taskID); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}
	// And it is gone.
	if _, err := s.GetPushConfig(ctx, c.ID, taskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete, get must be ErrNotFound, got %v", err)
	}
}

// TestPushConfigCascadeOnTaskDelete asserts the ON DELETE CASCADE FK removes a task's push configs
// when the task is deleted. Governing: migration 0012 (task_id references todos ON DELETE CASCADE).
func TestPushConfigCascadeOnTaskDelete(t *testing.T) {
	s, ctx := testStore(t)
	taskID := newTask(t, s, ctx, "pc-cascade")

	if _, err := s.CreatePushConfig(ctx, taskID, "https://hook.example/e", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM todos WHERE id = $1`, taskID); err != nil {
		t.Fatalf("delete todo: %v", err)
	}
	list, err := s.ListPushConfigs(ctx, taskID)
	if err != nil {
		t.Fatalf("list after cascade: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("configs must cascade-delete with their task, got %d", len(list))
	}
}
