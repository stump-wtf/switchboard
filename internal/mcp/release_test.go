package mcp

// release over MCP
//
// The fake-store half pins what lives in the MCP layer: release registers only behind its own
// grant, attemptReport accepts exactly the REQ-5 artifact forms, a refused call leaves the todo
// claimed, and no summary or artifact text reaches a log line. The real-database half drives the
// verb against Postgres, where the attempt row, the truncation flag, the owner predicate, the fence
// and the endpoint scope are the store's to prove.
//
// Governing: SPEC-0034 REQ-5 "Summary, Artifact and Claimant Inputs", REQ-9 "The release Verb",
// REQ-19 "Error Handling Standards"; REQ-6 (the fence), REQ-10 (tenant isolation).
//
// @joestump-agent 09/25/2026 - Added for #328 (epic #313).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

// drainWithoutRelease is every drain verb but release: none of them may imply it.
var drainWithoutRelease = []string{"list_todos", "get_todo", "claim", "claim_next", "complete", "fail", "heartbeat"}

// releaseSession connects a session with the given verbs whose Handler logs, at debug level, into
// the returned buffer.
func releaseSession(t *testing.T, ctx context.Context, f *fakeStore, verbs []string) (*sdk.ClientSession, *syncBuffer) {
	t.Helper()
	token := vend(t, f, defaultTestSlug, []string{"reviews"}, verbs)
	logs := &syncBuffer{}
	h := New(f, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	cs, err := connect(t, ctx, ts.URL+"/mcp/"+defaultTestSlug, token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, logs
}

// claimedTodo is a todo this test endpoint holds.
func claimedTodo(id string) store.Todo {
	lease := time.Now().Add(time.Minute)
	return store.Todo{ID: id, EndpointID: defaultTestEndpointID, Queue: "reviews", Title: id,
		State: "claimed", Owner: "agent:ag-1", Attempt: 1, LeaseExpiresAt: &lease}
}

// REQ-9: release is not implied by any other drain verb. Without its grant it is neither advertised
// nor callable (forbidden, the held todo untouched); with it, it is advertised with the report and
// fence arguments.
func TestReleaseNeedsItsOwnGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(claimedTodo("td_held"))
	cs, _ := releaseSession(t, ctx, f, drainWithoutRelease)
	if _, ok := toolNames(t, ctx, cs)["release"]; ok {
		t.Fatal("an endpoint holding every other drain verb advertises release")
	}
	callErr(t, ctx, cs, "release", map[string]any{"id": "td_held", "summary": "daemon stopping"}, codeForbidden)
	f.mu.Lock()
	got := f.todos["td_held"]
	f.mu.Unlock()
	if got.State != "claimed" || got.Owner != "agent:ag-1" || got.Attempt != 1 || got.LeaseExpiresAt == nil {
		t.Fatalf("a forbidden release changed the todo: %+v", got)
	}

	cs, _ = releaseSession(t, ctx, newFakeStore(), []string{"release"})
	tool := toolNames(t, ctx, cs)["release"]
	if tool == nil {
		t.Fatal("an endpoint granted release does not advertise it")
	}
	in := callRawSchema(t, tool)
	for _, field := range []string{`"id"`, `"summary"`, `"artifact"`, `"lease_token"`} {
		if !strings.Contains(in, field) {
			t.Errorf("release input schema lacks %s: %s", field, in)
		}
	}
	if strings.Contains(in, `"result"`) {
		t.Errorf("release input schema takes a result, which a release has no use for: %s", in)
	}
}

// callRawSchema renders a tool's input schema as JSON text.
func callRawSchema(t *testing.T, tool *sdk.Tool) string {
	t.Helper()
	b, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal %s input schema: %v", tool.Name, err)
	}
	return string(b)
}

