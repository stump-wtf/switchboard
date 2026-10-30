package ingest

// Intake-side tests for issue-dependency holds (ADR-0055, stump.wtf/switchboard#583): the state a
// delivery's todo is minted in is decided before the rows are written, so a held delivery never rings
// anything, and a delivery that closes an issue resolves what was waiting behind it.
//
// The gate is faked here: the provider read itself is tested in internal/deps and internal/provider.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/deps"
	"github.com/stump-wtf/switchboard/internal/store"
)

type fakeGate struct {
	open       []store.BlockerRef
	known      bool
	askedRefs  []deps.IssueRef
	released   []deps.IssueRef
	releaseErr error
}

func (f *fakeGate) BlockersFor(_ context.Context, _ string, ref deps.IssueRef) ([]store.BlockerRef, bool) {
	f.askedRefs = append(f.askedRefs, ref)
	return f.open, f.known
}

func (f *fakeGate) ReleaseBlocker(_ context.Context, ref deps.IssueRef) ([]store.Todo, error) {
	f.released = append(f.released, ref)
	return nil, f.releaseErr
}

const depSecret = "whsec_dependency_test"

const issueOpenedBody = `{"action":"opened","ref":"refs/heads/main","issue":{"number":12,"state":"open",` +
	`"title":"ship the hold","html_url":"https://git.test/octo/repo/issues/12"},` +
	`"repository":{"full_name":"octo/repo"}}`

const issueClosedBody = `{"action":"closed","issue":{"number":7,"state":"closed",` +
	`"html_url":"https://git.test/octo/repo/issues/7"},"repository":{"full_name":"octo/repo"}}`

// held202 decodes the accept body and returns the owner target's todo id and state.
func held202(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Todos []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode accept body %q: %v", rec.Body.String(), err)
	}
	if len(body.Todos) == 0 {
		t.Fatalf("accept body names no todos: %q", rec.Body.String())
	}
	return body.Todos[0].ID, body.Todos[0].State
}

func TestIssueWithOpenBlockersMintsAHeldSilentTodo(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	gate := &fakeGate{known: true, open: []store.BlockerRef{
		{Owner: "octo", Repo: "repo", Number: 7, State: "open"},
		{Owner: "other", Repo: "thing", Number: 3, State: "open"},
	}}
	ing.SetDependencyGate(gate)
	_, owner, _ := seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "dep-hold", depSecret)
	ch, cancel := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancel()

	rec := postSelfManaged(ing, "dep-hold", issueOpenedBody, map[string]string{"X-Gitea-Delivery": "guid-hold", "X-Gitea-Event": "issues", "X-Gitea-Signature": giteaSig(depSecret, issueOpenedBody)})
	if rec.Code != 202 {
		t.Fatalf("status = %d body %s, want 202", rec.Code, rec.Body)
	}
	id, state := held202(t, rec)
	if state != store.StateBlocked {
		t.Fatalf("accept body reported state %q, want %q", state, store.StateBlocked)
	}
	got, err := st.GetTodo(ctx, owner.ID, id)
	if err != nil || got.State != store.StateBlocked {
		t.Fatalf("stored todo: state=%q err=%v, want blocked", got.State, err)
	}
	if got.IssueRef != "octo/repo#12" {
		t.Errorf("issue_ref = %q, want octo/repo#12", got.IssueRef)
	}
	refs, err := st.BlockersFor(ctx, id)
	if err != nil {
		t.Fatalf("blockers: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("recorded %d refs, want both open blockers: %+v", len(refs), refs)
	}
	// The gate was asked about the issue the payload named, not about anything else.
	if len(gate.askedRefs) != 1 || gate.askedRefs[0] != (deps.IssueRef{Owner: "octo", Repo: "repo", Number: 12, State: "open"}) {
		t.Errorf("gate asked about %+v", gate.askedRefs)
	}
	// Nothing may claim it, and nothing rang.
	if _, err := st.ClaimNext(ctx, owner.ID, []string{"reviews"}, "worker", time.Minute); err == nil {
		t.Error("a held todo was handed out by claim_next")
	}
	select {
	case td := <-ch:
		t.Errorf("a held todo rang the doorbell: %+v", td)
	default:
	}
}

