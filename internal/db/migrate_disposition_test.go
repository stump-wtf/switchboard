package db

import (
	"io/fs"
	"path"
	"strings"
	"testing"
	"testing/fstest"
)

// migrationsBefore is the embedded chain up to, but not including, version: the database as it
// stood before that migration shipped, so a backfill can be tested against realistic rows.
func migrationsBefore(t *testing.T, version string) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= version {
			continue
		}
		b, err := fs.ReadFile(migrationsFS, path.Join("migrations", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

// 0022 backfills events.disposition exactly: an event a drop rule recorded is dropped, and every
// other existing event, including one whose old trace recorded a fault that was then routed past,
// is routed. Governing: SPEC-0026 REQ-1; SPEC-0004 REQ "Embedded, Ordered, Transactional
// Migrations".
func TestMigrate0022BackfillsDisposition(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0022_event_disposition.sql"), "migrations"); err != nil {
		t.Fatalf("apply chain before 0022: %v", err)
	}
	rows := map[string]string{
		"drop":    `{"stage":"rule","action":{"drop":true}}`,
		"queued":  `{"stage":"default","action":{"queue":"inbox"}}`,
		"oldfalt": `{"stage":"default","action":{"queue":"inbox"},"faults":[{"rule_index":0,"cause":"error"}]}`,
		"untrace": ``,
	}
	for ext, trace := range rows {
		var tr any
		if trace != "" {
			tr = trace
		}
		if _, err := pool.Exec(ctx, `INSERT INTO events (source, family, external_id, trust_mode, verified, payload, payload_size, routing_trace)
			VALUES ('github', 'webhook', $1, 'signed', true, '\x7b7d', 2, $2::jsonb)`, ext, tr); err != nil {
			t.Fatalf("seed %s: %v", ext, err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := map[string]string{"drop": "dropped", "queued": "routed", "oldfalt": "routed", "untrace": "routed"}
	for ext, disp := range want {
		var got string
		if err := pool.QueryRow(ctx, `SELECT disposition FROM events WHERE external_id = $1`, ext).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", ext, err)
		}
		if got != disp {
			t.Errorf("event %s disposition = %q, want %q", ext, got, disp)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET disposition = 'bogus' WHERE external_id = 'drop'`); err == nil {
		t.Fatal("the disposition check accepted an unknown value")
	}
}
