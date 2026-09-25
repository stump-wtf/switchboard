package web

// Drawer Attempt History Tests
//
// The todo drawer lists a todo's attempts. These tests render the drawer and the live refresh with
// no database and pin what SPEC-0034 asks of them: newest first, a died marker that is a glyph and
// a word with an aria-label, the last heartbeat on a death, agent-written text rendered inert, an
// artifact that is a link only when it is https, a heading and an aria-live region inside the
// dialog, and a live swap that obeys the htmx carrier rules. The owning-human scope is proven
// against Postgres in internal/server (todo_drawer_attempts_test.go) and internal/store.
//
// Governing: SPEC-0034 REQ-4, REQ-13 "Operator Surfaces", Accessibility Requirements.
//
// @joestump-agent 09/25/2026 - Added for #329 (epic #313).

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// attemptDrawer renders the drawer of a claimed todo whose history is as (newest first).
func attemptDrawer(t *testing.T, h *Handler, as []store.Attempt) (string, drawerView) {
	t.Helper()
	row := rowFixture("claimed")
	dv := drawerView{Row: row, IdempotencyKey: "k", Attempts: attemptList{
		TodoID: row.ID, Rows: attemptRows(as), Total: len(as),
	}}
	out, err := h.renderFragment("drawer", dv)
	if err != nil {
		t.Fatalf("render drawer: %v", err)
	}
	return out, dv
}

func tp(t time.Time) *time.Time { return &t }

// reapedHistory is a todo whose attempt 2 was reaped after one heartbeat, between a failed attempt
// 1 and an open attempt 3.
func reapedHistory(hb time.Time) []store.Attempt {
	now := time.Now()
	return []store.Attempt{
		{Seq: 3, Attempt: 1, ClaimerKind: "endpoint", Claimant: "fixer/run-3", ClaimedAt: now.Add(-time.Minute)},
		{Seq: 2, Attempt: 2, ClaimerKind: "endpoint", Claimant: "fixer/run-2", ClaimedAt: now.Add(-time.Hour),
			LastHeartbeatAt: &hb, EndedAt: tp(now.Add(-30 * time.Minute)), Outcome: "reaped",
			Disposition: "requeued", Died: true},
		{Seq: 1, Attempt: 1, ClaimerKind: "owner", ClaimedAt: now.Add(-2 * time.Hour),
			EndedAt: tp(now.Add(-90 * time.Minute)), Outcome: "failed", Disposition: "retry_scheduled",
			Summary: "tests still red"},
	}
}

// attemptItem returns the <li> of one attempt from a rendered drawer.
func attemptItem(t *testing.T, out string, seq string) string {
	t.Helper()
	start := strings.Index(out, `data-sb-attempt="`+seq+`"`)
	if start < 0 {
		t.Fatalf("attempt %s not rendered", seq)
	}
	end := strings.Index(out[start:], "</li>")
	return out[start : start+end]
}

// SPEC-0034 REQ-13 "Drawer shows a death": the reaped attempt shows its outcome, the died marker
// and its last heartbeat time; the list is newest first; a report-less attempt carries no marker.
func TestDrawerShowsReapedAttemptWithDiedMarkerAndHeartbeat(t *testing.T) {
	h := newTestHandler(t)
	hb := time.Date(2026, 9, 25, 14, 1, 0, 0, time.UTC)
	out, _ := attemptDrawer(t, h, reapedHistory(hb))

	reaped := attemptItem(t, out, "2")
	for _, want := range []string{
		`data-sb-outcome="reaped"`,
		`reaped → requeued`,
		`aria-label="died: no report"`,
		`data-sb-died>✝ died</span>`, // a glyph and a word: never colour alone
		`last heartbeat <time datetime="2026-09-25T14:01:00Z">`,
		`fixer/run-2`,
	} {
		if !strings.Contains(reaped, want) {
			t.Errorf("reaped attempt missing %q:\n%s", want, reaped)
		}
	}
	for _, seq := range []string{"1", "3"} {
		if strings.Contains(attemptItem(t, out, seq), "data-sb-died") {
			t.Errorf("attempt %s did not die but carries the died marker", seq)
		}
	}
	if open := attemptItem(t, out, "3"); !strings.Contains(open, `data-sb-outcome="open"`) || !strings.Contains(open, "in progress") {
		t.Errorf("open attempt should read as in progress:\n%s", open)
	}
	if owner := attemptItem(t, out, "1"); !strings.Contains(owner, "owner · Board") || !strings.Contains(owner, "tests still red") {
		t.Errorf("owner attempt should name the Board claim and keep its summary:\n%s", owner)
	}
	i3, i2, i1 := strings.Index(out, `data-sb-attempt="3"`), strings.Index(out, `data-sb-attempt="2"`), strings.Index(out, `data-sb-attempt="1"`)
	if !(i3 < i2 && i2 < i1) {
		t.Errorf("attempts not newest first: seq3@%d seq2@%d seq1@%d", i3, i2, i1)
	}

	// A death with no heartbeat says so rather than leaving the reader to infer it.
	silent := []store.Attempt{{Seq: 1, ClaimerKind: "endpoint", ClaimedAt: time.Now(), EndedAt: tp(time.Now()),
		Outcome: "lease_expired", Disposition: "requeued", Died: true}}
	out, _ = attemptDrawer(t, h, silent)
	if a := attemptItem(t, out, "1"); !strings.Contains(a, `aria-label="died: no report"`) || !strings.Contains(a, "no heartbeat") {
		t.Errorf("lease_expired attempt without a heartbeat:\n%s", a)
	}
}

