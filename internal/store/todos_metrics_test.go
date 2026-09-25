package store

// Lifecycle counter tests
//
// The store increments the SPEC-0023 REQ-3 counters through its Metrics seam after each transition
// commits. These tests install a recording fake in place of *metrics.Metrics (which this package
// must not import) and drive the real SQL paths, so what they pin is which committed transitions
// count, and how often: new rows but not idempotency dedups, every claim with its attempt number,
// claimant outcomes, and a lapsed lease exactly once whether the reaper or a new claimant finds it.
//
// Governing: SPEC-0023 REQ-3 "Lifecycle counters", REQ-5 "Cardinality"; ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 3).

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"
)

// recMetrics records every lifecycle increment. Safe for concurrent use.
type recMetrics struct {
	mu       sync.Mutex
	created  map[string]int   // "queue/source"
	claims   map[string][]int // queue -> attempt numbers, in claim order
	finished map[string]int   // "queue/outcome"
	expired  map[string]int   // queue
	// SPEC-0026 REQ-11: held reasons, and "outcome/by" resolutions as the store recorded them.
	held     map[string]int
	resolved map[string]int
}

func newRecMetrics() *recMetrics {
	return &recMetrics{
		created: map[string]int{}, claims: map[string][]int{},
		finished: map[string]int{}, expired: map[string]int{},
		held: map[string]int{}, resolved: map[string]int{},
	}
}

func (r *recMetrics) TodoCreated(queue, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created[queue+"/"+source]++
}

func (r *recMetrics) TodoClaimed(queue string, attempt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims[queue] = append(r.claims[queue], attempt)
}

func (r *recMetrics) TodoFinished(queue, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished[queue+"/"+outcome]++
}

func (r *recMetrics) LeaseExpired(queue string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expired[queue]++
}

func (r *recMetrics) QuarantineHeld(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held[reason]++
}

func (r *recMetrics) QuarantineResolved(outcome, by string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved[outcome+"/"+by]++
}

// quarantineSnapshot returns copies of the quarantine maps.
func (r *recMetrics) quarantineSnapshot() (held, resolved map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.held), maps.Clone(r.resolved)
}

// snapshot returns a copy of every map, so assertions never race a late increment.
func (r *recMetrics) snapshot() (created map[string]int, claims map[string][]int, finished, expired map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	created, finished, expired = map[string]int{}, map[string]int{}, map[string]int{}
	claims = map[string][]int{}
	for k, v := range r.created {
		created[k] = v
	}
	for k, v := range r.claims {
		claims[k] = slices.Clone(v)
	}
	for k, v := range r.finished {
		finished[k] = v
	}
	for k, v := range r.expired {
		expired[k] = v
	}
	return created, claims, finished, expired
}

// meteredStore is testStore with a recording sink installed.
func meteredStore(t *testing.T) (*Store, context.Context, *recMetrics) {
	t.Helper()
	s, ctx := testStore(t)
	rec := newRecMetrics()
	s.SetMetrics(rec)
	return s, ctx, rec
}

// expireLease backdates a claimed todo's lease so it has lapsed, the way the existing lease tests
// do: deterministic, with no sleeping on a wall clock.
func expireLease(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease %s: %v", id, err)
	}
}

func mustCreate(t *testing.T, s *Store, ctx context.Context, ep, queue, key string) Todo {
	t.Helper()
	td, created, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: queue, Source: "dev", Title: key, IdempotencyKey: key})
	if err != nil || !created {
		t.Fatalf("create %s: created=%v err=%v", key, created, err)
	}
	return td
}

// The spec's lapsed-lease scenario, reaper path: the reaper counts one expiry, the re-claim lands
// in attempt bucket 2, and claims outrun completions.
func TestLifecycleMetricsLapsedLeaseReaped(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "metrics-reaped", "forge")

	td := mustCreate(t, s, ctx, ep, "forge", "reap-1")
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	expireLease(t, s, ctx, td.ID)
	if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap = %d, %v; want 1", n, err)
	}
	// A second sweep finds nothing and must count nothing.
	if n, err := s.ReapExpired(ctx); err != nil || n != 0 {
		t.Fatalf("second reap = %d, %v; want 0", n, err)
	}
	// The row is pending again, so this re-claim is NOT a takeover and must not count a second expiry.
	again, err := s.ClaimNext(ctx, ep, []string{"forge"}, "w2", time.Hour)
	if err != nil || again.ID != td.ID {
		t.Fatalf("re-claim = %v, %v; want %s", again.ID, err, td.ID)
	}
	if _, err := s.CompleteTodo(ctx, ep, td.ID, "w2", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	created, claims, finished, expired := rec.snapshot()
	if created["forge/dev"] != 1 {
		t.Errorf("created = %v, want forge/dev:1", created)
	}
	if expired["forge"] != 1 {
		t.Errorf("leases expired = %v, want forge:1 (one reap, no double count on re-claim)", expired)
	}
	if got := claims["forge"]; !slices.Equal(got, []int{1, 2}) {
		t.Errorf("claim attempts = %v, want [1 2] (the re-claim is attempt 2)", got)
	}
	if len(claims["forge"]) <= finished["forge/complete"] {
		t.Errorf("claims (%d) must exceed completions (%d) after a lapsed lease", len(claims["forge"]), finished["forge/complete"])
	}
	if finished["forge/complete"] != 1 || finished["forge/fail"] != 0 {
		t.Errorf("finished = %v, want forge/complete:1 only", finished)
	}
}

