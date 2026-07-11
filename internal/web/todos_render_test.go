package web

// Companion render coverage for the SPEC-0013 Todos view/drawer (issue #104) that complements
// todos_test.go: the terminal DONE state (no lease, no actions), the data-attribute CONTRACT the
// thin vanilla-JS layer (static/sb.js) reads to animate lease countdowns and trap drawer focus, and
// the aria-live landmarks on the Todos surface. sb.js is an un-exported IIFE with no JS test runner
// in the `go test ./...` gate, so these template assertions are the browser-independent guard on its
// behavior: if a template stops emitting data-sb-deadline / data-sb-drawer, the countdown or focus
// trap silently breaks and one of these tests fails.
//
// Governing: SPEC-0013 REQ "Todos View — Durable Queue", REQ "Todo Detail Drawer", REQ "Live
// Updates and Toasts", REQ "Design Language Conformance" (WCAG landmarks).

import (
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// TestTodoRowDoneStateIsTerminal covers the fourth todo state the checklist calls out: a done row
// shows the done status with no lease sub-line and offers no lifecycle action (the queue is the
// record; a completed todo is read-only in the table).
func TestTodoRowDoneStateIsTerminal(t *testing.T) {
	h := newTestHandler(t)
	done := rowFixture("done")
	done.OwnerLabel = "operator"
	out, err := h.renderFragment("todo_row", done)
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	if !strings.Contains(out, `class="sb-status sb-status--done"`) {
		t.Errorf("done row: missing done status pill: %q", out)
	}
	// No action button and no lease/retry sub-line — the action cell is the muted placeholder.
	for _, banned := range []string{"hx-post=", "data-sb-countdown", "sb-trow__sub"} {
		if strings.Contains(out, banned) {
			t.Errorf("done row must not contain %q (terminal, no action/lease): %q", banned, out)
		}
	}
	if !strings.Contains(out, `class="sb-muted">—`) {
		t.Errorf("done row action cell should be the muted placeholder: %q", out)
	}
}

// TestDrawerDoneStateHasNoActionsOrLease covers the done drawer: the timeline's completion step is
// marked done, but there is no lease card, no dead-letter callout, and no footer action (nothing to
// claim/complete/fail/retry on a finished todo).
func TestDrawerDoneStateHasNoActionsOrLease(t *testing.T) {
	h := newTestHandler(t)
	completed := time.Now().Add(-1 * time.Minute)
	claimed := time.Now().Add(-3 * time.Minute)
	dv := drawerView{
		Row: rowFixture("done"), IdempotencyKey: "gh-done-1", PayloadJSON: "{}",
		HasEvent: true, ReceivedAt: time.Now().Add(-5 * time.Minute),
		ClaimedAt: &claimed, CompletedAt: &completed,
	}
	dv.Row.OwnerLabel = "operator"
	out, err := h.renderFragment("drawer", dv)
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	// The completed timeline step is resolved (all four steps done for a finished todo).
	if !strings.Contains(out, "completed · ack") {
		t.Errorf("done drawer: timeline missing completed step: %q", out)
	}
	if n := strings.Count(out, "sb-timeline__done"); n != 4 {
		t.Errorf("done drawer: %d resolved timeline steps, want 4 (received/created/claimed/completed): %q", n, out)
	}
	// A done todo offers no lease card, no dead-letter callout, and no footer action POST.
	for _, banned := range []string{"sb-lease", "sb-callout--danger", "hx-post="} {
		if strings.Contains(out, banned) {
			t.Errorf("done drawer must not contain %q: %q", banned, out)
		}
	}
}

// TestDrawerCountdownContract pins the exact data attributes static/sb.js reads to animate the lease
// countdown and progress bar toward the server-stamped deadline. The deadline value must flow through
// verbatim so the client re-syncs on every SSE swap; the lease bar carries data-sb-leasebar and a
// full-width start. Governing: SPEC-0013 REQ "Todo Detail Drawer" (lease countdown), design.md
// ("thin vanilla-JS layer" — the server stamps, JS only presents).
func TestDrawerCountdownContract(t *testing.T) {
	h := newTestHandler(t)
	row := rowFixture("claimed")
	const deadline = int64(1730000000000) // fixed unix-ms so the assertion is exact
	row.LeaseDeadlineMS = deadline
	out, err := h.renderFragment("drawer", drawerView{Row: row, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`data-sb-countdown`,                // the countdown element sb.js re-labels each tick
		`data-sb-deadline="1730000000000"`, // the server-stamped deadline flows through verbatim
		`role="timer"`,                     // the countdown is announced as a timer (AT)
		`data-sb-leasebar`,                 // the progress bar sb.js drains
		`style="width:100%"`,               // the bar starts full and drains toward the deadline
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer countdown contract: missing %q", want)
		}
	}
}

// TestTodoRowCountdownContract pins the table-row lease sub-line contract: the same data-sb-deadline
// stamp plus the row-specific data-sb-suffix that sb.js appends after the seconds.
func TestTodoRowCountdownContract(t *testing.T) {
	h := newTestHandler(t)
	row := rowFixture("claimed")
	const deadline = int64(1730000123000)
	row.LeaseDeadlineMS = deadline
	out, err := h.renderFragment("todo_row", row)
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	for _, want := range []string{
		`data-sb-countdown`,
		`data-sb-deadline="1730000123000"`,
		`data-sb-suffix=" left"`, // sb.js renders "<secs>s left" from this suffix
	} {
		if !strings.Contains(out, want) {
			t.Errorf("todo_row countdown contract: missing %q in %q", want, out)
		}
	}
}

// TestDrawerFocusTrapContract pins the DOM hooks static/sb.js uses to open the overlay, trap Tab
// focus, and close on Escape / scrim / the close button (with focus return). Without these the
// focus trap and Escape-to-close silently stop working, so the template must always emit them.
// Governing: SPEC-0013 REQ "Todo Detail Drawer" (focus trap, Escape close, focus return).
func TestDrawerFocusTrapContract(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("claimed"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`role="dialog"`,     // dialog semantics
		`aria-modal="true"`, // modal — AT confines to the drawer
		`data-sb-drawer`,    // the focus-trap scope sb.js queries for focusables
		`tabindex="-1"`,     // the drawer root is programmatically focusable
		`data-sb-close`,     // the close control sb.js binds (also the standalone-page backlink)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer focus-trap contract: missing %q", want)
		}
	}
}

