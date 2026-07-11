package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Governing: ADR-0010 (A2A discovery + human-vended friending), SPEC-0010 REQ "Friend-Request
// Lifecycle", REQ "Approval Is the Vend, Narrow-Only", REQ "Per-Direction, Revocable, Non-Transitive
// Edges". This file owns the friend-edge state machine only; the A2A intake route (#62), the
// approval-todo/web UI (#63), and the cross-agent todo flow (#64) are deliberately out of scope.

// ErrInvalidTransition is returned when a friend-edge lifecycle transition is not legal from the
// edge's current state (e.g. approving an edge that is not pending, revoking one that is not
// approved). Distinct from ErrNotFound so callers can tell "not yours / gone" from "wrong state".
var ErrInvalidTransition = errors.New("store: invalid friend-edge transition")

// ErrScopeExceedsRequest is returned when an approval's granted_scope is not a subset of the
// requested_scope — narrow-only approval forbids granting more than was asked for (SPEC-0010).
var ErrScopeExceedsRequest = errors.New("store: granted scope exceeds requested scope")

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
		nonNilStrings(grantedQueues), nonNilStrings(grantedVerbs))
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
