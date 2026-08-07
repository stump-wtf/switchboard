// Governing: ADR-0021 (A2A task-delegation transport), SPEC-0018 REQ "A Task Is a Todo, Not a
// Parallel Object"
//
// A2A wire types modeled as PROJECTIONS of the existing store.Todo (ADR-0007/SPEC-0003). There is no
// second data model and no `tasks` table: TaskFromTodo translates a single todo row into A2A's
// Task-shaped wire vocabulary, and TaskStateFromTodo maps the todo state machine onto A2A's
// TaskState enum. "Task" is A2A's boundary vocabulary for the same object the rest of switchboard
// calls a Todo — see SPEC-0018 "Keep 'Todo' as the internal name; 'Task' only at the A2A boundary".
package a2a

import (
	"encoding/json"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// TaskState is A2A's task lifecycle enum (https://a2a-protocol.org/latest/specification/#taskstate).
// It is the wire projection of store.Todo.State; the mapping is defined by TaskStateFromTodo.
type TaskState string

const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input-required"
	TaskStateAuthRequired  TaskState = "auth-required"
	TaskStateCompleted     TaskState = "completed"
	TaskStateFailed        TaskState = "failed"
	TaskStateCanceled      TaskState = "canceled"
	TaskStateRejected      TaskState = "rejected"
	// TaskStateUnknown is the projection of any todo state not covered by the mapping. A well-formed
	// store never produces one, but projecting to an explicit "unknown" rather than an empty string
	// keeps the wire value self-describing if the state machine grows a value this translation has not
	// been taught yet.
	TaskStateUnknown TaskState = "unknown"
)

// TaskStateFromTodo maps a store.Todo.State onto A2A's TaskState. The four base states map per
// SPEC-0018 REQ "Task State Machine Extension" (pending→submitted, claimed→working, done→completed,
// failed→failed); the four A2A extension states (canceled, rejected, input-required, auth-required)
// project to their like-named TaskState. An unrecognized state maps to TaskStateUnknown rather than
// silently emitting an empty string.
func TaskStateFromTodo(state string) TaskState {
	switch state {
	case "pending":
		return TaskStateSubmitted
	case "claimed":
		return TaskStateWorking
	case "done":
		return TaskStateCompleted
	case "failed":
		return TaskStateFailed
	case "canceled":
		return TaskStateCanceled
	case "rejected":
		return TaskStateRejected
	case "input-required":
		return TaskStateInputRequired
	case "auth-required":
		return TaskStateAuthRequired
	default:
		return TaskStateUnknown
	}
}

// Role is the A2A message role: work handed to the agent is authored by the "user" (the delegating
// caller); results the agent produces are authored by the "agent".
type Role string

const (
	RoleUser  Role = "user"
	RoleAgent Role = "agent"
)

// Part is one piece of an A2A Message's content (https://a2a-protocol.org/latest/specification/#part).
// switchboard's todo payload is opaque JSON, so it projects as a single DataPart; the Kind field
// discriminates the A2A part union ("text", "data", "file"). Text is populated for text parts; Data
// carries the decoded JSON payload for data parts.
type Part struct {
	Kind string          `json:"kind"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Message is an A2A message (https://a2a-protocol.org/latest/specification/#message): a role plus
// one or more parts. MessageID and TaskID/ContextID tie it to a task; the foundation populates the
// identifiers it can derive directly from the todo (TaskID) and leaves richer threading to the
// handler stories that record message history.
type Message struct {
	Role      Role   `json:"role"`
	Parts     []Part `json:"parts"`
	MessageID string `json:"messageId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	ContextID string `json:"contextId,omitempty"`
}

// TaskStatus is A2A's task status object (https://a2a-protocol.org/latest/specification/#taskstatus):
// the current state, an optional status message, and a timestamp. It projects directly from the
// todo's State and its most recent lifecycle timestamp.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

// Task is A2A's task object (https://a2a-protocol.org/latest/specification/#task): the wire
// projection of a single store.Todo row. ID is the todo id verbatim (SPEC-0018 REQ "Authorized
// SendMessage creates a todo": "a Task object whose id is the todo's id"); ContextID projects the
// todo's queue (the grouping key ListTasks filters on); Status carries the projected state. History
// is the recorded message list, bounded by GetTask's historyLength on read — empty in the base
// projection, which records only the delivered work as the sole history entry.
type Task struct {
	ID        string      `json:"id"`
	ContextID string      `json:"contextId,omitempty"`
	Kind      string      `json:"kind"`
	Status    TaskStatus  `json:"status"`
	History   []Message   `json:"history,omitempty"`
	Artifacts []Artifact  `json:"artifacts,omitempty"`
	Metadata  interface{} `json:"metadata,omitempty"`
}

// Artifact is an A2A output artifact (https://a2a-protocol.org/latest/specification/#artifact). The
// foundation projects a completed todo's result as a single artifact; the shape is defined here so
// the projection is complete, and the complete/streaming stories populate it on state transitions.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// TaskFromTodo projects a store.Todo onto its A2A Task wire representation. It is a pure translation:
// no store access, no mutation — the single source of truth for "what a todo looks like as an A2A
// Task". The projection is deliberately total: every Task field is derived from a Todo field, so a
// test can assert field-for-field equivalence (SPEC-0018 scenario "A2A-created and MCP-created todos
// are indistinguishable" is enforced in the store; this function guarantees the boundary translation
// itself never invents or drops state).
func TaskFromTodo(t store.Todo) Task {
	task := Task{
		ID:        t.ID,
		ContextID: t.Queue,
		Kind:      "task",
		Status: TaskStatus{
			State:     TaskStateFromTodo(t.State),
			Timestamp: statusTimestamp(t),
		},
	}
	// The delivered work is the task's originating user message: the todo title plus its payload,
	// projected as a single message so GetTask has a history entry to bound. An empty payload yields a
	// title-only text part; a present payload adds a data part carrying the raw JSON verbatim.
	msg := Message{Role: RoleUser, TaskID: t.ID, ContextID: t.Queue}
	if t.Title != "" {
		msg.Parts = append(msg.Parts, Part{Kind: "text", Text: t.Title})
	}
	if len(t.Payload) > 0 && json.Valid(t.Payload) {
		msg.Parts = append(msg.Parts, Part{Kind: "data", Data: json.RawMessage(t.Payload)})
	}
	if len(msg.Parts) > 0 {
		task.History = []Message{msg}
	}
	// A completed todo's result projects as an output artifact when it is valid JSON.
	if t.State == "done" && len(t.Result) > 0 && json.Valid(t.Result) {
		task.Artifacts = []Artifact{{
			ArtifactID: t.ID + "-result",
			Parts:      []Part{{Kind: "data", Data: json.RawMessage(t.Result)}},
		}}
	}
	return task
}

// statusTimestamp picks the todo timestamp that best represents its current state's "as of" moment:
// completion time for a terminal todo, else claim time for a working one, else creation time. It is
// formatted RFC 3339 in UTC, matching the todo projection the MCP tool layer already emits.
func statusTimestamp(t store.Todo) string {
	switch {
	case t.CompletedAt != nil:
		return t.CompletedAt.UTC().Format(time.RFC3339)
	case t.ClaimedAt != nil:
		return t.ClaimedAt.UTC().Format(time.RFC3339)
	default:
		return t.CreatedAt.UTC().Format(time.RFC3339)
	}
}
