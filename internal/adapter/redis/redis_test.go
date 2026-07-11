package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/joestump/switchboard/internal/adapter"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// opLog records the interleaving of sink deliveries and source acks, so tests can assert the
// store-then-ack ordering: the ack for a message appears strictly AFTER its successful delivery.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ops...)
}

// fakeSink is a recording adapter.Sink standing in for the shared back half. Deliver returning nil
// models "the todo is durably stored in PostgreSQL"; a scripted error models a store failure.
type fakeSink struct {
	ops  *opLog
	mu   sync.Mutex
	envs []adapter.Envelope
	// failures maps an idempotency key to how many times Deliver should fail for it before
	// succeeding — modeling a store failure that clears on redelivery.
	failures map[string]int
	err      error // error returned while failures remain
}

func (s *fakeSink) Deliver(ctx context.Context, env adapter.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The real Sink validates first (adapter.Sink contract); a transport handing over an invalid
	// envelope is a transport bug, so surface it loudly here.
	if err := env.Validate(); err != nil {
		panic("transport produced an invalid envelope: " + err.Error())
	}
	s.envs = append(s.envs, env)
	key := env.IdempotencyKey()
	if s.failures[key] > 0 {
		s.failures[key]--
		s.ops.add("deliver-fail:" + key)
		return s.err
	}
	s.ops.add("deliver:" + key)
	return nil
}

func (s *fakeSink) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.envs))
	for i, e := range s.envs {
		out[i] = e.IdempotencyKey()
	}
	return out
}

// bodyKey is the expected body-hash fallback key for an id-less transport (list, pub/sub).
func bodyKey(name, body string) string {
	sum := sha256.Sum256([]byte(body))
	return Source + ":" + name + ":" + hex.EncodeToString(sum[:])
}

func TestStreamPayloadExtraction(t *testing.T) {
	// Conventional single-body shape: the "payload" field verbatim.
	if got := streamPayload(map[string]any{"payload": `{"job":"deploy"}`}); string(got) != `{"job":"deploy"}` {
		t.Fatalf("payload field: got %q", got)
	}
	// No payload field: deterministic JSON of the whole map, so a redelivered entry derives an
	// identical body (and thus the same body-hash fallback key on id-less transports).
	got := streamPayload(map[string]any{"b": "2", "a": "1"})
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil || m["a"] != "1" || m["b"] != "2" {
		t.Fatalf("field map: got %q (err %v)", got, err)
	}
	if string(got) != string(streamPayload(map[string]any{"a": "1", "b": "2"})) {
		t.Fatal("field-map encoding must be deterministic across redeliveries")
	}
}

func TestNewClientParsesDSN(t *testing.T) {
	c, err := NewClient("redis://:secret@127.0.0.1:6379/1")
	if err != nil || c == nil {
		t.Fatalf("valid DSN: err %v", err)
	}
	if _, err := NewClient("http://not-redis"); err == nil {
		t.Fatal("invalid DSN must error")
	}
}
