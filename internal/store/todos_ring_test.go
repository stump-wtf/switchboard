// Doorbell heartbeat: re-ringing pending todos nobody claimed.
//
// The push is a hint and the queue is the ledger, but under push-only delivery that was only half
// true: a doorbell that arrived while every worker was mid-turn, or was dropped by a transport
// fault, was never repeated, so the todo sat pending forever. Fifty rows accumulated that way.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0013; SPEC-0011.
package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// age backdates a todo's created_at and its ring bookkeeping so a sweep sees it as due.
func age(t *testing.T, s *Store, ctx context.Context, id string, createdAgo time.Duration, attempts int, ringedAgo *time.Duration) {
	t.Helper()
	if ringedAgo == nil {
		_, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - $2::interval,
			ring_attempts = $3, last_ringed_at = NULL WHERE id = $1`, id, createdAgo, attempts)
		if err != nil {
			t.Fatalf("age: %v", err)
		}
		return
	}
	_, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - $2::interval,
		ring_attempts = $3, last_ringed_at = now() - $4::interval WHERE id = $1`,
		id, createdAgo, attempts, *ringedAgo)
	if err != nil {
		t.Fatalf("age: %v", err)
	}
}

// seedRingingTodo enqueues a todo the way the ingest path does — through CreateEventTodos, so it
// carries a delivery event with a trust mode. RingUnclaimed only rings doorbell-eligible rows
// (SPEC-0011 sender gate), so a sweep test must seed the event, not the bare todo.
func seedRingingTodo(t *testing.T, s *Store, ctx context.Context, ep, title string, verified bool, trustMode string) Todo {
	t.Helper()
	uniq := fmt.Sprintf("ring-%s-%d", title, time.Now().UnixNano())
	_, out, err := s.CreateEventTodos(ctx, EventInput{
		Source: "test-source", Family: "webhook", EventType: "ping",
		ExternalID: uniq, TrustMode: trustMode, Verified: verified,
	}, []string{ep}, CreateTodoParams{Queue: "forge", Title: title, Source: "test"})
	if err != nil {
		t.Fatalf("seed todo (%s): %v", title, err)
	}
	if len(out) != 1 {
		t.Fatalf("seed todo (%s): %d rows", title, len(out))
	}
	return out[0].Todo
}

func TestRingUnclaimedRepeatsAnUnpickedDoorbell(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")

	td := seedRingingTodo(t, s, ctx, ep, "unpicked", true, "signed")

	// Freshly created: still inside the first backoff, so it must NOT be rung — a todo whose
	// original doorbell may still be in flight must not be immediately doubled.
	if got, err := s.RingUnclaimed(ctx); err != nil || len(got) != 0 {
		t.Fatalf("fresh todo rung too early: %d rows, err %v", len(got), err)
	}

	age(t, s, ctx, td.ID, 10*time.Minute, 0, nil)
	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != 1 || got[0].ID != td.ID {
		t.Fatalf("expected the aged todo to be rung, got %d rows", len(got))
	}

	// Marked in the same statement, so a second sweep immediately after does not re-select it.
	if again, err := s.RingUnclaimed(ctx); err != nil || len(again) != 0 {
		t.Fatalf("same todo rung twice in a row: %d rows, err %v", len(again), err)
	}
}

// Token-trust self-managed webhooks authenticate by the unguessable ingest URL (SPEC-0006), so
// their deliveries ring the doorbell like verified ones — the reaper must re-ring them too.
func TestRingUnclaimedRepeatsTokenTrustDoorbell(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")

	td := seedRingingTodo(t, s, ctx, ep, "token-delivery", false, "token")
	age(t, s, ctx, td.ID, 10*time.Minute, 0, nil)

	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != 1 || got[0].ID != td.ID {
		t.Fatalf("expected the token-trust todo to be rung, got %d rows", len(got))
	}
}

// Sender gate (SPEC-0011): the reaper republishes through the same path a fresh delivery uses, so
// it must withhold exactly what the create path withholds — unverified (open-trust) deliveries and
// event-less todos degrade to pull, because the todo title is attacker-reachable text and verified
// attribution is the gate (ADR-0013). The gate lives in the wakeup SQL so it cannot be bypassed.
func TestRingUnclaimedNeverRingsUnverifiedOrEventlessTodos(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	long := 24 * time.Hour

	open := seedRingingTodo(t, s, ctx, ep, "unverified open delivery", false, "open")
	age(t, s, ctx, open.ID, 48*time.Hour, 0, &long)

	eventless, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "forge",
		Title: "created without an event", Source: "test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	age(t, s, ctx, eventless.ID, 48*time.Hour, 0, &long)

	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rang %d todos that must degrade to pull: %v", len(got), got)
	}
}

