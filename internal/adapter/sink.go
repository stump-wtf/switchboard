package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// DefaultDeliverTimeout bounds each Deliver's event-insert + todo-create transaction when
// StoreSinkConfig.Timeout is unset. The bound exists so a stalled database connection surfaces as a
// wrapped error (message left un-acked, broker redelivers) rather than hanging the adapter's poll
// loop forever. Governing: SPEC-0002 REQ "Enqueue Consumed Message as Todo — Database Operation
// Standards" (explicit connection lifecycle with timeouts).
const DefaultDeliverTimeout = 10 * time.Second

// EventTodoCreator is the narrow slice of the store the enqueue sink needs: the atomic
// event-insert + todo-create transaction (parameterized queries only; commits together or not at
// all). *store.Store satisfies it; tests fake it in memory.
type EventTodoCreator interface {
	CreateEventTodo(ctx context.Context, e store.EventInput, p store.CreateTodoParams) (int64, store.Todo, bool, error)
}

// StoreSinkConfig configures one adapter's StoreSink.
type StoreSinkConfig struct {
	// Queue is the target todo queue. Empty routes each message to its envelope Name (the
	// stream/list/channel it was consumed from), mirroring the generic push provider's
	// queue-defaults-to-name behavior.
	Queue string
	// TrustDetail names the broker/ACL identity the adapter's connection authenticated as (e.g.
	// "redis acl: deploy-bot" — Adapter.TrustDetail). Required: it is persisted as the event's
	// verify_detail, the operator-visible record of what stood in for per-message verification.
	// Governing: ADR-0003 (queue trust mode), SPEC-0002 REQ "Adapter Interface and Trust Mode".
	TrustDetail string
	// Timeout bounds each Deliver's database transaction. Defaults to DefaultDeliverTimeout.
	Timeout time.Duration
	// EndpointID is the vended endpoint that owns every todo this sink creates. Required: every
	// todo is pinned to exactly one endpoint for its whole lifecycle (ADR-0022; todos.endpoint_id
	// is NOT NULL with no sentinel), and a queue adapter has no per-message tenant to derive one
	// from — the broker connection authenticates the ADAPTER, not a principal.
	//
	// INTERIM, REVISITED IN PR 2 alongside the operator-configured webhook receivers: it is
	// supplied per adapter registry row (`endpoint_id` in the row config), falling back to the
	// server's operator-designated legacy endpoint. Deriving it from the target queue name instead
	// would reinstate the cross-tenant queue-string collision ADR-0022 exists to remove, so it is
	// stated explicitly or the adapter does not start.
	// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
	EndpointID string
}

// todoKind is the todo kind stamped on every queue-ingested todo — the pull analogue of the generic
// push provider's fixed "webhook" kind. A queue message carries no provider event type; the
// envelope's Name (already the event row's event_type) says where it came from.
const todoKind = "message"

// StoreSink is the concrete Sink: the enqueue coupling that wires a consumed envelope into the
// store's atomic event-insert + todo-create transaction. It is the shared back half every pull
// transport feeds — verify (trust = the broker connection, stamped by Envelope.EventInput) → derive
// idempotency key → normalize → create todo (dedup) → persist event — and its nil return is the
// transport's license to ack: Deliver returns nil only after the transaction is confirmed
// committed, so a source message is never acked ahead of the todo's durability (store-then-ack).
//
// Governing: ADR-0014 (one back-half contract, store-then-ack),
// SPEC-0002 REQ "Enqueue Consumed Message as Todo — Database Operation Standards",
// REQ "Error Handling Standards".
type StoreSink struct {
	store EventTodoCreator
	log   *slog.Logger
	cfg   StoreSinkConfig
}

var _ Sink = (*StoreSink)(nil)

