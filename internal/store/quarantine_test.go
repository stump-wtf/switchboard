package store

// DB-backed quarantine behaviour (ADR-0031, SPEC-0026 REQ-6, REQ-7, REQ-13): a held delivery is one
// todo on the owner endpoint, and a redelivery collapses onto it. Release moves it in place (same id)
// and fans out, discard records who and why, and a concurrent release and discard resolve exactly
// once. Expiry honours 30 days or the shorter retention bound, and the reserved name is refused
// wherever a queue enters.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var heldSeq atomic.Int64

// holdOn quarantines a fresh verified delivery on ep and returns the held todo and its key.
func holdOn(t *testing.T, s *Store, ctx context.Context, ep string) (Todo, string) {
	t.Helper()
	key := fmt.Sprintf("hold-%d", heldSeq.Add(1))
	_, out, disp, err := s.CreateIntakeEventTodos(ctx, EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed",
		Verified: true, Payload: []byte(`{"n":1}`), Disposition: DispositionQuarantined,
	}, []string{ep}, CreateTodoParams{Source: "github", Kind: "webhook", Title: "held " + key, Payload: []byte(`{"n":1}`),
		IdempotencyKey: key, QuarantineReason: "untrusted_actor", QuarantineDetail: []byte(`{"actor":{"sender":"mallory"}}`)})
	if err != nil || len(out) != 1 || disp != DispositionQuarantined {
		t.Fatalf("hold: %v (%d todos, %s)", err, len(out), disp)
	}
	return out[0].Todo, key
}

func TestQuarantineIntakeIsOneOwnerTodoAndRedeliveryCollapses(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "held-owner", "q")
	friend := seedEndpoint(t, s, ctx, "held-friend", "q")
	var rang int
	s.SetTodoDoorbellHook(func(Todo) { rang++ })
	t.Cleanup(func() { s.SetTodoDoorbellHook(nil) })

	held, key := holdOn(t, s, ctx, owner)
	if held.Queue != QueueQuarantine || held.EndpointID != owner || held.QuarantineReason != "untrusted_actor" ||
		string(held.QuarantineDetail) == "" || held.State != "pending" {
		t.Fatalf("held = %+v, want one pending quarantine todo on the owner", held)
	}
	// A redelivery routed to the owner AND a friend (the rules changed) collapses onto the held item
	// and mints nothing on either.
	_, out, disp, err := s.CreateIntakeEventTodos(ctx, EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed", Verified: true,
	}, []string{owner, friend}, CreateTodoParams{Queue: "q", Title: "again", IdempotencyKey: key})
	if err != nil || len(out) != 1 || out[0].New || out[0].Todo.ID != held.ID || disp != DispositionQuarantined {
		t.Fatalf("redelivery = %+v (%s, %v), want the held item back", out, disp, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos WHERE idempotency_key = $1`, key).Scan(&n); err != nil || n != 1 {
		t.Fatalf("todos for the delivery = %d (%v), want 1", n, err)
	}
	if rang != 0 {
		t.Fatalf("doorbells = %d, want none for a held delivery", rang)
	}
	// Quarantine needs exactly the owner as its target.
	if _, _, _, err := s.CreateIntakeEventTodos(ctx, EventInput{Source: "github", Family: "webhook", ExternalID: key + "-x",
		TrustMode: "signed", Disposition: DispositionQuarantined}, []string{owner, friend},
		CreateTodoParams{Title: "x", IdempotencyKey: key + "-x", QuarantineReason: "rule_action"}); err == nil {
		t.Fatal("a quarantine fanned out to two targets")
	}
}

// A delivery first recorded as dropped or faulted (with no quarantine item: before the queue
// existed) stays withheld when it is redelivered and would now be held: its dedup slot is spent.
// Governing: SPEC-0020 REQ "Drop Action Semantics"; SPEC-0026 REQ-1, REQ-6.
func TestWithheldRedeliveryIsNeverQuarantined(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "withheld-owner", "q")
	for _, tc := range []struct{ first, again, reason string }{
		{DispositionDropped, DispositionFaulted, "rule_fault"},
		{DispositionDropped, DispositionQuarantined, "untrusted_actor"},
		{DispositionFaulted, DispositionFaulted, "rule_fault"},
		{DispositionFaulted, DispositionQuarantined, "rule_action"},
	} {
		key := fmt.Sprintf("withheld-%d", heldSeq.Add(1))
		ev := EventInput{Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed",
			Verified: true, Payload: []byte(`{"n":1}`), Disposition: tc.first}
		if _, out, disp, err := s.CreateIntakeEventTodos(ctx, ev, nil, CreateTodoParams{}); err != nil || len(out) != 0 || disp != tc.first {
			t.Fatalf("%s: first = %d todos, %s, %v", tc.first, len(out), disp, err)
		}
		ev.Disposition = tc.again
		_, out, disp, err := s.CreateIntakeEventTodos(ctx, ev, []string{owner}, CreateTodoParams{Source: "github", Kind: "webhook",
			Title: "again", Payload: []byte(`{"n":1}`), IdempotencyKey: key, QuarantineReason: tc.reason})
		if err != nil || len(out) != 0 || disp != tc.first {
			t.Fatalf("%s then %s: %d todos, %s, %v; want withheld as %s", tc.first, tc.again, len(out), disp, err, tc.first)
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos WHERE idempotency_key = $1`, key).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s then %s: %d todos in the table (%v), want none", tc.first, tc.again, n, err)
		}
	}
}

