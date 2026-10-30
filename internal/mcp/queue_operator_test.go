package mcp

// Operator queue supervision over MCP
//
// The half of this surface that lives in the MCP layer: the supervision verbs are never implied by a
// consumer grant and never in the default grant (so a worker endpoint cannot see or call them, which
// is the report's load-bearing constraint), a queue outside the grant is refused before the store is
// touched, the stalled-versus-quiet question is answered in one call, a drain returns when the last
// lease lands, a requeue refuses a row that is not failed, a release names the holder it is taking
// the row from, and every state-changing call leaves exactly one audit line that carries no
// credential.
//
// The store's own SQL (per-queue liveness, held leases) is proven against Postgres in
// internal/store/queue_supervise_test.go.
//
// Governing: ADR-0056, ADR-0048, SPEC-0006 REQ "Scope Enforcement at the Boundary", SPEC-0014 REQ
// "Agent Tool Surface over MCP".
//
// @joestump-agent 10/30/2026 - Added for stump.wtf/switchboard#582.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

// supervisionFake is the operator seam over the ordinary fake: it records what the verbs asked the
// store to do, answers the lease reads from a scripted sequence (so a drain can be shown waiting for
// a lease to land), and refuses whatever the real store would refuse.
type supervisionFake struct {
	*fakeStore

	mu         sync.Mutex
	liveness   map[string]store.QueueLiveness
	leaseCalls int
	leaseSeq   [][]store.HeldLease
	pauses     map[string]string
	resumes    []string
	items      map[string]store.Todo
	retries    []string
	releases   map[string]string
	humans     []string
}

func newSupervisionFake() *supervisionFake {
	return &supervisionFake{
		fakeStore: newFakeStore(),
		liveness:  map[string]store.QueueLiveness{},
		pauses:    map[string]string{},
		items:     map[string]store.Todo{},
		releases:  map[string]string{},
	}
}

// nextLeases scripts what the kth lease read returns; the last entry repeats once the script ends.
func (f *supervisionFake) nextLeases(batches ...[]store.HeldLease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseSeq = batches
}

func (f *supervisionFake) leasesRead() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaseCalls
}

func (f *supervisionFake) pausedReason(queue string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.pauses[queue]
	return r, ok
}

func (f *supervisionFake) retried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.retries...)
}

func (f *supervisionFake) releasedOwners() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.releases {
		out[k] = v
	}
	return out
}

func (f *supervisionFake) seenHumans() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.humans...)
}

func (f *supervisionFake) QueueLivenessForHuman(_ context.Context, human string, queues []string) ([]store.QueueLiveness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.humans = append(f.humans, human)
	var out []store.QueueLiveness
	for _, q := range queues {
		if row, ok := f.liveness[q]; ok {
			out = append(out, row)
		} else {
			out = append(out, store.QueueLiveness{Queue: q})
		}
	}
	return out, nil
}

func (f *supervisionFake) OutstandingLeasesForHuman(_ context.Context, human string, _ []string, _ int) ([]store.HeldLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.humans = append(f.humans, human)
	i := f.leaseCalls
	f.leaseCalls++
	if len(f.leaseSeq) == 0 {
		return nil, nil
	}
	if i >= len(f.leaseSeq) {
		i = len(f.leaseSeq) - 1
	}
	return f.leaseSeq[i], nil
}

func (f *supervisionFake) PauseQueue(_ context.Context, queue, _, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses[queue] = reason
	return nil
}

func (f *supervisionFake) ResumeQueue(_ context.Context, queue string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes = append(f.resumes, queue)
	if _, ok := f.pauses[queue]; !ok {
		return false, nil
	}
	delete(f.pauses, queue)
	return true, nil
}

func (f *supervisionFake) GetTodoItem(_ context.Context, human, id string) (store.TodoItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.humans = append(f.humans, human)
	item, ok := f.items[id]
	if !ok {
		return store.TodoItem{}, store.ErrNotFound
	}
	return store.TodoItem{Todo: item}, nil
}

func (f *supervisionFake) RetryTodoOperatorOwned(_ context.Context, _, id string) (store.Todo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[id]
	if !ok {
		return store.Todo{}, store.ErrNotFound
	}
	if item.State != "failed" {
		return store.Todo{}, store.ErrConflict
	}
	item.State = "pending"
	item.Attempt = 0
	f.items[id] = item
	f.retries = append(f.retries, id)
	return item, nil
}

