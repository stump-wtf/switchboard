package a2a

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// TestTaskStateFromTodo asserts the full todo-state → A2A-TaskState mapping, including the four base
// states (SPEC-0018 REQ "Task State Machine Extension": pending→submitted, claimed→working,
// done→completed, failed→failed) and the four A2A extension states, plus the unknown fallback.
func TestTaskStateFromTodo(t *testing.T) {
	cases := map[string]TaskState{
		"pending":        TaskStateSubmitted,
		"claimed":        TaskStateWorking,
		"done":           TaskStateCompleted,
		"failed":         TaskStateFailed,
		"canceled":       TaskStateCanceled,
		"rejected":       TaskStateRejected,
		"input-required": TaskStateInputRequired,
		"auth-required":  TaskStateAuthRequired,
		"bogus-state":    TaskStateUnknown,
		"":               TaskStateUnknown,
	}
	for todoState, want := range cases {
		if got := TaskStateFromTodo(todoState); got != want {
			t.Errorf("TaskStateFromTodo(%q) = %q, want %q", todoState, got, want)
		}
	}
}

// TestTaskFromTodoProjectsFields asserts a projected Task equals its source Todo's fields: the id is
// carried verbatim, the queue projects to contextId, the state projects through the mapping, and the
// title/payload project into the task's originating user message. Governing: SPEC-0018 REQ "A Task Is
// a Todo, Not a Parallel Object" ("Every A2A task-shaped response MUST be a direct projection of a
// single underlying todo row"); ADR-0021 Confirmation.
func TestTaskFromTodoProjectsFields(t *testing.T) {
	created := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	todo := store.Todo{
		ID:        "td_abc123",
		Queue:     "reviews",
		Source:    "github",
		Kind:      "pull_request",
		Title:     "Review PR #42",
		Payload:   json.RawMessage(`{"pr":42,"repo":"switchboard"}`),
		State:     "pending",
		CreatedAt: created,
	}

	task := TaskFromTodo(todo)

	if task.ID != todo.ID {
		t.Errorf("task.ID = %q, want %q (the todo id verbatim)", task.ID, todo.ID)
	}
	if task.ContextID != todo.Queue {
		t.Errorf("task.ContextID = %q, want %q (the todo queue)", task.ContextID, todo.Queue)
	}
	if task.Kind != "task" {
		t.Errorf("task.Kind = %q, want %q", task.Kind, "task")
	}
	if task.Status.State != TaskStateSubmitted {
		t.Errorf("task.Status.State = %q, want %q (pending→submitted)", task.Status.State, TaskStateSubmitted)
	}
	if task.Status.Timestamp != created.Format(time.RFC3339) {
		t.Errorf("task.Status.Timestamp = %q, want %q", task.Status.Timestamp, created.Format(time.RFC3339))
	}

	// The delivered work projects as the task's single originating user message: a text part carrying
	// the title and a data part carrying the raw payload verbatim.
	if len(task.History) != 1 {
		t.Fatalf("len(task.History) = %d, want 1", len(task.History))
	}
	msg := task.History[0]
	if msg.Role != RoleUser {
		t.Errorf("message role = %q, want %q", msg.Role, RoleUser)
	}
	if msg.TaskID != todo.ID {
		t.Errorf("message taskId = %q, want %q", msg.TaskID, todo.ID)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2 (text title + data payload)", len(msg.Parts))
	}
	if msg.Parts[0].Kind != "text" || msg.Parts[0].Text != todo.Title {
		t.Errorf("text part = %+v, want title %q", msg.Parts[0], todo.Title)
	}
	if msg.Parts[1].Kind != "data" || string(msg.Parts[1].Data) != string(todo.Payload) {
		t.Errorf("data part = %s, want payload %s verbatim", msg.Parts[1].Data, todo.Payload)
	}
}

// TestTaskFromTodoCompletedArtifact asserts a completed todo projects its result as an output
// artifact and uses the completion timestamp for the status.
func TestTaskFromTodoCompletedArtifact(t *testing.T) {
	created := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	completed := created.Add(time.Hour)
	todo := store.Todo{
		ID:          "td_done1",
		Queue:       "reviews",
		Title:       "done work",
		State:       "done",
		Result:      json.RawMessage(`{"ok":true}`),
		CreatedAt:   created,
		CompletedAt: &completed,
	}

	task := TaskFromTodo(todo)

	if task.Status.State != TaskStateCompleted {
		t.Errorf("state = %q, want completed", task.Status.State)
	}
	if task.Status.Timestamp != completed.Format(time.RFC3339) {
		t.Errorf("timestamp = %q, want completion time %q", task.Status.Timestamp, completed.Format(time.RFC3339))
	}
	if len(task.Artifacts) != 1 {
		t.Fatalf("len(artifacts) = %d, want 1", len(task.Artifacts))
	}
	art := task.Artifacts[0]
	if art.ArtifactID != todo.ID+"-result" {
		t.Errorf("artifactId = %q, want %q", art.ArtifactID, todo.ID+"-result")
	}
	if len(art.Parts) != 1 || string(art.Parts[0].Data) != string(todo.Result) {
		t.Errorf("artifact part = %+v, want result %s verbatim", art.Parts, todo.Result)
	}
}

// TestTaskFromTodoEmptyPayload asserts a title-only todo (no payload) projects a text-only message
// and no data part, and a payload-less non-terminal todo carries no artifacts.
func TestTaskFromTodoEmptyPayload(t *testing.T) {
	todo := store.Todo{
		ID:        "td_bare",
		Queue:     "q",
		Title:     "just a title",
		State:     "claimed",
		CreatedAt: time.Now(),
	}
	claimed := time.Now()
	todo.ClaimedAt = &claimed

	task := TaskFromTodo(todo)

	if task.Status.State != TaskStateWorking {
		t.Errorf("state = %q, want working", task.Status.State)
	}
	if len(task.History) != 1 || len(task.History[0].Parts) != 1 || task.History[0].Parts[0].Kind != "text" {
		t.Fatalf("history = %+v, want a single text part", task.History)
	}
	if len(task.Artifacts) != 0 {
		t.Errorf("artifacts = %+v, want none for a non-done todo", task.Artifacts)
	}
}

// TestTaskFromTodoRoundTripsJSON asserts the projected Task marshals to JSON and back without losing
// the identity-critical fields — the wire boundary must be lossless for id/contextId/state.
func TestTaskFromTodoRoundTripsJSON(t *testing.T) {
	todo := store.Todo{
		ID: "td_rt", Queue: "reviews", Title: "x", State: "failed", CreatedAt: time.Now(),
	}
	raw, err := json.Marshal(TaskFromTodo(todo))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Task
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ID != todo.ID || back.ContextID != todo.Queue || back.Status.State != TaskStateFailed {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
}