// Lease takeover, no reaper: a claimant that takes over a lapsed lease counts the expiry itself, on
// every claim path — ClaimTodo, ClaimNext and the Board's ClaimTodoOperatorOwned.
func TestLifecycleMetricsLeaseTakeover(t *testing.T) {
	cases := map[string]func(s *Store, ctx context.Context, ep, human, id string) (Todo, error){
		"ClaimTodo": func(s *Store, ctx context.Context, ep, _, id string) (Todo, error) {
			return s.ClaimTodo(ctx, ep, id, "w2", time.Hour)
		},
		"ClaimNext": func(s *Store, ctx context.Context, ep, _, _ string) (Todo, error) {
			return s.ClaimNext(ctx, ep, []string{"forge"}, "w2", time.Hour)
		},
		"ClaimTodoOperatorOwned": func(s *Store, ctx context.Context, _, human, id string) (Todo, error) {
			return s.ClaimTodoOperatorOwned(ctx, human, id, "op:w2", time.Hour)
		},
	}
	for name, takeOver := range cases {
		t.Run(name, func(t *testing.T) {
			s, ctx, rec := meteredStore(t)
			ep := seedEndpoint(t, s, ctx, "metrics-takeover", "forge")
			human := ownerOf(t, s, ctx, ep)
			td := mustCreate(t, s, ctx, ep, "forge", "takeover-"+name)
			if _, err := s.ClaimTodo(ctx, ep, td.ID, "w1", time.Hour); err != nil {
				t.Fatalf("first claim: %v", err)
			}

			// A live lease cannot be taken over, and a refused claim counts nothing.
			if _, err := takeOver(s, ctx, ep, human, td.ID); err == nil {
				t.Fatal("claim over a live lease succeeded")
			}
			if _, _, _, expired := rec.snapshot(); len(expired) != 0 {
				t.Fatalf("refused claim counted an expiry: %v", expired)
			}

			expireLease(t, s, ctx, td.ID)
			got, err := takeOver(s, ctx, ep, human, td.ID)
			if err != nil || got.ID != td.ID {
				t.Fatalf("takeover = %v, %v; want %s", got.ID, err, td.ID)
			}
			// The reaper runs after the takeover and must not count the same lapse again: the
			// lease is fresh now.
			if n, err := s.ReapExpired(ctx); err != nil || n != 0 {
				t.Fatalf("reap after takeover = %d, %v; want 0", n, err)
			}

			_, claims, _, expired := rec.snapshot()
			if expired["forge"] != 1 {
				t.Errorf("leases expired = %v, want forge:1", expired)
			}
			if got := claims["forge"]; !slices.Equal(got, []int{1, 2}) {
				t.Errorf("claim attempts = %v, want [1 2]", got)
			}
		})
	}
}

