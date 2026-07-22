package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/db"
)

// testStore connects to a store-package-OWNED database derived from SWITCHBOARD_TEST_DATABASE_URL
// (created on first use), migrates, and truncates. `go test ./...` runs packages in parallel against
// the same test DSN, so truncating the shared database here would race the ingest/server/db
// packages' tests; a dedicated database (switchboard_test_store) isolates them fully. Tests skip
// cleanly when no test DB is configured, so `go test ./...` stays green without Postgres.
// Governing: issue #133 (per-package DB isolation for concurrent TRUNCATE).
func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run store tests")
	}
	ctx := context.Background()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	const testDB = "switchboard_test_store"
	if u.Path != "/"+testDB {
		admin, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect (admin): %v", err)
		}
		// CREATE DATABASE has no IF NOT EXISTS; a duplicate from an earlier run is fine. Tests
		// within one package run sequentially, so no concurrent CREATE races this.
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+testDB); err != nil &&
			!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
			admin.Close()
			t.Fatalf("create store test database: %v", err)
		}
		admin.Close()
		u.Path = "/" + testDB
	}
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, personas, friend_edges, todos, events, sessions, adapters, oauth_clients RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return New(pool), ctx
}

// seedEndpointCounter keeps the per-call credential hash and OIDC subject distinct across every
// seedEndpoint in the package: humans are unique on oidc_subject and endpoints on credential_hash,
// so two fixtures built from the same label would otherwise collide inside one test database.
var seedEndpointCounter atomic.Int64

// seedEndpoint provisions the full ownership chain a todo now requires — human → agent → vended
// endpoint — and returns the endpoint id that the calling test's todos are pinned to.
// todos.endpoint_id is NOT NULL and every agent-facing query predicates on it, so a test that
// enqueues work must name a real endpoint to own it: there is no sentinel scope and no system-todo
// escape hatch. Each call mints a distinct human, agent, and credential, so a test can seed two
// endpoints and assert that neither can see the other's work even when their queue names collide.
// Governing: ADR-0022, ADR-0008, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func seedEndpoint(t *testing.T, s *Store, ctx context.Context, label string, queues ...string) string {
	t.Helper()
	if len(queues) == 0 {
		queues = []string{"q"}
	}
	n := seedEndpointCounter.Add(1)
	uniq := fmt.Sprintf("%s-%d", label, n)

	h, err := s.UpsertHuman(ctx, "pocket|"+uniq, label, uniq+"@example.com")
	if err != nil {
		t.Fatalf("seed human (%s): %v", label, err)
	}
	ag, err := s.CreateAgent(ctx, h.ID, label, "")
	if err != nil {
		t.Fatalf("seed agent (%s): %v", label, err)
	}
	slug, err := MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("seed slug (%s): %v", label, err)
	}
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-"+uniq, "sbk_"+uniq, slug, queues,
		[]string{"list_todos", "claim", "complete", "fail", "heartbeat", "release", "retry"})
	if err != nil {
		t.Fatalf("seed endpoint (%s): %v", label, err)
	}
	return ep.ID
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

	// Successful auth stamps last-seen (SPEC-0014) and fires the endpoint-seen hook after the
	// commit (SPEC-0013 endpoint_seen typed event).
	var hookID string
	var hookSeen time.Time
	s.SetEndpointSeenHook(func(id string, seenAt time.Time) { hookID, hookSeen = id, seenAt })
	if err := s.TouchEndpoint(ctx, ep.ID); err != nil {
		t.Fatalf("touch endpoint: %v", err)
	}
	s.SetEndpointSeenHook(nil)
	if hookID != ep.ID || hookSeen.IsZero() {
		t.Fatalf("endpoint-seen hook: id=%q seen=%v", hookID, hookSeen)
	}
	eps, err := s.ListEndpoints(ctx, ag.ID)
	if err != nil || len(eps) != 1 {
		t.Fatalf("list endpoints: %v (%d)", err, len(eps))
	}
	if eps[0].LastSeenAt == nil {
		t.Fatal("touched endpoint should carry last_seen_at")
	}
	// Touching a nonexistent endpoint is a silent no-op (nothing to stamp, nothing to announce).
	if err := s.TouchEndpoint(ctx, "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("touch missing endpoint: %v", err)
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

// Server-side session lifecycle against real SQL: only the token hash is stored, lookup admits only
// live (unexpired) sessions via `expires_at > now()`, and deletion revokes server-side.
// Governing: SPEC-0008 REQ "Server-Side Session Establishment", REQ "Session-Gated Human Surface".
func TestSessionLifecycle(t *testing.T) {
	s, ctx := testStore(t)

	h, err := s.UpsertHuman(ctx, "pocket|sess", "Sess Human", "sess@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}

	// A live session resolves to its human.
	if err := s.CreateSession(ctx, "livehash", h.ID, time.Hour); err != nil {
		t.Fatalf("create live session: %v", err)
	}
	got, err := s.SessionHuman(ctx, "livehash")
	if err != nil || got.ID != h.ID {
		t.Fatalf("live session must resolve to its human: %+v, %v", got, err)
	}

	// An expired session is not honored.
	if err := s.CreateSession(ctx, "expiredhash", h.ID, -time.Minute); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if _, err := s.SessionHuman(ctx, "expiredhash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session must be ErrNotFound, got %v", err)
	}

	// An unknown token hash does not resolve.
	if _, err := s.SessionHuman(ctx, "nosuchhash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session must be ErrNotFound, got %v", err)
	}

	// Logout deletes the record server-side; the prior hash no longer authenticates.
	if err := s.DeleteSession(ctx, "livehash"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.SessionHuman(ctx, "livehash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session must be ErrNotFound, got %v", err)
	}
}

