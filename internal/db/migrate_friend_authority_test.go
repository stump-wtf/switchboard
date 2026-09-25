package db

import (
	"slices"
	"testing"
)

// TestFriendAuthorityMigration runs 0025_friend_edges_own_authority against rows shaped like
// production's: an approved friend edge whose endpoint was minted with webhook and event verbs (F3),
// an ordinary vended endpoint with the same verbs that is NOT a friend endpoint, and two tenants'
// same-named personas that the old instance-wide live-edge index refused (F13).
//
// Governing: ADR-0038, SPEC-0033 REQ "Closing the Audited Surfaces" (F3, F13).
func TestFriendAuthorityMigration(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0025"), "migrations"); err != nil {
		t.Fatalf("migrate to 0024: %v", err)
	}

	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return out
	}
	approver := id(`INSERT INTO humans (oidc_subject) VALUES ('approver') RETURNING id::text`)
	one := id(`INSERT INTO humans (oidc_subject) VALUES ('one') RETURNING id::text`)
	two := id(`INSERT INTO humans (oidc_subject) VALUES ('two') RETURNING id::text`)
	agent := id(`INSERT INTO agents (owner_human_id, name) VALUES ($1, 'a') RETURNING id::text`, approver)
	wide := []string{"create_for", "set_webhook_rules", "claim", "list_webhook_events"}
	friendEP := id(`INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix, scope_verbs)
		VALUES ($1, 'friend', 'h1', 'p', $2) RETURNING id::text`, agent, wide)
	ownEP := id(`INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix, scope_verbs)
		VALUES ($1, 'own', 'h2', 'p', $2) RETURNING id::text`, agent, wide)
	edge := id(`INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human, state, requested_verbs,
		granted_verbs, endpoint_id) VALUES ('reviewer', 'maint', $1, $2, 'approved', $3, $3, $4) RETURNING id::text`,
		one, approver, wide, friendEP)

	if err := applyMigrations(ctx, pool, migrationsFS, "migrations"); err != nil {
		t.Fatalf("apply 0025 and later: %v", err)
	}

	verbs := func(sql, key string) []string {
		t.Helper()
		var v []string
		if err := pool.QueryRow(ctx, sql, key).Scan(&v); err != nil {
			t.Fatalf("read %q: %v", sql, err)
		}
		return v
	}
	if got := verbs(`SELECT scope_verbs FROM endpoints WHERE id = $1`, friendEP); !slices.Equal(got, []string{"create_for", "claim"}) {
		t.Errorf("friend endpoint verbs = %v, want [create_for claim]", got)
	}
	if got := verbs(`SELECT granted_verbs FROM friend_edges WHERE id = $1`, edge); !slices.Equal(got, []string{"create_for", "claim"}) {
		t.Errorf("friend edge granted verbs = %v, want [create_for claim]", got)
	}
	if got := verbs(`SELECT scope_verbs FROM endpoints WHERE id = $1`, ownEP); !slices.Equal(got, wide) {
		t.Errorf("a non-friend endpoint was narrowed: %v, want %v untouched", got, wide)
	}

	// F13: a second tenant's same-named persona to the same target is now its own live edge.
	if _, err := pool.Exec(ctx, `INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human)
		VALUES ('reviewer', 'maint', $1, $2)`, two, approver); err != nil {
		t.Fatalf("second tenant's request collided with the first: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human)
		VALUES ('reviewer', 'maint', $1, $2)`, one, approver); err == nil {
		t.Error("the same human's second live request must still collide")
	}
}
