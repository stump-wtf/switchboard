package web

// Template render coverage for the SPEC-0015 Todos view and detail drawer (charm-web): filter
// chips with live count targets, the durable-queue table columns (dedup badge, lease/retry
// sub-lines, contextual actions), and the drawer (lease card, escaped payload, timeline,
// state-appropriate footer). These render fragments and pages without a database, exercising the
// pure render models. DOM assertions key on data-sb-* attributes and stable ids (ADR-0018).

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// pendingRow / claimedRow / failedRow build todoRow render models without a store.
func rowFixture(state string) todoRow {
	r := todoRow{
		ID: "td_" + state + "0000", ShortID: shortID("td_" + state + "0000"),
		Source: "github", Kind: "push", TrustMode: "signed", State: state,
		Attempt: 1, MaxAttempts: 5, CreatedAt: time.Now().Add(-4 * time.Minute),
	}
	switch state {
	case "claimed":
		r.OwnerLabel = "agent · reviewer-bot"
		r.HasLease = true
		r.LeaseSecs = 240
		r.LeaseDeadlineMS = time.Now().Add(4 * time.Minute).UnixMilli()
	case "failed":
		r.Attempt = 5
	}
	return r
}

func TestTodosPageRendersPillsAndTable(t *testing.T) {
	h := newTestHandler(t)
	rows := []todoRow{rowFixture("pending"), rowFixture("claimed")}
	rows[1].DedupCount = 3
	body := renderPage(t, h, "todos", view{
		Title: "Todos", Human: testHuman(), CSRF: "tok",
		Shell:     shell{Active: "todos", DBConnected: true, Initials: "JS"},
		Counts:    store.TodoCounts{All: 12, Pending: 5, Claimed: 3, Done: 3, Failed: 1},
		Filter:    "all",
		Query:     "",
		TodoItems: rows,
	})
	for _, want := range []string{
		`href="/todos" aria-current="page"`, // rail marks Todos active
		`id="sb-todos-panel"`,               // HTMX filter/search swap target
		`id="sb-todos-search"`,              // search box (focus-restored across swaps)
		`data-sb-filter="all" href="/todos?filter=all" hx-get="/todos?filter=all" hx-target="#sb-todos-panel" hx-swap="innerHTML" hx-include="#sb-todos-search" hx-push-url="true" role="tab" aria-selected="true"`, // all chip active by default
		`id="sb-tc-pending"`, `id="sb-tc-failed"`, // pill-count SSE targets exist before first frame
		">5</span>",                                // pending count
		"dedup ×3",                                 // dedup badge on the claimed row
		"agent · reviewer-bot",                     // resolved agent label
		`data-sb-deadline=`,                        // lease countdown deadline stamp
		`hx-post="/todos/td_pending0000/claim"`,    // pending row Claim action
		`hx-post="/todos/td_claimed0000/complete"`, // claimed row Complete action
		`hx-get="/todos/td_pending0000"`,           // id cell opens the drawer
		`hx-target="#sb-overlay"`,                  // into the overlay slot
	} {
		if !strings.Contains(body, want) {
			t.Errorf("todos page: missing %q", want)
		}
	}
}

func TestTodosPanelFragmentPreservesFilterAndQuery(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("todos_panel", panelView{
		Filter: "failed", Query: "stripe",
		Counts: store.TodoCounts{All: 4, Failed: 2},
		Rows:   []todoRow{rowFixture("failed")},
	})
	if err != nil {
		t.Fatalf("render todos_panel: %v", err)
	}
	for _, want := range []string{
		`value="stripe"`,                       // search text preserved
		`value="failed"`,                       // hidden filter preserved for the search form
		`href="/todos?filter=failed"`,          // Failed pill link
		"5 attempts",                           // failed sub-line (attempt/max)
		`hx-post="/todos/td_failed0000/retry"`, // failed row Retry action
	} {
		if !strings.Contains(out, want) {
			t.Errorf("todos_panel: missing %q in %q", want, out)
		}
	}
	// The failed chip must be the active one (aria-selected on its own anchor).
	chip := out[strings.Index(out, `data-sb-filter="failed"`):]
	chip = chip[:strings.Index(chip, "</a>")]
	if !strings.Contains(chip, `aria-selected="true"`) || !strings.Contains(chip, `failed <span id="sb-tc-failed"`) {
		t.Errorf("todos_panel: failed chip not marked active: %q", chip)
	}
}

func TestTodoRowLiveVariants(t *testing.T) {
	h := newTestHandler(t)
	// OOB replacement (live SSE) carries hx-swap-oob and the stable row id.
	oob, err := h.renderFragment("todo_row", todoRow{ID: "td_x", ShortID: "td_x", Source: "github", Kind: "push", TrustMode: "queue", State: "pending", OOB: true})
	if err != nil {
		t.Fatalf("render todo_row: %v", err)
	}
	if !strings.Contains(oob, `id="sb-tr-td_x"`) || !strings.Contains(oob, `hx-swap-oob="true"`) {
		t.Errorf("oob todo_row wrong: %q", oob)
	}
	// A reaper re-surface flags the row to flash; a server-rendered row does not.
	flash, _ := h.renderFragment("todo_row", todoRow{ID: "td_y", ShortID: "td_y", State: "pending", Flash: true})
	if !strings.Contains(flash, "data-sb-flash") {
		t.Errorf("flash row missing data-sb-flash stamp: %q", flash)
	}
	plain, _ := h.renderFragment("todo_row", todoRow{ID: "td_z", ShortID: "td_z", State: "pending"})
	if strings.Contains(plain, "data-sb-flash") || strings.Contains(plain, "hx-swap-oob") {
		t.Errorf("plain row must not flash or be OOB: %q", plain)
	}
}

