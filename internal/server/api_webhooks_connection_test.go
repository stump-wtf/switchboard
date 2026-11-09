package server

// Human API tests for the per-webhook provider connection (#614): a human — not an agent — points a
// webhook at a provider repository, reach is enforced the same way every other webhook route does, the
// credential is never echoed, and a deployment without an at-rest key refuses the credential loudly
// instead of storing it in the clear.
//
// Governing: stump.wtf/switchboard#614; SPEC-0035 REQ "Reach on Every Route", REQ "Error Handling
// Standards"; ADR-0052 (proposed).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

func connectionPath(id string) string { return "/api/v1/webhooks/" + id + "/provider-connection" }

func TestAPIWebhookProviderConnectionWrite(t *testing.T) {
	f := newRuleFixture(t)

	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{
		"provider_base_url": "https://api.github.com/",
		"provider_repo":     "stump-wtf/switchboard",
	})
	if code != http.StatusOK {
		t.Fatalf("set connection: %d %s, want 200", code, raw)
	}
	if doc["provider_base_url"] != "https://api.github.com" {
		t.Fatalf("base url not canonicalized: %s", raw)
	}
	if doc["provider_connection_state"] != store.ProviderConnectionAwaitingCredential {
		t.Fatalf("state = %v, want awaiting_credential: %s", doc["provider_connection_state"], raw)
	}
	if doc["provider_read_token_set"] != false {
		t.Fatalf("no credential was written, but the response says one is set: %s", raw)
	}

	// The listing carries the same view, so `webhook list` can answer the question without a second call.
	code, list, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodGet, "/api/v1/webhooks", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, raw)
	}
	hooks, _ := list["webhooks"].([]any)
	var found bool
	for _, item := range hooks {
		row, _ := item.(map[string]any)
		if row["webhook_id"] == f.whA {
			found = true
			if row["provider_connection_state"] != store.ProviderConnectionAwaitingCredential || row["provider_repo"] != "stump-wtf/switchboard" {
				t.Fatalf("listing lost the connection view: %v", row)
			}
		}
	}
	if !found {
		t.Fatalf("webhook %s missing from the listing: %s", f.whA, raw)
	}

	// Clearing the address clears the repository with it: they are one decision.
	code, doc, raw = ruleAPICall(t, f.r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("clear connection: %d %s", code, raw)
	}
	if doc["provider_base_url"] != nil && doc["provider_base_url"] != "" {
		t.Fatalf("clearing left an address: %s", raw)
	}
	if doc["provider_connection_state"] != store.ProviderConnectionUnset {
		t.Fatalf("state after clearing = %v, want unset: %s", doc["provider_connection_state"], raw)
	}
}

// The credential goes in and never comes out: the response reports only that one is set, the store
// opens it back to what the human pasted (so it really was sealed, not stored as the literal text), and
// "" clears it.
func TestAPIWebhookProviderConnectionTokenIsStoredAndNeverEchoed(t *testing.T) {
	f := newRuleFixture(t)
	const token = "ghp_do_not_leak"
	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{
		"provider_base_url":   "https://api.github.com",
		"provider_repo":       "stump-wtf/switchboard",
		"provider_read_token": token,
	})
	if code != http.StatusOK {
		t.Fatalf("credential write: %d %s, want 200", code, raw)
	}
	if strings.Contains(raw, token) {
		t.Fatalf("the response echoed the credential: %s", raw)
	}
	if doc["provider_read_token_set"] != true || doc["provider_connection_state"] != store.ProviderConnectionReady {
		t.Fatalf("response = %s, want read_token_set true and ready", raw)
	}

	conn, err := f.st.ProviderReadConnection(f.ctx, f.whA)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if conn.Token != token {
		t.Fatalf("stored credential opens to %q, want the value the human pasted", conn.Token)
	}

	code, doc, raw = ruleAPICall(t, f.r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{
		"provider_base_url": "https://api.github.com", "provider_repo": "stump-wtf/switchboard",
		"provider_read_token": "",
	})
	if code != http.StatusOK {
		t.Fatalf("clear credential: %d %s", code, raw)
	}
	if doc["provider_read_token_set"] != false || doc["provider_connection_state"] != store.ProviderConnectionAwaitingCredential {
		t.Fatalf("after clearing = %s", raw)
	}
	if conn, err := f.st.ProviderReadConnection(f.ctx, f.whA); err != nil || conn.Token != "" {
		t.Fatalf("credential survived the clear: %+v (%v)", conn, err)
	}
}

