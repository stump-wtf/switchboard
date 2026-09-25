package server

// End-to-end coverage of the Quarantine view through the real router (ADR-0031, SPEC-0026 REQ-9,
// REQ-7, "Tenancy", "CSRF Protection", "Redirect Validation", "Security Headers"): a second human sees
// none of the first's held items and every action on one is not-found; every action needs the CSRF
// token; release, discard and trust-this-actor change the store as the spec says, trust is refused for
// rule_fault; a release the rules send back to quarantine is a conflict and the item stays listed; a
// concurrent release race has exactly one outcome. DB-gated like the rest of the package.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

var quarSeq atomic.Int64

type quarFixture struct {
	r        chi.Router
	st       *store.Store
	ctx      context.Context
	alice    store.Human
	bob      store.Human
	aliceTok string
	bobTok   string
	epA      store.Endpoint
	whA      store.Webhook
}

func newQuarFixture(t *testing.T) *quarFixture {
	t.Helper()
	r, st, ctx, ing := newDBRouterWithIngest(t)
	// The in-process router evaluates rules without the sandbox child, so a test can give the
	// webhook rules (the conflict case) and still release through the real intake service.
	ing.SetRouter(routing.InProcess{})
	f := &quarFixture{r: r, st: st, ctx: ctx}
	f.alice, f.aliceTok = mintSession(t, st, ctx, "quar-alice", "Alice Quarantine", "qalice@example.com")
	f.bob, f.bobTok = mintSession(t, st, ctx, "quar-bob", "Bob Quarantine", "qbob@example.com")
	f.epA = seedEndpoint(t, st, ctx, f.alice.ID, "quar-agent-alice", "hash-qa", "sbk_qa", "reviews")
	seedEndpoint(t, st, ctx, f.bob.ID, "quar-agent-bob", "hash-qb", "sbk_qb", "reviews")
	wh, err := st.CreateWebhookWithTrust(ctx, f.epA.ID, "github", "reviews", "signed", "tok-quar-a", "whsec_qa", 5,
		[]byte(`{"logins":["joestump"],"match":"sender"}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	f.whA = wh
	return f
}

// hold quarantines a verified delivery from sender on alice's webhook, as the receiver would.
func (f *quarFixture) hold(t *testing.T, reason, sender string) store.Todo {
	t.Helper()
	key := fmt.Sprintf("quar-%d", quarSeq.Add(1))
	payload := fmt.Sprintf(`{"action":"opened","sender":{"login":%q},"issue":{"title":"<script>alert(1)</script>","user":{"login":%q}}}`, sender, sender)
	detail := fmt.Sprintf(`{"actor":{"sender":%q,"author":%q,"sender_trusted":false,"author_trusted":false,"trusted":false}}`, sender, sender)
	disp := store.DispositionQuarantined
	if reason == routing.QuarantineRuleFault {
		detail = `{"fault":{"rule_index":0,"rule_id":"triage","cause":"type_error","detail":"cannot index number"}}`
		disp = store.DispositionFaulted
	}
	_, out, _, err := f.st.CreateIntakeEventTodos(f.ctx, store.EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed",
		Verified: true, Payload: []byte(payload), WebhookID: f.whA.ID, Disposition: disp,
	}, []string{f.epA.ID}, store.CreateTodoParams{Source: "github", Kind: "issues", Title: "HELD " + key,
		Payload: []byte(payload), IdempotencyKey: key, QuarantineReason: reason, QuarantineDetail: []byte(detail)})
	if err != nil || len(out) != 1 {
		t.Fatalf("hold: %v (%d todos)", err, len(out))
	}
	return out[0].Todo
}

func (f *quarFixture) csrf(t *testing.T, tok string) string {
	t.Helper()
	return scrapeCSRF(t, getAs(t, f.r, tok, "/quarantine").Body.String())
}

// act posts an HTMX action as the session.
func (f *quarFixture) act(t *testing.T, tok, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return postFormAs(t, f.r, tok, f.csrf(t, tok), path, form)
}

// stillHeld reports whether the item is still on alice's quarantine queue.
func (f *quarFixture) stillHeld(t *testing.T, id string) bool {
	t.Helper()
	_, err := f.st.QuarantinedForHuman(f.ctx, f.alice.ID, id)
	return err == nil
}

func noticeOf(body string) string {
	const marker = `data-sb-quarantine-notice="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

// Human B sees none of human A's items, and any action on A's item id is not-found (REQ-9, Tenancy).
func TestQuarantineViewIsOwnerScoped(t *testing.T) {
	f := newQuarFixture(t)
	held := f.hold(t, routing.QuarantineUntrustedActor, "mallory")

	mine := getAs(t, f.r, f.aliceTok, "/quarantine")
	if mine.Code != http.StatusOK || !strings.Contains(mine.Body.String(), `data-sb-quar-item="`+held.ID+`"`) ||
		!strings.Contains(mine.Body.String(), "data-sb-quarantine-count") {
		t.Fatalf("alice's view (%d) does not list her held item and badge", mine.Code)
	}
	theirs := getAs(t, f.r, f.bobTok, "/quarantine")
	if theirs.Code != http.StatusOK || !strings.Contains(theirs.Body.String(), "data-sb-quarantine-empty") {
		t.Fatalf("bob's view (%d) is not the empty state", theirs.Code)
	}
	assertNotIn(t, "bob's /quarantine", theirs.Body.String(), held.ID, held.Title, "mallory", f.whA.ID)

	for _, action := range []string{"release", "discard", "trust"} {
		rec := f.act(t, f.bobTok, "/quarantine/"+held.ID+"/"+action, url.Values{"reason": {"not mine"}})
		if rec.Code != http.StatusNotFound {
			t.Errorf("bob %s on alice's item = %d, want 404", action, rec.Code)
		}
	}
	// Unknown and malformed ids are not-found too, and indistinguishable from foreign ones.
	for _, id := range []string{"td_nope", "00000000-0000-0000-0000-000000000000", "%3Cscript%3E"} {
		if rec := f.act(t, f.aliceTok, "/quarantine/"+id+"/release", nil); rec.Code != http.StatusNotFound {
			t.Errorf("release %s = %d, want 404", id, rec.Code)
		}
	}
	if !f.stillHeld(t, held.ID) {
		t.Fatal("bob's actions changed alice's held item")
	}
	if wh, err := f.st.WebhookForEndpoint(f.ctx, f.whA.ID, f.epA.ID); err != nil || strings.Contains(string(wh.TrustedActors), "mallory") {
		t.Fatalf("bob's trust action changed alice's trust list: %s (%v)", wh.TrustedActors, err)
	}

	// The endpoint card's webhook row is alice's alone.
	if body := getAs(t, f.r, f.aliceTok, "/endpoints").Body.String(); !strings.Contains(body, `data-sb-webhook="`+f.whA.ID+`"`) ||
		!strings.Contains(body, `data-sb-webhook-quarantined="1"`) {
		t.Error("alice's endpoint card is missing her webhook's quarantine count")
	}
	assertNotIn(t, "bob's /endpoints", getAs(t, f.r, f.bobTok, "/endpoints").Body.String(), f.whA.ID)
}

// Every action is a POST carrying the CSRF token: missing or forged is 403 before anything runs.
func TestQuarantineActionsRequireCSRF(t *testing.T) {
	f := newQuarFixture(t)
	held := f.hold(t, routing.QuarantineUntrustedActor, "mallory")
	for _, action := range []string{"release", "discard", "trust"} {
		path := "/quarantine/" + held.ID + "/" + action
		if rec := postNoCSRF(t, f.r, f.aliceTok, path); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF: %d, want 403", path, rec.Code)
		}
		if rec := postForgedCSRF(t, f.r, f.aliceTok, path); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with forged CSRF: %d, want 403", path, rec.Code)
		}
	}
	if !f.stillHeld(t, held.ID) {
		t.Fatal("a CSRF-less action changed the held item")
	}
}

func TestQuarantineReleaseDiscardAndTrust(t *testing.T) {
	f := newQuarFixture(t)

	// The view carries the existing secureHeaders, with no inline script allowed.
	page := getAs(t, f.r, f.aliceTok, "/quarantine")
	csp := page.Header().Get("Content-Security-Policy")
	var scriptSrc string
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src ") {
			scriptSrc = d
		}
	}
	if scriptSrc != "script-src 'self'" {
		t.Errorf("quarantine CSP script-src = %q (policy %q), want exactly script-src 'self': no inline script", scriptSrc, csp)
	}
	if strings.Contains(page.Body.String(), "<script>") {
		t.Error("the quarantine page carries an inline script")
	}

	// Release through the rules (none: the target queue), by the human.
	rel := f.hold(t, routing.QuarantineUntrustedActor, "mallory")
	rec := f.act(t, f.aliceTok, "/quarantine/"+rel.ID+"/release", nil)
	if rec.Code != http.StatusOK || noticeOf(rec.Body.String()) != "released" || strings.Contains(rec.Body.String(), rel.ID) {
		t.Fatalf("release = %d, notice %q; want released and the item gone from the list", rec.Code, noticeOf(rec.Body.String()))
	}
	moved, err := f.st.GetTodoOperatorOwned(f.ctx, f.alice.ID, rel.ID)
	if err != nil || moved.Queue != "reviews" || moved.ReleasedBy != "human:"+f.alice.ID {
		t.Fatalf("released todo = %+v (%v), want reviews, released by human:%s", moved, err, f.alice.ID)
	}

	// A named queue outside the webhook's grant is refused, and the item stays held.
	q := f.hold(t, routing.QuarantineUntrustedActor, "mallory")
	rec = f.act(t, f.aliceTok, "/quarantine/"+q.ID+"/release", url.Values{"queue": {"elsewhere"}})
	if noticeOf(rec.Body.String()) != "queue_refused" || !f.stillHeld(t, q.ID) {
		t.Fatalf("release to an ungranted queue: notice %q, held %v", noticeOf(rec.Body.String()), f.stillHeld(t, q.ID))
	}

	// Discard needs a reason, and records it.
	rec = f.act(t, f.aliceTok, "/quarantine/"+q.ID+"/discard", url.Values{"reason": {"  "}})
	if noticeOf(rec.Body.String()) != "reason_required" || !f.stillHeld(t, q.ID) {
		t.Fatalf("reasonless discard: notice %q", noticeOf(rec.Body.String()))
	}
	rec = f.act(t, f.aliceTok, "/quarantine/"+q.ID+"/discard", url.Values{"reason": {"spam"}})
	if noticeOf(rec.Body.String()) != "discarded" {
		t.Fatalf("discard: notice %q", noticeOf(rec.Body.String()))
	}
	done, err := f.st.GetTodoOperatorOwned(f.ctx, f.alice.ID, q.ID)
	var result map[string]any
	_ = json.Unmarshal(done.Result, &result)
	if err != nil || done.State != "done" || result["discarded"] != true || result["reason"] != "spam" || result["by"] != "human:"+f.alice.ID {
		t.Fatalf("discarded todo = %s %s (%v)", done.State, done.Result, err)
	}

	// SPEC-0026 REQ-9 scenario "Trust this actor": ["joestump"] becomes ["joestump", "newcontributor"],
	// and the item is released through routing.
	tr := f.hold(t, routing.QuarantineUntrustedActor, "newcontributor")
	rec = f.act(t, f.aliceTok, "/quarantine/"+tr.ID+"/trust", nil)
	if noticeOf(rec.Body.String()) != "trusted" || f.stillHeld(t, tr.ID) {
		t.Fatalf("trust: notice %q, still held %v", noticeOf(rec.Body.String()), f.stillHeld(t, tr.ID))
	}
	wh, err := f.st.WebhookForEndpoint(f.ctx, f.whA.ID, f.epA.ID)
	ta, _ := routing.DecodeTrustedActors(wh.SourceType, wh.TrustedActors)
	if err != nil || strings.Join(ta.Logins, ",") != "joestump,newcontributor" {
		t.Fatalf("trusted logins = %v (%v), want [joestump newcontributor]", ta.Logins, err)
	}

	// Trust is refused for rule_fault items, and nothing changes.
	fault := f.hold(t, routing.QuarantineRuleFault, "")
	rec = f.act(t, f.aliceTok, "/quarantine/"+fault.ID+"/trust", nil)
	if noticeOf(rec.Body.String()) != "trust_refused" || !f.stillHeld(t, fault.ID) {
		t.Fatalf("trust on rule_fault: notice %q, held %v", noticeOf(rec.Body.String()), f.stillHeld(t, fault.ID))
	}

	// A release the rules send straight back to quarantine is a conflict (REQ-7), shown as such, and
	// the item stays listed.
	if _, err := f.st.UpdateWebhookRouting(f.ctx, f.whA.ID, f.alice.ID, func(store.WebhookRouting) (routing.Config, error) {
		return routing.Config{Rules: []routing.Rule{{ID: "hold-all", Expr: "true", Action: routing.Action{Quarantine: true}}}}, nil
	}); err != nil {
		t.Fatalf("set rules: %v", err)
	}
	rec = f.act(t, f.aliceTok, "/quarantine/"+fault.ID+"/release", nil)
	if noticeOf(rec.Body.String()) != "conflict" || !strings.Contains(rec.Body.String(), `data-sb-quar-item="`+fault.ID+`"`) ||
		!f.stillHeld(t, fault.ID) {
		t.Fatalf("release back into quarantine: notice %q; want conflict with the item still listed", noticeOf(rec.Body.String()))
	}

	// Without HTMX, an action redirects only to the same-origin Quarantine view, with a notice code.
	form := url.Values{"csrf_token": {f.csrf(t, f.aliceTok)}, "queue": {"reviews"}}
	req := httptest.NewRequest(http.MethodPost, "/quarantine/"+fault.ID+"/release", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://evil.example/phish")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.aliceTok})
	plain := httptest.NewRecorder()
	f.r.ServeHTTP(plain, req)
	if plain.Code != http.StatusSeeOther || plain.Header().Get("Location") != "/quarantine?n=released_queue" {
		t.Fatalf("plain release = %d → %q, want 303 → /quarantine?n=released_queue", plain.Code, plain.Header().Get("Location"))
	}
	if f.stillHeld(t, fault.ID) {
		t.Fatal("a named-queue release skipped the rules and should have released the item")
	}

	// The form body is capped at 16 KiB.
	big := url.Values{"csrf_token": {f.csrf(t, f.aliceTok)}, "reason": {strings.Repeat("x", 17<<10)}}
	req = httptest.NewRequest(http.MethodPost, "/quarantine/"+rel.ID+"/discard", strings.NewReader(big.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.aliceTok})
	over := httptest.NewRecorder()
	f.r.ServeHTTP(over, req)
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized form = %d, want 413", over.Code)
	}
}

