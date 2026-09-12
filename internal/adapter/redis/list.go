package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/stump-wtf/switchboard/internal/adapter"
)

// ListClient is the narrow subset of go-redis commands the reliable-list adapter uses;
// *goredis.Client satisfies it, and tests fake it with goredis.New*Result values.
type ListClient interface {
	BRPopLPush(ctx context.Context, source, destination string, timeout time.Duration) *goredis.StringCmd
	LRange(ctx context.Context, key string, start, stop int64) *goredis.StringSliceCmd
	LRem(ctx context.Context, key string, count int64, value interface{}) *goredis.IntCmd
}

// ListConfig configures a reliable-list adapter.
type ListConfig struct {
	// Name is the adapter's registry name (adapters table primary key). Defaults to
	// "redis-list:{list}".
	Name string
	// List is the source list key to consume. Required.
	List string
	// Processing is the per-consumer processing list BRPOPLPUSH parks in-flight messages on.
	// Defaults to "{list}:processing".
	Processing string
	// TrustDetail names the broker/ACL identity the connection authenticated as; persisted as the
	// event's verify_detail.
	TrustDetail string
	// Block bounds each BRPOPLPUSH wait. Defaults to 5s — bounded (never 0/forever) so the loop
	// stays responsive to context cancellation.
	Block time.Duration
}

// List consumes a Redis list via the reliable-queue pattern — the acceptable ack-capable
// alternative to streams: BRPOPLPUSH parks each message on a processing list, the sink stores the
// todo durably, and only then LREM removes it from the processing list (the "ack"). A crash before
// store leaves the message on the processing list; the recovery pass at the next startup
// re-delivers it and the body-hash idempotency key (`redis:{list}:sha256(body)` — a list carries
// no per-message id) dedups it to exactly one todo.
//
// Governing: ADR-0014 (reliable list: BRPOPLPUSH + LREM after store),
// SPEC-0002 REQ "Store-Then-Ack Coupling", REQ "Redis Reference Transport Modes".
type List struct {
	client ListClient
	log    *slog.Logger
	cfg    ListConfig
	now    func() time.Time
}

var _ adapter.Adapter = (*List)(nil)

// NewList builds a reliable-list adapter, applying defaults.
func NewList(client ListClient, log *slog.Logger, cfg ListConfig) (*List, error) {
	if cfg.List == "" {
		return nil, errors.New("redis: list adapter requires a source list")
	}
	if cfg.Name == "" {
		cfg.Name = "redis-list:" + cfg.List
	}
	if cfg.Processing == "" {
		cfg.Processing = cfg.List + ":processing"
	}
	if cfg.Block <= 0 {
		cfg.Block = 5 * time.Second
	}
	return &List{client: client, log: log, cfg: cfg, now: time.Now}, nil
}

// Name is the adapter's registry name.
func (l *List) Name() string { return l.cfg.Name }

// TrustDetail names the broker/ACL identity standing in for per-message verification (ADR-0003).
func (l *List) TrustDetail() string { return l.cfg.TrustDetail }

// Consume first recovers messages stranded on the processing list (consumed before a crash but
// never removed because the todo never durably stored), then loops BRPOPLPUSH → deliver → LREM.
// The LREM ack happens only after Deliver returns nil, i.e. only after the todo is durably stored
// (store-then-ack). It blocks until ctx is cancelled; on cancellation it stops consuming and
// returns, leaving any in-flight-but-unstored message on the processing list for redelivery.
func (l *List) Consume(ctx context.Context, sink adapter.Sink) error {
	if err := l.recover(ctx, sink); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := l.client.BRPopLPush(ctx, l.cfg.List, l.cfg.Processing, l.cfg.Block).Result()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, goredis.Nil) {
				continue // block window elapsed with nothing new
			}
			return fmt.Errorf("redis list %q: brpoplpush: %w", l.cfg.List, err)
		}
		l.handle(ctx, sink, body)
	}
}

// recover re-delivers messages left on the processing list by a prior crash between consume and
// durable store — the redelivery half of store-then-ack for the list mode. Oldest first:
// BRPOPLPUSH LPUSHes to the head of the processing list, so the tail is the oldest.
func (l *List) recover(ctx context.Context, sink adapter.Sink) error {
	stranded, err := l.client.LRange(ctx, l.cfg.Processing, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("redis list %q: recover processing list %q: %w", l.cfg.List, l.cfg.Processing, err)
	}
	for i := len(stranded) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.handle(ctx, sink, stranded[i])
	}
	return nil
}

// handle runs the store-then-ack sequence for one message: deliver (todo durably stored) THEN
// remove it from the processing list.
func (l *List) handle(ctx context.Context, sink adapter.Sink, body string) {
	env := adapter.Envelope{
		Source: Source,
		Name:   l.cfg.List,
		// A list carries no per-message id: ExternalID stays empty and the idempotency key falls
		// back to redis:{list}:sha256(body) (SPEC-0002 REQ "Idempotency Key From Source Message Id").
		Payload:    []byte(body),
		ReceivedAt: l.now(),
	}
	if err := sink.Deliver(ctx, env); err != nil {
		// Store-then-ack: NO removal on a store failure. The message stays on the processing list
		// and the recovery pass redelivers it; the body-hash key dedups it to one todo.
		// Governing: SPEC-0002 REQ "Store-Then-Ack Coupling" (scenario "Store failure leaves
		// message for redelivery").
		l.log.Error("redis list deliver failed; leaving message on processing list for redelivery",
			"adapter", l.cfg.Name, "list", l.cfg.List, "processing", l.cfg.Processing, "err", err)
		return
	}
	// The todo is durably stored — durability reached, so removing the message is now safe.
	// count -1 removes the occurrence nearest the tail (the oldest — the one just handled).
	if err := l.client.LRem(ctx, l.cfg.Processing, -1, body).Err(); err != nil {
		// Removal failed AFTER durable store: the message will redeliver via recovery and the
		// body-hash key will dedup it to the already-stored todo. Safe, so log-and-continue.
		l.log.Warn("redis list ack (LREM) failed after durable store; redelivery will dedup",
			"adapter", l.cfg.Name, "list", l.cfg.List, "processing", l.cfg.Processing, "err", err)
	}
}
