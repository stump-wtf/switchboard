package store

// Quarantine (ADR-0031, SPEC-0026 REQ-6, REQ-7, REQ-13). A held delivery is one todo on the
// receiving webhook's OWNER endpoint, on the reserved queue "quarantine", with a reason and detail.
// It keeps the delivery's event, trace and idempotency key, so a redelivery collapses onto it
// (CreateIntakeEventTodos).
//
// The default filter: every agent-facing read and every lifecycle transition in this package
// carries `queue <> 'quarantine'` (todos.go, todos_a2a.go), and the wakeup and doorbell paths refuse
// quarantine outright (notifyTodoReady, fireDoorbell). No caller has to remember it.
// quarantine_filter_test.go holds every exported todo method to that, with an explicit allowlist for
// the owner's own read surfaces (the Board, the todo list and detail views, the queue gauges).
//
// A held todo leaves quarantine only three ways, and each is recorded on the row with who, when and
// the outcome:
//   - release (ApplyQuarantineRelease): routed again through the owner's rules, by the ingest
//     service, which computes the plan OUTSIDE any transaction. The row is updated in place, so it
//     keeps its id, and extra fan-out targets get new rows. released_by and released_at record it.
//   - discard (DiscardQuarantined): done, with result {"discarded": true, "reason", "by", "at"}.
//   - expiry (ExpireQuarantine): done after 30 days, or sooner under the operator's retention
//     bound, with result {"expired": true, "by": "system", "at"}.
//
// Release and discard lock the row, so a concurrent pair resolves once and the loser gets
// ErrConflict. Expiry is one statement, safe under concurrent instances.
//
// @joestump-agent 09/25/2026 - Added for #386.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/routing"
)

// QueueQuarantine is the reserved quarantine queue (routing.QueueQuarantine).
const QueueQuarantine = routing.QueueQuarantine

// QuarantineMaxAge is how long a held todo waits before it expires, unless the operator's
// retention bound (retention_max_age_days) is shorter. Governing: SPEC-0026 REQ-6.
const QuarantineMaxAge = 30 * 24 * time.Hour

// ErrReservedQueue is returned when a caller names the reserved quarantine queue where a working
// queue belongs: a webhook target, an endpoint scope or a webhook-queue ceiling.
var ErrReservedQueue = errors.New(`store: the queue name "quarantine" is reserved`)

// CheckQueueNames refuses the reserved quarantine queue among queues. Every place a queue name
// enters the system calls it, or routing.Validate for rule actions. Governing: SPEC-0026 REQ-6.
func CheckQueueNames(queues ...string) error {
	if slices.Contains(queues, QueueQuarantine) {
		return ErrReservedQueue
	}
	return nil
}

// ErrHeldEventGone is returned for a held item whose event no longer exists, so it cannot be routed
// again. Retention keeps a held item's event (Prune), so only rows older than that guard reach it.
// A caller refuses the release and says to discard the item instead.
var ErrHeldEventGone = errors.New("store: the held delivery's event is gone")

// QuarantinedItem is one held delivery: its todo, and the event it holds.
type QuarantinedItem struct {
	Todo  Todo
	Event EventHistoryDetail
}