func TestDrawerRendersLeaseCardAndFooterByState(t *testing.T) {
	h := newTestHandler(t)
	claimed := drawerView{
		Row: rowFixture("claimed"), IdempotencyKey: "gh-delivery-123",
		PayloadJSON: "{\n  \"action\": \"opened\"\n}", HasEvent: true,
		ReceivedAt: time.Now().Add(-5 * time.Minute), CSRF: "tok",
	}
	claimed.Row.DedupCount = 2
	out, err := h.renderFragment("drawer", claimed)
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	for _, want := range []string{
		`role="dialog"`, `aria-modal="true"`, // dialog semantics
		`data-sb-drawer`, `data-sb-close`, // focus-trap root + close control
		"sb-lease", `data-sb-countdown`, // lease card + countdown
		`hx-post="/todos/td_claimed0000/extend"`,   // Extend lease
		`hx-post="/todos/td_claimed0000/release"`,  // Release
		`hx-post="/todos/td_claimed0000/complete"`, // footer Complete
		`hx-post="/todos/td_claimed0000/fail"`,     // footer Fail
		"gh-delivery-123", "dedup ×2",              // idempotency key + dedup line
		"sb-timeline", // lifecycle timeline
		"action",      // payload content present
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drawer (claimed): missing %q", want)
		}
	}

	// Failed drawer shows the retry footer and dead-letter callout, no lease card.
	failed, _ := h.renderFragment("drawer", drawerView{Row: rowFixture("failed"), IdempotencyKey: "k"})
	if !strings.Contains(failed, `hx-post="/todos/td_failed0000/retry"`) {
		t.Errorf("drawer (failed): missing Retry footer: %q", failed)
	}
	if strings.Contains(failed, "sb-lease") {
		t.Errorf("drawer (failed): must not render a lease card")
	}

	// Pending drawer shows only the Claim footer.
	pending, _ := h.renderFragment("drawer", drawerView{Row: rowFixture("pending"), IdempotencyKey: "k"})
	if !strings.Contains(pending, `hx-post="/todos/td_pending0000/claim"`) {
		t.Errorf("drawer (pending): missing Claim footer: %q", pending)
	}
}

// The drawer payload must render inert: HTML/script inside the todo payload becomes escaped text.
func TestDrawerPayloadIsEscaped(t *testing.T) {
	h := newTestHandler(t)
	hostile := prettyJSON([]byte(`{"x":"</pre><script>alert(1)</script>"}`))
	out, err := h.renderFragment("drawer", drawerView{
		Row: rowFixture("pending"), IdempotencyKey: "k", PayloadJSON: hostile,
	})
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Errorf("drawer: payload rendered live markup: %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("drawer: expected escaped payload form: %q", out)
	}
}

// The standalone drawer page fallback renders the same drawer inside the shell.
func TestTodoStandalonePageRenders(t *testing.T) {
	h := newTestHandler(t)
	dv := drawerView{Row: rowFixture("pending"), IdempotencyKey: "k", PayloadJSON: "{}"}
	body := renderPage(t, h, "todo", view{
		Title: "Todo td_pend", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "todos", DBConnected: true, Initials: "JS"}, Drawer: &dv,
	})
	if !strings.Contains(body, "sb-drawer-inline") || !strings.Contains(body, `href="/todos"`) {
		t.Errorf("standalone todo page missing inline drawer or backlink")
	}
	if !strings.Contains(body, `<main class="sb-main">`) {
		t.Errorf("standalone todo page missing main landmark (shell)")
	}
}

func TestNormalizeAndFilterState(t *testing.T) {
	cases := map[string]string{"": "all", "PENDING": "pending", "bogus": "all", "failed": "failed"}
	for in, want := range cases {
		if got := normalizeFilter(in); got != want {
			t.Errorf("normalizeFilter(%q) = %q, want %q", in, got, want)
		}
	}
	if filterState("all") != "" {
		t.Errorf("filterState(all) should be empty (no state filter)")
	}
	if filterState("failed") != "failed" {
		t.Errorf("filterState(failed) should pass through")
	}
}

func TestPrettyJSON(t *testing.T) {
	if got := prettyJSON(nil); got != "(no payload)" {
		t.Errorf("prettyJSON(nil) = %q", got)
	}
	if got := prettyJSON([]byte(`not json`)); got != "not json" {
		t.Errorf("prettyJSON(non-json) should pass through raw, got %q", got)
	}
	if got := prettyJSON([]byte(`{"a":1}`)); !strings.Contains(got, "\n  \"a\": 1") {
		t.Errorf("prettyJSON should indent: %q", got)
	}
}
