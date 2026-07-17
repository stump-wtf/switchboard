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
// immutable (ADR-0008). The endpoint is bound to no persona (persona_id NULL) — persona binding is
// done through VendAgentEndpoint, which validates the persona against the endpoint's agent.
func (s *Store) CreateEndpoint(ctx context.Context, agentID, credHash, credPrefix, slug string, queues, verbs []string) (Endpoint, error) {
	return createEndpoint(ctx, s.pool, agentID, credHash, credPrefix, slug, queues, verbs, nil)
}

// createEndpoint is the querier-based core of CreateEndpoint: it runs on either the pool or a
// transaction so approval-time vending (SPEC-0010) can mint the endpoint in the SAME transaction
// that transitions a friend edge to approved — approval is the vend, atomically. Governing:
// ADR-0008 (URL + credential together = the grant; scope immutable). personaID is nil for an
// agent-level endpoint, or a persona uuid (as a string) to scope the endpoint to a persona of the
// SAME agent — the caller is responsible for that same-agent invariant (ADR-0009).
func createEndpoint(ctx context.Context, q querier, agentID, credHash, credPrefix, slug string, queues, verbs []string, personaID any) (Endpoint, error) {
	var e Endpoint
	err := q.QueryRow(ctx, `
		INSERT INTO endpoints (agent_id, credential_hash, credential_prefix, slug, scope_queues, scope_verbs, persona_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, agent_id::text, slug, credential_prefix, scope_queues, scope_verbs, mutability, state, created_at`,
		agentID, credHash, credPrefix, slug, queues, verbs, personaID,
	).Scan(&e.ID, &e.AgentID, &e.Slug, &e.CredentialPrefix, &e.ScopeQueues, &e.ScopeVerbs, &e.Mutability, &e.State, &e.CreatedAt)
	return e, err
}

// VendParams carries the inputs for VendAgentEndpoint: the vending human, the new agent's name (used
// only when no persona is bound), an optional persona id, and the pre-minted credential material +
// URL slug + immutable scope. The plaintext credential never enters the store — the caller mints it
// and keeps the plaintext for the one-time reveal, handing the store only the hash + display prefix.
type VendParams struct {
	OwnerHumanID string
	Name         string // agent name for the freshly created agent; ignored when PersonaID is set
	PersonaID    string // "" = agent-level endpoint; otherwise a persona the endpoint is scoped to
	CredHash     string
	CredPrefix   string
	Slug         string
	Queues       []string
	Verbs        []string
}

// VendResult is what VendAgentEndpoint returns: the backing agent's name (for the one-time reveal)
// and the freshly minted endpoint.
type VendResult struct {
	AgentName string
	Endpoint  Endpoint
}

