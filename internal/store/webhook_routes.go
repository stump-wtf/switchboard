package store

// Webhook route fan-out: maps a webhook to N target endpoints for deterministic, token-free
// delivery routing (ADR-0022). At ingest time the self-managed receiver resolves a webhook's
// target endpoints via this table and creates one todo per target, each pinned to that target.
// When no rows exist for a webhook the target set is the singleton {webhook.endpoint_id}.
// Routes are populated by human-approved actions (friending, a future routing verb) — never by a
// per-delivery agent decision.
//
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
// harmless. Governing: ADR-0022.
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
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func (s *Store) ResolveWebhookTargets(ctx context.Context, webhookID, ownerEndpointID string) ([]string, error) {
	// The owner endpoint is always a target. Explicit routes may add more. De-dup preserving the
	// owner first so the first todo is always the owner's (stable ordering aids testing).
	//
	// An UNRESOLVABLE owner (empty id) is skipped rather than seeded: seeding it would return a
	// one-element slice holding "", which reads as a target to every caller and only fails much
	// later, inside createTodo, as a 500 on a delivery the contract says must be a 503. The empty
	// return this function documents has to actually be reachable for the caller's misconfiguration
	// branch to mean anything.
	seen := map[string]struct{}{}
	var out []string
	if ownerEndpointID != "" {
		seen[ownerEndpointID] = struct{}{}
		out = append(out, ownerEndpointID)
	}
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

// --- routing authorization reads (backing the MCP routing verbs) ---
//
// AddWebhookRoute's contract is that its CALLER enforces ownership. The three reads below are what
// that caller (internal/mcp) enforces it WITH: who owns the webhook, who owns a candidate target
// endpoint, and whether an approved friend edge bridges two humans in the direction that matters.
// They are deliberately narrow reads returning only ids/booleans — a routing check must never become
// a cross-tenant enumeration surface. Governing: ADR-0022, ADR-0010 (human-vended friending),
// SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".

// WebhookOwnerEndpointForHuman resolves the endpoint that owns webhookID, but only when that
// endpoint's agent belongs to ownerHumanID. Unknown, malformed, and owned-by-another-human ids are
// ALL ErrNotFound and therefore indistinguishable — the same non-leaking shape RotateWebhookSecret /
// DeleteWebhook use, so a routing verb cannot be turned into an existence oracle for another human's
// webhooks. The returned endpoint id is the webhook's implicit always-target (ResolveWebhookTargets
// seeds it), which the caller surfaces so an agent can see the full fan-out set.
func (s *Store) WebhookOwnerEndpointForHuman(ctx context.Context, webhookID, ownerHumanID string) (string, error) {
	if !isUUID(webhookID) || !isUUID(ownerHumanID) {
		return "", ErrNotFound
	}
	var endpointID string
	err := s.pool.QueryRow(ctx, `
		SELECT w.endpoint_id::text
		FROM endpoint_webhooks w
		JOIN endpoints e ON e.id = w.endpoint_id
		JOIN agents   a ON a.id = e.agent_id
		WHERE w.id = $1 AND a.owner_human_id = $2`, webhookID, ownerHumanID).Scan(&endpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: webhook owner endpoint: %w", err)
	}
	return endpointID, nil
}

// EndpointOwnerHuman returns the human who owns an ACTIVE endpoint. A revoked endpoint is treated as
// absent (ErrNotFound): routing deliveries into a killed endpoint would mint todos nobody can drain,
// and revoking a friend's vended endpoint is exactly how a friendship is torn down (ADR-0010), so it
// must not survive as a routable target. Malformed and unknown ids are also ErrNotFound; the caller
// collapses every target-resolution failure into ONE code, so this never distinguishes "no such
// endpoint" from "someone else's endpoint".
func (s *Store) EndpointOwnerHuman(ctx context.Context, endpointID string) (string, error) {
	if !isUUID(endpointID) {
		return "", ErrNotFound
	}
	var humanID string
	err := s.pool.QueryRow(ctx, `
		SELECT a.owner_human_id::text
		FROM endpoints e
		JOIN agents a ON a.id = e.agent_id
		WHERE e.id = $1 AND e.state = 'active'`, endpointID).Scan(&humanID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: endpoint owner human: %w", err)
	}
	return humanID, nil
}

// FriendEdgeAuthorizesDelivery reports whether an approved friend edge authorizes fromHumanID to
// deliver work INTO an endpoint owned by toHumanID.
//
// The direction is the whole security property here, so the reasoning is recorded rather than left
// to inference. friend_edges rows are per-direction and non-transitive (0005_friend_edges.sql):
// from_human is the REQUESTER, to_human is the human who owns the target side and whose approval is
// the sole grant ("the grant flows requester→target"). ApproveFriendRequest makes it concrete — the
// TARGET human's approval mints a scoped endpoint that the REQUESTER then authenticates to, and
// CreateForFriend consumes exactly that endpoint to enqueue a todo into the target human's queue. So
// an approved A→B edge means "A may hand work to B", and says nothing whatsoever about B handing
// work to A; B needs its own approved B→A edge for that.
//
// Routing webhook W (owned by human A) to an endpoint owned by human B makes A's deliveries land as
// todos B can list and claim — A handing work to B. The authorizing row is therefore
// (from_human = A, to_human = B, state = 'approved'): the SAME direction CreateForFriend already
// relies on. Checking (from_human = B, to_human = A) instead would let A push into B on the strength
// of B's request to push into A — a privilege escalation that inverts who consented.
//
// The check is on the human principals, not the personas, because ownership of both the webhook and
// the target endpoint is a human fact (agents.owner_human_id) and approval is a human act (ADR-0008).
// An edge with a NULL from_human (unattested requester) authorizes nothing: from_human is compared
// directly, so a NULL fails the predicate rather than matching an unknown principal. Only
// state='approved' counts — pending grants nothing, and denied/revoked are terminal.
//
// Governing: ADR-0010, SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges", ADR-0022.
func (s *Store) FriendEdgeAuthorizesDelivery(ctx context.Context, fromHumanID, toHumanID string) (bool, error) {
	if !isUUID(fromHumanID) || !isUUID(toHumanID) {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM friend_edges
			WHERE from_human = $1 AND to_human = $2 AND state = 'approved')`,
		fromHumanID, toHumanID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: friend edge authorizes delivery: %w", err)
	}
	return ok, nil
}

// isUUID guards uuid-typed predicates against caller-supplied ids. Without it an empty or malformed
// id reaches Postgres as a 22P02 cast error, which is itself a distinguishable signal (and a 500 on
// what should be a clean not-found). Mirrors endpointScope's rationale in todos.go.
func isUUID(id string) bool {
	if id == "" {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}
