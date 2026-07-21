package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/adapter"
	"github.com/joestump/switchboard/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeQueueAdapterStore serves scripted registry rows; CreateEventTodo is never reached in these
// tests (no consume runs), it only satisfies the sink's store slice.
type fakeQueueAdapterStore struct {
	rows    []store.Adapter
	listErr error
}

func (f *fakeQueueAdapterStore) ListAdaptersByFamily(_ context.Context, family string) ([]store.Adapter, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []store.Adapter
	for _, r := range f.rows {
		if r.Family == family {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeQueueAdapterStore) CreateEventTodo(context.Context, store.EventInput, store.CreateTodoParams) (int64, store.Todo, bool, error) {
	return 0, store.Todo{}, false, errors.New("not used in wiring tests")
}

// fakeAdder records what the wiring attaches to the runner.
type fakeAdder struct {
	added  []string
	addErr error
}

func (f *fakeAdder) Add(a adapter.Adapter, sink adapter.Sink, _ []byte) error {
	if f.addErr != nil {
		return f.addErr
	}
	if a == nil || sink == nil {
		return errors.New("nil adapter/sink")
	}
	f.added = append(f.added, a.Name())
	return nil
}

func queueRow(name, config string) store.Adapter {
	return store.Adapter{Name: name, Family: "queue", TrustMode: "queue", Enabled: true, Config: []byte(config)}
}

// testLegacyEndpointID stands in for the operator's SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID: the
// fallback tenant for a registry row that names no endpoint of its own. A registry row carries an
// endpoint id as opaque non-secret topology and the sink only requires it to be non-empty, so a
// fixed uuid is enough here — no endpoints row is read on the wiring path.
const testLegacyEndpointID = "9f1d2e3a-4b5c-6d7e-8f90-a1b2c3d4e5f6"

// SPEC-0002 REQ "Adapter Interface and Trust Mode": startup reads the queue-family registry and
// attaches one runner worker per valid Redis row — bad rows fail soft (skipped, adapter stays
// dark), rows for other families or unimplemented transports are not attached, and DISABLED rows
// ARE attached (the runner honors the enabled flag at runtime, so re-enabling needs no restart).
func TestRegisterQueueAdaptersAttachesRegistryRows(t *testing.T) {
	disabled := queueRow("paused", `{"transport":"redis","mode":"list","list":"paused-jobs"}`)
	disabled.Enabled = false
	st := &fakeQueueAdapterStore{rows: []store.Adapter{
		queueRow("deploys", `{"transport":"redis","mode":"stream","stream":"deploys","group":"g","consumer":"c"}`),
		disabled,
		queueRow("future", `{"transport":"sqs","mode":"stream"}`),               // unimplemented transport: skip
		queueRow("broken", `{"transport":"redis","mode":"nope"}`),               // invalid config: fail soft
		{Name: "github", Family: "webhook", TrustMode: "signed", Enabled: true}, // push family: not ours
	}}
	run := &fakeAdder{}

	closeFn, err := registerQueueAdapters(context.Background(), st, run, "redis://deploy-bot:pw@127.0.0.1:6379/0", testLegacyEndpointID, discardLogger())
	if err != nil {
		t.Fatalf("registerQueueAdapters: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	if got := strings.Join(run.added, ","); got != "deploys,paused" {
		t.Fatalf("attached = %q, want deploys,paused (valid rows only, disabled included)", got)
	}
}

// With no queue rows the wiring is a no-op — no broker client is built at all, so a deployment
// without pull ingestion never needs SWITCHBOARD_REDIS_URL.
func TestRegisterQueueAdaptersNoRows(t *testing.T) {
	run := &fakeAdder{}
	closeFn, err := registerQueueAdapters(context.Background(), &fakeQueueAdapterStore{}, run, "", testLegacyEndpointID, discardLogger())
	if err != nil {
		t.Fatalf("registerQueueAdapters: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(run.added) != 0 {
		t.Fatalf("attached = %v, want none", run.added)
	}
}

// Queue rows without SWITCHBOARD_REDIS_URL: pull ingestion stays off (warned, not fatal) — the
// HTTP surface must come up even while the broker side is mid-rollout.
func TestRegisterQueueAdaptersNoDSN(t *testing.T) {
	st := &fakeQueueAdapterStore{rows: []store.Adapter{
		queueRow("deploys", `{"transport":"redis","mode":"stream","stream":"deploys"}`),
	}}
	run := &fakeAdder{}
	closeFn, err := registerQueueAdapters(context.Background(), st, run, "", testLegacyEndpointID, discardLogger())
	if err != nil {
		t.Fatalf("registerQueueAdapters: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if len(run.added) != 0 {
		t.Fatalf("attached = %v, want none without a DSN", run.added)
	}
}

// A malformed DSN is env misconfiguration and fails startup loudly (same posture as
// SWITCHBOARD_GENERIC_PROVIDERS) — with the DSN redacted from the error.
func TestRegisterQueueAdaptersBadDSNFailsStartup(t *testing.T) {
	st := &fakeQueueAdapterStore{rows: []store.Adapter{
		queueRow("deploys", `{"transport":"redis","mode":"stream","stream":"deploys"}`),
	}}
	const secret = "hunter2"
	_, err := registerQueueAdapters(context.Background(), st, &fakeAdder{},
		"http://user:"+secret+"@example.com", testLegacyEndpointID, discardLogger())
	if err == nil {
		t.Fatal("malformed DSN must fail startup")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("startup error leaks DSN credentials: %v", err)
	}
}

// ADR-0022: every todo an adapter enqueues is owned by exactly one endpoint, and a broker
// connection authenticates the ADAPTER rather than a principal, so ownership must be STATED — the
// row's own endpoint_id, else the operator's legacy fallback. With neither, the sink cannot be
// built and the adapter stays dark rather than failing on every insert: no endpoint, no consuming.
// This is the wiring-level expression of "todos.endpoint_id is NOT NULL with no sentinel" — the
// alternative (deriving an owner from the target queue) is precisely the shared-queue-string
// collision ADR-0022 exists to remove.
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func TestRegisterQueueAdaptersRequireAnOwningEndpoint(t *testing.T) {
	const dsn = "redis://127.0.0.1:6379/0"
	owned := queueRow("owned", `{"transport":"redis","mode":"stream","stream":"owned","endpoint_id":"`+testLegacyEndpointID+`"}`)
	orphan := queueRow("orphan", `{"transport":"redis","mode":"stream","stream":"orphan"}`)

	// No legacy fallback configured: the row stating its own tenant starts, the one that states
	// none stays dark — fail-soft, not a startup failure and not an unowned todo.
	run := &fakeAdder{}
	closeFn, err := registerQueueAdapters(context.Background(),
		&fakeQueueAdapterStore{rows: []store.Adapter{owned, orphan}}, run, dsn, "", discardLogger())
	if err != nil {
		t.Fatalf("a row without an owner must fail soft, got %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if got := strings.Join(run.added, ","); got != "owned" {
		t.Fatalf("attached = %q, want only owned (an adapter with no endpoint must not consume)", got)
	}

	// With the operator's fallback configured, the same orphan row inherits that tenant and starts.
	run = &fakeAdder{}
	closeFn2, err := registerQueueAdapters(context.Background(),
		&fakeQueueAdapterStore{rows: []store.Adapter{orphan}}, run, dsn, testLegacyEndpointID, discardLogger())
	if err != nil {
		t.Fatalf("registerQueueAdapters: %v", err)
	}
	t.Cleanup(func() { _ = closeFn2() })
	if got := strings.Join(run.added, ","); got != "orphan" {
		t.Fatalf("attached = %q, want orphan (the legacy endpoint is the stated fallback owner)", got)
	}
}

// A registry listing failure is a real startup error (the database just migrated, so it should
// never happen silently), and a runner Add failure fails soft per row like a config error.
func TestRegisterQueueAdaptersErrors(t *testing.T) {
	if _, err := registerQueueAdapters(context.Background(),
		&fakeQueueAdapterStore{listErr: errors.New("db down")}, &fakeAdder{}, "", testLegacyEndpointID, discardLogger()); err == nil {
		t.Fatal("list failure must surface")
	}

	st := &fakeQueueAdapterStore{rows: []store.Adapter{
		queueRow("deploys", `{"transport":"redis","mode":"stream","stream":"deploys"}`),
	}}
	run := &fakeAdder{addErr: errors.New("duplicate")}
	closeFn, err := registerQueueAdapters(context.Background(), st, run, "redis://127.0.0.1:6379/0", testLegacyEndpointID, discardLogger())
	if err != nil {
		t.Fatalf("Add failure must fail soft, got %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(run.added) != 0 {
		t.Fatalf("attached = %v, want none", run.added)
	}
}