func (f *supervisionFake) ReleaseTodoOperatorOwned(_ context.Context, _, id, owner string) (store.Todo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[id]
	if !ok {
		return store.Todo{}, store.ErrNotFound
	}
	if item.State != "claimed" || item.Owner != owner {
		return store.Todo{}, store.ErrConflict
	}
	item.State = "pending"
	item.Owner = ""
	item.LeaseExpiresAt = nil
	f.items[id] = item
	f.releases[id] = owner
	return item, nil
}

func heldLease(id, queue, owner string, expiresIn time.Duration) store.HeldLease {
	at := time.Now().Add(expiresIn)
	return store.HeldLease{TodoID: id, Queue: queue, Owner: owner, Attempt: 1, LeaseExpiresAt: &at}
}

func supervisionItem(id, queue, state, owner string) store.Todo {
	item := store.Todo{ID: id, EndpointID: defaultTestEndpointID,
		Queue: queue, Title: id, State: state, Owner: owner, Attempt: 2}
	if state == "claimed" {
		at := time.Now().Add(time.Minute)
		item.LeaseExpiresAt = &at
	}
	return item
}

// supervisionSession connects a session to the supervision fake holding the given verbs.
func supervisionSession(t *testing.T, ctx context.Context, f *supervisionFake, queues, verbs []string) (*sdk.ClientSession, *syncBuffer) {
	t.Helper()
	token := vend(t, f.fakeStore, defaultTestSlug, queues, verbs)
	logs := &syncBuffer{}
	h := New(f, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	cs, err := connect(t, ctx, ts.URL+"/mcp/"+defaultTestSlug, token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, logs
}

var supervisionVerbNames = []string{"queue_stats", "pause_queue", "resume_queue", "drain_queue",
	"requeue_todo", "release_lease"}

// The family is a family of its own: not in the default grant, not in AllVerbs (the consumer surface
// a "all" vend resolves to), not implied by any drain verb — and reachable by name or by its preset.
// This is the boundary the report asks for, stated as data before any session is opened.
func TestQueueOperatorVerbsAreNotImpliedByConsumerGrants(t *testing.T) {
	for _, grant := range [][]string{DefaultVendVerbs(), AllVerbs()} {
		for _, verb := range supervisionVerbNames {
			if verbAllowed(grant, verb) {
				t.Errorf("a consumer grant includes %s: %v", verb, grant)
			}
		}
	}
	if GrantHasQueueOperator(DrainVerbs()) || GrantHasQueueOperator(AllVerbs()) {
		t.Error("a consumer verb family implies operator supervision")
	}
	grant, err := ResolveVerbSpec([]string{"queue-operator"})
	if err != nil {
		t.Fatalf("resolve the queue-operator preset: %v", err)
	}
	for _, verb := range supervisionVerbNames {
		if !verbAllowed(grant, verb) {
			t.Errorf("the queue-operator preset does not grant %s: %v", verb, grant)
		}
	}
	if named := QueueOperatorVerbsIn(grant); len(named) != len(supervisionVerbNames) {
		t.Errorf("the preset resolves to %v, want the whole family", named)
	}
	if !GrantHasQueueOperator(grant) {
		t.Error("GrantHasQueueOperator misses a family grant")
	}
	if got, err := VendVerbGrant(nil); err != nil || GrantHasQueueOperator(got) {
		t.Errorf("the default vend grant carries supervision: %v (%v)", got, err)
	}
}

// The consumer refusal the report asks to be exercisable: a worker endpoint holding every consumer
// verb does not advertise a supervision verb, and calling it by name is a clean forbidden with no
// pause recorded.
func TestConsumerTokenIsRefusedSupervision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"}, AllVerbs())
	for _, verb := range supervisionVerbNames {
		if _, ok := toolNames(t, ctx, cs)[verb]; ok {
			t.Errorf("a consumer grant advertises %s", verb)
		}
		callErr(t, ctx, cs, verb, map[string]any{"queue": "inbox", "id": "td_1"}, codeForbidden)
	}
	if _, paused := f.pausedReason("inbox"); paused {
		t.Error("a refused pause reached the store")
	}
	if got := f.retried(); len(got) != 0 {
		t.Errorf("a refused requeue reached the store: %v", got)
	}
}

// The report's first gap, answered in one call: a queue with nothing waiting reads quiet, a queue
// whose backlog ages while nobody holds a lease reads stalled, a paused queue says so before
// anything else can, and a busy one reads working.
func TestQueueStatsVerdicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	f.liveness["quiet"] = store.QueueLiveness{Queue: "quiet"}
	f.liveness["stalled"] = store.QueueLiveness{Queue: "stalled", Pending: 3,
		DeadLettered: 1, OldestPendingSeconds: 900}
	f.liveness["busy"] = store.QueueLiveness{Queue: "busy", Pending: 2, Claimed: 1,
		OldestPendingSeconds: 900}
	f.liveness["held"] = store.QueueLiveness{Queue: "held", Pending: 4, Paused: true,
		OldestPendingSeconds: 900}
	cs, _ := supervisionSession(t, ctx, f, []string{"quiet", "stalled", "busy", "held"},
		[]string{"queue_stats"})

	var out queueStatsOut
	callOK(t, ctx, cs, "queue_stats", map[string]any{}, &out)
	want := map[string]struct {
		verdict string
		stalled bool
	}{
		"quiet": {"quiet", false}, "stalled": {"stalled", true},
		"busy": {"working", false}, "held": {"paused", false},
	}
	if len(out.Queues) != len(want) {
		t.Fatalf("stats covered %d queues, want %d: %+v", len(out.Queues), len(want), out.Queues)
	}
	for _, row := range out.Queues {
		w, ok := want[row.Queue]
		if !ok {
			t.Fatalf("stats named a queue the endpoint was not granted: %s", row.Queue)
		}
		if row.Verdict != w.verdict || row.Stalled != w.stalled {
			t.Errorf("queue %s reads %s (stalled %v), want %s (stalled %v)",
				row.Queue, row.Verdict, row.Stalled, w.verdict, w.stalled)
		}
	}
	for _, human := range f.seenHumans() {
		if human != "h-1" {
			t.Fatalf("stats read on behalf of %q, want the endpoint's own human", human)
		}
	}
	if out.LeasesListed != 0 {
		t.Errorf("stats listed %d leases with none outstanding", out.LeasesListed)
	}

	// An out-of-scope queue is refused before the seam is read.
	reads := len(f.seenHumans())
	callErr(t, ctx, cs, "queue_stats", map[string]any{"queue": "someone-elses"}, codeForbidden)
	if got := len(f.seenHumans()); got != reads {
		t.Errorf("the refused read reached the store: %d reads, was %d", got, reads)
	}
}

