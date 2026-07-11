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

	"github.com/joestump/switchboard/internal/store"
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

func TestEventsDeliversPublishedTodoTransition(t *testing.T) {
	h := testSSEHandler(1500, time.Hour /* isolate: no keep-alives */)
	rec := runEvents(t, h, 80*time.Millisecond, func() {
		time.Sleep(20 * time.Millisecond) // let the handler subscribe + write the retry frame
		// Queue name is attacker-influenced: must arrive HTML-escaped and newline-stripped, so a
		// payload can neither inject markup into the live region nor forge extra SSE frames.
		h.PublishTodoTransition("claimed", store.Todo{Queue: "ci\nx <img>"})
	})

	body := rec.Body.String()
	if !strings.HasPrefix(body, "retry: 1500\n\n") {
		t.Fatalf("retry frame missing or wrong:\n%q", body)
	}
	if !strings.Contains(body, "event: todo\n") {
		t.Fatalf("todo event not delivered:\n%q", body)
	}
	if !strings.Contains(body, "todo claimed · ci x &lt;img&gt;") {
		t.Fatalf("payload not escaped/sanitized:\n%q", body)
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
	ch, cancel, err := hub.subscribe("session-a")
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
		_, cancel, err := hub.subscribe("same-session")
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		cancels = append(cancels, cancel)
	}
	if _, _, err := hub.subscribe("same-session"); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("over-cap subscribe err = %v, want ErrStreamLimit", err)
	}
	// A different session is unaffected by another session's cap.
	if _, cancel, err := hub.subscribe("other-session"); err != nil {
		t.Fatalf("other session subscribe: %v", err)
	} else {
		cancel()
	}
	// Releasing a stream frees capacity; cancel is idempotent.
	cancels[0]()
	cancels[0]()
	if _, cancel, err := hub.subscribe("same-session"); err != nil {
		t.Fatalf("subscribe after release: %v", err)
	} else {
		cancel()
	}
}

func TestEventsOverStreamCapReturnsGeneric429(t *testing.T) {
	h := testSSEHandler(3000, time.Hour)
	// Exhaust the (unauthenticated-test) session key's cap directly on the hub.
	for i := 0; i < maxStreamsPerSession; i++ {
		if _, _, err := h.events.subscribe(""); err != nil {
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
