package mcp

// Operator queue supervision over MCP
//
// The operator half of the MCP surface: read a queue's health in one call, and quiesce, wake, retry
// or take over a lease without leaving the session. The consumer surface (tools.go) is a worker's
// hand and eye on its own lane; this file is a supervisor's, and the two are separated by the grant,
// not by politeness: the verbs here belong to no family a consumer verb implies and to no default
// grant (verbs.go), so a worker credential cannot reach them unless a vend names them or their
// preset.
//
// Everything here is queue-scoped by the endpoint's granted queues and tenant-scoped by the human
// who owns the endpoint: an operator supervises the queues it was granted, and only the rows its own
// human owns — the same boundary the Board enforces (SPEC-0007, ADR-0022). Pause, resume and drain
// drive ADR-0048's one pause switch; there is no second quiesce mechanism to fall out of step with.
// A requeue is the Board's "Retry now"; a lease release is its "Release".
//
// Every state-changing call writes one audit line naming the endpoint slug, the human behind it, the
// target and the operator's reason. Governing: ADR-0056, ADR-0048, ADR-0042, ADR-0047.

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

// queueOperatorVerbs is the operator supervision family (verbs.go QueueOperatorVerbs), held as a set
// so scopeGuard can tell "a supervision verb this endpoint was not granted" (forbidden) from "not a
// verb at all" (the SDK's unknown-tool error).
var queueOperatorVerbs = verbSet(QueueOperatorVerbs())

// drainWaitDefaultSeconds and drainWaitMaxSeconds bound drain_queue's wait: the default is a short
// pause-and-check, the ceiling keeps one tool call from holding a session open while a queue refuses
// to settle. A drain that runs out of wait has still paused the queue, and says so.
const (
	drainWaitDefaultSeconds = 30
	drainWaitMaxSeconds     = 300
	// drainPollInterval is how often a waiting drain re-reads the outstanding leases. The leases it
	// is waiting on are minutes long; a faster poll buys nothing and streams reads at the database.
	drainPollInterval = 2 * time.Second
	// stalledAfterDefaultSeconds is the age at which pending work with nothing claimed and no pause
	// reads as stalled rather than merely young. It is a default an operator may override per call;
	// the verdict it produces is documented in ADR-0056.
	stalledAfterDefaultSeconds = 300
	// superviseLeaseLimit is how many held leases one stats snapshot carries: the count of claimed
	// work is always exact, the list is a sample with a stated ceiling.
	superviseLeaseLimit = 50
)

// queueSupervisionStore is the seam this surface reads and writes through. *store.Store satisfies it;
// fakes need not carry it, so an endpoint that was never granted a supervision verb never meets it.
type queueSupervisionStore interface {
	QueueLivenessForHuman(ctx context.Context, ownerHumanID string, queues []string) ([]store.QueueLiveness, error)
	OutstandingLeasesForHuman(ctx context.Context, ownerHumanID string, queues []string, limit int) ([]store.HeldLease, error)
	PauseQueue(ctx context.Context, queue, pausedBy, reason string) error
	ResumeQueue(ctx context.Context, queue string) (bool, error)
	GetTodoItem(ctx context.Context, ownerHumanID, id string) (store.TodoItem, error)
	RetryTodoOperatorOwned(ctx context.Context, ownerHumanID, id string) (store.Todo, error)
	ReleaseTodoOperatorOwned(ctx context.Context, ownerHumanID, id, owner string) (store.Todo, error)
}

