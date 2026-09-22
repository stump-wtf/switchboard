package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Governing: ADR-0010 (A2A discovery + human-vended friending), SPEC-0010 REQ "Friend-Request
// Lifecycle", REQ "Approval Is the Vend, Narrow-Only", REQ "Per-Direction, Revocable, Non-Transitive
// Edges", REQ "Work Flows as Todos, Not A2A Tasks". This file owns the friend-edge state machine and
// the cross-agent work-handoff (create_for) flow that turns an accepted intent invocation into a
// durable todo. A pending request is surfaced to the target human straight from its friend_edges
// row, never as a todo (SPEC-0010 REQ "Approval Surfaced from the Friend Edge"); see the note above
// RemoveFriendEdge. The A2A intake route (#62) and the Friends web UI (#63) are out of scope.

// ErrInvalidTransition is returned when a friend-edge lifecycle transition is not legal from the
// edge's current state (e.g. approving an edge that is not pending, revoking one that is not
// approved). Distinct from ErrNotFound so callers can tell "not yours / gone" from "wrong state".
var ErrInvalidTransition = errors.New("store: invalid friend-edge transition")

// ErrScopeExceedsRequest is returned when an approval's granted_scope is not a subset of the
// requested_scope — narrow-only approval forbids granting more than was asked for (SPEC-0010).
var ErrScopeExceedsRequest = errors.New("store: granted scope exceeds requested scope")

// ErrFriendshipInactive is returned when a cross-agent work handoff is attempted against an edge
// that is not an active friendship — the edge is not approved, or its vended endpoint has been
// revoked (SPEC-0010 "Work Flows as Todos"; a pending/denied/revoked friendship grants nothing).
var ErrFriendshipInactive = errors.New("store: friendship not active")

// ErrIntentNotNegotiated is returned when a cross-agent handoff invokes an intent (verb) or targets
// a queue that is outside the friendship's negotiated (granted) scope. The vended endpoint's verb +
// queue scope is enforced at the boundary — a friend may only do exactly what was approved (SPEC-0010).
var ErrIntentNotNegotiated = errors.New("store: intent not negotiated for this friendship")

// FriendEdge is one per-direction, revocable, non-transitive friend link and its lifecycle. A
// pending edge grants nothing; approval mints EndpointID (the vend); revocation kills it. ADR-0010.
type FriendEdge struct {
	ID                 string
	FromPersona        string
	ToPersona          string
	Direction          string
	FromHuman          string // "" when unattested (null)
	ToHuman            string // owning/target human; the ownership-isolation key
	FromAgentID        string // "" until approval binds the vended agent (null)
	State              string // pending|approved|denied|revoked
	RequestedQueues    []string
	RequestedVerbs     []string
	GrantedQueues      []string
	GrantedVerbs       []string
	Reason             string
	ProvenanceVerified bool
	EndpointID         string // "" until approved (null)
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DecidedAt          *time.Time
	RevokedAt          *time.Time
	// EndpointLastSeenAt surfaces the vended endpoint's last_seen_at (stamped on every successful
	// credential authentication, SPEC-0007) for the Friends-view activity meta line (DESIGN
	// Friends: "last 2m ago"; story #175). Populated ONLY by ListFriendEdges; nil on write-path
	// returns, while no endpoint exists (pending/denied), or before the credential first
	// authenticates.
	EndpointLastSeenAt *time.Time
}

// friendEdgeCols is the canonical projection; null uuids collapse to ” so callers never juggle
// sql.Null* types (mirrors the COALESCE style in todos.go / agents.go).
const friendEdgeCols = `id::text, from_persona, to_persona, direction,
	COALESCE(from_human::text, ''), to_human::text, COALESCE(from_agent_id::text, ''), state,
	requested_queues, requested_verbs, granted_queues, granted_verbs,
	COALESCE(reason, ''), provenance_verified, COALESCE(endpoint_id::text, ''),
	created_at, updated_at, decided_at, revoked_at`

