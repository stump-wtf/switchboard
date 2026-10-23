package store

// Add-only endpoint scope widening (issue #590, ADR-0049 proposed).
//
// An endpoint's scope is fixed at vend (ADR-0008), and it stays fixed for anything that could SHRINK
// it: narrowing a grant or replacing a verb list can break a route that works today, so that remains a
// revoke and a re-vend. What has no path today is the change that cannot break anything — adding a
// queue to an endpoint's allowed webhook queues, an ingest source type, a verb, or a larger webhook
// ceiling. Without it, giving one more lane a route costs a whole re-vend: a new endpoint, a new
// webhook (a webhook belongs to the endpoint that created it), a new ingest URL and signing secret,
// and an edit in the source system's hook configuration.
//
// Widening is therefore additive and it is audited. The new value is computed as a UNION, so removal
// is not a case this code can express; the one field where an absolute value could smuggle a reduction
// (webhook_max) is refused before anything is written. Every field that actually moves writes one
// endpoint_scope_widenings row in the SAME transaction as the UPDATE, carrying the old and the new
// value, so the record cannot drift from the change it describes.
//
// Ownership is bound into the SELECT that locks the row, under the same predicate as every other
// human-facing endpoint write: another human's endpoint — or one that does not exist — is ErrNotFound,
// and one the caller owns but has revoked is ErrConflict (SPEC-0035 REQ "Reach on Every Route"). A
// widen touches no credential: the secret shown once at vend stays valid, and nothing here reads or
// rewrites it.
//
// Governing: ADR-0049 (proposed), ADR-0008, ADR-0012, ADR-0022 (tenant isolation),
// SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling".
//
// @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrScopeNarrowing is returned when a widen asks for less than the endpoint already has. It is the
// widen's only refusal of its own kind: everything else that is not additive cannot be expressed.
var ErrScopeNarrowing = errors.New("store: widening may only add to an endpoint's scope")

// ErrNothingToWiden is returned when a widen names no addition at all: it would change nothing, and an
// audit trail of no-ops is noise.
var ErrNothingToWiden = errors.New("store: a widen must name at least one addition")

// EndpointScope is the part of an endpoint's grant a widen can reach, plus the queues it drains, which
// a webhook grant is computed from but a widen never touches.
type EndpointScope struct {
	// ScopeQueues are the queues the endpoint drains. Read-only here: the union that authorizes a
	// routing action already counts them, and changing what an endpoint may CLAIM is not an additive
	// change to a route — it changes which work it can take.
	ScopeQueues []string
	// ScopeVerbs is the endpoint's MCP verb grant. Additive only.
	ScopeVerbs []string
	// WebhookMax is the ceiling on this endpoint's self-managed webhooks. A widen may raise it.
	WebhookMax int
	// WebhookSourceTypes are the ingest source types its agents may create webhooks for.
	WebhookSourceTypes []string
	// WebhookQueues is the webhook-queue ceiling: the queues a webhook this endpoint owns may route
	// to. This is the field the report in #590 needs to move.
	WebhookQueues []string
}

// ScopeAdditions names what a widen adds. Every field is additive: an absent or empty field adds
// nothing, and there is no field that removes, replaces or narrows.
type ScopeAdditions struct {
	WebhookQueues      []string
	WebhookSourceTypes []string
	WebhookVerbs       []string
	// WebhookMax is the ceiling to raise to; nil leaves it alone. A value below the current ceiling
	// is ErrScopeNarrowing, so the field cannot lower it.
	WebhookMax *int
}

// Empty reports whether the additions name nothing at all, which the caller refuses before reaching
// the database.
func (a ScopeAdditions) Empty() bool {
	return len(a.WebhookQueues) == 0 && len(a.WebhookSourceTypes) == 0 &&
		len(a.WebhookVerbs) == 0 && a.WebhookMax == nil
}

