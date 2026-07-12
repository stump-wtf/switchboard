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

// retryRowFixture builds the failed-with-scheduled-retry render model (SPEC-0003 scheduled
// backoff): attempt 2 of 5 with a live retry window stamped at a fixed unix-ms deadline.
func retryRowFixture(deadline int64) todoRow {
	row := rowFixture("failed")
	row.Attempt = 2
	row.RetryScheduled = true
	row.RetrySecs = 30
	row.RetryDeadlineMS = deadline
	return row
}

// TestDrawerRetryBackoffCardContract pins the drawer failed-card for a scheduled retry (design:
// "retry with backoff · attempt N" + live ↻ countdown; SPEC-0003 REQ "Bounded Retries via
// max_attempts", scheduled backoff): the card carries the design copy, the countdown element ticks
// via the same sb.js mechanism as the lease (data-sb-countdown toward the server-stamped
// deadline), and the dead-letter callout is NOT rendered while a retry is scheduled. "Retry now"
// stays in the footer as the manual override.
func TestDrawerRetryBackoffCardContract(t *testing.T) {
	h := newTestHandler(t)
	const deadline = int64(1730000456000)
	out, err := h.renderFragment("drawer", drawerView{Row: retryRowFixture(deadline), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`retry with backoff · attempt 2`,       // the design failed-card copy
		`↻ 30s`,                                // server-rendered countdown seed before the first JS tick
		`data-sb-countdown`,                    // sb.js re-labels it each tick
		`data-sb-deadline="1730000456000"`,     // the server-stamped retry deadline flows through verbatim
		`data-sb-prefix="↻ "`,                  // sb.js keeps the ↻ glyph in front of the ticking seconds
		`role="timer"`,                         // announced as a timer (AT)
		`hx-post="/todos/td_failed0000/retry"`, // "Retry now" remains the manual override
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer retry card contract: missing %q", want)
		}
	}
	if strings.Contains(out, "Dead-lettered") {
		t.Errorf("drawer with a scheduled retry must not render the dead-letter callout: %q", out)
	}
}

// TestTodoRowRetryCountdownContract pins the todos-table failed-row sub-line for a scheduled retry
// (design sub-line "↻ retry Ns"): the same data-sb-countdown mechanism, prefixed, replacing the
// static attempts sub-line while the window is open.
func TestTodoRowRetryCountdownContract(t *testing.T) {
	h := newTestHandler(t)
	const deadline = int64(1730000789000)
	out, err := h.renderFragment("todo_row", retryRowFixture(deadline))
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	for _, want := range []string{
		`↻ retry 30s`, // server-rendered seed of the design sub-line
		`data-sb-countdown`,
		`data-sb-deadline="1730000789000"`,
		`data-sb-prefix="↻ retry "`, // sb.js renders "↻ retry <secs>s" from this prefix
	} {
		if !strings.Contains(out, want) {
			t.Errorf("todo_row retry countdown contract: missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "attempts</span>") {
		t.Errorf("failed row with a scheduled retry should show the countdown, not the static attempts sub-line: %q", out)
	}
}

// TestDrawerIdentityBlock pins the drawer identity block the design puts at the top of the body
// (detail.tag / detail.src / detail.type, issue #178): the trust-tinted provider tag chip beside
// the source name and event type. Governing: SPEC-0013 REQ "Todo Detail Drawer".
func TestDrawerIdentityBlock(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("pending"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`class="sb-identity"`,              // the block itself, before the meta grid
		`class="sb-tag sb-tag--signed"`,    // provider tag tinted by the trust mode
		`>GH<`,                             // github → GH per providerTag
		`class="sb-identity__src">github<`, // the source name
		`class="sb-identity__type">push<`,  // the event type
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer identity block: missing %q", want)
		}
	}
}

// TestDrawerVisibilityLeaseCopy pins the design copy on the claimed drawer's lease card (#178):
// the "Visibility lease · {agent}" label, the reaper hint line, and the "Ns left" countdown seed
// (data-sb-suffix so sb.js keeps the phrasing on every tick). Governing: SPEC-0013 REQ "Todo
// Detail Drawer" (lease card), SPEC-0003 REQ "Visibility Window, Lease, Heartbeat".
func TestDrawerVisibilityLeaseCopy(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("claimed"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		"Visibility lease · agent · reviewer-bot",                             // label carries the leasing agent
		"if the lease expires, the reaper re-surfaces this todo to the queue", // reaper hint
		`data-sb-suffix=" left"`,                                              // sb.js renders "<secs>s left" from this suffix
		">240s left</span>",                                                   // server-rendered seed before the first JS tick
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer lease card copy: missing %q", want)
		}
	}
}