// A lease is reported with its holder and its expiry, so "who is holding this, and until when" is
// answerable without a listing.
func TestQueueStatsListsOutstandingLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	f.liveness["inbox"] = store.QueueLiveness{Queue: "inbox", Claimed: 1}
	f.nextLeases([]store.HeldLease{heldLease("td_held", "inbox", "agent:ag-9", 45*time.Second)})
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"}, []string{"queue_stats"})

	var out queueStatsOut
	callOK(t, ctx, cs, "queue_stats", map[string]any{}, &out)
	if len(out.Leases) != 1 {
		t.Fatalf("leases = %+v, want the one held row", out.Leases)
	}
	got := out.Leases[0]
	if got.TodoID != "td_held" || got.Owner != "agent:ag-9" || got.Queue != "inbox" ||
		got.LeaseExpiresAt == nil || *got.LeaseExpiresAt == "" {
		t.Errorf("lease row = %+v, want the todo, its queue, its holder and its expiry", got)
	}
	if got.ExpiresInSeconds == nil || *got.ExpiresInSeconds <= 0 || *got.ExpiresInSeconds > 45 {
		t.Errorf("expires_in_seconds = %v, want the seconds remaining", got.ExpiresInSeconds)
	}
}

// pause and resume drive ADR-0048's switch and nothing else: pause records the caller's reason,
// resume of an unpaused queue reports that it changed nothing, and a queue outside the grant is
// refused.
func TestPauseResumeDriveThePauseSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"},
		[]string{"pause_queue", "resume_queue"})

	callOK(t, ctx, cs, "pause_queue", map[string]any{"queue": "inbox", "reason": "upgrade"}, &queueActionOut{})
	if reason, ok := f.pausedReason("inbox"); !ok || reason != "upgrade" {
		t.Errorf("pause recorded %q (present %v), want the operator's reason", reason, ok)
	}
	callErr(t, ctx, cs, "pause_queue", map[string]any{"queue": "elsewhere"}, codeForbidden)

	var out queueActionOut
	callOK(t, ctx, cs, "resume_queue", map[string]any{"queue": "inbox"}, &out)
	if !out.Resumed || out.Paused {
		t.Errorf("resume of a paused queue reported %+v, want resumed and no longer paused", out)
	}
	if _, paused := f.pausedReason("inbox"); paused {
		t.Error("the resume left the queue paused")
	}
	var unpaused queueActionOut
	callOK(t, ctx, cs, "resume_queue", map[string]any{"queue": "inbox"}, &unpaused)
	if unpaused.Resumed {
		t.Errorf("resume of a queue that was not paused reported %+v, want resumed false", unpaused)
	}
	if got := f.resumes; len(got) != 2 || got[0] != "inbox" {
		t.Errorf("resume calls = %v, want both aimed at inbox", got)
	}
}

