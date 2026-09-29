package server

// The notify-hook dispatcher as Run wires it. Every dispatcher test in internal/notifyhook builds
// its own Dispatcher and calls Enqueue directly, so none of them would notice Run failing to
// subscribe it to the store's ready hook, never starting its workers, or dropping the ceiling, the
// SSRF guard or the process metrics. This drives startNotifyDispatcher, the function Run calls, from
// a ready-hook fire to a signed request at a receiver, a health write and a counted outcome.
// Governing: SPEC-0024 REQ-1 (kill switch), REQ-6 (trigger), REQ-8 (health), REQ-11 (metrics).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/notifyhook"
	"github.com/stump-wtf/switchboard/internal/store"
)

const wiringEndpoint = "11111111-1111-4111-8111-111111111111"

// wiringStore is a one-endpoint, one-hook store that records the ready-hook subscription.
type wiringStore struct {
	url    string
	secret string
	mu     sync.Mutex
	hook   store.TodoReadyHook
	subs   int
	health int // RecordNotifyHookDelivery calls
}

func (w *wiringStore) SetTodoReadyHook(fn store.TodoReadyHook) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hook, w.subs = fn, w.subs+1
}

func (w *wiringStore) readyHook() (store.TodoReadyHook, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hook, w.subs
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
			if labels["type"] == notifyhook.TypeTodoReady && labels["outcome"] == outcome {
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
