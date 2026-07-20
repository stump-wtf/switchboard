// Governing: ADR-0021 (A2A task-delegation transport), SPEC-0018 REQ "Error Handling Standards"
//
// Package a2a implements switchboard's native A2A (agent-to-agent) task RPC surface as a second
// wire protocol over the same authorized relationship MCP's vended endpoints already use. An A2A
// Task is a PROJECTION of a durable Todo (ADR-0007/SPEC-0003), never a parallel domain object — the
// wire types in types.go translate Todo<->A2A at the handler boundary only.
//
// This file defines the A2A error surface: sentinel errors for A2A-specific failure modes and the
// JSON-RPC error envelope they map onto. The A2A protocol layers its own error codes on top of the
// JSON-RPC 2.0 base codes; the constants below reproduce both so a handler can return the SPECIFIC
// error for a condition rather than a generic failure (SPEC-0018 REQ "Error Handling Standards":
// "switchboard MUST return the specific A2A sentinel error for that condition, not a generic
// failure"). Every handler wraps store/lower-layer errors with context at its boundary and maps the
// wrapped chain onto one of these codes via ErrorToRPC; nothing is silently swallowed.
package a2a

import (
	"errors"
	"fmt"
)

// JSON-RPC 2.0 base error codes (https://www.jsonrpc.org/specification#error_object). The A2A
// protocol reuses these for transport/parse-level failures before any A2A-specific code applies.
const (
	CodeParseError     = -32700 // invalid JSON was received
	CodeInvalidRequest = -32600 // the JSON sent is not a valid Request object
	CodeMethodNotFound = -32601 // the method does not exist / is not available
	CodeInvalidParams  = -32602 // invalid method parameters
	CodeInternalError  = -32603 // internal JSON-RPC error
)

// A2A-specific error codes, reserved in the -32001..-32099 server-error range by the A2A protocol
// specification (https://a2a-protocol.org/latest/specification/#error-handling). Only the codes this
// capability's foundation needs are defined here; task-lifecycle codes (TaskNotFound, etc.) that
// belong to individual RPC-method stories are added alongside those handlers.
const (
	// CodeUnsupportedOperation — the requested operation is not supported by this agent (e.g. a
	// capability-gated method the caller's endpoint scope does not advertise). A2A code -32004.
	CodeUnsupportedOperation = -32004
	// CodePushNotificationNotSupported — the agent does not support push notification configuration.
	// A2A code -32003.
	CodePushNotificationNotSupported = -32003
	// CodeTaskNotFound — the referenced task (todo) does not exist or is not visible to the caller.
	// A2A code -32001. Defined here so the foundation's error mapping is complete; the GetTask story
	// is what first returns it.
	CodeTaskNotFound = -32001

	// CodeRateLimited is not an A2A-reserved code; A2A leaves rate limiting to the HTTP layer (429).
	// SPEC-0018 REQ "Task-Creation Volume Rate Limiting" nonetheless requires a programmatically
	// distinguishable rate-limit error, so a stable code in the implementation-defined server-error
	// range (-32000) is used for the JSON-RPC body when a 429 also carries an error object.
	CodeRateLimited = -32000
)

// Sentinel errors for A2A-specific failure modes (SPEC-0018 REQ "Error Handling Standards":
// "Sentinel errors MUST be defined for A2A-specific failure modes ... so callers can distinguish
// them programmatically and map them to the correct A2A error codes"). Handlers wrap these with
// contextual detail via fmt.Errorf("%w", ...) at each layer boundary; ErrorToRPC unwraps the chain
// with errors.Is, so wrapping never loses the mapping.
var (
	// ErrUnsupportedOperation is returned when a caller invokes a capability-gated operation their
	// endpoint scope does not advertise (SPEC-0018 REQ "Streaming ... MUST return
	// UnsupportedOperationError if the caller's endpoint scope does not advertise the streaming
	// capability"). Maps to CodeUnsupportedOperation.
	ErrUnsupportedOperation = errors.New("a2a: unsupported operation")

	// ErrPushNotificationNotSupported is returned for calls that touch push-notification behavior the
	// agent does not support. Maps to CodePushNotificationNotSupported. The push-notifications
	// capability itself is a separate spec; this sentinel exists in the foundation so any handler can
	// reject a push-config call with the correct A2A code from day one.
	ErrPushNotificationNotSupported = errors.New("a2a: push notification not supported")

	// ErrRateLimited is returned when a vended endpoint exceeds its per-endpoint SendMessage rate
	// limit (SPEC-0018 REQ "Task-Creation Volume Rate Limiting"). Maps to CodeRateLimited and, at the
	// HTTP layer, to 429.
	ErrRateLimited = errors.New("a2a: rate limit exceeded")

	// ErrTaskNotFound is returned when a referenced task/todo does not exist or is not visible to the
	// caller. Maps to CodeTaskNotFound.
	ErrTaskNotFound = errors.New("a2a: task not found")

	// ErrUnauthenticated is returned when a call presents no valid vended-endpoint credential. It is
	// answered at the HTTP layer with 401 (identically to an unauthenticated MCP call), so it does not
	// carry an in-body JSON-RPC code; it exists as a sentinel so the auth boundary can signal the
	// condition without a bare string.
	ErrUnauthenticated = errors.New("a2a: unauthenticated")

	// ErrForbidden is returned when a valid credential is presented but its scope does not authorize
	// the request (e.g. wrong endpoint path, queue outside grant). Answered with 403 at the HTTP
	// layer, mirroring the MCP surface's credential/path-mismatch response.
	ErrForbidden = errors.New("a2a: forbidden")
)

// RPCError is the JSON-RPC 2.0 error object returned in a response envelope. Code is one of the
// constants above; Message is a stable, human-readable summary that never leaks credentials or
// internal detail; Data is optional structured context.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("a2a rpc error %d: %s", e.Code, e.Message)
}

// ErrorToRPC maps an error chain onto its JSON-RPC error object, unwrapping with errors.Is so a
// sentinel wrapped with contextual detail at a layer boundary still maps correctly. An already-built
// *RPCError passes through unchanged (a handler may construct a lifecycle-specific one directly). An
// unrecognized error maps to CodeInternalError with a generic message — the caller learns nothing
// beyond "internal error", while the handler is expected to have logged the wrapped chain with
// structured context before calling this. Governing: SPEC-0018 REQ "Error Handling Standards".
func ErrorToRPC(err error) *RPCError {
	if err == nil {
		return nil
	}
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc
	}
	switch {
	case errors.Is(err, ErrUnsupportedOperation):
		return &RPCError{Code: CodeUnsupportedOperation, Message: "unsupported operation"}
	case errors.Is(err, ErrPushNotificationNotSupported):
		return &RPCError{Code: CodePushNotificationNotSupported, Message: "push notification not supported"}
	case errors.Is(err, ErrRateLimited):
		return &RPCError{Code: CodeRateLimited, Message: "rate limit exceeded"}
	case errors.Is(err, ErrTaskNotFound):
		return &RPCError{Code: CodeTaskNotFound, Message: "task not found"}
	case errors.Is(err, ErrForbidden):
		// A forbidden condition that reaches the JSON-RPC body (rather than being answered as a bare
		// 403 at the HTTP layer) maps to InvalidRequest — the request is well-formed but not
		// authorized for what it asked.
		return &RPCError{Code: CodeInvalidRequest, Message: "forbidden"}
	default:
		return &RPCError{Code: CodeInternalError, Message: "internal error"}
	}
}
