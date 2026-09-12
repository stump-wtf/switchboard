package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// Sentinel validation errors, so callers can distinguish which envelope invariant failed.
var (
	// ErrNoSource means the envelope has no source (the adapter family member, e.g. "redis").
	ErrNoSource = errors.New("adapter: envelope has no source")
	// ErrNoName means the envelope has no sub-source name (stream/list/channel name).
	ErrNoName = errors.New("adapter: envelope has no name")
	// ErrNoIdentity means the envelope has neither a native message id nor a payload, so no
	// idempotency key can be derived for it.
	ErrNoIdentity = errors.New("adapter: envelope has neither external id nor payload")
	// ErrNoReceivedAt means the envelope's receipt time was never stamped.
	ErrNoReceivedAt = errors.New("adapter: envelope has no received-at time")
)

// Envelope is the normalized message a pull adapter hands to the shared back half — the pull
// family's analogue of an inbound webhook request. The transport front half fills it at consume
// time; everything downstream (idempotency key, event, todo) derives from it deterministically so a
// redelivered message maps onto the same todo.
//
// Governing: ADR-0014 (one back-half contract), SPEC-0002 REQ "Adapter Interface and Trust Mode".
type Envelope struct {
	// Source is the adapter family member the message came from, e.g. "redis". It becomes the
	// event/todo source and namespaces the idempotency key so sources cannot collide.
	Source string
	// Name is the sub-source within the broker the message was consumed from: the stream, list, or
	// pub/sub channel name. It further namespaces the idempotency key.
	Name string
	// ExternalID is the broker's native per-message id (e.g. a Redis stream entry id like
	// "1719345600000-0"). Empty when the transport carries no per-message id (Redis list, pub/sub).
	ExternalID string
	// Payload is the raw message body.
	Payload []byte
	// ReceivedAt is when the adapter consumed the message.
	ReceivedAt time.Time
}

// Validate checks the envelope invariants the back half depends on. It returns a sentinel error for
// the first violated invariant, or nil for a well-formed envelope.
func (e Envelope) Validate() error {
	switch {
	case e.Source == "":
		return ErrNoSource
	case e.Name == "":
		return ErrNoName
	case e.ExternalID == "" && len(e.Payload) == 0:
		return ErrNoIdentity
	case e.ReceivedAt.IsZero():
		return ErrNoReceivedAt
	}
	return nil
}

// keySegmentEscaper makes the source and name segments of an idempotency key delimiter-safe: the
// segments are joined with ':' but a sub-source Name may itself contain ':' (e.g. a Redis stream
// named "app:deploys"), which would otherwise let (name "a:b", id "c") and (name "a", id "b:c")
// derive the same key within one source. Escaping '\' then ':' in the prefix segments makes the
// split unambiguous (the trailing id segment can stay raw — with unambiguous prefixes, equal keys
// imply equal source, name, and id). Names without ':' keep the spec's literal
// `redis:{stream}:{entry-id}` shape.
var keySegmentEscaper = strings.NewReplacer(`\`, `\\`, `:`, `\:`)

// IdempotencyKey derives the message's idempotency key from the broker's native message id where one
// exists — `{source}:{name}:{external-id}`, e.g. `redis:{stream}:{entry-id}` — and falls back to
// `{source}:{name}:sha256(payload)` where the transport carries no per-message id (Redis list,
// pub/sub). The source and name prefixes namespace the key per adapter instance and sub-source
// (delimiter-escaped, see keySegmentEscaper), so keys from different sources or sub-sources cannot
// collide. A redelivered message derives the same key and dedups to exactly one todo — the property
// that makes store-then-ack safe.
//
// Governing: ADR-0014 (idempotency key from source message id),
// SPEC-0002 REQ "Idempotency Key From Source Message Id".
func (e Envelope) IdempotencyKey() string {
	prefix := keySegmentEscaper.Replace(e.Source) + ":" + keySegmentEscaper.Replace(e.Name) + ":"
	if e.ExternalID != "" {
		return prefix + e.ExternalID
	}
	sum := sha256.Sum256(e.Payload)
	return prefix + hex.EncodeToString(sum[:])
}

// EventInput stamps the envelope with the pull family's fixed trust metadata — family='queue',
// trust_mode='queue', verified=false — and the given verify_detail naming the broker/ACL identity
// (Adapter.TrustDetail), producing the event row for history. The derived idempotency key doubles as
// the event's external id, so the events table's (source, external_id) dedupe collapses redeliveries
// exactly like the todo dedupe does.
//
// Governing: ADR-0003 (queue trust mode: trust = broker connection),
// SPEC-0002 REQ "Adapter Interface and Trust Mode".
func (e Envelope) EventInput(trustDetail string) store.EventInput {
	return store.EventInput{
		Source:       e.Source,
		Family:       Family,
		EventType:    e.Name,
		ExternalID:   e.IdempotencyKey(),
		TrustMode:    TrustMode,
		Verified:     false,
		VerifyDetail: trustDetail,
		Payload:      e.Payload,
	}
}

// TodoParams normalizes the envelope into the same CreateTodoParams shape the push family builds, so
// a pull delivery and a push delivery of the same logical event produce the identical todo shape.
// The idempotency key is derived from the source message id (with the body-hash fallback), and the
// shared back half dedups on (queue, idempotency_key) among non-terminal rows.
//
// Governing: ADR-0014 (identical todo shape across families),
// SPEC-0002 REQ "Adapter Interface and Trust Mode".
func (e Envelope) TodoParams(queue, kind, title string) store.CreateTodoParams {
	return store.CreateTodoParams{
		Queue:          queue,
		Source:         e.Source,
		Kind:           kind,
		Title:          title,
		Payload:        e.Payload,
		IdempotencyKey: e.IdempotencyKey(),
	}
}
