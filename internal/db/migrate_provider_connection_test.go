package db

import (
	"strings"
	"testing"
)

// TestEndpointProviderConnectionMigration runs 0035_endpoint_provider_connection against an instance
// that already holds webhooks: the four columns appear, every existing row reads as "no connection
// configured" rather than as something broken, an address cannot be stored without its repository, and a
// credential can exist without an address — the order provisioning (#615) actually happens in.
//
// Governing: stump.wtf/switchboard#614 (migration), ADR-0002.
func TestEndpointProviderConnectionMigration(t *testing.T) {
	pool, ctx := migrateTestPool(t)
	if err := applyMigrations(ctx, pool, migrationsBefore(t, "0035"), "migrations"); err != nil {
		t.Fatalf("migrate to 0034: %v", err)
	}

	var human, agent, ep string
	if err := pool.QueryRow(ctx, `INSERT INTO humans (oidc_subject) VALUES ('provider-conn-mig') RETURNING id::text`).Scan(&human); err != nil {
		t.Fatalf("human: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agents (owner_human_id, name) VALUES ($1, 'conn-bot') RETURNING id::text`, human).Scan(&agent); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO endpoints (agent_id, slug, credential_hash, credential_prefix)
		VALUES ($1, 'pc-1', 'h', 'p') RETURNING id::text`, agent).Scan(&ep); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	var webhook string
	if err := pool.QueryRow(ctx, `INSERT INTO endpoint_webhooks (endpoint_id, source_type, target_queue, trust_mode, ingest_token, signing_secret, trusted_actors)
		VALUES ($1, 'github', 'github', 'signed', 'pc-ingest-1', 'whsec_existing', '{"allow_all": true}'::jsonb) RETURNING id::text`, ep).Scan(&webhook); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	if err := applyMigrations(ctx, pool, migrationsFS, "migrations"); err != nil {
		t.Fatalf("apply 0035 and later: %v", err)
	}

	// An existing webhook has no connection, and reads as unset rather than as a half-configured one.
	var baseURL, repo *string
	var token *string
	if err := pool.QueryRow(ctx, `SELECT provider_base_url, provider_repo, provider_read_token
		FROM endpoint_webhooks WHERE id = $1`, webhook).Scan(&baseURL, &repo, &token); err != nil {
		t.Fatalf("read new columns: %v", err)
	}
	if baseURL != nil || repo != nil || token != nil {
		t.Fatalf("a pre-existing webhook gained a connection: %v / %v / %v", baseURL, repo, token)
	}
	var setAt *string
	if err := pool.QueryRow(ctx, `SELECT provider_connection_set_at::text FROM endpoint_webhooks WHERE id = $1`, webhook).Scan(&setAt); err != nil {
		t.Fatalf("read set_at: %v", err)
	}
	if setAt != nil {
		t.Fatalf("provider_connection_set_at = %q on a webhook nobody configured", *setAt)
	}

	// Half an address cannot be stored.
	if _, err := pool.Exec(ctx, `UPDATE endpoint_webhooks SET provider_base_url = 'https://api.github.com' WHERE id = $1`, webhook); err == nil {
		t.Fatal("an address with no repository passed the row CHECK")
	} else if !strings.Contains(err.Error(), "provider_connection_address") {
		t.Fatalf("expected the provider_connection_address violation, got %v", err)
	}

	// A credential with no address is allowed: provisioning and addressing are separate sittings, and
	// the state that results is derived (awaiting_credential), not stored.
	if _, err := pool.Exec(ctx, `UPDATE endpoint_webhooks SET provider_read_token = 'enc:v1:placeholder' WHERE id = $1`, webhook); err != nil {
		t.Fatalf("a credential without an address was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE endpoint_webhooks
		SET provider_base_url = 'https://api.github.com', provider_repo = 'stump-wtf/switchboard' WHERE id = $1`, webhook); err != nil {
		t.Fatalf("a complete address was refused: %v", err)
	}
}
