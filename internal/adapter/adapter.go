// Package adapter defines the pull-family ingestion adapter contract (ADR-0014, SPEC-0002): a
// uniform interface that consumes messages from an external broker (Redis is the reference; SQS,
// NATS, AMQP later) and feeds them into the same shared back half the push (webhook) family uses —
// verify → derive idempotency key → normalize → create todo (dedup) → persist event — so a push
// delivery and a pull delivery of the same logical event produce the identical todo shape.
//
// This package owns the contract only: the Adapter interface, the message Envelope, queue trust-mode
// stamping (ADR-0003), and idempotency-key derivation. Concrete transports (the Redis reference
// adapter), the poll-loop lifecycle, and the store-then-ack enqueue coupling are built on top of it.
package adapter

import "context"

// Trust-mode constants for the pull family (ADR-0003): a queue message carries no per-message
// signature — trust is the broker connection itself (auth/ACL + TLS) — so every pull-ingested event
// is stamped family='queue', trust_mode='queue', verified=false.
const (
	// Family is the adapters-table / events-table family for every pull adapter.
	Family = "queue"
	// TrustMode is the trust mode for every pull-ingested event.
	TrustMode = "queue"
)

// Adapter is the uniform pull-adapter interface (SPEC-0002 REQ "Adapter Interface and Trust Mode").
// A transport implements the broker-specific front half (consume) and hands every message to the
// Sink as a normalized Envelope; the shared back half behind the Sink is identical to the push
// family's, so the two families cannot drift.
//
// Governing: ADR-0014 (push/pull adapter families, one back-half contract),
// SPEC-0002 REQ "Adapter Interface and Trust Mode".
type Adapter interface {
	// Name is the adapter's registry name — its primary key in the adapters table. The poll-loop
	// lifecycle checks the registry row's enabled flag under this name before consuming.
	Name() string

	// TrustDetail names the broker/ACL identity the connection authenticated as (e.g.
	// "redis acl: deploy-bot"). It is persisted as the event's verify_detail so an operator can see
	// what stood in for per-message verification.
	TrustDetail() string

	// Consume runs the transport front half: read messages from the broker, wrap each as an
	// Envelope, and pass it to sink.Deliver. Consume MUST NOT acknowledge or remove a source
	// message until Deliver returns nil — Deliver returning nil means the todo is durably stored,
	// and that durability is the queue's ack (store-then-ack, ADR-0014). Consume blocks until ctx
	// is cancelled or an unrecoverable transport error occurs; on cancellation it stops consuming,
	// leaves any in-flight-but-undelivered message un-acked (so the broker redelivers it), and
	// returns.
	Consume(ctx context.Context, sink Sink) error
}

// Sink is the shared back half a pull adapter feeds: it verifies (trust = the connection), derives
// the idempotency key, normalizes, creates the todo (dedup), and persists the event — atomically,
// like the push family's ingest path. Deliver returns nil only once the todo is durably stored in
// PostgreSQL, so the adapter may then (and only then) ack the source message. The concrete
// implementation is the enqueue coupling story; this package defines the seam.
//
// Governing: ADR-0014 (store-then-ack), SPEC-0002 REQ "Adapter Interface and Trust Mode".
type Sink interface {
	// Deliver runs the shared back half for one envelope. Implementations MUST call env.Validate()
	// first and reject invalid envelopes with the sentinel error, before deriving anything from the
	// envelope: IdempotencyKey()/EventInput() do not validate, so an unvalidated envelope with an
	// empty name or payload would still derive a (degenerate) key such as
	// `source::sha256("")` and could be stored as a bogus todo/event. Validate-then-derive is the
	// invariant the enqueue coupling (the concrete Sink) inherits.
	Deliver(ctx context.Context, env Envelope) error
}
