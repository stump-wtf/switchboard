// Governing: ADR-0021 (A2A task-delegation transport), SPEC-0018 REQ "Task State Machine Extension".
//
// This file holds the Todo↔A2A state vocabulary: the internal state constants (the four original
// ADR-0007 states plus the four A2A additions), the projection that maps an internal Todo.State onto
// A2A's wire-boundary TaskState, and small classifiers (terminal / interrupt) the transition helpers
// and downstream A2A handlers share. It is a mapping layer, NOT a rename: the internal primitive
// keeps the name `Todo` and the DB keeps the internal state strings (ADR-0021 "Keep 'Todo' as the
// internal name; 'Task' only at the A2A boundary").
package store

// Internal Todo.State values. The first four predate A2A (ADR-0007); the last four are the SPEC-0018
// additions. These are the exact strings stored in todos.state and enforced by the 0012 CHECK.
const (
	StatePending       = "pending"
	StateClaimed       = "claimed"
	StateDone          = "done"
	StateFailed        = "failed"
	StateCanceled      = "canceled"
	StateRejected      = "rejected"
	StateInputRequired = "input-required"
	StateAuthRequired  = "auth-required"
)

// A2A TaskState wire values (https://a2a-protocol.org/latest/specification/#taskstate). Only the
// subset switchboard projects onto is enumerated; the four new internal states pass through under
// the identical A2A spelling, so no separate constant is needed for them.
const (
	A2AStateSubmitted = "submitted"
	A2AStateWorking   = "working"
	A2AStateCompleted = "completed"
	A2AStateFailed    = "failed"
)

// a2aStateProjection maps each internal Todo.State onto its A2A TaskState. The four original states
// are renamed at the wire boundary (pending→submitted, claimed→working, done→completed,
// failed→failed); the four A2A-native states (canceled, rejected, input-required, auth-required)
// pass through unchanged because A2A already spells them the same way.
var a2aStateProjection = map[string]string{
	StatePending:       A2AStateSubmitted,
	StateClaimed:       A2AStateWorking,
	StateDone:          A2AStateCompleted,
	StateFailed:        A2AStateFailed,
	StateCanceled:      StateCanceled,      // "canceled"
	StateRejected:      StateRejected,      // "rejected"
	StateInputRequired: StateInputRequired, // "input-required"
	StateAuthRequired:  StateAuthRequired,  // "auth-required"
}

// A2ATaskState projects an internal Todo.State onto its A2A TaskState wire value (SPEC-0018 REQ
// "Task State Machine Extension"). It is a projection, not a rename: the store and MCP surface keep
// the internal names; only the A2A HTTP/JSON-RPC layer speaks these. An unrecognized state (which
// the 0012 CHECK constraint makes unreachable for a persisted row) maps to itself so the boundary
// degrades to passthrough rather than silently blanking an unknown state.
func A2ATaskState(todoState string) string {
	if s, ok := a2aStateProjection[todoState]; ok {
		return s
	}
	return todoState
}

// IsTerminalState reports whether a Todo.State is terminal — no further transition is defined except
// an explicit operator retry of a dead-lettered `failed` todo (SPEC-0003). `done`, `canceled`, and
// `rejected` are unconditionally terminal; `failed` is terminal-shaped (it is a resting state) and
// is grouped here so terminal-state consumers (e.g. an A2A stream that closes on a final event,
// SPEC-0018) treat it alongside the others. `canceled` and `rejected` are terminal but distinct from
// `failed`: neither is ever retried or dead-lettered.
func IsTerminalState(todoState string) bool {
	switch todoState {
	case StateDone, StateFailed, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// IsInterruptState reports whether a Todo.State is an A2A interrupt state — a claimed todo paused
// waiting on the client (input-required) or on auth resolution (auth-required). An interrupt state
// retains the todo's owner and lease and returns to `claimed` when the requirement is supplied
// (SPEC-0018 REQ "Task State Machine Extension"), so it is neither claimable-fresh nor terminal.
func IsInterruptState(todoState string) bool {
	switch todoState {
	case StateInputRequired, StateAuthRequired:
		return true
	default:
		return false
	}
}