// QuarantinedForHuman returns one open quarantine item (queue quarantine, state pending) that the
// human owns, with its event. Another human's item, an unknown id, and an item that is no longer
// held all return ErrNotFound. Governing: SPEC-0026 REQ-9 Tenancy ("unknown ids and foreign ids
// MUST both answer not_found").
func (s *Store) QuarantinedForHuman(ctx context.Context, ownerHumanID, id string) (QuarantinedItem, error) {
	t, err := scanTodo(s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
		WHERE id = $1 AND queue = 'quarantine' AND state = 'pending'`+operatorOwns+`$2)`, id, ownerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return QuarantinedItem{}, ErrNotFound
	}
	if err != nil {
		return QuarantinedItem{}, fmt.Errorf("store: quarantined item: %w", err)
	}
	if t.EventID == nil {
		return QuarantinedItem{Todo: t}, fmt.Errorf("%w: quarantined item %s", ErrHeldEventGone, t.ID)
	}
	ev, err := scanEventDetail(s.pool.QueryRow(ctx, eventDetailSelect+` WHERE id = $1`, *t.EventID))
	if errors.Is(err, ErrNotFound) {
		return QuarantinedItem{Todo: t}, fmt.Errorf("%w: quarantined item %s", ErrHeldEventGone, t.ID)
	}
	if err != nil {
		return QuarantinedItem{}, fmt.Errorf("store: quarantined item event: %w", err)
	}
	return QuarantinedItem{Todo: t, Event: ev}, nil
}

// ReleasePlan is where a release sends a held todo, decided by routing outside any transaction.
type ReleasePlan struct {
	TodoID       string
	OwnerHumanID string
	By           string   // human:<id> | classifier:<slug>
	Queue        string   // the routed queue; never quarantine
	Endpoints    []string // the routed targets in delivery order; the held row moves to the first
	Trace        []byte   // the release's routing trace
	WorkOrder    []byte   // the work order for every row, when the decision asked for one
	OnceKey      string   // the at-most-once key the decision claims, if any
	WebhookID    string   // the webhook the delivery arrived on (for the once claim)
	// Verified and TrustMode are the held delivery's, for the SPEC-0011 sender gate on the doorbells
	// that ring after commit.
	Verified  bool
	TrustMode string
}

// ApplyQuarantineRelease moves a held todo to where plan says, in one transaction: the held row is
// locked (FOR UPDATE), updated in place to the plan's first target and queue (so it keeps its id),
// and every further target gets a new row with the same event and idempotency key. Doorbells, the
// wakeup and the transition hooks fire after commit, exactly as for a freshly routed delivery.
//
// ErrNotFound: no such item in the human's scope. ErrConflict: the item is no longer held (a
// concurrent release or discard won), or the plan's once key was already claimed by another
// delivery. Governing: SPEC-0026 REQ-7 "Release and Discard", REQ-13.
func (s *Store) ApplyQuarantineRelease(ctx context.Context, plan ReleasePlan) ([]CreatedTodo, error) {
	if len(plan.Endpoints) == 0 || plan.Queue == "" || plan.Queue == QueueQuarantine {
		return nil, fmt.Errorf("store: release: a plan needs a working queue and at least one target")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	held, err := lockOwnedTodo(ctx, tx, plan.TodoID, plan.OwnerHumanID)
	if err != nil {
		return nil, err
	}
	if held.Queue != QueueQuarantine || held.State != "pending" {
		return nil, ErrConflict
	}
	if plan.OnceKey != "" && held.EventID != nil {
		var claimedBy int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO routing_once (webhook_id, once_key, event_id) VALUES ($1, $2, $3)
			ON CONFLICT (webhook_id, once_key) DO UPDATE SET once_key = EXCLUDED.once_key
			RETURNING event_id`, plan.WebhookID, plan.OnceKey, *held.EventID).Scan(&claimedBy); err != nil {
			return nil, fmt.Errorf("store: release: claim once key: %w", err)
		}
		if claimedBy != *held.EventID {
			return nil, ErrConflict
		}
	}
	moved, err := scanTodo(tx.QueryRow(ctx, `
		UPDATE todos SET queue = $2, endpoint_id = $3, routing_trace = $4, work_order = $5,
			released_by = $6, released_at = now(), updated_at = now()
		WHERE id = $1
		RETURNING `+todoCols, held.ID, plan.Queue, plan.Endpoints[0], plan.Trace, plan.WorkOrder, plan.By))
	if isUniqueViolation(err) {
		// The first target already holds a live todo for this delivery (idx_todos_dedupe): a
		// routed copy from before the owner tightened trust. Nothing is moved; the item stays
		// held for the human to discard.
		return nil, fmt.Errorf("%w: a live todo for this delivery already exists on the release target", ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("store: release: move held todo: %w", err)
	}
	out := []CreatedTodo{{Todo: moved, New: true}}
	for _, ep := range plan.Endpoints[1:] {
		t, isNew, err := createTodo(ctx, tx, CreateTodoParams{
			EndpointID: ep, Queue: plan.Queue, Source: held.Source, Kind: held.Kind, Title: held.Title,
			Payload: held.Payload, EventID: held.EventID, IdempotencyKey: held.IdempotencyKey,
			RoutingTrace: plan.Trace, WorkOrder: plan.WorkOrder,
		})
		if err != nil {
			return nil, fmt.Errorf("store: release: fan out: %w", err)
		}
		out = append(out, CreatedTodo{Todo: t, New: isNew})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: release: commit: %w", err)
	}
	for i, ct := range out {
		if !ct.New {
			continue
		}
		verb := "created"
		if i == 0 {
			verb = "pending" // the held row re-surfaced, as a re-queue does
		}
		s.fireTodoHook(verb, ct.Todo)
		s.metricsOrNop().TodoCreated(ct.Todo.Queue, todoMetricSource(CreateTodoParams{Source: ct.Todo.Source}))
		s.notifyTodoReady(ctx, ct.Todo.EndpointID, ct.Todo.Queue)
		// Every after-commit "work is ready" signal a freshly routed todo gets, under the same
		// sender gate: when the SPEC-0024 ready hook (fireReady, #358) lands, it fires here too,
		// and TestReadyHookFollowsTheDoorbellAndRefusesQuarantine fails until it does.
		if plan.Verified || plan.TrustMode == "token" {
			s.fireDoorbell(ct.Todo)
		}
	}
	return out, nil
}