// A drain pauses first and then waits for the leases: it returns when the last one lands, and when
// the wait runs out it leaves the queue paused and names what is still held.
func TestDrainWaitsForTheLastLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	f.nextLeases(
		[]store.HeldLease{heldLease("td_one", "inbox", "agent:ag-9", time.Minute)},
		[]store.HeldLease{heldLease("td_one", "inbox", "agent:ag-9", time.Minute)},
		nil)
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"}, []string{"drain_queue"})

	started := time.Now()
	var out queueActionOut
	callOK(t, ctx, cs, "drain_queue", map[string]any{"queue": "inbox", "wait_seconds": 6}, &out)
	if !out.Paused || !out.Drained || len(out.Leases) != 0 {
		t.Errorf("drain = %+v, want paused and drained with nothing outstanding", out)
	}
	if got := f.leasesRead(); got < 2 {
		t.Errorf("the drain read the leases %d times, wanting it to poll while waiting", got)
	}
	if waited := time.Since(started); waited < 2*time.Second {
		t.Errorf("the drain returned after %v without waiting a poll interval", waited)
	}
	if reason, ok := f.pausedReason("inbox"); !ok || reason != "drain_queue" {
		t.Errorf("drain left pause reason %q (present %v)", reason, ok)
	}

	// A zero wait reports the state without waiting, and says the queue is not drained.
	f.nextLeases([]store.HeldLease{heldLease("td_stuck", "inbox", "agent:ag-9", 20*time.Minute)})
	var stuck queueActionOut
	callOK(t, ctx, cs, "drain_queue", map[string]any{"queue": "inbox", "wait_seconds": 0}, &stuck)
	out = stuck
	if out.Drained || len(out.Leases) != 1 || out.Leases[0].TodoID != "td_stuck" {
		t.Errorf("no-wait drain = %+v, want the held lease reported", out)
	}
	if !out.Paused {
		t.Error("a drain that ran out of wait un-paused the queue")
	}
	callErr(t, ctx, cs, "drain_queue", map[string]any{"queue": "inbox", "wait_seconds": 99999},
		codeInvalid)
}

// A requeue moves a failed or dead-lettered row and refuses one that is not failed: a claimed row
// keeps its lease and its holder, and a row in a queue outside the grant is refused before the
// store is asked at all.
func TestRequeueTodoGuardsStateAndScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	f.items["td_dead"] = supervisionItem("td_dead", "inbox", "failed", "")
	f.items["td_held"] = supervisionItem("td_held", "inbox", "claimed", "agent:ag-9")
	f.items["td_foreign"] = supervisionItem("td_foreign", "elsewhere", "failed", "")
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"}, []string{"requeue_todo"})

	var row todoOut
	callOK(t, ctx, cs, "requeue_todo", map[string]any{"id": "td_dead", "reason": "fixed the cause"}, &row)
	if row.State != "pending" || row.Attempt != 0 {
		t.Errorf("requeue returned %+v, want pending with a fresh attempt budget", row)
	}
	if got := f.retried(); len(got) != 1 || got[0] != "td_dead" {
		t.Errorf("retries = %v, want just the dead letter", got)
	}
	if text := callErr(t, ctx, cs, "requeue_todo", map[string]any{"id": "td_held"},
		codeConflict); !strings.Contains(text, "claimed") {
		t.Errorf("conflict = %q, want it to name the state it found", text)
	}
	callErr(t, ctx, cs, "requeue_todo", map[string]any{"id": "td_foreign"}, codeForbidden)
	if got := f.retried(); len(got) != 1 {
		t.Errorf("a refused requeue reached the store: %v", got)
	}
	callErr(t, ctx, cs, "requeue_todo", map[string]any{"id": "td_absent"}, codeNotFound)
}

