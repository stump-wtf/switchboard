package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// testSSEHandler builds a Handler with just enough plumbing for the SSE surface: no DB (the retry
// interval is stubbed) and fast keep-alives so streaming tests finish in milliseconds.
func testSSEHandler(retryMS int, keepAlive time.Duration) *Handler {
	return &Handler{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		events:     newEventHub(),
		keepAlive:  keepAlive,
		sseRetryMS: func(context.Context) int { return retryMS },
	}
}

// runEvents drives the streaming handler until the request context expires and returns the
// recorder. publish (optional) runs concurrently against the hub only — never the recorder.
func runEvents(t *testing.T, h *Handler, d time.Duration, publish func()) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	ctx, cancel := context.WithTimeout(req.Context(), d)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	if publish != nil {
		go func() { defer close(done); publish() }()
	} else {
		close(done)
	}
	h.Events(rec, req) // blocks until ctx expires
	<-done
	return rec
}

// Governing: SPEC-0012 REQ "Live Updates via SSE" — streaming headers, initial retry: frame,
// periodic keep-alive comments.
func TestEventsStreamingHeadersRetryAndKeepAlive(t *testing.T) {
	h := testSSEHandler(3000, 5*time.Millisecond)
	rec := runEvents(t, h, 60*time.Millisecond, nil)

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "retry: 3000\n\n") {
		t.Fatalf("stream does not open with a retry: frame:\n%q", body)
	}
	if !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("stream has no keep-alive comment:\n%q", body)
	}
	if !rec.Flushed {
		t.Fatal("handler never flushed the stream")
	}
}

// Governing: SPEC-0013 REQ "Live Updates and Toasts" — a committed transition arrives as its
// TYPED named event carrying the pre-rendered fragment set. Attacker-influenced fields (source,
// event type from webhook payloads) must arrive HTML-escaped and newline-stripped, so a payload
// can neither inject markup into the live region nor forge extra SSE frames.
func TestEventsDeliversPublishedTodoTransition(t *testing.T) {
	h := newTestHandler(t)  // full template set (no store: counts frames are skipped)
	h.keepAlive = time.Hour /* isolate: no keep-alives */
	h.sseRetryMS = func(context.Context) int { return 1500 }
	rec := runEvents(t, h, 250*time.Millisecond, func() {
		time.Sleep(20 * time.Millisecond) // let the handler subscribe + write the retry frame
		h.PublishTodoTransition("claimed", store.Todo{
			ID: "td_1", Queue: "ci", Source: "ci\nx <img>", Kind: "push", State: "claimed",
			Owner: "agent:0a1b2c3d", CreatedAt: time.Now(),
		})
	})

	body := rec.Body.String()
	if !strings.HasPrefix(body, "retry: 1500\n\n") {
		t.Fatalf("retry frame missing or wrong:\n%q", body)
	}
	if !strings.Contains(body, "event: todo_claimed\n") {
		t.Fatalf("typed todo_claimed event not delivered:\n%q", body)
	}
	if !strings.Contains(body, "ci x &lt;img&gt;") {
		t.Fatalf("payload not escaped/sanitized:\n%q", body)
	}
	if !strings.Contains(body, "claimed · agent · 0a1b2c3") {
		t.Fatalf("lifecycle stage fragment missing:\n%q", body)
	}
	if !strings.Contains(body, "td_1 · claimed") {
		t.Fatalf("toast with the todo id missing:\n%q", body)
	}
	if strings.Contains(body, "<img>") || strings.Contains(body, "ci\nx") {
		t.Fatalf("raw attacker input leaked into the stream:\n%q", body)
	}
}

