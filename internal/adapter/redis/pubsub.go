package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/joestump/switchboard/internal/adapter"
)

// PubSubConn is the narrow subset of *goredis.PubSub the pub/sub adapter uses.
type PubSubConn interface {
	Channel(opts ...goredis.ChannelOption) <-chan *goredis.Message
	Close() error
}

// Subscriber opens a pub/sub subscription; SubscriberClient adapts *goredis.Client to it, and
// tests fake it with an in-memory message channel.
type Subscriber interface {
	Subscribe(ctx context.Context, channels ...string) PubSubConn
}

// SubscriberClient adapts a *goredis.Client to the Subscriber seam (the concrete Subscribe returns
// the concrete *goredis.PubSub, which Go's interfaces don't lift automatically).
type SubscriberClient struct{ Client *goredis.Client }

// Subscribe opens the subscription on the underlying client.
func (s SubscriberClient) Subscribe(ctx context.Context, channels ...string) PubSubConn {
	return s.Client.Subscribe(ctx, channels...)
}

// PubSubConfig configures a pub/sub adapter.
type PubSubConfig struct {
	// Name is the adapter's registry name (adapters table primary key). Defaults to
	// "redis-pubsub:{channel}".
	Name string
	// Channel is the pub/sub channel to subscribe to. Required.
	Channel string
	// TrustDetail names the broker/ACL identity the connection authenticated as; persisted as the
	// event's verify_detail.
	TrustDetail string
}

// PubSub consumes a Redis pub/sub channel. FIRE-AND-FORGET — read this before choosing it:
//
//   - There is NO ack and NO redelivery. A message published while the adapter is down is never
//     seen; a message whose todo fails to store is LOST (logged, but gone).
//   - Store-then-ack therefore degenerates to store-only: the ack half does not exist on this
//     transport, which is exactly why it MUST NOT be selected for work where message loss is
//     unacceptable. Use Stream (recommended) or List for durable work.
//
// It exists for loss-tolerant fan-out sources only, and is documented as such per SPEC-0002
// (scenario "Pub/sub is loss-tolerant only").
//
// Governing: ADR-0014 (pub/sub: no redelivery, not recommended for durable work),
// SPEC-0002 REQ "Redis Reference Transport Modes".
type PubSub struct {
	sub Subscriber
	log *slog.Logger
	cfg PubSubConfig
	now func() time.Time
}

var _ adapter.Adapter = (*PubSub)(nil)

// NewPubSub builds a pub/sub adapter, applying defaults. See the PubSub doc for the loss caveat.
func NewPubSub(sub Subscriber, log *slog.Logger, cfg PubSubConfig) (*PubSub, error) {
	if cfg.Channel == "" {
		return nil, errors.New("redis: pub/sub adapter requires a channel")
	}
	if cfg.Name == "" {
		cfg.Name = "redis-pubsub:" + cfg.Channel
	}
	return &PubSub{sub: sub, log: log, cfg: cfg, now: time.Now}, nil
}

// Name is the adapter's registry name.
func (p *PubSub) Name() string { return p.cfg.Name }

// TrustDetail names the broker/ACL identity standing in for per-message verification (ADR-0003).
func (p *PubSub) TrustDetail() string { return p.cfg.TrustDetail }

// Consume subscribes and delivers each published message to the sink. There is no source ack: a
// Deliver failure means the message is lost (no redelivery exists on this transport) and is logged
// loudly. It blocks until ctx is cancelled.
func (p *PubSub) Consume(ctx context.Context, sink adapter.Sink) error {
	conn := p.sub.Subscribe(ctx, p.cfg.Channel)
	defer func() { _ = conn.Close() }()
	ch := conn.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("redis pubsub %q: subscription closed", p.cfg.Channel)
			}
			env := adapter.Envelope{
				Source: Source,
				Name:   p.cfg.Channel,
				// Pub/sub carries no per-message id: the idempotency key falls back to
				// redis:{channel}:sha256(body).
				Payload:    []byte(msg.Payload),
				ReceivedAt: p.now(),
			}
			if err := sink.Deliver(ctx, env); err != nil {
				// Fire-and-forget: no redelivery exists, so this message is LOST. That is the
				// documented pub/sub trade-off — durable work belongs on Stream or List.
				p.log.Error("redis pubsub deliver failed; message lost (pub/sub has no redelivery)",
					"adapter", p.cfg.Name, "channel", p.cfg.Channel, "err", err)
			}
		}
	}
}
