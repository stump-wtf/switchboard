package store

// DB-backed tests for the SPEC-0024 ready hook: the trigger the notify-hook dispatcher subscribes
// to. It fires on push-eligible creations and on requeues back to pending, re-applies the SPEC-0011
// sender gate on the requeue path, and fires once per transition on the instance that performed it.
//
// Governing: SPEC-0024 REQ-6 "Trigger and Sender Gate".

import (
	"context"
	"sync"
	"testing"
	"time"
)

type readyRecorder struct {
	mu  sync.Mutex
	got []string // "<todo id>:<reason>:<attempt>"
}

func (r *readyRecorder) hook(t Todo, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, t.ID+":"+reason+":"+itoaTest(t.Attempt))
}

func (r *readyRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

func itoaTest(n int) string { // single digit attempts only
	return string(rune('0' + n))
}

func createEventTodo(t *testing.T, s *Store, ctx context.Context, ep, key string, verified bool, trust string) Todo {
	t.Helper()
	_, td, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: key,
			TrustMode: trust, Verified: verified, Payload: []byte("raw")},
		CreateTodoParams{EndpointID: ep, Queue: "inbox", Source: "gitea", Kind: "issue", Title: "t " + key,
			Payload: []byte(`{}`), IdempotencyKey: key})
	if err != nil || !created {
		t.Fatalf("create %s: created=%v err=%v", key, created, err)
	}
	return td
}

// abandonLease claims a todo and backdates its lease, as if its worker died holding it.
func abandonLease(t *testing.T, s *Store, ctx context.Context, ep, id string) {
	t.Helper()
	if _, err := s.ClaimTodo(ctx, ep, id, "agent:dead", time.Minute); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

func TestReadyHookCreatedAndSenderGate(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ready-create", "inbox")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	verified := createEventTodo(t, s, ctx, ep, "rc-1", true, "signed")
	token := createEventTodo(t, s, ctx, ep, "rc-2", false, "token")
	// Scenario "Unverified delivery calls no hook": the todo exists, and nothing fires.
	createEventTodo(t, s, ctx, ep, "rc-3", false, "signed")
	if got := rec.take(); len(got) != 2 || got[0] != verified.ID+":created:0" || got[1] != token.ID+":created:0" {
		t.Fatalf("ready fires = %v, want the verified and the token-trust creations only", got)
	}

	// Scenario "Redelivery does not re-fire".
	if _, _, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: "rc-1",
			TrustMode: "signed", Verified: true, Payload: []byte("raw")},
		CreateTodoParams{EndpointID: ep, Queue: "inbox", Title: "dup", Payload: []byte(`{}`), IdempotencyKey: "rc-1"}); err != nil || created {
		t.Fatalf("redelivery: created=%v err=%v", created, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("redelivery fired the ready hook: %v", got)
	}
}

// Scenario "Lease expiry re-fires", and the retry scheduler's requeue.
func TestReadyHookRequeued(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ready-requeue", "inbox")
	td := createEventTodo(t, s, ctx, ep, "rq-1", true, "signed")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	abandonLease(t, s, ctx, ep, td.ID)
	if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap = %d, %v", n, err)
	}
	if got := rec.take(); len(got) != 1 || got[0] != td.ID+":requeued:1" {
		t.Fatalf("reaper ready fires = %v, want one requeued with attempt 1", got)
	}

	// A failed todo whose scheduled retry is due comes back through the retry scheduler.
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET state='failed', next_retry_at = now() - interval '1 second' WHERE id = $1`, td.ID); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if n, err := s.RequeueDueRetries(ctx); err != nil || n != 1 {
		t.Fatalf("requeue retries = %d, %v", n, err)
	}
	if got := rec.take(); len(got) != 1 || got[0] != td.ID+":requeued:1" {
		t.Fatalf("retry scheduler ready fires = %v", got)
	}

	// A reaped todo that exhausted its attempts dead-letters: it is not ready and fires nothing.
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET max_attempts = 2 WHERE id = $1`, td.ID); err != nil {
		t.Fatalf("cap attempts: %v", err)
	}
	abandonLease(t, s, ctx, ep, td.ID)
	if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap to dead-letter = %d, %v", n, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("a dead-lettered todo fired the ready hook: %v", got)
	}
}

// Scenario "Requeue re-applies the sender gate": an unverified todo claimed by id and abandoned
// comes back to pending through both requeue paths without firing.
func TestReadyHookRequeueReappliesSenderGate(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ready-gate", "inbox")
	td := createEventTodo(t, s, ctx, ep, "rg-1", false, "signed")
	plain, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "inbox", Title: "no event", IdempotencyKey: "rg-2"})
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	abandonLease(t, s, ctx, ep, td.ID)
	abandonLease(t, s, ctx, ep, plain.ID)
	if n, err := s.ReapExpired(ctx); err != nil || n != 2 {
		t.Fatalf("reap = %d, %v", n, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET state='failed', next_retry_at = now() - interval '1 second' WHERE id = ANY($1)`,
		[]string{td.ID, plain.ID}); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if n, err := s.RequeueDueRetries(ctx); err != nil || n != 2 {
		t.Fatalf("requeue = %d, %v", n, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("an unverified or event-less todo fired on requeue: %v", got)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM todos WHERE id = $1`, td.ID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("unverified todo state = %q, %v; it must still be claimable", state, err)
	}
}

// Scenario "Two instances, one notification": two stores on one database each see only the
// transitions their own statements performed.
func TestReadyHookOnePerTransitionAcrossInstances(t *testing.T) {
	s1, ctx := testStore(t)
	s2 := New(s1.pool)
	ep := seedEndpoint(t, s1, ctx, "ready-two", "inbox")
	r1, r2 := &readyRecorder{}, &readyRecorder{}
	s1.SetTodoReadyHook(r1.hook)
	s2.SetTodoReadyHook(r2.hook)

	td := createEventTodo(t, s1, ctx, ep, "rt-1", true, "signed")
	if got1, got2 := r1.take(), r2.take(); len(got1) != 1 || len(got2) != 0 {
		t.Fatalf("creation fired on instance 1 %v and instance 2 %v; want exactly one, on 1", got1, got2)
	}

	// Both instances' reapers race for the same expired lease: exactly one moves it.
	abandonLease(t, s1, ctx, ep, td.ID)
	var wg sync.WaitGroup
	for _, s := range []*Store{s1, s2} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			if _, err := s.ReapExpired(ctx); err != nil {
				t.Errorf("reap: %v", err)
			}
		}(s)
	}
	wg.Wait()
	if n := len(r1.take()) + len(r2.take()); n != 1 {
		t.Fatalf("racing reapers fired %d notifications for one requeue, want 1", n)
	}
}
