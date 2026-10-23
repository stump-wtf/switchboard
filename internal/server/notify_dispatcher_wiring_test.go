package server

// The notify-hook dispatcher as Run wires it. Every dispatcher test in internal/notifyhook builds
// its own Dispatcher and calls Enqueue directly, so none of them would notice Run failing to
// subscribe it to the store's ready hook, never starting its workers, or dropping the ceiling, the
// SSRF guard or the process metrics. This drives startNotifyDispatcher, the function Run calls, from
// a ready-hook fire to a signed request at a receiver, a health write and a counted outcome.
// Governing: SPEC-0024 REQ-1 (kill switch), REQ-6 (trigger), REQ-8 (health), REQ-11 (metrics).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/notifyhook"
	"github.com/stump-wtf/switchboard/internal/store"
)

const wiringEndpoint = "11111111-1111-4111-8111-111111111111"

// wiringStore is a one-endpoint, one-hook store that records the ready and dead-letter hook
// subscriptions.
type wiringStore struct {
	url    string
	secret string
	mu     sync.Mutex
	hook   store.TodoReadyHook
	dlHook store.TodoDeadLetteredHook
	subs   int
	dlSubs int
	health int // RecordNotifyHookDelivery calls
}

func (w *wiringStore) SetTodoReadyHook(fn store.TodoReadyHook) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hook, w.subs = fn, w.subs+1
}

func (w *wiringStore) SetTodoDeadLetteredHook(fn store.TodoDeadLetteredHook) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dlHook, w.dlSubs = fn, w.dlSubs+1
}

func (w *wiringStore) readyHook() (store.TodoReadyHook, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hook, w.subs
}

func (w *wiringStore) deadLetterHook() (store.TodoDeadLetteredHook, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dlHook, w.dlSubs
}

func (w *wiringStore) NotifyEndpointForDispatch(context.Context, string) (store.NotifyEndpoint, error) {
	return store.NotifyEndpoint{Slug: "wiring", ScopeQueues: []string{"inbox"}}, nil
}

func (w *wiringStore) hookRow() store.NotifyHook {
	return store.NotifyHook{ID: "h1", EndpointID: wiringEndpoint, URL: w.url, Enabled: true}
}

func (w *wiringStore) ListNotifyHooks(context.Context, string) ([]store.NotifyHook, error) {
	return []store.NotifyHook{w.hookRow()}, nil
}

func (w *wiringStore) GetNotifyHook(context.Context, string, string) (store.NotifyHook, error) {
	return w.hookRow(), nil
}

func (w *wiringStore) NotifyHookSigningSecrets(context.Context, string, string) (store.NotifyHookSecrets, error) {
	return store.NotifyHookSecrets{Current: w.secret}, nil
}

func (w *wiringStore) DestroyExpiredNotifyHookSecrets(context.Context) (int64, error) { return 0, nil }

func (w *wiringStore) RecordNotifyHookDelivery(context.Context, string, bool, *int, string, int) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.health++
	return false, nil
}

func (w *wiringStore) healthWrites() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.health
}

// notifyCounter reads switchboard_notify_hook_notifications_total{type="todo.ready",outcome} from
// mtr's registry, and whether the series exists at all.
func notifyCounter(t *testing.T, mtr *metrics.Metrics, outcome string) (float64, bool) {
	t.Helper()
	return notifyCounterType(t, mtr, notifyhook.TypeTodoReady, outcome)
}