// A release reaches the row under another endpoint's lease, by way of the row itself, and closes it
// back to pending; the holder it named must be the holder the store lets go of.
func TestReleaseLeaseHandsBackAnotherHoldersLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	f.items["td_held"] = supervisionItem("td_held", "inbox", "claimed", "agent:ag-9")
	f.items["td_done"] = supervisionItem("td_done", "inbox", "done", "agent:ag-9")
	f.items["td_foreign"] = supervisionItem("td_foreign", "elsewhere", "claimed", "agent:ag-9")
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"}, []string{"release_lease"})

	var out queueActionOut
	callOK(t, ctx, cs, "release_lease",
		map[string]any{"id": "td_held", "reason": "its run was killed"}, &out)
	if owners := f.releasedOwners(); owners["td_held"] != "agent:ag-9" {
		t.Errorf("releases = %v, want the lease taken from its holder", owners)
	}
	if out.ID != "td_held" || out.Queue != "inbox" {
		t.Errorf("release reported %+v, want the todo it handed back and its queue", out)
	}
	callErr(t, ctx, cs, "release_lease", map[string]any{"id": "td_done"}, codeConflict)
	callErr(t, ctx, cs, "release_lease", map[string]any{"id": "td_foreign"}, codeForbidden)
	if owners := f.releasedOwners(); len(owners) != 1 {
		t.Errorf("refused releases reached the store: %v", owners)
	}
}

// Every state-changing supervision call leaves one audit line naming the endpoint, the human behind
// the credential, the target and the reason — and never the credential itself.
func TestSupervisionAuditLines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	f.items["td_held"] = supervisionItem("td_held", "inbox", "claimed", "agent:ag-9")
	cs, logs := supervisionSession(t, ctx, f, []string{"inbox"},
		[]string{"pause_queue", "release_lease", "queue_stats"})

	callOK(t, ctx, cs, "pause_queue", map[string]any{"queue": "inbox", "reason": "upgrade-window"}, &queueActionOut{})
	callOK(t, ctx, cs, "release_lease", map[string]any{"id": "td_held", "reason": "killed-run"}, &queueActionOut{})
	callOK(t, ctx, cs, "queue_stats", map[string]any{}, &queueStatsOut{})

	text := logs.String()
	for _, want := range []string{"mcp operator action", "tool=pause_queue", "queue=inbox",
		"reason=upgrade-window", "tool=release_lease", "todo=td_held", "holder=agent:ag-9",
		"reason=killed-run", "human=h-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("audit lines lack %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sbk_") {
		t.Errorf("an audit line carries a credential:\n%s", text)
	}
	if n := strings.Count(text, "mcp operator action"); n != 2 {
		t.Errorf("%d audit lines for two state-changing calls and one read:\n%s", n, text)
	}
}

// A granted verb on a server whose store cannot serve it is a deployment fault stated plainly, not a
// silent empty answer: the plain fake carries no supervision seam.
func TestSupervisionWithoutTheSeamIsAnInternalError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	token := vend(t, f, defaultTestSlug, []string{"inbox"}, []string{"queue_stats"})
	logs := &syncBuffer{}
	h := New(f, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	cs, err := connect(t, ctx, ts.URL+"/mcp/"+defaultTestSlug, token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	callErr(t, ctx, cs, "queue_stats", map[string]any{}, codeInternal)
}

// The advertised schemas carry the arguments an operator needs and no others.
func TestSupervisionToolSchemas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newSupervisionFake()
	cs, _ := supervisionSession(t, ctx, f, []string{"inbox"}, QueueOperatorVerbs())
	names := toolNames(t, ctx, cs)
	for _, verb := range supervisionVerbNames {
		if names[verb] == nil {
			t.Fatalf("the family grant does not advertise %s", verb)
		}
	}
	cases := map[string][]string{
		"queue_stats":   {`"queue"`, `"stalled_after_seconds"`, `"lease_limit"`},
		"pause_queue":   {`"queue"`, `"reason"`},
		"resume_queue":  {`"queue"`},
		"drain_queue":   {`"queue"`, `"wait_seconds"`},
		"requeue_todo":  {`"id"`, `"reason"`},
		"release_lease": {`"id"`, `"reason"`},
	}
	for verb, fields := range cases {
		in, err := json.Marshal(names[verb].InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", verb, err)
		}
		for _, field := range fields {
			if !strings.Contains(string(in), field) {
				t.Errorf("%s input schema lacks %s: %s", verb, field, in)
			}
		}
	}
}
