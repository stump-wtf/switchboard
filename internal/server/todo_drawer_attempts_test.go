package server

// Todo Drawer Attempt History, End to End
//
// Drives GET /todos/{id} against Postgres through the real router: the owning human's drawer shows
// the reaped attempt with its died marker and last heartbeat, and renders an agent's summary as
// text; a second human opening the same URL gets the not-found response, byte-identical to a
// never-minted id, with no attempt data in it.
//
// Governing: SPEC-0034 REQ-10 scenario "Second human on the Board", REQ-13 scenarios "Drawer shows
// a death" and "Summary markup is inert".
//
// @joestump-agent 09/25/2026 - Added for #329 (epic #313).

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stump-wtf/switchboard/internal/store"
)

// Values only alice's attempts carry, so their absence from bob's responses means something.
const (
	aliceClaimant = "ALICE-claimant/fixer-run-2"
	aliceSummary  = `ALICE-SUMMARY <script>alert(1)</script>`
	aliceArtifact = "https://cairn.example.com/a/ALICEart"
)

// seedAttemptHistory gives todo id a reaped attempt 1 (one heartbeat, then silence) and a failed
// attempt 2 with an agent-written summary and artifact, all through the store's lifecycle paths.
func seedAttemptHistory(t *testing.T, f *tenancyFixture, id string) {
	t.Helper()
	const owner = "agent:alice-worker"
	if _, _, err := f.st.ClaimTodoWith(f.ctx, f.epA.ID, id, owner, store.ClaimOpts{TTL: time.Hour, Claimant: "ALICE-claimant/fixer-run-1"}); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	// The heartbeat stamps last_heartbeat_at and leaves a lease that lapses almost at once.
	if _, err := f.st.HeartbeatTodo(f.ctx, f.epA.ID, id, owner, time.Millisecond); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if n, err := f.st.ReapExpired(f.ctx); err != nil || n < 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	if _, _, err := f.st.ClaimTodoWith(f.ctx, f.epA.ID, id, owner, store.ClaimOpts{TTL: time.Hour, Claimant: aliceClaimant}); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if _, err := f.st.FailTodoWith(f.ctx, f.epA.ID, id, owner, store.Report{Summary: aliceSummary, Artifact: aliceArtifact}); err != nil {
		t.Fatalf("fail 2: %v", err)
	}
}

func TestTodoDrawerShowsAttemptHistoryToItsOwnerOnly(t *testing.T) {
	f := newTenancyFixture(t)
	id := f.todoAPending.ID
	seedAttemptHistory(t, f, id)

	// HAPPY: alice's drawer lists both attempts, newest first, with the death marked.
	rec := getHX(t, f.r, f.aliceTok, "/todos/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice GET own drawer: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="sb-attempts-` + id + `" aria-live="polite"`,
		`>attempt history</h2>`,
		`data-sb-attempt="2" data-sb-outcome="failed"`,
		`data-sb-attempt="1" data-sb-outcome="reaped"`,
		`reaped → requeued`,
		`aria-label="died: no report"`,
		`last heartbeat <time datetime=`,
		aliceClaimant,
		`ALICE-SUMMARY &lt;script&gt;alert(1)&lt;/script&gt;`,
		`href="` + aliceArtifact + `" rel="noopener noreferrer"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("alice's drawer missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("an attempt summary rendered as live markup")
	}
	if strings.Index(body, `data-sb-attempt="2"`) > strings.Index(body, `data-sb-attempt="1"`) {
		t.Error("attempts are not newest first")
	}
	// The died marker sits on the reaped attempt, not the failed one.
	failed := body[strings.Index(body, `data-sb-attempt="2"`):strings.Index(body, `data-sb-attempt="1"`)]
	if strings.Contains(failed, "data-sb-died") {
		t.Error("a failed attempt carries the died marker")
	}

	// UNHAPPY: bob opening alice's drawer URL, as a fragment or a page, gets exactly the response a
	// never-minted id gets, and none of alice's attempt data.
	for _, get := range []func(string) (int, string){
		func(p string) (int, string) { r := getHX(t, f.r, f.bobTok, p); return r.Code, r.Body.String() },
		func(p string) (int, string) { r := f.bobGet(t, p); return r.Code, r.Body.String() },
	} {
		code, got := get("/todos/" + id)
		randCode, randBody := get("/todos/td_" + uuid.NewString())
		if code != http.StatusNotFound || randCode != http.StatusNotFound {
			t.Fatalf("bob's drawer of alice's todo = %d, of a random id = %d; want 404 for both", code, randCode)
		}
		if got != randBody {
			t.Errorf("foreign-todo 404 body %q differs from a never-minted id's %q", got, randBody)
		}
		assertNotIn(t, "bob GET /todos/"+id, got, aliceClaimant, "ALICE-SUMMARY", "ALICEart",
			"attempt history", "reaped", "died")
	}
}
