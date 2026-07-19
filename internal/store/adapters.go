package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Adapter is a row in the provider registry (ADR-0014, extended into the SPEC-0017 registry by
// ADR-0020): runtime enable/disable, trust mode, kind, secret PRESENCE, and non-secret config for
// a push (webhook) or pull (queue) provider. The held secret itself is never a field here — like
// Webhook, an Adapter value returned by any list/get surface can never leak secret material; the
// dispatch path reads the plaintext out-of-band through ResolveProvider.
type Adapter struct {
	Name      string
	Family    string // webhook|queue
	Kind      string // concrete implementation: github|stripe|slack|generic|redis|… ('' pre-registry)
	TrustMode string // signed|token|open|queue
	Enabled   bool
	// SecretConfigured reports whether the row HOLDS a (sealed) secret — presence classification
	// only, never the material. Governing: SPEC-0017 REQ "Providers View" (only configured/missing
	// ever surfaces), SPEC-0005 REQ "Provider Enumeration Without Secrets".
	SecretConfigured bool
	Config           []byte // non-secret JSON config (jsonb)
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// LastPollAt is when the poll-loop runner last attempted a consume for this adapter (nil until
	// the first attempt). Governing: SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety".
	LastPollAt *time.Time
	// LastError is the (credential-free) error text of the most recent failed consume attempt; nil
	// while the adapter is healthy.
	LastError *string
}

const adapterCols = `name, family, kind, trust_mode, enabled,
	(secret IS NOT NULL AND secret <> '') AS secret_configured,
	config, created_at, updated_at, last_poll_at, last_error`

func scanAdapter(row pgx.Row) (Adapter, error) {
	var a Adapter
	err := row.Scan(&a.Name, &a.Family, &a.Kind, &a.TrustMode, &a.Enabled, &a.SecretConfigured,
		&a.Config, &a.CreatedAt, &a.UpdatedAt, &a.LastPollAt, &a.LastError)
	return a, err
}

// RegisterAdapter upserts an adapter's registry row, keyed on name. Re-registering (e.g. on every
// startup) refreshes family/trust_mode/config but PRESERVES the enabled flag — enabled is the
// operator's runtime kill switch, not the adapter's to reset. New rows default to enabled.
//
// Governing: ADR-0014 (adapter registry), SPEC-0002 REQ "Adapter Interface and Trust Mode"
// (pull adapters MUST be registered in the adapters table with family='queue').
func (s *Store) RegisterAdapter(ctx context.Context, name, family, trustMode string, config []byte) (Adapter, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO adapters (name, family, trust_mode, config)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE
			SET family = EXCLUDED.family, trust_mode = EXCLUDED.trust_mode,
			    config = EXCLUDED.config, updated_at = now()
		RETURNING `+adapterCols,
		name, family, trustMode, config)
	a, err := scanAdapter(row)
	if err == nil {
		s.invalidateProviderCache()
	}
	return a, err
}

// ProviderSeed is the create-if-absent registry import of ONE env-configured provider (SPEC-0017
// REQ "Environment Config Import"). Secret is the plaintext held secret (shared-secret token or
// HMAC signing secret); SeedProvider seals it through the configured SecretCipher before storage,
// and an empty value persists NULL (open/queue providers hold none).
type ProviderSeed struct {
	Name      string
	Family    string // webhook|queue
	Kind      string // github|stripe|slack|generic|redis|…
	TrustMode string // signed|token|open|queue
	Secret    string // plaintext; sealed via the envelope before it touches the database
	Config    []byte // non-secret JSON config (jsonb), e.g. {"queue":"builds"}
}

// SeedProvider inserts a provider registry row if — and only if — no row with that name exists,
// reporting whether it created one. This is the boot-time env import: ON CONFLICT DO NOTHING makes
// it idempotent AND non-clobbering, so a registry row an operator has since edited (rotated secret,
// flipped enabled, changed queue) always wins over drifted env config — the registry is
// authoritative after the first import. Secrets are stored through the cred envelope (enc:v1:
// ciphertext when a cipher is configured), never inspected or logged here.
//
// Governing: ADR-0020 (env becomes a seed, not the authority), SPEC-0017 REQ "Environment Config
// Import" (create-if-absent, never clobber; scenario "Boot with existing registry").
func (s *Store) SeedProvider(ctx context.Context, seed ProviderSeed) (bool, error) {
	stored, err := s.sealSecret(seed.Secret)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO adapters (name, family, kind, trust_mode, secret, config)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		ON CONFLICT (name) DO NOTHING`,
		seed.Name, seed.Family, seed.Kind, seed.TrustMode, stored, seed.Config)
	if err != nil {
		return false, fmt.Errorf("store: seed provider %s: %w", seed.Name, err)
	}
	created := tag.RowsAffected() > 0
	if created {
		s.invalidateProviderCache()
	}
	return created, nil
}

