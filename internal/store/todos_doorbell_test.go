package store

import "testing"

// Governing: SPEC-0011 REQ "Sender Gate and Injection Safety" — only todos persisted with a
// VERIFIED delivery event are eligible for a channel push. The doorbell hook fires exactly once
// per newly created verified todo; unverified events, plain (event-less) creates, and idempotent
// duplicates never ring it.
func TestTodoDoorbellHookSenderGate(t *testing.T) {
	s, ctx := testStore(t)

	var rang []string
	s.SetTodoDoorbellHook(func(td Todo) { rang = append(rang, td.ID) })
	defer s.SetTodoDoorbellHook(nil)

	// Verified event + new todo → exactly one doorbell.
	_, td, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "db-1",
			TrustMode: "signed", Verified: true, Payload: []byte("raw")},
		CreateTodoParams{Queue: "reviews", Source: "github", Kind: "push", Title: "verified",
			Payload: []byte(`{}`), IdempotencyKey: "db-1"})
	if err != nil || !created {
		t.Fatalf("verified create: created=%v err=%v", created, err)
	}
	if len(rang) != 1 || rang[0] != td.ID {
		t.Fatalf("doorbell after verified create = %v, want exactly [%s]", rang, td.ID)
	}

	// Idempotent duplicate of the same delivery → no new todo, no doorbell.
	if _, _, created, err = s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "db-1",
			TrustMode: "signed", Verified: true, Payload: []byte("raw")},
		CreateTodoParams{Queue: "reviews", Source: "github", Kind: "push", Title: "verified",
			Payload: []byte(`{}`), IdempotencyKey: "db-1"}); err != nil || created {
		t.Fatalf("duplicate delivery: created=%v err=%v", created, err)
	}
	if len(rang) != 1 {
		t.Fatalf("duplicate delivery rang the doorbell: %v", rang)
	}

	// Unverified event → todo persists (queue is the ledger) but MUST NOT push.
	if _, _, created, err = s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "db-2",
			TrustMode: "open", Verified: false, Payload: []byte("raw")},
		CreateTodoParams{Queue: "reviews", Source: "github", Title: "unverified",
			Payload: []byte(`{}`), IdempotencyKey: "db-2"}); err != nil || !created {
		t.Fatalf("unverified create: created=%v err=%v", created, err)
	}
	// Plain create (no delivery event, e.g. the dev helper) → no verified sender, no push.
	if _, _, err = s.CreateTodo(ctx, CreateTodoParams{Queue: "reviews", Title: "plain", IdempotencyKey: "db-3"}); err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if len(rang) != 1 {
		t.Fatalf("sender gate leaked: doorbells = %v, want exactly one", rang)
	}
}
