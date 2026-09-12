package redis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/stump-wtf/switchboard/internal/adapter"
)

// readStep scripts one XReadGroup response.
type readStep struct {
	msgs []goredis.XMessage
	err  error
}

// fakeStreamClient scripts XReadGroup responses and records ack calls; when the script is
// exhausted it cancels the consume context, ending the loop like a shutdown would.
type fakeStreamClient struct {
	ops      *opLog
	mu       sync.Mutex
	groupErr error
	steps    []readStep
	cursors  []string // the id each XReadGroup call used
	acks     []string
	ackErr   error
	cancel   context.CancelFunc
}

func (f *fakeStreamClient) XGroupCreateMkStream(ctx context.Context, stream, group, start string) *goredis.StatusCmd {
	return goredis.NewStatusResult("OK", f.groupErr)
}

func (f *fakeStreamClient) XReadGroup(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors = append(f.cursors, a.Streams[len(a.Streams)-1])
	if len(f.steps) == 0 {
		f.cancel()
		return goredis.NewXStreamSliceCmdResult(nil, context.Canceled)
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	if step.err != nil {
		return goredis.NewXStreamSliceCmdResult(nil, step.err)
	}
	return goredis.NewXStreamSliceCmdResult(
		[]goredis.XStream{{Stream: a.Streams[0], Messages: step.msgs}}, nil)
}

func (f *fakeStreamClient) XAck(ctx context.Context, stream, group string, ids ...string) *goredis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr == nil {
		f.acks = append(f.acks, ids...)
		for _, id := range ids {
			f.ops.add("xack:" + id)
		}
	}
	return goredis.NewIntResult(int64(len(ids)), f.ackErr)
}

func (f *fakeStreamClient) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acks...)
}

func newStreamHarness(t *testing.T, steps []readStep, failures map[string]int) (*Stream, *fakeStreamClient, *fakeSink, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ops := &opLog{}
	client := &fakeStreamClient{ops: ops, steps: steps, cancel: cancel}
	sink := &fakeSink{ops: ops, failures: failures, err: errors.New("store failed")}
	s, err := NewStream(client, testLogger(), StreamConfig{
		Stream: "deploys", Group: "switchboard", Consumer: "c1", TrustDetail: "redis acl: deploy-bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, client, sink, ctx
}

func msg(id, payload string) goredis.XMessage {
	return goredis.XMessage{ID: id, Values: map[string]any{"payload": payload}}
}

// SPEC-0002 scenario "Stream consumer group acks after store": XREADGROUP, store the todo, and
// XACK only after the todo is durably stored — for every message, in order.
func TestStreamAckOnlyAfterStore(t *testing.T) {
	steps := []readStep{
		{}, // pending drain (cursor "0"): nothing stranded
		{msgs: []goredis.XMessage{msg("1-0", `{"a":1}`), msg("2-0", `{"b":2}`)}},
	}
	s, client, sink, ctx := newStreamHarness(t, steps, nil)

	if err := s.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}

	// Redelivery drain first: the first read uses cursor "0" (this consumer's pending entries),
	// only then ">" (new entries).
	if client.cursors[0] != "0" || client.cursors[1] != ">" {
		t.Fatalf("cursors = %v, want pending drain (\"0\") then new (\">\")", client.cursors)
	}

	// Store-then-ack ordering: each XACK strictly follows that message's successful delivery.
	want := []string{
		"deliver:redis:deploys:1-0", "xack:1-0",
		"deliver:redis:deploys:2-0", "xack:2-0",
	}
	if got := sink.ops.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("op order = %v, want %v", got, want)
	}
	if got := client.ackedIDs(); len(got) != 2 || got[0] != "1-0" || got[1] != "2-0" {
		t.Fatalf("acks = %v, want [1-0 2-0]", got)
	}

	// The envelope carries the native entry id and the queue trust source.
	env := sink.envs[0]
	if env.Source != Source || env.Name != "deploys" || env.ExternalID != "1-0" {
		t.Fatalf("envelope = %+v, want redis/deploys/1-0", env)
	}
	if string(env.Payload) != `{"a":1}` {
		t.Fatalf("payload = %q", env.Payload)
	}
	if env.IdempotencyKey() != "redis:deploys:1-0" {
		t.Fatalf("key = %q, want redis:deploys:1-0", env.IdempotencyKey())
	}
}

// SPEC-0002 scenario "Store failure leaves message for redelivery" + scenario "Crash before
// todo-store redelivers and dedups to one todo": a Deliver (store) failure must NOT ack, the
// redelivered entry derives the identical idempotency key, and only the successful delivery acks.
func TestStreamStoreFailureLeavesUnackedAndRedeliveryDedups(t *testing.T) {
	steps := []readStep{
		{msgs: []goredis.XMessage{msg("1-0", `{"a":1}`)}}, // pending drain: stranded by a "crash"
		{}, // pending drained → switch to new entries
		{msgs: []goredis.XMessage{msg("1-0", `{"a":1}`)}}, // redelivery
	}
	s, client, sink, ctx := newStreamHarness(t, steps, map[string]int{"redis:deploys:1-0": 1})

	if err := s.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}

	// Acked exactly once — never after the failed store, only after the successful one.
	if got := client.ackedIDs(); len(got) != 1 || got[0] != "1-0" {
		t.Fatalf("acks = %v, want exactly [1-0]", got)
	}
	want := []string{"deliver-fail:redis:deploys:1-0", "deliver:redis:deploys:1-0", "xack:1-0"}
	if got := sink.ops.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("op order = %v, want %v", got, want)
	}

	// Both deliveries derived the SAME key — the dedup property that collapses the redelivery to
	// one todo.
	keys := sink.keys()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("keys = %v, want the same key twice", keys)
	}
}

