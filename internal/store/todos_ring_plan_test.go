// Ring caps under a hostile query plan.
//
// A LIMIT ... FOR UPDATE SKIP LOCKED pick nested in `WHERE id IN (...)` is capped only while the
// planner evaluates it once. Given a Nested Loop Semi Join with the pick on the inner side, the
// executor rescans the pick for every outer row, SKIP LOCKED drops the rows this same UPDATE has
// already changed, and each rescan's LIMIT reaches further down the backlog — RingOnAttach rang all
// five rows of TestRingOnAttachIsCapped in CI (the ci job for PR 492, 2026-09-26) because an
// autovacuum ANALYZE of the shared test database happened to tip the plan. These tests pin that plan
// on purpose instead of waiting for the planner to choose it. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0002 (SKIP LOCKED claiming); SPEC-0011.
package store

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/db"
)

// rescanPlanStore opens a second Store on s's database whose sessions cannot hash, merge, sort or
// materialize, which leaves the planner a Nested Loop Semi Join that rescans an IN-subquery once
// per outer row — the plan a small, freshly analyzed todos table can get on its own.
func rescanPlanStore(t *testing.T, s *Store, ctx context.Context) *Store {
	t.Helper()
	u, err := url.Parse(s.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse store dsn: %v", err)
	}
	q := u.Query()
	for _, gate := range []string{"enable_hashjoin", "enable_mergejoin", "enable_hashagg", "enable_sort", "enable_material"} {
		q.Set(gate, "off") // pgx sends unknown DSN keys as startup parameters
	}
	u.RawQuery = q.Encode()
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect (rescan plan): %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

func TestRingOnAttachCapSurvivesARescanPlan(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "attach-rescan", "forge")
	for i := 0; i < attachRingLimit+2; i++ {
		seedRingingTodo(t, s, ctx, ep, fmt.Sprintf("row-%d", i), true, "signed")
	}
	got, err := rescanPlanStore(t, s, ctx).RingOnAttach(ctx, ep, []string{"forge"})
	if err != nil {
		t.Fatalf("ring on attach: %v", err)
	}
	if len(got) != attachRingLimit {
		t.Fatalf("rung %d rows under a rescan plan, want the cap of %d", len(got), attachRingLimit)
	}
	assertRungRows(t, s, ctx, attachRingLimit)
}

func TestRingUnclaimedCapSurvivesARescanPlan(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring-rescan", "forge")
	long := 24 * time.Hour
	for i := 0; i < ringSweepLimit+2; i++ {
		td := seedRingingTodo(t, s, ctx, ep, fmt.Sprintf("backlog-%d", i), true, "signed")
		age(t, s, ctx, td.ID, 48*time.Hour, 1, &long)
	}
	got, err := rescanPlanStore(t, s, ctx).RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != ringSweepLimit {
		t.Fatalf("swept %d todos under a rescan plan, want the cap of %d", len(got), ringSweepLimit)
	}
}

// assertRungRows checks the ledger, not the returned slice: a rescan charges ring_attempts on every
// row it touches, so the cap has to hold in the table too.
func assertRungRows(t *testing.T, s *Store, ctx context.Context, want int) {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos WHERE last_ringed_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count rung rows: %v", err)
	}
	if n != want {
		t.Fatalf("%d rows charged a ring, want %d", n, want)
	}
}
