package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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
// Slug is the endpoint's public URL segment (/mcp/{slug}); it is not a secret (SPEC-0014).
type Endpoint struct {
	ID               string
	AgentID          string
	Slug             string
	CredentialPrefix string
	ScopeQueues      []string
	ScopeVerbs       []string
	Mutability       string
	State            string
	CreatedAt        time.Time
	LastSeenAt       *time.Time // nil until the credential first authenticates (TouchEndpoint)
}

// AuthEndpoint is the minimal view resolved from a presented credential to authorize an agent call.
// The webhook ceiling (WebhookMax, WebhookSourceTypes, WebhookQueues) is the human-vended policy an
// agent's webhook self-management verbs operate strictly within (ADR-0012, SPEC-0006).
type AuthEndpoint struct {
	ID           string
	AgentID      string
	AgentName    string
	OwnerHumanID string
	Slug         string
	ScopeQueues  []string
	ScopeVerbs   []string
	// Webhook ceiling — the maximum number of self-managed webhooks, the source types an agent may
	// create, and the target queues those webhooks may route to. Enforced at the boundary before any
	// webhook mutation. Governing: ADR-0012, SPEC-0006 REQ "Webhook Self-Management Within a Vended
	// Ceiling".
	WebhookMax         int
	WebhookSourceTypes []string
	WebhookQueues      []string
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

// AgentNameByID resolves an agent's display name from its id, for rendering `claimed · <agent name>`
// lease-owner labels on the board (owners are stored as `agent:<agent id>`). Returns ErrNotFound for
// an unknown id. Governing: SPEC-0013 REQ "Board View — Live Incoming Lines" (claimed · <agent name>).
func (s *Store) AgentNameByID(ctx context.Context, id string) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM agents WHERE id = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("agent name by id: %w", err)
	}
	return name, nil
}

// MintSlug derives an endpoint's public URL slug from its agent's name plus a short random suffix
// so slugs are unique without being guessable from the name alone. The slug is not a secret: the
// URL grants nothing without the credential. Governing: SPEC-0014 REQ "Streamable HTTP MCP Endpoint".
func MintSlug(agentName string) (string, error) {
	var b strings.Builder
	prevDash := true // suppress a leading dash
	for _, r := range strings.ToLower(agentName) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	base := strings.TrimSuffix(b.String(), "-")
	if base == "" {
		base = "endpoint"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("store: mint slug: %w", err)
	}
	return base + "-" + hex.EncodeToString(suffix), nil
}

// CreateEndpoint vends a scoped endpoint for an agent. The caller supplies the credential hash + prefix
// (the plaintext is shown to the human once and never stored) and the minted URL slug. Scope is
// immutable (ADR-0008).
func (s *Store) CreateEndpoint(ctx context.Context, agentID, credHash, credPrefix, slug string, queues, verbs []string) (Endpoint, error) {
	return createEndpoint(ctx, s.pool, agentID, credHash, credPrefix, slug, queues, verbs)
}

// createEndpoint is the querier-based core of CreateEndpoint: it runs on either the pool or a
// transaction so approval-time vending (SPEC-0010) can mint the endpoint in the SAME transaction
// that transitions a friend edge to approved — approval is the vend, atomically. Governing:
// ADR-0008 (URL + credential together = the grant; scope immutable).
func createEndpoint(ctx context.Context, q querier, agentID, credHash, credPrefix, slug string, queues, verbs []string) (Endpoint, error) {
	var e Endpoint
	err := q.QueryRow(ctx, `
		INSERT INTO endpoints (agent_id, credential_hash, credential_prefix, slug, scope_queues, scope_verbs)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, agent_id::text, slug, credential_prefix, scope_queues, scope_verbs, mutability, state, created_at`,
		agentID, credHash, credPrefix, slug, queues, verbs,
	).Scan(&e.ID, &e.AgentID, &e.Slug, &e.CredentialPrefix, &e.ScopeQueues, &e.ScopeVerbs, &e.Mutability, &e.State, &e.CreatedAt)
	return e, err
}

// ListEndpoints returns an agent's endpoints, newest first.
func (s *Store) ListEndpoints(ctx context.Context, agentID string) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, agent_id::text, slug, credential_prefix, scope_queues, scope_verbs, mutability, state, created_at, last_seen_at
		FROM endpoints WHERE agent_id = $1 ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Slug, &e.CredentialPrefix, &e.ScopeQueues, &e.ScopeVerbs, &e.Mutability, &e.State, &e.CreatedAt, &e.LastSeenAt); err != nil {
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
		SELECT e.id::text, e.agent_id::text, ag.name, ag.owner_human_id::text, e.slug, e.scope_queues, e.scope_verbs,
		       e.webhook_max, e.webhook_source_types, e.webhook_queues
		FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE e.credential_hash = $1 AND e.state = 'active'`,
		credHash,
	).Scan(&a.ID, &a.AgentID, &a.AgentName, &a.OwnerHumanID, &a.Slug, &a.ScopeQueues, &a.ScopeVerbs,
		&a.WebhookMax, &a.WebhookSourceTypes, &a.WebhookQueues)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthEndpoint{}, ErrNotFound
	}
	return a, err
}

// TouchEndpoint stamps an endpoint's last_seen_at, recording that its credential just authenticated
// successfully, and fires the endpoint-seen hook after the stamp commits (the SPEC-0013
// endpoint_seen typed SSE event). Governing: SPEC-0014 REQ "Bearer Authentication Bound to the
// Vended Endpoint", SPEC-0013 REQ "Live Updates and Toasts".
func (s *Store) TouchEndpoint(ctx context.Context, endpointID string) error {
	var seenAt time.Time
	err := s.pool.QueryRow(ctx,
		`UPDATE endpoints SET last_seen_at = now() WHERE id = $1 RETURNING last_seen_at`,
		endpointID).Scan(&seenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Endpoint vanished between auth and stamp: nothing to record, nothing to announce.
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: touch endpoint: %w", err)
	}
	s.fireEndpointSeenHook(endpointID, seenAt)
	return nil
}
