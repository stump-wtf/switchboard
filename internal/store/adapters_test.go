package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// jsonEqual compares two JSON documents structurally. adapters.config is jsonb, and Postgres
// normalizes jsonb on output (whitespace, key order), so byte-equality against the input literal
// is not a valid assertion.
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want %q: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// SPEC-0002 REQ "Adapter Interface and Trust Mode": pull adapters are registered in the adapters
// table with family='queue' and honor the runtime enabled flag.
func TestAdapterRegistry(t *testing.T) {
	s, ctx := testStore(t)

	a, err := s.RegisterAdapter(ctx, "redis", "queue", "queue", []byte(`{"mode":"stream"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if a.Name != "redis" || a.Family != "queue" || a.TrustMode != "queue" {
		t.Fatalf("registered row wrong: %+v", a)
	}
	if !a.Enabled {
		t.Fatal("new adapter should default to enabled")
	}

	got, err := s.GetAdapter(ctx, "redis")
	if err != nil || got.Name != "redis" || !jsonEqual(t, got.Config, []byte(`{"mode":"stream"}`)) {
		t.Fatalf("get: %+v err=%v", got, err)
	}

	// The enabled flag is the operator's runtime kill switch.
	if err := s.SetAdapterEnabled(ctx, "redis", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	enabled, err := s.AdapterEnabled(ctx, "redis")
	if err != nil || enabled {
		t.Fatalf("AdapterEnabled after disable = %v, %v; want false, nil", enabled, err)
	}

	// Re-registering (e.g. on startup) refreshes config but PRESERVES the operator's enabled flag.
	a2, err := s.RegisterAdapter(ctx, "redis", "queue", "queue", []byte(`{"mode":"list"}`))
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if a2.Enabled {
		t.Fatal("re-register must not re-enable a disabled adapter")
	}
	if !jsonEqual(t, a2.Config, []byte(`{"mode":"list"}`)) {
		t.Fatalf("re-register should refresh config, got %s", a2.Config)
	}

	// Unregistered adapters resolve to ErrNotFound everywhere.
	if _, err := s.GetAdapter(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v, want ErrNotFound", err)
	}
	if _, err := s.AdapterEnabled(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("enabled missing: %v, want ErrNotFound", err)
	}
	if err := s.SetAdapterEnabled(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set enabled missing: %v, want ErrNotFound", err)
	}
}

// Server startup enumerates the queue family via ListAdaptersByFamily to attach poll loops:
// family-scoped, name-ordered, and INCLUDING disabled rows (the runner honors the enabled flag at
// runtime, so a disabled row can be re-enabled without a restart).
func TestListAdaptersByFamily(t *testing.T) {
	s, ctx := testStore(t)

	for _, a := range []struct{ name, family string }{
		{"redis-jobs", "queue"},
		{"redis-deploys", "queue"},
		{"github", "webhook"},
	} {
		if _, err := s.RegisterAdapter(ctx, a.name, a.family, "queue", nil); err != nil {
			t.Fatalf("register %s: %v", a.name, err)
		}
	}
	if err := s.SetAdapterEnabled(ctx, "redis-jobs", false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	got, err := s.ListAdaptersByFamily(ctx, "queue")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Name != "redis-deploys" || got[1].Name != "redis-jobs" {
		t.Fatalf("queue family = %+v, want [redis-deploys redis-jobs] in name order", got)
	}
	if got[1].Enabled {
		t.Fatal("disabled rows must still be listed (runtime flag is the runner's to honor)")
	}

	if got, err := s.ListAdaptersByFamily(ctx, "webhook"); err != nil || len(got) != 1 || got[0].Name != "github" {
		t.Fatalf("webhook family = %+v err=%v, want just github", got, err)
	}
	if got, err := s.ListAdaptersByFamily(ctx, "carrier-pigeon"); err != nil || len(got) != 0 {
		t.Fatalf("unknown family = %+v err=%v, want empty", got, err)
	}
}

// SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety": the poll-loop runner stamps each
// consume attempt's outcome (last poll time + last error) on the adapter's registry row so an
// operator can see a degraded adapter without shell access.
func TestRecordAdapterPoll(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.RegisterAdapter(ctx, "redis", "queue", "queue", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	a, err := s.GetAdapter(ctx, "redis")
	if err != nil || a.LastPollAt != nil || a.LastError != nil {
		t.Fatalf("fresh row must have no poll health: %+v err=%v", a, err)
	}

	// A degraded attempt records the (credential-free) error text.
	if err := s.RecordAdapterPoll(ctx, "redis", "redis stream: xreadgroup: connection refused"); err != nil {
		t.Fatalf("record failing poll: %v", err)
	}
	a, err = s.GetAdapter(ctx, "redis")
	if err != nil || a.LastPollAt == nil || a.LastError == nil {
		t.Fatalf("failing poll not stamped: %+v err=%v", a, err)
	}
	if *a.LastError != "redis stream: xreadgroup: connection refused" {
		t.Fatalf("last_error = %q", *a.LastError)
	}

	// A healthy attempt clears last_error and refreshes last_poll_at.
	if err := s.RecordAdapterPoll(ctx, "redis", ""); err != nil {
		t.Fatalf("record healthy poll: %v", err)
	}
	a, err = s.GetAdapter(ctx, "redis")
	if err != nil || a.LastPollAt == nil {
		t.Fatalf("healthy poll not stamped: %+v err=%v", a, err)
	}
	if a.LastError != nil {
		t.Fatalf("healthy poll must clear last_error, got %q", *a.LastError)
	}

	// Unregistered adapters resolve to ErrNotFound here too.
	if err := s.RecordAdapterPoll(ctx, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record missing: %v, want ErrNotFound", err)
	}
}

// SPEC-0017 REQ "Environment Config Import" (scenario "Boot with existing registry"): the env seed
// is create-if-absent — a boot with env config for a provider the registry already holds leaves the
// registry row (trust mode, secret, config, and any operator edit) untouched, and no duplicate
// provider appears. Governing: ADR-0020.
func TestSeedProviderCreateIfAbsent(t *testing.T) {
	s, ctx := testStore(t)

	created, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "dockerhub", Family: "webhook", Kind: "generic", TrustMode: "token",
		Secret: "tok-a", Config: []byte(`{"queue":"builds"}`),
	})
	if err != nil || !created {
		t.Fatalf("first seed: created=%v err=%v", created, err)
	}
	row, secret, err := s.ResolveProvider(ctx, "dockerhub")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if row.Kind != "generic" || row.TrustMode != "token" || !row.Enabled ||
		!row.SecretConfigured || secret != "tok-a" {
		t.Fatalf("seeded row wrong: %+v secret=%q", row, secret)
	}

	// Operator edit after import: disable the provider.
	if err := s.SetAdapterEnabled(ctx, "dockerhub", false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// Next boot re-seeds with drifted env values: the registry row wins — nothing is clobbered.
	created2, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "dockerhub", Family: "webhook", Kind: "generic", TrustMode: "open",
		Secret: "tok-b", Config: []byte(`{"queue":"other"}`),
	})
	if err != nil || created2 {
		t.Fatalf("re-seed must be a no-op: created=%v err=%v", created2, err)
	}
	row2, secret2, err := s.ResolveProvider(ctx, "dockerhub")
	if err != nil {
		t.Fatalf("resolve after re-seed: %v", err)
	}
	if row2.TrustMode != "token" || secret2 != "tok-a" || row2.Enabled ||
		!jsonEqual(t, row2.Config, []byte(`{"queue":"builds"}`)) {
		t.Fatalf("registry row must win over env drift: %+v secret=%q", row2, secret2)
	}

	// And no duplicate provider appeared.
	rows, err := s.ListProviders(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("providers = %+v err=%v, want exactly 1", rows, err)
	}
}

// SPEC-0017 REQ "Runtime Provider Registry": provider secrets are stored encrypted via the existing
// cred envelope — enc:v1: ciphertext at rest, never plaintext — and only the dispatch-path
// ResolveProvider ever opens them; the enumeration surface (ListProviders/Adapter) carries a
// presence flag and no secret field at all. Governing: ADR-0020 ("secrets through the envelope").
func TestSeedProviderSecretEncryptedAtRest(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)

	const plaintext = "whsec_live_hmac_key"
	if _, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed",
		Secret: plaintext, Config: []byte(`{"queue":"reviews"}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Read the column exactly as persisted, bypassing the store's decrypt path.
	var raw *string
	if err := s.pool.QueryRow(ctx, `SELECT secret FROM adapters WHERE name = 'github'`).Scan(&raw); err != nil {
		t.Fatalf("raw select secret: %v", err)
	}
	if raw == nil || !strings.HasPrefix(*raw, "enc:v1:") || strings.Contains(*raw, plaintext) {
		t.Fatalf("secret must sit on disk as enc:v1: ciphertext, got %v", raw)
	}

	row, secret, err := s.ResolveProvider(ctx, "github")
	if err != nil || secret != plaintext {
		t.Fatalf("resolve must decrypt transparently: %q err=%v", secret, err)
	}
	if !row.SecretConfigured {
		t.Fatalf("row must classify the secret as configured: %+v", row)
	}
}

// ADR-0020 "resolve-at-request, cache-lightly": ResolveProvider serves a short in-process cache —
// an out-of-band database write stays invisible until the TTL lapses, but every registry write
// through the store invalidates immediately, so same-process changes bind on the very next request.
func TestResolveProviderCacheInvalidatedOnWrite(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "lan", Family: "webhook", Kind: "generic", TrustMode: "open",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if a, _, err := s.ResolveProvider(ctx, "lan"); err != nil || !a.Enabled {
		t.Fatalf("first resolve: %+v err=%v", a, err)
	}

	// Bypass the store (simulating another process): the cache still serves the old row.
	if _, err := s.pool.Exec(ctx, `UPDATE adapters SET enabled = false WHERE name = 'lan'`); err != nil {
		t.Fatalf("raw update: %v", err)
	}
	if a, _, err := s.ResolveProvider(ctx, "lan"); err != nil || !a.Enabled {
		t.Fatalf("resolve within TTL should serve the cache: %+v err=%v", a, err)
	}

	// A registry write through the store invalidates: the next resolve reads the database.
	if err := s.SetAdapterEnabled(ctx, "lan", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if a, _, err := s.ResolveProvider(ctx, "lan"); err != nil || a.Enabled {
		t.Fatalf("resolve after a store write must see enabled=false: %+v err=%v", a, err)
	}

	// Unknown names are ErrNotFound and never cached, so a just-created provider resolves at once.
	if _, _, err := s.ResolveProvider(ctx, "fresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown provider: %v, want ErrNotFound", err)
	}
	if _, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "fresh", Family: "webhook", Kind: "generic", TrustMode: "open",
	}); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if a, _, err := s.ResolveProvider(ctx, "fresh"); err != nil || a.Name != "fresh" {
		t.Fatalf("just-created provider must resolve immediately: %+v err=%v", a, err)
	}
}

