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

	"github.com/stump-wtf/switchboard/internal/auth"
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
	// Owner scopes delivery to one human's streams (a human id): the hub routes an owned frame
	// only to subscribers authenticated as that human, so one human's data never rides another's
	// stream (SPEC-0012/0013 — the event stream is scoped to the human's own data). Empty routes
	// to every authenticated subscriber; live.go documents which frames legitimately broadcast
	// (data with no per-human ownership edge in today's schema).
	Owner string
}

// EventHub fans UI events out to connected humans' SSE streams. Lossy by design (bounded buffers,
// drop-on-full): the hub is a doorbell for the presentation layer, not a ledger. Subscribers are
// keyed by the owning human (frame routing) and their browser session (the stream cap).
type EventHub struct {
	mu   sync.Mutex
	subs map[int]*sseSub
	next int
}

type sseSub struct {
	human   string // owning human id — routes Owner-scoped frames
	session string // browser session key — enforces maxStreamsPerSession
	ch      chan Event
}

func newEventHub() *EventHub { return &EventHub{subs: map[int]*sseSub{}} }

// subscribe registers a stream owned by human, keyed by the caller's session for the
// maxStreamsPerSession cap. The returned cancel func is idempotent and closes the channel.
func (h *EventHub) subscribe(human, session string) (<-chan Event, func(), error) {
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
	sub := &sseSub{human: human, session: session, ch: make(chan Event, sseBufferSize)}
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

// humans returns the distinct human ids that currently hold a stream, so a per-human render loop
// costs one pass per CONNECTED human rather than per row in the humans table — and costs nothing
// at all when nobody is watching.
//
// A human with three tabs open appears once; Publish fans the single frame to all three streams.
func (h *EventHub) humans() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[string]struct{}, len(h.subs))
	out := make([]string, 0, len(h.subs))
	for _, s := range h.subs {
		if s.human == "" {
			continue
		}
		if _, dup := seen[s.human]; dup {
			continue
		}
		seen[s.human] = struct{}{}
		out = append(out, s.human)
	}
	return out
}

// Publish fans an event out to its audience: every subscriber for an unowned frame, only the
// owning human's streams for an Owner-scoped one. Never blocks: a full buffer drops the event
// (SPEC-0012 — a missed event costs a swap, never state; reload renders DB truth).
func (h *EventHub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if e.Owner != "" && s.human != e.Owner {
			continue // scoped frame: another human's stream never carries it
		}
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
	// The stream is registered under the authenticated human (RequireHuman guarantees one), so
	// the hub can route owner-scoped frames to exactly this human's streams. The per-session CSRF
	// token is a stable, non-guessable session key — ideal for capping concurrent streams per
	// session without touching the raw session cookie.
	human, _ := auth.FromContext(r.Context())
	ch, cancel, err := h.events.subscribe(human.ID, auth.CSRFFromContext(r.Context()))
	if err != nil {
		h.log.Warn("sse subscribe", "err", err)
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", h.sseRetryMS(r.Context())); err != nil {
		return
	}
	flusher.Flush()

	keepAlive := time.NewTicker(h.keepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sseSanitize(e.Name), sseSanitize(e.Data)); err != nil {
				return
			}
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
