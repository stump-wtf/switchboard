package notifyhook

// Hook health through the dispatcher: auto-disable after DisableAfter failed deliveries, recovery
// resetting the count, a store outage during the health write, and the REQ-11 counters.
//
// Governing: SPEC-0024 REQ-8 "Hook Health and Auto-Disable", REQ-11 "Observability", REQ-12
// scenario "Store outage during health update".

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

type countingMetrics struct {
	mu            sync.Mutex
	notifications map[string]int
	attempts      map[string]int
	disabled      map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{notifications: map[string]int{}, attempts: map[string]int{}, disabled: map[string]int{}}
}

func (c *countingMetrics) NotifyHookNotification(typ, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifications[typ+"/"+outcome]++
}

func (c *countingMetrics) NotifyHookAttempt(result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[result]++
}

func (c *countingMetrics) NotifyHookDisabled(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disabled[reason]++
}

func (c *countingMetrics) snapshot() (map[string]int, map[string]int, map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := func(m map[string]int) map[string]int {
		out := map[string]int{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return cp(c.notifications), cp(c.attempts), cp(c.disabled)
}

func (m *memStore) hook(id string) store.NotifyHook {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hooks[id]
}

// Scenario "Dead receiver is disabled".
func TestDeadReceiverIsDisabled(t *testing.T) {
	rcv := newReceiver(t, http.StatusUnauthorized) // one attempt per notification: a permanent failure
	st := newMemStore()
	st.addHook(t, testEndpoint, "h1", rcv.hookURL("/"))
	met := newCountingMetrics()
	h := newHarness(t, st, rcv, func(o *Options) { o.Metrics = met })

	for i := 0; i < DisableAfter; i++ {
		h.d.Enqueue(readyTodo(testEndpoint, "inbox"), store.ReadyCreated)
		h.wait(t)
	}
	hk := st.hook("h1")
	if hk.Enabled || hk.DisabledReason == nil || *hk.DisabledReason != store.NotifyHookDisabledFailures ||
		hk.ConsecutiveFailures != DisableAfter || hk.LastError == nil || *hk.LastError != "client_error" ||
		hk.LastStatus == nil || *hk.LastStatus != http.StatusUnauthorized {
		t.Fatalf("hook after %d failures = %+v", DisableAfter, hk)
	}
	// The 11th ready todo does not call it.
	h.d.Enqueue(readyTodo(testEndpoint, "inbox"), store.ReadyCreated)
	h.quiet(t)
	if n := len(rcv.got()); n != DisableAfter {
		t.Fatalf("a disabled hook was called: %d requests", n)
	}
	notes, attempts, disabled := met.snapshot()
	if notes["todo.ready/failed"] != DisableAfter || attempts["4xx"] != DisableAfter || disabled["consecutive_failures"] != 1 {
		t.Fatalf("metrics: notifications %v attempts %v disabled %v", notes, attempts, disabled)
	}
}

// Scenario "Recovery resets the count".
func TestRecoveryResetsTheCount(t *testing.T) {
	rcv := newReceiver(t, http.StatusNoContent)
	st := newMemStore()
	st.addHook(t, testEndpoint, "h1", rcv.hookURL("/"))
	st.mu.Lock()
	hk := st.hooks["h1"]
	hk.ConsecutiveFailures = 7
	st.hooks["h1"] = hk
	st.mu.Unlock()
	met := newCountingMetrics()
	h := newHarness(t, st, rcv, func(o *Options) { o.Metrics = met })

	h.d.Enqueue(readyTodo(testEndpoint, "inbox"), store.ReadyCreated)
	h.wait(t)
	if got := st.hook("h1"); got.ConsecutiveFailures != 0 || got.LastError != nil || got.LastStatus == nil || *got.LastStatus != 204 {
		t.Fatalf("hook after a delivery = %+v", got)
	}
	if notes, attempts, _ := met.snapshot(); notes["todo.ready/delivered"] != 1 || attempts["2xx"] != 1 {
		t.Fatalf("metrics: %v %v", notes, attempts)
	}
}

// Scenario "Store outage during health update": logged with both ids, and the worker carries on.
func TestHealthStoreOutageDoesNotStallDelivery(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	rcv := newReceiver(t)
	st := newMemStore()
	st.addHook(t, testEndpoint, "h1", rcv.hookURL("/"))
	st.mu.Lock()
	st.healthErr = errors.New("database unavailable")
	st.mu.Unlock()
	h := newHarness(t, st, rcv, func(o *Options) { o.Log = newLockedLogger(&logs, &logMu); o.Workers = 1 })

	h.d.Enqueue(readyTodo(testEndpoint, "inbox"), store.ReadyCreated)
	first := h.wait(t)
	h.d.Enqueue(readyTodo(testEndpoint, "inbox"), store.ReadyCreated)
	h.wait(t)
	if n := len(rcv.got()); n != 2 {
		t.Fatalf("worker stalled after a health-write failure: %d deliveries", n)
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	if !strings.Contains(out, "notify hook health update failed") || !strings.Contains(out, "hook=h1") ||
		!strings.Contains(out, first.NotificationID) {
		t.Fatalf("health failure not logged with the hook and notification ids:\n%s", out)
	}
}

// REQ-11 "Secret never logged", across a rotation: neither secret, no signature and no query.
func TestSecretsNeverLoggedAcrossRotation(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	rcv := newReceiver(t, http.StatusBadGateway, http.StatusNoContent)
	st := newMemStore()
	current := st.addHook(t, testEndpoint, "h1", rcv.hookURL("/sb?token=querysecret"))
	previous, _ := MintSecret()
	st.mu.Lock()
	st.secrets["h1"] = store.NotifyHookSecrets{Current: current, Previous: previous}
	st.mu.Unlock()
	h := newHarness(t, st, rcv, func(o *Options) { o.Log = newLockedLogger(&logs, &logMu) })
	h.d.Enqueue(readyTodo(testEndpoint, "inbox"), store.ReadyCreated)
	h.wait(t)

	leaks := []string{current, previous, strings.TrimPrefix(current, "whsec_"), strings.TrimPrefix(previous, "whsec_"), "querysecret"}
	for _, r := range rcv.got() {
		for _, e := range strings.Fields(r.header.Get("webhook-signature")) {
			leaks = append(leaks, strings.TrimPrefix(e, "v1,"))
		}
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	for _, leak := range leaks {
		if strings.Contains(out, leak) {
			t.Fatalf("logs leak %q:\n%s", leak, out)
		}
	}
	if strings.Count(out, "notify hook attempt") != 2 {
		t.Fatalf("want one structured line per attempt:\n%s", out)
	}
}
