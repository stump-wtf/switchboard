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

	closeFn, err := registerQueueAdapters(context.Background(), st, run, "redis://deploy-bot:pw@127.0.0.1:6379/0", discardLogger())
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
	closeFn, err := registerQueueAdapters(context.Background(), &fakeQueueAdapterStore{}, run, "", discardLogger())
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
	closeFn, err := registerQueueAdapters(context.Background(), st, run, "", discardLogger())
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
		"http://user:"+secret+"@example.com", discardLogger())
	if err == nil {
		t.Fatal("malformed DSN must fail startup")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("startup error leaks DSN credentials: %v", err)
	}
}

// A registry listing failure is a real startup error (the database just migrated, so it should
// never happen silently), and a runner Add failure fails soft per row like a config error.
func TestRegisterQueueAdaptersErrors(t *testing.T) {
	if _, err := registerQueueAdapters(context.Background(),
		&fakeQueueAdapterStore{listErr: errors.New("db down")}, &fakeAdder{}, "", discardLogger()); err == nil {
		t.Fatal("list failure must surface")
	}

	st := &fakeQueueAdapterStore{rows: []store.Adapter{
		queueRow("deploys", `{"transport":"redis","mode":"stream","stream":"deploys"}`),
	}}
	run := &fakeAdder{addErr: errors.New("duplicate")}
	closeFn, err := registerQueueAdapters(context.Background(), st, run, "redis://127.0.0.1:6379/0", discardLogger())
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
