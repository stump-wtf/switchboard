package adapter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// fakeEventTodoStore is an in-memory EventTodoCreator that mirrors the real store's dedup contract:
// events dedup on (source, external_id), todos dedup on (queue, idempotency_key) among non-terminal
// rows, and the pair commits atomically (a configured failure persists nothing). It records every
// call and an ops log so tests can assert ordering against a transport's ack.
type fakeEventTodoStore struct {
	mu       sync.Mutex
	failures int // fail this many leading calls with err
	err      error

	calls       int
	hadDeadline bool // whether the last ctx carried a deadline (bounded-timeout standard)
	lastEvent   store.EventInput
	lastParams  store.CreateTodoParams
	todos       map[string]store.Todo // (queue, idempotency_key) → todo
	ops         []string              // "store <key>" entries, interleaved with transport acks
	nextID      int64
}

func newFakeEventTodoStore() *fakeEventTodoStore {
	return &fakeEventTodoStore{todos: map[string]store.Todo{}}
}

func (f *fakeEventTodoStore) CreateEventTodo(ctx context.Context, e store.EventInput, p store.CreateTodoParams) (int64, store.Todo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	_, f.hadDeadline = ctx.Deadline()
	f.lastEvent, f.lastParams = e, p
	if f.failures > 0 {
		f.failures--
		return 0, store.Todo{}, false, f.err // atomic: nothing persisted on failure
	}
	key := p.Queue + "\x00" + p.IdempotencyKey
	if td, ok := f.todos[key]; ok {
		return *td.EventID, td, false, nil // redelivery collapses onto the existing row
	}
	f.nextID++
	ev := f.nextID
	td := store.Todo{
		ID: "td_" + p.IdempotencyKey, Queue: p.Queue, Source: p.Source, Kind: p.Kind,
		Title: p.Title, Payload: p.Payload, EventID: &ev, IdempotencyKey: p.IdempotencyKey,
		State: "pending",
	}
	f.todos[key] = td
	f.ops = append(f.ops, "store "+p.IdempotencyKey)
	return ev, td, true, nil
}

func (f *fakeEventTodoStore) recordAck(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "ack "+id)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestSink(t *testing.T, st EventTodoCreator, cfg StoreSinkConfig) *StoreSink {
	t.Helper()
	if cfg.TrustDetail == "" {
		cfg.TrustDetail = "redis acl: deploy-bot"
	}
	s, err := NewStoreSink(st, testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewStoreSink: %v", err)
	}
	return s
}

func TestNewStoreSinkRequiredFields(t *testing.T) {
	if _, err := NewStoreSink(nil, testLogger(), StoreSinkConfig{TrustDetail: "x"}); err == nil {
		t.Error("NewStoreSink(nil store) = nil error, want error")
	}
	// SPEC-0002 REQ "Adapter Interface and Trust Mode": every pull-ingested event MUST carry a
	// verify_detail naming the broker/ACL identity, so a sink with no trust detail must not build.
	if _, err := NewStoreSink(newFakeEventTodoStore(), testLogger(), StoreSinkConfig{}); err == nil {
		t.Error("NewStoreSink(no trust detail) = nil error, want error")
	}
}

// SPEC-0002 REQ "Enqueue Consumed Message as Todo — Database Operation Standards" (scenario "Todo
// persisted with parameterized queries before ack"): a valid envelope lands as one atomic
// event+todo write — queue trust stamped, idempotency key derived — under a bounded-timeout context.
func TestStoreSinkDeliverEnqueuesEnvelope(t *testing.T) {
	st := newFakeEventTodoStore()
	sink := newTestSink(t, st, StoreSinkConfig{Queue: "deploys"})
	env := validEnvelope()

	if err := sink.Deliver(context.Background(), env); err != nil {
		t.Fatalf("Deliver() = %v, want nil", err)
	}

	// The event row is stamped with the pull family's fixed trust metadata (ADR-0003).
	e := st.lastEvent
	if e.Family != Family || e.TrustMode != TrustMode || e.Verified {
		t.Errorf("event Family/TrustMode/Verified = %q/%q/%v, want %q/%q/false",
			e.Family, e.TrustMode, e.Verified, Family, TrustMode)
	}
	if e.VerifyDetail != "redis acl: deploy-bot" {
		t.Errorf("event VerifyDetail = %q, want the broker/ACL identity", e.VerifyDetail)
	}
	if e.ExternalID != env.IdempotencyKey() {
		t.Errorf("event ExternalID = %q, want derived idempotency key %q", e.ExternalID, env.IdempotencyKey())
	}

	// The todo row carries the derived key, the configured queue, and the envelope's payload.
	p := st.lastParams
	if p.Queue != "deploys" || p.Source != env.Source || p.IdempotencyKey != env.IdempotencyKey() {
		t.Errorf("todo Queue/Source/Key = %q/%q/%q, want deploys/%s/%s",
			p.Queue, p.Source, p.IdempotencyKey, env.Source, env.IdempotencyKey())
	}
	if string(p.Payload) != string(env.Payload) {
		t.Error("todo payload must pass through unchanged")
	}
	if p.Kind == "" || p.Title == "" {
		t.Errorf("todo Kind/Title = %q/%q, want non-empty", p.Kind, p.Title)
	}

	// Database Operation Standards: the write ran under an explicit bounded timeout.
	if !st.hadDeadline {
		t.Error("CreateEventTodo ctx carried no deadline; Deliver must bound the transaction")
	}
}

