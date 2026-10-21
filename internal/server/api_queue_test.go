package server

// Operator-API queue pause (GET /api/v1/queues, POST /api/v1/queues/{queue}/pause|resume).
//
// These tests pin the contract the `switchboard queue` verbs depend on: the pause is visible in the
// listing with its reason, it suppresses claims through the store, resume reports whether it lifted
// anything, and the quarantine queue — already never delivered — refuses a pause. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0048 (proposed), SPEC-0035 REQ "Human API Surface".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

func apiQueueCall(t *testing.T, r http.Handler, method, path, bearer, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc
}

func queueHarness(t *testing.T, label string) (http.Handler, *store.Store, string, store.Endpoint, string, context.Context) {
	t.Helper()
	r, st, ctx := newDBRouter(t)
	human, _ := mintSession(t, st, ctx, "test|queue-"+label, "Joe Stump", "q@example.com")
	bearer := mintOperatorBearer(t, st, ctx, human.ID, "cid-queue-"+label)
	ep := consentFixture(t, st, ctx, human.ID, "queue-"+label, "queue-"+label+"-ab12cd34", "cid-fixture-"+label)
	return r, st, bearer, ep, ep.ScopeQueues[0], ctx
}

func TestAPIQueuePauseLifecycle(t *testing.T) {
	r, st, bearer, ep, queue, ctx := queueHarness(t, "lifecycle")

	// The route is a human surface: no bearer, no answer.
	if code, _ := apiQueueCall(t, r, http.MethodGet, "/api/v1/queues", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ls = %d, want 401", code)
	}

	// Pause with a reason.
	code, doc := apiQueueCall(t, r, http.MethodPost, "/api/v1/queues/"+queue+"/pause", bearer, `{"reason":"draining"}`)
	if code != http.StatusOK || doc["paused"] != true {
		t.Fatalf("pause = (%d, %v), want 200 paused", code, doc)
	}

	// Intake persists behind the pause, and the claim verbs hand out nothing.
	todo, created, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: ep.ID, Queue: queue,
		Title: "held", Payload: []byte(`{}`), IdempotencyKey: "api-qp-1"})
	if err != nil || !created {
		t.Fatalf("intake while paused: created=%v err=%v", created, err)
	}
	if _, err := st.ClaimNext(ctx, ep.ID, []string{queue}, "w", time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim while paused = %v, want ErrNotFound", err)
	}

	// The listing says why nothing is moving.
	code, doc = apiQueueCall(t, r, http.MethodGet, "/api/v1/queues", bearer, "")
	if code != http.StatusOK {
		t.Fatalf("ls = %d, want 200", code)
	}
	row, ok := findQueueRow(doc, queue)
	if !ok || row["paused"] != true || row["reason"] != "draining" {
		t.Fatalf("ls row for %s = %v, want paused with reason", queue, doc)
	}
	if _, stillPending := row["pending"]; !stillPending {
		t.Fatalf("ls row lost its counts: %v", row)
	}

	// Resume lifts the pause and re-rings; the held todo claims.
	code, doc = apiQueueCall(t, r, http.MethodPost, "/api/v1/queues/"+queue+"/resume", bearer, "")
	if code != http.StatusOK || doc["was_paused"] != true {
		t.Fatalf("resume = (%d, %v), want 200 was_paused", code, doc)
	}
	taken, err := st.ClaimNext(ctx, ep.ID, []string{queue}, "w", time.Minute)
	if err != nil || taken.ID != todo.ID {
		t.Fatalf("claim after resume = (%v, %v), want the held todo", taken.ID, err)
	}

	// Resuming an unpaused queue is not an error; it just reports that nothing was lifted.
	if code, doc = apiQueueCall(t, r, http.MethodPost, "/api/v1/queues/"+queue+"/resume", bearer, ""); code != http.StatusOK || doc["was_paused"] != false {
		t.Fatalf("second resume = (%d, %v), want 200 not-paused", code, doc)
	}
}

func TestAPIQueuePauseQuarantineRefused(t *testing.T) {
	r, _, bearer, _, _, _ := queueHarness(t, "quarantine")
	code, doc := apiQueueCall(t, r, http.MethodPost, "/api/v1/queues/quarantine/pause", bearer, "")
	if code != http.StatusBadRequest || doc["code"] != "invalid_argument" {
		t.Fatalf("pause quarantine = (%d, %v), want 400 invalid_argument", code, doc)
	}
}

func findQueueRow(doc map[string]any, queue string) (map[string]any, bool) {
	rows, _ := doc["queues"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["queue"] == queue {
			return row, true
		}
	}
	return nil, false
}