func TestUnknownDependencyMeansNoHold(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	ing.SetDependencyGate(&fakeGate{known: false}) // the tracker could not be asked
	_, owner, _ := seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "dep-unknown", depSecret)

	rec := postSelfManaged(ing, "dep-unknown", issueOpenedBody, map[string]string{"X-Gitea-Delivery": "guid-unknown", "X-Gitea-Event": "issues", "X-Gitea-Signature": giteaSig(depSecret, issueOpenedBody)})
	id, state := held202(t, rec)
	if state != "pending" {
		t.Fatalf("state = %q, want pending: an unknown tracker must never hold work", state)
	}
	got, err := st.GetTodo(ctx, owner.ID, id)
	if err != nil || got.State != "pending" {
		t.Fatalf("todo: state=%q err=%v, want pending", got.State, err)
	}
}

func TestClosedIssueReleasesWhatWaitedBehindIt(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	gate := &fakeGate{known: true}
	ing.SetDependencyGate(gate)
	_, owner, _ := seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "dep-close", depSecret)

	rec := postSelfManaged(ing, "dep-close", issueClosedBody, map[string]string{"X-Gitea-Delivery": "guid-close", "X-Gitea-Event": "issues", "X-Gitea-Signature": giteaSig(depSecret, issueClosedBody)})
	if rec.Code != 202 {
		t.Fatalf("status = %d body %s, want 202", rec.Code, rec.Body)
	}
	if len(gate.released) != 1 || gate.released[0] != (deps.IssueRef{Owner: "octo", Repo: "repo", Number: 7, State: "closed"}) {
		t.Fatalf("gate released %v, want octo/repo#7", gate.released)
	}
	// The close is still work that happened: it mints its own todo, unheld.
	id, state := held202(t, rec)
	if state != "pending" {
		t.Errorf("the closing delivery minted state %q, want pending", state)
	}
	if _, err := st.GetTodo(ctx, owner.ID, id); err != nil {
		t.Errorf("the closing delivery's todo is missing: %v", err)
	}
	// A close whose dependents could not be resolved is still an accepted delivery: the sweep retries.
	gate.releaseErr = errors.New("store exploded")
	rec = postSelfManaged(ing, "dep-close", issueClosedBody, map[string]string{"X-Gitea-Delivery": "guid-close-2", "X-Gitea-Event": "issues", "X-Gitea-Signature": giteaSig(depSecret, issueClosedBody)})
	if rec.Code != 202 {
		t.Fatalf("a failed release must not fail the delivery: %d %s", rec.Code, rec.Body)
	}
}

func TestDeliveryWithoutAnIssueAsksNothing(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	gate := &fakeGate{known: true, open: []store.BlockerRef{{Owner: "a", Repo: "b", Number: 1, State: "open"}}}
	ing.SetDependencyGate(gate)
	_, _, _ = seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "dep-noref", depSecret)

	body := `{"action":"pinned","repository":{"full_name":"octo/repo"}}`
	rec := postSelfManaged(ing, "dep-noref", body, map[string]string{"X-Gitea-Delivery": "guid-noref", "X-Gitea-Event": "issues", "X-Gitea-Signature": giteaSig(depSecret, body)})
	if rec.Code != 202 {
		t.Fatalf("status = %d body %s, want 202", rec.Code, rec.Body)
	}
	if len(gate.askedRefs) != 0 {
		t.Errorf("a delivery that names no issue consulted the gate: %v", gate.askedRefs)
	}
	if _, state := held202(t, rec); state != "pending" {
		t.Errorf("state = %q, want pending", state)
	}
}

func TestNoGateWiredIsTheOldBehaviour(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	_, owner, _ := seedWebhook(t, st, ctx, "gitea", "signed", "reviews", "dep-off", depSecret)
	rec := postSelfManaged(ing, "dep-off", issueOpenedBody, map[string]string{"X-Gitea-Delivery": "guid-off", "X-Gitea-Event": "issues", "X-Gitea-Signature": giteaSig(depSecret, issueOpenedBody)})
	id, state := held202(t, rec)
	if state != "pending" {
		t.Fatalf("state = %q, want pending: with no gate wired nothing may be held", state)
	}
	if got, err := st.GetTodo(ctx, owner.ID, id); err != nil || got.IssueRef != "" {
		t.Errorf("an unwired gate still wrote an issue ref: %q, %v", got.IssueRef, err)
	}
}
