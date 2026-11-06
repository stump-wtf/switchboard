package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTodoLineageMigration runs 0037_todo_lineage against a database that already holds a todo
// predating lineage, and proves the constraints actually bite: a link whose target does not match
// its kind is refused by its named CHECK, so is an unknown provenance, a repeated statement, and
// a tag the grammar does not allow; a spawned link aimed at a todo that does not exist is
// refused by the foreign key; deleting a todo takes its tags and both directions of its links
// with it. Applying the chain a second time must change nothing.
//
// Governing: SPEC-0037 REQ-4/REQ-18, ADR-0044

func seedLineageEndpoint(t *testing.T, pool *pgxpool.Pool, ctx context.Context, slug string) string {
	t.Helper()
	var human, agent string
	if err := pool.QueryRow(ctx, `INSERT INTO humans (oidc_subject) VALUES ($1) RETURNING id::text`, "lineage-"+slug).Scan(&human); err != nil {
		t.Fatalf("human: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agents (owner_human_id, name) VALUES ($1, $2) RETURNING id::text`, human, "lineage-agent-"+slug).Scan(&agent); err != nil {
		t.Fatalf("agent: %v", err)
	}
	var ep string
	if err := pool.QueryRow(ctx, `INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix)
		VALUES ($1, $2, 'h', 'p') RETURNING id::text`, agent, slug).Scan(&ep); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	return ep
}

func seedLineageTodo(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id, ep string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO todos (id, endpoint_id, queue, title) VALUES ($1, $2, 'lineage', 't')`, id, ep); err != nil {
		t.Fatalf("seed todo %s: %v", id, err)
	}
}

// expectRejected fails when the statement was accepted at all, or was refused by anything other
// than a check constraint (23514) under the constraint named want.
func expectRejected(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the row was accepted, want it rejected", label)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("%s: want check violation 23514, got %v", label, err)
	}
	if want != "" && !strings.Contains(pgErr.ConstraintName, want) {
		t.Fatalf("%s: want constraint %q, got %q (%v)", label, want, pgErr.ConstraintName, err)
	}
}

// execErr keeps only the error from a pgx Exec, so a statement expected to fail can be handed
// straight to expectRejected.
func execErr(_ pgconn.CommandTag, err error) error { return err }

// expectForeignRejected fails when the statement was accepted at all, or was refused by anything
// other than the foreign key on a spawned link's target todo (23503,
// todo_links_to_todo_id_fkey).
func expectForeignRejected(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the row was accepted, want it rejected", label)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("%s: want foreign key violation 23503, got %v", label, err)
	}
	if !strings.Contains(pgErr.ConstraintName, "todo_links_to_todo_id_fkey") {
		t.Fatalf("%s: want constraint todo_links_to_todo_id_fkey, got %q (%v)", label, pgErr.ConstraintName, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

func TestTodoLineageMigration(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0037"), "migrations"); err != nil {
		t.Fatalf("migrate to before 0037: %v", err)
	}
	ep := seedLineageEndpoint(t, pool, ctx, "tl")
	seedLineageTodo(t, pool, ctx, "tl-old", ep)

	if err := applyMigrations(ctx, pool, migrationsFS, "migrations"); err != nil {
		t.Fatalf("apply 0037 and later: %v", err)
	}
	if !recorded(t, pool, ctx, "0037_todo_lineage.sql") {
		t.Fatal("0037_todo_lineage.sql did not apply")
	}
	var oldKey *string
	if err := pool.QueryRow(ctx, `SELECT subject_key FROM todos WHERE id = 'tl-old'`).Scan(&oldKey); err != nil {
		t.Fatalf("read subject_key of the pre-lineage todo: %v", err)
	}
	if oldKey != nil {
		t.Fatalf("pre-lineage todo has subject_key %q, want NULL", *oldKey)
	}
	for _, idx := range []string{"idx_todos_subject_key", "uq_todo_links_stmt", "idx_todo_links_to_key", "idx_todo_links_to_todo", "idx_todo_tags_tag"} {
		if n := count(t, pool, ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx); n != 1 {
			t.Fatalf("index %s present %d times, want exactly 1", idx, n)
		}
	}

	seedLineageTodo(t, pool, ctx, "tl-child", ep)
	link := func(cols, vals string) error {
		_, err := pool.Exec(ctx, `INSERT INTO todo_links (from_todo_id, `+cols+`) VALUES `+vals)
		return err
	}

	// A produced link points at a thread key, never a todo, and vice versa: each CHECK is a
	// named biconditional. The produced CHECK is defined first, so Postgres names it for every
	// mix-up that puts a key under the wrong kind; the one case that leaves it satisfied — a
	// produced row carrying a todo id — is caught by the spawned CHECK instead.
	expectRejected(t, "produced without a subject key",
		link("kind, to_todo_id, provenance, actor_ref", "('tl-old', 'produced', 'tl-child', 'declared', 'endpoint:tl')"), "todo_links_produced_target_check")
	expectRejected(t, "produced with no target at all",
		link("kind, provenance, actor_ref", "('tl-old', 'produced', 'declared', 'endpoint:tl')"), "todo_links_produced_target_check")
	expectRejected(t, "spawned with both targets",
		link("kind, to_subject_key, to_todo_id, provenance, actor_ref", "('tl-old', 'spawned', 'thread:dup', 'tl-child', 'declared', 'endpoint:tl')"), "todo_links_produced_target_check")
	expectRejected(t, "spawned with only a subject key",
		link("kind, to_subject_key, provenance, actor_ref", "('tl-old', 'spawned', 'thread:dup', 'declared', 'endpoint:tl')"), "todo_links_produced_target_check")
	expectRejected(t, "produced with both targets",
		link("kind, to_subject_key, to_todo_id, provenance, actor_ref", "('tl-old', 'produced', 'thread:dup', 'tl-child', 'declared', 'endpoint:tl')"), "todo_links_spawned_target_check")
	// A spawned edge names a real todo, so a link aimed at a todo that was never created is
	// refused by the foreign key rather than left dangling in the graph.
	expectForeignRejected(t, "spawned at a todo that does not exist",
		link("kind, to_todo_id, provenance, actor_ref", "('tl-old', 'spawned', 'tl-ghost', 'declared', 'endpoint:tl')"))
	expectRejected(t, "unknown provenance",
		link("kind, to_subject_key, provenance, actor_ref", "('tl-old', 'produced', 'thread:dup', 'inferred', 'endpoint:tl')"), "todo_links_provenance_check")

	if err := link("kind, to_subject_key, provenance, actor_ref", "('tl-old', 'produced', 'thread:deploy', 'declared', 'endpoint:tl')"); err != nil {
		t.Fatalf("a well-formed produced link was refused: %v", err)
	}
	// The same statement from the same todo is idempotent by key, so a retried call cannot draw
	// the edge twice (SPEC-0037 REQ-4).
	dup := link("kind, to_subject_key, provenance, actor_ref", "('tl-old', 'produced', 'thread:deploy', 'declared', 'endpoint:tl')")
	var pgErr *pgconn.PgError
	if dup == nil || !errors.As(dup, &pgErr) || pgErr.Code != "23505" || !strings.Contains(pgErr.ConstraintName, "uq_todo_links_stmt") {
		t.Fatalf("the duplicate statement was not refused by uq_todo_links_stmt: %v", dup)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM todo_links WHERE from_todo_id = 'tl-old' AND kind = 'produced'`); n != 1 {
		t.Fatalf("%d produced links after the duplicate statement, want 1", n)
	}
	// Same key, different provenance is a different statement, and draws its own edge.
	if err := link("kind, to_subject_key, provenance, actor_ref", "('tl-old', 'produced', 'thread:deploy', 'tagged', 'endpoint:tl')"); err != nil {
		t.Fatalf("the same key under another provenance was refused: %v", err)
	}
	if err := link("kind, to_todo_id, provenance, actor_ref", "('tl-old', 'spawned', 'tl-child', 'declared', 'endpoint:tl')"); err != nil {
		t.Fatalf("a well-formed spawned link was refused: %v", err)
	}
	if err := link("kind, to_todo_id, provenance, actor_ref", "('tl-child', 'spawned', 'tl-old', 'declared', 'endpoint:tl')"); err != nil {
		t.Fatalf("the reverse spawned link was refused: %v", err)
	}
	if err := link("kind, to_subject_key, provenance, actor_ref", "('tl-child', 'produced', 'thread:deploy', 'declared', 'endpoint:tl')"); err != nil {
		t.Fatalf("the child's produced link was refused: %v", err)
	}

	tag := func(literal string) error {
		_, err := pool.Exec(ctx, `INSERT INTO todo_tags (todo_id, tag, origin, added_by) VALUES ('tl-old', `+literal+`, 'human', 'human:tl')`)
		return err
	}
	expectRejected(t, "an uppercase tag", tag("'Lane:S'"), "todo_tags_tag_check")
	expectRejected(t, "a 65-byte tag", tag("repeat('t', 65)"), "todo_tags_tag_check")
	expectRejected(t, "an unknown tag origin",
		execErr(pool.Exec(ctx, `INSERT INTO todo_tags (todo_id, tag, origin, added_by) VALUES ('tl-old', 'lane:m', 'vibes', 'human:tl')`)), "todo_tags_origin_check")
	if err := tag("'lane:m'"); err != nil {
		t.Fatalf("a well-formed tag was refused: %v", err)
	}
	if err := tag("'repo:stump.wtf/switchboard'"); err != nil {
		t.Fatalf("a tag with the grammar's punctuation was refused: %v", err)
	}
	if err := tag("repeat('t', 64)"); err != nil {
		t.Fatalf("a 64-byte tag was refused: %v", err)
	}
	expectRejected(t, "a 257-byte subject key",
		execErr(pool.Exec(ctx, `INSERT INTO todos (id, endpoint_id, queue, title, subject_key) VALUES ('tl-longkey', $1, 'lineage', 't', repeat('k', 257))`, ep)),
		"todos_subject_key_check")
	if _, err := pool.Exec(ctx, `INSERT INTO todos (id, endpoint_id, queue, title, subject_key) VALUES ('tl-ok', $1, 'lineage', 't', repeat('k', 256))`, ep); err != nil {
		t.Fatalf("a 256-byte subject key was refused: %v", err)
	}

	// Deleting a todo removes its tags and every edge it touches, as source or as the spawned
	// target, while leaving the rest of the graph standing (REQ-18).
	if _, err := pool.Exec(ctx, `DELETE FROM todos WHERE id = 'tl-old'`); err != nil {
		t.Fatalf("delete the parent todo: %v", err)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM todo_tags WHERE todo_id = 'tl-old'`); n != 0 {
		t.Fatalf("%d tags survived deleting their todo, want 0", n)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM todo_links WHERE from_todo_id = 'tl-old'`); n != 0 {
		t.Fatalf("%d links still name a deleted todo as their source, want 0", n)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM todo_links WHERE to_todo_id = 'tl-old'`); n != 0 {
		t.Fatalf("%d links still name a deleted todo as their spawned target, want 0", n)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM todo_links`); n != 1 {
		t.Fatalf("%d links remain after the cascade, want the child's one untouched link", n)
	}

	// Applying the chain again is a no-op: the migration is recorded once and the schema is
	// unchanged, so a restart or a second replica migrating does not re-run the DDL.
	before := count(t, pool, ctx, `SELECT count(*) FROM schema_migrations`)
	beforeIndexes := count(t, pool, ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'`)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate a second time: %v", err)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM schema_migrations`); n != before {
		t.Fatalf("second migrate recorded %d of %d migrations, want no change", n, before)
	}
	if n := count(t, pool, ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'`); n != beforeIndexes {
		t.Fatalf("second migrate changed the index count from %d to %d", beforeIndexes, n)
	}
}