// A todo nobody wants must stop costing model turns. Five doorbells over ~7h is the budget.
func TestRingUnclaimedStopsAtTheAttemptCap(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	td := seedRingingTodo(t, s, ctx, ep, "nobody wants this", true, "signed")
	long := 24 * time.Hour
	age(t, s, ctx, td.ID, 48*time.Hour, ringMaxAttempts, &long)

	if got, err := s.RingUnclaimed(ctx); err != nil || len(got) != 0 {
		t.Fatalf("a todo at the attempt cap was rung again: %d rows, err %v", len(got), err)
	}
}

// Only pending rows. A claimed todo has a lease and a working owner; ringing it would wake a second
// worker for work already in hand.
func TestRingUnclaimedIgnoresClaimedAndDoneTodos(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	long := 24 * time.Hour

	claimed := seedRingingTodo(t, s, ctx, ep, "in hand", true, "signed")
	age(t, s, ctx, claimed.ID, 48*time.Hour, 0, &long)
	if _, err := s.ClaimTodo(ctx, ep, claimed.ID, "agent:x", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	done := seedRingingTodo(t, s, ctx, ep, "finished", true, "signed")
	age(t, s, ctx, done.ID, 48*time.Hour, 0, &long)
	if _, err := s.ClaimTodo(ctx, ep, done.ID, "agent:x", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, ep, done.ID, "agent:x", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	for _, r := range got {
		if r.ID == claimed.ID || r.ID == done.ID {
			t.Fatalf("rang a %s todo: %s", r.State, r.ID)
		}
	}
}

// A backlog is exactly when this matters: waking every worker for fifty rows at once is a token
// bomb, since each push costs a model turn.
func TestRingUnclaimedIsCappedPerSweep(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	long := 24 * time.Hour
	for i := 0; i < ringSweepLimit*3; i++ {
		td := seedRingingTodo(t, s, ctx, ep, fmt.Sprintf("backlog-%d", i), true, "signed")
		age(t, s, ctx, td.ID, 48*time.Hour, 1, &long)
	}
	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != ringSweepLimit {
		t.Fatalf("swept %d todos, want the cap of %d — an unbounded sweep is a token bomb", len(got), ringSweepLimit)
	}
}

// The heartbeat must never spend its budget on a revoked endpoint.
//
// A revoked endpoint's todos are undrainable by construction — the credential no longer
// authenticates, so no session can ever receive the push — and because the sweep is oldest-first
// and globally ordered, they are also the OLDEST rows in the table. In production 1,442 pending
// rows across two revoked endpoints absorbed every sweep while the live endpoints holding real
// work were never reached, and the symptom was silent: each ring resolved to no session, so the
// publish returned early and logged neither a delivery nor a drop.
func TestRingUnclaimedSkipsRevokedEndpoints(t *testing.T) {
	s, ctx := testStore(t)
	dead := seedEndpoint(t, s, ctx, "revoked-ep", "forge")
	live := seedEndpoint(t, s, ctx, "live-ep", "forge")
	long := 24 * time.Hour

	// The revoked endpoint's rows are OLDER, so oldest-first ordering would pick them first.
	for i := 0; i < ringSweepLimit*2; i++ {
		td := seedRingingTodo(t, s, ctx, dead, "undrainable", true, "signed")
		age(t, s, ctx, td.ID, 72*time.Hour, 1, &long)
	}
	liveTodo := seedRingingTodo(t, s, ctx, live, "real work someone asked for", true, "signed")
	age(t, s, ctx, liveTodo.ID, time.Hour, 1, &long)

	// Revoke directly: RevokeEndpoint needs the owning human, which this fixture does not model.
	if _, err := s.pool.Exec(ctx, `UPDATE endpoints SET state='revoked' WHERE id=$1`, dead); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the live endpoint's todo was never reached")
	}
	for _, r := range got {
		if r.EndpointID == dead {
			t.Fatalf("rang a todo on a revoked endpoint: %s (%s)", r.ID, r.Title)
		}
	}
	if got[0].ID != liveTodo.ID {
		t.Fatalf("expected the live endpoint's todo, got %s", got[0].Title)
	}
}