// Governing: SPEC-0012 — SSE is best-effort presentation. A slow consumer's full buffer drops
// events without blocking the publisher and without corrupting anything: the database (rendered on
// reload) remains the authoritative state, so nothing here needs repair afterwards.
func TestEventHubDropsOnFullBufferWithoutBlocking(t *testing.T) {
	hub := newEventHub()
	ch, cancel, err := hub.subscribe("h1", "session-a")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < sseBufferSize+10; i++ { // overflow the bounded buffer
			hub.Publish(Event{Name: "todo", Data: fmt.Sprintf("n%d", i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	if got := len(ch); got != sseBufferSize {
		t.Fatalf("buffered %d events, want exactly the bounded %d (rest dropped)", got, sseBufferSize)
	}
	// Publishing after the drop still works — the stream is not wedged by the overflow.
	hubDrain(ch)
	hub.Publish(Event{Name: "todo", Data: "after-drop"})
	if e := <-ch; e.Data != "after-drop" {
		t.Fatalf("post-drop publish delivered %q", e.Data)
	}
}

// Governing: SPEC-0012/0013 (#186) — the hub scopes frames to the owning human's streams. An
// Owner-stamped frame must reach ONLY subscribers authenticated as that human; a frame with no
// owner (single-tenant board data — see live.go's ownership-routing note) reaches everyone.
func TestEventHubScopesOwnedFramesToOwningHuman(t *testing.T) {
	hub := newEventHub()
	aliceCh, cancelA, err := hub.subscribe("human-alice", "sess-a")
	if err != nil {
		t.Fatalf("subscribe alice: %v", err)
	}
	defer cancelA()
	// A second stream for the same human (another tab) must also receive her scoped frames.
	aliceCh2, cancelA2, err := hub.subscribe("human-alice", "sess-a2")
	if err != nil {
		t.Fatalf("subscribe alice tab 2: %v", err)
	}
	defer cancelA2()
	bobCh, cancelB, err := hub.subscribe("human-bob", "sess-b")
	if err != nil {
		t.Fatalf("subscribe bob: %v", err)
	}
	defer cancelB()

	hub.Publish(Event{Name: "endpoint_seen", Data: "alice-endpoint", Owner: "human-alice"})
	for i, ch := range []<-chan Event{aliceCh, aliceCh2} {
		select {
		case e := <-ch:
			if e.Data != "alice-endpoint" {
				t.Fatalf("alice stream %d got %q", i+1, e.Data)
			}
		default:
			t.Fatalf("alice stream %d did not receive her scoped frame", i+1)
		}
	}
	select {
	case e := <-bobCh:
		t.Fatalf("bob received alice's scoped frame: %+v", e)
	default: // correct: another human's stream never carries an owned frame
	}

	// An unowned frame (counts, feed rows — no per-human ownership edge) reaches every subscriber.
	hub.Publish(Event{Name: "counts", Data: "global"})
	for _, ch := range []<-chan Event{aliceCh, aliceCh2, bobCh} {
		select {
		case e := <-ch:
			if e.Data != "global" {
				t.Fatalf("broadcast frame corrupted: %q", e.Data)
			}
		default:
			t.Fatal("broadcast frame missing from a subscriber")
		}
	}
}

func hubDrain(ch <-chan Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestEventHubCapsStreamsPerSession(t *testing.T) {
	hub := newEventHub()
	cancels := make([]func(), 0, maxStreamsPerSession)
	for i := 0; i < maxStreamsPerSession; i++ {
		_, cancel, err := hub.subscribe("h1", "same-session")
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		cancels = append(cancels, cancel)
	}
	if _, _, err := hub.subscribe("h1", "same-session"); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("over-cap subscribe err = %v, want ErrStreamLimit", err)
	}
	// A different session is unaffected by another session's cap.
	if _, cancel, err := hub.subscribe("h2", "other-session"); err != nil {
		t.Fatalf("other session subscribe: %v", err)
	} else {
		cancel()
	}
	// Releasing a stream frees capacity; cancel is idempotent.
	cancels[0]()
	cancels[0]()
	if _, cancel, err := hub.subscribe("h1", "same-session"); err != nil {
		t.Fatalf("subscribe after release: %v", err)
	} else {
		cancel()
	}
}

func TestEventsOverStreamCapReturnsGeneric429(t *testing.T) {
	h := testSSEHandler(3000, time.Hour)
	// Exhaust the (unauthenticated-test) session key's cap directly on the hub.
	for i := 0; i < maxStreamsPerSession; i++ {
		if _, _, err := h.events.subscribe("", ""); err != nil {
			t.Fatalf("prefill %d: %v", i, err)
		}
	}
	rec := httptest.NewRecorder()
	h.Events(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ErrStreamLimit") {
		t.Fatalf("response leaks internal error detail: %q", rec.Body.String())
	}
}
