package store

import (
	"context"
	"encoding/json"
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
// durable todo. The A2A intake route (#62) and the approval-todo/web UI (#63) are out of scope.

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
// carried for the legible approval todo. The requesting agent is bound later, at approval time.
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
	defer tx.Rollback(ctx) // no-op once committed; rolls back on any early return

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
		nonNilStrings(grantedQueues), nonNilStrings(grantedVerbs), nil)
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
	defer tx.Rollback(ctx)

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
	// endpoint is touched, so revocation stays one-sided.
	if edge.EndpointID != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE endpoints SET state = 'revoked', revoked_at = now() WHERE id = $1 AND state = 'active'`,
			edge.EndpointID); err != nil {
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
	defer tx.Rollback(ctx)

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
// returns every edge the owner owns. Governing: SPEC-0010 REQ "Per-Direction, Revocable,
// Non-Transitive Edges", REQ "Approval Delivered as a Todo" (list_pending_approvals).
func (s *Store) ListFriendEdges(ctx context.Context, ownerHumanID string, states ...string) ([]FriendEdge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+friendEdgeCols+` FROM friend_edges
		WHERE to_human = $1 AND (cardinality($2::text[]) = 0 OR state = ANY($2))
		ORDER BY created_at DESC`,
		ownerHumanID, nonNilStrings(states))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FriendEdge
	for rows.Next() {
		e, err := scanFriendEdge(rows)
		if err != nil {
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

// approvalQueue is the durable queue a target human's friend-approval todos land in. The operator
// drains it from the Friends view (approve is the vend, SPEC-0010) exactly like any other todo.
const approvalQueue = "friend-approvals"

// approvalTodoKind is the kind stamped on a friend-approval todo so the queue view and the Friends
// surface can recognize it.
const approvalTodoKind = "friend.request"

// approvalKey is the idempotency key of the approval todo for one edge — a stable derivation so the
// "create the approval todo if not already" contract dedups a re-delivered intake onto one todo.
func approvalKey(edgeID string) string { return "friend-approval:" + edgeID }

// ApprovalTodoParams carries the legible who/why a friend request surfaces to the target human as a
// durable approval todo. Fields mirror the edge that provoked it. Governing: SPEC-0010 REQ "Approval
// Delivered as a Todo".
type ApprovalTodoParams struct {
	EdgeID             string
	FromHuman          string
	FromPersona        string
	ToPersona          string
	ToHuman            string // the owning/target human — routed via the todo assignee
	RequestedQueues    []string
	RequestedVerbs     []string
	Reason             string
	ProvenanceVerified bool
}

// CreateApprovalTodo records the durable approval todo for a pending friend edge: it lands in the
// target human's approvals queue carrying request_id, from_human, from_persona, to_persona,
// requested_scope, reason, and provenance_verified so the human can decide with full context. It is
// idempotent on the edge id (a re-delivered intake collapses onto the same todo — "if not already").
// Governing: SPEC-0010 REQ "Approval Delivered as a Todo".
func (s *Store) CreateApprovalTodo(ctx context.Context, p ApprovalTodoParams) (Todo, bool, error) {
	payload, err := json.Marshal(map[string]any{
		"request_id":          p.EdgeID,
		"from_human":          p.FromHuman,
		"from_persona":        p.FromPersona,
		"to_persona":          p.ToPersona,
		"requested_queues":    nonNilStrings(p.RequestedQueues),
		"requested_verbs":     nonNilStrings(p.RequestedVerbs),
		"reason":              p.Reason,
		"provenance_verified": p.ProvenanceVerified,
	})
	if err != nil {
		return Todo{}, false, fmt.Errorf("store: marshal approval todo payload: %w", err)
	}
	return s.CreateTodo(ctx, CreateTodoParams{
		Queue:          approvalQueue,
		Source:         p.FromPersona,
		Kind:           approvalTodoKind,
		Title:          "Friend request · " + p.FromPersona + " → " + p.ToPersona,
		Payload:        payload,
		Assignee:       p.ToHuman,
		IdempotencyKey: approvalKey(p.EdgeID),
	})
}

// ResolveApprovalTodo marks the approval todo for edgeID done (best-effort) once its friend edge is
// decided (approved or declined) from the Friends view, so the durable queue entry clears rather
// than lingering. A missing todo is a no-op. Governing: SPEC-0010 REQ "Approval Delivered as a Todo".
func (s *Store) ResolveApprovalTodo(ctx context.Context, edgeID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE todos SET state = 'done', completed_at = now(), updated_at = now()
		 WHERE queue = $1 AND idempotency_key = $2 AND state NOT IN ('done', 'failed')`,
		approvalQueue, approvalKey(edgeID))
	if err != nil {
		return fmt.Errorf("store: resolve approval todo: %w", err)
	}
	return nil
}

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
	defer tx.Rollback(ctx)

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
	return s.CreateTodo(ctx, CreateTodoParams{
		Queue:          p.Queue,
		Source:         edge.FromPersona,
		Kind:           p.Intent,
		Title:          p.Title,
		Payload:        p.Payload,
		IdempotencyKey: idempotencyKey,
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