// ListProviders returns every provider registry row across both families, ordered by family then
// name — the read behind providerStatuses()/list_providers and the Providers view. Rows never
// carry secret material (Adapter has no secret field; SecretConfigured is the only trace).
//
// Governing: ADR-0020, SPEC-0017 REQ "Runtime Provider Registry"; SPEC-0005 REQ "Provider
// Enumeration Without Secrets".
func (s *Store) ListProviders(ctx context.Context) ([]Adapter, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+adapterCols+` FROM adapters ORDER BY family, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Adapter
	for rows.Next() {
		a, err := scanAdapter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// providerCacheTTL bounds how stale a dispatch-path registry read may be. Same-process registry
// writes invalidate the cache immediately, so the TTL only covers out-of-process writes (another
// instance's wizard edit against the shared database) — a few seconds of staleness on a webhook
// route is invisible next to provider retry cadences, while the cache keeps the per-request
// registry lookup off the database for hot providers. Governing: ADR-0020 ("resolve-at-request,
// cache-lightly").
const providerCacheTTL = 3 * time.Second

type providerCacheEntry struct {
	adapter Adapter
	secret  string
	expires time.Time
}

// ResolveProvider is the dispatch-path registry read: the row plus its DECRYPTED held secret, for
// the webhook receivers (internal/ingest) that must trust-check a delivery against it. Results are
// served from a short in-process cache (providerCacheTTL) that every registry write through this
// store invalidates, so wizard/operator changes take effect on the next request — no restart.
// Unknown names return ErrNotFound (never cached: a name probe must not grow memory, and a
// just-created provider must resolve immediately).
//
// Governing: ADR-0020 (registry resolved at request time), SPEC-0017 REQ "Runtime Provider
// Registry" (scenario "Wizard-created provider is live immediately").
func (s *Store) ResolveProvider(ctx context.Context, name string) (Adapter, string, error) {
	s.provMu.Lock()
	if e, ok := s.provCache[name]; ok && time.Now().Before(e.expires) {
		s.provMu.Unlock()
		return e.adapter, e.secret, nil
	}
	s.provMu.Unlock()

	var a Adapter
	var stored *string
	err := s.pool.QueryRow(ctx,
		`SELECT `+adapterCols+`, secret FROM adapters WHERE name = $1`, name,
	).Scan(&a.Name, &a.Family, &a.Kind, &a.TrustMode, &a.Enabled, &a.SecretConfigured,
		&a.Config, &a.CreatedAt, &a.UpdatedAt, &a.LastPollAt, &a.LastError, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return Adapter{}, "", ErrNotFound
	}
	if err != nil {
		return Adapter{}, "", fmt.Errorf("store: resolve provider %s: %w", name, err)
	}
	var sealed string
	if stored != nil {
		sealed = *stored
	}
	// Decrypt transparently on the dispatch path (legacy plaintext passes through unchanged), the
	// same envelope discipline as the self-managed webhook secrets. Governing: SPEC-0017 REQ
	// "Runtime Provider Registry" (secrets stored encrypted via the existing envelope).
	secret, err := s.openSecret(sealed)
	if err != nil {
		return Adapter{}, "", err
	}

	s.provMu.Lock()
	if s.provCache == nil {
		s.provCache = map[string]providerCacheEntry{}
	}
	s.provCache[name] = providerCacheEntry{adapter: a, secret: secret, expires: time.Now().Add(providerCacheTTL)}
	s.provMu.Unlock()
	return a, secret, nil
}

// invalidateProviderCache drops every cached dispatch resolution. Every registry WRITE through this
// store calls it (seed, register, enable/disable — and future wizard edits MUST too), so a change
// is visible to the very next request in this process. Health stamps (RecordAdapterPoll) do not
// invalidate: dispatch never reads the health columns, and a per-poll flush would defeat the cache.
func (s *Store) invalidateProviderCache() {
	s.provMu.Lock()
	s.provCache = nil
	s.provMu.Unlock()
}

// ListAdaptersByFamily returns every registry row with the given family (e.g. "queue"), ordered by
// name. Server startup reads the queue family through this to know which pull adapters to attach to
// the poll-loop runner; disabled rows are included — the runner itself honors the enabled flag at
// runtime, so a row disabled at startup can be re-enabled without a restart.
//
// Governing: ADR-0014 (adapter registry), SPEC-0002 REQ "Adapter Interface and Trust Mode".
func (s *Store) ListAdaptersByFamily(ctx context.Context, family string) ([]Adapter, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+adapterCols+` FROM adapters WHERE family = $1 ORDER BY name`, family)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Adapter
	for rows.Next() {
		a, err := scanAdapter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAdapter returns the registry row for name, or ErrNotFound.
func (s *Store) GetAdapter(ctx context.Context, name string) (Adapter, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+adapterCols+` FROM adapters WHERE name = $1`, name)
	a, err := scanAdapter(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Adapter{}, ErrNotFound
	}
	return a, err
}

// AdapterEnabled reports the runtime enabled flag for name, or ErrNotFound for an unregistered
// adapter. A pull adapter's poll loop MUST honor this flag: disabled means do not consume.
//
// Governing: SPEC-0002 REQ "Adapter Interface and Trust Mode" (honor the runtime enabled flag).
func (s *Store) AdapterEnabled(ctx context.Context, name string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT enabled FROM adapters WHERE name = $1`, name).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return enabled, err
}