func TestReleaseMovesInPlaceAndFansOut(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "rel-owner", "q", "lane-m")
	friend := seedEndpoint(t, s, ctx, "rel-friend", "lane-m")
	human := ownerOf(t, s, ctx, owner)
	var rang []string
	s.SetTodoDoorbellHook(func(t Todo) { rang = append(rang, t.EndpointID) })
	t.Cleanup(func() { s.SetTodoDoorbellHook(nil) })

	held, _ := holdOn(t, s, ctx, owner)
	item, err := s.QuarantinedForHuman(ctx, human, held.ID)
	if err != nil || item.Todo.ID != held.ID || string(item.Event.Payload) != `{"n":1}` {
		t.Fatalf("quarantined item = %+v (%v)", item.Todo, err)
	}
	if _, err := s.QuarantinedForHuman(ctx, ownerOf(t, s, ctx, friend), held.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign human read the item: %v", err)
	}

	plan := ReleasePlan{TodoID: held.ID, OwnerHumanID: human, By: "human:" + human, Queue: "lane-m",
		Endpoints: []string{owner, friend}, Trace: []byte(`{"stage":"rule","rule_id":"r"}`),
		WorkOrder: []byte(`{"released_by":"human:x"}`), Verified: true, TrustMode: "signed"}
	out, err := s.ApplyQuarantineRelease(ctx, plan)
	if err != nil || len(out) != 2 {
		t.Fatalf("release = %+v (%v), want the moved todo and one fan-out", out, err)
	}
	moved := out[0].Todo
	if moved.ID != held.ID || moved.Queue != "lane-m" || moved.EndpointID != owner || moved.ReleasedBy != "human:"+human ||
		moved.ReleasedAt == nil || moved.QuarantineReason != "untrusted_actor" || string(moved.WorkOrder) == "" {
		t.Fatalf("moved = %+v, want the same id on lane-m, with the release recorded", moved)
	}
	if fan := out[1].Todo; fan.EndpointID != friend || fan.Queue != "lane-m" || fan.EventID == nil || *fan.EventID != *held.EventID {
		t.Fatalf("fan-out = %+v", fan)
	}
	if len(rang) != 2 {
		t.Fatalf("doorbells after release = %v, want both targets rung", rang)
	}
	// Now agents see it: the released todo is ordinary work on its queue.
	if got, err := s.GetTodo(ctx, owner, held.ID); err != nil || got.Queue != "lane-m" {
		t.Fatalf("GetTodo after release = %+v (%v)", got, err)
	}
	// It is no longer held: a second release or a discard conflicts.
	if _, err := s.ApplyQuarantineRelease(ctx, plan); !errors.Is(err, ErrConflict) {
		t.Fatalf("second release = %v, want conflict", err)
	}
	if _, err := s.DiscardQuarantined(ctx, human, held.ID, "human:"+human, "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("discard after release = %v, want conflict", err)
	}
	if _, err := s.ApplyQuarantineRelease(ctx, ReleasePlan{TodoID: "td_nope", OwnerHumanID: human, Queue: "q",
		Endpoints: []string{owner}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release of an unknown id = %v, want not found", err)
	}
	if _, err := s.ApplyQuarantineRelease(ctx, ReleasePlan{TodoID: held.ID, OwnerHumanID: human, Queue: QueueQuarantine,
		Endpoints: []string{owner}}); err == nil {
		t.Fatal("a release back onto quarantine was applied")
	}
}