// registerQueueOperatorTools installs the supervision verbs this endpoint was granted. A verb no
// grant names is never registered, so tools/list cannot advertise it (SPEC-0014 REQ "Agent Tool
// Surface over MCP").
func (h *Handler) registerQueueOperatorTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "queue_stats") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "queue_stats",
			Description: "Supervise the granted queues in one call: depth by state and the age of the " +
				"oldest pending work per queue, whether each is paused, a verdict that tells a stalled " +
				"queue from a merely quiet one, and the leases outstanding on them with their expiry. " +
				"Counts cover the rows the endpoint's human owns, never another tenant's.",
		}, h.queueStatsTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "pause_queue") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "pause_queue",
			Description: "Quiesce delivery on one granted queue (ADR-0048): intake keeps accepting and " +
				"storing todos, nothing is handed out, and nothing is lost or spent. It is the same " +
				"pause the Board sets, not a second switch.",
		}, h.pauseQueueTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "resume_queue") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "resume_queue",
			Description: "Lift the pause on one granted queue and re-ring its backlog. Resuming a queue " +
				"that was never paused is a no-op, and the result says so.",
		}, h.resumeQueueTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "drain_queue") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "drain_queue",
			Description: "Pause one granted queue and report the leases still outstanding on it. With " +
				"wait_seconds it returns when the last lease lands or when the wait runs out, leaving the " +
				"queue paused either way. It releases nothing itself.",
		}, h.drainQueueTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "requeue_todo") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "requeue_todo",
			Description: "Move one failed or dead-lettered todo in a granted queue back to pending with " +
				"a fresh attempt budget. A claimed, pending or done todo is refused with conflict and left " +
				"untouched; the fresh budget is what makes a dead letter runnable again.",
		}, h.requeueTodoTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "release_lease") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "release_lease",
			Description: "Hand a todo in a granted queue back to the queue from under the lease its " +
				"holder holds, for a run that was killed or has gone away. The attempt closes as released " +
				"with your reason; a row that is not currently claimed is refused and untouched.",
		}, h.releaseLeaseTool(ep))
	}
}

// --- inputs and outputs ---

type queueStatsIn struct {
	Queue string `json:"queue,omitempty" jsonschema:"restrict the snapshot to one granted queue (default: every granted queue)"`
	// StalledAfterSeconds re-tunes the stalled verdict for a queue whose work is normally slow; it
	// changes the label on the row, never the counts.
	StalledAfterSeconds *int `json:"stalled_after_seconds,omitempty" jsonschema:"pending work older than this many seconds with nothing claimed and no pause reads as stalled (default 300)"`
	LeaseLimit          *int `json:"lease_limit,omitempty" jsonschema:"how many outstanding leases to list (default 50, max 200); the claimed count is always exact"`
}

type queueStatRow struct {
	Queue             string  `json:"queue" jsonschema:"the queue these counts describe"`
	Pending           int     `json:"pending" jsonschema:"rows waiting for a consumer"`
	Claimed           int     `json:"claimed" jsonschema:"rows under a live lease"`
	Done              int     `json:"done" jsonschema:"rows completed"`
	Failed            int     `json:"failed" jsonschema:"rows failed, retrying or dead"`
	Blocked           int     `json:"blocked" jsonschema:"rows held by an issue-dependency hold"`
	DeadLettered      int     `json:"dead_lettered" jsonschema:"failed rows with no scheduled retry"`
	OldestPendingAgeS float64 `json:"oldest_pending_seconds" jsonschema:"age of the oldest pending row, 0 when none is pending"`
	Paused            bool    `json:"paused" jsonschema:"delivery is quiesced by decision (ADR-0048)"`
	Stalled           bool    `json:"stalled" jsonschema:"pending work is older than the threshold while nothing is claimed and the queue is not paused"`
	Verdict           string  `json:"verdict" jsonschema:"one of paused, stalled, working, waiting, quiet"`
}

type leaseRow struct {
	TodoID           string   `json:"todo_id" jsonschema:"the todo under this lease"`
	Queue            string   `json:"queue" jsonschema:"the queue the todo sits on"`
	Owner            string   `json:"owner" jsonschema:"the claimant holding the lease"`
	Attempt          int      `json:"attempt" jsonschema:"attempt count the next claim will follow"`
	LeaseExpiresAt   *string  `json:"lease_expires_at,omitempty" jsonschema:"RFC 3339 expiry; absent when no expiry was written"`
	ExpiresInSeconds *float64 `json:"expires_in_seconds,omitempty" jsonschema:"seconds until expiry, negative once the lease is overdue"`
}

