package ingest

// DB-backed tests for the routing stage on the self-managed receiver, and for cairn as a verified
// source end to end: a signed cairn delivery verifies and dedups on its signed event_id, replays and
// tampering are refused with nothing persisted, a drop is recorded but silent (and stays dropped),
// rules narrow fan-out to a routed endpoint, and neither a shrunken grant nor a rule row written
// behind validation's back can put work in a queue or tenant the webhook does not reach.
//
// Rules run through routing.InProcess here; the out-of-process sandbox is exercised in
// internal/routing and internal/mcp.
//
// Governing: ADR-0024, SPEC-0020 REQ "Drop Action Semantics", REQ "Routing Trace", REQ "Isolation
// and Tenant Safety"; SPEC-0001 REQ "Signed Webhook Verification", REQ "Idempotency Key Extraction
// and Dedup"; ADR-0022.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

const cairnSecret = "whsec_cairn_ingest"

func cairnHeaders(body, eventID string) map[string]string {
	return map[string]string{
		"X-Cairn-Signature": cairnSig(cairnSecret, body),
		"X-Cairn-Event":     "artifact.created",
		"X-Cairn-Event-Id":  eventID,
	}
}

// cairnHandoffBody is a cairn body whose metadata names a handoff target (a future cairn field the
// envelope already passes through as .artifact.metadata).
func cairnHandoffBody(eventID, title, handoffTo string) string {
	return fmt.Sprintf(`{"source":"cairn","kind":"artifact.created","event_id":%q,"created_at":%q,`+
		`"data":{"id":"h1","share_type":"markdown","title":%q,"url":"https://cairn.example/a/h1","metadata":{"handoff_to":%q}}}`,
		eventID, time.Now().UTC().Format(time.RFC3339Nano), title, handoffTo)
}

func setRules(t *testing.T, ctx context.Context, st *store.Store, webhookID, humanID string, cfg routing.Config) {
	t.Helper()
	if _, err := st.UpdateWebhookRouting(ctx, webhookID, humanID, func(store.WebhookRouting) (routing.Config, error) {
		return cfg, nil
	}); err != nil {
		t.Fatalf("set rules: %v", err)
	}
}