// TestTodosPageLiveRegionsAndDrawerTrigger asserts the Todos view carries the aria-live landmarks a
// background transition needs to be announced (the toast region, the overlay slot, and the table
// body live region) and that a row's id cell is the drawer trigger sb.js keys focus-return on
// (hx-target #sb-overlay + aria-haspopup dialog). Governing: SPEC-0013 REQ "Live Updates and Toasts",
// REQ "Todo Detail Drawer".
func TestTodosPageLiveRegionsAndDrawerTrigger(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "todos", view{
		Title: "Todos", Human: testHuman(), CSRF: "tok",
		Shell:     shell{Active: "todos", DBConnected: true, Initials: "JS"},
		Counts:    store.TodoCounts{All: 1, Pending: 1},
		Filter:    "all",
		TodoItems: []todoRow{rowFixture("pending")},
	})
	for _, want := range []string{
		`id="sb-toasts"`,                        // toast region present before the first frame
		`aria-live="polite"`,                    // ...and it is a polite live region
		`id="sb-overlay"`,                       // the drawer overlay slot the row opens into
		`id="sb-todos-body" aria-live="polite"`, // the table body announces live row swaps
		`hx-target="#sb-overlay"`,               // the id cell opens the drawer into the overlay
		`aria-haspopup="dialog"`,                // ...announced as a dialog trigger
	} {
		if !strings.Contains(body, want) {
			t.Errorf("todos page: missing live-region/drawer hook %q", want)
		}
	}
}

// TestTodoPillsActiveStateAcrossFilters rounds out the filter-pill coverage: every one of the five
// SPEC-0013 pills (All/Pending/Claimed/Done/Failed) becomes the single active pill for its filter,
// and its aria-selected flips to true. todos_test.go covers all/failed; this covers the rest,
// including Done which no other test exercises.
func TestTodoPillsActiveStateAcrossFilters(t *testing.T) {
	h := newTestHandler(t)
	labels := map[string]string{
		"all": "All", "pending": "Pending", "claimed": "Claimed", "done": "Done", "failed": "Failed",
	}
	for filter, label := range labels {
		out, err := h.renderFragment("todo_pills", panelView{Filter: filter, Counts: store.TodoCounts{}})
		if err != nil {
			t.Fatalf("render todo_pills(%s): %v", filter, err)
		}
		// Exactly one pill is active.
		if n := strings.Count(out, "sb-fpill--active"); n != 1 {
			t.Errorf("filter %q: %d active pills, want exactly 1", filter, n)
		}
		// The active pill is the one for this filter, with aria-selected="true".
		wantHref := `href="/todos?filter=` + filter + `"`
		if !strings.Contains(out, wantHref) {
			t.Errorf("filter %q: missing pill link %q", filter, wantHref)
		}
		if c := strings.Count(out, `aria-selected="true"`); c != 1 {
			t.Errorf("filter %q: %d pills marked selected, want 1", filter, c)
		}
		if !strings.Contains(out, label+" <span") {
			t.Errorf("filter %q: missing %q pill label", filter, label)
		}
	}
}
