package store

import "testing"

// Governing: SPEC-0018 REQ "Task State Machine Extension" — the A2A wire projection maps the four
// original states and passes the four new ones through unchanged. Pure function, no DB.
func TestA2ATaskStateProjection(t *testing.T) {
	cases := map[string]string{
		StatePending:       "submitted",
		StateClaimed:       "working",
		StateDone:          "completed",
		StateFailed:        "failed",
		StateCanceled:      "canceled",
		StateRejected:      "rejected",
		StateInputRequired: "input-required",
		StateAuthRequired:  "auth-required",
	}
	for internal, want := range cases {
		if got := A2ATaskState(internal); got != want {
			t.Errorf("A2ATaskState(%q)=%q want %q", internal, got, want)
		}
	}
	// An unrecognized state (unreachable for a persisted row under the 0012 CHECK) degrades to
	// passthrough rather than blanking the wire value.
	if got := A2ATaskState("weird"); got != "weird" {
		t.Errorf("A2ATaskState passthrough: got %q want %q", got, "weird")
	}
}

// Governing: SPEC-0018 REQ "Task State Machine Extension" — terminal/interrupt classifiers used by
// the transition helpers and downstream A2A handlers.
func TestStateClassifiers(t *testing.T) {
	terminal := []string{StateDone, StateFailed, StateCanceled, StateRejected}
	for _, s := range terminal {
		if !IsTerminalState(s) {
			t.Errorf("IsTerminalState(%q)=false want true", s)
		}
		if IsInterruptState(s) {
			t.Errorf("IsInterruptState(%q)=true want false", s)
		}
	}
	interrupt := []string{StateInputRequired, StateAuthRequired}
	for _, s := range interrupt {
		if !IsInterruptState(s) {
			t.Errorf("IsInterruptState(%q)=false want true", s)
		}
		if IsTerminalState(s) {
			t.Errorf("IsTerminalState(%q)=true want false", s)
		}
	}
	// Live, non-terminal, non-interrupt states.
	for _, s := range []string{StatePending, StateClaimed} {
		if IsTerminalState(s) {
			t.Errorf("IsTerminalState(%q)=true want false", s)
		}
		if IsInterruptState(s) {
			t.Errorf("IsInterruptState(%q)=true want false", s)
		}
	}
}
