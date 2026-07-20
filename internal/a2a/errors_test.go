package a2a

import (
	"errors"
	"fmt"
	"testing"
)

// TestSentinelToCode asserts each A2A sentinel maps to the correct A2A/JSON-RPC error code, both
// bare and wrapped with contextual detail at a layer boundary (errors.Is unwraps the chain, so
// wrapping must never lose the mapping). Governing: SPEC-0018 REQ "Error Handling Standards"
// ("Sentinel errors MUST be defined ... so callers can distinguish them programmatically and map
// them to the correct A2A error codes"; scenario "Unsupported operation maps to the correct A2A
// error").
func TestSentinelToCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unsupported operation", ErrUnsupportedOperation, CodeUnsupportedOperation},
		{"push notification not supported", ErrPushNotificationNotSupported, CodePushNotificationNotSupported},
		{"rate limited", ErrRateLimited, CodeRateLimited},
		{"task not found", ErrTaskNotFound, CodeTaskNotFound},
		{"forbidden", ErrForbidden, CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Bare sentinel.
			if got := ErrorToRPC(tc.err); got.Code != tc.want {
				t.Fatalf("bare: code = %d, want %d", got.Code, tc.want)
			}
			// Wrapped once at a boundary.
			wrapped := fmt.Errorf("SendMessage failed: %w", tc.err)
			if got := ErrorToRPC(wrapped); got.Code != tc.want {
				t.Fatalf("wrapped: code = %d, want %d", got.Code, tc.want)
			}
			// Wrapped twice (two layer boundaries).
			twice := fmt.Errorf("handler: %w", fmt.Errorf("store: %w", tc.err))
			if got := ErrorToRPC(twice); got.Code != tc.want {
				t.Fatalf("wrapped twice: code = %d, want %d", got.Code, tc.want)
			}
		})
	}
}

// TestUnrecognizedErrorIsInternal asserts an error not in the sentinel set maps to the generic
// internal code with a non-leaking message — the handler is expected to have logged the real chain.
func TestUnrecognizedErrorIsInternal(t *testing.T) {
	got := ErrorToRPC(errors.New("some unexpected database failure with sensitive detail"))
	if got.Code != CodeInternalError {
		t.Fatalf("code = %d, want %d (internal)", got.Code, CodeInternalError)
	}
	if got.Message != "internal error" {
		t.Fatalf("message = %q, want generic %q — must not leak the underlying error", got.Message, "internal error")
	}
}

// TestNilErrorMapsToNil asserts ErrorToRPC(nil) is nil (a nil error is not an error object).
func TestNilErrorMapsToNil(t *testing.T) {
	if got := ErrorToRPC(nil); got != nil {
		t.Fatalf("ErrorToRPC(nil) = %+v, want nil", got)
	}
}

// TestPrebuiltRPCErrorPassesThrough asserts a handler that returns an already-constructed *RPCError
// (e.g. a lifecycle-specific one) has it passed through unchanged rather than remapped to internal.
func TestPrebuiltRPCErrorPassesThrough(t *testing.T) {
	orig := &RPCError{Code: CodeTaskNotFound, Message: "task td_x not found"}
	got := ErrorToRPC(fmt.Errorf("wrapped: %w", orig))
	if got != orig {
		t.Fatalf("prebuilt *RPCError was not passed through: got %+v", got)
	}
}
