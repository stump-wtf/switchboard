package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/stump-wtf/switchboard/internal/adapter"
)

// StreamClient is the narrow subset of go-redis commands the stream adapter uses;
// *goredis.Client satisfies it, and tests fake it with goredis.New*Result values.
type StreamClient interface {
	XGroupCreateMkStream(ctx context.Context, stream, group, start string) *goredis.StatusCmd
	XReadGroup(ctx context.Context, a *goredis.XReadGroupArgs) *goredis.XStreamSliceCmd
	XAck(ctx context.Context, stream, group string, ids ...string) *goredis.IntCmd
}

// StreamConfig configures a streams + consumer-group adapter.
type StreamConfig struct {
	// Name is the adapter's registry name (adapters table primary key). Defaults to
	// "redis-stream:{stream}".
	Name string
	// Stream is the Redis stream key to consume. Required.
	Stream string
	// Group is the consumer group. Required — the group is what gives per-message ack + redelivery.
	Group string
	// Consumer is this instance's consumer name within the group. Required.
	Consumer string
	// TrustDetail names the broker/ACL identity the connection authenticated as (e.g.
	// "redis acl: deploy-bot"); it is persisted as the event's verify_detail.
	TrustDetail string
	// Block bounds each XREADGROUP wait for new entries. Defaults to 5s. A bounded block (never
	// BLOCK 0) keeps the loop responsive to context cancellation.
	Block time.Duration
	// Count caps entries per read. Defaults to 16.
	Count int64
}

// Stream consumes a Redis stream via a consumer group — the RECOMMENDED durable transport mode:
// XREADGROUP, deliver to the sink (todo durably stored), and only then XACK. On startup it first
// drains this consumer's pending entries (consumed-but-never-acked, i.e. a prior crash between
// consume and store), so redelivery needs no extra machinery: the un-acked entry is re-read and
// the idempotency key `redis:{stream}:{entry-id}` dedups it to exactly one todo.
//
// Governing: ADR-0014 (streams + consumer groups preferred, XACK after store),
// SPEC-0002 REQ "Store-Then-Ack Coupling", REQ "Redis Reference Transport Modes".
type Stream struct {
	client StreamClient
	log    *slog.Logger
	cfg    StreamConfig
	now    func() time.Time
}

var _ adapter.Adapter = (*Stream)(nil)

// NewStream builds a streams + consumer-group adapter, applying defaults.
func NewStream(client StreamClient, log *slog.Logger, cfg StreamConfig) (*Stream, error) {
	if cfg.Stream == "" || cfg.Group == "" || cfg.Consumer == "" {
		return nil, errors.New("redis: stream adapter requires stream, group, and consumer")
	}
	if cfg.Name == "" {
		cfg.Name = "redis-stream:" + cfg.Stream
	}
	if cfg.Block <= 0 {
		cfg.Block = 5 * time.Second
	}
	if cfg.Count <= 0 {
		cfg.Count = 16
	}
	return &Stream{client: client, log: log, cfg: cfg, now: time.Now}, nil
}

// Name is the adapter's registry name.
func (s *Stream) Name() string { return s.cfg.Name }

// TrustDetail names the broker/ACL identity standing in for per-message verification (ADR-0003).
func (s *Stream) TrustDetail() string { return s.cfg.TrustDetail }

// Consume reads entries and delivers each to the sink, acking (XACK) an entry only after Deliver
// returns nil — i.e. only after the todo is durably stored (store-then-ack). It blocks until ctx
// is cancelled; on cancellation it stops consuming and returns, leaving any in-flight-but-unstored
// entry pending (un-acked) so the group redelivers it.
func (s *Stream) Consume(ctx context.Context, sink adapter.Sink) error {
	if err := s.ensureGroup(ctx); err != nil {
		return err
	}
	// Phase 1: drain this consumer's pending entries — consumed before a crash but never acked
	// because the todo never durably stored. This IS the redelivery half of store-then-ack.
	// Phase 2 (cursor ">"): block for new entries.
	cursor := "0"
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		args := &goredis.XReadGroupArgs{
			Group:    s.cfg.Group,
			Consumer: s.cfg.Consumer,
			Streams:  []string{s.cfg.Stream, cursor},
			Count:    s.cfg.Count,
			Block:    -1, // pending drain: no BLOCK — an empty read ends phase 1
		}
		if cursor == ">" {
			args.Block = s.cfg.Block
		}
		streams, err := s.client.XReadGroup(ctx, args).Result()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, goredis.Nil) {
				// Block window elapsed with nothing new (or no pending backlog).
				if cursor != ">" {
					cursor = ">"
				}
				continue
			}
			return fmt.Errorf("redis stream %q: xreadgroup: %w", s.cfg.Stream, err)
		}
		delivered := 0
		for _, st := range streams {
			for _, msg := range st.Messages {
				if err := ctx.Err(); err != nil {
					// Shutdown mid-batch: the rest of the batch stays pending (un-acked) and
					// redelivers on restart.
					return err
				}
				s.handle(ctx, sink, msg)
				delivered++
				if cursor != ">" {
					// Advance the pending cursor past this entry so a Deliver failure cannot
					// spin phase 1 forever on one poison message; it stays pending for the
					// next restart.
					cursor = msg.ID
				}
			}
		}
		if cursor != ">" && delivered == 0 {
			cursor = ">" // pending backlog drained; switch to new entries
		}
	}
}

// handle runs the store-then-ack sequence for one entry: deliver (todo durably stored) THEN ack.
func (s *Stream) handle(ctx context.Context, sink adapter.Sink, msg goredis.XMessage) {
	env := adapter.Envelope{
		Source:     Source,
		Name:       s.cfg.Stream,
		ExternalID: msg.ID, // the native entry id → idempotency key redis:{stream}:{entry-id}
		Payload:    streamPayload(msg.Values),
		ReceivedAt: s.now(),
	}
	if err := sink.Deliver(ctx, env); err != nil {
		// Store-then-ack: NO ack on a store failure. The entry stays pending in the group, is
		// redelivered, and the entry-id idempotency key dedups it to one todo — never dropped.
		// Governing: SPEC-0002 REQ "Store-Then-Ack Coupling" (scenario "Store failure leaves
		// message for redelivery").
		s.log.Error("redis stream deliver failed; leaving entry un-acked for redelivery",
			"adapter", s.cfg.Name, "stream", s.cfg.Stream, "group", s.cfg.Group, "id", msg.ID, "err", err)
		return
	}
	// The todo is durably stored — durability reached, so the source ack is now safe.
	if err := s.client.XAck(ctx, s.cfg.Stream, s.cfg.Group, msg.ID).Err(); err != nil {
		// Ack failed AFTER durable store: the entry will redeliver and the idempotency key will
		// dedup it to the already-stored todo. Safe, so log-and-continue rather than crash.
		s.log.Warn("redis stream ack failed after durable store; redelivery will dedup",
			"adapter", s.cfg.Name, "stream", s.cfg.Stream, "group", s.cfg.Group, "id", msg.ID, "err", err)
	}
}

// ensureGroup creates the consumer group (and the stream, MKSTREAM) if missing; an existing group
// (BUSYGROUP) is fine — creation is idempotent across restarts.
func (s *Stream) ensureGroup(ctx context.Context) error {
	err := s.client.XGroupCreateMkStream(ctx, s.cfg.Stream, s.cfg.Group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("redis stream %q: create group %q: %w", s.cfg.Stream, s.cfg.Group, err)
	}
	return nil
}
