package db

import (
	"slices"
	"testing"
)

// TestFriendReleaseVerbMigration runs 0028_friend_release_verb against rows shaped like the
// databases migration 0025 damaged: approved friend edges whose grant and whose minted
// endpoint lost the release verb (#507 postdates 0025's hardcoded narrowing list), alongside
// the rows that must stay untouched — an edge that never requested release, a pending edge,
// and an ordinary endpoint that is not a friend's.
//
// Governing: SPEC-0010 (granted ⊆ requested), SPEC-0033 REQ "Closing the Audited Surfaces"
// (F3), #507 (release is friend-grantable), #538 (the SQL side of the list was not corrected).
func TestFriendReleaseVerbMigration(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0028"), "migrations"); err != nil {
		t.Fatalf("migrate to 0027: %v", err)
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
	ep := func(slug string, verbs []string) string {
		t.Helper()
		return id(`INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix, scope_verbs)
			VALUES ($1, $2, 'h'||$2, 'p', $3) RETURNING id::text`, agent, slug, verbs)
	}
	// Post-0025 shape: what release grants were narrowed down to.
	stripped := []string{"create_for", "claim"}
	strippedEP := ep("friend", stripped)
	withheldEP := ep("friend-narrow", []string{"create_for"})
	otherEP := ep("friend-other", stripped)
	ownEP := ep("own", []string{"set_webhook_rules", "release"})

	// A release grant was stripped (the bug): requested carries release, granted does not.
	damaged := id(`INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human, state,
		requested_verbs, granted_verbs, endpoint_id)
		VALUES ('reviewer', 'maint', $1, $2, 'approved', $3, $4, $5) RETURNING id::text`,
		one, approver, []string{"create_for", "release", "claim"}, stripped, strippedEP)
	// An approver who withheld release is indistinguishable from a stripped grant; 0028
	// restores release here too. Pinned so the choice is deliberate and visible.
	withheld := id(`INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human, state,
		requested_verbs, granted_verbs, endpoint_id)
		VALUES ('curious', 'maint', $1, $2, 'approved', $3, $4, $5) RETURNING id::text`,
		one, approver, []string{"create_for", "release"}, []string{"create_for"}, withheldEP)
	// No release was ever requested; nothing to restore.
	other := id(`INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human, state,
		requested_verbs, granted_verbs, endpoint_id)
		VALUES ('helper', 'maint', $1, $2, 'approved', $3, $3, $4) RETURNING id::text`,
		two, approver, stripped, otherEP)
	// A pending edge grants nothing; approval filters verbs against the corrected store list.
	pending := id(`INSERT INTO friend_edges (from_persona, to_persona, from_human, to_human, state, requested_verbs)
		VALUES ('watcher', 'maint', $1, $2, 'pending', $3) RETURNING id::text`,
		two, approver, []string{"release", "set_webhook_rules"})

	if err := applyMigrations(ctx, pool, migrationsFS, "migrations"); err != nil {
		t.Fatalf("apply 0028 and later: %v", err)
	}

	verbs := func(sql, key string) []string {
		t.Helper()
		var v []string
		if err := pool.QueryRow(ctx, sql, key).Scan(&v); err != nil {
			t.Fatalf("read %q: %v", sql, err)
		}
		return v
	}
	want := []string{"create_for", "claim", "release"}
	if got := verbs(`SELECT granted_verbs FROM friend_edges WHERE id = $1`, damaged); !slices.Equal(got, want) {
		t.Errorf("stripped edge granted verbs = %v, want %v", got, want)
	}
	if got := verbs(`SELECT scope_verbs FROM endpoints WHERE id = $1`, strippedEP); !slices.Equal(got, want) {
		t.Errorf("stripped endpoint scope verbs = %v, want %v", got, want)
	}
	if got := verbs(`SELECT granted_verbs FROM friend_edges WHERE id = $1`, withheld); !slices.Equal(got, []string{"create_for", "release"}) {
		t.Errorf("withheld edge granted verbs = %v, want [create_for release]", got)
	}
	if got := verbs(`SELECT granted_verbs FROM friend_edges WHERE id = $1`, other); !slices.Equal(got, stripped) {
		t.Errorf("edge that never requested release changed: %v, want %v untouched", got, stripped)
	}
	if got := verbs(`SELECT scope_verbs FROM endpoints WHERE id = $1`, otherEP); !slices.Equal(got, stripped) {
		t.Errorf("endpoint of an edge that never requested release changed: %v, want %v untouched", got, stripped)
	}
	if got := verbs(`SELECT scope_verbs FROM endpoints WHERE id = $1`, ownEP); !slices.Equal(got, []string{"set_webhook_rules", "release"}) {
		t.Errorf("a non-friend endpoint changed: %v, want untouched", got)
	}
	var req []string
	if err := pool.QueryRow(ctx, `SELECT requested_verbs FROM friend_edges WHERE id = $1`, pending).Scan(&req); err != nil {
		t.Fatalf("read pending edge: %v", err)
	}
	if !slices.Equal(req, []string{"release", "set_webhook_rules"}) {
		t.Errorf("pending edge requested verbs changed: %v", req)
	}
}