// VendAgentEndpoint mints an agent+endpoint (or a persona-scoped endpoint) in ONE transaction, so a
// failure minting the endpoint can never orphan a freshly created agent. Two modes:
//
//   - No persona (PersonaID == ""): creates a new agent named Name and vends an agent-level endpoint
//     on it. Agent create + endpoint create commit together or not at all.
//   - With a persona (PersonaID != ""): resolves the persona within the vending human's ownership and
//     vends the endpoint on the persona's OWN backing agent (Name is ignored), binding persona_id.
//     Because the endpoint reuses the persona's agent, the bound persona always backs the SAME agent
//     the endpoint is vended for. A persona that is unknown or owned by another human yields no row →
//     ErrNotFound, so a vend can never bind a foreign persona or one that backs a different agent.
//
// Governing: ADR-0008 (URL + credential = the grant, minted atomically), ADR-0009 (persona_id scopes
// a vended endpoint to a persona of the same agent), SPEC-0013 REQ "Endpoints View and Vend Modal".
func (s *Store) VendAgentEndpoint(ctx context.Context, p VendParams) (VendResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VendResult{}, fmt.Errorf("store: vend begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed; rolls back on any early return

	var agentID, agentName string
	var personaID any // nil → persona_id NULL
	if p.PersonaID != "" {
		// Owner-scoped join: an unknown or cross-owner persona returns no row (ErrNotFound), so the
		// binding can never reach another human's persona, and the endpoint is always vended on the
		// persona's own agent — the same-agent invariant holds by construction, never by trust.
		err = tx.QueryRow(ctx, `
			SELECT a.id::text, a.name
			FROM personas pe JOIN agents a ON a.id = pe.agent_id
			WHERE pe.id = $1 AND pe.owner_human_id = $2`,
			p.PersonaID, p.OwnerHumanID,
		).Scan(&agentID, &agentName)
		if errors.Is(err, pgx.ErrNoRows) {
			return VendResult{}, ErrNotFound
		}
		if err != nil {
			return VendResult{}, fmt.Errorf("store: vend resolve persona: %w", err)
		}
		personaID = p.PersonaID
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO agents (owner_human_id, name)
			VALUES ($1, $2)
			RETURNING id::text, name`,
			p.OwnerHumanID, p.Name,
		).Scan(&agentID, &agentName)
		if err != nil {
			return VendResult{}, fmt.Errorf("store: vend create agent: %w", err)
		}
	}

	ep, err := createEndpoint(ctx, tx, agentID, p.CredHash, p.CredPrefix, p.Slug, p.Queues, p.Verbs, personaID)
	if err != nil {
		return VendResult{}, fmt.Errorf("store: vend create endpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return VendResult{}, fmt.Errorf("store: vend commit: %w", err)
	}
	return VendResult{AgentName: agentName, Endpoint: ep}, nil
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

// EndpointCard is the operator Endpoints-view projection of a vended endpoint (SPEC-0013 REQ
// "Endpoints View and Vend Modal"): the endpoint enriched with its agent's name and, when personas
// are enabled, the bound persona's name. It never carries the credential — only its non-secret
// display prefix — so the cards can render without ever touching a reversible secret.
type EndpointCard struct {
	ID               string
	AgentID          string
	AgentName        string
	PersonaName      string // "" when no persona is bound (or personas are disabled)
	Slug             string
	CredentialPrefix string
	ScopeQueues      []string
	ScopeVerbs       []string
	State            string
	CreatedAt        time.Time
	RevokedAt        *time.Time // set once the endpoint was killed (revoke), for the dimmed card stamp
	LastSeenAt       *time.Time // nil until the credential first authenticates
}

// ListEndpointCards returns every endpoint owned by a human (via its agents), enriched with the
// backing agent name and bound persona name, active endpoints first then newest-first. It scopes
// strictly by owner_human_id so one operator can never see another's endpoints.
// Governing: SPEC-0013 REQ "Endpoints View and Vend Modal", SPEC-0007 REQ "Human as Accountable
// Principal".
func (s *Store) ListEndpointCards(ctx context.Context, ownerHumanID string) ([]EndpointCard, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id::text, e.agent_id::text, ag.name, COALESCE(p.name, ''),
		       e.slug, e.credential_prefix, e.scope_queues, e.scope_verbs,
		       e.state, e.created_at, e.revoked_at, e.last_seen_at
		FROM endpoints e
		JOIN agents ag ON ag.id = e.agent_id
		LEFT JOIN personas p ON p.id = e.persona_id
		WHERE ag.owner_human_id = $1
		ORDER BY (e.state = 'active') DESC, e.created_at DESC`, ownerHumanID)
	if err != nil {
		return nil, fmt.Errorf("store: list endpoint cards: %w", err)
	}
	defer rows.Close()
	var out []EndpointCard
	for rows.Next() {
		var c EndpointCard
		if err := rows.Scan(&c.ID, &c.AgentID, &c.AgentName, &c.PersonaName, &c.Slug,
			&c.CredentialPrefix, &c.ScopeQueues, &c.ScopeVerbs, &c.State, &c.CreatedAt,
			&c.RevokedAt, &c.LastSeenAt); err != nil {
			return nil, fmt.Errorf("store: scan endpoint card: %w", err)
		}
		out = append(out, c)
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

// DeleteEndpoint permanently removes a REVOKED endpoint the given human owns (via its agent),
// clearing a dead card from the Endpoints view. The delete is doubly constrained in the write:
// state='revoked' so an active grant can never be removed without first being revoked (which is what
// tears down live MCP sessions), and ownership via the agent's owner_human_id so one human can never
// delete another's endpoint. The endpoint's child endpoint_webhooks rows cascade with it. A zero-row
// result — not found, not owned, or still active — reports ErrNotFound.
// Governing: SPEC-0007 REQ "Permanent Deletion of Revoked Endpoints", REQ "Database Operation
// Standards"; SPEC-0013 REQ "Endpoints View and Vend Modal".
func (s *Store) DeleteEndpoint(ctx context.Context, endpointID, ownerHumanID string) error {
	ct, err := s.pool.Exec(ctx, `
		DELETE FROM endpoints
		WHERE id = $1 AND state = 'revoked'
		  AND agent_id IN (SELECT id FROM agents WHERE owner_human_id = $2)`,
		endpointID, ownerHumanID)
	if err != nil {
		return fmt.Errorf("store: delete endpoint: %w", err)
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

// EndpointOwner resolves the human who owns an endpoint (endpoint → agent → owner_human_id).
// The SSE publisher uses it to scope the endpoint_seen live frame to the owning human's streams
// (SPEC-0013: the event stream is "scoped to the human's own data"). Returns ErrNotFound for
// unknown ids so callers can fail closed rather than broadcasting.
func (s *Store) EndpointOwner(ctx context.Context, endpointID string) (string, error) {
	var owner string
	err := s.pool.QueryRow(ctx, `
		SELECT ag.owner_human_id::text
		FROM endpoints e JOIN agents ag ON ag.id = e.agent_id
		WHERE e.id = $1`, endpointID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
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

// KnownQueues returns every distinct queue name the store knows — queues todos have ridden plus
// queues scoped onto vended endpoints — sorted for stable display. The vend modal offers these as
// its scoped-queue toggle chips (SPEC-0013 REQ "Endpoints View and Vend Modal": queues are chosen
// via toggle chips, per the Endpoints design canvas). Queues are a global namespace (SPEC-0003),
// so the enumeration is not per-owner; it reveals only names already visible on the shared Board.
func (s *Store) KnownQueues(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT q FROM (
			SELECT queue AS q FROM todos
			UNION ALL
			SELECT unnest(scope_queues) FROM endpoints
		) AS qs
		ORDER BY q`)
	if err != nil {
		return nil, fmt.Errorf("store: known queues: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			return nil, fmt.Errorf("store: scan known queue: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}