// The target queue defaults to the envelope's Name (stream/list/channel) when the sink is not
// pinned to one queue — mirroring the generic push provider's queue-defaults-to-name behavior.
func TestStoreSinkDefaultQueueIsEnvelopeName(t *testing.T) {
	st := newFakeEventTodoStore()
	sink := newTestSink(t, st, StoreSinkConfig{})
	env := validEnvelope()
	if err := sink.Deliver(context.Background(), env); err != nil {
		t.Fatalf("Deliver() = %v, want nil", err)
	}
	if st.lastParams.Queue != env.Name {
		t.Errorf("todo Queue = %q, want envelope name %q", st.lastParams.Queue, env.Name)
	}
}

// Traceability test for the Sink contract's validate-then-derive invariant (previously doc-only on
// the adapter seam): Deliver MUST call Envelope.Validate() and reject an invalid envelope with its
// sentinel error BEFORE deriving anything or touching the store — an unvalidated envelope would
// still derive a degenerate idempotency key (e.g. `source::sha256("")`) and could persist a bogus
// todo/event. Governing: SPEC-0002 REQ "Error Handling Standards" (sentinel errors, wrapped with
// context, never swallowed).
func TestStoreSinkValidatesBeforeDeliver(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Envelope)
		want   error
	}{
		{"missing source", func(e *Envelope) { e.Source = "" }, ErrNoSource},
		{"missing name", func(e *Envelope) { e.Name = "" }, ErrNoName},
		{"missing id and payload", func(e *Envelope) { e.ExternalID = ""; e.Payload = nil }, ErrNoIdentity},
		{"missing received-at", func(e *Envelope) { e.ReceivedAt = time.Time{} }, ErrNoReceivedAt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeEventTodoStore()
			sink := newTestSink(t, st, StoreSinkConfig{})
			env := validEnvelope()
			tc.mutate(&env)

			err := sink.Deliver(context.Background(), env)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Deliver() = %v, want sentinel %v", err, tc.want)
			}
			if st.calls != 0 {
				t.Fatalf("store called %d times for an invalid envelope, want 0 (validate before derive)", st.calls)
			}
		})
	}
}

// SPEC-0002 REQ "Error Handling Standards" (scenario "Store failure leaves message for
// redelivery"): a store failure surfaces as a wrapped, distinguishable error — never nil, never
// swallowed — so the transport leaves the source message un-acked and the broker redelivers it.
func TestStoreSinkWrapsStoreFailure(t *testing.T) {
	boom := errors.New("connection reset")
	st := newFakeEventTodoStore()
	st.failures, st.err = 1, boom
	sink := newTestSink(t, st, StoreSinkConfig{})

	err := sink.Deliver(context.Background(), validEnvelope())
	if !errors.Is(err, boom) {
		t.Fatalf("Deliver() = %v, want wrapped %v", err, boom)
	}
	if !strings.Contains(err.Error(), "create event+todo") {
		t.Errorf("error %q lacks the boundary context it crossed", err)
	}
}

// SPEC-0002 REQ "Idempotency Key From Source Message Id": a duplicate delivery of the same source
// message derives the same key and collapses onto the existing todo. Deliver still returns nil so
// the transport acks the redelivered copy.
func TestStoreSinkDuplicateDeliveryCollapses(t *testing.T) {
	st := newFakeEventTodoStore()
	sink := newTestSink(t, st, StoreSinkConfig{Queue: "deploys"})
	env := validEnvelope()

	for i := 0; i < 2; i++ {
		if err := sink.Deliver(context.Background(), env); err != nil {
			t.Fatalf("Deliver() #%d = %v, want nil", i+1, err)
		}
	}
	if got := len(st.todos); got != 1 {
		t.Fatalf("todos stored = %d, want 1 (duplicate must dedup)", got)
	}
}

