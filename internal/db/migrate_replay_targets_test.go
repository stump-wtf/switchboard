package db

import "testing"

// TestOwnedReplayTargetsMigration runs 0026_owned_replay_targets against an instance that had both
// retired settings configured, the F9 shape: the rows are deleted, unrelated settings survive, and
// an existing endpoint gains an empty owned-target list rather than inheriting the instance target.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets" (the migration deletes their rows).
func TestOwnedReplayTargetsMigration(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0026"), "migrations"); err != nil {
		t.Fatalf("migrate to 0025: %v", err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES
		('replay_default_target', 'http://10.0.0.5:8080/hook'),
		('replay_allowed_targets', '10.0.0.5:8080, internal.example')`); err != nil {
		t.Fatalf("seed retired settings: %v", err)
	}
	var human, agent, ep string
	if err := pool.QueryRow(ctx, `INSERT INTO humans (oidc_subject) VALUES ('replay-mig') RETURNING id::text`).Scan(&human); err != nil {
		t.Fatalf("human: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agents (owner_human_id, name) VALUES ($1, 'a') RETURNING id::text`, human).Scan(&agent); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix)
		VALUES ($1, 'rt-1', 'h', 'p') RETURNING id::text`, agent).Scan(&ep); err != nil {
		t.Fatalf("endpoint: %v", err)
	}

	if err := applyMigrations(ctx, pool, migrationsFS, "migrations"); err != nil {
		t.Fatalf("apply 0026 and later: %v", err)
	}

	var retired int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM settings
		WHERE key IN ('replay_default_target', 'replay_allowed_targets')`).Scan(&retired); err != nil {
		t.Fatalf("count retired settings: %v", err)
	}
	if retired != 0 {
		t.Fatalf("%d retired replay setting row(s) survived the migration", retired)
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM settings WHERE key = 'retention_max_age_days'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("unrelated setting rows = %d (%v), want the retention row kept", kept, err)
	}
	var targets []string
	if err := pool.QueryRow(ctx, `SELECT replay_targets FROM endpoints WHERE id = $1`, ep).Scan(&targets); err != nil {
		t.Fatalf("read replay_targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("existing endpoint owns %v after the migration, want none", targets)
	}
}
