package ingest

// End-to-end tests for handoff work orders and difficulty lanes through the real self-managed
// receiver, with the checked-in fleet rule pack installed exactly as an operator would install it:
// one router endpoint owns signed gitea and cairn webhooks and routes to one pool endpoint per lane,
// each scoped to its own lane queue. The properties under test are the ones only the whole pipeline
// can show — exactly one todo per work order, on the lane's endpoint alone; a relabel routes once and
// never loops; a redelivery never re-mints; a re-size re-routes; untrusted provenance never becomes
// work — plus the work order a worker actually receives.
//
// Governing: ADR-0025, SPEC-0020 REQ "Work Order Trust", REQ "Exclusive Delivery", REQ "At-Most-Once
// Work Orders", REQ "Work Orders".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joestump/switchboard/internal/routing"
	"github.com/joestump/switchboard/internal/store"
)

const giteaLanesSecret = "whsec_gitea_lanes"

var lanesQueues = []string{"triage", "lane-local", "lane-zai-flash", "lane-zai", "lane-hyper", "lane-vision", "hold"}

type lanesWorld struct {
	ing    *Ingest
	hub    *Hub
	pool   *pgxpool.Pool
	ctx    context.Context
	st     *store.Store
	router store.Endpoint
	lanes  map[string]store.Endpoint
	dup    store.Endpoint // a second endpoint scoped to lane-zai-flash, routed LAST
}

func newLanesWorld(t *testing.T) *lanesWorld {
	t.Helper()
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	h, router := seedEndpoint(t, st, ctx, "lanes-router", []string{"router"})
	setWebhookQueues(t, ctx, pool, router.ID, lanesQueues...)
	w := &lanesWorld{ing: ing, hub: hub, pool: pool, ctx: ctx, st: st, router: router, lanes: map[string]store.Endpoint{}}

	raw, err := os.ReadFile("../../docs/routing/rule-packs/fleet.json")
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	var pack routing.Config
	if err := json.Unmarshal(raw, &pack); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	for _, spec := range []struct{ source, token, secret string }{
		{"gitea", "lanes-gitea", giteaLanesSecret}, {"cairn", "lanes-cairn", cairnSecret},
	} {
		wh, err := st.CreateWebhook(ctx, router.ID, spec.source, "triage", "signed", spec.token, spec.secret, 5)
		if err != nil {
			t.Fatalf("create %s webhook: %v", spec.source, err)
		}
		for _, q := range lanesQueues {
			ep, ok := w.lanes[q]
			if !ok {
				ep = secondEndpoint(t, ctx, st, h.ID, "lane-"+q, []string{q})
				w.lanes[q] = ep
			}
			if err := st.AddWebhookRoute(ctx, wh.ID, ep.ID, h.ID); err != nil {
				t.Fatalf("route %s: %v", q, err)
			}
			time.Sleep(time.Millisecond)
		}
		if w.dup.ID == "" {
			w.dup = secondEndpoint(t, ctx, st, h.ID, "lane-dup-zai-flash", []string{"lane-zai-flash"})
		}
		if err := st.AddWebhookRoute(ctx, wh.ID, w.dup.ID, h.ID); err != nil {
			t.Fatalf("route dup: %v", err)
		}
		setRules(t, ctx, st, wh.ID, h.ID, pack)
	}
	return w
}