type queueStatsOut struct {
	Queues          []queueStatRow `json:"queues" jsonschema:"one row per queue, sorted by name"`
	Leases          []leaseRow     `json:"leases,omitempty" jsonschema:"outstanding leases, soonest expiring first"`
	LeasesListed    int            `json:"leases_listed" jsonschema:"how many of the outstanding leases are listed here"`
	LeasesTruncated bool           `json:"leases_truncated" jsonschema:"more leases are outstanding than the listing carries"`
}

type queueActionIn struct {
	Queue  string `json:"queue" jsonschema:"the granted queue to act on"`
	Reason string `json:"reason,omitempty" jsonschema:"why the operator is doing this; recorded in the audit line"`
}

type drainIn struct {
	Queue       string `json:"queue" jsonschema:"the granted queue to quiesce and wait on"`
	Reason      string `json:"reason,omitempty" jsonschema:"why the operator is draining it; recorded in the audit line"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the outstanding leases to land (default 30, max 300); 0 reports them without waiting"`
	LeaseLimit  *int   `json:"lease_limit,omitempty" jsonschema:"how many outstanding leases to report (default 50, max 200)"`
}

type todoActionIn struct {
	ID     string `json:"id" jsonschema:"the todo to act on"`
	Reason string `json:"reason,omitempty" jsonschema:"why the operator is doing this; recorded in the audit line"`
}

type queueActionOut struct {
	Queue           string     `json:"queue,omitempty" jsonschema:"the queue acted on"`
	ID              string     `json:"id,omitempty" jsonschema:"the todo acted on"`
	Paused          bool       `json:"paused" jsonschema:"the queue's pause state after the call"`
	Resumed         bool       `json:"resumed,omitempty" jsonschema:"false when the queue was not paused, so the call changed nothing"`
	Drained         bool       `json:"drained,omitempty" jsonschema:"true when no lease was outstanding when the call returned"`
	WaitedSeconds   *float64   `json:"waited_seconds,omitempty" jsonschema:"how long the drain actually waited"`
	Leases          []leaseRow `json:"leases,omitempty" jsonschema:"the leases still outstanding"`
	LeasesListed    int        `json:"leases_listed,omitempty" jsonschema:"how many outstanding leases are listed"`
	LeasesTruncated bool       `json:"leases_truncated,omitempty" jsonschema:"more leases are outstanding than the listing carries"`
}

// --- registration and guards ---

// supervision returns the store seam or a stable internal error naming the tool: a granted verb on a
// server whose store cannot serve it is a deployment fault, never a silent empty answer.
func (h *Handler) supervision(tool string) (queueSupervisionStore, *toolError) {
	st, ok := h.store.(queueSupervisionStore)
	if !ok {
		h.log.Error("mcp operator supervision seam missing", "tool", tool)
		return nil, &toolError{codeInternal, "operator supervision is not available on this server"}
	}
	return st, nil
}

// guardQueueName enforces the queue grant for a verb that names a queue instead of a todo: the same
// boundary guardQueue enforces by way of the row.
func (h *Handler) guardQueueName(ep store.AuthEndpoint, tool, queue string) *toolError {
	if hasScope(ep.ScopeQueues, queue) {
		return nil
	}
	h.log.Warn("mcp queue out of scope", "slug", ep.Slug, "tool", tool,
		"err", fmt.Errorf("queue %s: %w", queue, errForbidden))
	return &toolError{codeForbidden, "queue " + queue + " not in this endpoint's scope"}
}

// guardSupervisedTodo reads the target row on the operator seam, enforces the queue grant on it, and
// returns it. A row another tenant owns is indistinguishable from an absent one, so the read answers
// not_found — the same shape the Board's drawer uses so it cannot enumerate other tenants' work.
func (h *Handler) guardSupervisedTodo(ctx context.Context, st queueSupervisionStore, ep store.AuthEndpoint, tool, id string) (store.TodoItem, error) {
	item, err := st.GetTodoItem(ctx, ep.OwnerHumanID, id)
	if err != nil {
		return store.TodoItem{}, h.mapStoreErr(ep, tool, err)
	}
	if !hasScope(ep.ScopeQueues, item.Queue) {
		h.log.Warn("mcp todo queue out of scope", "slug", ep.Slug, "tool", tool,
			"err", fmt.Errorf("todo %s in queue %s: %w", id, item.Queue, errForbidden))
		return item, &toolError{codeForbidden, "todo's queue not in this endpoint's scope"}
	}
	return item, nil
}