func setWebhookQueues(t *testing.T, ctx context.Context, pool *pgxpool.Pool, endpointID string, queues ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE endpoints SET webhook_queues = $2 WHERE id = $1`, endpointID, queues); err != nil {
		t.Fatalf("set webhook queues: %v", err)
	}
}

// secondEndpoint vends another endpoint under the SAME human, so a route to it needs no friendship.
func secondEndpoint(t *testing.T, ctx context.Context, st *store.Store, humanID, label string, queues []string) store.Endpoint {
	t.Helper()
	ag, err := st.CreateAgent(ctx, humanID, "bot2-"+label, "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, "credhash2-"+label, "sbk2_"+label, slug, queues, []string{"list_todos"})
	if err != nil {
		t.Fatalf("vend second endpoint: %v", err)
	}
	return ep
}

type routedResponse struct {
	Todos   []acceptedTodo `json:"todos"`
	Created int            `json:"created"`
	Dropped bool           `json:"dropped"`
	ID      string         `json:"id"`
}

// routed decodes a 202 from the routed receiver, including the dropped shape accepted202 rejects.
func routed(t *testing.T, rec *httptest.ResponseRecorder) routedResponse {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var r routedResponse
	decode(t, rec.Body.Bytes(), &r)
	return r
}

func traceOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) routing.Trace {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		t.Fatalf("read trace (%s): %v", query, err)
	}
	var tr routing.Trace
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("decode trace %s: %v", raw, err)
	}
	return tr
}

func TestSelfManagedCairnSignedRoundTrip(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	_, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-roundtrip", cairnSecret)

	body := cairnBody("evt-rt-1", time.Now(), "hello")
	rec := postSelfManaged(ing, "cairn-roundtrip", body, cairnHeaders(body, "evt-rt-1"))
	id1, queue := accepted202(t, rec)
	if queue != "inbox" {
		t.Fatalf("queue = %q, want inbox", queue)
	}

	var eventType, externalID, webhookID string
	var verified bool
	if err := pool.QueryRow(ctx, `SELECT COALESCE(event_type,''), external_id, webhook_id::text, verified FROM events WHERE source = 'cairn'`).
		Scan(&eventType, &externalID, &webhookID, &verified); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if eventType != "artifact.created" || externalID != wh.ID+":evt-rt-1" || webhookID != wh.ID || !verified {
		t.Fatalf("event = (%s, %s, %s, verified %v), want the verified artifact.created keyed on the signed event_id", eventType, externalID, webhookID, verified)
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM todos WHERE id = $1 AND endpoint_id = $2`, id1, owner.ID).Scan(&title); err != nil {
		t.Fatalf("read todo: %v", err)
	}
	if title != "cairn artifact.created — hello (markdown)" {
		t.Fatalf("title = %q", title)
	}
	if tr := traceOf(t, ctx, pool, `SELECT routing_trace FROM todos WHERE id = $1`, id1); tr.Stage != routing.StageDefault || tr.Cause != routing.CauseNoMatch {
		t.Fatalf("todo trace = %+v, want the default with no rules", tr)
	}

	// An identical replay inside the window collapses onto the original todo.
	id2, _ := accepted202(t, postSelfManaged(ing, "cairn-roundtrip", body, cairnHeaders(body, "evt-rt-1")))
	if id2 != id1 {
		t.Fatalf("replay minted %q, want dedup onto %q", id2, id1)
	}

	// Every forgery fails closed and persists nothing.
	stale := cairnBody("evt-rt-stale", time.Now().Add(-10*time.Minute), "old")
	for name, attempt := range map[string]struct {
		body string
		hdr  map[string]string
	}{
		"fresh dedup key via unsigned header": {body, cairnHeaders(body, "evt-rt-forged")},
		"tampered body":                       {strings.Replace(body, "hello", "hellO", 1), cairnHeaders(body, "evt-rt-1")},
		"missing signature":                   {body, map[string]string{"X-Cairn-Event": "artifact.created"}},
		"replay outside the window":           {stale, cairnHeaders(stale, "evt-rt-stale")},
	} {
		rec := postSelfManaged(ing, "cairn-roundtrip", attempt.body, attempt.hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401 (body: %s)", name, rec.Code, rec.Body.String())
		}
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE source = 'cairn'`); n != 1 {
		t.Fatalf("cairn events = %d, want 1 (rejections persist nothing)", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE source = 'cairn'`); n != 1 {
		t.Fatalf("cairn todos = %d, want 1", n)
	}
}

func TestSelfManagedRoutingDropIsRecordedButSilent(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-drop", cairnSecret)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "noise", Expr: `.artifact.title | startswith("[noise]")`, Action: routing.Action{Drop: true}},
	}})
	ch, cancel := hub.Subscribe(owner.ID, []string{"inbox"})
	defer cancel()

	noise := cairnBody("evt-noise", time.Now(), "[noise] scratch")
	rec := postSelfManaged(ing, "cairn-drop", noise, cairnHeaders(noise, "evt-noise"))
	if r := routed(t, rec); !r.Dropped || r.Created != 0 || len(r.Todos) != 0 {
		t.Fatalf("noise response = %+v, want dropped with no todos", r)
	}
	if tr := traceOf(t, ctx, pool, `SELECT routing_trace FROM events WHERE external_id = $1`, wh.ID+":evt-noise"); !tr.Action.Drop || tr.RuleID != "noise" {
		t.Fatalf("event trace = %+v, want the noise rule's drop", tr)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE source = 'cairn'`); n != 0 {
		t.Fatalf("todos after drop = %d, want 0", n)
	}
	if n := drainHub(ch); n != 0 {
		t.Fatalf("doorbells after drop = %d, want 0", n)
	}

	// The owner removes the rule; a redelivery of the dropped event is STILL dropped (its dedup slot
	// was spent by the first decision), while a new delivery routes normally.
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{})
	rec = postSelfManaged(ing, "cairn-drop", noise, cairnHeaders(noise, "evt-noise"))
	if r := routed(t, rec); !r.Dropped {
		t.Fatalf("redelivery after rule change = %+v, want still dropped", r)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE source = 'cairn'`); n != 1 {
		t.Fatalf("events = %d, want the one dropped delivery", n)
	}
	real := cairnBody("evt-real", time.Now(), "[noise] but after the rule was removed")
	if _, q := accepted202(t, postSelfManaged(ing, "cairn-drop", real, cairnHeaders(real, "evt-real"))); q != "inbox" {
		t.Fatalf("new delivery queue = %q, want inbox", q)
	}
	if n := drainHub(ch); n != 1 {
		t.Fatalf("doorbells after a routed delivery = %d, want 1", n)
	}
}