// SetAdapterEnabled flips the runtime enabled flag for name, returning ErrNotFound for an
// unregistered adapter.
func (s *Store) SetAdapterEnabled(ctx context.Context, name string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE adapters SET enabled = $2, updated_at = now() WHERE name = $1`, name, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// The enabled flag gates dispatch (webhook receivers reject, poll loops park), so the flip must
	// reach the next request immediately. Governing: ADR-0020 (registry writes invalidate the cache).
	s.invalidateProviderCache()
	return nil
}

// RotateProviderSecret replaces the held secret on a provider's registry row, sealing the new
// plaintext through the configured SecretCipher exactly like SeedProvider — the database only ever
// sees the envelope. Returns ErrNotFound for an unregistered provider. The old secret stops working
// on the very next request: the write invalidates the dispatch cache, so no restart (and no grace
// window) is involved.
//
// Governing: ADR-0020, SPEC-0017 REQ "Provider Lifecycle" (rotate the secret for token/signed
// webhook kinds); the caller enforces WHICH kinds are rotatable — this is the storage primitive.
func (s *Store) RotateProviderSecret(ctx context.Context, name, secret string) error {
	stored, err := s.sealSecret(secret)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE adapters SET secret = NULLIF($2, ''), updated_at = now() WHERE name = $1`,
		name, stored)
	if err != nil {
		return fmt.Errorf("store: rotate provider secret %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateProviderCache()
	return nil
}

// RemoveProvider deletes a provider's registry row, returning ErrNotFound for an unknown name. The
// delete touches ONLY the adapters table: events and todos reference providers by source NAME with
// no foreign key into the registry, so everything the provider ever ingested remains queryable —
// removal kills the line, never the history. The write invalidates the dispatch cache, so the
// provider's ingestion URL is dead (404) on the next request.
//
// Governing: SPEC-0017 REQ "Provider Lifecycle" (removal SHALL NOT delete previously ingested
// events or todos; scenario "Disable stops the line" — history stays queryable).
func (s *Store) RemoveProvider(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM adapters WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("store: remove provider %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateProviderCache()
	return nil
}

// ProviderHealth is one provider's ingest-side health for the Providers view: the trailing
// one-minute in-rate and the newest accepted delivery's timestamp. Derived entirely from the
// events table (accepted deliveries only — rejected payloads never persist, SPEC-0001), keyed by
// event source, which is the provider's registry name on every ingest path.
type ProviderHealth struct {
	EventsPerMin int        // accepted deliveries in the trailing minute — the per-line in-rate
	LastSeenAt   *time.Time // newest accepted delivery; nil = never seen
}

// ProviderHealthBySource aggregates per-provider health across the events table in one query:
// source → {in-rate, last-seen}. Providers that never ingested simply have no entry — the view
// renders those as idle/never-seen rather than erroring.
//
// Governing: SPEC-0017 REQ "Providers View" (per-provider in-rate and last-seen).
func (s *Store) ProviderHealthBySource(ctx context.Context) (map[string]ProviderHealth, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source,
		       count(*) FILTER (WHERE received_at > now() - interval '1 minute'),
		       max(received_at)
		FROM events GROUP BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProviderHealth{}
	for rows.Next() {
		var source string
		var h ProviderHealth
		if err := rows.Scan(&source, &h.EventsPerMin, &h.LastSeenAt); err != nil {
			return nil, err
		}
		out[source] = h
	}
	return out, rows.Err()
}

// RecordAdapterPoll stamps the outcome of one poll-loop consume attempt on the adapter's registry
// row: last_poll_at is set to now, and last_error carries the attempt's (credential-free) error
// text — or clears to NULL on a healthy attempt. Returns ErrNotFound for an unregistered adapter.
// Callers MUST NOT pass error text containing broker credentials (the redis transports already
// redact the DSN).
//
// Governing: SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety" (health/last-poll tracking),
// REQ "Error Handling Standards" (never log broker credentials).
func (s *Store) RecordAdapterPoll(ctx context.Context, name string, pollErr string) error {
	var lastErr *string
	if pollErr != "" {
		lastErr = &pollErr
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE adapters SET last_poll_at = now(), last_error = $2 WHERE name = $1`, name, lastErr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