// NewStoreSink builds the concrete enqueue sink over the given store slice, applying defaults. The
// trust detail is required so no queue-ingested event can persist without naming the broker/ACL
// identity that stood in for verification.
func NewStoreSink(st EventTodoCreator, log *slog.Logger, cfg StoreSinkConfig) (*StoreSink, error) {
	if st == nil {
		return nil, errors.New("adapter: store sink requires a store")
	}
	if cfg.TrustDetail == "" {
		return nil, errors.New("adapter: store sink requires a trust detail (broker/ACL identity)")
	}
	// Fail at construction, not at the first message: an endpoint-less sink would 23502 on every
	// insert, leaving every consumed message un-acked and redelivered forever. The caller treats
	// this as fail-soft-per-row — the adapter stays dark and loudly logged (ADR-0022).
	if cfg.EndpointID == "" {
		return nil, errors.New("adapter: store sink requires an owning endpoint id")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultDeliverTimeout
	}
	return &StoreSink{store: st, log: log, cfg: cfg}, nil
}

// Deliver runs the shared back half for one envelope and returns nil only once the todo is durably
// committed in PostgreSQL — the transport acks its source message on nil, and ONLY on nil.
//
// Error handling per SPEC-0002 REQ "Error Handling Standards": every failure is wrapped with the
// normalize/insert/create boundary it crossed and returned (never swallowed), so the transport
// leaves the source message un-acked and the broker redelivers it — a failure between consume and
// ack can never silently drop a message. Log lines carry structured context (source, name, message
// id) and never broker credentials — the envelope holds none.
func (s *StoreSink) Deliver(ctx context.Context, env Envelope) error {
	// Normalize boundary — validate BEFORE deriving anything. IdempotencyKey()/EventInput() do not
	// validate, so an unvalidated envelope with an empty name or payload would still derive a
	// degenerate key such as `source::sha256("")` and could be stored as a bogus todo/event. The
	// sentinel (ErrNoSource et al.) stays unwrappable via errors.Is so callers can distinguish which
	// envelope invariant failed. Governing: the Sink contract's validate-then-derive invariant,
	// SPEC-0002 REQ "Error Handling Standards" (sentinel errors, wrapped with context).
	if err := env.Validate(); err != nil {
		return fmt.Errorf("adapter sink: reject envelope from %s:%s: %w", env.Source, env.Name, err)
	}

	queue := s.cfg.Queue
	if queue == "" {
		queue = env.Name
	}

	// Database boundary — one atomic transaction (CreateEventTodo: parameterized queries only, event
	// insert + todo create commit together or not at all) under an explicit bounded timeout, so a
	// stalled connection becomes a redelivery, not a hung poll loop. Governing: SPEC-0002 REQ
	// "Enqueue Consumed Message as Todo — Database Operation Standards" (scenario "Todo persisted
	// with parameterized queries before ack").
	dctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	_, td, created, err := s.store.CreateEventTodo(dctx,
		env.EventInput(s.cfg.TrustDetail),
		s.owned(env.TodoParams(queue, todoKind, summarizeEnvelope(env))))
	if err != nil {
		// Wrapped and surfaced: the transport logs it and leaves the message un-acked, so the broker
		// redelivers (scenario "Store failure leaves message for redelivery"). A partial failure
		// cannot have persisted anything — the transaction rolled back.
		return fmt.Errorf("adapter sink: enqueue %s:%s message: create event+todo: %w", env.Source, env.Name, err)
	}

	if created {
		s.log.Info("queue message enqueued as todo",
			"source", env.Source, "name", env.Name, "message_id", env.ExternalID,
			"todo", td.ID, "queue", td.Queue)
	} else {
		// A redelivery collapsed onto the existing non-terminal todo — the dedup half of
		// store-then-ack. Returning nil lets the transport ack the redelivered copy.
		s.log.Debug("queue redelivery collapsed by idempotency key",
			"source", env.Source, "name", env.Name, "message_id", env.ExternalID,
			"todo", td.ID, "idempotency_key", td.IdempotencyKey)
	}
	return nil
}

// owned pins the derived todo params to this sink's owning endpoint. Envelope.TodoParams knows
// only the message, never the tenant, so ownership is stamped here — the single place a queue
// adapter's tenancy is decided. Governing: ADR-0022.
func (s *StoreSink) owned(p store.CreateTodoParams) store.CreateTodoParams {
	p.EndpointID = s.cfg.EndpointID
	return p
}

// summarizeEnvelope builds a one-line, legible todo title for a queue-ingested message — the pull
// analogue of the push family's summarize helpers.
func summarizeEnvelope(env Envelope) string {
	return "queue " + env.Source + ":" + env.Name + " message"
}
