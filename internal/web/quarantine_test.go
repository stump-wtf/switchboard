package web

// Render and handler coverage for the Quarantine view that runs in the `go test ./...` gate without a
// database (ADR-0031, SPEC-0026 REQ-9): the empty state, each reason, the collapsed payload, inert
// rendering of a <script> and a Markdown-link payload, the fixed notice table and redirect target,
// the rail entry, the endpoint card's webhook signals, the actor projection and the trust names per
// match mode, and the input checks that answer before any store or service call. The end-to-end
// actions (owner scope, CSRF, the release race) run in the DB-backed internal/server suite.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/ingest"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

func renderQuarantine(t *testing.T, panel quarantinePanelView, count int) string {
	t.Helper()
	h := newTestHandler(t)
	return renderPage(t, h, "quarantine", view{Title: "Quarantine", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "quarantine", DBConnected: true, Initials: "JS", QuarantineCount: count}, Quarantine: &panel})
}

func heldItem(reason, detail, payload string) store.QuarantinedItem {
	id := "td_" + reason
	return store.QuarantinedItem{
		Todo: store.Todo{ID: id, EndpointID: "ep1", Queue: store.QueueQuarantine, Source: "github", Kind: "webhook",
			Title: "held " + reason, State: "pending", CreatedAt: time.Now(),
			QuarantineReason: reason, QuarantineDetail: []byte(detail)},
		Event: store.EventHistoryDetail{
			EventHistoryItem: store.EventHistoryItem{Provider: "github", EventType: "issues", TrustMode: "signed",
				Verified: true, WebhookID: "wh-1", ReceivedAt: time.Now()},
			Payload: []byte(payload)},
		Trust: &store.WebhookTrust{SourceType: "github", TrustedActors: []byte(`{"logins":["joestump"],"match":"sender"}`)},
	}
}

// The trust button names exactly who the action adds, in its visible text and its accessible label,
// under the webhook's match mode; it is offered only when that mode names someone (the same
// trustNames the action runs), and never when the webhook is gone.
func TestQuarantineTrustButtonNamesWhoIsTrusted(t *testing.T) {
	const both = `{"actor":{"sender":"joestump","author":"mallory","sender_trusted":true,"author_trusted":false,"trusted":false}}`
	const noSender = `{"actor":{"author":"someone","author_trusted":false,"trusted":false}}`
	cases := []struct {
		name, stored, detail string
		trust                bool   // whether the webhook is still there
		want                 string // the named actors, or "" for no button
	}{
		{"match sender", `{"logins":[],"match":"sender"}`, both, true, "joestump"},
		{"match author", `{"logins":["joestump"],"match":"author"}`, both, true, "mallory"},
		{"match both", `{"logins":[],"match":"both"}`, both, true, "joestump and mallory"},
		{"match sender, no sender", `{"logins":[],"match":"sender"}`, noSender, true, ""},
		{"webhook gone", `{"logins":[]}`, both, false, ""},
	}
	for _, c := range cases {
		it := heldItem(routing.QuarantineUntrustedActor, c.detail, `{}`)
		it.Trust.TrustedActors = []byte(c.stored)
		if !c.trust {
			it.Trust = nil
		}
		v := quarantineItemFrom(it, nil)
		body := renderQuarantine(t, quarantinePanelView{CSRF: "tok", Items: []quarantineItemView{v}}, 1)
		if c.want == "" {
			if v.CanTrust || strings.Contains(body, "data-sb-quar-trust") {
				t.Errorf("%s: trust offered (%v), want no button: the action would add nobody", c.name, v.TrustNames)
			}
			continue
		}
		for _, want := range []string{
			`aria-label="Trust ` + c.want + ` on this webhook and release td_untr"`,
			`<span class="sb-mono" data-sb-quar-trust-names>` + c.want + `</span> · release`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", c.name, want)
			}
		}
	}
}

