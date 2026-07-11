package redis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// popStep scripts one BRPopLPush response.
type popStep struct {
	val string
	err error
}

// fakeListClient scripts BRPopLPush responses, serves a fixed processing-list snapshot for the
// recovery pass, and records LREM acks; when the pop script is exhausted it cancels the consume
// context, ending the loop like a shutdown would.
type fakeListClient struct {
	ops        *opLog
	mu         sync.Mutex
	processing []string // recovery snapshot, head → tail (tail = oldest)
	rangeErr   error
	steps      []popStep
	lrems      []string
	lremErr    error
	cancel     context.CancelFunc
}

func (f *fakeListClient) LRange(ctx context.Context, key string, start, stop int64) *goredis.StringSliceCmd {
	return goredis.NewStringSliceResult(append([]string(nil), f.processing...), f.rangeErr)
}

func (f *fakeListClient) BRPopLPush(ctx context.Context, source, destination string, timeout time.Duration) *goredis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.steps) == 0 {
		f.cancel()
		return goredis.NewStringResult("", context.Canceled)
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	return goredis.NewStringResult(step.val, step.err)
}

func (f *fakeListClient) LRem(ctx context.Context, key string, count int64, value interface{}) *goredis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lremErr == nil {
		v := value.(string)
		f.lrems = append(f.lrems, v)
		f.ops.add("lrem:" + v)
	}
	return goredis.NewIntResult(1, f.lremErr)
}

func (f *fakeListClient) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lrems...)
}

func newListHarness(t *testing.T, client *fakeListClient, failures map[string]int) (*List, *fakeSink, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ops := &opLog{}
	client.ops = ops
	client.cancel = cancel
	sink := &fakeSink{ops: ops, failures: failures, err: errors.New("store failed")}
	l, err := NewList(client, testLogger(), ListConfig{List: "jobs", TrustDetail: "redis acl: deploy-bot"})
	if err != nil {
		t.Fatal(err)
	}
	return l, sink, ctx
}

func listKey(body string) string { return bodyKey("jobs", body) }

// SPEC-0002 REQ "Redis Reference Transport Modes" (reliable list): BRPOPLPUSH parks the message on
// the processing list and LREM removes it only after the todo is durably stored.
func TestListAckOnlyAfterStore(t *testing.T) {
	client := &fakeListClient{steps: []popStep{{val: `{"a":1}`}}}
	l, sink, ctx := newListHarness(t, client, nil)

	if err := l.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}

	want := []string{"deliver:" + listKey(`{"a":1}`), `lrem:{"a":1}`}
	if got := sink.ops.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("op order = %v, want %v", got, want)
	}

	// A list message has no native id: the envelope leaves ExternalID empty and the key falls
	// back to redis:{list}:sha256(body).
	env := sink.envs[0]
	if env.Source != Source || env.Name != "jobs" || env.ExternalID != "" {
		t.Fatalf("envelope = %+v, want redis/jobs with empty ExternalID", env)
	}
	if env.IdempotencyKey() != listKey(`{"a":1}`) {
		t.Fatalf("key = %q, want body-hash fallback", env.IdempotencyKey())
	}
}

// SPEC-0002 scenario "Store failure leaves message for redelivery": a Deliver (store) failure must
// NOT LREM — the message stays on the processing list for the recovery pass.
func TestListStoreFailureLeavesProcessing(t *testing.T) {
	client := &fakeListClient{steps: []popStep{{val: `{"a":1}`}}}
	l, sink, ctx := newListHarness(t, client, map[string]int{listKey(`{"a":1}`): 1})

	if err := l.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	if got := client.removed(); len(got) != 0 {
		t.Fatalf("lrems = %v, want none (message must stay on processing list)", got)
	}
}

// SPEC-0002 scenario "Crash before todo-store redelivers and dedups to one todo" (list mode): the
// recovery pass re-delivers messages stranded on the processing list, oldest (tail) first, and
// removes each only after its todo is durably stored.
func TestListRecoveryRedeliversStranded(t *testing.T) {
	// BRPOPLPUSH LPUSHes to the head, so head("new") → tail("old"): "old" was consumed first.
	client := &fakeListClient{processing: []string{`{"new":true}`, `{"old":true}`}}
	l, sink, ctx := newListHarness(t, client, nil)

	if err := l.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}

	want := []string{
		"deliver:" + listKey(`{"old":true}`), `lrem:{"old":true}`,
		"deliver:" + listKey(`{"new":true}`), `lrem:{"new":true}`,
	}
	if got := sink.ops.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("op order = %v, want oldest-first with ack-after-store: %v", got, want)
	}

	// A redelivered body derives the identical key — the dedup half of store-then-ack.
	if sink.envs[0].IdempotencyKey() != listKey(`{"old":true}`) {
		t.Fatalf("key = %q, want deterministic body-hash key", sink.envs[0].IdempotencyKey())
	}
}

// A recovery-pass failure keeps the stranded message on the processing list and moves on.
func TestListRecoveryStoreFailureKeepsMessage(t *testing.T) {
	client := &fakeListClient{processing: []string{`{"a":1}`}}
	l, sink, ctx := newListHarness(t, client, map[string]int{listKey(`{"a":1}`): 1})

	if err := l.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	if got := client.removed(); len(got) != 0 {
		t.Fatalf("lrems = %v, want none", got)
	}
}

// An empty block window (redis.Nil) is not an error — the loop just polls again.
func TestListNilContinues(t *testing.T) {
	client := &fakeListClient{steps: []popStep{{err: goredis.Nil}, {val: `{"a":1}`}}}
	l, sink, ctx := newListHarness(t, client, nil)

	if err := l.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	if got := sink.keys(); len(got) != 1 {
		t.Fatalf("deliveries = %v, want one after the empty window", got)
	}
}

// A broker pop error is wrapped with context and returned (backoff/retry is the lifecycle's job).
func TestListPopErrorWrapped(t *testing.T) {
	client := &fakeListClient{steps: []popStep{{err: errors.New("connection refused")}}}
	l, sink, ctx := newListHarness(t, client, nil)

	err := l.Consume(ctx, sink)
	if err == nil || !strings.Contains(err.Error(), "brpoplpush") {
		t.Fatalf("Consume = %v, want wrapped brpoplpush error", err)
	}
}

// NewList validates required config and applies naming defaults.
func TestNewListConfig(t *testing.T) {
	if _, err := NewList(&fakeListClient{}, testLogger(), ListConfig{}); err == nil {
		t.Fatal("missing list must error")
	}
	l, err := NewList(&fakeListClient{}, testLogger(), ListConfig{List: "jobs"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Name() != "redis-list:jobs" {
		t.Fatalf("Name() = %q, want redis-list:jobs", l.Name())
	}
	if l.cfg.Processing != "jobs:processing" {
		t.Fatalf("Processing = %q, want jobs:processing", l.cfg.Processing)
	}
}
