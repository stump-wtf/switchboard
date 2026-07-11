// SSE live updates for the web UI.
//
// Governing: SPEC-0012 REQ "Live Updates via SSE" — a plain net/http handler using http.Flusher
// (no SSE library), subscribed from the DOM by the vendored htmx-ext-sse extension. Delivery is
// best-effort presentation: buffers are bounded and drop-on-full, because PostgreSQL is the
// authoritative state and a reload always renders current truth. The SPEC-0013 typed event
// taxonomy (live.go) layers on top of this hub; this file is the baseline transport.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/joestump/switchboard/internal/auth"
)

const (
	// sseBufferSize bounds each subscriber's channel; a slow consumer drops events, never blocks.
	sseBufferSize = 16
	// maxStreamsPerSession caps concurrent SSE streams per browser session (multi-tab headroom
	// without letting one session pin unlimited server connections).
	maxStreamsPerSession = 4
	// defaultSSERetryMS mirrors the seeded settings row 'sse_retry_ms' (0001_init.sql).
	defaultSSERetryMS = 3000
	// defaultKeepAlive is the interval between keep-alive comments holding the connection open.
	defaultKeepAlive = 15 * time.Second
)

// ErrStreamLimit is returned by subscribe when a session already holds the maximum number of
// concurrent event streams.
var ErrStreamLimit = errors.New("web: too many concurrent event streams for session")

// Event is one server-sent event: a name the DOM subscribes to via sse-swap, and a single-line
// HTML fragment payload swapped into the live region.
type Event struct {
	Name string
	Data string
}

// EventHub fans UI events out to connected humans' SSE streams. Lossy by design (bounded buffers,
// drop-on-full): the hub is a doorbell for the presentation layer, not a ledger.
type EventHub struct {
	mu   sync.Mutex
	subs map[int]*sseSub
	next int
}

type sseSub struct {
	session string
	ch      chan Event
}

func newEventHub() *EventHub { return &EventHub{subs: map[int]*sseSub{}} }

// subscribe registers a stream keyed by the caller's session, enforcing maxStreamsPerSession.
// The returned cancel func is idempotent and closes the channel.
func (h *EventHub) subscribe(session string) (<-chan Event, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.subs {
		if s.session == session {
			n++
		}
	}
	if n >= maxStreamsPerSession {
		return nil, nil, ErrStreamLimit
	}
	sub := &sseSub{session: session, ch: make(chan Event, sseBufferSize)}
	id := h.next
	h.next++
	h.subs[id] = sub
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(sub.ch)
		}
	}
	return sub.ch, cancel, nil
}

// Publish fans an event out to every subscriber. Never blocks: a full buffer drops the event
// (SPEC-0012 — a missed event costs a swap, never state; reload renders DB truth).
func (h *EventHub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		select {
		case s.ch <- e:
		default: // slow consumer: drop — best-effort presentation
		}
	}
}

// Events streams live UI updates over SSE. Requires human (mounted behind RequireHuman).
// Governing: SPEC-0012 REQ "Live Updates via SSE" — streaming headers, an initial retry: frame
// advertising the client reconnect interval, and periodic keep-alive comments.
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.fail(w, errors.New("sse: response writer does not support streaming"))
		return
	}
	// The per-session CSRF token is a stable, non-guessable session key — ideal for capping
	// concurrent streams per session without touching the raw session cookie.
	ch, cancel, err := h.events.subscribe(auth.CSRFFromContext(r.Context()))
	if err != nil {
		h.log.Warn("sse subscribe", "err", err)
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: %d\n\n", h.sseRetryMS(r.Context()))
	flusher.Flush()

	keepAlive := time.NewTicker(h.keepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sseSanitize(e.Name), sseSanitize(e.Data))
			flusher.Flush()
		}
	}
}

// sseRetrySetting reads the client reconnect interval from the settings table, falling back to
// the seeded default when the row is missing or unreadable (the error is logged, never leaked —
// SPEC-0012 REQ "Error Handling and Server-Side Logging").
func (h *Handler) sseRetrySetting(ctx context.Context) int {
	v, err := h.store.SettingInt(ctx, "sse_retry_ms", defaultSSERetryMS)
	if err != nil {
		h.log.Error("sse retry setting", "err", err)
	}
	return v
}

// sseSanitize collapses CR/LF so no event name or payload can terminate a frame early or inject
// fields into the SSE stream.
func sseSanitize(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}