// REQ-5: attemptReport accepts an mcp://cairn handle or an absolute https URL of at most 512 bytes,
// and refuses everything else with invalid, naming the argument and never echoing the value. The
// summary and the token hash pass through untouched (the store clips the summary itself).
func TestAttemptReportArtifactRules(t *testing.T) {
	good := []string{
		"",
		"mcp://cairn/Ab12Cd34",
		"mcp://cairn/a_b-C",
		"mcp://cairn/" + strings.Repeat("x", 64),
		"https://ci.example.com/runs/7",
		"https://example.com",
		"https://example.com/" + strings.Repeat("p", store.AttemptArtifactMax-len("https://example.com/")),
	}
	for _, a := range good {
		r, err := attemptReport("s", a, "tok")
		if err != nil {
			t.Errorf("attemptReport(artifact %q) = %v, want accepted", a, err)
			continue
		}
		if r.Artifact != a || r.Summary != "s" || string(r.TokenHash) != string(leaseTokenHash("tok")) {
			t.Errorf("attemptReport(artifact %q) = %+v, want the inputs passed through", a, r)
		}
	}

	long := strings.Repeat("s", store.AttemptSummaryMax*2)
	if r, err := attemptReport(long, "", ""); err != nil || r.Summary != long || r.TokenHash != nil {
		t.Errorf("attemptReport(long summary) = (%d bytes, %v, %v), want it unclipped for the store to cut", len(r.Summary), r.TokenHash, err)
	}

	bad := []string{
		"file:///etc/passwd",
		"http://example.com/x",
		"https://",
		"https:///path-only",
		"https://:443/x",
		"//example.com/x",
		"/relative/path",
		"example.com/x",
		"javascript:alert(1)",
		"mcp://cairn/",
		"mcp://cairn/has space",
		"mcp://cairn/Ab12/../../x",
		"mcp://cairn/" + strings.Repeat("x", 65),
		"mcp://other/Ab12",
		"https://example.com/" + strings.Repeat("p", store.AttemptArtifactMax),
	}
	for _, a := range bad {
		_, err := attemptReport("s", a, "")
		var te *toolError
		if !errors.As(err, &te) || te.code != codeInvalid || !strings.Contains(te.msg, "artifact") {
			t.Errorf("attemptReport(artifact %q) = %v, want invalid naming artifact", a, err)
			continue
		}
		// One fixed message for every bad value, so none is echoed back.
		if te.msg != errInvalidArtifact.msg {
			t.Errorf("attemptReport(artifact %q) message = %q, want the fixed %q", a, te.msg, errInvalidArtifact.msg)
		}
	}
}

// REQ-5 over MCP: a malformed artifact is invalid, naming the argument, and no transition applies;
// the same call with a valid artifact releases.
func TestReleaseInvalidArtifactLeavesTodoClaimed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	f.putTodo(claimedTodo("td_held"))
	cs, _ := releaseSession(t, ctx, f, []string{"release"})

	msg := callErr(t, ctx, cs, "release", map[string]any{"id": "td_held", "artifact": "file:///etc/passwd"}, codeInvalid)
	if !strings.Contains(msg, "artifact") {
		t.Fatalf("invalid error %q does not name the artifact argument", msg)
	}
	if got := f.todoState("td_held"); got != "claimed" {
		t.Fatalf("an invalid release moved the todo to %s", got)
	}
	var out todoOut
	callOK(t, ctx, cs, "release", map[string]any{"id": "td_held", "artifact": "https://ci.example.com/runs/7"}, &out)
	if out.State != "pending" || out.Owner != "" || out.LeaseExpiresAt != "" || out.Attempt != 1 {
		t.Fatalf("release = %+v, want pending with no owner or lease and attempt 1", out)
	}
}

// REQ-19: no log line carries a summary or an artifact, on success, on an invalid call, on a
// forbidden call, or on a store failure.
func TestReleaseLogsNoSummaryOrArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const (
		summary  = "summary-marker-7f3a"
		artifact = "mcp://cairn/artifactMarker9c1e"
		badMark  = "bad-artifact-marker-44d0"
	)
	f := newFakeStore()
	f.putTodo(claimedTodo("td_ok"))
	f.putTodo(claimedTodo("td_bad"))
	f.putTodo(store.Todo{ID: "td_other_queue", EndpointID: defaultTestEndpointID, Queue: "deploys",
		Title: "not granted", State: "claimed", Owner: "agent:ag-1", Attempt: 1})
	cs, logs := releaseSession(t, ctx, f, []string{"release"})

	var out todoOut
	callOK(t, ctx, cs, "release", map[string]any{"id": "td_ok", "summary": summary, "artifact": artifact}, &out)
	callErr(t, ctx, cs, "release", map[string]any{"id": "td_bad", "summary": summary, "artifact": "file:///" + badMark}, codeInvalid)
	callErr(t, ctx, cs, "release", map[string]any{"id": "td_other_queue", "summary": summary, "artifact": artifact}, codeForbidden)
	callErr(t, ctx, cs, "release", map[string]any{"id": "td_ok", "summary": summary, "artifact": artifact}, codeConflict)
	f.mu.Lock()
	f.failErr = errors.New("db down")
	f.mu.Unlock()
	callErr(t, ctx, cs, "release", map[string]any{"id": "td_bad", "summary": summary, "artifact": "https://" + badMark + ".example"}, codeInternal)

	text := logs.String()
	if text == "" {
		t.Fatal("captured no log output at all; the grep below proves nothing")
	}
	for _, marker := range []string{summary, "artifactMarker9c1e", badMark} {
		if strings.Contains(text, marker) {
			t.Fatalf("a release input reached the log (%s):\n%s", marker, text)
		}
	}
}