// notifyCounterType is notifyCounter for one event type, so a test can tell a ready notification's
// counter from a dead letter's.
func notifyCounterType(t *testing.T, mtr *metrics.Metrics, typ, outcome string) (float64, bool) {
	t.Helper()
	mfs, err := mtr.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "switchboard_notify_hook_notifications_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["type"] == typ && labels["outcome"] == outcome {
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestNotifyDispatcherWiring(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("webhook-id")
	}))
	defer srv.Close()
	secret, err := notifyhook.MintSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The receiver is plain http on loopback, so it is reachable only through the operator's http
	// opt-in and CIDR allowlist: a dispatcher wired without Run's Validator refuses it.
	v, _, err := notifyHookValidator(config.Config{Addr: "127.0.0.1:1", NotifyHookAllowCIDRs: "127.0.0.1/32", PushAllowHTTP: true})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	todo := store.Todo{ID: "td_wiring", EndpointID: wiringEndpoint, Queue: "inbox", Title: "wired"}

	t.Run("ceiling 0 subscribes nothing", func(t *testing.T) {
		st := &wiringStore{url: srv.URL, secret: secret}
		mtr := metrics.New(metrics.Options{})
		if d := startNotifyDispatcher(ctx, st, v, 0, mtr, nil); d != nil {
			t.Fatal("a dispatcher was started with the ceiling at 0")
		}
		if _, subs := st.readyHook(); subs != 0 {
			t.Fatalf("the ready hook was subscribed %d times with the ceiling at 0", subs)
		}
		if _, subs := st.deadLetterHook(); subs != 0 {
			t.Fatalf("the dead-letter hook was subscribed %d times with the ceiling at 0", subs)
		}
		// No dispatcher, so its series stay honestly absent rather than a misleading zero.
		if _, ok := notifyCounter(t, mtr, "delivered"); ok {
			t.Fatal("notify-hook series exist with no dispatcher running")
		}
	})

	t.Run("a ready todo reaches the hook", func(t *testing.T) {
		st := &wiringStore{url: srv.URL, secret: secret}
		mtr := metrics.New(metrics.Options{})
		if d := startNotifyDispatcher(ctx, st, v, 5, mtr, nil); d == nil {
			t.Fatal("no dispatcher with the ceiling at 5")
		}
		// REQ-11: the series exist at zero before anything is sent, from the first scrape.
		if n, ok := notifyCounter(t, mtr, "delivered"); !ok || n != 0 {
			t.Fatalf("delivered series = %v (exists %v) before any delivery, want 0 and present", n, ok)
		}
		fire, subs := st.readyHook()
		if subs != 1 || fire == nil {
			t.Fatalf("ready hook subscribed %d times, want once", subs)
		}
		fire(todo, store.ReadyCreated)
		select {
		case id := <-got:
			if id == "" {
				t.Fatal("the receiver got a request with no webhook-id")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a ready todo never reached the hook: the dispatcher is not subscribed, not running, or not allowed the target")
		}
		// The outcome is counted in the process registry and its health written to the store, after
		// the receiver answers; poll briefly for that.
		deadline := time.Now().Add(5 * time.Second)
		for {
			n, _ := notifyCounter(t, mtr, "delivered")
			if n == 1 && st.healthWrites() == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("after one delivery: delivered counter %v, health writes %d; want 1 and 1", n, st.healthWrites())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// TestNotifyDispatcherWiringDeadLetter proves the other half of the wiring: Run subscribes the
// dispatcher to the store's dead-letter hook too, and a dead letter delivered through it is signed,
// carries the dead-letter context, and never carries the todo's payload (issue #588).
func TestNotifyDispatcherWiringDeadLetter(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := io.ReadFull(r.Body, body)
		got <- r.Header.Get("webhook-signature") + "\n" + string(body[:n])
	}))
	defer srv.Close()
	secret, err := notifyhook.MintSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	v, _, err := notifyHookValidator(config.Config{Addr: "127.0.0.1:1", NotifyHookAllowCIDRs: "127.0.0.1/32", PushAllowHTTP: true})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &wiringStore{url: srv.URL, secret: secret}
	mtr := metrics.New(metrics.Options{})
	if d := startNotifyDispatcher(ctx, st, v, 5, mtr, nil); d == nil {
		t.Fatal("no dispatcher with the ceiling at 5")
	}
	fire, subs := st.deadLetterHook()
	if subs != 1 || fire == nil {
		t.Fatalf("dead-letter hook subscribed %d times, want once", subs)
	}
	summary := "upstream returned 502 three times"
	artifact := "mcp://cairn/AbcdEfGh"
	todo := store.Todo{
		ID: "td_dead", EndpointID: wiringEndpoint, Queue: "inbox", Title: "publish the release",
		Attempt: 3, Payload: []byte("the payload nobody may read: " + t.Name()),
		FinalAttempt: &store.FinalAttempt{Seq: 3, Outcome: "failed", Summary: &summary,
			Artifact: &artifact, AttemptsTotal: 3},
	}
	fire(todo, store.DeadLetteredExhausted)
	select {
	case line := <-got:
		sig, body, _ := strings.Cut(line, "\n")
		if sig == "" {
			t.Fatal("the dead letter arrived unsigned")
		}
		for _, want := range []string{`"type":"todo.dead_lettered"`, `"todo_id":"td_dead"`,
			`"reason":"attempts_exhausted"`, `"attempts_total":3`, summary, artifact} {
			if !strings.Contains(body, want) {
				t.Fatalf("dead letter body %s lacks %s", body, want)
			}
		}
		if strings.Contains(body, "the payload nobody may read") {
			t.Fatalf("the dead letter carried the todo payload: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a dead letter never reached the hook: the dispatcher is not subscribed to the dead-letter hook")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, _ := notifyCounterType(t, mtr, notifyhook.TypeTodoDeadLettered, "delivered")
		if n == 1 && st.healthWrites() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after one dead-letter delivery: counter %v, health writes %d; want 1 and 1", n, st.healthWrites())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n, ok := notifyCounter(t, mtr, "delivered"); !ok || n != 0 {
		t.Fatalf("a dead letter also counted as a todo.ready delivery (%v, exists %v)", n, ok)
	}
}