// Two concurrent releases of one item have exactly one outcome: one release, one conflict, and one
// todo on the routed queue (REQ-7, REQ-13).
func TestQuarantineReleaseRaceHasOneOutcome(t *testing.T) {
	f := newQuarFixture(t)
	held := f.hold(t, routing.QuarantineUntrustedActor, "mallory")
	csrf := f.csrf(t, f.aliceTok)

	var wg sync.WaitGroup
	notices := make([]string, 2)
	for i := range notices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postFormAs(t, f.r, f.aliceTok, csrf, "/quarantine/"+held.ID+"/release", nil)
			notices[i] = fmt.Sprintf("%d:%s", rec.Code, noticeOf(rec.Body.String()))
		}()
	}
	wg.Wait()
	got := strings.Join(notices, " ")
	if !((notices[0] == "200:released" && notices[1] == "200:conflict") || (notices[0] == "200:conflict" && notices[1] == "200:released")) {
		t.Fatalf("race outcomes = %s, want one released and one conflict", got)
	}
	items, err := f.st.ListTodoItems(f.ctx, f.alice.ID, "", "", 200)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	n := 0
	for _, it := range items {
		if it.IdempotencyKey == held.IdempotencyKey {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("todos for the delivery = %d, want 1 (the release applied once)", n)
	}
	moved, err := f.st.GetTodoOperatorOwned(f.ctx, f.alice.ID, held.ID)
	if err != nil || moved.Queue != "reviews" || moved.State != "pending" {
		t.Fatalf("released todo = %+v (%v)", moved, err)
	}
}