func scanFriendEdge(row pgx.Row) (FriendEdge, error) {
	var e FriendEdge
	err := row.Scan(&e.ID, &e.FromPersona, &e.ToPersona, &e.Direction,
		&e.FromHuman, &e.ToHuman, &e.FromAgentID, &e.State,
		&e.RequestedQueues, &e.RequestedVerbs, &e.GrantedQueues, &e.GrantedVerbs,
		&e.Reason, &e.ProvenanceVerified, &e.EndpointID,
		&e.CreatedAt, &e.UpdatedAt, &e.DecidedAt, &e.RevokedAt)
	return e, err
}

// CreateFriendRequestParams are the inputs to CreateFriendRequest — a pending edge that grants
// nothing. ToHuman (the owning/target human) is required; FromHuman is the OIDC-attested requester
// carried so the pending row itself is the legible who/why the target human decides on. The
// requesting agent is bound later, at approval time.
type CreateFriendRequestParams struct {
	FromPersona        string
	ToPersona          string
	Direction          string // defaults to "outbound" when empty
	FromHuman          string // "" when unattested
	ToHuman            string // required
	RequestedQueues    []string
	RequestedVerbs     []string
	Reason             string
	ProvenanceVerified bool
}

// CreateFriendRequest records a PENDING friend edge that confers no access until a human approves
// (SPEC-0010 "Pending edge grants nothing"). A second live (pending/approved) request for the same
// (from_persona, to_persona, direction) collides on the partial unique index and returns
// ErrConflict — the pending-edge-grants-nothing / anti-flood invariant.
func (s *Store) CreateFriendRequest(ctx context.Context, p CreateFriendRequestParams) (FriendEdge, error) {
	direction := p.Direction
	if direction == "" {
		direction = "outbound"
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO friend_edges
			(from_persona, to_persona, direction, from_human, to_human,
			 requested_queues, requested_verbs, reason, provenance_verified)
		VALUES ($1, $2, $3, NULLIF($4,'')::uuid, $5, $6, $7, NULLIF($8,''), $9)
		RETURNING `+friendEdgeCols,
		p.FromPersona, p.ToPersona, direction, p.FromHuman, p.ToHuman,
		nonNilStrings(p.RequestedQueues), nonNilStrings(p.RequestedVerbs), p.Reason, p.ProvenanceVerified)
	e, err := scanFriendEdge(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return FriendEdge{}, ErrConflict
		}
		return FriendEdge{}, err
	}
	return e, nil
}

// ApproveFriendRequestParams are the inputs to ApproveFriendRequest. Approval IS the vend: it
// narrows (never widens) the scope, binds AgentID as the vended agent, and mints the endpoint whose
// credential the caller supplies (plaintext shown once, only its hash stored). Empty GrantedQueues
// AND GrantedVerbs mean "no narrowing" — the minted scope equals the requested scope.
type ApproveFriendRequestParams struct {
	EdgeID           string
	OwnerHumanID     string // MUST equal the edge's to_human, or ErrNotFound
	AgentID          string // the local agent the minted endpoint is vended for (required)
	GrantedQueues    []string
	GrantedVerbs     []string
	CredentialHash   string
	CredentialPrefix string
	Slug             string
}

// ApproveFriendRequest transitions a pending edge to approved and mints a scoped endpoint in ONE
// transaction (approval is the vend). It rejects any granted_scope that is not a subset of the
// requested_scope (ErrScopeExceedsRequest); with no narrowing the minted scope equals the requested
// scope. Cross-owner or unknown edges are ErrNotFound; a non-pending edge is ErrInvalidTransition.
// Governing: SPEC-0010 REQ "Approval Is the Vend, Narrow-Only", ADR-0008 (approval mints the grant).
func (s *Store) ApproveFriendRequest(ctx context.Context, p ApproveFriendRequestParams) (FriendEdge, Endpoint, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FriendEdge{}, Endpoint{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed; rolls back on any early return

	// Lock the edge under the ownership predicate. A cross-owner or missing id is indistinguishable
	// (ErrNotFound), so ownership failures never leak existence (matches agents.go isolation).
	edge, err := scanFriendEdge(tx.QueryRow(ctx,
		`SELECT `+friendEdgeCols+` FROM friend_edges WHERE id = $1 AND to_human = $2 FOR UPDATE`,
		p.EdgeID, p.OwnerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FriendEdge{}, Endpoint{}, ErrNotFound
	}
	if err != nil {
		return FriendEdge{}, Endpoint{}, err
	}
	if edge.State != "pending" {
		return FriendEdge{}, Endpoint{}, ErrInvalidTransition
	}
	// A locally sent (direction=outgoing) pending edge awaits the REMOTE operator's approval; its
	// owner may only withdraw it. Approving it here would mint a vended endpoint no remote operator
	// ever consented to (SPEC-0010: both operators must approve; approval is the vend), so the
	// owner-side approval of an outgoing edge is an invalid transition — enforced here, not just in
	// the web confirm page, because this store owns every lifecycle rule.
	if edge.Direction == "outgoing" {
		return FriendEdge{}, Endpoint{}, ErrInvalidTransition
	}

	// Narrow-only: default to the requested scope, else validate the human's narrowing is a subset.
	grantedQueues, grantedVerbs := p.GrantedQueues, p.GrantedVerbs
	if len(grantedQueues) == 0 && len(grantedVerbs) == 0 {
		grantedQueues, grantedVerbs = edge.RequestedQueues, edge.RequestedVerbs
	}
	if !isSubset(grantedQueues, edge.RequestedQueues) || !isSubset(grantedVerbs, edge.RequestedVerbs) {
		return FriendEdge{}, Endpoint{}, ErrScopeExceedsRequest
	}

	// Approval is the vend: mint the scoped endpoint in this same transaction (ADR-0008).
	ep, err := createEndpoint(ctx, tx, p.AgentID, p.CredentialHash, p.CredentialPrefix, p.Slug,
		nonNilStrings(grantedQueues), nonNilStrings(grantedVerbs), nil, nil, 0, nil, nil)
	if err != nil {
		return FriendEdge{}, Endpoint{}, err
	}

	edge, err = scanFriendEdge(tx.QueryRow(ctx, `
		UPDATE friend_edges
		SET state = 'approved', from_agent_id = $2, granted_queues = $3, granted_verbs = $4,
		    endpoint_id = $5, decided_at = now(), updated_at = now()
		WHERE id = $1
		RETURNING `+friendEdgeCols,
		p.EdgeID, p.AgentID, nonNilStrings(grantedQueues), nonNilStrings(grantedVerbs), ep.ID))
	if err != nil {
		return FriendEdge{}, Endpoint{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FriendEdge{}, Endpoint{}, err
	}
	return edge, ep, nil
}

// DenyFriendRequest transitions a pending edge to denied (terminal; no endpoint minted). Cross-owner
// or unknown edges are ErrNotFound; a non-pending edge is ErrInvalidTransition. The requester MAY
// re-request later subject to quotas. Governing: SPEC-0010 REQ "Friend-Request Lifecycle".
func (s *Store) DenyFriendRequest(ctx context.Context, edgeID, ownerHumanID string) (FriendEdge, error) {
	return s.decideTerminal(ctx, edgeID, ownerHumanID, "pending", `
		UPDATE friend_edges SET state = 'denied', decided_at = now(), updated_at = now()
		WHERE id = $1 RETURNING `+friendEdgeCols)
}

// RevokeFriendEdge transitions an approved edge to revoked and kills its vended endpoint in ONE
// transaction — instant and one-sided: only this edge's endpoint dies, leaving the reverse-direction
// edge (a separate row) untouched. Cross-owner or unknown edges are ErrNotFound; a non-approved edge
// is ErrInvalidTransition. Governing: SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges".
func (s *Store) RevokeFriendEdge(ctx context.Context, edgeID, ownerHumanID string) (FriendEdge, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FriendEdge{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	edge, err := scanFriendEdge(tx.QueryRow(ctx,
		`SELECT `+friendEdgeCols+` FROM friend_edges WHERE id = $1 AND to_human = $2 FOR UPDATE`,
		edgeID, ownerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FriendEdge{}, ErrNotFound
	}
	if err != nil {
		return FriendEdge{}, err
	}
	if edge.State != "approved" {
		return FriendEdge{}, ErrInvalidTransition
	}

	// Kill the vended endpoint (ADR-0008 revoke = invalidate credential + unroute). Only this edge's
	// endpoint is touched, so revocation stays one-sided. The OAuth cascade runs in the SAME
	// transaction — every token minted onto the endpoint is revoked and every unspent code is
	// force-expired — mirroring RevokeEndpoint and ExpireEndpoints so friend-edge revocation is
	// instant and total from every credential's point of view. Governing: SPEC-0016 REQ
	// "Revocation Cascade", SPEC-0007 REQ "Instant, Total Revocation".
	if edge.EndpointID != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE endpoints SET state = 'revoked', revoked_at = now() WHERE id = $1 AND state = 'active'`,
			edge.EndpointID); err != nil {
			return FriendEdge{}, err
		}
		if err := deadLetterEndpointTodos(ctx, tx, []string{edge.EndpointID}); err != nil {
			return FriendEdge{}, err
		}
		if err := revokeEndpointOAuth(ctx, tx, []string{edge.EndpointID}); err != nil {
			return FriendEdge{}, err
		}
	}

	edge, err = scanFriendEdge(tx.QueryRow(ctx, `
		UPDATE friend_edges SET state = 'revoked', revoked_at = now(), updated_at = now()
		WHERE id = $1 RETURNING `+friendEdgeCols, edgeID))
	if err != nil {
		return FriendEdge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FriendEdge{}, err
	}
	return edge, nil
}