func TestDiscardRecordsWhoAndWhy(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "disc-owner", "q")
	stranger := seedEndpoint(t, s, ctx, "disc-stranger", "q")
	human := ownerOf(t, s, ctx, owner)
	held, _ := holdOn(t, s, ctx, owner)

	if _, err := s.DiscardQuarantined(ctx, ownerOf(t, s, ctx, stranger), held.ID, "human:x", "spam"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign discard = %v, want not found", err)
	}
	done, err := s.DiscardQuarantined(ctx, human, held.ID, "human:"+human, "spam")
	if err != nil || done.State != "done" || done.CompletedAt == nil {
		t.Fatalf("discard = %+v (%v)", done, err)
	}
	var result map[string]any
	if err := json.Unmarshal(done.Result, &result); err != nil || result["discarded"] != true ||
		result["reason"] != "spam" || result["by"] != "human:"+human || result["at"] == nil {
		t.Fatalf("discard result = %s (%v), want discarded, reason, by and at", done.Result, err)
	}
	if _, err := s.DiscardQuarantined(ctx, human, held.ID, "human:"+human, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second discard = %v, want conflict", err)
	}
}

// SPEC-0026 REQ-13 scenario "Concurrent release and discard": exactly one succeeds, the other gets
// conflict, and the todo records one outcome. Run under -race in CI.
func TestConcurrentReleaseAndDiscardResolveOnce(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "race-owner", "q")
	human := ownerOf(t, s, ctx, owner)
	for i := 0; i < 20; i++ {
		held, _ := holdOn(t, s, ctx, owner)
		var wg sync.WaitGroup
		var relErr, disErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, relErr = s.ApplyQuarantineRelease(ctx, ReleasePlan{TodoID: held.ID, OwnerHumanID: human, By: "human:h",
				Queue: "q", Endpoints: []string{owner}, Trace: []byte(`{}`)})
		}()
		go func() {
			defer wg.Done()
			_, disErr = s.DiscardQuarantined(ctx, human, held.ID, "classifier:c", "noise")
		}()
		wg.Wait()
		switch {
		case relErr == nil && errors.Is(disErr, ErrConflict):
		case disErr == nil && errors.Is(relErr, ErrConflict):
		default:
			t.Fatalf("round %d: release %v, discard %v; want exactly one success and one conflict", i, relErr, disErr)
		}
		var queue, state string
		var released, discarded bool
		if err := s.pool.QueryRow(ctx, `SELECT queue, state, released_by IS NOT NULL, COALESCE((result->>'discarded')::boolean, false)
			FROM todos WHERE id = $1`, held.ID).Scan(&queue, &state, &released, &discarded); err != nil {
			t.Fatalf("read outcome: %v", err)
		}
		if released == discarded || (released && (queue != "q" || state != "pending")) || (discarded && (queue != QueueQuarantine || state != "done")) {
			t.Fatalf("round %d: row %s/%s released=%v discarded=%v, want exactly one outcome", i, queue, state, released, discarded)
		}
	}
}

