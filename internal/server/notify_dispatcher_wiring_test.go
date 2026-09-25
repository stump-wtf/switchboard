package server

// The notify-hook dispatcher as Run wires it. Every dispatcher test in internal/notifyhook builds
// its own Dispatcher and calls Enqueue directly, so none of them would notice Run failing to
// subscribe it to the store's ready hook, never starting its workers, or dropping the ceiling or
// the SSRF guard. This drives startNotifyDispatcher, the function Run calls, from a ready-hook fire
// to a signed request at a receiver.
// Governing: SPEC-0024 REQ-1 (kill switch), REQ-6 (trigger).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/config"
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
	return false, nil
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
		if d := startNotifyDispatcher(ctx, st, v, 0, nil, nil); d != nil {
			t.Fatal("a dispatcher was started with the ceiling at 0")
		}
		if _, subs := st.readyHook(); subs != 0 {
			t.Fatalf("the ready hook was subscribed %d times with the ceiling at 0", subs)
		}
	})

	t.Run("a ready todo reaches the hook", func(t *testing.T) {
		st := &wiringStore{url: srv.URL, secret: secret}
		if d := startNotifyDispatcher(ctx, st, v, 5, nil, nil); d == nil {
			t.Fatal("no dispatcher with the ceiling at 5")
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
	})
}
