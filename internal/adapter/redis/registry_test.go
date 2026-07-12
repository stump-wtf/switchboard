package redis

import (
	"errors"
	"strings"
	"testing"
)

func testFactory(t *testing.T, dsn string) *Factory {
	t.Helper()
	f, err := NewFactory(dsn, testLogger())
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// A registry row's config jsonb builds the matching transport mode, and the row's primary-key name
// becomes the adapter's Name VERBATIM — the runner registers/checks enabled under that name, so a
// derived name would fork the registry (SPEC-0002 REQ "Adapter Interface and Trust Mode").
func TestFactoryFromRegistryModes(t *testing.T) {
	f := testFactory(t, "redis://deploy-bot:pw@127.0.0.1:6379/0")

	cases := []struct {
		name   string
		config string
		mode   string
	}{
		{"deploys", `{"transport":"redis","mode":"stream","stream":"app:deploys","group":"g","consumer":"c"}`, "stream"},
		{"jobs", `{"transport":"redis","mode":"list","list":"jobs"}`, "list"},
		{"chatter", `{"transport":"redis","mode":"pubsub","channel":"chatter"}`, "pubsub"},
	}
	for _, tc := range cases {
		a, cfg, err := f.FromRegistry(tc.name, []byte(tc.config))
		if err != nil {
			t.Fatalf("%s: FromRegistry: %v", tc.mode, err)
		}
		if a.Name() != tc.name {
			t.Fatalf("%s: Name() = %q, want the registry row name %q", tc.mode, a.Name(), tc.name)
		}
		if cfg.Mode != tc.mode {
			t.Fatalf("cfg.Mode = %q, want %q", cfg.Mode, tc.mode)
		}
		// Trust detail defaults to the connection's ACL identity from the DSN (ADR-0003: trust is
		// the broker connection), since the row set none.
		if a.TrustDetail() != "redis acl: deploy-bot" {
			t.Fatalf("%s: TrustDetail() = %q, want the DSN ACL identity", tc.mode, a.TrustDetail())
		}
	}
}

// A row may override the trust detail with a more descriptive (non-secret) label, and carries the
// target todo queue for the sink.
func TestFactoryFromRegistryOverrides(t *testing.T) {
	f := testFactory(t, "redis://127.0.0.1:6379/0")

	a, cfg, err := f.FromRegistry("deploys", []byte(
		`{"transport":"redis","mode":"stream","stream":"deploys","queue":"deploy-work","trust_detail":"redis acl: deploy-bot (prod)"}`))
	if err != nil {
		t.Fatalf("FromRegistry: %v", err)
	}
	if a.TrustDetail() != "redis acl: deploy-bot (prod)" {
		t.Fatalf("TrustDetail() = %q, want the row override", a.TrustDetail())
	}
	if cfg.Queue != "deploy-work" {
		t.Fatalf("cfg.Queue = %q, want deploy-work", cfg.Queue)
	}

	// No ACL username in the DSN and no row override → the generic connection identity.
	a, _, err = f.FromRegistry("jobs", []byte(`{"transport":"redis","mode":"list","list":"jobs"}`))
	if err != nil {
		t.Fatalf("FromRegistry: %v", err)
	}
	if a.TrustDetail() != "redis connection" {
		t.Fatalf("TrustDetail() = %q, want the default connection identity", a.TrustDetail())
	}
}

// Stream mode applies the documented defaults when the row omits group/consumer: group
// "switchboard", consumer the host name (never empty — NewStream requires one).
func TestFactoryFromRegistryStreamDefaults(t *testing.T) {
	f := testFactory(t, "redis://127.0.0.1:6379/0")

	a, _, err := f.FromRegistry("deploys", []byte(`{"transport":"redis","mode":"stream","stream":"deploys"}`))
	if err != nil {
		t.Fatalf("FromRegistry with defaulted group/consumer: %v", err)
	}
	s, ok := a.(*Stream)
	if !ok {
		t.Fatalf("adapter = %T, want *Stream", a)
	}
	if s.cfg.Group != "switchboard" {
		t.Fatalf("group = %q, want switchboard", s.cfg.Group)
	}
	if s.cfg.Consumer == "" {
		t.Fatal("consumer must default to a non-empty name")
	}
}

// Rows naming another transport (future SQS/NATS/AMQP packages) are ErrForeignTransport — a skip
// signal for the server wiring, distinguishable from misconfiguration via errors.Is.
func TestFactoryFromRegistryForeignTransport(t *testing.T) {
	f := testFactory(t, "redis://127.0.0.1:6379/0")

	for _, config := range []string{
		`{"transport":"sqs","mode":"stream"}`,
		`{"mode":"stream","stream":"deploys"}`, // no transport claimed at all
	} {
		if _, _, err := f.FromRegistry("other", []byte(config)); !errors.Is(err, ErrForeignTransport) {
			t.Fatalf("config %s: err = %v, want ErrForeignTransport", config, err)
		}
	}
}

// Everything else wrong with a row is a descriptive error naming the row, never a panic and never
// a silently-started adapter.
func TestFactoryFromRegistryConfigErrors(t *testing.T) {
	f := testFactory(t, "redis://127.0.0.1:6379/0")

	cases := []struct {
		config string
		want   string
	}{
		{``, "has no config"},
		{`{not json`, "parse registry config"},
		{`{"transport":"redis","mode":"carrier-pigeon"}`, "unknown mode"},
		{`{"transport":"redis","mode":"stream"}`, "requires stream"},    // NewStream validation, wrapped
		{`{"transport":"redis","mode":"list"}`, "requires a source"},    // NewList validation, wrapped
		{`{"transport":"redis","mode":"pubsub"}`, "requires a channel"}, // NewPubSub validation, wrapped
	}
	for _, tc := range cases {
		_, _, err := f.FromRegistry("bad", []byte(tc.config))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("config %q: err = %v, want it to contain %q", tc.config, err, tc.want)
		}
		if !strings.Contains(err.Error(), `"bad"`) {
			t.Fatalf("config %q: err = %v, want it to name the row", tc.config, err)
		}
	}
}

// NewFactory redacts the DSN from parse errors exactly like NewClient (SPEC-0002 REQ "Error
// Handling Standards": never broker credentials in errors/logs).
func TestNewFactoryRedactsDSNFromParseErrors(t *testing.T) {
	const secret = "hunter2"
	_, err := NewFactory("http://user:"+secret+"@example.com", testLogger())
	if err == nil {
		t.Fatal("wrong-scheme DSN must error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error leaks DSN credentials: %v", err)
	}
}
