package ingest

// The trust gate at the receiver (ADR-0031, SPEC-0026 REQ-5, REQ-10): it runs after verification,
// before any rule, on every github, gitea and cairn webhook. An untrusted delivery never reaches the
// rules. Until the quarantine queue lands (#386) it is recorded (disposition faulted, stage
// trust_gate, the actor on the trace) and routed nowhere. A trusted delivery reaches the rules with
// .actor, and its work order carries author_trusted.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

func githubIssueBody(action, author, sender string) string {
	return fmt.Sprintf(`{"action":%q,"issue":{"number":7,"title":"t","html_url":"https://github.com/o/r/issues/7",`+
		`"state":"open","user":{"login":%q},"labels":[{"name":"size/M"}]},"label":{"name":"size/M"},`+
		`"repository":{"full_name":"o/r"},"sender":{"login":%q}}`, action, author, sender)
}

func postGitHubIssue(ing *Ingest, token, secret, delivery, body string) int {
	return postSelfManaged(ing, token, body, map[string]string{
		"X-GitHub-Event": "issues", "X-GitHub-Delivery": delivery, "X-Hub-Signature-256": githubSig(secret, body),
	}).Code
}

func eventRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string) (string, routing.Trace) {
	t.Helper()
	var disp string
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT disposition, routing_trace FROM events WHERE external_id LIKE '%' || $1`, key).Scan(&disp, &raw); err != nil {
		t.Fatalf("read event %s: %v", key, err)
	}
	var tr routing.Trace
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("decode trace %s: %v", raw, err)
	}
	return disp, tr
}

func setTrust(t *testing.T, ctx context.Context, st *store.Store, wh store.Webhook, raw string) {
	t.Helper()
	if _, err := st.SetWebhookTrustedActors(ctx, wh.ID, wh.EndpointID, []byte(raw)); err != nil {
		t.Fatalf("set trusted actors: %v", err)
	}
}

// REQ-5 scenario "A new webhook fails closed", then "Maintainer label promotes an outsider's issue".
func TestTrustGateHoldsUntrustedAndPromotesOnMaintainerLabel(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	const secret = "whsec_trustgate"
	h, owner := seedEndpoint(t, st, ctx, "trust-gate", []string{"reviews"})
	wh, err := st.CreateWebhook(ctx, owner.ID, "github", "reviews", "signed", "tok-trust-gate", secret, 3) // no list: fail closed
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{
		Rules: []routing.Rule{{ID: "outsider-text", Expr: `.issue.label_event and .actor.author_trusted == false`,
			Action: routing.Action{Queue: "reviews", WorkOrder: true}}},
		Default: &routing.Action{Drop: true},
	})

	// A new webhook trusts no one: even the owner's own delivery is held.
	if code := postGitHubIssue(ing, "tok-trust-gate", secret, "d-new", githubIssueBody("opened", "joestump", "joestump")); code != 202 {
		t.Fatalf("new-webhook delivery = %d, want 202", code)
	}
	if disp, tr := eventRow(t, ctx, pool, "d-new"); disp != store.DispositionFaulted || tr.Stage != routing.StageTrustGate ||
		tr.Cause != routing.CauseUntrustedActor || tr.Actor == nil || tr.Actor.IsTrusted() {
		t.Fatalf("new-webhook event = %s %+v, want held at the trust gate", disp, tr)
	}

	setTrust(t, ctx, st, wh, `{"logins":["JoeStump"],"match":"sender"}`)

	// mallory opens an issue: held, no rule runs, no todo.
	if code := postGitHubIssue(ing, "tok-trust-gate", secret, "d-open", githubIssueBody("opened", "mallory", "mallory")); code != 202 {
		t.Fatalf("outsider delivery = %d, want 202", code)
	}
	disp, tr := eventRow(t, ctx, pool, "d-open")
	if disp != store.DispositionFaulted || tr.Stage != routing.StageTrustGate || tr.RuleID != "" ||
		*tr.Actor.Sender != "mallory" || *tr.Actor.SenderTrusted {
		t.Fatalf("outsider event = %s %+v, want held with mallory untrusted", disp, tr)
	}
	if !strings.Contains(logs.String(), "untrusted actor") || !strings.Contains(logs.String(), "sender=mallory") {
		t.Fatalf("log = %q, want the hold logged with the sender", logs.String())
	}

	// joestump labels it: trusted sender, untrusted author, and the rule reads .actor.
	if code := postGitHubIssue(ing, "tok-trust-gate", secret, "d-label", githubIssueBody("labeled", "mallory", "joestump")); code != 202 {
		t.Fatalf("maintainer label = %d, want 202", code)
	}
	disp, tr = eventRow(t, ctx, pool, "d-label")
	if disp != store.DispositionRouted || tr.RuleID != "outsider-text" {
		t.Fatalf("maintainer event = %s %+v, want routed by outsider-text", disp, tr)
	}
	var wo []byte
	if err := pool.QueryRow(ctx, `SELECT work_order FROM todos WHERE endpoint_id = $1`, owner.ID).Scan(&wo); err != nil {
		t.Fatalf("read todo: %v", err)
	}
	var order routing.WorkOrder
	if err := json.Unmarshal(wo, &order); err != nil || order.AuthorTrusted == nil || *order.AuthorTrusted {
		t.Fatalf("work order = %s (%v), want author_trusted false", wo, err)
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 1 {
		t.Fatalf("todos = %d, want only the promoted one", n)
	}
}

// REQ-5 scenario "Self-reported Cairn identity ignored", and a corrupt stored list trusts no one.
func TestTrustGateCairnAndCorruptList(t *testing.T) {
	ing, pool, ctx, logs := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	_, owner := seedEndpoint(t, st, ctx, "trust-cairn", []string{"inbox"})
	wh, err := st.CreateWebhookWithTrust(ctx, owner.ID, "cairn", "inbox", "signed", "tok-trust-cairn", cairnSecret, 3,
		[]byte(`{"actor_ids":["acct_joe"]}`))
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	body := fmt.Sprintf(`{"source":"cairn","kind":"artifact.created","event_id":"evt-obo","created_at":%q,`+
		`"data":{"id":"a1","title":"t","actor_id":"acct_other","on_behalf_of":"acct_joe"}}`, time.Now().UTC().Format(time.RFC3339Nano))
	if code := postSelfManaged(ing, "tok-trust-cairn", body, cairnHeaders(body, "evt-obo")).Code; code != 202 {
		t.Fatalf("cairn delivery = %d, want 202", code)
	}
	if disp, tr := eventRow(t, ctx, pool, "evt-obo"); disp != store.DispositionFaulted || tr.Stage != routing.StageTrustGate {
		t.Fatalf("on_behalf_of delivery = %s %+v, want held", disp, tr)
	}

	// A corrupt stored list (possible only by hand, past the application) trusts no one and says so.
	if _, err := pool.Exec(ctx, `UPDATE endpoint_webhooks SET trusted_actors = '{"logins":"acct_joe"}' WHERE id = $1`, wh.ID); err != nil {
		t.Fatalf("corrupt list: %v", err)
	}
	body2 := fmt.Sprintf(`{"source":"cairn","kind":"artifact.created","event_id":"evt-corrupt","created_at":%q,`+
		`"data":{"id":"a2","title":"t","actor_id":"acct_joe"}}`, time.Now().UTC().Format(time.RFC3339Nano))
	if code := postSelfManaged(ing, "tok-trust-cairn", body2, cairnHeaders(body2, "evt-corrupt")).Code; code != 202 {
		t.Fatalf("corrupt-list delivery = %d, want 202", code)
	}
	if disp, _ := eventRow(t, ctx, pool, "evt-corrupt"); disp != store.DispositionFaulted {
		t.Fatalf("corrupt-list delivery disposition = %s, want held", disp)
	}
	if out := logs.String(); !strings.Contains(out, "stage=\"trust gate\"") || !strings.Contains(out, "trusting no one") {
		t.Fatalf("log = %q, want the trust-gate error", out)
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, owner.ID); n != 0 {
		t.Fatalf("todos = %d, want none", n)
	}
}

// A source with no actor projection has no gate: a generic webhook routes as before.
func TestTrustGateSkipsSourcesWithoutProjection(t *testing.T) {
	ing, pool, ctx, _ := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	_, _, _ = seedWebhook(t, st, ctx, "generic", "token", "inbox", "tok-trust-generic", "")
	if _, q := accepted202(t, postSelfManaged(ing, "tok-trust-generic", `{"sender":{"login":"mallory"}}`, nil)); q != "inbox" {
		t.Fatalf("generic queue = %q, want inbox", q)
	}
}
