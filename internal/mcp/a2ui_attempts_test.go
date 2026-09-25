package mcp

// A2UI Todo Detail Attempt Tests
//
// The A2UI todo detail lists the todo's attempts, like the Board drawer: newest first, a died
// marker in words with the last heartbeat, summaries and artifacts as text. The history comes from
// the endpoint-scoped TodoAttempts, so a foreign todo stays not_found.
//
// Governing: SPEC-0034 REQ-4, REQ-10, REQ-13 "Operator Surfaces".
//
// @joestump-agent 09/25/2026 - Added for #329 (epic #313).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

func a2uiHistory() []store.Attempt {
	hb := time.Date(2026, 9, 25, 14, 1, 0, 0, time.UTC)
	ended := hb.Add(10 * time.Minute)
	return []store.Attempt{
		{Seq: 3, Attempt: 1, ClaimerKind: "endpoint", Claimant: "fixer/run-3", ClaimedAt: ended.Add(time.Minute)},
		{Seq: 2, Attempt: 2, ClaimerKind: "endpoint", Claimant: "fixer/run-2", ClaimedAt: hb.Add(-time.Minute),
			LastHeartbeatAt: &hb, EndedAt: &ended, Outcome: "reaped", Disposition: "requeued", Died: true},
		{Seq: 1, Attempt: 1, ClaimerKind: "owner", ClaimedAt: hb.Add(-time.Hour), EndedAt: &hb,
			Outcome: "failed", Disposition: "retry_scheduled", Summary: "tests still red",
			SummaryTruncated: true, Artifact: "mcp://cairn/Zz9"},
	}
}

func TestRenderTodoA2UIListsAttempts(t *testing.T) {
	todo := store.Todo{ID: "td_h", Queue: "reviews", Title: "flaky", State: "claimed", Attempt: 1, MaxAttempts: 5, CreatedAt: time.Now()}
	p := renderTodoA2UI(todo, a2uiAttempts{Rows: a2uiHistory(), Total: 25})
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
	drawn := renderedIDs(p)
	for _, id := range []string{"attempts", "attempts-title", "attempts-list", "attempts-more",
		"attempt-head-0", "attempt-head-1", "attempt-head-2", "attempt-times-1", "attempt-summary-2", "attempt-artifact-2"} {
		if !drawn[id] {
			t.Errorf("%s is not reachable from the root", id)
		}
	}
	text := func(id string) string {
		c := findComponent(p, id)
		if c == nil {
			t.Fatalf("missing %s", id)
		}
		return c.Text
	}
	if got := text("attempts-title"); got != "Attempts (25)" {
		t.Errorf("title = %q", got)
	}
	if got := text("attempts-more"); got != "22 earlier attempts not shown." {
		t.Errorf("more = %q", got)
	}
	// Newest first, in the list's own order.
	if list := findComponent(p, "attempts-list"); !sliceEq(list.Children, []string{"attempt-card-0", "attempt-card-1", "attempt-card-2"}) {
		t.Errorf("list children = %v", list.Children)
	}
	if got := text("attempt-head-0"); got != "#3 · fixer/run-3 · in progress" {
		t.Errorf("open head = %q", got)
	}
	// REQ-13 "Drawer shows a death", on the A2UI surface: outcome, marker in words, heartbeat.
	if got := text("attempt-head-1"); got != "#2 · fixer/run-2 · reaped → requeued · ✝ died: no report" {
		t.Errorf("reaped head = %q", got)
	}
	if got := text("attempt-times-1"); !strings.Contains(got, "last heartbeat 2026-09-25T14:01:00Z") {
		t.Errorf("reaped times = %q, want its last heartbeat", got)
	}
	if got := text("attempt-head-2"); got != "#1 · owner (Board) · failed → retry_scheduled" || strings.Contains(got, "died") {
		t.Errorf("failed head = %q", got)
	}
	if got := text("attempt-summary-2"); got != "Summary: tests still red (truncated)" {
		t.Errorf("summary = %q", got)
	}
	if got := text("attempt-artifact-2"); got != "Artifact: mcp://cairn/Zz9" {
		t.Errorf("artifact = %q", got)
	}
	// Attempts sit between the metadata and the actions.
	col := findComponent(p, "col").Children
	if i, j := indexOf(col, "attempts"), indexOf(col, "actions"); i < 0 || i > j {
		t.Errorf("column order %v: attempts should precede actions", col)
	}
}

func TestRenderTodoA2UINoAttempts(t *testing.T) {
	p := renderTodoA2UI(store.Todo{ID: "td_n", Queue: "q", Title: "new", State: "pending", CreatedAt: time.Now()}, a2uiAttempts{})
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
	if !renderedIDs(p)["attempts-empty"] || findComponent(p, "attempts-list") != nil || findComponent(p, "attempts-more") != nil {
		t.Error("an empty history should render only the empty placeholder")
	}
}

// Over the real SDK: the resource lists the endpoint's own todo's attempts, and another endpoint's
// todo stays not_found, its history unread.
func TestA2UITodoResourceListsAttemptsInScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_hist", Queue: "reviews", Title: "flaky", State: "claimed"})
	f.attempts["td_hist"] = a2uiHistory()
	f.putTodo(store.Todo{EndpointID: endpointIDFor("agent-z-99999999"), ID: "td_foreign", Queue: "reviews", Title: "theirs", State: "failed"})
	f.attempts["td_foreign"] = []store.Attempt{{Seq: 1, ClaimerKind: "endpoint", Summary: "their secret"}}
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://todo/td_hist/a2ui"})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	var p a2uiPayload
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
	if c := findComponent(p, "attempt-head-1"); c == nil || !strings.Contains(c.Text, "died: no report") {
		t.Fatalf("resource should list the reaped attempt with its died marker: %+v", c)
	}
	if c := findComponent(p, "attempts-title"); c == nil || c.Text != "Attempts (3)" {
		t.Errorf("attempts title = %+v", c)
	}

	_, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://todo/td_foreign/a2ui"})
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("foreign todo: err = %v, want not_found", err)
	}
	if strings.Contains(err.Error(), "their secret") {
		t.Error("a foreign todo's attempt summary leaked into the error")
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