// ScopeWidening is one audit record: a field of one endpoint's scope, as it was and as it became.
// Before and After are the stored JSONB values decoded — []string for an array field, an int64 for
// webhook_max.
type ScopeWidening struct {
	Field     string
	Before    any
	After     any
	CreatedAt time.Time
}

// ScopeWidenResult is what a widen returns: the scope as it was, as it is now, and the audit entries it
// wrote (empty when the additions added nothing the endpoint did not already have).
type ScopeWidenResult struct {
	Before  EndpointScope
	After   EndpointScope
	Widened []ScopeWidening
}

// WidenScope computes the scope an endpoint has after a widen, without touching anything. The preview
// an operator confirms against and the write path both come through here, so what the confirmation
// showed is what the change does. The result is a superset of before by construction: arrays are
// unioned, and a webhook_max below the current ceiling is ErrScopeNarrowing.
func WidenScope(before EndpointScope, add ScopeAdditions) (EndpointScope, error) {
	add = normalizeAdditions(add)
	if add.Empty() {
		return EndpointScope{}, ErrNothingToWiden
	}
	if err := CheckQueueNames(add.WebhookQueues...); err != nil {
		return EndpointScope{}, err
	}
	after := before
	after.WebhookQueues = addedStrings(before.WebhookQueues, add.WebhookQueues)
	after.WebhookSourceTypes = addedStrings(before.WebhookSourceTypes, add.WebhookSourceTypes)
	after.ScopeVerbs = addedStrings(before.ScopeVerbs, add.WebhookVerbs)
	if add.WebhookMax != nil {
		if *add.WebhookMax < before.WebhookMax {
			return EndpointScope{}, ErrScopeNarrowing
		}
		after.WebhookMax = *add.WebhookMax
	}
	return after, nil
}

// ScopeWidenings describes the move from one scope to another as audit entries, oldest field first. It
// is empty when the two grant the same thing, which is why re-adding a queue an endpoint already has
// writes no history.
func ScopeWidenings(before, after EndpointScope) []ScopeWidening {
	entries := []ScopeWidening{}
	for _, f := range []struct {
		name                    string
		beforeField, afterField []string
	}{
		{"webhook_queues", before.WebhookQueues, after.WebhookQueues},
		{"webhook_source_types", before.WebhookSourceTypes, after.WebhookSourceTypes},
		{"scope_verbs", before.ScopeVerbs, after.ScopeVerbs},
	} {
		if sameStringSet(f.beforeField, f.afterField) {
			continue
		}
		entries = append(entries, ScopeWidening{
			Field:  f.name,
			Before: nonNilStrings(f.beforeField),
			After:  nonNilStrings(f.afterField),
		})
	}
	if before.WebhookMax != after.WebhookMax {
		entries = append(entries, ScopeWidening{Field: "webhook_max", Before: before.WebhookMax, After: after.WebhookMax})
	}
	return entries
}

// EndpointScopeForHuman reads the widen-reachable part of an endpoint's grant. An endpoint the human
// does not own, or that does not exist, is ErrNotFound.
func (s *Store) EndpointScopeForHuman(ctx context.Context, endpointID, ownerHumanID string) (EndpointScope, error) {
	var sc EndpointScope
	if !isUUID(endpointID) {
		return sc, ErrNotFound
	}
	err := s.pool.QueryRow(ctx, `
		SELECT e.scope_queues, e.scope_verbs, e.webhook_max, e.webhook_source_types, e.webhook_queues
		FROM endpoints e JOIN agents a ON a.id = e.agent_id
		WHERE e.id = $1 AND a.owner_human_id = $2`, endpointID, ownerHumanID,
	).Scan(&sc.ScopeQueues, &sc.ScopeVerbs, &sc.WebhookMax, &sc.WebhookSourceTypes, &sc.WebhookQueues)
	if errors.Is(err, pgx.ErrNoRows) {
		return EndpointScope{}, ErrNotFound
	}
	if err != nil {
		return EndpointScope{}, fmt.Errorf("store: endpoint scope: %w", err)
	}
	return sc, nil
}

