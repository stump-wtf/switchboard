package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint violation (SQLSTATE 23505),
// e.g. a duplicate (owner, slug). Lets the store surface a distinguishable ErrConflict.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ErrScopeExceeded is returned when a persona's verb_subset or queues are not a subset of the
// backing agent's vended grant. The offending verb/queue is wrapped for context; callers match with
// errors.Is(err, ErrScopeExceeded). A persona whose scope exceeds the grant is never persisted.
// Governing: ADR-0009, SPEC-0009 REQ "Persona Record"
// (scenario "Persona scope may not exceed the agent's vended grant").
var ErrScopeExceeded = errors.New("store: persona scope exceeds agent vended grant")

// Persona is a named, scoped face of one registered agent: base agent + human-authored system
// prompt + a subset of the agent's vended verbs/queues (its capability slice). The verb_subset is
// the unit of capability scoping and is validated as a subset of the agent's vended grant on
// create/update. Governing: ADR-0009, SPEC-0009 REQ "Persona Record".
type Persona struct {
	ID           string
	OwnerHumanID string
	AgentID      string
	Name         string
	Slug         string
	SystemPrompt string
	VerbSubset   []string
	Queues       []string
	Description  string
	Discoverable bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreatePersonaParams carries the authored inputs for a new persona. Slug is optional: when empty it
// is derived from Name. VerbSubset/Queues MUST each be a subset of the agent's vended grant.
type CreatePersonaParams struct {
	OwnerHumanID string
	AgentID      string
	Name         string
	Slug         string
	SystemPrompt string
	VerbSubset   []string
	Queues       []string
	Description  string
}

// UpdatePersonaParams carries the mutable fields of a persona. VerbSubset/Queues are re-validated
// against the agent's current vended grant so "advertised capability" can never exceed "actual
// capability". The backing AgentID is fixed at creation and cannot be changed.
type UpdatePersonaParams struct {
	ID           string
	OwnerHumanID string
	Name         string
	SystemPrompt string
	VerbSubset   []string
	Queues       []string
	Description  string
}

// slugifyPersona derives a readable, non-secret URL segment from a persona name: lowercased, runs of
// non-alphanumeric characters collapsed to a single '-', leading/trailing dashes trimmed. Unlike an
// endpoint slug (MintSlug) it carries no random suffix — persona slugs are unique per owner, so a
// human gets stable, human-meaningful slugs. Falls back to "persona" when the name has no usable
// characters. Governing: SPEC-0009 REQ "Well-Known Card Endpoint" (per-persona base path).
func slugifyPersona(name string) string {
	var b strings.Builder
	prevDash := true // suppress a leading dash
	for _, r := range strings.ToLower(name) {
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
		return "persona"
	}
	return base
}

// subsetViolation reports the first element of subset that is absent from superset. Empty subsets
// (a persona that vends nothing) are trivially valid. Comparison is exact-string, matching how verbs
// and queues are stored and authorized elsewhere in the store.
func subsetViolation(subset, superset []string) (string, bool) {
	set := make(map[string]struct{}, len(superset))
	for _, s := range superset {
		set[s] = struct{}{}
	}
	for _, v := range subset {
		if _, ok := set[v]; !ok {
			return v, true
		}
	}
	return "", false
}

// agentVendedGrant returns the union of verbs and queues vended to an agent across its active
// endpoints — the agent's total vended grant a persona's scope must fall within — but only if the
// agent is owned by ownerHumanID. A cross-owner or missing agent yields ErrNotFound, so ownership
// failures are byte-for-byte indistinguishable from a missing id and never leak existence.
// Governing: ADR-0008 (vended endpoints), ADR-0009, SPEC-0009 REQ "Persona Record".
func (s *Store) agentVendedGrant(ctx context.Context, agentID, ownerHumanID string) (verbs, queues []string, err error) {
	// The ownership predicate lives in the query so an unowned agent returns no row (ErrNotFound),
	// never an empty-grant false positive. The grant is the DISTINCT union over active endpoints.
	err = s.pool.QueryRow(ctx, `
		SELECT
			ARRAY(SELECT DISTINCT v FROM endpoints e, unnest(e.scope_verbs) AS v
			      WHERE e.agent_id = a.id AND e.state = 'active'),
			ARRAY(SELECT DISTINCT q FROM endpoints e, unnest(e.scope_queues) AS q
			      WHERE e.agent_id = a.id AND e.state = 'active')
		FROM agents a
		WHERE a.id = $1 AND a.owner_human_id = $2`,
		agentID, ownerHumanID,
	).Scan(&verbs, &queues)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: agent vended grant: %w", err)
	}
	return verbs, queues, nil
}

// validatePersonaScope checks that verbSubset and queues are each a subset of the agent's vended
// grant, returning ErrScopeExceeded (wrapped with the offending value) on the first violation.
func validatePersonaScope(verbSubset, queues, grantVerbs, grantQueues []string) error {
	if bad, over := subsetViolation(verbSubset, grantVerbs); over {
		return fmt.Errorf("%w: verb %q not vended to agent", ErrScopeExceeded, bad)
	}
	if bad, over := subsetViolation(queues, grantQueues); over {
		return fmt.Errorf("%w: queue %q not vended to agent", ErrScopeExceeded, bad)
	}
	return nil
}

