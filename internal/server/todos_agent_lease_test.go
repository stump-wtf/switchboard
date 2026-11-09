// End-to-end coverage for the SPEC-0034 REQ-6 Board exemption through the real router +
// PostgreSQL: the owning human releases and fails a lease their own agent holds, and the attempt
// closes released / failed as REQ-3 tables it. The seeded claim is fenced, so the exemption is
// proven on the lease an agent most strictly cannot close, not only an unfenced one. A human who
// owns no endpoint of the tenant gets an existence-hiding 404 and touches nothing. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL, matching the store test pattern.
// Governing: SPEC-0034 REQ-6 "Lease Token Fence" (Board exemption), REQ-3 outcome table; issue #513.
package server

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

func TestBoardReleasesAndFailsAgentHeldLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	h, token := mintSession(t, st, ctx, "req6-op", "Op", "req6@example.com")
	ep := seedEndpoint(t, st, ctx, h.ID, "req6-agent", "hash-req6", "sbk_req60")
	agentOwner := "agent:" + ep.AgentID

	newTodo := func(external, title string) string {
		t.Helper()
		td, _, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: ep.ID, Queue: "reviews", Source: "github", Kind: "push", Title: title, IdempotencyKey: external})
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		return td.ID
	}

	// claimFenced claims as the agent with a lease token, the way a require_fence claim does: the
	// store keeps only the token's SHA-256, so an unfenced close by the agent itself is a conflict.
	claimFenced := func(id string) {
		t.Helper()
		sum := sha256.Sum256([]byte("lease-token-" + id))
		_, ca, err := st.ClaimTodoWith(ctx, ep.ID, id, agentOwner, store.ClaimOpts{TTL: time.Hour, Claimant: "harness/box/req6", TokenHash: sum[:]})
		if err != nil || ca.Seq != 1 {
			t.Fatalf("fenced claim %s: seq=%d err=%v", id, ca.Seq, err)
		}
	}

	latestAttempt := func(id string) store.Attempt {
		t.Helper()
		ats, _, _, err := st.TodoAttemptsOperatorOwned(ctx, h.ID, id, 10)
		if err != nil || len(ats) == 0 {
			t.Fatalf("read attempts %s: n=%d err=%v", id, len(ats), err)
		}
		return ats[0]
	}

	// The fence is armed on the seeded claim: the agent cannot close its own fenced attempt.
	releaseID := newTodo("d-req6-release", "req6 release")
	claimFenced(releaseID)
	if _, err := st.CompleteTodo(ctx, ep.ID, releaseID, agentOwner, []byte(`{}`)); err != store.ErrConflict {
		t.Fatalf("unfenced complete on a fenced claim = %v, want ErrConflict", err)
	}

	page := getAs(t, r, token, "/todos")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /todos: %d", page.Code)
	}
	csrf := scrapeCSRF(t, page.Body.String())

	// Release the agent's fenced lease from the drawer (the drawer's HX-Target): 200, and the
	// refreshed drawer shows the attempt closed released → requeued.
	rec := postAs(t, r, token, csrf, "sb-overlay", "/todos/"+releaseID+"/release")
	if rec.Code != http.StatusOK {
		body := rec.Body.String()
		t.Fatalf("POST release: %d, want 200: %.300s", rec.Code, body)
	}
	if !strings.Contains(rec.Body.String(), "released → requeued") {
		t.Errorf("drawer after release should show the closed attempt: %.300s", rec.Body.String())
	}
	if got := latestAttempt(releaseID); got.Outcome != "released" || got.Disposition != "requeued" {
		t.Fatalf("release closed the attempt as %s/%s, want released/requeued", got.Outcome, got.Disposition)
	}
	if it, err := st.GetTodoOperatorOwned(ctx, h.ID, releaseID); err != nil || it.State != "pending" {
		t.Fatalf("after release: state=%s err=%v, want pending", it.State, err)
	}

	// Fail the same shape on a second fenced claim: 200, the attempt closed failed with the retry
	// scheduled, and the todo in backoff.
	failID := newTodo("d-req6-fail", "req6 fail")
	claimFenced(failID)
	rec = postAs(t, r, token, csrf, "sb-overlay", "/todos/"+failID+"/fail")
	if rec.Code != http.StatusOK {
		body := rec.Body.String()
		t.Fatalf("POST fail: %d, want 200: %.300s", rec.Code, body)
	}
	if !strings.Contains(rec.Body.String(), "failed → retry_scheduled") {
		t.Errorf("drawer after fail should show the closed attempt: %.300s", rec.Body.String())
	}
	if got := latestAttempt(failID); got.Outcome != "failed" || got.Disposition != "retry_scheduled" {
		t.Fatalf("fail closed the attempt as %s/%s, want failed/retry_scheduled", got.Outcome, got.Disposition)
	}
	if it, err := st.GetTodoOperatorOwned(ctx, h.ID, failID); err != nil || it.State != "failed" || it.NextRetryAt == nil {
		t.Fatalf("after fail: state=%s nextRetry=%v err=%v, want failed in backoff", it.State, it.NextRetryAt, err)
	}

	// A human who owns no endpoint of the tenant gets an existence-hiding 404, and the row is
	// untouched.
	_, strangerToken := mintSession(t, st, ctx, "req6-stranger", "S", "stranger@example.com")
	spage := getAs(t, r, strangerToken, "/todos")
	if spage.Code != http.StatusOK {
		t.Fatalf("stranger GET /todos: %d", spage.Code)
	}
	scsrf := scrapeCSRF(t, spage.Body.String())
	foreignID := newTodo("d-req6-foreign", "req6 foreign")
	claimFenced(foreignID)
	rec = postAs(t, r, strangerToken, scsrf, "sb-overlay", "/todos/"+foreignID+"/release")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign release: %d, want 404", rec.Code)
	}
	if it, err := st.GetTodoOperatorOwned(ctx, h.ID, foreignID); err != nil || it.State != "claimed" || it.Owner != agentOwner {
		t.Fatalf("foreign release touched the row: state=%s owner=%s err=%v", it.State, it.Owner, err)
	}

	// No regression on the lease the human took from the Board: claim it via the Board, then
	// release it the same way.
	ownID := newTodo("d-req6-own", "req6 own")
	rec = postAs(t, r, token, csrf, "sb-overlay", "/todos/"+ownID+"/claim")
	if rec.Code != http.StatusOK {
		t.Fatalf("board claim: %d, want 200", rec.Code)
	}
	rec = postAs(t, r, token, csrf, "sb-overlay", "/todos/"+ownID+"/release")
	if rec.Code != http.StatusOK {
		t.Fatalf("board release: %d, want 200", rec.Code)
	}
	if got := latestAttempt(ownID); got.Outcome != "released" || got.Disposition != "requeued" {
		t.Fatalf("board lease release closed the attempt as %s/%s, want released/requeued", got.Outcome, got.Disposition)
	}
}
