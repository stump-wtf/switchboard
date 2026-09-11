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

	"github.com/joestump/switchboard/internal/routing"
)

// WebhookRouting is a webhook's routing configuration plus the switchboard-side facts a grant is
// computed from. WebhookQueues is the OWNING endpoint's webhook-queue ceiling — never the calling
// endpoint's — because the rules route the owner's deliveries.
type WebhookRouting struct {
	WebhookID     string
	EndpointID    string
	SourceType    string
	TrustMode     string
	TargetQueue   string
	WebhookQueues []string
	Config        routing.Config
}

const webhookRoutingSelect = `
	SELECT w.id::text, w.endpoint_id::text, w.source_type, w.trust_mode, w.target_queue,
	       e.webhook_queues, w.routing_rules, w.default_action
	FROM endpoint_webhooks w
	JOIN endpoints e ON e.id = w.endpoint_id`

func scanWebhookRouting(row pgx.Row) (WebhookRouting, error) {
	var wr WebhookRouting
	var rules, def []byte
	err := row.Scan(&wr.WebhookID, &wr.EndpointID, &wr.SourceType, &wr.TrustMode, &wr.TargetQueue,
		&wr.WebhookQueues, &rules, &def)
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
	return wr, nil
}

// WebhookRoutingByID reads a webhook's routing for the delivery path, where the ingest token has
// already resolved the webhook and no caller identity exists.
func (s *Store) WebhookRoutingByID(ctx context.Context, webhookID string) (WebhookRouting, error) {
	if !isUUID(webhookID) {
		return WebhookRouting{}, ErrNotFound
	}
	return scanWebhookRouting(s.pool.QueryRow(ctx, webhookRoutingSelect+` WHERE w.id = $1`, webhookID))
}

// WebhookRoutingForHuman reads a webhook's routing when ownerHumanID owns it, else ErrNotFound.
func (s *Store) WebhookRoutingForHuman(ctx context.Context, webhookID, ownerHumanID string) (WebhookRouting, error) {
	if !isUUID(webhookID) || !isUUID(ownerHumanID) {
		return WebhookRouting{}, ErrNotFound
	}
	return scanWebhookRouting(s.pool.QueryRow(ctx, webhookRoutingSelect+`
	JOIN agents a ON a.id = e.agent_id
	WHERE w.id = $1 AND a.owner_human_id = $2`, webhookID, ownerHumanID))
}

// UpdateWebhookRouting replaces a webhook's routing configuration with whatever mutate returns,
// holding a row lock on the webhook from read to write. mutate receives the current state and
// validates the new configuration; any error it returns aborts the update and is returned unchanged,
// leaving the previous configuration in force (SPEC-0020 scenario "Typo in a rule is rejected, not
// silently inert").
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
	JOIN agents a ON a.id = e.agent_id
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
	if _, err := tx.Exec(ctx,
		`UPDATE endpoint_webhooks SET routing_rules = $2, default_action = $3 WHERE id = $1`,
		webhookID, rules, def); err != nil {
		return WebhookRouting{}, fmt.Errorf("store: update webhook routing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return WebhookRouting{}, fmt.Errorf("store: update webhook routing commit: %w", err)
	}
	wr.Config = cfg
	return wr, nil
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
