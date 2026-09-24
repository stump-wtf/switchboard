package store

// Webhook routing configuration (ADR-0024, SPEC-0020): the ordered jq rules and default action stored
// on each self-managed webhook, read on the delivery path and read-modify-written by the MCP rule
// verbs. The store neither compiles nor validates rules — internal/routing does, inside the mutate
// callback, while the webhook row is locked — so a concurrent edit can never interleave between
// "validated against the grant" and "written".
//
// Ownership is checked at the HUMAN, exactly as the ADR-0022 route verbs do: a webhook is its owning
// human's to configure from any of that human's endpoints, and an unknown, malformed, or
// another-human's webhook id is uniformly ErrNotFound.
//
// Governing: ADR-0024, SPEC-0020 REQ "Rule Validation at Save Time", REQ "Isolation and Tenant
// Safety"; ADR-0022; ADR-0012.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/routing"
)

// WebhookRouting is a webhook's routing configuration plus the switchboard-side facts a grant is
// computed from. WebhookQueues is the webhook OWNER's allowed webhook queues — never the calling
// endpoint's — because the rules route the owner's deliveries: the owning endpoint's webhook-queue
// ceiling united with the scope and webhook queues of every active, unexpired endpoint the owner's
// other agents hold (vending an endpoint for queue Q demonstrably grants the owner Q, so the
// routing grant follows the endpoints the owner already has — issue #270). Revoking or expiring an
// endpoint shrinks the union on the next read; no vend ever mutates another endpoint's ceiling.
type WebhookRouting struct {
	WebhookID     string
	EndpointID    string
	SourceType    string
	TrustMode     string
	TargetQueue   string
	WebhookQueues []string
	Config        routing.Config
}

// webhookRoutingSelect projects a webhook's routing row and computes the owner's allowed webhook
// queues in one read, so the save-time grant (UpdateWebhookRouting's locked row) and the
// delivery-time grant (WebhookRoutingByID) are the SAME union and cannot drift. The subquery runs
// inside the caller's query — never as a separate Store read — so it is safe under the
// "mutate must not touch the pool" rule below.
const webhookRoutingSelect = `
	SELECT w.id::text, w.endpoint_id::text, w.source_type, w.trust_mode, w.target_queue,
	       COALESCE((
		       SELECT array_agg(DISTINCT q ORDER BY q)
		       FROM (
			       SELECT unnest(e.webhook_queues) AS q
			       UNION
			       SELECT unnest(e2.scope_queues || e2.webhook_queues)
			       FROM endpoints e2
			       JOIN agents a2 ON a2.id = e2.agent_id
			       WHERE a2.owner_human_id = a.owner_human_id
			         AND e2.state = 'active'
			         AND (e2.expires_at IS NULL OR e2.expires_at > now())
		       ) granted
		       WHERE q IS NOT NULL
	       ), '{}'),
	       w.routing_rules, w.default_action, w.routing_params
	FROM endpoint_webhooks w
	JOIN endpoints e ON e.id = w.endpoint_id
	JOIN agents a ON a.id = e.agent_id`

func scanWebhookRouting(row pgx.Row) (WebhookRouting, error) {
	var wr WebhookRouting
	var rules, def, params []byte
	err := row.Scan(&wr.WebhookID, &wr.EndpointID, &wr.SourceType, &wr.TrustMode, &wr.TargetQueue,
		&wr.WebhookQueues, &rules, &def, &params)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookRouting{}, ErrNotFound
	}
	if err != nil {
		return WebhookRouting{}, fmt.Errorf("store: scan webhook routing: %w", err)
	}
	if err := json.Unmarshal(rules, &wr.Config.Rules); err != nil {
		return WebhookRouting{}, fmt.Errorf("store: decode routing rules: %w", err)
	}
	if len(def) > 0 && string(def) != "null" {
		var a routing.Action
		if err := json.Unmarshal(def, &a); err != nil {
			return WebhookRouting{}, fmt.Errorf("store: decode default action: %w", err)
		}
		wr.Config.Default = &a
	}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &wr.Config.Params); err != nil {
			return WebhookRouting{}, fmt.Errorf("store: decode routing params: %w", err)
		}
	}
	return wr, nil
}

// WebhookRoutingByID reads a webhook's routing for the delivery path, where the ingest token has
// already resolved the webhook and no caller identity exists.
func (s *Store) WebhookRoutingByID(ctx context.Context, webhookID string) (WebhookRouting, error) {
	if !isUUID(webhookID) {
		return WebhookRouting{}, ErrNotFound
	}
	return scanWebhookRouting(s.pool.QueryRow(ctx, webhookRoutingSelect+`
	WHERE w.id = $1`, webhookID))
}

// WebhookRoutingForHuman reads a webhook's routing when ownerHumanID owns it, else ErrNotFound.
func (s *Store) WebhookRoutingForHuman(ctx context.Context, webhookID, ownerHumanID string) (WebhookRouting, error) {
	if !isUUID(webhookID) || !isUUID(ownerHumanID) {
		return WebhookRouting{}, ErrNotFound
	}
	return scanWebhookRouting(s.pool.QueryRow(ctx, webhookRoutingSelect+`
	WHERE w.id = $1 AND a.owner_human_id = $2`, webhookID, ownerHumanID))
}