// TestDrawerDoneTerminalFooter pins the terminal footer for done todos (#178): where the action
// buttons would be, a finished todo shows the design's "✓ completed · acked to source" line (the
// footer was previously empty). Governing: SPEC-0013 REQ "Todo Detail Drawer" (footer actions
// appropriate to the state).
func TestDrawerDoneTerminalFooter(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("done"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if !strings.Contains(out, `class="sb-drawer__done">✓ completed · acked to source<`) {
		t.Errorf("done drawer: missing terminal footer: %q", out)
	}
	// Terminal means read-only: the footer line must not appear on live states.
	for _, state := range []string{"pending", "claimed", "failed"} {
		live, err := h.renderFragment("drawer", drawerView{Row: rowFixture(state), IdempotencyKey: "k"})
		if err != nil {
			t.Fatalf("render drawer (%s): %v", state, err)
		}
		if strings.Contains(live, "acked to source") {
			t.Errorf("%s drawer must not render the done terminal footer", state)
		}
	}
}

// TestDrawerDedupLineAlwaysRendered pins the dedupLine sentence under the idempotency key (#178):
// always present — "N duplicate deliveries collapsed via idempotency key" once the key has
// collapsed deliveries, and the "no duplicate deliveries" baseline otherwise. The count is the
// store's DedupCount (events sharing the todo's idempotency key; see store/queue_view.go).
// Governing: SPEC-0013 REQ "Todo Detail Drawer" (idempotency key with a dedup summary).
func TestDrawerDedupLineAlwaysRendered(t *testing.T) {
	h := newTestHandler(t)
	baseline, err := h.renderFragment("drawer", drawerView{Row: rowFixture("pending"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if !strings.Contains(baseline, "no duplicate deliveries") {
		t.Errorf("drawer without dedup: missing baseline dedupLine: %q", baseline)
	}
	row := rowFixture("pending")
	row.DedupCount = 3
	collapsed, err := h.renderFragment("drawer", drawerView{Row: row, IdempotencyKey: "gh-key-1"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if !strings.Contains(collapsed, "3 duplicate deliveries collapsed via idempotency key") {
		t.Errorf("drawer with dedup ×3: missing collapsed dedupLine: %q", collapsed)
	}
	if strings.Contains(collapsed, "no duplicate deliveries") {
		t.Errorf("drawer with dedup ×3 must not also render the baseline line: %q", collapsed)
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

// TestTodosHeaderDurableQueueCopy pins the #179 header copy from the design record: the page
// heading is 'Durable queue' with the queue-semantics tagline, and the animated reaper pill sits
// in the header ("the view MUST surface that the reaper is active" — SPEC-0013 REQ "Todos View —
// Durable Queue"). The rail entry stays 'Todos'.
func TestTodosHeaderDurableQueueCopy(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "todos", view{
		Title: "Todos", Human: testHuman(), CSRF: "tok",
		Shell:  shell{Active: "todos", DBConnected: true, Initials: "JS"},
		Filter: "all",
	})
	for _, want := range []string{
		`<h1 class="sb-page-title">Durable queue</h1>`,
		"claim under a lease · complete with an ack · dedup by idempotency key · at-least-once",
		`class="sb-reaper"`,
		"lease reaper active · re-surfaces abandoned work",
		`<span class="sb-rail__label">Todos</span>`, // the rail entry keeps its nav name
	} {
		if !strings.Contains(body, want) {
			t.Errorf("todos header: missing %q", want)
		}
	}
}

// TestTodoRowWholeRowOpenContract pins the #179 whole-row affordance contract sb.js drives: the
// <tr> carries data-sb-row-open + tabindex="0" (focusable, Enter/Space forwarded) and the id
// cell's drawer button carries data-sb-row-trigger, so a click anywhere on the row body lands on
// the same accessible, HTMX-wired trigger. The <tr> keeps its native row role — no role override —
// so the table stays a table for AT.
func TestTodoRowWholeRowOpenContract(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_row", rowFixture("pending"))
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	for _, want := range []string{
		`data-sb-row-open`,               // the row-open scope sb.js delegates on
		`tabindex="0"`,                   // the row is keyboard-focusable
		`data-sb-row-trigger`,            // the forwarding target inside the id cell
		`hx-get="/todos/td_pending0000"`, // ...which stays the HTMX drawer trigger
		`aria-haspopup="dialog"`,         // ...announced as a dialog trigger
	} {
		if !strings.Contains(out, want) {
			t.Errorf("todo_row row-open contract: missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "<tr role=") {
		t.Errorf("todo_row must keep its native row role (no role override): %q", out)
	}
}

// TestTodoRowCarriesSourceTag pins the #179 two-letter provider tag on todo rows: the same sb-tag
// chip the Board feed renders, aria-hidden (AT reads the full source name beside it), and absent
// entirely when the row has no source to abbreviate.
func TestTodoRowCarriesSourceTag(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_row", rowFixture("pending")) // Source: github
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	if !strings.Contains(out, `<span class="sb-tag" aria-hidden="true">GH</span>`) {
		t.Errorf("todo_row: missing two-letter source tag chip: %q", out)
	}
	bare := rowFixture("pending")
	bare.Source = ""
	out, err = h.renderFragment("todo_row", bare)
	if err != nil {
		t.Fatalf("render todo_row (no source): %v", err)
	}
	if strings.Contains(out, "sb-tag") {
		t.Errorf("todo_row without a source must not render a tag chip: %q", out)
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