// A failed XACK after a durable store is safe (redelivery dedups); the loop continues rather than
// crashing, and the entry is simply not recorded as acked.
func TestStreamAckFailureContinues(t *testing.T) {
	steps := []readStep{
		{},
		{msgs: []goredis.XMessage{msg("1-0", `{"a":1}`)}},
	}
	s, client, sink, ctx := newStreamHarness(t, steps, nil)
	client.ackErr = errors.New("connection reset")

	if err := s.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	if got := client.ackedIDs(); len(got) != 0 {
		t.Fatalf("acks = %v, want none recorded", got)
	}
	if got := sink.keys(); len(got) != 1 {
		t.Fatalf("deliveries = %v, want the message still delivered once", got)
	}
}

// An existing consumer group (BUSYGROUP) is fine — group creation is idempotent across restarts.
func TestStreamBusyGroupTolerated(t *testing.T) {
	s, client, sink, ctx := newStreamHarness(t, nil, nil)
	client.groupErr = errors.New("BUSYGROUP Consumer Group name already exists")

	if err := s.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled (BUSYGROUP tolerated)", err)
	}
}

// Any other group-creation failure is wrapped and returned.
func TestStreamGroupCreateErrorWrapped(t *testing.T) {
	s, client, sink, ctx := newStreamHarness(t, nil, nil)
	client.groupErr = errors.New("NOAUTH Authentication required")

	err := s.Consume(ctx, sink)
	if err == nil || !strings.Contains(err.Error(), "create group") {
		t.Fatalf("Consume = %v, want wrapped create-group error", err)
	}
}

// A broker read error is wrapped with context and returned (the poll-loop lifecycle owns
// backoff/retry — the transport must not swallow it).
func TestStreamReadErrorWrapped(t *testing.T) {
	steps := []readStep{{err: errors.New("connection refused")}}
	s, _, sink, ctx := newStreamHarness(t, steps, nil)

	err := s.Consume(ctx, sink)
	if err == nil || !strings.Contains(err.Error(), "xreadgroup") {
		t.Fatalf("Consume = %v, want wrapped xreadgroup error", err)
	}
}

// SPEC-0002 scenario "Graceful shutdown stops consuming and abandons in-flight safely" (transport
// half): cancellation mid-batch stops before the next message, leaving it un-acked for redelivery.
func TestStreamCancelMidBatchLeavesRestUnacked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ops := &opLog{}
	client := &fakeStreamClient{ops: ops, cancel: cancel, steps: []readStep{
		{},
		{msgs: []goredis.XMessage{msg("1-0", `{"a":1}`), msg("2-0", `{"b":2}`)}},
	}}
	sink := &cancellingSink{fakeSink: &fakeSink{ops: ops}, cancel: cancel}
	s, err := NewStream(client, testLogger(), StreamConfig{Stream: "deploys", Group: "g", Consumer: "c"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	// First message was stored and acked; the second was never consumed after cancellation, so it
	// stays pending (un-acked) for redelivery.
	if got := client.ackedIDs(); len(got) != 1 || got[0] != "1-0" {
		t.Fatalf("acks = %v, want only [1-0]", got)
	}
	if got := sink.keys(); len(got) != 1 {
		t.Fatalf("deliveries = %v, want only the first message", got)
	}
}

// cancellingSink cancels the consume context from inside the first successful delivery, modeling a
// shutdown that lands mid-batch.
type cancellingSink struct {
	*fakeSink
	cancel context.CancelFunc
}

func (s *cancellingSink) Deliver(ctx context.Context, env adapter.Envelope) error {
	err := s.fakeSink.Deliver(ctx, env)
	s.cancel()
	return err
}

// NewStream validates required config and applies naming defaults.
func TestNewStreamConfig(t *testing.T) {
	if _, err := NewStream(&fakeStreamClient{}, testLogger(), StreamConfig{Stream: "s", Group: "g"}); err == nil {
		t.Fatal("missing consumer must error")
	}
	s, err := NewStream(&fakeStreamClient{}, testLogger(), StreamConfig{
		Stream: "deploys", Group: "g", Consumer: "c", TrustDetail: "redis acl: deploy-bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "redis-stream:deploys" {
		t.Fatalf("Name() = %q, want redis-stream:deploys", s.Name())
	}
	if s.TrustDetail() != "redis acl: deploy-bot" {
		t.Fatalf("TrustDetail() = %q", s.TrustDetail())
	}
}
