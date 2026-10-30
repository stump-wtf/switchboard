package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// ADR-0055 issue-dependency holds: a blocked todo is visible on the board but must never offer a
// Claim control, and it has to say what it is waiting on. These are the render-side halves of the
// promise the store keeps (unclaimable state, doorbell suppressed).

func blockedRow() todoRow {
	r := rowFixture("blocked")
	r.WaitingOn = "waiting on stump.wtf/switchboard#583, other/repo#12"
	return r
}

func TestBlockedRowNamesBlockersAndOffersNoClaim(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_row", blockedRow())
	if err != nil {
		t.Fatalf("render todo_row(blocked): %v", err)
	}
	if !strings.Contains(out, `data-sb-state="blocked"`) || !strings.Contains(out, ">blocked</span>") {
		t.Errorf("blocked row must stamp and name its state: %q", out)
	}
	if !strings.Contains(out, `data-sb-blockers`) || !strings.Contains(out, "waiting on stump.wtf/switchboard#583, other/repo#12") {
		t.Errorf("blocked row must render its blocker refs: %q", out)
	}
	if strings.Contains(out, "/claim") {
		t.Errorf("blocked row must offer no Claim control: %q", out)
	}
	if !strings.Contains(out, `class="sb-muted">—<`) {
		t.Errorf("blocked row action cell should read as no action: %q", out)
	}
}

func TestBlockedRowWithoutRefsRendersNoEmptySubLine(t *testing.T) {
	h := newTestHandler(t)
	r := rowFixture("blocked")
	out, err := h.renderFragment("todo_row", r)
	if err != nil {
		t.Fatalf("render todo_row(blocked, no refs): %v", err)
	}
	if strings.Contains(out, "data-sb-blockers") {
		t.Errorf("a blocked row with no resolved refs must not render an empty sub-line: %q", out)
	}
}

func TestPendingRowStillOffersClaim(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_row", rowFixture("pending"))
	if err != nil {
		t.Fatalf("render todo_row(pending): %v", err)
	}
	if !strings.Contains(out, `hx-post="/todos/td_pending0000/claim"`) {
		t.Errorf("pending row must keep its Claim control: %q", out)
	}
}

func TestBlockedPillCountsAndFilter(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_pills", panelView{Filter: "blocked", Counts: store.TodoCounts{Blocked: 3}})
	if err != nil {
		t.Fatalf("render todo_pills: %v", err)
	}
	if !strings.Contains(out, `id="sb-tc-blocked" class="sb-fpill__n" aria-live="polite">3<`) {
		t.Errorf("blocked pill must show its live count: %q", out)
	}
	if got := normalizeFilter("Blocked "); got != "blocked" {
		t.Errorf("normalizeFilter = %q, want blocked", got)
	}
	if got := filterState("blocked"); got != store.StateBlocked {
		t.Errorf("filterState = %q, want %q", got, store.StateBlocked)
	}
}

func TestWaitingOnText(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		refs []store.BlockerRef
		want string
	}{
		{"no refs recorded", nil, "waiting on blockers"},
		{"one blocker", []store.BlockerRef{{Owner: "a", Repo: "b", Number: 7, State: "open", CheckedAt: now}}, "waiting on a/b#7"},
		{"cross-repo blockers", []store.BlockerRef{
			{Owner: "a", Repo: "b", Number: 7, State: "open", CheckedAt: now},
			{Owner: "c", Repo: "d", Number: 1, State: "open", CheckedAt: now},
		}, "waiting on a/b#7, c/d#1"},
	}
	for _, c := range cases {
		if got := waitingOn(c.refs); got != c.want {
			t.Errorf("%s: waitingOn = %q, want %q", c.name, got, c.want)
		}
	}
}
