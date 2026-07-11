// Package redis implements the reference pull-family transports (ADR-0014, SPEC-0002): the Redis
// front halves that consume messages, wrap each as an adapter.Envelope, and hand it to the shared
// back half behind adapter.Sink. Three modes, per SPEC-0002 REQ "Redis Reference Transport Modes":
//
//   - Stream — streams + consumer groups (XREADGROUP … XACK after store). Per-message ack and
//     redelivery on restart. RECOMMENDED for durable work.
//   - List — reliable list (BRPOPLPUSH onto a processing list, LREM after store). An acceptable
//     ack-capable alternative.
//   - PubSub — subscribe only. Fire-and-forget: there is NO ack and NO redelivery, so a message
//     whose todo never stores is lost. Loss-tolerant work only — MUST NOT be selected for work
//     where message loss is unacceptable.
//
// The store-then-ack coupling lives here, on the transport side of the seam: an adapter issues its
// source ack (XACK / LREM) only after Sink.Deliver returns nil — and Deliver returning nil means
// the todo is durably stored in PostgreSQL. A crash or a Deliver failure between consume and store
// leaves the source message un-acked, the broker redelivers it, and the idempotency key (derived
// from the stream entry id, or sha256(body) for list/pub-sub) dedups it to exactly one todo.
//
// Trust is the broker connection itself (auth/ACL + TLS, ADR-0003): the DSN comes from
// SWITCHBOARD_REDIS_URL (internal/config) and is never logged.
//
// Governing: ADR-0014 (store-then-ack, Redis reference transport modes),
// SPEC-0002 REQ "Store-Then-Ack Coupling", REQ "Redis Reference Transport Modes".
package redis

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"
)

// Source is the adapter family member stamped on every envelope this package produces. It becomes
// the event/todo source and the leading segment of every idempotency key (`redis:{name}:{id}`).
const Source = "redis"

// NewClient parses a Redis DSN (redis:// or rediss://, e.g. SWITCHBOARD_REDIS_URL) into a client
// that satisfies StreamClient and ListClient. The DSN may embed credentials, so it is never logged
// and never included in error text: go-redis's ParseURL (via net/url) embeds the raw URL in its
// parse errors, so the DSN is redacted from the returned error before it can reach a log line.
//
// Governing: SPEC-0002 REQ "Error Handling Standards" (never broker credentials).
func NewClient(dsn string) (*goredis.Client, error) {
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		return nil, errors.New("redis: parse dsn: " + redactDSN(err.Error(), dsn))
	}
	return goredis.NewClient(opts), nil
}

// redactDSN strips every occurrence of the DSN from an error message — both raw and in the
// strconv.Quote form net/url uses when embedding the URL in *url.Error text — so a malformed DSN
// carrying credentials can never leak into logs via a wrapped parse error.
func redactDSN(msg, dsn string) string {
	if dsn == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, strconv.Quote(dsn), `"[redacted]"`)
	return strings.ReplaceAll(msg, dsn, "[redacted]")
}

// payloadField is the stream entry field treated as the raw message body when present.
const payloadField = "payload"

// streamPayload extracts the message body from a stream entry's field-value map: the "payload"
// field verbatim when present (the conventional single-body shape), otherwise the JSON encoding of
// the whole map (deterministic — encoding/json sorts map keys — so a redelivered entry derives an
// identical body).
func streamPayload(values map[string]any) []byte {
	if v, ok := values[payloadField]; ok {
		if s, ok := v.(string); ok {
			return []byte(s)
		}
	}
	b, err := json.Marshal(values)
	if err != nil {
		// map[string]any from the Redis protocol only ever holds strings; unreachable in practice.
		return nil
	}
	return b
}