// CreatePersona records a new scoped face of an agent after validating that its verb_subset and
// queues fall within the agent's vended grant. Ownership is enforced (the agent must belong to
// ownerHumanID) and the (owner, slug) pair must be unique. A persona whose scope exceeds the grant
// is rejected with ErrScopeExceeded and never persisted.
// Governing: ADR-0009, SPEC-0009 REQ "Persona Record".
func (s *Store) CreatePersona(ctx context.Context, p CreatePersonaParams) (Persona, error) {
	grantVerbs, grantQueues, err := s.agentVendedGrant(ctx, p.AgentID, p.OwnerHumanID)
	if err != nil {
		return Persona{}, err
	}
	if err := validatePersonaScope(p.VerbSubset, p.Queues, grantVerbs, grantQueues); err != nil {
		return Persona{}, err
	}

	slug := p.Slug
	if slug == "" {
		slug = slugifyPersona(p.Name)
	}

	var out Persona
	err = s.pool.QueryRow(ctx, `
		INSERT INTO personas (owner_human_id, agent_id, name, slug, system_prompt, verb_subset, queues, description)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		RETURNING id::text, owner_human_id::text, agent_id::text, name, slug, system_prompt,
		          verb_subset, queues, COALESCE(description, ''), discoverable, created_at, updated_at`,
		p.OwnerHumanID, p.AgentID, p.Name, slug, p.SystemPrompt, p.VerbSubset, p.Queues, p.Description,
	).Scan(&out.ID, &out.OwnerHumanID, &out.AgentID, &out.Name, &out.Slug, &out.SystemPrompt,
		&out.VerbSubset, &out.Queues, &out.Description, &out.Discoverable, &out.CreatedAt, &out.UpdatedAt)
	if isUniqueViolation(err) {
		return Persona{}, fmt.Errorf("%w: persona slug %q already exists for this owner", ErrConflict, slug)
	}
	if err != nil {
		return Persona{}, fmt.Errorf("store: create persona: %w", err)
	}
	return out, nil
}

// personaCols is the shared projection for reading a persona row.
const personaCols = `id::text, owner_human_id::text, agent_id::text, name, slug, system_prompt,
	verb_subset, queues, COALESCE(description, ''), discoverable, created_at, updated_at`