// decideTerminal locks an owned edge, asserts it is in fromState, and applies a terminal UPDATE.
// Shared by DenyFriendRequest (the pure-state terminal transition); revoke keeps its own body
// because it also touches the endpoint substrate transactionally.
func (s *Store) decideTerminal(ctx context.Context, edgeID, ownerHumanID, fromState, updateSQL string) (FriendEdge, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FriendEdge{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	edge, err := scanFriendEdge(tx.QueryRow(ctx,
		`SELECT `+friendEdgeCols+` FROM friend_edges WHERE id = $1 AND to_human = $2 FOR UPDATE`,
		edgeID, ownerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FriendEdge{}, ErrNotFound
	}
	if err != nil {
		return FriendEdge{}, err
	}
	if edge.State != fromState {
		return FriendEdge{}, ErrInvalidTransition
	}
	edge, err = scanFriendEdge(tx.QueryRow(ctx, updateSQL, edgeID))
	if err != nil {
		return FriendEdge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FriendEdge{}, err
	}
	return edge, nil
}

// ListFriendEdges returns the target human's inbound edges (to_human = ownerHumanID), newest first,
// optionally filtered to the given states. Non-transitive by construction: it enumerates only the
// owner's own edges and never traverses to a friend's friends/personas/queues. Passing no states
// returns every edge the owner owns. Each edge additionally carries EndpointLastSeenAt (a scalar
// subquery on the vended endpoint) so the Friends view can render real last-activity without a
// second round trip (#175). Governing: SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive
// Edges", REQ "Approval Delivered as a Todo" (list_pending_approvals).
func (s *Store) ListFriendEdges(ctx context.Context, ownerHumanID string, states ...string) ([]FriendEdge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+friendEdgeCols+`,
		       (SELECT e.last_seen_at FROM endpoints e WHERE e.id = friend_edges.endpoint_id)
		FROM friend_edges
		WHERE to_human = $1 AND (cardinality($2::text[]) = 0 OR state = ANY($2))
		ORDER BY created_at DESC`,
		ownerHumanID, nonNilStrings(states))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FriendEdge
	for rows.Next() {
		var e FriendEdge
		if err := rows.Scan(&e.ID, &e.FromPersona, &e.ToPersona, &e.Direction,
			&e.FromHuman, &e.ToHuman, &e.FromAgentID, &e.State,
			&e.RequestedQueues, &e.RequestedVerbs, &e.GrantedQueues, &e.GrantedVerbs,
			&e.Reason, &e.ProvenanceVerified, &e.EndpointID,
			&e.CreatedAt, &e.UpdatedAt, &e.DecidedAt, &e.RevokedAt, &e.EndpointLastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountLiveFriendRequestsFrom returns how many LIVE (pending or approved) friend edges a requesting
// human currently holds — the per-requester quota counter the A2A intake checks before recording a
// new pending edge. Denied/revoked edges are terminal and do not count, so a requester whose past
// requests were all resolved is never blocked. Governing: SPEC-0010 REQ "Anti-Spam — Bounded
// Discovery and Quotas" (friend requests quota'd per requester; over-quota creates no pending edge).
func (s *Store) CountLiveFriendRequestsFrom(ctx context.Context, fromHuman string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM friend_edges
		 WHERE from_human = NULLIF($1,'')::uuid AND state IN ('pending', 'approved')`,
		fromHuman).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count live friend requests: %w", err)
	}
	return n, nil
}

// A pending friend request is NOT a todo. It is surfaced to the target human directly from its
// friend_edges row — the row already carries the full legible who/why (from_human, from_persona,
// to_persona, requested_queues, requested_verbs, reason, provenance_verified, created_at), so a
// parallel todo was pure duplication of a durable record the Friends view already reads via
// ListFriendEdges (owner-scoped on to_human). It could not survive ADR-0022 either: an approval todo
// is HUMAN work with no owning endpoint, and todos.endpoint_id is NOT NULL.
//
// Duplicate suppression, which the approval todo nominally provided via its idempotency key, lives
// where it belongs — the partial unique index idx_friend_edges_live on
// (from_persona, to_persona, direction) WHERE state IN ('pending','approved') (migration 0005). All
// three indexed columns are NOT NULL, so there is no NULLS-distinct escape hatch: a re-delivered
// intake collides in CreateFriendRequest and surfaces as ErrConflict rather than double-listing.
// (The old approval-todo key never actually deduped — it hung off a null endpoint_id, so its
// ON CONFLICT arm never fired.)
//
// Delegating approval to an agent later WILL mint a normal endpoint-owned todo pinned to the
// delegate's endpoint; the tenant-isolation rules in SPEC-0003 already govern that case.
//
// Governing: ADR-0022, SPEC-0010 REQ "Approval Surfaced from the Friend Edge",
// SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".

// RemoveFriendEdge deletes an owner-scoped edge whose current state is one of fromStates, returning
// the edge as it was before deletion. It backs the Friends view "Withdraw" (a pending request) and
// "Unblock" (removing a terminal denied/revoked entry from the ledger) actions. Ownership isolation
// holds: a cross-owner or missing id is ErrNotFound (byte-identical, never leaking existence), and an
// edge in a state outside fromStates is ErrInvalidTransition with the row left untouched. Deleting the
// edge never touches the endpoints substrate (the FK is edge→endpoint ON DELETE SET NULL); a revoked
// edge's endpoint was already killed by RevokeFriendEdge. Governing: SPEC-0013 Endpoints table POST
// /friends/{id}/withdraw|unblock, SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges".
func (s *Store) RemoveFriendEdge(ctx context.Context, edgeID, ownerHumanID string, fromStates ...string) (FriendEdge, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FriendEdge{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	edge, err := scanFriendEdge(tx.QueryRow(ctx,
		`SELECT `+friendEdgeCols+` FROM friend_edges WHERE id = $1 AND to_human = $2 FOR UPDATE`,
		edgeID, ownerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FriendEdge{}, ErrNotFound
	}
	if err != nil {
		return FriendEdge{}, err
	}
	if !containsString(fromStates, edge.State) {
		return FriendEdge{}, ErrInvalidTransition
	}
	if _, err := tx.Exec(ctx, `DELETE FROM friend_edges WHERE id = $1`, edgeID); err != nil {
		return FriendEdge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FriendEdge{}, err
	}
	return edge, nil
}

// containsString reports whether v is in set (small linear scan for the RemoveFriendEdge state guard).
func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// CreateForFriendParams are the inputs to CreateForFriend — a cross-agent work handoff. EndpointID is
// the vended endpoint the presented credential authenticated to (the boundary resolves it via
// EndpointByCredHash); it identifies the friendship. Intent is the negotiated verb being invoked
// (e.g. "create_for"); Queue is the target queue. Both MUST fall inside the friendship's granted
// scope. Title/Payload/IdempotencyKey are the work itself; IdempotencyKey dedups the handoff.
type CreateForFriendParams struct {
	EndpointID     string
	Queue          string
	Intent         string
	Title          string
	Payload        []byte
	IdempotencyKey string
}

// CreateForFriend is the create_for verb backend: a friended remote agent, authenticated by its
// vended MCP endpoint credential, hands work to the target human — and that work lands as a DURABLE
// TODO in the granted queue, never as an ephemeral A2A peer task (the todo-queue IS the transport).
// It enforces the friendship is active (edge approved + endpoint live) and that the invoked intent
// and queue are both inside the negotiated grant, rejecting anything outside it, then enqueues the
// todo attributed to the requesting persona. Returns ErrFriendshipInactive when no active friendship
// backs the endpoint, and ErrIntentNotNegotiated when the intent or queue is out of scope. The
// returned bool reports whether a new todo was created (false = an idempotent duplicate).
// Governing: ADR-0010 (A2A discovers, the todo-queue transports), SPEC-0010 REQ "Work Flows as
// Todos, Not A2A Tasks", ADR-0007 (todos as the durable core primitive).
func (s *Store) CreateForFriend(ctx context.Context, p CreateForFriendParams) (Todo, bool, error) {
	// Resolve the friendship from the vended endpoint. It must be approved AND its endpoint still
	// active, so a pending edge (no endpoint), a denied one, or a revoked one (endpoint killed) all
	// collapse to "no active friendship" — indistinguishable, leaking no lifecycle state. The
	// endpoint check is an EXISTS subquery (not a JOIN) so the friendEdgeCols projection stays
	// single-table and its bare `id`/`state` columns never collide with the endpoints table.
	edge, err := scanFriendEdge(s.pool.QueryRow(ctx, `
		SELECT `+friendEdgeCols+`
		FROM friend_edges
		WHERE endpoint_id = $1 AND state = 'approved'
		  AND EXISTS (SELECT 1 FROM endpoints e WHERE e.id = friend_edges.endpoint_id AND e.state = 'active')`,
		p.EndpointID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, ErrFriendshipInactive
	}
	if err != nil {
		return Todo{}, false, err
	}

	// Enforce the negotiated scope at the boundary: the invoked intent MUST be a granted verb and the
	// target queue MUST be a granted queue. A friend may do exactly what was approved, nothing wider.
	if !isSubset([]string{p.Intent}, edge.GrantedVerbs) || !isSubset([]string{p.Queue}, edge.GrantedQueues) {
		return Todo{}, false, ErrIntentNotNegotiated
	}

	// Governing: SPEC-0010 REQ "Work Flows as Todos, Not A2A Tasks". Namespace the caller-supplied
	// idempotency key by the friendship identity (the friend-edge id) before it reaches the flat,
	// global (queue, idempotency_key) dedup namespace. Two friends granted the same queue would
	// otherwise share one namespace: friend B replaying friend A's key on a shared queue would get
	// created=false, mint nothing, AND receive A's existing Todo — Payload and Source included — back
	// from the store. Prefixing with the edge id isolates each friendship's keyspace so B can neither
	// collide with, suppress, nor read A's handoff. A friend's own legitimate retry keeps the same
	// endpoint → same edge → same prefix, so idempotent replay for the rightful sender is preserved.
	// An empty key stays empty (never namespaced) so it continues to opt out of dedup entirely.
	idempotencyKey := p.IdempotencyKey
	if idempotencyKey != "" {
		idempotencyKey = "friend:" + edge.ID + ":" + idempotencyKey
	}

	// The work is just a todo: durable, owned, dedup'd, leaseable. Attribute it to the requesting
	// persona (who handed the work) and record the intent as the kind. CreateTodo rings the
	// LISTEN/NOTIFY doorbell so a worker on the granted queue wakes without polling.
	//
	// The owning endpoint is the friendship's own vended endpoint (edge.endpoint_id, which the query
	// above already proved equals p.EndpointID and is live). That endpoint belongs to the TARGET's
	// tenant — approval minted it onto an agent the target CHOSE (ApproveFriendRequest's p.AgentID),
	// so the handed-off work lands under an agent the target designated to handle this friendship,
	// and it is separated from every other friendship by endpoint_id rather than by queue string.
	// That is exactly the leak ADR-0022 closes: two friendships granted the same queue name no
	// longer share a visibility surface.
	//
	// Be precise about what this does NOT give you, because the endpoint's OWNERSHIP and its
	// CREDENTIAL diverge here and only ownership is a tenancy boundary:
	//
	//   - The requesting friend RETAINS access to work it hands over. ADR-0010 delivers this
	//     endpoint's plaintext credential to the remote agent (see internal/web/friends.go), so the
	//     requester authenticates AS this endpoint and its list_todos/claim reach these rows —
	//     subject to the verbs the target granted. Handing work over is not a transfer of custody.
	//   - The target's OTHER endpoints cannot see this work. Visibility is per-endpoint, so the
	//     target drains a friendship through the agent it designated at approval, not through an
	//     unrelated endpoint that merely shares the queue name.
	//
	// Both properties are pinned by TestCreateForFriendHandoffReachability. Whether they are the
	// desired shape of a handoff is a SPEC-0010 question, not something this function may decide
	// unilaterally — it is recorded here so the next reader inherits the fact, not an assumption.
	// Governing: ADR-0022, ADR-0010, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)",
	// SPEC-0010 REQ "Work Flows as Todos, Not A2A Tasks".
	return s.CreateTodo(ctx, CreateTodoParams{
		EndpointID:     p.EndpointID,
		Queue:          p.Queue,
		Source:         edge.FromPersona,
		Kind:           p.Intent,
		Title:          p.Title,
		Payload:        p.Payload,
		IdempotencyKey: idempotencyKey,
		// Source is the persona name, free text that must never reach a metric label; count the
		// creation as source="friend" instead. Governing: SPEC-0023 REQ-3, REQ-5.
		origin: "friend",
	})
}

// isSubset reports whether every element of sub is present in super (set containment). Used to
// enforce narrow-only approval: granted_scope ⊆ requested_scope (SPEC-0010).
func isSubset(sub, super []string) bool {
	set := make(map[string]struct{}, len(super))
	for _, v := range super {
		set[v] = struct{}{}
	}
	for _, v := range sub {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}

// nonNilStrings normalizes a nil slice to an empty (non-nil) one so pgx binds a SQL empty array
// rather than NULL for NOT NULL text[] columns and ANY(...) filters.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