func TestQuarantineViewEmptyState(t *testing.T) {
	body := renderQuarantine(t, quarantinePanelView{CSRF: "tok"}, 0)
	for _, want := range []string{
		`<h1 class="sb-page-title">quarantine</h1>`,
		`id="sb-quarantine-panel" aria-live="polite"`,
		`data-sb-quarantine-empty`,
		`data-sb-nav="q" href="/quarantine" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty quarantine view: missing %q", want)
		}
	}
	if strings.Contains(body, "data-sb-quarantine-count") {
		t.Error("the rail badge renders at zero; it should show only when items are held")
	}
	if strings.Count(body, `id="sb-quarantine-panel"`) != 1 {
		t.Error("the panel swap target must appear exactly once")
	}
	// An unreadable list is reported as such, never as the empty state.
	body = renderQuarantine(t, quarantinePanelView{CSRF: "tok", Error: true}, 0)
	if !strings.Contains(body, "could not be read") || strings.Contains(body, "data-sb-quarantine-empty") {
		t.Error("a failed read renders as an empty quarantine")
	}
}

// Each reason renders its label and detail, and trust-this-actor is offered only for untrusted_actor.
func TestQuarantineViewRendersEachReason(t *testing.T) {
	agents := map[string]string{"ep1": "triage-bot"}
	items := []quarantineItemView{
		quarantineItemFrom(heldItem(routing.QuarantineUntrustedActor,
			`{"actor":{"sender":"newcontributor","author":"newcontributor","sender_trusted":false,"author_trusted":false,"trusted":false}}`,
			`{"action":"opened"}`), agents),
		quarantineItemFrom(heldItem(routing.QuarantineRuleFault,
			`{"fault":{"rule_index":0,"rule_id":"triage","cause":"type_error","detail":"cannot index number"}}`, `{}`), agents),
		quarantineItemFrom(heldItem(routing.QuarantineRuleAction, `{"rule_id":"hold-edits"}`, `{}`), agents),
	}
	body := renderQuarantine(t, quarantinePanelView{CSRF: "tok", Items: items}, 3)

	for _, want := range []string{
		`data-sb-quarantine-count aria-label="3 held">3</span>`,
		// untrusted_actor: the actor and its trust flags, the receiving webhook and endpoint, the trust action.
		`data-sb-quar-reason="untrusted_actor"`, "untrusted actor",
		`<code class="sb-code">newcontributor</code> <span class="sb-chip sb-chip--mono">untrusted</span>`,
		`<code class="sb-code">wh-1</code> <span class="sb-muted">on triage-bot</span>`,
		`action="/quarantine/td_untrusted_actor/trust"`, `aria-label="Trust newcontributor on this webhook and release td_untr"`,
		// rule_fault: the cause and detail.
		`data-sb-quar-reason="rule_fault"`, "rule fault",
		`<code class="sb-code">type_error</code> <span class="sb-muted">cannot index number</span>`,
		// rule_action: the holding rule.
		`data-sb-quar-reason="rule_action"`, "held by a rule", `<code class="sb-code">hold-edits</code>`,
		// Every item offers release (optionally to a queue) and discard (with a required reason).
		`action="/quarantine/td_rule_fault/release"`, `action="/quarantine/td_rule_fault/discard"`,
		`name="reason" maxlength="500" required`, `name="queue" maxlength="128"`,
		`<input type="hidden" name="csrf_token" value="tok">`,
		`hx-target="#sb-quarantine-panel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("quarantine view: missing %q", want)
		}
	}
	if n := strings.Count(body, `data-sb-quar-trust>`); n != 1 {
		t.Errorf("trust-this-actor rendered %d times, want once (untrusted_actor only; refused for rule_fault)", n)
	}
	if strings.Contains(body, `action="/quarantine/td_rule_fault/trust"`) {
		t.Error("trust-this-actor offered on a rule_fault item")
	}
}

