package store

// Issue-Dependency Holds
//
// A tracker issue can say, in the tracker's own words, that it waits on other issues (Gitea's
// "blocked by" links). Switchboard honours that instead of inventing a second dependency model: when
// the delivery that would mint a todo names an issue with open blockers, the todo is recorded in the
// `blocked` state and its blocking refs are stored in todo_blockers. A blocked row is queue state, not
// a queue: it is visible in listings and counts, it is never handed out by either claim path (both
// require `pending`, so nothing here needs a new claim predicate), and it rings nothing — no
// LISTEN/NOTIFY, no doorbell, no ready hook (SPEC-0011's sender gate is amended the same way
// SPEC-0026 REQ-6 amends it for quarantine).
//
// Release is keyed off the tracker, never off the queue: the signal that the waiting is over is "the
// blocking issue is closed", not "the blocking todo completed", because an implement-stage todo
// completes when the PR opens, days before anything merges. So a close event for a blocker resolves
// every dependent that references it, and the periodic re-check (internal/deps) re-resolves holds
// whose links were edited silently — a dependency link add or remove fires no webhook at all.
//
// Refs are stored per todo rather than as one shared graph edge table: links cross repositories, the
// same blocker can hold several dependents in different queues, and each hold is resolved on its own
// schedule. The blocker-keyed index is the release lookup.
//
// Governing: stump.wtf/switchboard#583, ADR-0055 (proposed: what a provider read may be used for);
// ADR-0002 (the database is the record).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StateBlocked is the held lifecycle state: recorded, counted and visible, but not claimable and
// silent. It is in the todos_state_check domain from migration 0036.
const StateBlocked = "blocked"

// BlockerRef is one tracker issue that keeps a todo waiting. Owner/Repo/Number name the blocker,
// which is not necessarily in the dependent's repository; State is the blocker's own tracker state
// ("open" or "closed") as of CheckedAt.
type BlockerRef struct {
	Owner     string    `json:"owner"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// RefString is the canonical "owner/repo#number" form, shared with IssueRefOf so a hold key and a
// blocker row are written the same way.
func (b BlockerRef) RefString() string { return IssueRefOf(b.Owner, b.Repo, b.Number) }

// IssueRefOf canonicalizes a tracker issue's identity as stored in todos.issue_ref. It is a display
// and lookup key only; the release path matches on the owner/repo/number triple, never on this string.
func IssueRefOf(owner, repo string, number int) string {
	return owner + "/" + repo + "#" + strconv.Itoa(number)
}

// blockerRows is the bounded-insert half of a hold: the refs are written in one statement so a hold
// and its reasons commit together (a hold with no rows would be released by the next sweep as an
// orphan). Duplicate refs in one call collapse on the primary key.
func blockerRows(ctx context.Context, q querier, todoID string, refs []BlockerRef) error {
	if len(refs) == 0 {
		return nil
	}
	for _, b := range refs {
		if _, err := q.Exec(ctx, `
			INSERT INTO todo_blockers (todo_id, owner, repo, number, state, html_url, checked_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), now())
			ON CONFLICT (todo_id, owner, repo, number)
				DO UPDATE SET state = EXCLUDED.state, html_url = EXCLUDED.html_url, checked_at = now()`,
			todoID, b.Owner, b.Repo, b.Number, blockerState(b.State), b.HTMLURL); err != nil {
			return fmt.Errorf("store: record blockers: %w", err)
		}
	}
	return nil
}

// blockerState normalizes a tracker state to the two values the column allows: only an explicit
// "closed" closes a blocker, so an unrecognized state keeps the dependent held rather than releasing
// it on a guess.
func blockerState(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "closed") {
		return "closed"
	}
	return "open"
}

// insertHeldTodo mints (or folds into) the single held todo for one issue. It is createTodo's blocked
// twin: same columns, state='blocked', and an ON CONFLICT clause that mirrors uq_todos_held_per_issue
// so a second, third and tenth event for an issue that is already waiting refreshes the item that is
// waiting — payload, title, event link and coalesced_count — instead of queueing one todo per event.
// Without that fold, release would produce a burst of stale todos for the same issue.
func insertHeldTodo(ctx context.Context, q querier, p CreateTodoParams) (Todo, bool, error) {
	if p.EndpointID == "" {
		return Todo{}, false, fmt.Errorf("store: insertHeldTodo requires a non-empty EndpointID")
	}
	if p.IssueRef == "" {
		return Todo{}, false, fmt.Errorf("store: insertHeldTodo requires an IssueRef to hold on")
	}
	id := "td_" + uuid.NewString()
	// A redelivery of a delivery already minted is deduped exactly as the unheld path dedups it — by
	// (endpoint, idempotency_key) — before the hold fold is considered. Without this the insert below
	// would collide with idx_todos_dedupe, whose conflict its ON CONFLICT clause does not name, and a
	// provider retry would surface as a unique violation instead of "already have this".
	if p.IdempotencyKey != "" {
		live, err := scanTodo(q.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
			WHERE endpoint_id = $1 AND idempotency_key = $2 AND state <> 'done'
				AND (state <> 'failed' OR next_retry_at IS NOT NULL)
			ORDER BY created_at LIMIT 1`, p.EndpointID, p.IdempotencyKey))
		if err == nil {
			return live, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Todo{}, false, err
		}
	}
	var created bool
	// A fold returns the row it updated, so an insert is told from a fold by (xmax = 0) rather than by
	// an absent row — ON CONFLICT DO UPDATE always hands one back. created=false is what keeps the
	// caller from publishing a card or ringing anything for a delivery that minted no new work.
	t, err := scanTodo(q.QueryRow(ctx, `
		INSERT INTO todos (id, endpoint_id, queue, source, kind, title, payload, event_id, idempotency_key,
			assignee, routing_trace, work_order, state, issue_ref)
		VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7, $8, NULLIF($9,''),
			NULLIF($10,''), $11, $12, 'blocked', $13)
		ON CONFLICT (endpoint_id, queue, issue_ref) WHERE state = 'blocked' AND issue_ref IS NOT NULL
			DO UPDATE SET payload = EXCLUDED.payload, title = EXCLUDED.title,
				event_id = EXCLUDED.event_id, updated_at = now(),
				coalesced_count = todos.coalesced_count + 1
		RETURNING `+todoCols+`, (xmax = 0)`,
		id, p.EndpointID, p.Queue, p.Source, p.Kind, p.Title, p.Payload, p.EventID, p.IdempotencyKey,
		p.Assignee, p.RoutingTrace, p.WorkOrder, p.IssueRef), &created)
	if err != nil {
		return Todo{}, false, err
	}
	if err := blockerRows(ctx, q, t.ID, p.Blockers); err != nil {
		return Todo{}, false, err
	}
	return t, created, nil
}

