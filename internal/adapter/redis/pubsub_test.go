package redis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// fakePubSubConn feeds scripted messages through an in-memory channel.
type fakePubSubConn struct {
	ch     chan *goredis.Message
	closed bool
}

func (f *fakePubSubConn) Channel(opts ...goredis.ChannelOption) <-chan *goredis.Message {
	return f.ch
}

func (f *fakePubSubConn) Close() error {
	f.closed = true
	return nil
}

type fakeSubscriber struct {
	conn     *fakePubSubConn
	channels []string
}

func (f *fakeSubscriber) Subscribe(ctx context.Context, channels ...string) PubSubConn {
	f.channels = channels
	return f.conn
}

func newPubSubHarness(t *testing.T, failures map[string]int) (*PubSub, *fakeSubscriber, *fakeSink, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub := &fakeSubscriber{conn: &fakePubSubConn{ch: make(chan *goredis.Message, 8)}}
	sink := &fakeSink{ops: &opLog{}, failures: failures, err: errors.New("store failed")}
	p, err := NewPubSub(sub, testLogger(), PubSubConfig{Channel: "alerts", TrustDetail: "redis acl: alert-bot"})
	if err != nil {
		t.Fatal(err)
	}
	return p, sub, sink, ctx, cancel
}

// SPEC-0002 scenario "Pub/sub is loss-tolerant only": messages deliver with body-hash keys, there
// is no ack primitive at all, and a store failure means the message is simply lost (logged) — the
// documented fire-and-forget trade-off that bars pub/sub from durable work.
func TestPubSubDeliversFireAndForget(t *testing.T) {
	p, sub, sink, ctx, cancel := newPubSubHarness(t, nil)
	sub.conn.ch <- &goredis.Message{Channel: "alerts", Payload: `{"sev":"low"}`}
	go func() {
		// After the first delivery is recorded, stop the loop like a shutdown would.
		for len(sink.keys()) == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	if err := p.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	if !sub.conn.closed {
		t.Fatal("subscription must be closed on return")
	}
	if len(sub.channels) != 1 || sub.channels[0] != "alerts" {
		t.Fatalf("subscribed channels = %v, want [alerts]", sub.channels)
	}

	env := sink.envs[0]
	if env.Source != Source || env.Name != "alerts" || env.ExternalID != "" {
		t.Fatalf("envelope = %+v, want redis/alerts with empty ExternalID (body-hash key)", env)
	}
	if !strings.HasPrefix(env.IdempotencyKey(), "redis:alerts:") {
		t.Fatalf("key = %q, want redis:alerts:sha256(body)", env.IdempotencyKey())
	}
}

// A store failure on pub/sub does not retry and does not crash the loop: the message is lost by
// design (no redelivery exists), which is exactly why durable work MUST NOT ride pub/sub.
func TestPubSubStoreFailureDropsMessage(t *testing.T) {
	body := `{"sev":"high"}`
	p, sub, sink, ctx, cancel := newPubSubHarness(t, map[string]int{bodyKey("alerts", body): 1})
	sub.conn.ch <- &goredis.Message{Channel: "alerts", Payload: body}
	sub.conn.ch <- &goredis.Message{Channel: "alerts", Payload: `{"sev":"low"}`}
	go func() {
		for len(sink.keys()) < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	if err := p.Consume(ctx, sink); !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	// Both messages were attempted exactly once each — no retry of the lost one.
	keys := sink.keys()
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("keys = %v, want two distinct single attempts", keys)
	}
}

// A closed subscription surfaces as a wrapped error (the lifecycle owns reconnect/backoff).
func TestPubSubClosedSubscription(t *testing.T) {
	p, sub, sink, ctx, _ := newPubSubHarness(t, nil)
	close(sub.conn.ch)

	err := p.Consume(ctx, sink)
	if err == nil || !strings.Contains(err.Error(), "subscription closed") {
		t.Fatalf("Consume = %v, want subscription-closed error", err)
	}
}

// NewPubSub validates required config and applies naming defaults.
func TestNewPubSubConfig(t *testing.T) {
	if _, err := NewPubSub(&fakeSubscriber{}, testLogger(), PubSubConfig{}); err == nil {
		t.Fatal("missing channel must error")
	}
	p, err := NewPubSub(&fakeSubscriber{}, testLogger(), PubSubConfig{Channel: "alerts"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "redis-pubsub:alerts" {
		t.Fatalf("Name() = %q, want redis-pubsub:alerts", p.Name())
	}
}