// Claiming a pending todo, or a due retry, is not a takeover.
func TestLifecycleMetricsOrdinaryClaimsCountNoExpiry(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "metrics-ordinary", "q")

	td := mustCreate(t, s, ctx, ep, "q", "ordinary-1")
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, td.ID, "w", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}
	// Make the scheduled retry due, then claim it straight from `failed`.
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id = $1`, td.ID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if _, err := s.ClaimNext(ctx, ep, []string{"q"}, "w", time.Hour); err != nil {
		t.Fatalf("claim due retry: %v", err)
	}

	_, claims, finished, expired := rec.snapshot()
	if len(expired) != 0 {
		t.Errorf("leases expired = %v, want none", expired)
	}
	if got := claims["q"]; !slices.Equal(got, []int{1, 2}) {
		t.Errorf("claim attempts = %v, want [1 2]", got)
	}
	if finished["q/fail"] != 1 {
		t.Errorf("finished = %v, want q/fail:1", finished)
	}
}

// Claimant outcomes on the agent paths and the operator twins, including a fail at the attempt cap
// (a claimant-reported dead-letter, which IS a fail outcome).
func TestLifecycleMetricsOutcomes(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "metrics-outcomes", "q")
	human := ownerOf(t, s, ctx, ep)

	claim := func(key string) Todo {
		t.Helper()
		td := mustCreate(t, s, ctx, ep, "q", key)
		if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
			t.Fatalf("claim %s: %v", key, err)
		}
		return td
	}

	if _, err := s.CompleteTodo(ctx, ep, claim("done-agent").ID, "w", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, claim("fail-agent").ID, "w", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}
	capped := claim("fail-cap")
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET attempt = max_attempts WHERE id = $1`, capped.ID); err != nil {
		t.Fatalf("exhaust: %v", err)
	}
	if dl, err := s.FailTodo(ctx, ep, capped.ID, "w", nil); err != nil || dl.NextRetryAt != nil {
		t.Fatalf("fail at cap = %+v, %v; want a dead-letter", dl.NextRetryAt, err)
	}

	opDone := mustCreate(t, s, ctx, ep, "q", "done-op")
	if _, err := s.ClaimTodoOperatorOwned(ctx, human, opDone.ID, "op", time.Hour); err != nil {
		t.Fatalf("operator claim: %v", err)
	}
	if _, err := s.CompleteTodoOperatorOwned(ctx, human, opDone.ID, "op", nil); err != nil {
		t.Fatalf("operator complete: %v", err)
	}
	opFail := mustCreate(t, s, ctx, ep, "q", "fail-op")
	if _, err := s.ClaimTodoOperatorOwned(ctx, human, opFail.ID, "op", time.Hour); err != nil {
		t.Fatalf("operator claim: %v", err)
	}
	if _, err := s.FailTodoOperatorOwned(ctx, human, opFail.ID, "op", nil); err != nil {
		t.Fatalf("operator fail: %v", err)
	}

	// A refused completion (wrong owner) counts nothing.
	stray := claim("stray")
	if _, err := s.CompleteTodo(ctx, ep, stray.ID, "someone-else", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-owner complete = %v, want ErrConflict", err)
	}

	_, claims, finished, _ := rec.snapshot()
	if finished["q/complete"] != 2 || finished["q/fail"] != 3 {
		t.Errorf("finished = %v, want q/complete:2 q/fail:3", finished)
	}
	if len(claims["q"]) != 6 {
		t.Errorf("claims = %v, want 6", claims["q"])
	}
}

