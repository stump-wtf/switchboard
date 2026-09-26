package store

// The owner's Quarantine view reads (ADR-0031, SPEC-0026 REQ-9): the open quarantine items in the
// signed-in human's scope, the rail badge count, the per-webhook signals the endpoint card shows (open
// quarantine count and the 24-hour fault count), and the "trust this actor" write.
//
// Every read is scoped by the human who owns the receiving endpoint's agent (operatorOwns), so a
// second human reads nothing. Teams (ADR-0038 / SPEC-0033) will widen the scope to the teams in
// which the human may configure webhooks; until then the human's own endpoints are the scope.
//
// Governing: ADR-0031, SPEC-0026 REQ-9 "Quarantine View and Owner Signals", REQ-5 "Trusted Actors",
// "Tenancy" (unknown and foreign ids both answer not_found).
//
// @joestump-agent 09/25/2026 - Added for #387.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/routing"
)

// QuarantineListCap bounds one Quarantine view listing. The view is a working list, not an archive:
// items expire after 30 days, and a flood is fixed at the source (a trust list or a rule).
const QuarantineListCap = 200

// QuarantinePayloadPreview bounds the payload bytes one listed item carries. A listing holds up to
// QuarantineListCap items and an inbound body may be 5 MiB, so the list reads a preview of each body,
// never the whole of it. The full body stays readable through get_webhook_event.
const QuarantinePayloadPreview = 16 << 10

// quarantineListEventSelect is eventDetailSelect with the payload cut to the preview in SQL, so the
// database never ships a whole body for a listing.
var quarantineListEventSelect = mustReplaceOnce(eventDetailSelect, "COALESCE(payload, ''::bytea)",
	fmt.Sprintf("substring(COALESCE(payload, ''::bytea) from 1 for %d)", QuarantinePayloadPreview))

// quarantineListTodoCols is todoCols with an oversized todo payload left out. The view shows the
// event's (previewed) body, and falls back to the todo's payload only when the event has none.
var quarantineListTodoCols = mustReplaceOnce(todoCols, "title, payload,",
	fmt.Sprintf("title, CASE WHEN octet_length(payload::text) <= %d THEN payload END,", QuarantinePayloadPreview))

// mustReplaceOnce derives a list projection from a shared one, and fails at start-up (and in every
// test) if the shared projection changes shape under it.
func mustReplaceOnce(s, old, repl string) string {
	if strings.Count(s, old) != 1 {
		panic("store: the shared projection changed; update the quarantine list projection for " + old)
	}
	return strings.Replace(s, old, repl, 1)
}

// ErrTrustListRefused is returned by AddWebhookTrustedActors when the grown list would be invalid
// (over the 256-entry limit, or an entry over 128 bytes). The wrapped message is safe to show.
var ErrTrustListRefused = errors.New("store: the trust list refused the actor")

// ListQuarantinedForHuman returns the open quarantine items (queue quarantine, state pending) on the
// human's endpoints, newest first, each with the event it holds (its payload cut to
// QuarantinePayloadPreview bytes) and its webhook's trust configuration. limit is clamped to
// [1, QuarantineListCap]. Governing: SPEC-0026 REQ-9 (the owner scope's items only).
func (s *Store) ListQuarantinedForHuman(ctx context.Context, ownerHumanID string, limit int) ([]QuarantinedItem, error) {
	if limit <= 0 || limit > QuarantineListCap {
		limit = QuarantineListCap
	}
	rows, err := s.pool.Query(ctx, `SELECT `+quarantineListTodoCols+` FROM todos
		WHERE queue = 'quarantine' AND state = 'pending'`+operatorOwns+`$1)
		ORDER BY created_at DESC, id DESC LIMIT $2`, ownerHumanID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list quarantined: %w", err)
	}
	var todos []Todo
	var eventIDs []int64
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list quarantined scan: %w", err)
		}
		todos = append(todos, t)
		if t.EventID != nil {
			eventIDs = append(eventIDs, *t.EventID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list quarantined: %w", err)
	}
	events := map[int64]EventHistoryDetail{}
	if len(eventIDs) > 0 {
		erows, err := s.pool.Query(ctx, quarantineListEventSelect+` WHERE id = ANY($1)`, eventIDs)
		if err != nil {
			return nil, fmt.Errorf("store: list quarantined events: %w", err)
		}
		for erows.Next() {
			ev, err := scanEventDetail(erows)
			if err != nil {
				erows.Close()
				return nil, fmt.Errorf("store: list quarantined events: %w", err)
			}
			events[ev.ID] = ev
		}
		erows.Close()
		if err := erows.Err(); err != nil {
			return nil, fmt.Errorf("store: list quarantined events: %w", err)
		}
	}
	trust, err := s.webhookTrustByID(ctx, events)
	if err != nil {
		return nil, err
	}
	out := make([]QuarantinedItem, 0, len(todos))
	for _, t := range todos {
		it := QuarantinedItem{Todo: t}
		if t.EventID != nil {
			it.Event = events[*t.EventID]
		}
		it.PayloadTruncated = len(it.Event.Payload) >= QuarantinePayloadPreview && it.Event.PayloadSize > len(it.Event.Payload)
		// Only the webhook the held todo's endpoint owns counts, as WebhookForEndpoint reads it for
		// the trust action itself.
		if w, ok := trust[it.Event.WebhookID]; ok && w.endpointID == t.EndpointID {
			it.Trust = &w.WebhookTrust
		}
		out = append(out, it)
	}
	return out, nil
}

