package store

// Human-Scoped Webhook Reads
//
// The human API (/api/v1, SPEC-0035) manages webhooks as the signed-in human rather than as one of
// their endpoints. These two reads are its reach boundary: every webhook whose OWNING endpoint hangs
// off one of the human's agents is in reach, and nothing else is. Until teams ship (SPEC-0033),
// reach is exactly the human's own resources, so the reach value is the human id, applied inside the
// query (never by filtering rows afterwards).
//
// Neither read returns an ingest token or a signing secret: the ingest URL is a credential for a
// token-trust webhook, and it was revealed once, at create or rotate, to the endpoint that owns it.
// WebhookOwnerForHuman answers only which endpoint owns a webhook, so the rule-management code in
// internal/manage can then drive the SAME endpoint-scoped reads and row-locked write the MCP verbs use
// (WebhookRoutingForEndpoint, UpdateWebhookRouting). The endpoint-to-endpoint rule (F19) is untouched.
//
// Governing: SPEC-0035 REQ "Reach on Every Route", REQ "Webhook Management"; SPEC-0033 REQ "Reach
// and Effective Reach"; SPEC-0007 REQ "Human as Accountable Principal".
//
// @joestump-agent 09/29/2026 - Added for the human API's webhook list and rule routes.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HumanWebhook is one webhook in a human's reach, with its owning endpoint and a summary of its
// routing configuration. It deliberately has no ingest token and no secret.
type HumanWebhook struct {
	ID            string
	EndpointID    string
	EndpointSlug  string
	EndpointState string
	AgentName     string
	SourceType    string
	TargetQueue   string
	TrustMode     string
	// RuleCount is the number of routing rules; HasDefault says a default action is set, and
	// HasParams that routing params are.
	RuleCount  int
	HasDefault bool
	HasParams  bool
	CreatedAt  time.Time
	RotatedAt  *time.Time
}

// ListWebhooksForHuman returns every webhook whose owning endpoint belongs to ownerHumanID (through
// its agent), active endpoints first and then newest first. A revoked endpoint's webhooks are listed
// too: their routes can still deliver, and their history stays inspectable (SPEC-0035).
func (s *Store) ListWebhooksForHuman(ctx context.Context, ownerHumanID string) ([]HumanWebhook, error) {
	if !isUUID(ownerHumanID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT w.id::text, w.endpoint_id::text, e.slug, e.state, a.name,
		       w.source_type, w.target_queue, w.trust_mode,
		       jsonb_array_length(w.routing_rules), w.default_action IS NOT NULL,
		       w.routing_params IS NOT NULL AND w.routing_params <> 'null'::jsonb,
		       w.created_at, w.rotated_at
		FROM endpoint_webhooks w
		JOIN endpoints e ON e.id = w.endpoint_id
		JOIN agents a ON a.id = e.agent_id
		WHERE a.owner_human_id = $1
		ORDER BY (e.state = 'active') DESC, w.created_at DESC, w.id`, ownerHumanID)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks for human: %w", err)
	}
	defer rows.Close()
	var out []HumanWebhook
	for rows.Next() {
		var w HumanWebhook
		if err := rows.Scan(&w.ID, &w.EndpointID, &w.EndpointSlug, &w.EndpointState, &w.AgentName,
			&w.SourceType, &w.TargetQueue, &w.TrustMode, &w.RuleCount, &w.HasDefault, &w.HasParams,
			&w.CreatedAt, &w.RotatedAt); err != nil {
			return nil, fmt.Errorf("store: list webhooks for human scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WebhookOwner names the endpoint that owns a webhook, and that endpoint's lifecycle state.
type WebhookOwner struct {
	EndpointID    string
	EndpointSlug  string
	EndpointState string
}

// WebhookOwnerForHuman resolves the endpoint that owns webhookID, but only when that endpoint belongs
// to ownerHumanID. Unknown, malformed, and another human's webhook ids are ALL ErrNotFound, so the
// human API cannot be used as an existence oracle for webhooks outside the caller's reach.
func (s *Store) WebhookOwnerForHuman(ctx context.Context, webhookID, ownerHumanID string) (WebhookOwner, error) {
	if !isUUID(webhookID) || !isUUID(ownerHumanID) {
		return WebhookOwner{}, ErrNotFound
	}
	var o WebhookOwner
	err := s.pool.QueryRow(ctx, `
		SELECT e.id::text, e.slug, e.state
		FROM endpoint_webhooks w
		JOIN endpoints e ON e.id = w.endpoint_id
		JOIN agents a ON a.id = e.agent_id
		WHERE w.id = $1 AND a.owner_human_id = $2`, webhookID, ownerHumanID).Scan(&o.EndpointID, &o.EndpointSlug, &o.EndpointState)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookOwner{}, ErrNotFound
	}
	if err != nil {
		return WebhookOwner{}, fmt.Errorf("store: webhook owner for human: %w", err)
	}
	return o, nil
}

// EndpointCeiling is one endpoint's webhook ceiling: the source types it may create webhooks for, the
// queues those webhooks may route to, and how many it may hold. It is the ceiling the wizard vends
// and create_webhook enforces, read for a human who is allowed to administer the endpoint on its owner's
// behalf. State is the endpoint's state so a caller can refuse a write against a revoked endpoint instead
// of minting a credential nothing delivers to.
type EndpointCeiling struct {
	ID            string
	Slug          string
	State         string
	WebhookMax    int
	SourceTypes   []string
	WebhookQueues []string
}

// EndpointCeilingForHuman resolves an endpoint by id or slug, strictly within ownerHumanID's ownership,
// and returns its webhook ceiling. Unknown, malformed and another human's refs are ALL ErrNotFound, so
// POST /api/v1/endpoints/{ref}/webhooks cannot be used as an existence oracle for endpoints outside the
// caller's reach. A revoked endpoint still resolves: deleting and rotating its webhooks stays possible,
// and the caller refuses only a create.
//
// Governing: SPEC-0035 REQ "Webhook Management", REQ "Reach on Every Route"; SPEC-0007 REQ
// "Human as Accountable Principal".
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584: the human CLI and API need the
// ceiling a create runs against without holding an endpoint credential.
func (s *Store) EndpointCeilingForHuman(ctx context.Context, ref, ownerHumanID string) (EndpointCeiling, error) {
	if ref == "" || !isUUID(ownerHumanID) {
		return EndpointCeiling{}, ErrNotFound
	}
	var c EndpointCeiling
	// A slug is never a valid uuid, so the id half of the predicate is only added for a ref that parses
	// as one; otherwise the cast would abort the query rather than miss it.
	where := "e.slug = $1"
	if isUUID(ref) {
		where = "(e.id::text = $1 OR e.slug = $1)"
	}
	err := s.pool.QueryRow(ctx, `
		SELECT e.id::text, e.slug, e.state, e.webhook_max, e.webhook_source_types, e.webhook_queues
		FROM endpoints e JOIN agents a ON a.id = e.agent_id
		WHERE `+where+` AND a.owner_human_id = $2`, ref, ownerHumanID,
	).Scan(&c.ID, &c.Slug, &c.State, &c.WebhookMax, &c.SourceTypes, &c.WebhookQueues)
	if errors.Is(err, pgx.ErrNoRows) {
		return EndpointCeiling{}, ErrNotFound
	}
	if err != nil {
		return EndpointCeiling{}, fmt.Errorf("store: endpoint ceiling for human: %w", err)
	}
	return c, nil
}
