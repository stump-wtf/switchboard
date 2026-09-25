package mcp

// tools/call tests for the SPEC-0026 REQ-5 trust surface against the REAL store: create_webhook's
// optional trusted_actors (omitted means an empty list, and the result says so), set_trusted_actors
// (argument required, validated per source, allow_all exclusive and flagged), clear_trusted_actors,
// list_webhooks echoing the field and allow_all, and tenancy: another human's webhook, and an
// unknown one, answer not_found.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// --- fakeStore trust stubs (the behaviour under test runs against the real store below) ---

func (f *fakeStore) CreateWebhookWithTrust(ctx context.Context, endpointID, sourceType, targetQueue, trustMode, ingestToken, secret string, max int, trustedActors []byte) (store.Webhook, error) {
	w, err := f.CreateWebhook(ctx, endpointID, sourceType, targetQueue, trustMode, ingestToken, secret, max)
	if err != nil {
		return w, err
	}
	if trustedActors == nil {
		if empty, ok := routing.DefaultTrustedActors(sourceType); ok {
			trustedActors, _ = json.Marshal(empty)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.TrustedActors = trustedActors
	f.webhooks[w.ID] = w
	return w, nil
}

func (f *fakeStore) WebhookForEndpoint(_ context.Context, id, endpointID string) (store.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok || w.EndpointID != endpointID {
		return store.Webhook{}, store.ErrNotFound
	}
	return w, nil
}

func (f *fakeStore) SetWebhookTrustedActors(_ context.Context, id, endpointID string, trustedActors []byte) (store.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok || w.EndpointID != endpointID {
		return store.Webhook{}, store.ErrNotFound
	}
	w.TrustedActors = trustedActors
	f.webhooks[id] = w
	return w, nil
}

func (f *fakeStore) QuarantineCounts(context.Context, string) (map[string]int, error) {
	return map[string]int{}, nil
}

var trustVerbs = []string{"create_webhook", "list_webhooks", "set_trusted_actors", "clear_trusted_actors"}

// An endpoint vended before the trust verbs existed holds create_webhook but not set_trusted_actors
// (endpoint scope never widens, SPEC-0007). Its empty-list warning must not end at a verb it cannot
// call: it says to recreate the webhook with trusted_actors or re-vend, and recreating works.
func TestCreateWebhookWarnsWhenTrustVerbsAreNotGranted(t *testing.T) {
	pool, ctx := routeTestPool(t)
	f := newRouteFixture(t, ctx, pool)
	epID, token := mustEndpoint(t, ctx, f.st, f.agentA1, "trust-old-99999999", []string{"create_webhook", "list_webhooks", "delete_webhook"})
	if _, err := pool.Exec(ctx, `UPDATE endpoints SET webhook_max = 10,
		webhook_source_types = ARRAY['github'], webhook_queues = ARRAY['reviews'] WHERE id = $1`, epID); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}
	cs := routeSession(t, ctx, f.st, "trust-old-99999999", token)

	var created webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews"}, &created)
	if !strings.Contains(created.Warning, "quarantined") || !strings.Contains(created.Warning, "not granted set_trusted_actors") ||
		!strings.Contains(created.Warning, "re-vend") {
		t.Fatalf("warning = %q, want the empty-list warning plus the recreate/re-vend path", created.Warning)
	}
	// The verb really is out of reach for this endpoint.
	if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "set_trusted_actors", Arguments: map[string]any{
		"webhook_id": created.WebhookID, "trusted_actors": map[string]any{"logins": []string{"joestump"}}}}); err == nil && !res.IsError {
		t.Fatal("set_trusted_actors succeeded on an endpoint that was not granted it")
	}
	var recreated webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews",
		"trusted_actors": map[string]any{"logins": []string{"joestump"}}}, &recreated)
	if recreated.TrustedActors == nil || len(recreated.TrustedActors.Logins) != 1 || recreated.Warning != "" {
		t.Fatalf("recreate with a list = %+v, want the list and no warning", recreated)
	}
}