// SPEC-0026 REQ-9 scenario "Payload is never rendered as markup": a <script> payload and a Markdown
// link render as inert, escaped text in a collapsed disclosure, with no element or link created.
func TestQuarantinePayloadIsNeverMarkup(t *testing.T) {
	// Raw bytes as a sender would send them (json.Marshal would pre-escape the angle brackets).
	payload := `{"body":"<script>alert(1)</script><img src=x onerror=alert(2)>","title":"[click me](javascript:alert(3)) **bold**"}`
	if !json.Valid([]byte(payload)) {
		t.Fatal("fixture payload is not JSON")
	}
	it := heldItem(routing.QuarantineUntrustedActor, `{"actor":{"sender":"<b>mallory</b>"}}`, payload)
	it.Todo.Title = `</h2><script>alert(4)</script>`
	body := renderQuarantine(t, quarantinePanelView{CSRF: "tok", Items: []quarantineItemView{quarantineItemFrom(it, nil)}}, 1)

	for _, bad := range []string{"<script>alert", "<img src=x", `href="javascript`, "<b>mallory</b>", "<strong>bold", "</h2><script>"} {
		if strings.Contains(body, bad) {
			t.Errorf("payload rendered as markup: found %q", bad)
		}
	}
	for _, want := range []string{
		`<details class="sb-quar__payload" data-sb-disclosure>`,
		`aria-expanded="false" aria-controls="sb-quar-payload-td_untrusted_actor"`,
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"[click me](javascript:alert(3)) **bold**",
		"&lt;b&gt;mallory&lt;/b&gt;",
		"&lt;/h2&gt;&lt;script&gt;alert(4)&lt;/script&gt;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("payload: missing the escaped text %q", want)
		}
	}
	if strings.Contains(body, "<details class=\"sb-quar__payload\" data-sb-disclosure open") {
		t.Error("the payload must be collapsed by default")
	}
}

func TestQuarantineNoticesAndRedirect(t *testing.T) {
	for code := range quarantineNotices {
		if got := quarantineRedirect(code); got != "/quarantine?n="+code {
			t.Errorf("redirect(%q) = %q", code, got)
		}
		if n := noticeFor(code); n == nil || n.Text == "" || n.Code != code {
			t.Errorf("notice(%q) = %+v", code, n)
		}
	}
	// A code outside the table never reaches the page or the Location header.
	for _, code := range []string{"", "https://evil.example", "<script>", "released\r\nX: y"} {
		if got := quarantineRedirect(code); got != "/quarantine" {
			t.Errorf("redirect(%q) = %q, want the bare view", code, got)
		}
		if noticeFor(code) != nil {
			t.Errorf("notice(%q) rendered text from outside the table", code)
		}
	}
	body := renderQuarantine(t, quarantinePanelView{CSRF: "tok", Notice: noticeFor("conflict")}, 0)
	if !strings.Contains(body, `sb-callout--warn" role="status" data-sb-quarantine-notice="conflict">conflict`) {
		t.Error("the conflict notice is not shown as a status")
	}
}