// SPEC-0034 REQ-13 "Summary markup is inert": agent-written text never becomes markup, and an
// artifact that is not https is never a link.
func TestDrawerAttemptTextIsInert(t *testing.T) {
	h := newTestHandler(t)
	out, _ := attemptDrawer(t, h, []store.Attempt{{
		Seq: 1, ClaimerKind: "endpoint", Claimant: `"><img src=x onerror=alert(2)>`, ClaimedAt: time.Now(),
		EndedAt: tp(time.Now()), Outcome: "failed", Disposition: "retry_scheduled",
		Summary: `<script>alert(1)</script>`, Artifact: `javascript:alert(3)`,
	}})
	for _, bad := range []string{"<script>alert(1)</script>", "<img src=x", `href="javascript:`} {
		if strings.Contains(out, bad) {
			t.Errorf("drawer rendered agent-written %q as markup", bad)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the summary should still be shown, as literal escaped text")
	}
	if !strings.Contains(out, `<code class="sb-attempt__artifact" data-sb-artifact>javascript:alert(3)</code>`) {
		t.Errorf("a non-https artifact should render as text:\n%s", attemptItem(t, out, "1"))
	}
}

// SPEC-0034 REQ-13 and Security "Redirect Validation": only an absolute https URL is a link, with
// rel="noopener noreferrer"; an mcp://cairn handle and an http URL are text.
func TestDrawerAttemptArtifactLinks(t *testing.T) {
	for _, tc := range []struct {
		artifact string
		link     bool
	}{
		{"https://cairn.example.com/a/Zz9", true},
		{"mcp://cairn/Zz9", false},
		{"http://example.com/run", false},
		{"https:relative", false},
		{"//example.com/x", false},
	} {
		if got := httpsArtifact(tc.artifact) != ""; got != tc.link {
			t.Errorf("httpsArtifact(%q) link = %v, want %v", tc.artifact, got, tc.link)
		}
	}

	h := newTestHandler(t)
	out, _ := attemptDrawer(t, h, []store.Attempt{
		{Seq: 2, ClaimerKind: "endpoint", ClaimedAt: time.Now(), EndedAt: tp(time.Now()), Outcome: "completed",
			Disposition: "done", Artifact: "https://cairn.example.com/a/Zz9"},
		{Seq: 1, ClaimerKind: "endpoint", ClaimedAt: time.Now(), EndedAt: tp(time.Now()), Outcome: "failed",
			Disposition: "retry_scheduled", Artifact: "mcp://cairn/Zz9"},
	})
	if a := attemptItem(t, out, "2"); !strings.Contains(a,
		`<a class="sb-attempt__artifact" href="https://cairn.example.com/a/Zz9" rel="noopener noreferrer" data-sb-artifact>`) {
		t.Errorf("https artifact should be a noopener link:\n%s", a)
	}
	if a := attemptItem(t, out, "1"); strings.Contains(a, "<a ") || !strings.Contains(a, "<code") {
		t.Errorf("mcp://cairn handle should be text:\n%s", a)
	}
}

var tagRe = regexp.MustCompile(`<([a-z0-9]+)\b[^>]*>`)

// SPEC-0034 Accessibility Requirements: the list sits inside the dialog under a visible heading
// the section is labelled by, inside an aria-live="polite" region; the died marker has its
// aria-label; artifact links stay in tab order.
func TestDrawerAttemptsAccessibility(t *testing.T) {
	h := newTestHandler(t)
	hist := reapedHistory(time.Now().Add(-40 * time.Minute))
	hist[0].Artifact = "https://cairn.example.com/a/Zz9"
	out, dv := attemptDrawer(t, h, hist)
	id := dv.Row.ID

	dialog := strings.Index(out, `role="dialog"`)
	heading := strings.Index(out, `<h2 class="sb-microlabel sb-attempts__heading" id="sb-attempts-h-`+id+`">attempt history</h2>`)
	section := strings.Index(out, `aria-labelledby="sb-attempts-h-`+id+`"`)
	region := strings.Index(out, `<div id="sb-attempts-`+id+`" aria-live="polite" data-sb-attempts>`)
	list := strings.Index(out, `<ol class="sb-attempts__list">`)
	if dialog < 0 || heading < 0 || section < 0 || region < 0 || list < 0 {
		t.Fatalf("missing landmark: dialog@%d heading@%d labelled-section@%d live-region@%d list@%d",
			dialog, heading, section, region, list)
	}
	if !(dialog < section && section < heading && heading < region && region < list) {
		t.Errorf("want dialog > section > heading, then live region > list; got %d %d %d %d %d",
			dialog, section, heading, region, list)
	}
	if !strings.Contains(out, `role="img" aria-label="died: no report"`) {
		t.Error("died marker lacks its accessible name")
	}
	// Tab order: nothing in the history removes itself from it, and the artifact is a real link.
	hist2 := out[region:]
	for _, m := range tagRe.FindAllStringSubmatch(hist2, -1) {
		if strings.Contains(m[0], `tabindex="-`) {
			t.Errorf("attempt history takes an element out of tab order: %s", m[0])
		}
	}
	if !strings.Contains(hist2, `<a class="sb-attempt__artifact" href="https://cairn.example.com/a/Zz9"`) {
		t.Error("artifact link should be a focusable <a href>")
	}
}

// The render model cannot carry what REQ-13 forbids: no field of attemptRow names a session, a
// lease token or the claimer endpoint, so no template change can print one.
func TestAttemptRowHasNoSecretFields(t *testing.T) {
	rt := reflect.TypeOf(attemptRow{})
	for i := range rt.NumField() {
		name := strings.ToLower(rt.Field(i).Name)
		for _, bad := range []string{"session", "token", "hash", "endpoint"} {
			if strings.Contains(name, bad) {
				t.Errorf("attemptRow.%s looks like %s data the drawer must never render", rt.Field(i).Name, bad)
			}
		}
	}
}

// Empty, unavailable and paged histories each say what they are.
func TestDrawerAttemptListStates(t *testing.T) {
	h := newTestHandler(t)
	for name, tc := range map[string]struct {
		al   attemptList
		want string
	}{
		"empty":       {attemptList{TodoID: "td_1"}, "no attempts yet"},
		"unavailable": {attemptList{TodoID: "td_1", Unavailable: true}, "attempt history unavailable"},
		"paged": {attemptList{TodoID: "td_1", Total: 60, Hidden: 40,
			Rows: attemptRows([]store.Attempt{{Seq: 60, ClaimedAt: time.Now()}})}, "40 earlier attempts not shown · 60 in all"},
	} {
		if out := renderFrag(t, h, "attempt_list", tc.al); !strings.Contains(out, tc.want) {
			t.Errorf("%s: want %q in %s", name, tc.want, out)
		}
	}
}

// The live refresh obeys the carrier rules oob_contract_test.go pins: an innerHTML swap on a bare
// <div>, aimed at the drawer's live region, carrying no table markup; and a frame that appends it
// after the row still parses.
func TestAttemptsOOBSwapIsABareCarrierIntoTheLiveRegion(t *testing.T) {
	h := newTestHandler(t)
	out, dv := attemptDrawer(t, h, reapedHistory(time.Now()))
	oob := renderFrag(t, h, "attempts_oob", dv.Attempts)

	want := `<div hx-swap-oob="innerHTML:#sb-attempts-` + dv.Row.ID + `">`
	if !strings.HasPrefix(oob, want) {
		t.Fatalf("attempts_oob should open with the bare carrier %q: %.120q", want, oob)
	}
	if !strings.Contains(out, `id="sb-attempts-`+dv.Row.ID+`"`) {
		t.Fatal("the drawer has no element the live swap can target")
	}
	assertCarriersOnly(t, "attempts_oob", oob)
	if tableTag.MatchString(oob) {
		t.Error("attempts_oob carries table markup, which body-mode parsing would drop")
	}
	if !strings.Contains(oob, `aria-label="died: no report"`) {
		t.Error("the live refresh must render the same rows as the drawer")
	}
	frame := liveFrame(t, h, rowFixture("claimed")) + oob
	assertCarriersOnly(t, "frame with attempts", frame)
}