// BlockersFor returns the blocking refs recorded for one todo, oldest link first. Empty for a todo
// that is not, and never was, held. A quarantine row's refs never surface: the hold keys it never
// legitimately has.
func (s *Store) BlockersFor(ctx context.Context, todoID string) ([]BlockerRef, error) {
	out, err := queryBlockers(ctx, s.pool, `
		SELECT owner, repo, number, state, COALESCE(html_url,''), checked_at FROM todo_blockers b
		WHERE b.todo_id = $1
			AND NOT EXISTS (SELECT 1 FROM todos t WHERE t.id = b.todo_id AND t.queue = 'quarantine')
		ORDER BY b.owner, b.repo, b.number`, todoID)
	return out, err
}

// BlockersForTodos returns the refs of many todos in one round trip, keyed by todo id, so a listing
// can show why each held item waits without a query per row. Quarantine rows never appear: the
// caller's id may name one, and its refs are not this listing's to report.
func (s *Store) BlockersForTodos(ctx context.Context, todoIDs []string) (map[string][]BlockerRef, error) {
	out := make(map[string][]BlockerRef, len(todoIDs))
	if len(todoIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT b.todo_id, b.owner, b.repo, b.number, b.state, COALESCE(b.html_url,''), b.checked_at
		FROM todo_blockers b
		WHERE b.todo_id = ANY($1::text[])
			AND NOT EXISTS (SELECT 1 FROM todos t WHERE t.id = b.todo_id AND t.queue = 'quarantine')
		ORDER BY b.todo_id, b.owner, b.repo, b.number`, todoIDs)
	if err != nil {
		return nil, fmt.Errorf("store: blockers for todos: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var b BlockerRef
		if err := rows.Scan(&id, &b.Owner, &b.Repo, &b.Number, &b.State, &b.HTMLURL, &b.CheckedAt); err != nil {
			return nil, fmt.Errorf("store: blockers for todos scan: %w", err)
		}
		out[id] = append(out[id], b)
	}
	return out, rows.Err()
}

func queryBlockers(ctx context.Context, q querier, sql string, args ...any) ([]BlockerRef, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: blockers: %w", err)
	}
	defer rows.Close()
	var out []BlockerRef
	for rows.Next() {
		var b BlockerRef
		if err := rows.Scan(&b.Owner, &b.Repo, &b.Number, &b.State, &b.HTMLURL, &b.CheckedAt); err != nil {
			return nil, fmt.Errorf("store: blockers scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// HeldTodoForIssue returns the todo, if any, currently holding one issue on one endpoint+queue pair —
// the row a new delivery for that issue folds into.
func (s *Store) HeldTodoForIssue(ctx context.Context, endpointID, queue, issueRef string) (Todo, bool, error) {
	t, err := scanTodo(s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
		WHERE endpoint_id = $1 AND queue = $2 AND issue_ref = $3 AND state = 'blocked'
			AND queue <> 'quarantine'
		ORDER BY created_at LIMIT 1`, endpointID, queue, issueRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, nil
	}
	return t, err == nil, err
}