// WidenEndpointScopeForHuman additively widens the scope of an endpoint the human owns and records each
// field that moved. A queue named in the additions that is reserved is refused as everywhere else a
// queue name is taken (ErrReservedQueue); an endpoint the human does not own, or that is gone, is
// ErrNotFound; one they own that is revoked is ErrConflict; a webhook_max below the current ceiling is
// ErrScopeNarrowing. Nothing is written when the transaction fails.
func (s *Store) WidenEndpointScopeForHuman(ctx context.Context, endpointID, actorHumanID string, add ScopeAdditions) (ScopeWidenResult, error) {
	if !isUUID(endpointID) {
		return ScopeWidenResult{}, ErrNotFound
	}
	if add.Empty() {
		return ScopeWidenResult{}, ErrNothingToWiden
	}
	if err := CheckQueueNames(add.WebhookQueues...); err != nil {
		return ScopeWidenResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ScopeWidenResult{}, fmt.Errorf("store: widen scope begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before EndpointScope
	err = tx.QueryRow(ctx, `
		SELECT e.scope_queues, e.scope_verbs, e.webhook_max, e.webhook_source_types, e.webhook_queues
		FROM endpoints e JOIN agents a ON a.id = e.agent_id
		WHERE e.id = $1 AND a.owner_human_id = $2 AND e.state = 'active'
		FOR UPDATE OF e`, endpointID, actorHumanID,
	).Scan(&before.ScopeQueues, &before.ScopeVerbs, &before.WebhookMax,
		&before.WebhookSourceTypes, &before.WebhookQueues)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ScopeWidenResult{}, s.ownedButNotActive(ctx, tx, endpointID, actorHumanID)
		}
		return ScopeWidenResult{}, fmt.Errorf("store: widen scope read: %w", err)
	}

	after, err := WidenScope(before, add)
	if err != nil {
		return ScopeWidenResult{}, err
	}

	// One entry per field that actually moved, so re-adding a queue an endpoint already grants is
	// idempotent and stays out of the trail.
	changes := ScopeWidenings(before, after)
	if len(changes) == 0 {
		return ScopeWidenResult{Before: before, After: after}, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE endpoints
		SET webhook_queues = $2, webhook_source_types = $3, webhook_max = $4, scope_verbs = $5
		WHERE id = $1`, endpointID,
		after.WebhookQueues, after.WebhookSourceTypes, after.WebhookMax, after.ScopeVerbs); err != nil {
		return ScopeWidenResult{}, fmt.Errorf("store: widen scope update: %w", err)
	}

	result := ScopeWidenResult{Before: before, After: after}
	for _, c := range changes {
		beforeJSON, err := json.Marshal(c.Before)
		if err != nil {
			return ScopeWidenResult{}, fmt.Errorf("store: widen scope audit: %w", err)
		}
		afterJSON, err := json.Marshal(c.After)
		if err != nil {
			return ScopeWidenResult{}, fmt.Errorf("store: widen scope audit: %w", err)
		}
		var entry ScopeWidening
		var rawBefore, rawAfter string
		if err := tx.QueryRow(ctx, `
			INSERT INTO endpoint_scope_widenings (endpoint_id, actor_human_id, field, before, after)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING field, before::text, after::text, created_at`,
			endpointID, actorHumanID, c.Field, beforeJSON, afterJSON,
		).Scan(&entry.Field, &rawBefore, &rawAfter, &entry.CreatedAt); err != nil {
			return ScopeWidenResult{}, fmt.Errorf("store: widen scope audit insert: %w", err)
		}
		if entry.Before, err = decodeScopeValue(rawBefore); err != nil {
			return ScopeWidenResult{}, err
		}
		if entry.After, err = decodeScopeValue(rawAfter); err != nil {
			return ScopeWidenResult{}, err
		}
		result.Widened = append(result.Widened, entry)
	}

	if err := tx.Commit(ctx); err != nil {
		return ScopeWidenResult{}, fmt.Errorf("store: widen scope commit: %w", err)
	}
	return result, nil
}

// ListEndpointScopeWidenings returns an endpoint's widenings, oldest first — the trail as it happened.
// An endpoint the human does not own (or that does not exist) is ErrNotFound, the same answer as
// everywhere else an endpoint is out of reach.
func (s *Store) ListEndpointScopeWidenings(ctx context.Context, endpointID, ownerHumanID string) ([]ScopeWidening, error) {
	if !isUUID(endpointID) {
		return nil, ErrNotFound
	}
	// The history of an endpoint you cannot reach is not an empty history: an empty list means "this
	// endpoint has never been widened", and that is only true of one you own.
	if _, err := s.EndpointScopeForHuman(ctx, endpointID, ownerHumanID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT w.field, w.before::text, w.after::text, w.created_at
		FROM endpoint_scope_widenings w
		JOIN endpoints e ON e.id = w.endpoint_id
		JOIN agents a ON a.id = e.agent_id
		WHERE w.endpoint_id = $1 AND a.owner_human_id = $2
		ORDER BY w.id`, endpointID, ownerHumanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScopeWidening{}
	for rows.Next() {
		var w ScopeWidening
		var before, after string
		if err := rows.Scan(&w.Field, &before, &after, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan scope widening: %w", err)
		}
		if w.Before, err = decodeScopeValue(before); err != nil {
			return nil, err
		}
		if w.After, err = decodeScopeValue(after); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// decodeScopeValue renders one stored JSONB value for a caller: the string slice or the number it
// holds. A value of an unexpected shape is an error rather than a silent nil, so a history view can
// never quietly show "nothing was there".
func decodeScopeValue(raw string) (any, error) {
	body := []byte(raw)
	var list []string
	if err := json.Unmarshal(body, &list); err == nil {
		return list, nil
	}
	var n float64
	if err := json.Unmarshal(body, &n); err == nil {
		return int64(n), nil
	}
	return nil, fmt.Errorf("store: scope audit value is neither a list nor a number: %s", raw)
}

// ownedButNotActive answers a widen or a history read that found no row under the ownership predicate:
// an endpoint the caller owns but has revoked is ErrConflict, and anything else — including another
// human's endpoint — is ErrNotFound, so probing an endpoint you do not own learns nothing.
func (s *Store) ownedButNotActive(ctx context.Context, q querier, endpointID, ownerHumanID string) error {
	var state string
	err := q.QueryRow(ctx, `
		SELECT e.state FROM endpoints e JOIN agents a ON a.id = e.agent_id
		WHERE e.id = $1 AND a.owner_human_id = $2`, endpointID, ownerHumanID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: widen scope ownership probe: %w", err)
	}
	return ErrConflict
}

// normalizeAdditions drops the blanks a shell or a script leaves behind, so ["  "] is "no addition"
// rather than a change to a queue with no name.
func normalizeAdditions(a ScopeAdditions) ScopeAdditions {
	trim := func(in []string) []string {
		if in == nil {
			return nil
		}
		out := make([]string, 0, len(in))
		for _, s := range in {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	a.WebhookQueues = trim(a.WebhookQueues)
	a.WebhookSourceTypes = trim(a.WebhookSourceTypes)
	a.WebhookVerbs = trim(a.WebhookVerbs)
	return a
}

// addedStrings is the widen's whole mechanism: the existing value with each added name appended once,// in the order the existing list already had them. Existing entries are never dropped, reordered or
// replaced, which is what makes the result a superset by construction.
func addedStrings(existing, added []string) []string {
	out := make([]string, 0, len(existing)+len(added))
	for _, s := range existing {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range added {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// sameStringSet compares two grants as sets: a widen that only re-adds what the endpoint already
// grants changed nothing and writes no audit row.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, s := range a {
		if !slices.Contains(b, s) {
			return false
		}
	}
	return true
}