// A token (generic) webhook routes on headers too: Gitea CI chatter dropped, issues kept.
func TestSelfManagedRoutingOnHeaders(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, _, wh := seedWebhook(t, st, ctx, "generic", "token", "forge", "generic-headers", "")
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "ci", Expr: `.kind == "workflow_run" or .headers["x-gitea-event"] == "workflow_job"`, Action: routing.Action{Drop: true}},
	}})
	rec := postSelfManaged(ing, "generic-headers", `{"action":"completed"}`, map[string]string{"X-Gitea-Event": "workflow_run", "X-Gitea-Delivery": "d-ci"})
	if r := routed(t, rec); !r.Dropped {
		t.Fatalf("workflow_run = %+v, want dropped", r)
	}
	if _, q := accepted202(t, postSelfManaged(ing, "generic-headers", `{"action":"opened","issue":{"number":1,"title":"x"}}`,
		map[string]string{"X-Gitea-Event": "issues", "X-Gitea-Delivery": "d-issue"})); q != "forge" {
		t.Fatalf("issues queue = %q, want forge", q)
	}
}

// Handoff: a rule narrows a delivery to one routed endpoint and queue; unmatched deliveries keep the
// full fan-out; revoking the route or shrinking the queue ceiling sends the handoff back to the
// default instead of anywhere the webhook no longer reaches.
func TestSelfManagedRoutingNarrowsFanOutWithinTheGrant(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-handoff", cairnSecret)
	pool2 := secondEndpoint(t, ctx, st, h.ID, "handoff", []string{"inbox", "handoff"})
	if err := st.AddWebhookRoute(ctx, wh.ID, pool2.ID, h.ID); err != nil {
		t.Fatalf("add route: %v", err)
	}
	setWebhookQueues(t, ctx, pool, owner.ID, "inbox", "handoff")
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "to-pool2", Expr: `.artifact.metadata.handoff_to == "pool2"`, Action: routing.Action{Queue: "handoff", Endpoints: []string{pool2.ID}}},
	}})

	handoff := cairnHandoffBody("evt-h1", "please review", "pool2")
	rec := postSelfManaged(ing, "cairn-handoff", handoff, cairnHeaders(handoff, "evt-h1"))
	r := routed(t, rec)
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != pool2.ID || r.Todos[0].Queue != "handoff" || r.ID != r.Todos[0].ID {
		t.Fatalf("handoff = %+v, want one todo on %s/handoff", r, pool2.ID)
	}
	if tr := traceOf(t, ctx, pool, `SELECT routing_trace FROM todos WHERE id = $1`, r.Todos[0].ID); tr.RuleID != "to-pool2" || tr.Stage != routing.StageRule {
		t.Fatalf("handoff trace = %+v", tr)
	}

	other := cairnBody("evt-h2", time.Now(), "notes")
	rec = postSelfManaged(ing, "cairn-handoff", other, cairnHeaders(other, "evt-h2"))
	if r := routed(t, rec); len(r.Todos) != 2 || r.Todos[0].EndpointID != owner.ID || r.Todos[0].Queue != "inbox" {
		t.Fatalf("unmatched = %+v, want inbox on both targets, owner first", r)
	}

	// Revoke the route: the rule still matches, but pool2 is no longer a target.
	if err := st.RemoveWebhookRoute(ctx, wh.ID, pool2.ID); err != nil {
		t.Fatalf("remove route: %v", err)
	}
	revoked := cairnHandoffBody("evt-h3", "please review", "pool2")
	rec = postSelfManaged(ing, "cairn-handoff", revoked, cairnHeaders(revoked, "evt-h3"))
	r = routed(t, rec)
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != owner.ID || r.Todos[0].Queue != "inbox" {
		t.Fatalf("after revoke = %+v, want the default on the owner only", r)
	}
	if tr := traceOf(t, ctx, pool, `SELECT routing_trace FROM todos WHERE id = $1`, r.Todos[0].ID); tr.Cause != routing.CauseRuleNotGranted || tr.RuleID != "to-pool2" {
		t.Fatalf("after revoke trace = %+v, want %s naming the rule", tr, routing.CauseRuleNotGranted)
	}

	// Issue #270: shrinking one endpoint's ceiling does NOT shrink the owner's allowed
	// queues while another active endpoint still grants the queue. pool2 has scope_queues
	// ["inbox", "handoff"], so the owner's grant remains ["inbox", "handoff"] and a rule
	// to "handoff" continues to route there (SPEC-0020 REQ "Rule Validation at Save Time"
	// plus the fix: vending an endpoint for queue Q demonstrably grants the owner Q).
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "to-handoff", Expr: `true`, Action: routing.Action{Queue: "handoff"}},
	}})
	setWebhookQueues(t, ctx, pool, owner.ID, "inbox")
	cep := cairnBody("evt-h4", time.Now(), "anything")
	if _, q := accepted202(t, postSelfManaged(ing, "cairn-handoff", cep, cairnHeaders(cep, "evt-h4"))); q != "handoff" {
		t.Fatalf("after ceiling shrink queue = %q, want handoff (grant includes pool2 scope_queues)", q)
	}
	// Revoke pool2: now NO active endpoint grants "handoff", so the grant shrinks to ["inbox"]
	// and the same rule falls through to the default on the owner.
	if err := st.RevokeEndpoint(ctx, pool2.ID, h.ID); err != nil {
		t.Fatalf("revoke pool2: %v", err)
	}
	after := cairnBody("evt-h5", time.Now(), "anything")
	if _, q := accepted202(t, postSelfManaged(ing, "cairn-handoff", after, cairnHeaders(after, "evt-h5"))); q != "inbox" {
		t.Fatalf("after revoke pool2 queue = %q, want inbox (owner grant now only inbox)", q)
	}
}

