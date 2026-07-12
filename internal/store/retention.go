package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Governing: SPEC-0004 REQ "Hybrid Retention and Bounded Growth", ADR-0002.
//
// Retention bounds both time and size: a single transaction prunes over-age rows and then trims
// whatever still exceeds the row cap. Either bound alone leaves the other unbounded (design.md
// "Hybrid age + row-cap retention"). Only the growth-prone, append-mostly tables are pruned —
// events and terminal todos (done, or failed dead-letters with no retry window). Pending and
// claimed todos are the live queue, and a parked retry (failed with an open next_retry_at window,
// SPEC-0003 scheduled backoff) is live work awaiting re-queue — none of these are ever touched by
// retention, so pruning can never lose in-flight or scheduled work.

// PruneResult reports how many rows retention removed, per surface.
type PruneResult struct {
	EventsAged   int64 // events deleted for exceeding max age
	EventsCapped int64 // events deleted for exceeding the row cap
	TodosAged    int64 // terminal todos deleted for exceeding max age
	TodosCapped  int64 // terminal todos deleted for exceeding the row cap
}

// Total is the number of rows removed across all surfaces.
func (r PruneResult) Total() int64 {
	return r.EventsAged + r.EventsCapped + r.TodosAged + r.TodosCapped
}

// Prune enforces the hybrid age + row-cap retention policy in a single transaction, seeded from the
// settings table (retention_max_age_days, retention_max_rows). It deletes over-age events and
// terminal todos, then trims events and terminal todos still beyond the row cap (keeping the newest).
// Pending and claimed todos are left intact. Running Prune when nothing qualifies is a no-op.
func (s *Store) Prune(ctx context.Context) (PruneResult, error) {
	var res PruneResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Read the policy once, inside the transaction, so a concurrent settings change can't split the
	// age/cap decision across two different values. Defaults mirror 0001_init's seeded settings.
	var maxAgeDays, maxRows int
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE((SELECT value FROM settings WHERE key = 'retention_max_age_days'), '30')::int,
			COALESCE((SELECT value FROM settings WHERE key = 'retention_max_rows'), '500000')::int`,
	).Scan(&maxAgeDays, &maxRows); err != nil {
		return res, err
	}

	// Age: drop events older than the age bound.
	if res.EventsAged, err = exec(ctx, tx, `
		DELETE FROM events
		WHERE received_at < now() - make_interval(days => $1)`, maxAgeDays); err != nil {
		return res, err
	}
	// Age: drop terminal todos whose last transition is older than the age bound. Pending/claimed
	// rows are excluded by the state filter, and a parked retry (failed with an open next_retry_at
	// window, SPEC-0003 scheduled backoff) is LIVE work awaiting re-queue — not a terminal record —
	// so it is excluded too. Live work is untouched regardless of age.
	if res.TodosAged, err = exec(ctx, tx, `
		DELETE FROM todos
		WHERE state IN ('done', 'failed')
		  AND next_retry_at IS NULL
		  AND updated_at < now() - make_interval(days => $1)`, maxAgeDays); err != nil {
		return res, err
	}

	// Cap: after aging, delete the oldest events beyond retention_max_rows (keep the newest maxRows).
	if res.EventsCapped, err = exec(ctx, tx, `
		DELETE FROM events
		WHERE id IN (
			SELECT id FROM events ORDER BY received_at DESC, id DESC OFFSET $1
		)`, maxRows); err != nil {
		return res, err
	}
	// Cap: trim terminal todos beyond the cap. Only done/failed rows with no open retry window are
	// eligible, so the pending/claimed queue and parked retries never count against — nor are
	// trimmed by — the cap.
	if res.TodosCapped, err = exec(ctx, tx, `
		DELETE FROM todos
		WHERE id IN (
			SELECT id FROM todos
			WHERE state IN ('done', 'failed') AND next_retry_at IS NULL
			ORDER BY updated_at DESC, id DESC OFFSET $1
		)`, maxRows); err != nil {
		return res, err
	}

	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// exec runs a parameterized statement on tx and returns the affected row count.
func exec(ctx context.Context, tx pgx.Tx, sql string, args ...any) (int64, error) {
	ct, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}
