// Hub fans newly-accepted todos out to in-process subscribers, filtered by queue. It is a lossy,
// best-effort doorbell — the durable PostgreSQL queue is always the ledger, so a dropped nudge only
// costs latency, never work.
//
// It moved here from the retired internal/agentapi package when the bespoke /agent/* REST + SSE
// surface was deleted (ADR-0017; SPEC-0014): ingest is now its sole owner. The production channel
// push to live MCP sessions flows through the store's committed-transition doorbell hook
// (store.SetTodoDoorbellHook → mcp.PublishTodoReady); this Hub remains the ingest-local seam the
// accept-path tests observe to assert "published exactly once, only when newly created".
package ingest

import (
	"sync"

	"github.com/joestump/switchboard/internal/store"
)

// Hub fans newly-created todos out to subscribed consumers, filtered by queue. Lossy by design.
type Hub struct {
	mu   sync.RWMutex
	subs map[int]*subscriber
	next int
}

type subscriber struct {
	queues map[string]bool
	ch     chan store.Todo
}

// NewHub builds an empty Hub.
func NewHub() *Hub { return &Hub{subs: map[int]*subscriber{}} }

// Subscribe returns a channel of todos in the given queues plus an unsubscribe func.
func (h *Hub) Subscribe(queues []string) (<-chan store.Todo, func()) {
	s := &subscriber{queues: map[string]bool{}, ch: make(chan store.Todo, 32)}
	for _, q := range queues {
		s.queues[q] = true
	}
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = s
	h.mu.Unlock()
	return s.ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(s.ch)
		}
		h.mu.Unlock()
	}
}

// Publish fans a todo out to subscribers whose scope includes its queue. Never blocks.
func (h *Hub) Publish(t store.Todo) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subs {
		if s.queues[t.Queue] {
			select {
			case s.ch <- t:
			default: // slow consumer: drop — the queue is the ledger, this is a doorbell
			}
		}
	}
}
