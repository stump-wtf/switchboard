package ingest

// Quarantine at the receiver and the release service (ADR-0031, SPEC-0026 REQ-6, REQ-7, REQ-10):
// held deliveries belong to the webhook's owner whatever the fan-out; a rule can quarantine; a
// release goes back through the owner's current rules with .release and the original .actor, keeps
// the todo's id, and refuses to route back into quarantine; a named queue must be within the
// ceiling; discard records the outcome.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

func heldTodo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, st *store.Store, endpointID string) store.Todo {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT id FROM todos WHERE endpoint_id = $1 AND queue = 'quarantine' AND state = 'pending'
		ORDER BY created_at DESC LIMIT 1`, endpointID).Scan(&id); err != nil {
		t.Fatalf("find held todo: %v", err)
	}
	h, err := st.EndpointOwner(ctx, endpointID)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	item, err := st.QuarantinedForHuman(ctx, h, id)
	if err != nil {
		t.Fatalf("read held todo: %v", err)
	}
	return item.Todo
}

// SPEC-0026 REQ-6 scenario "Quarantine is owned by the webhook's owner": the webhook fans out to the
// owner and a second endpoint, and an untrusted delivery yields one held todo on the owner only.
func TestQuarantineBelongsToTheOwnerNotTheFanOut(t *testing.T) {
	ing, pool, ctx, _ := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	const secret = "whsec_q_owner"
	h, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "tok-q-owner", secret)
	other := secondEndpoint(t, ctx, st, h.ID, "q-owner-f", []string{"reviews"})
	if err := st.AddWebhookRoute(ctx, wh.ID, other.ID, h.ID); err != nil {
		t.Fatalf("route: %v", err)
	}
	setTrust(t, ctx, st, wh, `{"logins":["joestump"]}`)

	if code := postGitHubIssue(ing, "tok-q-owner", secret, "q-own-1", githubIssueBody("opened", "mallory", "mallory")); code != 202 {
		t.Fatalf("delivery = %d", code)
	}
	assertHeld(t, ctx, pool, owner.ID, routing.QuarantineUntrustedActor, `"sender": "mallory"`)
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1`, other.ID); n != 0 {
		t.Fatalf("fan-out target has %d todos, want none", n)
	}
	// A trusted delivery still fans out as before.
	if code := postGitHubIssue(ing, "tok-q-owner", secret, "q-own-2", githubIssueBody("opened", "joestump", "joestump")); code != 202 {
		t.Fatalf("trusted delivery = %d", code)
	}
	if n := countWhere(t, ctx, pool, `SELECT count(*) FROM todos WHERE endpoint_id = $1 AND queue = 'reviews'`, other.ID); n != 1 {
		t.Fatalf("fan-out target has %d reviews todos after a trusted delivery, want 1", n)
	}
}

