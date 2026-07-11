package store

import (
	"encoding/json"
	"errors"
	"reflect"
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