// HoldTodo puts a live todo back into `blocked` with the given refs: the re-hold path. It is how a
// reopened blocker re-holds a dependent that was released but not yet claimed, and how the re-check
// acts on a link added silently in the tracker. A claimed or terminal row is left alone: the hold is
// a gate on delivery, never a way to interrupt work in flight. A quarantine row is never held:
// intake already refused its delivery, and re-holding it would rewrite a row only release, discard
// and expiry may touch.
func (s *Store) HoldTodo(ctx context.Context, todoID, issueRef string, refs []BlockerRef) (Todo, error) {
	if len(refs) == 0 {
		return Todo{}, fmt.Errorf("store: hold %s: a hold needs at least one blocking ref", todoID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Todo{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanTodo(tx.QueryRow(ctx, `
		UPDATE todos SET state = 'blocked', issue_ref = $2, updated_at = now()
		WHERE id = $1 AND state = 'pending' AND queue <> 'quarantine'
		RETURNING `+todoCols, todoID, issueRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrConflict
	}
	if err != nil {
		return Todo{}, fmt.Errorf("store: hold todo %s: %w", todoID, err)
	}
	if err := blockerRows(ctx, tx, t.ID, refs); err != nil {
		return Todo{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Todo{}, err
	}
	s.fireTodoHook("blocked", t)
	return t, nil
}

// ReleaseHeldTodo moves one held todo back to `pending` and wakes for it: the release is the moment
// the work becomes claimable, so it notifies, rings the doorbell and fires the ready hook exactly
// once — the held row was silent on the way in, and an event that arrived while it was held folded
// into this same row rather than queueing a second one.
func (s *Store) ReleaseHeldTodo(ctx context.Context, todoID string) (Todo, error) {
	t, err := scanTodo(s.pool.QueryRow(ctx, `
		UPDATE todos SET state = 'pending', updated_at = now()
		WHERE id = $1 AND state = 'blocked' AND queue <> 'quarantine'
		RETURNING `+todoCols, todoID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	if err != nil {
		return Todo{}, fmt.Errorf("store: release held todo %s: %w", todoID, err)
	}
	s.notifyTodoReady(ctx, t.EndpointID, t.Queue)
	s.fireTodoHook("released", t)
	s.fireDoorbell(t)
	s.fireReady(t, ReadyCreated)
	return t, nil
}

// MarkBlockerState records what the tracker just told us about one issue — a close event, or a
// re-check that read its state — and releases every held todo whose last open blocker it was.
//
// It returns the todos it released. Closing a blocker delivers an event for the blocker only: nothing
// is sent for the dependents, so this is the one place that resolves them, and it does so from the
// refs it already holds rather than a fresh read of the tracker.
func (s *Store) MarkBlockerState(ctx context.Context, owner, repo string, number int, state string) ([]Todo, error) {
	state = blockerState(state)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE todo_blockers SET state = $4, checked_at = now()
		WHERE owner = $1 AND repo = $2 AND number = $3`, owner, repo, number, state); err != nil {
		return nil, fmt.Errorf("store: mark blocker %s/%s#%d: %w", owner, repo, number, err)
	}
	if state != "closed" {
		// A reopened (or still-open) blocker keeps whatever its dependents already are: a dependent
		// mid-flight is not interrupted, and the re-check re-holds the pending ones.
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		UPDATE todos SET state = 'pending', updated_at = now()
		WHERE state = 'blocked' AND queue <> 'quarantine' AND NOT EXISTS (
			SELECT 1 FROM todo_blockers b WHERE b.todo_id = todos.id AND b.state <> 'closed')
		RETURNING `+todoCols)
	if err != nil {
		return nil, fmt.Errorf("store: release dependents of %s/%s#%d: %w", owner, repo, number, err)
	}
	var released []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		released = append(released, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, t := range released {
		s.notifyTodoReady(ctx, t.EndpointID, t.Queue)
		s.fireTodoHook("released", t)
		s.fireDoorbell(t)
		s.fireReady(t, ReadyCreated)
	}
	return released, nil
}

// HeldTodosDependingOn lists the todos currently held because of one issue, across repositories.
func (s *Store) HeldTodosDependingOn(ctx context.Context, owner, repo string, number int) ([]Todo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+todoCols+` FROM todos
		WHERE state = 'blocked' AND queue <> 'quarantine' AND EXISTS (
			SELECT 1 FROM todo_blockers b WHERE b.todo_id = todos.id AND b.owner = $1 AND b.repo = $2 AND b.number = $3)
		ORDER BY created_at`, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("store: dependents of %s/%s#%d: %w", owner, repo, number, err)
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TodoWithBlockers pairs a live todo with the refs recorded for it, for the re-check pass. WebhookID is
// the webhook whose delivery minted it ("" when the todo did not come from a webhook): that is whose
// stored connection the pass dials to re-ask the tracker.
type TodoWithBlockers struct {
	Todo      Todo
	WebhookID string
	Blockers  []BlockerRef
}

// HoldRefresh records that a hold's refs were re-resolved just now, without touching the todo's state.
// A pass that learned "still blocked by these", or "nothing blocks it any more", has to say when it
// learned it, or the next pass would read the same row as stale again and dial the tracker every tick.
func (s *Store) HoldRefresh(ctx context.Context, todoID string, refs []BlockerRef) error {
	if err := blockerRows(ctx, s.pool, todoID, refs); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todo_blockers SET checked_at = now() WHERE todo_id = $1`, todoID); err != nil {
		return fmt.Errorf("store: refresh hold %s: %w", todoID, err)
	}
	return nil
}

// TodosWithHolds returns live todos that are waiting on, or were once held by, tracker links, each
// with the refs recorded for it — the re-check candidate set. It includes released-but-not-yet-claimed
// `pending` rows whose refs are still recorded, because that is exactly the case a silent link edit or
// a reopened blocker catches: the row is claimable now, and the tracker says it should not be.
//
// Candidates are the ones whose refs have gone stale (nothing checked inside olderThan) or that are
// pending with every recorded ref closed (a link removed by hand — an orphan, which the caller
// releases rather than holding forever). Capped by limit and ordered so a busy instance re-checks the
// longest-waiting holds first and the pass itself stays bounded.
func (s *Store) TodosWithHolds(ctx context.Context, olderThan time.Duration, limit int) ([]TodoWithBlockers, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+todoCols+` FROM todos t
		WHERE t.state IN ('blocked', 'pending') AND t.issue_ref IS NOT NULL AND t.queue <> 'quarantine'
			AND EXISTS (SELECT 1 FROM todo_blockers b WHERE b.todo_id = t.id)
			AND (NOT EXISTS (SELECT 1 FROM todo_blockers b WHERE b.todo_id = t.id
					AND b.checked_at >= now() - $1::interval)
				OR (t.state = 'pending' AND NOT EXISTS (SELECT 1 FROM todo_blockers b
						WHERE b.todo_id = t.id AND b.state <> 'closed')))
		ORDER BY t.created_at
		LIMIT $2`, olderThan.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: todos with holds: %w", err)
	}
	var todos []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		todos = append(todos, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(todos))
	for _, t := range todos {
		ids = append(ids, t.ID)
	}
	byID, err := s.BlockersForTodos(ctx, ids)
	if err != nil {
		return nil, err
	}
	// The webhook whose delivery minted each todo, reached through the event it links to: that row is
	// the one holding the provider connection this pass dials. A todo with no event has no webhook, and
	// the caller skips it — there is nothing to ask and no connection to ask it of.
	hook := make(map[string]string, len(ids))
	hookRows, err := s.pool.Query(ctx, `
		SELECT t.id, COALESCE(e.webhook_id::text,'') FROM todos t
		LEFT JOIN events e ON e.id = t.event_id WHERE t.id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: todos with holds: webhook lookup: %w", err)
	}
	for hookRows.Next() {
		var id, webhookID string
		if err := hookRows.Scan(&id, &webhookID); err != nil {
			hookRows.Close()
			return nil, fmt.Errorf("store: todos with holds: webhook scan: %w", err)
		}
		hook[id] = webhookID
	}
	hookRows.Close()
	if err := hookRows.Err(); err != nil {
		return nil, err
	}
	out := make([]TodoWithBlockers, 0, len(todos))
	for _, t := range todos {
		out = append(out, TodoWithBlockers{Todo: t, WebhookID: hook[t.ID], Blockers: byID[t.ID]})
	}
	return out, nil
}