// auditSupervision writes the one line every state-changing supervision call owes: who acted (the
// endpoint and the human behind its credential), what it did, to what, and why. It carries no
// credential and no payload.
func (h *Handler) auditSupervision(ep store.AuthEndpoint, tool, target, reason string) {
	h.log.Info("mcp operator action", "slug", ep.Slug, "human", ep.OwnerHumanID,
		"agent", owner(ep), "tool", tool, "target", target, "reason", reason)
}

// --- verb handlers ---

func (h *Handler) queueStatsTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[queueStatsIn, queueStatsOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in queueStatsIn) (*sdk.CallToolResult, queueStatsOut, error) {
		st, terr := h.supervision("queue_stats")
		if terr != nil {
			return nil, queueStatsOut{}, terr
		}
		queues := ep.ScopeQueues
		if in.Queue != "" {
			if terr := h.guardQueueName(ep, "queue_stats", in.Queue); terr != nil {
				return nil, queueStatsOut{}, terr
			}
			queues = []string{in.Queue}
		}
		liveness, err := st.QueueLivenessForHuman(ctx, ep.OwnerHumanID, queues)
		if err != nil {
			return nil, queueStatsOut{}, h.mapStoreErr(ep, "queue_stats", err)
		}
		limit := superviseLeaseLimit
		if in.LeaseLimit != nil {
			if *in.LeaseLimit < 0 {
				return nil, queueStatsOut{}, &toolError{codeInvalid, "lease_limit must not be negative"}
			}
			if *in.LeaseLimit > 0 {
				limit = *in.LeaseLimit
			}
		}
		leases, err := st.OutstandingLeasesForHuman(ctx, ep.OwnerHumanID, queues, limit)
		if err != nil {
			return nil, queueStatsOut{}, h.mapStoreErr(ep, "queue_stats", err)
		}
		after := float64(stalledAfterDefaultSeconds)
		if in.StalledAfterSeconds != nil {
			if *in.StalledAfterSeconds < 0 {
				return nil, queueStatsOut{}, &toolError{codeInvalid, "stalled_after_seconds must not be negative"}
			}
			after = float64(*in.StalledAfterSeconds)
		}
		out := queueStatsOut{Queues: make([]queueStatRow, 0, len(liveness)),
			Leases: make([]leaseRow, 0, len(leases)), LeasesListed: len(leases)}
		for _, q := range liveness {
			out.Queues = append(out.Queues, queueStatRow{
				Queue: q.Queue, Pending: q.Pending, Claimed: q.Claimed, Done: q.Done,
				Failed: q.Failed, Blocked: q.Blocked, DeadLettered: q.DeadLettered,
				OldestPendingAgeS: q.OldestPendingSeconds, Paused: q.Paused,
				Stalled: stalled(q, after), Verdict: verdict(q, after),
			})
		}
		for _, l := range leases {
			out.Leases = append(out.Leases, toLeaseRow(l))
		}
		out.LeasesTruncated = len(leases) >= limit
		return nil, out, nil
	}
}

func (h *Handler) pauseQueueTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[queueActionIn, queueActionOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in queueActionIn) (*sdk.CallToolResult, queueActionOut, error) {
		st, terr := h.supervision("pause_queue")
		if terr != nil {
			return nil, queueActionOut{}, terr
		}
		if in.Queue == "" {
			return nil, queueActionOut{}, &toolError{codeInvalid, "queue is required"}
		}
		if terr := h.guardQueueName(ep, "pause_queue", in.Queue); terr != nil {
			return nil, queueActionOut{}, terr
		}
		if err := st.PauseQueue(ctx, in.Queue, owner(ep), in.Reason); err != nil {
			return nil, queueActionOut{}, h.mapStoreErr(ep, "pause_queue", err)
		}
		h.auditSupervision(ep, "pause_queue", "queue="+in.Queue, in.Reason)
		return nil, queueActionOut{Queue: in.Queue, Paused: true}, nil
	}
}

