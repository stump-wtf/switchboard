package db

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// migrationsBefore returns the embedded migrations whose filename sorts strictly before cut, so a
// test can stand a database up at the schema a live instance had just before a given migration,
// seed rows in that shape, and then watch the migration run against them.
func migrationsBefore(t *testing.T, cut string) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		if e.IsDir() || e.Name() >= cut {
			continue
		}
		b, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}
