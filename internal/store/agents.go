package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Agent is a lightweight owned record; registration grants nothing. ADR-0008.
type Agent struct {
	ID           string
	OwnerHumanID string
	Name         string
	Description  string
	CreatedAt    time.Time
}

// Endpoint is a vended capability: URL + credential = the grant, scope is immutable. ADR-0008.
type Endpoint struct {
	ID               string
	AgentID          string
	CredentialPrefix string
	ScopeQueues      []string
	ScopeVerbs       []string
	Mutability       string
	State            string
	CreatedAt        time.Time
}

// AuthEndpoint is the minimal view resolved from a presented credential to authorize an agent call.
type AuthEndpoint struct {
	ID           string
	AgentID      string
	AgentName    string
	OwnerHumanID string
	ScopeQueues  []string
	ScopeVerbs   []string
}

// CreateAgent registers an agent owned by a human.
func (s *Store) CreateAgent(ctx context.Context, ownerHumanID, name, description string) (Agent, error) {
	var a Agent
	err := s.pool.QueryRow(ctx, `
		INSERT INTO agents (owner_human_id, name, description)
		VALUES ($1, $2, NULLIF($3, ''))
		RETURNING id::text, owner_human_id::text, name, COALESCE(description, ''), created_at`,
		ownerHumanID, name, description,
	).Scan(&a.ID, &a.OwnerHumanID, &a.Name, &a.Description, &a.CreatedAt)
	return a, err
}

// ListAgents returns a human's agents, newest first.
func (s *Store) ListAgents(ctx context.Context, ownerHumanID string) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, owner_human_id::text, name, COALESCE(description, ''), created_at
		FROM agents WHERE owner_human_id = $1 ORDER BY created_at DESC`, ownerHumanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.OwnerHumanID, &a.Name, &a.Description, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAgentOwned returns an agent only if owned by the given human (authorization guard).
func (s *Store) GetAgentOwned(ctx context.Context, id, ownerHumanID string) (Agent, error) {
	var a Agent
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, owner_human_id::text, name, COALESCE(description, ''), created_at
		FROM agents WHERE id = $1 AND owner_human_id = $2`, id, ownerHumanID,
	).Scan(&a.ID, &a.OwnerHumanID, &a.Name, &a.Description, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// CreateEndpoint vends a scoped endpoint for an agent. The caller supplies the credential hash + prefix
// (the plaintext is shown to the human once and never stored). Scope is immutable (ADR-0008).
func (s *Store) CreateEndpoint(ctx context.Context, agentID, credHash, credPrefix string, queues, verbs []string) (Endpoint, error) {
	var e Endpoint
	err := s.pool.QueryRow(ctx, `
		INSERT INTO endpoints (agent_id, credential_hash, credential_prefix, scope_queues, scope_verbs)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, agent_id::text, credential_prefix, scope_queues, scope_verbs, mutability, state, created_at`,
		agentID, credHash, credPrefix, queues, verbs,
	).Scan(&e.ID, &e.AgentID, &e.CredentialPrefix, &e.ScopeQueues, &e.ScopeVerbs, &e.Mutability, &e.State, &e.CreatedAt)
	return e, err
}

// ListEndpoints returns an agent's endpoints, newest first.
func (s *Store) ListEndpoints(ctx context.Context, agentID string) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, agent_id::text, credential_prefix, scope_queues, scope_verbs, mutability, state, created_at
		FROM endpoints WHERE agent_id = $1 ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.ID, &e.AgentID, &e.CredentialPrefix, &e.ScopeQueues, &e.ScopeVerbs, &e.Mutability, &e.State, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RevokeEndpoint marks an endpoint revoked, but only if it belongs to the given human (via its agent).
// Revoke = invalidate credential + unroute; instant and total (ADR-0008).
func (s *Store) RevokeEndpoint(ctx context.Context, endpointID, ownerHumanID string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE endpoints SET state = 'revoked', revoked_at = now()
		WHERE id = $1 AND state = 'active'
		  AND agent_id IN (SELECT id FROM agents WHERE owner_human_id = $2)`,
		endpointID, ownerHumanID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EndpointByCredHash resolves an active endpoint from a presented credential hash, for agent auth.
// Returns ErrNotFound for unknown or revoked credentials.
func (s *Store) EndpointByCredHash(ctx context.Context, credHash string) (AuthEndpoint, error) {
	var a AuthEndpoint
	err := s.pool.QueryRow(ctx, `
		SELECT e.id::text, e.agent_id::text, ag.name, ag.owner_human_id::text, e.scope_queues, e.scope_verbs
		FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE e.credential_hash = $1 AND e.state = 'active'`,
		credHash,
	).Scan(&a.ID, &a.AgentID, &a.AgentName, &a.OwnerHumanID, &a.ScopeQueues, &a.ScopeVerbs)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthEndpoint{}, ErrNotFound
	}
	return a, err
}
