package server

// Operator hand-off over the API: POST /api/v1/endpoints/{ref}/todos (ADR-0040). Skipped without
// SWITCHBOARD_TEST_DATABASE_URL like every DB-backed suite. Governing: SPEC-0011 scenario
// "Operator-authored todo is pushed"; ADR-0022 (owner-only, queue inside the vended scope).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

// pushVia POSTs the hand-off route as the given operator bearer and returns status + decoded body.
// A string body is sent verbatim (for malformed-JSON cases); anything else is JSON-encoded.
func pushVia(t *testing.T, r http.Handler, bearer, ref string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	case nil:
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/"+ref+"/todos", &buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc
}

func TestAPIPushTodoMintsAVerifiedOperatorTodoAndRings(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, _ := mintSession(t, st, ctx, "test|push", "Joe Stump", "joe@example.com")
	bearer := mintOperatorBearer(t, st, ctx, human.ID, "cid-push")
	ep := consentFixture(t, st, ctx, human.ID, "pusher", "pusher-ab12cd34", "cid-fixture-push")

	var mu sync.Mutex
	var rung []store.Todo
	st.SetTodoDoorbellHook(func(td store.Todo) {
		mu.Lock()
		defer mu.Unlock()
		rung = append(rung, td)
	})

	code, doc := pushVia(t, r, bearer, ep.Slug, map[string]any{
		"title": "look at PR 7", "queue": "ci", "payload": map[string]any{"pr": 7}, "key": "pr-7"})
	if code != http.StatusCreated {
		t.Fatalf("push: got %d, want 201 (body %v)", code, doc)
	}
	id, _ := doc["id"].(string)
	if id == "" || doc["created"] != true || doc["queue"] != "ci" || doc["slug"] != ep.Slug || doc["state"] != "pending" {
		t.Fatalf("push response = %v, want a fresh pending todo on ci", doc)
	}

	// Pinned to the endpoint, carrying the operator's payload, born from a verified operator event
	// that names the human — the ordinary shape every downstream reader already understands.
	td, err := st.GetTodo(ctx, ep.ID, id)
	if err != nil {
		t.Fatalf("get todo: %v", err)
	}
	if td.EndpointID != ep.ID || td.Queue != "ci" || td.Source != "operator" || td.Kind != "operator" || td.Title != "look at PR 7" {
		t.Fatalf("todo = %+v, want an operator todo pinned to %s", td, ep.ID)
	}
	if !bytes.Contains(td.Payload, []byte(`"pr"`)) {
		t.Fatalf("payload = %s, want the operator's JSON", td.Payload)
	}
	if td.EventID == nil {
		t.Fatal("operator todo must be backed by a delivery event")
	}
	ev, err := st.EventHistoryByID(ctx, *td.EventID)
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev.Provider != "operator" || ev.TrustMode != "operator" || !ev.Verified {
		t.Fatalf("event = %+v, want a verified operator delivery", ev)
	}
	if !strings.Contains(ev.VerifyDetail, "Joe Stump") || !strings.Contains(ev.VerifyDetail, human.ID) {
		t.Fatalf("verify_detail = %q, want the operator named", ev.VerifyDetail)
	}
	if ev.ExternalID != ep.ID+":pr-7" {
		t.Fatalf("external_id = %q, want the key scoped to the endpoint", ev.ExternalID)
	}

	// The doorbell rang exactly once, for this todo.
	mu.Lock()
	n, first := len(rung), rung
	mu.Unlock()
	if n != 1 || first[0].ID != id {
		t.Fatalf("doorbell hook fired %d times (%v), want once for %s", n, first, id)
	}

	// The same key again: the existing todo comes back, nothing is minted, nothing rings.
	code, again := pushVia(t, r, bearer, ep.Slug, map[string]any{"title": "look at PR 7 (again)", "queue": "ci", "key": "pr-7"})
	if code != http.StatusCreated || again["id"] != id || again["created"] != false {
		t.Fatalf("repeat push = %d %v, want the existing todo with created:false", code, again)
	}
	mu.Lock()
	n = len(rung)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("repeat push rang the doorbell (%d rings)", n)
	}

	// Keys are scoped to the endpoint: the same key on another endpoint of the same human mints
	// that endpoint's own todo (ADR-0022).
	ep2 := consentFixture(t, st, ctx, human.ID, "pusher-two", "pusher-two-ab12cd34", "cid-fixture-push-2")
	code, other := pushVia(t, r, bearer, ep2.ID, map[string]any{"title": "look at PR 7", "queue": "ci", "key": "pr-7"})
	if code != http.StatusCreated || other["created"] != true || other["id"] == id || other["slug"] != ep2.Slug {
		t.Fatalf("push by id to a second endpoint = %d %v, want its own fresh todo", code, other)
	}

	// Without a key every push is new work.
	code, a := pushVia(t, r, bearer, ep.Slug, map[string]any{"title": "one", "queue": "github"})
	_, b := pushVia(t, r, bearer, ep.Slug, map[string]any{"title": "one", "queue": "github"})
	if code != http.StatusCreated || a["id"] == b["id"] {
		t.Fatalf("keyless pushes must each mint a todo: %v vs %v", a, b)
	}
}

