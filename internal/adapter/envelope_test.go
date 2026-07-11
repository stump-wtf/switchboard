package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func validEnvelope() Envelope {
	return Envelope{
		Source:     "redis",
		Name:       "deploys",
		ExternalID: "1719345600000-0",
		Payload:    []byte(`{"job":"deploy"}`),
		ReceivedAt: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
}

func TestEnvelopeValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Envelope)
		want   error
	}{
		{"valid", func(e *Envelope) {}, nil},
		{"valid without external id (payload identity)", func(e *Envelope) { e.ExternalID = "" }, nil},
		{"valid without payload (native id identity)", func(e *Envelope) { e.Payload = nil }, nil},
		{"missing source", func(e *Envelope) { e.Source = "" }, ErrNoSource},
		{"missing name", func(e *Envelope) { e.Name = "" }, ErrNoName},
		{"missing id and payload", func(e *Envelope) { e.ExternalID = ""; e.Payload = nil }, ErrNoIdentity},
		{"missing received-at", func(e *Envelope) { e.ReceivedAt = time.Time{} }, ErrNoReceivedAt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnvelope()
			tc.mutate(&e)
			if err := e.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// SPEC-0002 REQ "Idempotency Key From Source Message Id": the native id becomes the key, namespaced
// redis:{stream}:{entry-id}.
func TestIdempotencyKeyFromNativeID(t *testing.T) {
	e := validEnvelope()
	if got, want := e.IdempotencyKey(), "redis:deploys:1719345600000-0"; got != want {
		t.Fatalf("IdempotencyKey() = %q, want %q", got, want)
	}
	// A redelivery (same envelope) derives the identical key — the dedup property.
	if e.IdempotencyKey() != validEnvelope().IdempotencyKey() {
		t.Fatal("redelivered envelope must derive the same key")
	}
}

// SPEC-0002 scenario "List/pub-sub message uses a body hash": with no per-message id the key falls
// back to redis:{name}:sha256(body).
func TestIdempotencyKeyBodyHashFallback(t *testing.T) {
	e := validEnvelope()
	e.ExternalID = ""
	sum := sha256.Sum256(e.Payload)
	want := "redis:deploys:" + hex.EncodeToString(sum[:])
	if got := e.IdempotencyKey(); got != want {
		t.Fatalf("IdempotencyKey() = %q, want %q", got, want)
	}
	// Different body ⇒ different key.
	e2 := e
	e2.Payload = []byte(`{"job":"other"}`)
	if e2.IdempotencyKey() == e.IdempotencyKey() {
		t.Fatal("different bodies must derive different keys")
	}
}

// SPEC-0002 REQ "Idempotency Key From Source Message Id": keys are namespaced by adapter source and
// sub-source, so identical ids/bodies from different sources cannot collide.
func TestIdempotencyKeyNamespacing(t *testing.T) {
	base := validEnvelope()

	otherName := base
	otherName.Name = "alerts"
	if otherName.IdempotencyKey() == base.IdempotencyKey() {
		t.Fatal("same entry id on a different stream must not collide")
	}

	otherSource := base
	otherSource.Source = "sqs"
	if otherSource.IdempotencyKey() == base.IdempotencyKey() {
		t.Fatal("same entry id from a different source must not collide")
	}

	// Body-hash fallback is namespaced too.
	a, b := base, base
	a.ExternalID, b.ExternalID = "", ""
	b.Name = "alerts"
	if a.IdempotencyKey() == b.IdempotencyKey() {
		t.Fatal("identical bodies on different names must not collide")
	}
}

// SPEC-0002 REQ "Idempotency Key From Source Message Id": a sub-source Name may itself contain the
// ':' join delimiter (e.g. a Redis stream named "app:deploys"). The escaped prefix segments keep
// (name "a:b", id "c") and (name "a", id "b:c") from deriving the same key.
func TestIdempotencyKeyDelimiterInName(t *testing.T) {
	a := validEnvelope()
	a.Name, a.ExternalID = "a:b", "c"

	b := validEnvelope()
	b.Name, b.ExternalID = "a", "b:c"

	if a.IdempotencyKey() == b.IdempotencyKey() {
		t.Fatalf("delimiter ambiguity: %q collides across (name,id) boundaries", a.IdempotencyKey())
	}

	// Plain names (no delimiter) keep the spec's literal redis:{stream}:{entry-id} shape.
	if got, want := validEnvelope().IdempotencyKey(), "redis:deploys:1719345600000-0"; got != want {
		t.Fatalf("plain-name key = %q, want %q", got, want)
	}

	// Escaping is deterministic: a redelivery of the colon-named message derives the same key.
	a2 := validEnvelope()
	a2.Name, a2.ExternalID = "a:b", "c"
	if a.IdempotencyKey() != a2.IdempotencyKey() {
		t.Fatal("redelivered colon-named envelope must derive the same key")
	}
}

// SPEC-0002 REQ "Adapter Interface and Trust Mode": a pull-ingested event persists family='queue',
// trust_mode='queue', verified=false, and a verify_detail naming the broker/ACL identity.
func TestEventInputTrustStamping(t *testing.T) {
	e := validEnvelope()
	in := e.EventInput("redis acl: deploy-bot")

	if in.Family != "queue" {
		t.Errorf("Family = %q, want %q", in.Family, "queue")
	}
	if in.TrustMode != "queue" {
		t.Errorf("TrustMode = %q, want %q", in.TrustMode, "queue")
	}
	if in.Verified {
		t.Error("Verified = true, want false (queue trust has no per-message verification)")
	}
	if in.VerifyDetail != "redis acl: deploy-bot" {
		t.Errorf("VerifyDetail = %q, want the broker/ACL identity", in.VerifyDetail)
	}
	if in.Source != "redis" || in.EventType != "deploys" {
		t.Errorf("Source/EventType = %q/%q, want redis/deploys", in.Source, in.EventType)
	}
	if in.ExternalID != e.IdempotencyKey() {
		t.Errorf("ExternalID = %q, want the derived idempotency key %q", in.ExternalID, e.IdempotencyKey())
	}
	if string(in.Payload) != string(e.Payload) {
		t.Error("Payload must pass through unchanged")
	}
}

// SPEC-0002 REQ "Adapter Interface and Trust Mode": the envelope normalizes into the same
// CreateTodoParams shape the push family builds, so pull and push yield the identical todo shape.
func TestTodoParamsSharedShape(t *testing.T) {
	e := validEnvelope()
	p := e.TodoParams("deploys", "deploy.requested", "Deploy requested")

	if p.Queue != "deploys" || p.Kind != "deploy.requested" || p.Title != "Deploy requested" {
		t.Errorf("Queue/Kind/Title = %q/%q/%q", p.Queue, p.Kind, p.Title)
	}
	if p.Source != "redis" {
		t.Errorf("Source = %q, want redis", p.Source)
	}
	if p.IdempotencyKey != e.IdempotencyKey() {
		t.Errorf("IdempotencyKey = %q, want %q", p.IdempotencyKey, e.IdempotencyKey())
	}
	if string(p.Payload) != string(e.Payload) {
		t.Error("Payload must pass through unchanged")
	}
}
