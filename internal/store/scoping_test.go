package store

// DB-backed coverage for the #186 scoping seams: EndpointOwner (routes the endpoint_seen SSE
// frame to its owning human) and PendingDoorbellTodos (the todo_ready LISTEN nudge with the
// SPEC-0011 sender gate applied in SQL). Skips without SWITCHBOARD_TEST_DATABASE_URL, like the
// rest of this suite.

import (
	"errors"
	"testing"
	"time"
)

func TestEndpointOwnerResolvesOwningHuman(t *testing.T) {
	s, ctx := testStore(t)

	h, err := s.UpsertHuman(ctx, "pocket|owner", "Owner", "owner@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	ag, err := s.CreateAgent(ctx, h.ID, "scoped-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	slug, err := MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-owner", "sbk_own", slug, []string{"ci"}, []string{"list_todos"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}

	owner, err := s.EndpointOwner(ctx, ep.ID)
	if err != nil {
		t.Fatalf("endpoint owner: %v", err)
	}
	if owner != h.ID {
		t.Fatalf("owner = %q, want %q (endpoint → agent → owner_human_id)", owner, h.ID)
	}
	// Unknown ids are ErrNotFound so the SSE publisher can fail closed instead of broadcasting.
	if _, err := s.EndpointOwner(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown endpoint err = %v, want ErrNotFound", err)
	}
}

// Governing: SPEC-0011 REQ "Sender Gate and Injection Safety" — only pending todos whose delivery
// event passed verification are eligible for the notification-driven doorbell nudge; event-less
// and unverified todos degrade to pull.
func TestPendingDoorbellTodosAppliesSenderGate(t *testing.T) {
	s, ctx := testStore(t)

	// Verified delivery → eligible.
	_, verified, _, err := s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", ExternalID: "v1", TrustMode: "signed", Verified: true},
		CreateTodoParams{Queue: "ci", Title: "verified work"})
	if err != nil {
		t.Fatalf("create verified event todo: %v", err)
	}
	// Unverified delivery → gated out.
	if _, _, _, err := s.CreateEventTodo(ctx,
		EventInput{Source: "generic", Family: "webhook", ExternalID: "u1", TrustMode: "open", Verified: false},
		CreateTodoParams{Queue: "ci", Title: "unverified work"}); err != nil {
		t.Fatalf("create unverified event todo: %v", err)
	}
	// Event-less todo (dev helper / queue adapter) → gated out.
	if _, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "ci", Title: "eventless work"}); err != nil {
		t.Fatalf("create eventless todo: %v", err)
	}
	// Verified but on another queue → not part of this queue's nudge.
	if _, _, _, err := s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", ExternalID: "v2", TrustMode: "signed", Verified: true},
		CreateTodoParams{Queue: "deploys", Title: "other queue"}); err != nil {
		t.Fatalf("create other-queue todo: %v", err)
	}

	got, err := s.PendingDoorbellTodos(ctx, "ci", 10)
	if err != nil {
		t.Fatalf("pending doorbell todos: %v", err)
	}
	if len(got) != 1 || got[0].ID != verified.ID {
		t.Fatalf("eligible = %+v, want exactly the verified ci todo %s", got, verified.ID)
	}

	// A claimed todo leaves the pending nudge set — the doorbell is for claimable work only.
	if _, err := s.ClaimTodo(ctx, verified.ID, "agent:x", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err = s.PendingDoorbellTodos(ctx, "ci", 10)
	if err != nil {
		t.Fatalf("pending doorbell todos after claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("claimed todo still nudged: %+v", got)
	}
}
