package web

// Companion render coverage for the SPEC-0015 Todos view/drawer that complements todos_test.go:
// the terminal DONE state (no lease, no actions), the data-attribute CONTRACT the presentation-JS
// modules (static/js/sb-live.js, sb-overlay.js) read to animate lease countdowns and trap drawer
// focus, and the aria-live landmarks on the Todos surface. The JS modules are un-exported IIFEs
// with no JS test runner in the `go test ./...` gate, so these template assertions are the
// browser-independent guard on their behavior: if a template stops emitting data-sb-deadline /
// data-sb-drawer, the countdown or focus trap silently breaks and one of these tests fails.
// Assertions key on data-sb-* attributes and stable ids, never on styling classes (ADR-0018: the
// sb-* class layer is presentation; data-sb-* is behavior).
//
// Governing: ADR-0018; SPEC-0015 REQ "Todos View And Drawer" (all SPEC-0003 actions and SSE row
// updates preserved), REQ "Application Shell And Navigation"; SPEC-0012 REQ "Live Updates via
// SSE" (aria-live landmarks).

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// TestTodoRowDoneStateIsTerminal covers the fourth todo state: a done row shows the done state
// chip with no lease sub-line and offers no lifecycle action (the queue is the record; a
// completed todo is read-only in the table).
func TestTodoRowDoneStateIsTerminal(t *testing.T) {
	h := newTestHandler(t)
	done := rowFixture("done")
	done.OwnerLabel = "operator"
	out, err := h.renderFragment("todo_row", done)
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	if !strings.Contains(out, `data-sb-state="done"`) {
		t.Errorf("done row: missing done state chip stamp: %q", out)
	}
	// No action button and no lease/retry sub-line — the action cell is the muted placeholder.
	for _, banned := range []string{"hx-post=", "data-sb-countdown"} {
		if strings.Contains(out, banned) {
			t.Errorf("done row must not contain %q (terminal, no action/lease): %q", banned, out)
		}
	}
	if !strings.Contains(out, ">—</span></td>") {
		t.Errorf("done row action cell should be the muted placeholder: %q", out)
	}
}

// TestDrawerDoneStateHasNoActionsOrLease covers the done drawer: every lifecycle timeline step is
// resolved (data-sb-done), but there is no lease card, no dead-letter callout, and no footer
// action (nothing to claim/complete/fail/retry on a finished todo).
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
	if n := strings.Count(out, "data-sb-done"); n != 4 {
		t.Errorf("done drawer: %d resolved timeline steps, want 4 (received/created/claimed/resolved): %q", n, out)
	}
	// A done todo offers no lease card, no dead-letter callout, and no footer action POST.
	for _, banned := range []string{"data-sb-lease", "Dead-lettered", "hx-post="} {
		if strings.Contains(out, banned) {
			t.Errorf("done drawer must not contain %q: %q", banned, out)
		}
	}
}

// TestDrawerCountdownContract pins the exact data attributes sb-live.js reads to animate the
// lease countdown and progress bar toward the server-stamped deadline. The deadline value must
// flow through verbatim so the client re-syncs on every SSE swap; the lease bar carries
// data-sb-leasebar and a full-width start. Governing: SPEC-0015 REQ "Todos View And Drawer"
// (lease countdown), design.md ("the server stamps, JS only presents").
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
		`data-sb-lease`,                    // the lease card itself
		`data-sb-countdown`,                // the countdown element sb-live.js re-labels each tick
		`data-sb-deadline="1730000000000"`, // the server-stamped deadline flows through verbatim
		`role="timer"`,                     // the countdown is announced as a timer (AT)
		`data-sb-leasebar`,                 // the progress bar sb-live.js drains
		`style="width:100%"`,               // the bar starts full and drains toward the deadline
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer countdown contract: missing %q", want)
		}
	}
}