func (h *Handler) resumeQueueTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[queueActionIn, queueActionOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in queueActionIn) (*sdk.CallToolResult, queueActionOut, error) {
		st, terr := h.supervision("resume_queue")
		if terr != nil {
			return nil, queueActionOut{}, terr
		}
		if in.Queue == "" {
			return nil, queueActionOut{}, &toolError{codeInvalid, "queue is required"}
		}
		if terr := h.guardQueueName(ep, "resume_queue", in.Queue); terr != nil {
			return nil, queueActionOut{}, terr
		}
		resumed, err := st.ResumeQueue(ctx, in.Queue)
		if err != nil {
			return nil, queueActionOut{}, h.mapStoreErr(ep, "resume_queue", err)
		}
		h.auditSupervision(ep, "resume_queue", "queue="+in.Queue, in.Reason)
		return nil, queueActionOut{Queue: in.Queue, Paused: false, Resumed: resumed}, nil
	}
}

func (h *Handler) drainQueueTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[drainIn, queueActionOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in drainIn) (*sdk.CallToolResult, queueActionOut, error) {
		st, terr := h.supervision("drain_queue")
		if terr != nil {
			return nil, queueActionOut{}, terr
		}
		if in.Queue == "" {
			return nil, queueActionOut{}, &toolError{codeInvalid, "queue is required"}
		}
		if terr := h.guardQueueName(ep, "drain_queue", in.Queue); terr != nil {
			return nil, queueActionOut{}, terr
		}
		wait := drainWaitDefaultSeconds
		if in.WaitSeconds != nil {
			if *in.WaitSeconds < 0 || *in.WaitSeconds > drainWaitMaxSeconds {
				return nil, queueActionOut{}, &toolError{codeInvalid,
					fmt.Sprintf("wait_seconds must be between 0 and %d", drainWaitMaxSeconds)}
			}
			wait = *in.WaitSeconds
		}
		limit := superviseLeaseLimit
		if in.LeaseLimit != nil && *in.LeaseLimit > 0 {
			limit = *in.LeaseLimit
		}
		// Pause first, always: a drain that reported the leases and left the queue delivering would
		// be a snapshot, not a drain. ADR-0048's switch is what makes the wait terminate.
		if err := st.PauseQueue(ctx, in.Queue, owner(ep), drainReason(in.Reason)); err != nil {
			return nil, queueActionOut{}, h.mapStoreErr(ep, "drain_queue", err)
		}
		h.auditSupervision(ep, "drain_queue", "queue="+in.Queue, in.Reason)
		queues := []string{in.Queue}
		started := time.Now()
		leases, err := st.OutstandingLeasesForHuman(ctx, ep.OwnerHumanID, queues, limit)
		if err != nil {
			return nil, queueActionOut{}, h.mapStoreErr(ep, "drain_queue", err)
		}
		for wait > 0 && len(leases) > 0 {
			wait -= int(drainPollInterval / time.Second)
			select {
			case <-ctx.Done():
				return nil, queueActionOut{}, &toolError{codeInternal, "drain interrupted"}
			case <-time.After(drainPollInterval):
			}
			if wait < 0 {
				wait = 0
			}
			if leases, err = st.OutstandingLeasesForHuman(ctx, ep.OwnerHumanID, queues, limit); err != nil {
				return nil, queueActionOut{}, h.mapStoreErr(ep, "drain_queue", err)
			}
		}
		out := queueActionOut{Queue: in.Queue, Paused: true, Drained: len(leases) == 0,
			Leases: make([]leaseRow, 0, len(leases)), LeasesListed: len(leases),
			LeasesTruncated: len(leases) >= limit}
		for _, l := range leases {
			out.Leases = append(out.Leases, toLeaseRow(l))
		}
		waited := time.Since(started).Round(time.Second).Seconds()
		out.WaitedSeconds = &waited
		return nil, out, nil
	}
}