func TestTrustedActorVerbs(t *testing.T) {
	pool, ctx := routeTestPool(t)
	f := newRouteFixture(t, ctx, pool)
	epID, token := mustEndpoint(t, ctx, f.st, f.agentA1, "trust-a-66666666", trustVerbs)
	if _, err := pool.Exec(ctx, `UPDATE endpoints SET webhook_max = 10,
		webhook_source_types = ARRAY['github','gitea','cairn','generic'], webhook_queues = ARRAY['reviews'] WHERE id = $1`, epID); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}
	cs := routeSession(t, ctx, f.st, "trust-a-66666666", token)

	// REQ-5 scenario "A new webhook fails closed": omitted means an empty list, and the result says
	// every delivery will be held.
	var created webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews"}, &created)
	if created.TrustedActors == nil || created.TrustedActors.AllowAll || len(created.TrustedActors.Logins) != 0 ||
		created.TrustedActors.Match != routing.MatchSender || !strings.Contains(created.Warning, "quarantined") {
		t.Fatalf("create without a list = %+v, want an empty list and the quarantine warning", created)
	}
	if strings.Contains(created.Warning, "re-vend") {
		t.Fatalf("warning %q sends an endpoint that holds set_trusted_actors to re-vend", created.Warning)
	}
	var listed webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "gitea", "target_queue": "reviews",
		"trusted_actors": map[string]any{"logins": []string{"joestump"}, "match": "both"}}, &listed)
	if listed.TrustedActors == nil || len(listed.TrustedActors.Logins) != 1 || listed.TrustedActors.Match != "both" || listed.Warning != "" {
		t.Fatalf("create with a list = %+v", listed)
	}
	// SPEC-0026 REQ-6 scenario "Reserved name refused".
	if msg := callErr(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "quarantine"},
		codeInvalidArgument); !strings.Contains(msg, "reserved") {
		t.Fatalf("reserved target refusal %q does not say why", msg)
	}
	// REQ-5 scenario "Token-trust webhook refused", on create and on set.
	if msg := callErr(t, ctx, cs, "create_webhook", map[string]any{"source_type": "generic", "target_queue": "reviews",
		"trusted_actors": map[string]any{"allow_all": true}}, codeInvalidArgument); !strings.Contains(msg, "not signed") {
		t.Fatalf("generic create refusal %q does not say why", msg)
	}
	var generic webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "generic", "target_queue": "reviews"}, &generic)
	if generic.TrustedActors != nil || generic.Warning != "" {
		t.Fatalf("generic create = %+v, want no trust list", generic)
	}
	if msg := callErr(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": generic.WebhookID,
		"trusted_actors": map[string]any{"logins": []string{"x"}}}, codeInvalidArgument); !strings.Contains(msg, "not signed") {
		t.Fatalf("generic set refusal %q does not say why", msg)
	}
	callErr(t, ctx, cs, "clear_trusted_actors", map[string]any{"webhook_id": generic.WebhookID}, codeInvalidArgument)

	// REQ-5 scenario "Allow-all is explicit and flagged".
	var out trustedActorsOut
	callOK(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": created.WebhookID,
		"trusted_actors": map[string]any{"allow_all": true}}, &out)
	if !out.AllowAll || !strings.Contains(out.Warning, "allow_all") {
		t.Fatalf("allow_all = %+v, want flagged", out)
	}
	var list listWebhooksOut
	callOK(t, ctx, cs, "list_webhooks", map[string]any{}, &list)
	found := false
	for _, w := range list.Webhooks {
		if w.WebhookID == created.WebhookID {
			found = w.AllowAll && w.TrustedActors != nil && w.TrustedActors.AllowAll
		}
		if w.WebhookID == generic.WebhookID && (w.AllowAll || w.TrustedActors != nil) {
			t.Fatalf("generic row = %+v, want no trust list", w)
		}
	}
	if !found {
		t.Fatalf("list_webhooks = %+v, want the allow_all flag on %s", list.Webhooks, created.WebhookID)
	}

	// REQ-5 scenario "Omitted argument never clears", plus the refused shapes.
	if msg := callErr(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": created.WebhookID}, codeInvalidArgument); !strings.Contains(msg, "required") {
		t.Fatalf("omitted refusal %q", msg)
	}
	callErr(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": created.WebhookID,
		"trusted_actors": map[string]any{"allow_all": true, "logins": []string{"a"}}}, codeInvalidArgument)
	callErr(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": created.WebhookID,
		"trusted_actors": map[string]any{"actor_ids": []string{"a"}}}, codeInvalidArgument)
	stored, err := f.st.WebhookForEndpoint(ctx, created.WebhookID, epID)
	if err != nil || !strings.Contains(string(stored.TrustedActors), "allow_all") {
		t.Fatalf("stored after refused sets = %s (%v), want allow_all unchanged", stored.TrustedActors, err)
	}

	out = trustedActorsOut{} // decode fresh: omitempty fields would otherwise carry over
	callOK(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": created.WebhookID,
		"trusted_actors": map[string]any{"logins": []string{"joestump", "joestump-agent"}}}, &out)
	if out.AllowAll || len(out.TrustedActors.Logins) != 2 || out.Warning != "" {
		t.Fatalf("set list = %+v", out)
	}
	out = trustedActorsOut{}
	callOK(t, ctx, cs, "clear_trusted_actors", map[string]any{"webhook_id": created.WebhookID}, &out)
	if out.AllowAll || len(out.TrustedActors.Logins) != 0 || !strings.Contains(out.Warning, "quarantined") {
		t.Fatalf("clear = %+v, want the empty list", out)
	}
}