// seedReleaseTodo mints a pending todo on an endpoint's "reviews" queue.
func seedReleaseTodo(t *testing.T, ctx context.Context, st *store.Store, endpointID, key string) string {
	t.Helper()
	td, _, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: endpointID, Queue: "reviews",
		Title: key, Payload: []byte(`{}`), IdempotencyKey: key})
	if err != nil {
		t.Fatalf("seed todo %s: %v", key, err)
	}
	return td.ID
}

// newestAttempt reads a todo through get_todo and returns it with its newest attempt.
func newestAttempt(t *testing.T, ctx context.Context, cs *sdk.ClientSession, id string) (getTodoOut, attemptOut) {
	t.Helper()
	var got getTodoOut
	callOK(t, ctx, cs, "get_todo", map[string]any{"id": id}, &got)
	if len(got.Attempts) == 0 {
		t.Fatalf("todo %s has no attempts", id)
	}
	return got, got.Attempts[0]
}

// REQ-9 and REQ-5 against Postgres: release on shutdown, a long summary, a malformed artifact, the
// fence, and the owner and endpoint predicates. Endpoint A holds claim, release and get_todo; B
// (another human's) holds release; N (A's second agent) holds every drain verb but release.
func TestReleaseOverRealStore(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)

	epA, csA := getTodoDBSession(t, ctx, f, f.agentA1, "release-a-12121212", []string{"claim", "release", "get_todo"})
	_, csB := getTodoDBSession(t, ctx, f, f.agentB, "release-b-34343434", []string{"release", "get_todo"})
	epN, csN := getTodoDBSession(t, ctx, f, f.agentA2, "release-n-56565656", drainWithoutRelease)

	t.Run("release on shutdown", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epA, "release-shutdown")
		var claimed claimOut
		callOK(t, ctx, csA, "claim", map[string]any{"id": id}, &claimed)
		var out todoOut
		callOK(t, ctx, csA, "release", map[string]any{"id": id, "summary": "daemon stopping",
			"artifact": "mcp://cairn/Ab12Cd34"}, &out)
		if out.State != "pending" || out.Owner != "" || out.LeaseExpiresAt != "" || out.Attempt != claimed.Attempt ||
			out.NextRetryAt != nil || out.DeadLetter {
			t.Fatalf("release = %+v, want pending, no owner or lease, attempt %d, no retry", out, claimed.Attempt)
		}
		got, a := newestAttempt(t, ctx, csA, id)
		if got.State != "pending" || got.Attempt != claimed.Attempt || got.AttemptsTotal != 1 {
			t.Fatalf("get_todo state=%s attempt=%d total=%d, want pending/%d/1", got.State, got.Attempt, got.AttemptsTotal, claimed.Attempt)
		}
		if a.Seq != 1 || a.Outcome == nil || *a.Outcome != "released" || a.Disposition == nil || *a.Disposition != "requeued" ||
			a.EndedAt == nil || a.Died || a.Summary == nil || *a.Summary != "daemon stopping" || a.SummaryTruncated ||
			a.Artifact == nil || *a.Artifact != "mcp://cairn/Ab12Cd34" {
			t.Fatalf("attempt = %+v, want seq 1 released/requeued with the summary and artifact", a)
		}
	})

	t.Run("long summary is truncated", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epA, "release-long")
		var claimed claimOut
		callOK(t, ctx, csA, "claim", map[string]any{"id": id}, &claimed)
		long := "a" + strings.Repeat("é", 1500) // 3001 bytes; byte 2048 falls inside a rune
		var out todoOut
		callOK(t, ctx, csA, "release", map[string]any{"id": id, "summary": long}, &out)
		_, a := newestAttempt(t, ctx, csA, id)
		if !a.SummaryTruncated || a.Summary == nil || len(*a.Summary) > store.AttemptSummaryMax ||
			len(*a.Summary) < store.AttemptSummaryMax-3 || !utf8.ValidString(*a.Summary) || !strings.HasPrefix(long, *a.Summary) {
			t.Fatalf("summary_truncated=%v summary len=%d, want a valid UTF-8 prefix of at most %d bytes, flagged",
				a.SummaryTruncated, len(derefStr(a.Summary)), store.AttemptSummaryMax)
		}
	})

	t.Run("invalid artifact leaves the todo claimed", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epA, "release-invalid")
		var claimed claimOut
		callOK(t, ctx, csA, "claim", map[string]any{"id": id}, &claimed)
		msg := callErr(t, ctx, csA, "release", map[string]any{"id": id, "summary": "x", "artifact": "file:///etc/passwd"}, codeInvalid)
		if !strings.Contains(msg, "artifact") {
			t.Fatalf("invalid error %q does not name the artifact argument", msg)
		}
		got, a := newestAttempt(t, ctx, csA, id)
		if got.State != "claimed" || got.Owner != claimed.Owner || got.AttemptsTotal != 1 || a.Outcome != nil || a.EndedAt != nil {
			t.Fatalf("after an invalid release: state=%s owner=%s total=%d attempt=%+v, want the claim and its open attempt untouched",
				got.State, got.Owner, got.AttemptsTotal, a)
		}
	})

	t.Run("fenced attempt needs its token", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epA, "release-fenced")
		var claimed claimOut
		callOK(t, ctx, csA, "claim", map[string]any{"id": id, "require_fence": true}, &claimed)
		assertLeaseToken(t, claimed.LeaseToken)
		callErr(t, ctx, csA, "release", map[string]any{"id": id}, codeConflict)
		callErr(t, ctx, csA, "release", map[string]any{"id": id, "lease_token": "wrong"}, codeConflict)
		if got, _ := newestAttempt(t, ctx, csA, id); got.State != "claimed" {
			t.Fatalf("a fence miss moved the todo to %s", got.State)
		}
		var out todoOut
		callOK(t, ctx, csA, "release", map[string]any{"id": id, "lease_token": claimed.LeaseToken}, &out)
		if _, a := newestAttempt(t, ctx, csA, id); out.State != "pending" || a.Outcome == nil || *a.Outcome != "released" {
			t.Fatalf("the holder's release = %s / %+v, want pending and the attempt released", out.State, a)
		}
	})

	t.Run("non-owner is conflict", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epA, "release-nonowner")
		if _, err := f.st.ClaimTodo(ctx, epA, id, "agent:someone-else", time.Minute); err != nil {
			t.Fatalf("claim as another owner: %v", err)
		}
		callErr(t, ctx, csA, "release", map[string]any{"id": id, "summary": "not mine"}, codeConflict)
		got, a := newestAttempt(t, ctx, csA, id)
		if got.State != "claimed" || got.Owner != "agent:someone-else" || a.Outcome != nil {
			t.Fatalf("a non-owner release changed the todo: state=%s owner=%s attempt=%+v", got.State, got.Owner, a)
		}
		pending := seedReleaseTodo(t, ctx, f.st, epA, "release-pending")
		callErr(t, ctx, csA, "release", map[string]any{"id": pending}, codeConflict)
	})

	t.Run("another endpoint's todo is not_found", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epA, "release-foreign")
		var claimed claimOut
		callOK(t, ctx, csA, "claim", map[string]any{"id": id}, &claimed)
		random := callRaw(t, ctx, csB, "release", map[string]any{"id": "td_never_minted_0000"})
		if !strings.Contains(random, codeNotFound+":") {
			t.Fatalf("never-minted id = %s, want not_found", random)
		}
		if foreign := callRaw(t, ctx, csB, "release", map[string]any{"id": id, "summary": "theirs"}); foreign != random {
			t.Fatalf("B's release of A's todo = %s\nwant the never-minted bytes %s", foreign, random)
		}
		if got, a := newestAttempt(t, ctx, csA, id); got.State != "claimed" || a.Outcome != nil {
			t.Fatalf("B's release changed A's todo: state=%s attempt=%+v", got.State, a)
		}
	})

	t.Run("without the grant is forbidden", func(t *testing.T) {
		id := seedReleaseTodo(t, ctx, f.st, epN, "release-nogrant")
		var claimed claimOut
		callOK(t, ctx, csN, "claim", map[string]any{"id": id}, &claimed)
		callErr(t, ctx, csN, "release", map[string]any{"id": id, "summary": "daemon stopping"}, codeForbidden)
		got, a := newestAttempt(t, ctx, csN, id)
		if got.State != "claimed" || got.Owner != claimed.Owner || got.Attempt != claimed.Attempt ||
			got.AttemptsTotal != 1 || a.Outcome != nil || a.Summary != nil {
			t.Fatalf("a forbidden release changed the todo: state=%s attempt=%d total=%d open=%+v",
				got.State, got.Attempt, got.AttemptsTotal, a)
		}
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
