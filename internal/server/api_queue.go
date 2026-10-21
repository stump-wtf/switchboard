package server

// Human API: Per-Queue Pause
//
// POST /api/v1/queues/{queue}/pause and POST /api/v1/queues/{queue}/resume quiesce and restart
// delivery on one queue without touching intake: an operator draining a downstream consumer keeps
// accepting webhooks (todos persist, nothing is dropped or failed) while claim, claim_next, the
// channel doorbell and the ready-hook wakes all treat the queue as empty. Resume re-rings the
// backlog. Queues are a global namespace (SPEC-0003), so the pause is operator-wide, not scoped to
// one endpoint's copy of a name — pausing is saying "this queue stops delivering", everywhere.
//
// GET /api/v1/queues lists every known queue with its pending/claimed counts and its paused flag,
// so "why is nothing moving" answers itself.
//
// The routes follow SPEC-0035's conventions: errors are {"error", "code"}; writes are capped and
// draw from the per-human write bucket; the quarantine queue — already never delivered — refuses a
// pause with invalid_argument.
//
// Governing: ADR-0048 (proposed), SPEC-0035 REQ "Human API Surface", ADR-0007.
//
// @joestump-agent 10/20/2026 - Added for stump.wtf/switchboard#581.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/store"
)

// pauseQueueIn is the pause body: an optional reason the board and the audit trail can show.
type pauseQueueIn struct {
	Reason string `json:"reason"`
}

// queueRow is one queue's listing row.
type queueRow struct {
	Queue    string `json:"queue"`
	Pending  int64  `json:"pending"`
	Claimed  int64  `json:"claimed"`
	Paused   bool   `json:"paused"`
	Reason   string `json:"reason,omitempty"`
	PausedAt string `json:"paused_at,omitempty"`
}

// ListQueues is GET /api/v1/queues: every known queue, its live counts, and whether it is paused.
func (a *apiHandler) ListQueues(w http.ResponseWriter, r *http.Request) {
	stats, err := a.st.QueueStats(r.Context())
	if err != nil {
		a.log.Error("api: queue list", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "queue list failed", nil)
		return
	}
	paused, err := a.st.ListQueuePauses(r.Context())
	if err != nil {
		a.log.Error("api: pause list", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "queue list failed", nil)
		return
	}
	byQueue := make(map[string]store.QueuePause, len(paused))
	for _, p := range paused {
		byQueue[p.Queue] = p
	}
	out := make([]queueRow, 0, len(stats))
	for _, s := range stats {
		row := queueRow{Queue: s.Queue, Pending: s.Pending, Claimed: s.Claimed, Paused: s.Paused}
		if p, ok := byQueue[s.Queue]; ok {
			row.Reason = p.Reason
			row.PausedAt = p.PausedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"queues": out}); err != nil {
		a.log.Error("api: queue list encode", "err", err)
	}
}

// PauseQueue is POST /api/v1/queues/{queue}/pause: {"reason": "..."} (body optional). Idempotent:
// pausing an already-paused queue refreshes the reason and answers 200 either way.
func (a *apiHandler) PauseQueue(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	queue := strings.TrimSpace(chi.URLParam(r, "queue"))
	if queue == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "queue name must not be empty", nil)
		return
	}
	var in pauseQueueIn
	if r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_argument", "request body is too large", nil)
				return
			}
			writeAPIError(w, http.StatusBadRequest, "invalid_argument", "invalid JSON body: "+err.Error(), nil)
			return
		}
	}
	if queue == store.QueueQuarantine {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument",
			"quarantine is never delivered, so it cannot be paused", nil)
		return
	}
	err := a.st.PauseQueue(r.Context(), queue, human.ID, in.Reason)
	if err != nil {
		a.log.Error("api: pause queue", "queue", queue, "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "pause failed", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"queue": queue, "paused": true})
}

// ResumeQueue is POST /api/v1/queues/{queue}/resume: lifts the pause and re-rings the backlog.
// Resuming a queue that was never paused is not an error — the queue simply was not paused — and
// the response says whether a pause was actually lifted.
func (a *apiHandler) ResumeQueue(w http.ResponseWriter, r *http.Request) {
	queue := strings.TrimSpace(chi.URLParam(r, "queue"))
	if queue == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "queue name must not be empty", nil)
		return
	}
	resumed, err := a.st.ResumeQueue(r.Context(), queue)
	if err != nil {
		a.log.Error("api: resume queue", "queue", queue, "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "resume failed", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"queue": queue, "paused": false, "was_paused": resumed})
}
