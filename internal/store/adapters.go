package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Adapter is a row in the ingestion-adapter registry (ADR-0014): runtime enable/disable plus
// NON-SECRET config for a push (webhook) or pull (queue) adapter. Connection secrets stay in
// environment/config and are never stored here.
type Adapter struct {
	Name      string
	Family    string // webhook|queue
	TrustMode string // signed|token|open|queue
	Enabled   bool
	Config    []byte // non-secret JSON config (jsonb)
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastPollAt is when the poll-loop runner last attempted a consume for this adapter (nil until
	// the first attempt). Governing: SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety".
	LastPollAt *time.Time
	// LastError is the (credential-free) error text of the most recent failed consume attempt; nil
	// while the adapter is healthy.
	LastError *string
}

const adapterCols = `name, family, trust_mode, enabled, config, created_at, updated_at, last_poll_at, last_error`

func scanAdapter(row pgx.Row) (Adapter, error) {
	var a Adapter
	err := row.Scan(&a.Name, &a.Family, &a.TrustMode, &a.Enabled, &a.Config, &a.CreatedAt, &a.UpdatedAt,
		&a.LastPollAt, &a.LastError)
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
	return scanAdapter(row)
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
	return nil
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