// A rule row naming another tenant's endpoint — written straight to the store, as if validation had
// been bypassed — still cannot deliver there: evaluation intersects with the webhook's own targets.
func TestSelfManagedRoutingCannotReachAnotherTenant(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "inbox", "cairn-tenant", cairnSecret)
	_, stranger := seedEndpoint(t, st, ctx, "stranger", []string{"inbox"})
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "escape", Expr: `true`, Action: routing.Action{Queue: "inbox", Endpoints: []string{stranger.ID}}},
	}, Default: &routing.Action{Queue: "inbox", Endpoints: []string{stranger.ID}}})

	body := cairnBody("evt-escape", time.Now(), "exfiltrate")
	rec := postSelfManaged(ing, "cairn-tenant", body, cairnHeaders(body, "evt-escape"))
	r := routed(t, rec)
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != owner.ID {
		t.Fatalf("escape attempt = %+v, want the webhook's own target only", r)
	}
	if n := countRows(t, ctx, pool, fmt.Sprintf(`SELECT count(*) FROM todos WHERE endpoint_id = '%s'`, stranger.ID)); n != 0 {
		t.Fatalf("stranger received %d todos, want 0", n)
	}
	// Both the rule and the default named the stranger, so the delivery fell through to the webhook's
	// own target queue on its own targets; the trace keeps the matched rule and names the last miss.
	if tr := traceOf(t, ctx, pool, `SELECT routing_trace FROM todos WHERE id = $1`, r.Todos[0].ID); tr.Cause != routing.CauseDefaultNotGranted || tr.RuleID != "escape" {
		t.Fatalf("trace = %+v, want %s naming the escape rule", tr, routing.CauseDefaultNotGranted)
	}
}
