package db

import (
	"testing"
)

// TestEventsOwnerBackfill runs 0024_events_owner against rows shaped like production's: a webhook
// delivery (owned through webhook_id), an operator push (owned through its single todo, whose
// endpoint prefixes its external id), a delivery whose webhook was already deleted (no provable
// owner), and two owners' deliveries sharing (source, external_id), which the old instance-wide
// dedup index would have refused.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owner-Scoped History Reads" (backfill through webhook_id; an
// event whose owner cannot be established stays unowned), REQ "Closing the Audited Surfaces" (F14).
func TestEventsOwnerBackfill(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0024"), "migrations"); err != nil {
		t.Fatalf("migrate to 0023: %v", err)
	}

	var human, agent, ep, wh string
	if err := pool.QueryRow(ctx, `INSERT INTO humans (oidc_subject) VALUES ('backfill') RETURNING id::text`).Scan(&human); err != nil {
		t.Fatalf("human: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agents (owner_human_id, name) VALUES ($1, 'a') RETURNING id::text`, human).Scan(&agent); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix)
		VALUES ($1, 'bf-1', 'h', 'p') RETURNING id::text`, agent).Scan(&ep); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO endpoint_webhooks (endpoint_id, source_type, target_queue, trust_mode, ingest_token)
		VALUES ($1, 'github', 'q', 'token', 'tok') RETURNING id::text`, ep).Scan(&wh); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	insertEvent := func(family, externalID string, webhookID any) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO events (source, family, external_id, trust_mode, verified, webhook_id)
			VALUES ('github', $1, $2, 'token', false, $3::uuid) RETURNING id`, family, externalID, webhookID).Scan(&id); err != nil {
			t.Fatalf("event %s: %v", externalID, err)
		}
		return id
	}
	viaWebhook := insertEvent("webhook", "d-1", wh)
	operator := insertEvent("operator", ep+":k", nil)
	if _, err := pool.Exec(ctx, `INSERT INTO todos (id, queue, title, event_id, endpoint_id)
		VALUES ('t-op', 'q', 'op', $1, $2)`, operator, ep); err != nil {
		t.Fatalf("operator todo: %v", err)
	}
	orphan := insertEvent("webhook", "d-2", nil)

	if err := applyMigrations(ctx, pool, migrationsFS, "migrations"); err != nil {
		t.Fatalf("apply 0024 and later: %v", err)
	}

	owner := func(id int64) string {
		t.Helper()
		var o *string
		if err := pool.QueryRow(ctx, `SELECT endpoint_id::text FROM events WHERE id = $1`, id).Scan(&o); err != nil {
			t.Fatalf("read owner %d: %v", id, err)
		}
		if o == nil {
			return ""
		}
		return *o
	}
	if got := owner(viaWebhook); got != ep {
		t.Errorf("webhook delivery owner = %q, want the webhook's endpoint %q", got, ep)
	}
	if got := owner(operator); got != ep {
		t.Errorf("operator push owner = %q, want its todo's endpoint %q", got, ep)
	}
	if got := owner(orphan); got != "" {
		t.Errorf("delivery with no provable owner was given one: %q", got)
	}

	// F14: after the migration a second owner may record the same (source, external_id).
	var other string
	if err := pool.QueryRow(ctx, `INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix)
		VALUES ($1, 'bf-2', 'h2', 'p') RETURNING id::text`, agent).Scan(&other); err != nil {
		t.Fatalf("second endpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (source, family, external_id, trust_mode, verified, endpoint_id)
		VALUES ('github', 'webhook', 'd-1', 'token', false, $1)`, other); err != nil {
		t.Fatalf("a second owner's delivery with the same (source, external_id) must be accepted: %v", err)
	}
	// ...while the same owner still dedups, and owner-less rows still dedup among themselves.
	if _, err := pool.Exec(ctx, `INSERT INTO events (source, family, external_id, trust_mode, verified, endpoint_id)
		VALUES ('github', 'webhook', 'd-1', 'token', false, $1)`, ep); err == nil {
		t.Error("the same owner's duplicate delivery must still violate the dedup index")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (source, family, external_id, trust_mode, verified)
		VALUES ('github', 'webhook', 'd-2', 'token', false)`); err == nil {
		t.Error("owner-less rows must keep deduplicating among themselves (NULLS NOT DISTINCT)")
	}
}