// Tenancy: human A cannot read or change human B's trust list, and an unknown or malformed id looks
// the same. B's list is unchanged afterwards.
func TestTrustedActorVerbsCrossTenant(t *testing.T) {
	pool, ctx := routeTestPool(t)
	f := newRouteFixture(t, ctx, pool)
	_, token := mustEndpoint(t, ctx, f.st, f.agentA1, "trust-x-77777777", trustVerbs)
	cs := routeSession(t, ctx, f.st, "trust-x-77777777", token)

	for _, id := range []string{f.webhookB, f.webhookA, "00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		// webhookA belongs to A's OTHER endpoint (epA1): the trust verbs are endpoint-scoped, like
		// rotate and delete.
		callErr(t, ctx, cs, "set_trusted_actors", map[string]any{"webhook_id": id,
			"trusted_actors": map[string]any{"allow_all": true}}, codeNotFound)
		callErr(t, ctx, cs, "clear_trusted_actors", map[string]any{"webhook_id": id}, codeNotFound)
	}
	b, err := f.st.WebhookForEndpoint(ctx, f.webhookB, f.epB)
	if err != nil || strings.Contains(string(b.TrustedActors), "allow_all") {
		t.Fatalf("B's list after A's attempts = %s (%v), want unchanged", b.TrustedActors, err)
	}
}

// test_webhook_rules computes .actor from the webhook's stored trust list, as the receiver does, and
// reports whether the gate would hold the delivery.
func TestTestWebhookRulesReportsTheTrustGate(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")
	if _, err := f.st.SetWebhookTrustedActors(ctx, f.webhookA, f.epA1, []byte(`{"logins":["JoeStump"],"match":"sender"}`)); err != nil {
		t.Fatalf("set trust: %v", err)
	}
	rules := []any{map[string]any{"id": "outsider", "expr": `.actor.author_trusted == false`, "action": map[string]any{"queue": "forge"}}}
	issue := func(sender string) map[string]any {
		return map[string]any{"action": "labeled", "issue": map[string]any{"number": 1, "user": map[string]any{"login": "mallory"}},
			"sender": map[string]any{"login": sender}}
	}
	hdr := map[string]string{"X-GitHub-Event": "issues"}

	var res testWebhookRulesOut
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA, "payload": issue("mallory"),
		"headers": hdr, "rules": rules, "omit_envelope": true}, &res)
	if !res.Held || res.Actor == nil || res.Actor.IsTrusted() || *res.Actor.Sender != "mallory" {
		t.Fatalf("outsider dry-run = %+v, want held with mallory untrusted", res)
	}
	// The decision is what the receiver records for a held delivery, not what the rules would do;
	// the rules' outcome is reported aside, and a held delivery previews no work order.
	if d := res.Decision; d.Disposition != routing.DispositionFaulted || !d.Faulted || d.Queue != "" || len(d.Endpoints) != 0 ||
		res.Trace.Stage != routing.StageTrustGate || res.Trace.Cause != routing.CauseUntrustedActor || res.WorkOrder != nil {
		t.Fatalf("outsider decision = %+v, trace %+v, want the trust gate's faulted outcome", d, res.Trace)
	}
	if res.RulesWould == nil || res.RulesWould.Decision.Queue != "forge" || res.RulesWould.Decision.Disposition != routing.DispositionRouted ||
		res.RulesWould.Trace.RuleID != "outsider" {
		t.Fatalf("rules_would = %+v, want the outsider rule routing to forge", res.RulesWould)
	}
	res = testWebhookRulesOut{}
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{"webhook_id": f.webhookA, "payload": issue("joestump"),
		"headers": hdr, "rules": rules}, &res)
	if res.Held || res.RulesWould != nil || !res.Actor.IsTrusted() || res.Decision.Queue != "forge" || res.Trace.RuleID != "outsider" {
		t.Fatalf("maintainer dry-run = %+v, want trusted and routed by the author_trusted rule", res)
	}
	actor, _ := res.Envelope["actor"].(map[string]any)
	if actor["author_trusted"] != false || actor["sender_trusted"] != true {
		t.Fatalf("envelope .actor = %v", actor)
	}
}