func (h *Handler) requeueTodoTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[todoActionIn, todoOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in todoActionIn) (*sdk.CallToolResult, todoOut, error) {
		st, terr := h.supervision("requeue_todo")
		if terr != nil {
			return nil, todoOut{}, terr
		}
		if in.ID == "" {
			return nil, todoOut{}, &toolError{codeInvalid, "id is required"}
		}
		item, gerr := h.guardSupervisedTodo(ctx, st, ep, "requeue_todo", in.ID)
		if gerr != nil {
			return nil, todoOut{}, gerr
		}
		// Only a failed row is requeueable, and the check happens before the write so a claimed or
		// done row is refused with a reason instead of being silently passed to a transition that
		// would report it as a bare conflict.
		if item.State != "failed" {
			return nil, todoOut{}, &toolError{codeConflict,
				"requeue moves a failed or dead-lettered todo; this todo is " + item.State}
		}
		t, err := st.RetryTodoOperatorOwned(ctx, ep.OwnerHumanID, in.ID)
		if err != nil {
			return nil, todoOut{}, h.mapStoreErr(ep, "requeue_todo", err)
		}
		h.auditSupervision(ep, "requeue_todo", "todo="+in.ID+" queue="+item.Queue, in.Reason)
		return nil, toRow(t), nil
	}
}

func (h *Handler) releaseLeaseTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[todoActionIn, queueActionOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in todoActionIn) (*sdk.CallToolResult, queueActionOut, error) {
		st, terr := h.supervision("release_lease")
		if terr != nil {
			return nil, queueActionOut{}, terr
		}
		if in.ID == "" {
			return nil, queueActionOut{}, &toolError{codeInvalid, "id is required"}
		}
		item, gerr := h.guardSupervisedTodo(ctx, st, ep, "release_lease", in.ID)
		if gerr != nil {
			return nil, queueActionOut{}, gerr
		}
		if item.State != "claimed" {
			return nil, queueActionOut{}, &toolError{codeConflict,
				"release hands back a held lease; this todo is " + item.State}
		}
		t, err := st.ReleaseTodoOperatorOwned(ctx, ep.OwnerHumanID, in.ID, item.Owner)
		if err != nil {
			return nil, queueActionOut{}, h.mapStoreErr(ep, "release_lease", err)
		}
		h.auditSupervision(ep, "release_lease", "todo="+in.ID+" queue="+item.Queue+" holder="+item.Owner, in.Reason)
		return nil, queueActionOut{Queue: t.Queue, ID: t.ID, Paused: false}, nil
	}
}

// --- verdicts ---

// stalled is the report's question answered: work is waiting, nobody is working, the wait is older
// than the threshold, and no operator paused it on purpose. Anything else is a queue that is quiet,
// busy, held back by decision, or simply young.
func stalled(q store.QueueLiveness, afterSeconds float64) bool {
	return !q.Paused && q.Pending > 0 && q.Claimed == 0 && q.OldestPendingSeconds >= afterSeconds
}

// verdict names the same reading in one word, for a caller that would rather branch on a label than
// re-derive it from the counts. The order is the reasoning: a pause explains the age before anything
// else can, and a queue with rows under lease is working whatever its backlog looks like.
func verdict(q store.QueueLiveness, afterSeconds float64) string {
	switch {
	case q.Paused:
		return "paused"
	case q.Pending == 0:
		return "quiet"
	case q.Claimed > 0:
		return "working"
	case stalled(q, afterSeconds):
		return "stalled"
	default:
		return "waiting"
	}
}

func toLeaseRow(l store.HeldLease) leaseRow {
	out := leaseRow{TodoID: l.TodoID, Queue: l.Queue, Owner: l.Owner, Attempt: l.Attempt}
	if l.LeaseExpiresAt != nil {
		stamp := l.LeaseExpiresAt.UTC().Format(time.RFC3339)
		out.LeaseExpiresAt = &stamp
		remaining := time.Until(*l.LeaseExpiresAt).Round(time.Second).Seconds()
		out.ExpiresInSeconds = &remaining
	}
	return out
}

func drainReason(reason string) string {
	if reason == "" {
		return "drain_queue"
	}
	return reason
}