func TestAPIPushTodoRefusesWhatItMust(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, _ := mintSession(t, st, ctx, "test|push-refuse", "Joe Stump", "joe@example.com")
	bearer := mintOperatorBearer(t, st, ctx, human.ID, "cid-push-refuse")
	ep := consentFixture(t, st, ctx, human.ID, "guarded", "guarded-ab12cd34", "cid-fixture-guarded")
	other, _ := mintSession(t, st, ctx, "test|push-other", "Someone Else", "else@example.com")
	otherBearer := mintOperatorBearer(t, st, ctx, other.ID, "cid-push-other")

	okBody := map[string]any{"title": "x", "queue": "ci"}
	cases := []struct {
		name   string
		bearer string
		ref    string
		body   any
		want   int
	}{
		{"no bearer", "", ep.Slug, okBody, http.StatusUnauthorized},
		{"another operator's endpoint", otherBearer, ep.Slug, okBody, http.StatusNotFound},
		{"unknown endpoint", bearer, "no-such-endpoint", okBody, http.StatusNotFound},
		{"malformed body", bearer, ep.Slug, `{"title": `, http.StatusBadRequest},
		{"empty title", bearer, ep.Slug, map[string]any{"title": "   ", "queue": "ci"}, http.StatusBadRequest},
		{"title too long", bearer, ep.Slug, map[string]any{"title": strings.Repeat("x", maxPushTitle+1), "queue": "ci"}, http.StatusBadRequest},
		{"queue required when the endpoint drains several", bearer, ep.Slug, map[string]any{"title": "x"}, http.StatusBadRequest},
		{"queue outside the vended scope", bearer, ep.Slug, map[string]any{"title": "x", "queue": "deploys"}, http.StatusBadRequest},
		{"key too long", bearer, ep.Slug, map[string]any{"title": "x", "queue": "ci", "key": strings.Repeat("k", maxPushKey+1)}, http.StatusBadRequest},
		// The route caps the body at 64 KiB: an over-cap hand-off is a 413, not a malformed-JSON 400.
		{"oversized body", bearer, ep.Slug, `{"title":"x","queue":"ci","payload":"` + strings.Repeat("p", 64<<10) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, doc := pushVia(t, r, tc.bearer, tc.ref, tc.body)
			if code != tc.want {
				t.Fatalf("got %d, want %d (body %v)", code, tc.want, doc)
			}
		})
	}
	// A revoked endpoint has no session that could ever drain the work: conflict, not a mint.
	if err := st.RevokeEndpoint(ctx, ep.ID, human.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code, doc := pushVia(t, r, bearer, ep.Slug, okBody); code != http.StatusConflict {
		t.Fatalf("push to a revoked endpoint: got %d, want 409 (body %v)", code, doc)
	}
	// None of the above minted anything.
	todos, err := st.ListTodos(ctx, ep.ID, ep.ScopeQueues, "", 50)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	if len(todos) != 0 {
		t.Fatalf("refused pushes minted %d todos", len(todos))
	}
}
