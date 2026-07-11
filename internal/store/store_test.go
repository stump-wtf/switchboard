package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/db"
)

// testStore connects to the test database (SWITCHBOARD_TEST_DATABASE_URL), migrates, and truncates.
// Tests skip cleanly when no test DB is configured, so `go test ./...` stays green without Postgres.
func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run store tests")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, todos, events, sessions, adapters RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return New(pool), ctx
}

func TestHumanAgentVend(t *testing.T) {
	s, ctx := testStore(t)

	h, err := s.UpsertHuman(ctx, "pocket|abc", "Joe", "joe@stump.rocks")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	// Upsert is idempotent on subject.
	h2, err := s.UpsertHuman(ctx, "pocket|abc", "Joe Stump", "")
	if err != nil || h2.ID != h.ID {
		t.Fatalf("re-upsert should return same id: %v (%s vs %s)", err, h2.ID, h.ID)
	}

	ag, err := s.CreateAgent(ctx, h.ID, "reviewer-bot", "reviews PRs")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	slug, err := MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	if !strings.HasPrefix(slug, "reviewer-bot-") {
		t.Fatalf("slug should derive from agent name, got %q", slug)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash123", "sbk_ab12cd", slug, []string{"reviews"}, []string{"list_todos", "claim", "complete"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	if ep.State != "active" || ep.Mutability != "immutable" {
		t.Fatalf("endpoint defaults: state=%s mutability=%s", ep.State, ep.Mutability)
	}
	if ep.Slug != slug {
		t.Fatalf("endpoint slug=%q want %q", ep.Slug, slug)
	}

	// Resolve by credential hash → scope + owner.
	auth, err := s.EndpointByCredHash(ctx, "credhash123")
	if err != nil {
		t.Fatalf("auth by credhash: %v", err)
	}
	if auth.OwnerHumanID != h.ID || auth.AgentID != ag.ID || len(auth.ScopeQueues) != 1 || auth.ScopeQueues[0] != "reviews" {
		t.Fatalf("auth resolved wrong: %+v", auth)
	}
	if auth.Slug != slug {
		t.Fatalf("auth slug=%q want %q", auth.Slug, slug)
	}

	// Successful auth stamps last-seen (SPEC-0014).
	if err := s.TouchEndpoint(ctx, ep.ID); err != nil {
		t.Fatalf("touch endpoint: %v", err)
	}

	// Revoke → credential no longer resolves.
	if err := s.RevokeEndpoint(ctx, ep.ID, h.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.EndpointByCredHash(ctx, "credhash123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked credential should not resolve, got %v", err)
	}
	// Revoking someone else's endpoint / already revoked → ErrNotFound.
	if err := s.RevokeEndpoint(ctx, ep.ID, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke should be ErrNotFound, got %v", err)
	}
}

func TestTodoQueueLifecycle(t *testing.T) {
	s, ctx := testStore(t)

	td, created, err := s.CreateTodo(ctx, CreateTodoParams{
		Queue: "reviews", Source: "github", Kind: "pull_request", Title: "PR #482 opened",
		IdempotencyKey: "gh-482",
	})
	if err != nil || !created {
		t.Fatalf("create todo: created=%v err=%v", created, err)
	}
	if td.State != "pending" {
		t.Fatalf("new todo state=%s want pending", td.State)
	}

	// Dedup: same idempotency key returns the existing todo, no new row.
	dup, created2, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "reviews", Title: "dup", IdempotencyKey: "gh-482"})
	if err != nil || created2 || dup.ID != td.ID {
		t.Fatalf("dedup failed: created=%v id=%s want %s err=%v", created2, dup.ID, td.ID, err)
	}

	// Claim.
	claimed, err := s.ClaimTodo(ctx, td.ID, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.State != "claimed" || claimed.Owner != "worker-1" || claimed.Attempt != 1 || claimed.LeaseExpiresAt == nil {
		t.Fatalf("claimed wrong: %+v", claimed)
	}

	// Second claim loses the race → conflict.
	if _, err := s.ClaimTodo(ctx, td.ID, "worker-2", 30*time.Second); !errors.Is(err, ErrConflict) {
		t.Fatalf("double claim should conflict, got %v", err)
	}

	// Complete by owner.
	done, err := s.CompleteTodo(ctx, td.ID, "worker-1", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.State != "done" || done.CompletedAt == nil {
		t.Fatalf("completed wrong: %+v", done)
	}

	// Completing again → conflict (no longer claimed).
	if _, err := s.CompleteTodo(ctx, td.ID, "worker-1", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-complete should conflict, got %v", err)
	}
	// Unknown id → not found.
	if _, err := s.ClaimTodo(ctx, "td_nope", "w", time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim unknown should be ErrNotFound, got %v", err)
	}
}

func TestClaimNextSkipLocked(t *testing.T) {
	s, ctx := testStore(t)
	for i := 0; i < 3; i++ {
		if _, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got := 0
	for {
		td, err := s.ClaimNext(ctx, []string{"q"}, "w", time.Minute)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("claim next: %v", err)
		}
		if td.State != "claimed" {
			t.Fatalf("claim next state=%s", td.State)
		}
		got++
	}
	if got != 3 {
		t.Fatalf("claimed %d, want 3", got)
	}
}