type listedWebhookTrust struct {
	WebhookTrust
	endpointID string
}

// webhookTrustByID reads the trust configuration of the webhooks the listed events arrived on.
func (s *Store) webhookTrustByID(ctx context.Context, events map[int64]EventHistoryDetail) (map[string]listedWebhookTrust, error) {
	var ids []string
	for _, ev := range events {
		if isUUID(ev.WebhookID) && !slices.Contains(ids, ev.WebhookID) {
			ids = append(ids, ev.WebhookID)
		}
	}
	out := map[string]listedWebhookTrust{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, endpoint_id::text, source_type, trusted_actors
		FROM endpoint_webhooks WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list quarantined webhooks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var w listedWebhookTrust
		if err := rows.Scan(&id, &w.endpointID, &w.SourceType, &w.TrustedActors); err != nil {
			return nil, fmt.Errorf("store: list quarantined webhooks: %w", err)
		}
		out[id] = w
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list quarantined webhooks: %w", err)
	}
	return out, nil
}

// CountQuarantinedForHuman returns how many open quarantine items the human's endpoints hold: the
// Quarantine rail badge.
func (s *Store) CountQuarantinedForHuman(ctx context.Context, ownerHumanID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM todos
		WHERE queue = 'quarantine' AND state = 'pending'`+operatorOwns+`$1)`, ownerHumanID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count quarantined: %w", err)
	}
	return n, nil
}

// WebhookSignal is one webhook's intake signals for its owner: the open quarantine count, the
// faulted deliveries over the last 24 hours, and whether it trusts every verified sender.
type WebhookSignal struct {
	WebhookID   string
	EndpointID  string
	SourceType  string
	TargetQueue string
	AllowAll    bool
	Quarantined int
	Faults24h   int
}

// WebhookSignalsForHuman returns the signals of every webhook on the human's endpoints, newest
// webhook first. The fault count reads the partial idx_events_faulted index (migration 0022).
// Governing: SPEC-0026 REQ-9 ("the webhook card MUST show the webhook's open quarantine count and
// its fault count over the last 24 hours"), REQ-1 (the board warning), REQ-5 (the allow_all warning).
func (s *Store) WebhookSignalsForHuman(ctx context.Context, ownerHumanID string) ([]WebhookSignal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id::text, w.endpoint_id::text, w.source_type, w.target_queue,
		       COALESCE(w.trusted_actors->'allow_all' = 'true'::jsonb, false),
		       (SELECT count(*) FROM todos t JOIN events e ON e.id = t.event_id
		         WHERE t.endpoint_id = w.endpoint_id AND t.queue = 'quarantine' AND t.state = 'pending'
		           AND e.webhook_id = w.id),
		       (SELECT count(*) FROM events e
		         WHERE e.webhook_id = w.id AND e.disposition = 'faulted'
		           AND e.received_at > now() - interval '24 hours')
		  FROM endpoint_webhooks w
		  JOIN endpoints ep ON ep.id = w.endpoint_id
		  JOIN agents ag ON ag.id = ep.agent_id
		 WHERE ag.owner_human_id = $1
		 ORDER BY w.created_at DESC, w.id`, ownerHumanID)
	if err != nil {
		return nil, fmt.Errorf("store: webhook signals: %w", err)
	}
	defer rows.Close()
	var out []WebhookSignal
	for rows.Next() {
		var w WebhookSignal
		if err := rows.Scan(&w.WebhookID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.AllowAll,
			&w.Quarantined, &w.Faults24h); err != nil {
			return nil, fmt.Errorf("store: webhook signals scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AddWebhookTrustedActors adds names to the trust list of a webhook the given endpoint owns: logins
// (compared case-insensitively) on github and gitea, actor ids (exact) on cairn. Names already on
// the list are skipped, and a list that trusts every sender (allow_all) is left as it is. The row is
// locked for the read-modify-write, so two concurrent additions both land. It reports whether the
// list changed.
//
// ErrNotFound: another endpoint's, an unknown, or a malformed id. ErrTrustListRefused: the grown
// list is invalid (over the limit). Governing: SPEC-0026 REQ-9 "trust this actor", REQ-5.
func (s *Store) AddWebhookTrustedActors(ctx context.Context, webhookID, endpointID string, names []string) (Webhook, bool, error) {
	if !isUUID(webhookID) || !isUUID(endpointID) {
		return Webhook{}, false, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Webhook{}, false, fmt.Errorf("store: add trusted actors: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var w Webhook
	err = tx.QueryRow(ctx, `
		SELECT id::text, endpoint_id::text, source_type, target_queue, trust_mode, ingest_token, created_at, rotated_at, trusted_actors
		  FROM endpoint_webhooks WHERE id = $1 AND endpoint_id = $2 FOR UPDATE`, webhookID, endpointID,
	).Scan(&w.ID, &w.EndpointID, &w.SourceType, &w.TargetQueue, &w.TrustMode, &w.IngestToken, &w.CreatedAt, &w.RotatedAt, &w.TrustedActors)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, false, ErrNotFound
	}
	if err != nil {
		return Webhook{}, false, fmt.Errorf("store: add trusted actors: %w", err)
	}
	if !routing.HasActorProjection(w.SourceType) {
		return Webhook{}, false, fmt.Errorf("%w: %v", ErrTrustListRefused, routing.ErrNoActorProjection)
	}
	// An unreadable stored list decodes to the empty list (fail closed), so growing it can only
	// trust the names added here.
	cur, _ := routing.DecodeTrustedActors(w.SourceType, w.TrustedActors)
	if cur.AllowAll {
		return w, false, nil
	}
	cairn := w.SourceType == routing.SourceCairn
	list := slices.Clone(cur.Logins)
	if cairn {
		list = slices.Clone(cur.ActorIDs)
	}
	changed := false
	for _, n := range names {
		if n == "" {
			continue
		}
		has := slices.ContainsFunc(list, func(l string) bool {
			if cairn {
				return l == n
			}
			return strings.EqualFold(l, n)
		})
		if !has {
			list = append(list, n)
			changed = true
		}
	}
	if !changed {
		return w, false, nil
	}
	in := routing.TrustedActorsInput{Logins: &list, Match: &cur.Match}
	if cairn {
		in = routing.TrustedActorsInput{ActorIDs: &list}
	} else if cur.Match == "" {
		in.Match = nil
	}
	next, err := routing.ParseTrustedActors(w.SourceType, in)
	if err != nil {
		return Webhook{}, false, fmt.Errorf("%w: %v", ErrTrustListRefused, err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return Webhook{}, false, fmt.Errorf("store: add trusted actors: %w", err)
	}
	if err := tx.QueryRow(ctx, `UPDATE endpoint_webhooks SET trusted_actors = $2::jsonb WHERE id = $1
		RETURNING trusted_actors`, w.ID, raw).Scan(&w.TrustedActors); err != nil {
		return Webhook{}, false, fmt.Errorf("store: add trusted actors: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Webhook{}, false, fmt.Errorf("store: add trusted actors: commit: %w", err)
	}
	return w, true, nil
}
