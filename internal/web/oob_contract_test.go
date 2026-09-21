package web

// Out-Of-Band Swap Contract
//
// The live layer ships HTML fragments over SSE and lets htmx place them out of band. Two rules of
// that pipeline are invisible to a strings.Contains assertion, and breaking either one fails
// SILENTLY in the browser — the markup still "contains" everything the older tests look for:
//
//  1. For every hx-swap-oob strategy except outerHTML ("true"), htmx inserts the out-of-band
//     element's CHILDREN and throws the element itself away. So the attribute must sit on a
//     throwaway carrier, never on the node we want in the page.
//  2. A frame is several fragments concatenated, and it opens with an <li>. That parks the HTML
//     parser in "in body" mode, where <tr>/<td>/<tbody> start tags are parse errors and are
//     dropped. Table markup therefore has to travel inside its own <template>, which gets a fresh
//     insertion mode and which htmx unwraps.
//
// There is no browser in CI, so these tests encode the two rules directly. Both were verified
// against the vendored htmx 2.0.6 in a real browser when they were written: with the rules broken
// a toast landed as a bare icon plus a loose text node, a lane card as three anonymous <div>s, and
// a todo row not at all.
//
// @joestump 09/21/2026 - Added after an outside self-hoster reported the Board's toasts rendering
// as unstyled, ever-growing text; the same two faults turned out to break lane cards and rows.
//
// Governing: SPEC-0015 REQ "Live Fragment Architecture" (OOB removal + insertion), ADR-0018.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"
)

// insertionOOB matches a tag carrying an hx-swap-oob strategy whose wrapper htmx discards. Groups 1
// and 3 are whatever else sits on that tag — for a carrier, nothing.
var insertionOOB = regexp.MustCompile(
	`<[a-z]+([^<>]*?)\s*hx-swap-oob="(afterbegin|beforeend|beforebegin|afterend|innerHTML):[^"]*"([^<>]*)>`)

// assertCarriersOnly fails for every insertion-strategy OOB tag in html that carries any other
// attribute: an id, a class, or a data-* stamp on that tag is markup htmx will throw away.
func assertCarriersOnly(t *testing.T, where, html string) {
	t.Helper()
	for _, m := range insertionOOB.FindAllStringSubmatch(html, -1) {
		if extra := strings.TrimSpace(m[1] + m[3]); extra != "" {
			t.Errorf("%s: hx-swap-oob=%q sits on a tag that also carries %q — htmx discards that tag "+
				"and inserts only its children; move the attribute onto a bare carrier element:\n  %s",
				where, m[2], extra, m[0])
		}
	}
}

// Rule 1, over the template SOURCES, so a fragment no test happens to render is still covered.
func TestOOBInsertionsUseBareCarriers(t *testing.T) {
	found := 0
	err := fs.WalkDir(tmplFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		src, err := fs.ReadFile(tmplFS, path)
		if err != nil {
			return err
		}
		// Template actions inside a tag (`{{if .OOB}} hx-swap-oob=…{{end}}`) are exactly how the
		// attribute ended up on a real element; strip the action delimiters but keep their text so
		// the conditional attribute is judged as part of its tag.
		flat := strings.NewReplacer("{{", " ", "}}", " ").Replace(string(src))
		found += len(insertionOOB.FindAllString(flat, -1))
		assertCarriersOnly(t, path, flat)
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	// A zero here means the scan looked at nothing — a moved templates dir or a regex that no
	// longer matches — and a guard that matches nothing passes forever.
	if found < 3 {
		t.Fatalf("scanned templates and found %d insertion-style OOB tags, want at least the toast, "+
			"lane card and todo row carriers — the scan is not seeing the templates", found)
	}
}

// liveFrame renders one todo-transition frame in the order publishTodoTransition concatenates it:
// lane_move, then todo_row, then toast.
func liveFrame(t *testing.T, h *Handler, row todoRow) string {
	t.Helper()
	card := laneCard{
		DomID: "sb-lc-td1", Lane: "verified", State: "pending", Source: "gitea", Kind: "pull_request",
		Title: "PR #1", TrustMode: "signed", TodoID: "td1", At: time.Now(), OOB: true,
	}
	return renderFrag(t, h, "lane_move", laneMove{RemoveID: "sb-lc-td1", Card: card}) +
		renderFrag(t, h, "todo_row", row) +
		renderFrag(t, h, "toast", toastMsg{Kind: "resurfaced", Text: "td_0001 · re-surfaced to queue"})
}

var tableTag = regexp.MustCompile(`<(/?)(template|tbody|tr|td)\b`)

// Rules 1 and 2, over real rendered frames.
func TestLiveFrameSurvivesParsingAndOOBStripping(t *testing.T) {
	h := newTestHandler(t)
	base := todoRow{
		ID: "td1", ShortID: "td_0001", Source: "gitea", Kind: "pull_request", TrustMode: "signed",
		State: "pending", MaxAttempts: 3, CreatedAt: time.Now(), OOB: true,
	}
	insert := base
	insert.Insert = true

	for name, row := range map[string]todoRow{"update": base, "insert": insert} {
		t.Run(name, func(t *testing.T) {
			frame := liveFrame(t, h, row)
			assertCarriersOnly(t, name+" frame", frame)

			if !strings.HasPrefix(frame, "<li") {
				t.Fatalf("frame no longer opens with the lane_move <li> — the body-mode premise of "+
					"this test changed, re-derive rule 2: %.80q", frame)
			}
			// Rule 2: no table markup at template depth 0.
			depth, sawRow := 0, false
			for _, m := range tableTag.FindAllStringSubmatch(frame, -1) {
				closing, tag := m[1] == "/", m[2]
				sawRow = sawRow || (tag == "tr" && !closing)
				switch {
				case tag == "template" && !closing:
					depth++
				case tag == "template" && closing:
					depth--
				case !closing && depth == 0:
					t.Errorf("<%s> at the top level of a frame that opens with <li>: the HTML parser "+
						"drops it in body mode, so htmx never sees the row — wrap it in <template>", tag)
				}
			}
			if depth != 0 {
				t.Errorf("unbalanced <template> wrappers in frame (depth %d)", depth)
			}
			if !sawRow {
				t.Error("frame carried no <tr> at all — the todo_row fragment did not render")
			}

			// The nodes we want in the page keep their identity: id and class stay on the element
			// that survives, not on a carrier.
			for _, want := range []string{
				`<li id="sb-lc-td1" class="sb-lcard`,        // lane card
				`<tr id="sb-tr-td1" class="sb-trow`,         // table row
				`<div class="sb-toast sb-toast--resurfaced`, // toast
			} {
				if !strings.Contains(frame, want) {
					t.Errorf("frame lost %q", want)
				}
			}
			if row.Insert {
				// Delete must precede the insertion: both address the same id, and htmx processes
				// nested templates in document order.
				del := strings.Index(frame, `<tr id="sb-tr-td1" hx-swap-oob="delete">`)
				ins := strings.Index(frame, `<tbody hx-swap-oob="afterbegin:#sb-todos-body">`)
				if del < 0 || ins < 0 || del > ins {
					t.Errorf("insert frame wants delete (%d) before the carrier <tbody> (%d)", del, ins)
				}
			}
		})
	}
}
