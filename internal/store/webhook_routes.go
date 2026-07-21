package store

// Webhook route fan-out: maps a webhook to N target endpoints for deterministic, token-free
// delivery routing (ADR-0021). At ingest time the self-managed receiver resolves a webhook's
// target endpoints via this table and creates one todo per target, each pinned to that target.
// When no rows exist for a webhook the target set is the singleton {webhook.endpoint_id}.
// Routes are populated by human-approved actions (friending, a future routing verb) — never by a
// per-delivery agent decision.
//
// Governing: ADR-0021, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".

import (
	"context"
	"fmt"
	"time"
)

// WebhookRoute is one target endpoint a webhook's deliveries fan out to.
type WebhookRoute struct {
	WebhookID        string
	TargetEndpointID string
	GrantedByHumanID string
	GrantedAt        time.Time
}

// AddWebhookRoute records that a webhook's deliveries should fan out to targetEndpointID. Idempotent
// on (webhook_id, target_endpoint_id) — re-adding an existing route is a no-op. The route MUST be
// granted by a human who owns the webhook (the caller enforces ownership); the webhook's owning
// endpoint is implicitly always a target, so a route targeting the owner endpoint is redundant but
// harmless. Governing: ADR-0021.
func (s *Store) AddWebhookRoute(ctx context.Context, webhookID, targetEndpointID, grantedByHumanID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_routes (webhook_id, target_endpoint_id, granted_by_human_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (webhook_id, target_endpoint_id) DO NOTHING`,
		webhookID, targetEndpointID, grantedByHumanID)
	if err != nil {
		return fmt.Errorf("store: add webhook route: %w", err)
	}
	return nil
}

// RemoveWebhookRoute removes a route. Removing the implicit owner route is a no-op — the owner
// endpoint is always a target regardless of this table (the receiver falls back to {owner} when
// resolving targets, and explicitly includes the owner in the resolved set).
func (s *Store) RemoveWebhookRoute(ctx context.Context, webhookID, targetEndpointID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_routes WHERE webhook_id = $1 AND target_endpoint_id = $2`,
		webhookID, targetEndpointID)
	if err != nil {
		return fmt.Errorf("store: remove webhook route: %w", err)
	}
	return nil
}

// ListWebhookRoutes returns the explicit routes recorded for a webhook, newest first. The implicit
// owner-endpoint target is NOT included here; ResolveWebhookTargets adds it.
func (s *Store) ListWebhookRoutes(ctx context.Context, webhookID string) ([]WebhookRoute, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT webhook_id::text, target_endpoint_id::text, granted_by_human_id::text, granted_at
		FROM webhook_routes WHERE webhook_id = $1 ORDER BY granted_at DESC`, webhookID)
	if err != nil {
		return nil, fmt.Errorf("store: list webhook routes: %w", err)
	}
	defer rows.Close()
	var out []WebhookRoute
	for rows.Next() {
		var r WebhookRoute
		if err := rows.Scan(&r.WebhookID, &r.TargetEndpointID, &r.GrantedByHumanID, &r.GrantedAt); err != nil {
			return nil, fmt.Errorf("store: list webhook routes scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveWebhookTargets returns the distinct set of endpoint ids a webhook's deliveries fan out to:
// the union of {ownerEndpointID} and every explicit webhook_routes target, de-duplicated. This is
// the delivery-path read the self-managed receiver calls once per delivery to decide how many
// todos to create. An empty slice (owner not resolvable) means no work is produced — the caller
// SHOULD treat that as a misconfiguration and 503 rather than silently dropping the delivery.
// Governing: ADR-0021, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func (s *Store) ResolveWebhookTargets(ctx context.Context, webhookID, ownerEndpointID string) ([]string, error) {
	// The owner endpoint is always a target. Explicit routes may add more. De-dup preserving the
	// owner first so the first todo is always the owner's (stable ordering aids testing).
	seen := map[string]struct{}{ownerEndpointID: {}}
	out := []string{ownerEndpointID}
	rows, err := s.pool.Query(ctx,
		`SELECT target_endpoint_id::text FROM webhook_routes WHERE webhook_id = $1`, webhookID)
	if err != nil {
		return nil, fmt.Errorf("store: resolve webhook targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: resolve webhook targets scan: %w", err)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, rows.Err()
}
