package web

// Dead-Letter Context Is Invisible To SSE
//
// The store hands a dead-letter transition's Todo to the transition hook with FinalAttempt set, for
// the notification sinks. The Board's live stream is not one of those consumers: its frames must
// be byte-for-byte the same whether or not the field is set.
//
// Governing: SPEC-0034 REQ-15 "Dead-Letter Context for Notifications"; SPEC-0012 REQ "Live Updates
// via SSE".
//
// @joestump-agent 09/25/2026 - Added for #330 (epic #313).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

func TestDeadLetterSSEFrameIgnoresFinalAttempt(t *testing.T) {
	created := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	deadLetter := store.Todo{
		ID: "td_dead5", Queue: "ci", Source: "gitea", Kind: "push", State: "failed",
		Owner: "agent:0a1b2c3d", Attempt: 5, MaxAttempts: 5, CreatedAt: created,
		Result: []byte(`{"error":"tests"}`),
	}
	summary, artifact := "still red", "mcp://cairn/Zz9"
	withFinal := deadLetter
	withFinal.FinalAttempt = &store.FinalAttempt{
		Seq: 5, Outcome: "failed", Summary: &summary, Artifact: &artifact, AttemptsTotal: 5,
	}

	stream := func(td store.Todo) string {
		h := newTestHandler(t)
		h.keepAlive = time.Hour
		h.sseRetryMS = func(context.Context) int { return 1500 }
		rec := runEvents(t, h, 250*time.Millisecond, func() {
			time.Sleep(20 * time.Millisecond) // let the handler subscribe and write the retry frame
			h.PublishTodoTransition("failed", td)
		})
		return rec.Body.String()
	}

	without, with := stream(deadLetter), stream(withFinal)
	if !strings.Contains(without, "event: todo_failed\n") {
		t.Fatalf("no todo_failed frame on the stream:\n%q", without)
	}
	if with != without {
		t.Fatalf("FinalAttempt changed the SSE bytes:\nwithout: %q\nwith:    %q", without, with)
	}
	for _, leaked := range []string{summary, artifact} {
		if strings.Contains(with, leaked) {
			t.Fatalf("final-attempt field %q reached the SSE stream", leaked)
		}
	}
}