// The endpoint card carries each webhook's open quarantine count, 24-hour faults, and the allow_all
// warning (SPEC-0026 REQ-9, REQ-1, REQ-5).
func TestEndpointCardWebhookSignals(t *testing.T) {
	h := newTestHandler(t)
	card := endpointCard{ID: "e1", AgentName: "bot", Initials: "BO", State: "active", Webhooks: []webhookSignalView{
		{ID: "wh-held", ShortID: "wh-held", Source: "github", TargetQueue: "reviews", Quarantined: 4, Faults24h: 2},
		{ID: "wh-open", ShortID: "wh-open", Source: "gitea", TargetQueue: "triage", AllowAll: true},
	}}
	body := renderPage(t, h, "endpoints", view{Title: "Endpoints", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"}, EndpointCards: []endpointCard{card}})
	for _, want := range []string{
		`data-sb-webhook="wh-held"`, `href="/quarantine" data-sb-webhook-quarantined="4">4 held</a>`,
		`data-sb-webhook-faults="2"`, "⚠ 2 faulted · 24h",
		`data-sb-webhook="wh-open"`, `data-sb-webhook-quarantined="0">0 held`, `data-sb-webhook-faults="0">0 faults · 24h`,
		`data-sb-webhook-allow-all`, "trusts every sender",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoint card: missing %q", want)
		}
	}
	// A card with no webhooks renders no webhook row.
	body = renderPage(t, h, "endpoints", view{Title: "Endpoints", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"}, EndpointCards: []endpointCard{{ID: "e2", State: "active"}}})
	if strings.Contains(body, "sb-quar-webhooks") {
		t.Error("a card without webhooks renders an empty webhook row")
	}
}

func TestTrustNamesFollowTheMatchMode(t *testing.T) {
	sender, author := "sam", "ann"
	a := &routing.ActorTrust{Sender: &sender, Author: &author}
	empty := ""
	cases := []struct {
		source, stored string
		actor          *routing.ActorTrust
		want           []string
	}{
		{"github", `{"logins":[],"match":"sender"}`, a, []string{"sam"}},
		{"github", `{"logins":[]}`, a, []string{"sam"}},
		{"gitea", `{"logins":[],"match":"author"}`, a, []string{"ann"}},
		{"gitea", `{"logins":[],"match":"author"}`, &routing.ActorTrust{Sender: &sender, Author: &empty}, []string{"sam"}},
		{"github", `{"logins":[],"match":"both"}`, a, []string{"sam", "ann"}},
		{"cairn", `{"actor_ids":[]}`, &routing.ActorTrust{Sender: &sender, Author: &sender}, []string{"sam"}},
		{"github", `{"logins":[]}`, &routing.ActorTrust{}, nil},
	}
	for _, c := range cases {
		got := trustNames(c.source, []byte(c.stored), c.actor)
		if !slices.Equal(got, c.want) {
			t.Errorf("trustNames(%s, %s) = %v, want %v", c.source, c.stored, got, c.want)
		}
	}
}

// Input checks answer before any store or service call, with a 303 to the view (no JS) that carries
// only a notice code; with no intake service wired, every action is "unavailable" and changes nothing.
func TestQuarantineActionInputChecks(t *testing.T) {
	h := newTestHandler(t) // nil store, no quarantine service
	post := func(path string, form url.Values, fn http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec
	}
	cases := []struct {
		name string
		form url.Values
		fn   http.HandlerFunc
		want string
	}{
		{"queue with a space", url.Values{"queue": {"lane m"}}, h.ReleaseQuarantined, "/quarantine?n=queue_invalid"},
		{"queue too long", url.Values{"queue": {strings.Repeat("q", 129)}}, h.ReleaseQuarantined, "/quarantine?n=queue_invalid"},
		{"no service", url.Values{"queue": {"lane-m"}}, h.ReleaseQuarantined, "/quarantine?n=unavailable"},
		{"reason missing", url.Values{"reason": {"   "}}, h.DiscardQuarantined, "/quarantine?n=reason_required"},
		{"reason too long", url.Values{"reason": {strings.Repeat("é", 501)}}, h.DiscardQuarantined, "/quarantine?n=reason_required"},
		{"reason at the limit, no service", url.Values{"reason": {strings.Repeat("é", 500)}}, h.DiscardQuarantined, "/quarantine?n=unavailable"},
		{"trust, no service", url.Values{}, h.TrustQuarantinedActor, "/quarantine?n=unavailable"},
	}
	for _, c := range cases {
		rec := post("/quarantine/td_1/x", c.form, c.fn)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != c.want {
			t.Errorf("%s: %d → %q, want 303 → %q", c.name, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}
}

// After trust-this-actor commits the trust list, every release failure maps to a notice that says the
// actor was trusted, and none of them claims that nothing changed.
func TestTrustedReleaseFailureSaysTrusted(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", ingest.ErrRoutingUnavailable), "trusted_unavailable"},
		{fmt.Errorf("wrapped: %w", ingest.ErrReleaseConflict), "trusted_conflict"},
		{store.ErrConflict, "trusted_conflict"},
		{store.ErrNotFound, "trusted_conflict"}, // a lost race: the item was the human's a moment earlier
		{errors.New("db: connection reset"), "trusted_error"},
	} {
		got := trustedReleaseFailure(tc.err)
		n := noticeFor(got)
		if got != tc.want || n == nil || !strings.HasPrefix(n.Text, "actor trusted") || strings.Contains(n.Text, "nothing was changed") {
			t.Errorf("trustedReleaseFailure(%v) = %q (%+v), want %q with a trust-aware text", tc.err, got, n, tc.want)
		}
	}
}