func TestTodoQueueLifecycle(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "lifecycle", "reviews")

	td, created, err := s.CreateTodo(ctx, CreateTodoParams{
		EndpointID: ep,
		Queue:      "reviews", Source: "github", Kind: "pull_request", Title: "PR #482 opened",
		IdempotencyKey: "gh-482",
	})
	if err != nil || !created {
		t.Fatalf("create todo: created=%v err=%v", created, err)
	}
	if td.State != "pending" {
		t.Fatalf("new todo state=%s want pending", td.State)
	}

	// Dedup: same idempotency key returns the existing todo, no new row.
	dup, created2, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "reviews", Title: "dup", IdempotencyKey: "gh-482"})
	if err != nil || created2 || dup.ID != td.ID {
		t.Fatalf("dedup failed: created=%v id=%s want %s err=%v", created2, dup.ID, td.ID, err)
	}

	// Dedup is per-endpoint: a second endpoint reusing the same key on the same queue name gets its
	// OWN row, not a handle on this tenant's todo. Governing: ADR-0022, SPEC-0003 REQ
	// "Per-Endpoint Idempotency and Dedup".
	other := seedEndpoint(t, s, ctx, "lifecycle-other", "reviews")
	otherTd, createdOther, err := s.CreateTodo(ctx, CreateTodoParams{
		EndpointID: other, Queue: "reviews", Title: "same key, other tenant", IdempotencyKey: "gh-482"})
	if err != nil || !createdOther {
		t.Fatalf("cross-endpoint same idempotency key must mint a new row: created=%v err=%v", createdOther, err)
	}
	if otherTd.ID == td.ID {
		t.Fatalf("cross-endpoint dedup collided onto %s — keys must be scoped per endpoint", td.ID)
	}

	// Claim.
	claimed, err := s.ClaimTodo(ctx, ep, td.ID, "worker-1", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.State != "claimed" || claimed.Owner != "worker-1" || claimed.Attempt != 1 || claimed.LeaseExpiresAt == nil {
		t.Fatalf("claimed wrong: %+v", claimed)
	}

	// Second claim loses the race → conflict.
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "worker-2", 30*time.Second); !errors.Is(err, ErrConflict) {
		t.Fatalf("double claim should conflict, got %v", err)
	}

	// Another endpoint cannot claim this tenant's todo at all — it is not merely a lease conflict,
	// the row is invisible outside its owning scope. Governing: ADR-0022.
	if _, err := s.ClaimTodo(ctx, other, td.ID, "intruder", 30*time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-endpoint claim should be ErrNotFound, got %v", err)
	}

	// Complete by owner.
	done, err := s.CompleteTodo(ctx, ep, td.ID, "worker-1", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.State != "done" || done.CompletedAt == nil {
		t.Fatalf("completed wrong: %+v", done)
	}

	// Completing again → conflict (no longer claimed).
	if _, err := s.CompleteTodo(ctx, ep, td.ID, "worker-1", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-complete should conflict, got %v", err)
	}
	// Unknown id → not found.
	if _, err := s.ClaimTodo(ctx, ep, "td_nope", "w", time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim unknown should be ErrNotFound, got %v", err)
	}
}

func TestClaimNextSkipLocked(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "skiplocked", "q")
	for i := 0; i < 3; i++ {
		if _, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A second endpoint on the SAME queue name seeds work this scan must never reach: the drain
	// below must stop at exactly 3. Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership".
	noise := seedEndpoint(t, s, ctx, "skiplocked-noise", "q")
	if _, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: noise, Queue: "q", Title: "not yours"}); err != nil {
		t.Fatalf("seed noise: %v", err)
	}
	got := 0
	for {
		td, err := s.ClaimNext(ctx, ep, []string{"q"}, "w", time.Minute)
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