func TestExpireQuarantine(t *testing.T) {
	s, ctx := testStore(t)
	owner := seedEndpoint(t, s, ctx, "exp-owner", "q")
	old, _ := holdOn(t, s, ctx, owner)
	mid, _ := holdOn(t, s, ctx, owner)
	fresh, _ := holdOn(t, s, ctx, owner)
	age := func(id, interval string) {
		if _, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - $2::interval WHERE id = $1`, id, interval); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}
	age(old.ID, "31 days")
	age(mid.ID, "10 days")
	age(fresh.ID, "3 days")
	// The settings table is shared by the package's tests: pin the retention bound for this test and
	// put back whatever was there.
	var prior *string
	_ = s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'retention_max_age_days'`).Scan(&prior)
	setRetention := func(days string) {
		if _, err := s.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('retention_max_age_days', $1)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, days); err != nil {
			t.Fatalf("set retention: %v", err)
		}
	}
	t.Cleanup(func() {
		if prior != nil {
			setRetention(*prior)
			return
		}
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM settings WHERE key = 'retention_max_age_days'`)
	})
	setRetention("90") // longer than 30 days: the 30-day quarantine bound applies

	state := func(id string) (string, map[string]any) {
		var st string
		var raw []byte
		if err := s.pool.QueryRow(ctx, `SELECT state, result FROM todos WHERE id = $1`, id).Scan(&st, &raw); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		var r map[string]any
		_ = json.Unmarshal(raw, &r)
		return st, r
	}

	if _, err := s.ExpireQuarantine(ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if st, r := state(old.ID); st != "done" || r["expired"] != true || r["by"] != "system" {
		t.Fatalf("31-day item = %s %v, want expired by system", st, r)
	}
	if st, _ := state(mid.ID); st != "pending" {
		t.Fatalf("10-day item = %s, want still held under the 30-day bound", st)
	}
	// A shorter operator retention bound expires it sooner.
	setRetention("7")
	if _, err := s.ExpireQuarantine(ctx); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if st, r := state(mid.ID); st != "done" || r["expired"] != true {
		t.Fatalf("10-day item under a 7-day bound = %s %v, want expired", st, r)
	}
	if st, _ := state(fresh.ID); st != "pending" {
		t.Fatalf("3-day item = %s, want still held", st)
	}
}

func TestReservedQuarantineQueueRefused(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "reserved", "q")
	if _, err := s.CreateWebhook(ctx, ep, "generic", QueueQuarantine, "token", "tok-reserved", "", 5); !errors.Is(err, ErrReservedQueue) {
		t.Fatalf("webhook target quarantine = %v, want ErrReservedQueue", err)
	}
	h, err := s.UpsertHuman(ctx, "pocket|reserved-h", "R", "reserved@example.com")
	if err != nil {
		t.Fatalf("human: %v", err)
	}
	ag, err := s.CreateAgent(ctx, h.ID, "reserved-agent", "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if _, err := s.CreateEndpoint(ctx, ag.ID, "hash-reserved", "sbk_reserved", "reserved-aaaa", []string{QueueQuarantine}, nil); !errors.Is(err, ErrReservedQueue) {
		t.Fatalf("scope quarantine = %v, want ErrReservedQueue", err)
	}
	if _, err := s.VendAgentEndpoint(ctx, VendParams{OwnerHumanID: h.ID, Name: "reserved-vend", CredHash: "hash-rv",
		CredPrefix: "sbk_rv", Slug: "reserved-vend-aaaa", Queues: []string{"q"}, WebhookMax: 1,
		WebhookSourceTypes: []string{"generic"}, WebhookQueues: []string{QueueQuarantine}}); !errors.Is(err, ErrReservedQueue) {
		t.Fatalf("webhook ceiling quarantine = %v, want ErrReservedQueue", err)
	}
}