// fakeAdapter is a minimal pull transport for the end-to-end sink test: it consumes a fixed batch
// of messages (with native ids) and honors the Adapter contract's store-then-ack rule — a message
// is acked only after Deliver returns nil.
type fakeAdapter struct {
	name     string
	messages []fakeMessage
	store    *fakeEventTodoStore // shared ops log for ordering assertions
	acked    []string
}

type fakeMessage struct {
	id   string
	body string
}

func (a *fakeAdapter) Name() string        { return a.name }
func (a *fakeAdapter) TrustDetail() string { return "redis acl: deploy-bot" }

func (a *fakeAdapter) Consume(ctx context.Context, sink Sink) error {
	for _, m := range a.messages {
		env := Envelope{
			Source:     "redis",
			Name:       "deploys",
			ExternalID: m.id,
			Payload:    []byte(m.body),
			ReceivedAt: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		}
		if err := sink.Deliver(ctx, env); err != nil {
			// Store-then-ack: no ack on failure — the message stays with the broker for redelivery.
			continue
		}
		a.acked = append(a.acked, m.id)
		a.store.recordAck(m.id)
	}
	return nil
}

var _ Adapter = (*fakeAdapter)(nil)

// End-to-end over the adapter seam: fake transport → Envelope → StoreSink → atomic event+todo row,
// with queue trust stamping and native-id idempotency; a duplicate delivery collapses to one todo;
// a store failure is not acked (redelivery), and every ack happens only AFTER the durable store.
// Governing: SPEC-0002 REQ "Store-Then-Ack Coupling" (scenario "Ack happens only after durable
// store"), REQ "Enqueue Consumed Message as Todo — Database Operation Standards".
func TestEndToEndFakeAdapterStoreThenAck(t *testing.T) {
	st := newFakeEventTodoStore()
	boom := errors.New("db down")
	st.failures, st.err = 1, boom // first delivery fails at the store; nothing persists
	sink := newTestSink(t, st, StoreSinkConfig{Queue: "deploys"})

	a := &fakeAdapter{
		name:  "redis-stream:deploys",
		store: st,
		messages: []fakeMessage{
			{id: "1-0", body: `{"job":"deploy"}`}, // store fails → must NOT ack (broker redelivers)
			{id: "1-0", body: `{"job":"deploy"}`}, // the redelivery → stores → ack
			{id: "1-0", body: `{"job":"deploy"}`}, // duplicate → dedups onto the same todo → ack
			{id: "2-0", body: `{"job":"release"}`},
		},
	}
	if err := a.Consume(context.Background(), sink); err != nil {
		t.Fatalf("Consume() = %v, want nil", err)
	}

	// The failed first delivery was not acked; the redelivery and the rest were.
	if want := []string{"1-0", "1-0", "2-0"}; len(a.acked) != len(want) {
		t.Fatalf("acked = %v, want %v (store failure must not ack)", a.acked, want)
	}

	// Exactly two todos: the redelivered/duplicate message collapsed onto one row.
	if got := len(st.todos); got != 2 {
		t.Fatalf("todos stored = %d, want 2 (redelivery dedups)", got)
	}
	td, ok := st.todos["deploys\x00redis:deploys:1-0"]
	if !ok {
		t.Fatalf("todo for key redis:deploys:1-0 missing; have %v", st.ops)
	}
	if td.Source != "redis" || td.Queue != "deploys" || td.State != "pending" {
		t.Errorf("todo Source/Queue/State = %q/%q/%q, want redis/deploys/pending", td.Source, td.Queue, td.State)
	}
	if td.EventID == nil {
		t.Error("todo EventID = nil, want linked event row (atomic event+todo)")
	}

	// Ordering: for each message id, the durable store strictly precedes the first ack.
	for _, id := range []string{"1-0", "2-0"} {
		storeAt, ackAt := -1, -1
		for i, op := range st.ops {
			if storeAt < 0 && strings.HasPrefix(op, "store ") && strings.HasSuffix(op, ":"+id) {
				storeAt = i
			}
			if ackAt < 0 && op == "ack "+id {
				ackAt = i
			}
		}
		if storeAt < 0 || ackAt < 0 || storeAt > ackAt {
			t.Errorf("message %s: store at %d, ack at %d — ack must follow durable store (ops %v)",
				id, storeAt, ackAt, st.ops)
		}
	}
}