// UpdateWebhookRouting replaces a webhook's routing configuration with whatever mutate returns,
// holding a row lock on the webhook from read to write. mutate receives the current state and
// validates the new configuration; any error it returns aborts the update and is returned unchanged,
// leaving the previous configuration in force (SPEC-0020 scenario "Typo in a rule is rejected, not
// silently inert").
//
// mutate runs while this transaction holds a pooled connection, so it MUST NOT touch the pool (any
// Store read): on a pool of N connections, N concurrent updates would each hold one and block forever
// acquiring another. Read what validation needs before calling.
func (s *Store) UpdateWebhookRouting(ctx context.Context, webhookID, ownerHumanID string,
	mutate func(WebhookRouting) (routing.Config, error)) (WebhookRouting, error) {
	if !isUUID(webhookID) || !isUUID(ownerHumanID) {
		return WebhookRouting{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebhookRouting{}, fmt.Errorf("store: update webhook routing begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	wr, err := scanWebhookRouting(tx.QueryRow(ctx, webhookRoutingSelect+`
	WHERE w.id = $1 AND a.owner_human_id = $2
	FOR UPDATE OF w`, webhookID, ownerHumanID))
	if err != nil {
		return WebhookRouting{}, err
	}
	cfg, err := mutate(wr)
	if err != nil {
		return WebhookRouting{}, err
	}
	if cfg.Rules == nil {
		cfg.Rules = []routing.Rule{}
	}
	rules, err := json.Marshal(cfg.Rules)
	if err != nil {
		return WebhookRouting{}, fmt.Errorf("store: encode routing rules: %w", err)
	}
	var def []byte // nil persists NULL: "the webhook's target queue"
	if cfg.Default != nil {
		if def, err = json.Marshal(cfg.Default); err != nil {
			return WebhookRouting{}, fmt.Errorf("store: encode default action: %w", err)
		}
	}
	var params []byte // nil persists NULL: no params
	if len(cfg.Params) > 0 {
		if params, err = json.Marshal(cfg.Params); err != nil {
			return WebhookRouting{}, fmt.Errorf("store: encode routing params: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE endpoint_webhooks SET routing_rules = $2, default_action = $3, routing_params = $4 WHERE id = $1`,
		webhookID, rules, def, params); err != nil {
		return WebhookRouting{}, fmt.Errorf("store: update webhook routing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return WebhookRouting{}, fmt.Errorf("store: update webhook routing commit: %w", err)
	}
	wr.Config = cfg
	return wr, nil
}

// EndpointScopeQueues returns the scope queues of the given endpoints, keyed by id. It feeds the
// routing grant's EndpointQueues, which exclusive delivery selects on (ADR-0025). Callers pass ids
// that ResolveWebhookTargets already authorized, so this is a projection, not an authorization
// read; malformed ids are skipped and unknown ids are simply absent.
func (s *Store) EndpointScopeQueues(ctx context.Context, endpointIDs []string) (map[string][]string, error) {
	ids := make([]string, 0, len(endpointIDs))
	for _, id := range endpointIDs {
		if isUUID(id) {
			ids = append(ids, id)
		}
	}
	out := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, scope_queues FROM endpoints WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: endpoint scope queues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var queues []string
		if err := rows.Scan(&id, &queues); err != nil {
			return nil, fmt.Errorf("store: endpoint scope queues scan: %w", err)
		}
		out[id] = queues
	}
	return out, rows.Err()
}

// EventForWebhook returns one stored event, but only if it arrived on webhookID. The caller has
// already established that it owns webhookID; this is what keeps a dry-run by event id from reading
// any other webhook's deliveries.
func (s *Store) EventForWebhook(ctx context.Context, eventID int64, webhookID string) (EventHistoryDetail, error) {
	if eventID <= 0 || !isUUID(webhookID) {
		return EventHistoryDetail{}, ErrNotFound
	}
	return scanEventDetail(s.pool.QueryRow(ctx, eventDetailSelect+` WHERE id = $1 AND webhook_id = $2`, eventID, webhookID))
}

// RecentWebhookEvents returns up to limit of webhookID's most recent stored deliveries, newest first,
// with the full record a dry-run needs (headers, payload). The caller has already established
// ownership of webhookID. It serves the save-time dry-run (SPEC-0026 REQ-3), which must run BEFORE
// UpdateWebhookRouting takes its row lock: a pooled read inside that lock deadlocks the pool under
// concurrent rule edits (see mcp/webhook_rules.go). idx_events_webhook covers the scan.
func (s *Store) RecentWebhookEvents(ctx context.Context, webhookID string, limit int) ([]EventHistoryDetail, error) {
	if !isUUID(webhookID) || limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, eventDetailSelect+`
		WHERE webhook_id = $1 ORDER BY received_at DESC, id DESC LIMIT $2`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent webhook events: %w", err)
	}
	defer rows.Close()
	var out []EventHistoryDetail
	for rows.Next() {
		e, err := scanEventDetail(rows)
		if err != nil {
			return nil, fmt.Errorf("store: recent webhook events scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