// SPEC-0017 REQ "Provider Lifecycle": rotate replaces the held secret — the old plaintext is dead
// on the very next resolve (the write invalidates the dispatch cache), the new one resolves, and
// an unknown provider is ErrNotFound.
func TestRotateProviderSecret(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "homelab", Family: "webhook", Kind: "generic", TrustMode: "token", Secret: "old-secret",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, secret, err := s.ResolveProvider(ctx, "homelab"); err != nil || secret != "old-secret" {
		t.Fatalf("resolve before rotate: %q err=%v", secret, err)
	}

	if err := s.RotateProviderSecret(ctx, "homelab", "new-secret"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	a, secret, err := s.ResolveProvider(ctx, "homelab")
	if err != nil || secret != "new-secret" {
		t.Fatalf("resolve after rotate must serve the NEW secret immediately: %q err=%v", secret, err)
	}
	if !a.SecretConfigured {
		t.Fatalf("rotated row must classify configured: %+v", a)
	}

	if err := s.RotateProviderSecret(ctx, "ghost", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate unknown provider: %v, want ErrNotFound", err)
	}
}

// SPEC-0017 REQ "Provider Lifecycle": removal deletes ONLY the registry row — everything the
// provider ingested (events and todos) stays queryable, and the removed name resolves ErrNotFound
// immediately (cache invalidated). Scenario: removal never deletes ingested events or todos.
func TestRemoveProviderKeepsEventsAndTodos(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.SeedProvider(ctx, ProviderSeed{
		Name: "doomed", Family: "webhook", Kind: "generic", TrustMode: "token", Secret: "tok",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, td, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "doomed", Family: "webhook", ExternalID: "d1", TrustMode: "token",
			Verified: false, Payload: []byte(`{}`)},
		CreateTodoParams{Queue: "doomed", Source: "doomed", Kind: "webhook", Title: "webhook doomed delivery",
			Payload: []byte(`{}`), IdempotencyKey: "d1"})
	if err != nil || !created {
		t.Fatalf("ingest fixture: created=%v err=%v", created, err)
	}

	if err := s.RemoveProvider(ctx, "doomed"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := s.GetAdapter(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("registry row must be gone: %v, want ErrNotFound", err)
	}
	if _, _, err := s.ResolveProvider(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dispatch resolve after remove: %v, want ErrNotFound (cache must not serve the ghost)", err)
	}

	// History survives: the ingested event and its todo remain queryable by the source name.
	events, err := s.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Source == "doomed" {
			found = true
		}
	}
	if !found {
		t.Fatal("removal must NOT delete previously ingested events")
	}
	got, err := s.GetTodo(ctx, td.ID)
	if err != nil || got.Queue != "doomed" {
		t.Fatalf("removal must NOT delete todos: %+v err=%v", got, err)
	}

	if err := s.RemoveProvider(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v, want ErrNotFound", err)
	}
}

// SPEC-0017 REQ "Providers View": per-provider in-rate and last-seen derive from the events table
// keyed by source; providers that never ingested simply have no entry.
func TestProviderHealthBySource(t *testing.T) {
	s, ctx := testStore(t)

	if _, _, _, err := s.CreateEventTodo(ctx,
		EventInput{Source: "hb", Family: "webhook", ExternalID: "h1", TrustMode: "token",
			Verified: false, Payload: []byte(`{}`)},
		CreateTodoParams{Queue: "hb", Source: "hb", Kind: "webhook", Title: "t",
			Payload: []byte(`{}`), IdempotencyKey: "h1"}); err != nil {
		t.Fatalf("ingest fixture: %v", err)
	}

	health, err := s.ProviderHealthBySource(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	h, ok := health["hb"]
	if !ok {
		t.Fatalf("health missing source hb: %+v", health)
	}
	if h.EventsPerMin < 1 {
		t.Fatalf("in-rate: got %d, want >= 1 (event just ingested)", h.EventsPerMin)
	}
	if h.LastSeenAt == nil {
		t.Fatal("last-seen must be stamped by the ingested event")
	}
	if _, ok := health["never-ingested"]; ok {
		t.Fatal("sources that never ingested must have no entry")
	}
}