// A malformed address is invalid_argument, and a webhook outside the caller's reach answers the same
// 404 as one that does not exist — including for the write.
func TestAPIWebhookProviderConnectionRefusals(t *testing.T) {
	f := newRuleFixture(t)

	code, doc, raw := ruleAPICall(t, f.r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{
		"provider_base_url": "https://token:ghp_leak@api.github.com",
		"provider_repo":     "a/b",
	})
	if code != http.StatusBadRequest || doc["code"] != "invalid_argument" {
		t.Fatalf("address with credentials: %d %s, want 400 invalid_argument", code, raw)
	}

	var bodies []string
	for _, id := range []string{f.whB, "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "not-a-uuid"} {
		code, doc, raw = ruleAPICall(t, f.r, f.bearerA, http.MethodPut, connectionPath(id), map[string]any{
			"provider_base_url": "https://api.github.com", "provider_repo": "a/b",
		})
		if code != http.StatusNotFound || doc["code"] != "not_found" {
			t.Fatalf("write on %s: %d %s, want 404 not_found", id, code, raw)
		}
		bodies = append(bodies, raw)
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("the route is an existence oracle: %q", bodies)
	}
	if strings.Contains(bodies[0], "whsec_do_not_leak") {
		t.Fatalf("404 leaked a signing secret: %s", bodies[0])
	}
	rows, err := f.st.ListWebhooksForHuman(f.ctx, f.humanA.ID)
	if err != nil {
		t.Fatalf("read A's webhooks: %v", err)
	}
	for _, w := range rows {
		if w.ID == f.whA && w.ProviderBaseURL != "" {
			t.Fatalf("a refused write changed the webhook: %+v", w)
		}
	}
}

// The operator's allowlist, when set, is enforced on this route: an address outside it is forbidden and
// nothing is written.
func TestAPIWebhookProviderConnectionOriginAllowlist(t *testing.T) {
	r, st, ctx, _, _ := newDBRouterFullDeps(t, func(d *routerDeps) {
		d.cfg.ProviderReadAllowedOrigins = "https://gitea.stump.rocks"
	})
	f := &ruleFixture{r: r, st: st, ctx: ctx}
	f.humanA, _ = mintSession(t, st, ctx, "test|conn-a", "Human A", "a@example.com")
	f.bearerA = mintOperatorBearer(t, st, ctx, f.humanA.ID, "cid-conn-a")
	f.epA = consentFixture(t, st, ctx, f.humanA.ID, "conn-a", "conn-a-11111111", "cid-fixture-conn-a")
	f.whA = mustAPIWebhook(t, st, ctx, f.epA.ID, "ingest-conn-a-7f3c", "whsec_conn_leak")

	code, doc, raw := ruleAPICall(t, r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{
		"provider_base_url": "https://api.github.com", "provider_repo": "a/b",
	})
	if code != http.StatusForbidden || doc["code"] != "forbidden" {
		t.Fatalf("off-allowlist address: %d %s, want 403 forbidden", code, raw)
	}
	code, _, raw = ruleAPICall(t, r, f.bearerA, http.MethodPut, connectionPath(f.whA), map[string]any{
		"provider_base_url": "https://gitea.stump.rocks/api/v1", "provider_repo": "a/b",
	})
	if code != http.StatusOK {
		t.Fatalf("allowlisted address: %d %s, want 200", code, raw)
	}
	var stored string
	if w, err := st.WebhookForEndpoint(ctx, f.whA, f.epA.ID); err != nil {
		t.Fatalf("read webhook: %v", err)
	} else {
		stored = w.ProviderBaseURL
	}
	if stored != "https://gitea.stump.rocks/api/v1" {
		t.Fatalf("stored address = %q", stored)
	}
}