// DiscardQuarantined completes a held todo with {"discarded": true, "reason", "by", "at"}. The
// UPDATE takes the row lock, so it resolves once against a concurrent release: whichever commits
// first wins, and the other sees a row that is no longer held and gets ErrConflict. An item outside
// the human's scope is ErrNotFound. Governing: SPEC-0026 REQ-7, REQ-13.
func (s *Store) DiscardQuarantined(ctx context.Context, ownerHumanID, id, by, reason string) (Todo, error) {
	t, err := scanTodo(s.pool.QueryRow(ctx, `
		UPDATE todos SET state = 'done', completed_at = now(), updated_at = now(),
			result = jsonb_build_object('discarded', true, 'reason', $3::text, 'by', $4::text, 'at', now())
		WHERE id = $1 AND queue = 'quarantine' AND state = 'pending'`+operatorOwns+`$2)
		RETURNING `+todoCols, id, ownerHumanID, reason, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	if err != nil {
		return Todo{}, fmt.Errorf("store: discard quarantined: %w", err)
	}
	s.fireTodoHook("done", t)
	return t, nil
}

// ExpireQuarantine completes every held todo older than QuarantineMaxAge, or than the operator's
// retention bound when that is shorter, with {"expired": true, "by": "system", "at"}. It is one
// statement: row locks make it safe with a concurrent release, discard or second instance, each of
// which sees the row no longer held. Returns the number expired.
// Governing: SPEC-0026 REQ-6 (expiry), REQ-13 (single statement, safe under concurrency).
func (s *Store) ExpireQuarantine(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE todos SET state = 'done', completed_at = now(), updated_at = now(),
			result = jsonb_build_object('expired', true, 'by', 'system', 'at', now())
		WHERE queue = 'quarantine' AND state = 'pending'
		  AND created_at < now() - LEAST(
		      make_interval(secs => $1::float8),
		      make_interval(days => COALESCE((SELECT value FROM settings WHERE key = 'retention_max_age_days'), '30')::int))
		RETURNING `+todoCols, QuarantineMaxAge.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: expire quarantine: %w", err)
	}
	defer rows.Close()
	var expired []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return 0, fmt.Errorf("store: expire quarantine scan: %w", err)
		}
		expired = append(expired, t)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: expire quarantine: %w", err)
	}
	for _, t := range expired {
		s.fireTodoHook("done", t)
	}
	return int64(len(expired)), nil
}

// QuarantineCounts returns the number of open quarantine items per webhook, for the webhooks of one
// endpoint (list_webhooks' quarantined count, SPEC-0026 REQ-5).
func (s *Store) QuarantineCounts(ctx context.Context, endpointID string) (map[string]int, error) {
	if endpointScope(endpointID) != nil {
		return map[string]int{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.webhook_id::text, count(*)
		  FROM todos t JOIN events e ON e.id = t.event_id
		 WHERE t.endpoint_id = $1 AND t.queue = 'quarantine' AND t.state = 'pending' AND e.webhook_id IS NOT NULL
		 GROUP BY e.webhook_id`, endpointID)
	if err != nil {
		return nil, fmt.Errorf("store: quarantine counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: quarantine counts scan: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// lockOwnedTodo locks one todo the human owns (FOR UPDATE), or returns ErrNotFound.
func lockOwnedTodo(ctx context.Context, tx pgx.Tx, id, ownerHumanID string) (Todo, error) {
	t, err := scanTodo(tx.QueryRow(ctx, `SELECT `+todoCols+` FROM todos WHERE id = $1`+operatorOwns+`$2)
		FOR UPDATE`, id, ownerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	if err != nil {
		return Todo{}, fmt.Errorf("store: lock todo: %w", err)
	}
	return t, nil
}