// A reaper dead-letter is a lease expiry, not a fail outcome: nobody reported it (the documented
// no-double-count decision in ReapExpired).
func TestLifecycleMetricsReaperDeadLetterIsNotAFail(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "metrics-reap-dl", "q")

	td := mustCreate(t, s, ctx, ep, "q", "reap-dl")
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET attempt = max_attempts WHERE id = $1`, td.ID); err != nil {
		t.Fatalf("exhaust: %v", err)
	}
	expireLease(t, s, ctx, td.ID)
	if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap = %d, %v; want 1", n, err)
	}
	if got, _ := s.GetTodo(ctx, ep, td.ID); got.State != "failed" {
		t.Fatalf("state = %s, want a dead-letter", got.State)
	}

	_, _, finished, expired := rec.snapshot()
	if expired["q"] != 1 {
		t.Errorf("leases expired = %v, want q:1", expired)
	}
	if len(finished) != 0 {
		t.Errorf("finished = %v, want none (a reaper dead-letter is not a claimant outcome)", finished)
	}
}

// Only genuinely new rows count as creations, on every create path.
func TestLifecycleMetricsIdempotentDedupNotCounted(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	epA := seedEndpoint(t, s, ctx, "metrics-dedup-a", "q")
	epB := seedEndpoint(t, s, ctx, "metrics-dedup-b", "q")

	// CreateTodo: same key twice.
	mustCreate(t, s, ctx, epA, "q", "dup-plain")
	if _, created, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: epA, Queue: "q", Source: "dev", Title: "again", IdempotencyKey: "dup-plain"}); err != nil || created {
		t.Fatalf("dedup CreateTodo: created=%v err=%v", created, err)
	}

	// CreateEventTodo: a redelivery of the same event.
	ev := EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "dup-ev", TrustMode: "signed", Verified: true}
	p := CreateTodoParams{EndpointID: epA, Queue: "q", Source: "github", Kind: "push", Title: "ev", IdempotencyKey: "dup-ev"}
	for i := range 2 {
		if _, _, created, err := s.CreateEventTodo(ctx, ev, p); err != nil || created != (i == 0) {
			t.Fatalf("CreateEventTodo #%d: created=%v err=%v", i+1, created, err)
		}
	}

	// CreateRoutedEventTodos: fan-out to two endpoints, then a redelivery that mints nothing new.
	fan := EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: "dup-fan", TrustMode: "signed", Verified: true}
	fp := CreateTodoParams{Queue: "q", Source: "gitea", Kind: "webhook", Title: "fan", IdempotencyKey: "dup-fan"}
	for i := range 2 {
		_, out, dropped, err := s.CreateRoutedEventTodos(ctx, fan, false, []string{epA, epB}, fp)
		if err != nil || dropped || len(out) != 2 {
			t.Fatalf("fan-out #%d: %d todos, dropped=%v, err=%v", i+1, len(out), dropped, err)
		}
	}
	// A dropped delivery mints nothing and counts nothing.
	drop := EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: "dropped", TrustMode: "signed", Verified: true}
	if _, _, dropped, err := s.CreateRoutedEventTodos(ctx, drop, true, nil, fp); err != nil || !dropped {
		t.Fatalf("drop: dropped=%v err=%v", dropped, err)
	}

	created, _, _, _ := rec.snapshot()
	want := map[string]int{"q/dev": 1, "q/github": 1, "q/gitea": 2}
	if fmt.Sprint(created) != fmt.Sprint(want) {
		t.Errorf("created = %v, want %v", created, want)
	}
}

// The source label is a bounded origin. A friend handoff counts as "friend", never as the persona
// name its Source column stores, and any other unknown value reports as "__other__".
func TestLifecycleMetricsSourceLabels(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "metrics-source", "q")

	for i, src := range []string{"dev", "operator", "cairn", "Joe's Persona <joe@example.com>", ""} {
		if _, created, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Source: src, Title: "s",
			IdempotencyKey: fmt.Sprintf("src-%d", i)}); err != nil || !created {
			t.Fatalf("create source %q: created=%v err=%v", src, created, err)
		}
	}

	target := mustHuman(t, s, ctx, "pocket|metrics-friend", "Target")
	vendAgent := mustAgent(t, s, ctx, target.ID, "metrics-friend-agent")
	_, fep := mustApprovedFriend(t, s, ctx, target, vendAgent, []string{"reviews"}, []string{"create_for"})
	td, created, err := s.CreateForFriend(ctx, CreateForFriendParams{
		EndpointID: fep.ID, Queue: "reviews", Intent: "create_for", Title: "handoff", IdempotencyKey: "fh-1",
	})
	if err != nil || !created {
		t.Fatalf("create_for: created=%v err=%v", created, err)
	}
	if td.Source != "a@a" {
		t.Fatalf("stored source = %q: the metrics origin must not change what is persisted", td.Source)
	}

	got, _, _, _ := rec.snapshot()
	want := map[string]int{"q/dev": 1, "q/operator": 1, "q/cairn": 1, "q/__other__": 2, "reviews/friend": 1}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("created = %v, want %v", got, want)
	}
}

// todoMetricSource is the bound on its own: an origin override wins, and only allowlisted values
// pass through.
func TestTodoMetricSource(t *testing.T) {
	cases := []struct {
		p    CreateTodoParams
		want string
	}{
		{CreateTodoParams{Source: "github"}, "github"},
		{CreateTodoParams{Source: "generic"}, "generic"},
		{CreateTodoParams{Source: "GitHub"}, "__other__"},
		{CreateTodoParams{Source: "td_0f3c"}, "__other__"},
		{CreateTodoParams{Source: ""}, "__other__"},
		{CreateTodoParams{Source: "github", origin: "friend"}, "friend"},
		{CreateTodoParams{Source: "dev", origin: "bogus"}, "__other__"},
	}
	for _, c := range cases {
		if got := todoMetricSource(c.p); got != c.want {
			t.Errorf("todoMetricSource(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

// Concurrent claimers of one lapsed lease: exactly one wins the takeover, and the expiry is counted
// exactly once alongside a racing reaper. Run under -race.
func TestLifecycleMetricsConcurrentTakeoverCountsOnce(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "metrics-race", "q")

	td := mustCreate(t, s, ctx, ep, "q", "race-1")
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w0", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	expireLease(t, s, ctx, td.ID)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%4 == 0 {
				_, _ = s.ReapExpired(ctx)
				return
			}
			_, _ = s.ClaimTodo(ctx, ep, td.ID, fmt.Sprintf("w%d", i), time.Hour)
		}()
	}
	wg.Wait()

	_, _, _, expired := rec.snapshot()
	if expired["q"] != 1 {
		t.Errorf("leases expired = %v, want exactly q:1 across racing claimers and reapers", expired)
	}
}