// sampleBody reads a routing fixture sample and applies a shallow top-level and nested patch.
func sampleBody(t *testing.T, name string, patch func(map[string]any)) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile("../routing/testdata/fleet/samples/" + name + ".json")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	var s struct {
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	if patch != nil {
		patch(s.Body)
	}
	body, err := json.Marshal(s.Body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return s.Headers, string(body)
}

func (w *lanesWorld) postGitea(t *testing.T, delivery string, patch func(map[string]any)) *httptest.ResponseRecorder {
	t.Helper()
	sample, body := sampleBody(t, "gitea-issue-label-updated", patch)
	hdr := map[string]string{
		"X-Gitea-Event": sample["X-Gitea-Event"], "X-Gitea-Event-Type": sample["X-Gitea-Event-Type"],
		"X-Gitea-Delivery": delivery, "X-Hub-Signature-256": githubSig(giteaLanesSecret, body),
	}
	return postSelfManaged(w.ing, "lanes-gitea", body, hdr)
}

func (w *lanesWorld) todosOn(t *testing.T, ep store.Endpoint) []store.Todo {
	t.Helper()
	todos, err := w.st.ListTodos(w.ctx, ep.ID, ep.ScopeQueues, "", 50)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	return todos
}

func (w *lanesWorld) totalTodos(t *testing.T) int {
	t.Helper()
	return countRows(t, w.ctx, w.pool, `SELECT count(*) FROM todos`)
}

func setIssue(action string, labels ...string) func(map[string]any) {
	return func(b map[string]any) {
		b["action"] = action
		issue := b["issue"].(map[string]any)
		ls := make([]any, 0, len(labels))
		for i, l := range labels {
			ls = append(ls, map[string]any{"id": 5000 + i, "name": l})
		}
		issue["labels"] = ls
	}
}

func TestLanesIssueLifecycle(t *testing.T) {
	w := newLanesWorld(t)
	triageBell, cancel := w.hub.Subscribe(w.lanes["triage"].ID, []string{"triage"})
	defer cancel()
	routerBell, cancelRouter := w.hub.Subscribe(w.router.ID, []string{"triage", "router"})
	defer cancelRouter()

	// 1. A trusted, unsized issue is opened: one work order, on the triage pool only.
	r := routed(t, w.postGitea(t, "d-open", setIssue("opened")))
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != w.lanes["triage"].ID || r.Todos[0].Queue != "triage" || w.totalTodos(t) != 1 {
		t.Fatalf("opened = %+v (total %d), want one triage todo on the triage pool", r, w.totalTodos(t))
	}
	if drainHub(triageBell) != 1 || drainHub(routerBell) != 0 {
		t.Fatalf("doorbells: want exactly the triage pool rung")
	}
	triage := w.todosOn(t, w.lanes["triage"])
	var wo routing.WorkOrder
	if err := json.Unmarshal(triage[0].WorkOrder, &wo); err != nil {
		t.Fatalf("work order %s: %v", triage[0].WorkOrder, err)
	}
	if wo.Lane != "triage" || !wo.Verified || wo.TrustMode != "signed" || wo.AuthorizedBy.RuleID != "unsized-new-issue" ||
		wo.Subject == nil || wo.Subject.URL != "https://gitea.stump.rocks/stump.wtf/switchboard/issues/212" ||
		wo.Subject.Author != "joestump" || wo.Authority != routing.WorkOrderAuthority {
		t.Fatalf("triage work order = %+v", wo)
	}

	// 2. Triage sizes it M: the label event routes to zai-flash — to the FIRST scoped pool, not the
	// duplicate routed after it, and not back to triage.
	r = routed(t, w.postGitea(t, "d-size-m", setIssue("label_updated", "size/M")))
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != w.lanes["lane-zai-flash"].ID || len(w.todosOn(t, w.dup)) != 0 {
		t.Fatalf("size/M = %+v, want one todo on the first zai-flash pool and none on the duplicate", r)
	}
	zaiFlashTodo := r.Todos[0].ID

	// 3. Another label change (a worker adds "bug"; size/M still present) is a new delivery about the
	// same issue to the same lane: recorded, not re-run.
	rec := w.postGitea(t, "d-add-bug", setIssue("label_updated", "size/M", "bug"))
	if rr := routed(t, rec); len(rr.Todos) != 0 {
		t.Fatalf("second label event = %s, want a repeat with no todos", rec.Body.String())
	}
	var repeat struct {
		Repeat bool `json:"repeat"`
	}
	decode(t, rec.Body.Bytes(), &repeat)
	if !repeat.Repeat || w.totalTodos(t) != 2 {
		t.Fatalf("repeat flag %v, total todos %d; want repeat and still 2", repeat.Repeat, w.totalTodos(t))
	}

	// 4. Gitea redelivers step 2: the same todo is reported, nothing new.
	r = routed(t, w.postGitea(t, "d-size-m", setIssue("label_updated", "size/M")))
	if len(r.Todos) != 1 || r.Todos[0].ID != zaiFlashTodo || r.Created != 0 || w.totalTodos(t) != 2 {
		t.Fatalf("redelivery = %+v (total %d), want the original todo reported", r, w.totalTodos(t))
	}

	// 5. Removing the size label is an unsized label event: dropped, never a loop back to triage.
	if rr := routed(t, w.postGitea(t, "d-unsize", setIssue("label_updated", "bug"))); !rr.Dropped {
		t.Fatalf("unsized label event = %+v, want dropped", rr)
	}

	// 6. A re-size to L is a different lane: it routes once.
	r = routed(t, w.postGitea(t, "d-size-l", setIssue("label_updated", "size/L")))
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != w.lanes["lane-zai"].ID || w.totalTodos(t) != 3 {
		t.Fatalf("size/L = %+v (total %d)", r, w.totalTodos(t))
	}

	// 7. The same issue text from an untrusted author, or labeled by a stranger, is never work.
	if rr := routed(t, w.postGitea(t, "d-stranger", func(b map[string]any) {
		setIssue("opened")(b)
		b["issue"].(map[string]any)["user"] = map[string]any{"login": "stranger"}
		b["number"], b["issue"].(map[string]any)["number"] = 999, 999
	})); !rr.Dropped {
		t.Fatalf("untrusted author = %+v, want dropped", rr)
	}
	if rr := routed(t, w.postGitea(t, "d-stranger-label", func(b map[string]any) {
		setIssue("label_updated", "size/S")(b)
		b["sender"] = map[string]any{"login": "stranger"}
	})); !rr.Dropped {
		t.Fatalf("stranger labeler = %+v, want dropped", rr)
	}
	if w.totalTodos(t) != 3 {
		t.Fatalf("total todos = %d, want 3", w.totalTodos(t))
	}
}

func TestLanesCairnHandoff(t *testing.T) {
	w := newLanesWorld(t)
	post := func(eventID string, patch func(data map[string]any)) *httptest.ResponseRecorder {
		t.Helper()
		_, body := sampleBody(t, "cairn-artifact-created", func(b map[string]any) {
			b["event_id"] = eventID
			b["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			if patch != nil {
				patch(b["data"].(map[string]any))
			}
		})
		return postSelfManaged(w.ing, "lanes-cairn", body, cairnHeaders(body, eventID))
	}

	r := routed(t, post("evt-handoff-1", nil))
	if len(r.Todos) != 1 || r.Todos[0].EndpointID != w.lanes["lane-zai-flash"].ID {
		t.Fatalf("handoff = %+v, want one todo on the zai-flash pool", r)
	}
	todo := w.todosOn(t, w.lanes["lane-zai-flash"])[0]
	var wo routing.WorkOrder
	if err := json.Unmarshal(todo.WorkOrder, &wo); err != nil {
		t.Fatalf("work order: %v", err)
	}
	if wo.Subject == nil || wo.Subject.Handle != "mcp://cairn/hx7Qm2" || wo.Subject.ActorID != "joestump-agent" ||
		wo.Subject.OnBehalfOf != "joestump" || wo.Lane != "lane-zai-flash" || wo.AuthorizedBy.RuleID != "cairn-lane-zai-flash" {
		t.Fatalf("cairn work order = %+v", wo)
	}

	// The same artifact announced again (a new cairn event id) is not a second work order.
	rec := post("evt-handoff-2", nil)
	var repeat struct {
		Repeat bool `json:"repeat"`
	}
	decode(t, rec.Body.Bytes(), &repeat)
	if rec.Code != http.StatusAccepted || !repeat.Repeat {
		t.Fatalf("second announcement = %d %s, want a repeat", rec.Code, rec.Body.String())
	}

	// Labels are asserted by whoever wrote the artifact: they choose a lane for trusted work and grant
	// nothing to an actor outside the allowlist.
	if rr := routed(t, post("evt-handoff-3", func(d map[string]any) {
		d["id"], d["actor_id"] = "other1", "someone-else"
		d["labels"] = map[string]any{"handoff": "true", "lane": "local", "trusted": "true"}
	})); !rr.Dropped {
		t.Fatalf("untrusted actor = %+v, want dropped", rr)
	}
	if n := countRows(t, w.ctx, w.pool, `SELECT count(*) FROM todos`); n != 1 {
		t.Fatalf("total todos = %d, want 1", n)
	}
}
