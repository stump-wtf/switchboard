// Hub fans newly-accepted todos out to in-process subscribers, filtered by OWNING ENDPOINT first
// and queue second. It is a lossy, best-effort doorbell — the durable PostgreSQL queue is always
// the ledger, so a dropped nudge only costs latency, never work.
//
// The endpoint dimension is a tenant boundary, not a convenience filter. Filtering on queue alone
// made this the same cross-tenant leak as the pre-ADR-0022 todos table: any two subscribers
// watching a common queue string ("github", "reviews") saw each other's todos — including the
// payload — across humans. Queue name is a human-readable label and a secondary intra-endpoint
// filter; it is NOT a tenant boundary.
//
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)",
// SPEC-0011 REQ "Scope-Filtered Fan-Out".
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

// Hub fans newly-created todos out to subscribed consumers, scoped to one endpoint and filtered by
// queue within it. Lossy by design.
type Hub struct {
	mu   sync.RWMutex
	subs map[int]*subscriber
	next int
}

type subscriber struct {
	endpointID string // tenant scope; a subscriber only ever sees this endpoint's todos
	queues     map[string]bool
	ch         chan store.Todo
}

// NewHub builds an empty Hub.
func NewHub() *Hub { return &Hub{subs: map[int]*subscriber{}} }

// Subscribe returns a channel of todos OWNED BY endpointID in the given queues, plus an
// unsubscribe func. An empty endpointID subscribes to nothing: there is no unscoped subscription,
// because an unscoped subscription is precisely the cross-tenant leak (ADR-0022). Callers that
// legitimately observe every tenant are operator surfaces and MUST read from the store's
// operator-scoped paths instead.
func (h *Hub) Subscribe(endpointID string, queues []string) (<-chan store.Todo, func()) {
	s := &subscriber{endpointID: endpointID, queues: map[string]bool{}, ch: make(chan store.Todo, 32)}
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

// Publish fans a todo out to subscribers that own it AND whose queue scope includes it. Never
// blocks. The endpoint check is first and unconditional: a todo with an empty EndpointID (which
// the NOT NULL column makes impossible for a persisted row) matches no subscriber, so a
// half-populated struct fails closed rather than fanning out to everyone.
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func (h *Hub) Publish(t store.Todo) {
	if t.EndpointID == "" {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subs {
		if s.endpointID == t.EndpointID && s.queues[t.Queue] {
			select {
			case s.ch <- t:
			default: // slow consumer: drop — the queue is the ledger, this is a doorbell
			}
		}
	}
}