// TestTodoRowCountdownContract pins the table-row lease sub-line contract: the same
// data-sb-deadline stamp plus the row-specific data-sb-suffix that sb-live.js appends after the
// seconds.
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
		`data-sb-suffix=" left"`, // sb-live.js renders "<secs>s left" from this suffix
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
// max_attempts", scheduled backoff): the card carries the design copy, the countdown element
// ticks via the same sb-live.js mechanism as the lease (data-sb-countdown toward the
// server-stamped deadline), and the dead-letter callout is NOT rendered while a retry is
// scheduled. "Retry now" stays in the footer as the manual override.
func TestDrawerRetryBackoffCardContract(t *testing.T) {
	h := newTestHandler(t)
	const deadline = int64(1730000456000)
	out, err := h.renderFragment("drawer", drawerView{Row: retryRowFixture(deadline), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`data-sb-retry`,                        // the retry card itself
		`retry with backoff · attempt 2`,       // the design failed-card copy
		`↻ 30s`,                                // server-rendered countdown seed before the first JS tick
		`data-sb-countdown`,                    // sb-live.js re-labels it each tick
		`data-sb-deadline="1730000456000"`,     // the server-stamped retry deadline flows through verbatim
		`data-sb-prefix="↻ "`,                  // sb-live.js keeps the ↻ glyph in front of the ticking seconds
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

// TestTodoRowRetryCountdownContract pins the todos-table failed-row sub-line for a scheduled
// retry (design sub-line "↻ retry Ns"): the same data-sb-countdown mechanism, prefixed, replacing
// the static attempts sub-line while the window is open.
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
		`data-sb-prefix="↻ retry "`, // sb-live.js renders "↻ retry <secs>s" from this prefix
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
// (detail.tag / detail.src / detail.type): the trust-tinted provider tag beside the source name
// and event type, each carrying its data-sb-* stamp. Governing: SPEC-0015 REQ "Todos View And
// Drawer" (drawer identity).
func TestDrawerIdentityBlock(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("pending"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`data-sb-identity`,       // the block itself, before the meta grid
		`data-sb-trust="signed"`, // provider tag stamped with the trust mode
		`sb-icon-wrap`,           // github → SVG icon (replaces two-letter tag)
		`data-sb-source>github<`, // the source name
		`data-sb-kind>push<`,     // the event type
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer identity block: missing %q", want)
		}
	}
}

// TestDrawerVisibilityLeaseCopy pins the charm-web copy on the claimed drawer's lease card: the
// lowercase "visibility lease · {agent}" microlabel, the reaper hint line, and the "Ns left"
// countdown seed (data-sb-suffix so sb-live.js keeps the phrasing on every tick). Governing:
// SPEC-0015 REQ "Todos View And Drawer" (lease, claimed-by), SPEC-0003 REQ "Visibility Window,
// Lease, Heartbeat".
func TestDrawerVisibilityLeaseCopy(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("claimed"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		"visibility lease · agent · reviewer-bot",                             // label carries the leasing agent
		"if the lease expires, the reaper re-surfaces this todo to the queue", // reaper hint
		`data-sb-suffix=" left"`,                                              // sb-live.js renders "<secs>s left" from this suffix
		">240s left</span>",                                                   // server-rendered seed before the first JS tick
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer lease card copy: missing %q", want)
		}
	}
}

// TestDrawerDoneTerminalFooter pins the terminal footer for done todos: where the action buttons
// would be, a finished todo shows the design's "✓ completed · acked to source" line
// (data-sb-acked). Governing: SPEC-0015 REQ "Todos View And Drawer" (footer appropriate to the
// state).
func TestDrawerDoneTerminalFooter(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("done"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if !strings.Contains(out, `data-sb-acked>✓ completed · acked to source<`) {
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

// TestDrawerDedupLineAlwaysRendered pins the dedupLine sentence under the idempotency key
// (data-sb-dedupline): always present — "N duplicate deliveries collapsed via idempotency key"
// once the key has collapsed deliveries, and the "no duplicate deliveries" baseline otherwise.
// The count is the store's DedupCount (events sharing the todo's idempotency key; see
// store/queue_view.go). Governing: SPEC-0015 REQ "Todos View And Drawer" (idempotency key with a
// dedup summary).
func TestDrawerDedupLineAlwaysRendered(t *testing.T) {
	h := newTestHandler(t)
	baseline, err := h.renderFragment("drawer", drawerView{Row: rowFixture("pending"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if !strings.Contains(baseline, "data-sb-dedupline>no duplicate deliveries") {
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

// TestDrawerFocusTrapContract pins the DOM hooks sb-overlay.js uses to open the overlay, trap
// Tab focus, and close on Escape / scrim / the close button (with focus return). Without these
// the focus trap and Escape-to-close silently stop working, so the template must always emit
// them. Governing: SPEC-0015 REQ "Todos View And Drawer" (drawer preserved from SPEC-0013 with
// its focus trap, Escape close, focus return).
func TestDrawerFocusTrapContract(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("drawer", drawerView{Row: rowFixture("claimed"), IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`role="dialog"`,     // dialog semantics
		`aria-modal="true"`, // modal — AT confines to the drawer
		`data-sb-drawer`,    // the focus-trap scope sb-overlay.js queries for focusables
		`tabindex="-1"`,     // the drawer root is programmatically focusable
		`data-sb-close`,     // the close control sb-overlay.js binds
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer focus-trap contract: missing %q", want)
		}
	}
}

// TestTodosPageLiveRegionsAndDrawerTrigger asserts the Todos view carries the aria-live
// landmarks a background transition needs to be announced (the toast region, the overlay slot,
// and the table body live region) and that a row's line cell is the drawer trigger
// sb-overlay.js keys focus-return on (hx-target #sb-overlay + aria-haspopup dialog). Governing:
// SPEC-0015 REQ "Todos View And Drawer" (SSE row updates), SPEC-0012 REQ "Live Updates via SSE".
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
		`hx-target="#sb-overlay"`,               // the line cell opens the drawer into the overlay
		`aria-haspopup="dialog"`,                // ...announced as a dialog trigger
	} {
		if !strings.Contains(body, want) {
			t.Errorf("todos page: missing live-region/drawer hook %q", want)
		}
	}
}

// TestTodosHeaderCharmCopy pins the charm-web view head: the lowercase view title (ADR-0018:
// view titles render lowercase), the queue-semantics tagline in middot voice, and the animated
// reaper pill in the header ("the view MUST surface that the reaper is active"). The nav entry
// keeps its capitalized label. Governing: SPEC-0015 REQ "Todos View And Drawer", REQ
// "Application Shell And Navigation".
func TestTodosHeaderCharmCopy(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "todos", view{
		Title: "Todos", Human: testHuman(), CSRF: "tok",
		Shell:  shell{Active: "todos", DBConnected: true, Initials: "JS"},
		Filter: "all",
	})
	for _, want := range []string{
		`>todos</h1>`, // lowercase view title (ADR-0018)
		"the durable queue · claim under a lease · complete with an ack · dedup by idempotency key · at-least-once",
		"lease reaper active · re-surfaces abandoned work",
		`<span class="sb-rail__label">Todos</span>`, // the nav entry keeps its label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("todos header: missing %q", want)
		}
	}
}

// TestTodoRowWholeRowOpenContract pins the whole-row affordance contract the presentation JS
// drives: the <tr> carries data-sb-row-open + tabindex="0" (focusable; Space forwarded by
// sb-overlay.js, Enter by the sb-keys.js keymap registry per SPEC-0015 "Global Keyboard Map")
// and the line cell's drawer button carries data-sb-row-trigger, so a click anywhere on the row
// body lands on the same accessible, HTMX-wired trigger. The <tr> keeps its native row role — no
// role override — so the table stays a table for AT.
func TestTodoRowWholeRowOpenContract(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_row", rowFixture("pending"))
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	for _, want := range []string{
		`data-sb-row-open`,               // the row-open scope the JS delegates on
		`tabindex="0"`,                   // the row is keyboard-focusable
		`data-sb-row-trigger`,            // the forwarding target inside the line cell
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

// TestTodoRowCarriesSourceTag pins the two-letter provider tag on todo rows: the same tag tile
// the Board feed renders (data-sb-tag), aria-hidden (AT reads the full source name beside it),
// and absent entirely when the row has no source to abbreviate.
func TestTodoRowCarriesSourceTag(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todo_row", rowFixture("pending")) // Source: github
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	// github ships a brand SVG, so the row renders the icon tile — pin the icon, not just the class,
	// so a broken iconpath lookup silently falling back can never pass this assertion.
	if !strings.Contains(out, `<img src="/static/icons/brands/github.svg"`) {
		t.Errorf("todo_row: missing github provider icon: %q", out)
	}
	// A source with no brand SVG keeps the two-letter tag tile — the fallback branch stays covered.
	noIcon := rowFixture("pending")
	noIcon.Source = "healthchecks"
	out2, err := h.renderFragment("todo_row", noIcon)
	if err != nil {
		t.Fatalf("render todo_row (no icon): %v", err)
	}
	if !strings.Contains(out2, `data-sb-tag aria-hidden="true">HL</span>`) {
		t.Errorf("todo_row: iconless source must fall back to the two-letter tag: %q", out2)
	}
	bare := rowFixture("pending")
	bare.Source = ""
	out, err = h.renderFragment("todo_row", bare)
	if err != nil {
		t.Fatalf("render todo_row (no source): %v", err)
	}
	if strings.Contains(out, "data-sb-tag") {
		t.Errorf("todo_row without a source must not render a tag tile: %q", out)
	}
}

// TestTodoRowStampsTrustAndState pins the data-sb-trust/data-sb-state stamps on the row's trust
// badge and state chip — the attribute contract the DOM tests and any future JS key on instead
// of styling classes. Both chips also carry their mode as readable TEXT (trust and state legible
// without color). Governing: SPEC-0015 REQ "Todos View And Drawer" (table columns), REQ "Design
// Token System" (trust/state as first-class vocabulary).
func TestTodoRowStampsTrustAndState(t *testing.T) {
	h := newTestHandler(t)
	for _, state := range []string{"pending", "claimed", "done", "failed"} {
		out, err := h.renderFragment("todo_row", rowFixture(state))
		if err != nil {
			t.Fatalf("render todo_row(%s): %v", state, err)
		}
		if !strings.Contains(out, `data-sb-trust="signed"`) || !strings.Contains(out, ">signed</span>") {
			t.Errorf("%s row: trust badge must carry the data-sb-trust stamp and the mode text: %q", state, out)
		}
		// The trust badge tooltip carries the shared one-line definition (#84).
		if !strings.Contains(out, `title="`+trustDefs["signed"]+`"`) {
			t.Errorf("%s row: trust badge missing the shared definition tooltip: %q", state, out)
		}
		if !strings.Contains(out, `data-sb-state="`+state+`"`) || !strings.Contains(out, ">"+state+"</span>") {
			t.Errorf("%s row: state chip must carry the data-sb-state stamp and the state text: %q", state, out)
		}
	}
}

// TestTodoPillsActiveStateAcrossFilters rounds out the filter-chip coverage: every one of the
// five chips (all/pending/claimed/done/failed) becomes the single active chip for its filter,
// with aria-selected="true" and its canonical data-sb-filter stamp. todos_test.go covers
// all/failed; this covers the rest, including done which no other test exercises.
func TestTodoPillsActiveStateAcrossFilters(t *testing.T) {
	h := newTestHandler(t)
	for _, filter := range []string{"all", "pending", "claimed", "done", "failed"} {
		out, err := h.renderFragment("todo_pills", panelView{Filter: filter, Counts: store.TodoCounts{}})
		if err != nil {
			t.Fatalf("render todo_pills(%s): %v", filter, err)
		}
		// Every chip carries its canonical filter stamp; exactly one is selected.
		for _, key := range todoFilters {
			if !strings.Contains(out, `data-sb-filter="`+key+`"`) {
				t.Errorf("filter %q: missing chip stamp data-sb-filter=%q", filter, key)
			}
		}
		if c := strings.Count(out, `aria-selected="true"`); c != 1 {
			t.Errorf("filter %q: %d chips marked selected, want 1", filter, c)
		}
		// The selected chip is the one for THIS filter: scope to its anchor (stamp → </a>) and
		// assert aria-selected inside it, plus the lowercase label and the stable count-span id.
		idx := strings.Index(out, `data-sb-filter="`+filter+`"`)
		if idx < 0 {
			t.Fatalf("filter %q: missing its own chip anchor", filter)
		}
		chip := out[idx:]
		chip = chip[:strings.Index(chip, "</a>")]
		if !strings.Contains(chip, `aria-selected="true"`) {
			t.Errorf("filter %q: its chip must carry aria-selected=true: %q", filter, chip)
		}
		if !strings.Contains(chip, ">"+filter+" <span id=\"sb-tc-"+filter+"\"") {
			t.Errorf("filter %q: missing lowercase label + count target: %q", filter, chip)
		}
	}
}

// TestDrawerTimelineStepStamps pins the glyph timeline's attribute contract: the four lifecycle
// steps carry canonical data-sb-step names inside the data-sb-timeline list, and resolution is
// expressed by data-sb-done (never only by a styling class). A claimed-but-unfinished todo
// resolves exactly received/created/claimed. Governing: SPEC-0015 REQ "Todos View And Drawer"
// (lifecycle timeline).
func TestDrawerTimelineStepStamps(t *testing.T) {
	h := newTestHandler(t)
	claimed := time.Now().Add(-2 * time.Minute)
	out, err := h.renderFragment("drawer", drawerView{
		Row: rowFixture("claimed"), IdempotencyKey: "k",
		HasEvent: true, ReceivedAt: time.Now().Add(-4 * time.Minute), ClaimedAt: &claimed,
	})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if !strings.Contains(out, "data-sb-timeline") {
		t.Errorf("drawer: missing timeline list stamp: %q", out)
	}
	for _, step := range []string{"received", "created", "claimed", "resolved"} {
		if !strings.Contains(out, `data-sb-step="`+step+`"`) {
			t.Errorf("drawer timeline: missing step %q", step)
		}
	}
	if n := strings.Count(out, "data-sb-done"); n != 3 {
		t.Errorf("claimed drawer: %d resolved steps, want 3 (received/created/claimed): %q", n, out)
	}
}

// TestTodoRowInsertPairForCreation pins the live-insertion contract behind #96: a row rendered
// with Insert (todo_created / todo_resurfaced frames) is the lane_move idiom — an idempotent OOB
// delete of the stable row id followed by the row itself inserted afterbegin into
// #sb-todos-body, stamped data-sb-live-row for the sb-live.js filter sweep. A plain OOB render
// (every other transition) stays an id-targeted replacement and never carries the insertion
// artifacts, because an in-place update must not move the row. Governing: SPEC-0015 REQ "Todos
// View And Drawer", REQ "Live Fragment Architecture" (OOB removal + insertion).
func TestTodoRowInsertPairForCreation(t *testing.T) {
	h := newTestHandler(t)

	ins := rowFixture("pending")
	ins.OOB, ins.Insert = true, true
	out, err := h.renderFragment("todo_row", ins)
	if err != nil {
		t.Fatalf("render todo_row (insert): %v", err)
	}
	for _, want := range []string{
		`<tr id="sb-tr-` + ins.ID + `" hx-swap-oob="delete"></tr>`, // delete no-ops when absent — the pair is idempotent
		`hx-swap-oob="afterbegin:#sb-todos-body"`,
		`data-sb-live-row`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("insert row missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, `hx-swap-oob="true"`) {
		t.Errorf("insert row must not also render the id-replacement swap: %q", out)
	}

	upd := rowFixture("claimed")
	upd.OOB = true
	out, err = h.renderFragment("todo_row", upd)
	if err != nil {
		t.Fatalf("render todo_row (update): %v", err)
	}
	if !strings.Contains(out, `hx-swap-oob="true"`) {
		t.Errorf("update row must keep the id-targeted replacement swap: %q", out)
	}
	for _, forbid := range []string{`hx-swap-oob="delete"`, "afterbegin", "data-sb-live-row"} {
		if strings.Contains(out, forbid) {
			t.Errorf("update row must not carry insertion artifact %q: %q", forbid, out)
		}
	}
}

// A held todo (queue quarantine) is the owner's to see, but never claimable work: the table row,
// the drawer and the Board lane card offer no Claim, and the drawer points at the Quarantine view.
// Governing: SPEC-0026 REQ-6 (never offered as claimable work), REQ-9.
func TestHeldTodoOffersNoLifecycleControls(t *testing.T) {
	h := newTestHandler(t)
	held := store.TodoItem{Todo: store.Todo{ID: "td_held", Queue: store.QueueQuarantine, Source: "github", Kind: "issues",
		State: "pending", CreatedAt: time.Now()}, TrustMode: "signed"}
	row := todoRow{ID: held.ID, ShortID: "held", State: "pending", Held: true, CreatedAt: held.CreatedAt}
	if out := renderFrag(t, h, "todo_row", row); strings.Contains(out, "/claim") || !strings.Contains(out, "data-sb-held") {
		t.Errorf("held row offers a lifecycle control or no held marker: %q", out)
	}
	if out := renderFrag(t, h, "drawer", drawerView{Row: row}); strings.Contains(out, "/claim") || !strings.Contains(out, "Quarantine view") {
		t.Errorf("held drawer offers Claim or no pointer to the Quarantine view: %q", out)
	}
	card := laneCardFromItem(held)
	if !card.Held {
		t.Fatal("lane card from a quarantine item is not marked held")
	}
	if out := renderFrag(t, h, "lane_card", card); strings.Contains(out, "/claim") {
		t.Errorf("held lane card offers Claim: %q", out)
	}
	// An ordinary pending todo still does.
	row.Held = false
	if out := renderFrag(t, h, "todo_row", row); !strings.Contains(out, "/claim") {
		t.Errorf("pending row lost its Claim: %q", out)
	}
}