// SPEC-0026 REQ-7 scenario "Human releases an outside report to triage", at the service level, and
// "Release into quarantine again is refused".
func TestReleaseRoutesThroughTheRules(t *testing.T) {
	ing, pool, ctx, _ := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	const secret = "whsec_q_release"
	h, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "tok-q-release", secret)
	setWebhookQueues(t, ctx, pool, owner.ID, "lane-m", "reviews")
	setTrust(t, ctx, st, wh, `{"logins":["joestump"]}`)
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "hold-comments", Expr: `.payload.action == "edited"`, Action: routing.Action{Quarantine: true}},
		{ID: "human-release", Expr: `.release != null and (.release.by | startswith("human:"))`,
			Action: routing.Action{Queue: "lane-m", WorkOrder: true}},
	}, Default: &routing.Action{Queue: "reviews"}})

	if code := postGitHubIssue(ing, "tok-q-release", secret, "q-rel-1", githubIssueBody("opened", "mallory", "mallory")); code != 202 {
		t.Fatalf("delivery = %d", code)
	}
	held := heldTodo(t, ctx, pool, st, owner.ID)
	by := "human:" + h.ID

	// A named queue outside the ceiling, or quarantine itself, is refused.
	for _, q := range []string{"elsewhere", store.QueueQuarantine} {
		if _, err := ing.ReleaseQuarantined(ctx, h.ID, held.ID, by, q); !errors.Is(err, ErrQueueNotGranted) {
			t.Fatalf("release to %q = %v, want ErrQueueNotGranted", q, err)
		}
	}
	// Another human cannot release it.
	stranger, _ := seedEndpoint(t, st, ctx, "q-stranger", []string{"q"})
	if _, err := ing.ReleaseQuarantined(ctx, stranger.ID, held.ID, "human:"+stranger.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign release = %v, want not found", err)
	}

	out, err := ing.ReleaseQuarantined(ctx, h.ID, held.ID, by, "")
	if err != nil || len(out) != 1 {
		t.Fatalf("release = %+v (%v)", out, err)
	}
	moved := out[0].Todo
	if moved.ID != held.ID || moved.Queue != "lane-m" || moved.ReleasedBy != by {
		t.Fatalf("released todo = %+v, want the same id on lane-m, released by %s", moved, by)
	}
	var wo routing.WorkOrder
	if err := json.Unmarshal(moved.WorkOrder, &wo); err != nil || wo.ReleasedBy != by || wo.AuthorTrusted == nil || *wo.AuthorTrusted {
		t.Fatalf("work order = %s (%v), want released_by and author_trusted false (the original .actor)", moved.WorkOrder, err)
	}
	if _, err := ing.ReleaseQuarantined(ctx, h.ID, held.ID, by, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second release = %v, want not found (no longer held)", err)
	}

	// A delivery the rules quarantine (rule_action) cannot be released back into quarantine by the
	// same rules; naming a granted queue skips the rules and releases it.
	editBody := githubIssueBody("edited", "joestump", "joestump")
	if code := postGitHubIssue(ing, "tok-q-release", secret, "q-rel-2", editBody); code != 202 {
		t.Fatalf("edit delivery = %d", code)
	}
	again := heldTodo(t, ctx, pool, st, owner.ID)
	if again.QuarantineReason != routing.QuarantineRuleAction {
		t.Fatalf("rule-held reason = %q, want rule_action", again.QuarantineReason)
	}
	if _, err := ing.ReleaseQuarantined(ctx, h.ID, again.ID, "classifier:triage-x", ""); !errors.Is(err, ErrReleaseConflict) {
		t.Fatalf("release into quarantine = %v, want ErrReleaseConflict", err)
	}
	if still := heldTodo(t, ctx, pool, st, owner.ID); still.ID != again.ID {
		t.Fatalf("held item after a refused release = %s, want %s still held", still.ID, again.ID)
	}
	named, err := ing.ReleaseQuarantined(ctx, h.ID, again.ID, "classifier:triage-x", "reviews")
	if err != nil || named[0].Todo.Queue != "reviews" || named[0].Todo.ReleasedBy != "classifier:triage-x" {
		t.Fatalf("named release = %+v (%v)", named, err)
	}
}

func TestDiscardThroughTheService(t *testing.T) {
	ing, pool, ctx, _ := ingestWithLogCapture(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)
	const secret = "whsec_q_discard"
	h, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "tok-q-discard", secret)
	setTrust(t, ctx, st, wh, `{"logins":[]}`)
	if code := postGitHubIssue(ing, "tok-q-discard", secret, "q-dis-1", githubIssueBody("opened", "mallory", "mallory")); code != 202 {
		t.Fatalf("delivery = %d", code)
	}
	held := heldTodo(t, ctx, pool, st, owner.ID)
	done, err := ing.DiscardQuarantined(ctx, h.ID, held.ID, "human:"+h.ID, "spam")
	if err != nil || done.State != "done" {
		t.Fatalf("discard = %+v (%v)", done, err)
	}
	if _, err := ing.DiscardQuarantined(ctx, h.ID, held.ID, "human:"+h.ID, "again"); !errors.Is(err, ErrReleaseConflict) {
		t.Fatalf("second discard = %v, want ErrReleaseConflict", err)
	}
}
