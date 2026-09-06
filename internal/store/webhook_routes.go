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
// the union of {ownerEndpointID} and every explicit webhook_routes target that is STILL AUTHORIZED,
// de-duplicated. This is the delivery-path read the self-managed receiver calls once per delivery to
// decide how many todos to create. An empty slice (owner not resolvable) means no work is produced —
// the caller SHOULD treat that as a misconfiguration and 503 rather than silently dropping the
// delivery.
//
// Authorization is re-evaluated HERE, on every delivery, not merely at grant time. A webhook_routes
// row is a standing grant, and the two facts it rests on are both revocable after the fact:
//
//   - the target endpoint may have been revoked (or expired), and
//   - the approved friend edge that permitted a CROSS-HUMAN target may have been revoked.
//
// Nothing deletes the route row when either happens: RevokeFriendEdge and RevokeEndpoint only flip
// state to 'revoked', so the ON DELETE CASCADE on target_endpoint_id never fires, and the webhook's
// owner (the only principal who may call remove_webhook_route) is precisely the human who has no
// incentive to. Without this re-check a friendship torn down months ago would keep minting todos,
// carrying the sender's raw payloads, into a tenant that withdrew consent — unbounded and with no
// off switch on the receiving side. That directly contradicts SPEC-0010's "instant, total" revocation,
// and it is the guarantee list_webhook_routes/remove_webhook_route already cite when they skip target
// re-authorization ("the delivery path is where a revoked endpoint stops mattering"). This is that
// place; the claim is now true.
//
// A target that fails the re-check is skipped silently rather than failing the delivery: the other
// targets' work is still valid, and a revoked friendship is a normal end state, not an error the
// producer can act on.
//
// The OWNER endpoint gets the identical liveness re-check, and that is not symmetry for its own
// sake. It used to be seeded unconditionally, on the reasoning that "the receiver has already
// resolved that endpoint to accept the request at all" — but the receiver resolves the webhook by
// its ingest TOKEN (GetWebhookByToken), and that lookup has never touched endpoint state. So a
// revoked owner kept minting todos on every delivery, forever: an endpoint revoked on 2026-08-22
// had accumulated 1,130 pending rows by 2026-09-06, none of which any session could ever claim,
// because revoking the credential is exactly what makes them unclaimable. Silent, unbounded, and
// with no off switch short of deleting the webhook.
//
// When the owner is dead and no live route remains, the target set is empty and the receiver's
// existing misconfiguration branch answers 503 — which is the honest reply. A webhook whose owner
// was revoked IS misconfigured; telling the producer beats accepting deliveries into a hole.
//
// Governing: ADR-0022, ADR-0010, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges".
func (s *Store) ResolveWebhookTargets(ctx context.Context, webhookID, ownerEndpointID string) ([]string, error) {
	// The owner endpoint is a target while it is still live. Explicit routes may add more. De-dup
	// preserving the owner first so the first todo is always the owner's (stable ordering aids
	// testing).
	//
	// An UNRESOLVABLE owner — empty id, or one that no longer passes the liveness re-check — is
	// skipped rather than seeded. Seeding an empty id would return a one-element slice holding "",
	// which reads as a target to every caller and only fails much later, inside createTodo, as a
	// 500 on a delivery the contract says must be a 503. The empty return this function documents
	// has to actually be reachable for the caller's misconfiguration branch to mean anything.
	seen := map[string]struct{}{}
	var out []string
	if ownerEndpointID != "" {
		var live bool
		// Same predicate the routes below use, for the same reason: expiry is checked directly
		// rather than trusting the state flag, because ExpireEndpoints only flips the row on a 30s
		// reaper tick and delivery must not be routable in that window.
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM endpoints
				WHERE id = $1 AND state = 'active'
				  AND (expires_at IS NULL OR expires_at > now())
			)`, ownerEndpointID).Scan(&live); err != nil {
			return nil, fmt.Errorf("store: resolve webhook owner liveness: %w", err)
		}
		if live {
			seen[ownerEndpointID] = struct{}{}
			out = append(out, ownerEndpointID)
		}
	}
	// The route survives only while BOTH of its underlying facts still hold. Expressed as one
	// statement so the check is atomic with the read and cannot drift from it:
	//
	//   - the target endpoint is live: state='active' AND not past its expiry. Expiry is checked
	//     directly rather than trusting the state flag, because ExpireEndpoints only flips the row
	//     on a 30s reaper tick — EndpointByCredHash already refuses a passed expiry ahead of the
	//     reaper for exactly this reason, and delivery must not be routable in that window either.
	//   - the target is same-human as the webhook's owner, OR an approved friend edge still runs
	//     from the webhook owner's human TO the target's human. The direction matches
	//     FriendEdgeAuthorizesDelivery: an approved A→B edge means "A may hand work to B".
	rows, err := s.pool.Query(ctx, `
		SELECT r.target_endpoint_id::text
		FROM webhook_routes r
		JOIN endpoints te ON te.id = r.target_endpoint_id
		JOIN agents    ta ON ta.id = te.agent_id
		JOIN endpoint_webhooks w ON w.id = r.webhook_id
		JOIN endpoints oe ON oe.id = w.endpoint_id
		JOIN agents    oa ON oa.id = oe.agent_id
		WHERE r.webhook_id = $1
		  AND te.state = 'active'
		  AND (te.expires_at IS NULL OR te.expires_at > now())
		  AND (
		        ta.owner_human_id = oa.owner_human_id
		     OR EXISTS (SELECT 1 FROM friend_edges fe
		                 WHERE fe.from_human = oa.owner_human_id
		                   AND fe.to_human   = ta.owner_human_id
		                   AND fe.state      = 'approved')
		  )`, webhookID)
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

// IsUUID is the exported form of isUUID, for callers outside this package that must reject a
// malformed id BEFORE it reaches a uuid-typed predicate — e.g. the MCP verbs that skip the
// store-side authorization reads where isUUID is otherwise applied. Same contract: no allocation,
// no I/O, purely a shape check.
func IsUUID(id string) bool { return isUUID(id) }

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