// scanPersona reads a persona row in personaCols order.
func scanPersona(row pgx.Row) (Persona, error) {
	var p Persona
	err := row.Scan(&p.ID, &p.OwnerHumanID, &p.AgentID, &p.Name, &p.Slug, &p.SystemPrompt,
		&p.VerbSubset, &p.Queues, &p.Description, &p.Discoverable, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// GetPersona returns a persona only if owned by ownerHumanID. A cross-owner or missing id is
// ErrNotFound — indistinguishable, so ownership failures do not leak existence.
// Governing: ADR-0009, SPEC-0009 (owner-controlled personas).
func (s *Store) GetPersona(ctx context.Context, id, ownerHumanID string) (Persona, error) {
	p, err := scanPersona(s.pool.QueryRow(ctx,
		`SELECT `+personaCols+` FROM personas WHERE id = $1 AND owner_human_id = $2`, id, ownerHumanID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Persona{}, ErrNotFound
	}
	if err != nil {
		return Persona{}, fmt.Errorf("store: get persona: %w", err)
	}
	return p, nil
}

// ListPersonas returns a human's personas, newest first. Tenant-scoped: each human sees only their
// own personas. Governing: ADR-0009, SPEC-0009 (owner-controlled personas).
func (s *Store) ListPersonas(ctx context.Context, ownerHumanID string) ([]Persona, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+personaCols+` FROM personas WHERE owner_human_id = $1 ORDER BY created_at DESC`, ownerHumanID)
	if err != nil {
		return nil, fmt.Errorf("store: list personas: %w", err)
	}
	defer rows.Close()
	var out []Persona
	for rows.Next() {
		p, err := scanPersona(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan persona: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePersona edits a persona's authored fields, re-validating verb_subset and queues against the
// backing agent's current vended grant so a persona can never advertise capability it does not hold.
// Owner-scoped: a cross-owner or missing id is ErrNotFound; an out-of-grant scope is ErrScopeExceeded
// and nothing is written. Governing: ADR-0009, SPEC-0009 REQ "Persona Record".
func (s *Store) UpdatePersona(ctx context.Context, p UpdatePersonaParams) (Persona, error) {
	// Resolve the persona's backing agent within owner scope first: this both enforces ownership and
	// pins the agent whose grant bounds the new scope. The backing agent is immutable, so we validate
	// against it rather than trusting any caller-supplied agent id.
	var agentID string
	err := s.pool.QueryRow(ctx,
		`SELECT agent_id::text FROM personas WHERE id = $1 AND owner_human_id = $2`, p.ID, p.OwnerHumanID,
	).Scan(&agentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Persona{}, ErrNotFound
	}
	if err != nil {
		return Persona{}, fmt.Errorf("store: update persona lookup: %w", err)
	}

	grantVerbs, grantQueues, err := s.agentVendedGrant(ctx, agentID, p.OwnerHumanID)
	if err != nil {
		return Persona{}, err
	}
	if err := validatePersonaScope(p.VerbSubset, p.Queues, grantVerbs, grantQueues); err != nil {
		return Persona{}, err
	}

	out, err := scanPersona(s.pool.QueryRow(ctx, `
		UPDATE personas
		SET name = $3, system_prompt = $4, verb_subset = $5, queues = $6,
		    description = NULLIF($7, ''), updated_at = now()
		WHERE id = $1 AND owner_human_id = $2
		RETURNING `+personaCols,
		p.ID, p.OwnerHumanID, p.Name, p.SystemPrompt, p.VerbSubset, p.Queues, p.Description))
	if errors.Is(err, pgx.ErrNoRows) {
		return Persona{}, ErrNotFound
	}
	if err != nil {
		return Persona{}, fmt.Errorf("store: update persona: %w", err)
	}
	return out, nil
}

// SetPersonaDiscoverable sets a persona's discoverable (owner-controlled discoverability) flag.
// Owner-scoped: a cross-owner or missing id is ErrNotFound and nothing changes. Discoverability is an
// explicit owner choice — a persona is never listed in a directory unless its owner marks it
// discoverable. Governing: SPEC-0009 REQ "Discoverability Is Owner-Controlled".
func (s *Store) SetPersonaDiscoverable(ctx context.Context, id, ownerHumanID string, discoverable bool) (Persona, error) {
	out, err := scanPersona(s.pool.QueryRow(ctx, `
		UPDATE personas SET discoverable = $3, updated_at = now()
		WHERE id = $1 AND owner_human_id = $2
		RETURNING `+personaCols, id, ownerHumanID, discoverable))
	if errors.Is(err, pgx.ErrNoRows) {
		return Persona{}, ErrNotFound
	}
	if err != nil {
		return Persona{}, fmt.Errorf("store: set persona discoverable: %w", err)
	}
	return out, nil
}

// DiscoverablePersona bundles a discoverable persona with the minimal owner provenance its Agent Card
// needs — the owner's display name only. It deliberately excludes owner PII (email, OIDC subject) so
// the public card projection can never leak it. Governing: SPEC-0009 REQ "Agent Card Mapping".
type DiscoverablePersona struct {
	Persona
	OwnerDisplayName string
}

// GetDiscoverablePersona resolves a persona by id for the PUBLIC Agent Card endpoint — no owner
// scope, because the card is served to unauthenticated A2A peers. It returns a row ONLY when the
// persona exists AND its owner has marked it discoverable; an unknown id, a malformed id, or a
// non-discoverable persona all yield ErrNotFound, byte-for-byte indistinguishable, so the endpoint
// never leaks the existence of a persona the owner has not published.
// Governing: SPEC-0009 REQ "Discoverability Is Owner-Controlled", REQ "Well-Known Card Endpoint".
func (s *Store) GetDiscoverablePersona(ctx context.Context, id string) (DiscoverablePersona, error) {
	// A path segment that is not a valid UUID cannot match any row; short-circuit to ErrNotFound so
	// pgx never surfaces a 22P02 (invalid_text_representation) as a 500 for arbitrary public input.
	if _, err := uuid.Parse(id); err != nil {
		return DiscoverablePersona{}, ErrNotFound
	}
	var out DiscoverablePersona
	err := s.pool.QueryRow(ctx, `
		SELECT p.id::text, p.owner_human_id::text, p.agent_id::text, p.name, p.slug, p.system_prompt,
		       p.verb_subset, p.queues, COALESCE(p.description, ''), p.discoverable,
		       p.created_at, p.updated_at, COALESCE(h.display_name, '')
		FROM personas p JOIN humans h ON h.id = p.owner_human_id
		WHERE p.id = $1 AND p.discoverable = true`, id,
	).Scan(&out.ID, &out.OwnerHumanID, &out.AgentID, &out.Name, &out.Slug, &out.SystemPrompt,
		&out.VerbSubset, &out.Queues, &out.Description, &out.Discoverable,
		&out.CreatedAt, &out.UpdatedAt, &out.OwnerDisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscoverablePersona{}, ErrNotFound
	}
	if err != nil {
		return DiscoverablePersona{}, fmt.Errorf("store: get discoverable persona: %w", err)
	}
	return out, nil
}

// DeletePersona removes a persona, but only if it belongs to ownerHumanID. A cross-owner or missing
// id is ErrNotFound and mutates nothing. Governing: ADR-0009, SPEC-0009 (owner-controlled personas).
func (s *Store) DeletePersona(ctx context.Context, id, ownerHumanID string) error {
	ct, err := s.pool.Exec(ctx,
		`DELETE FROM personas WHERE id = $1 AND owner_human_id = $2`, id, ownerHumanID)
	if err != nil {
		return fmt.Errorf("store: delete persona: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
