package server

// The endpoint default lease in the operator UI, through the real router + PostgreSQL as a no-JS
// browser: quick vend and the wizard's lifetime step take it (a refused value re-renders inline with
// what was typed), the wizard's confirm summary and the reveal state it, re-vend seeds it, and every
// active card edits it in place through a CSRF-guarded form whose refusals render on the card.
// Another human's endpoint is a 404 that changes nothing, and a revoked card has no form.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL.
//
// Governing: ADR-0043, SPEC-0015 REQ "Endpoints View And Vend Wizard", SPEC-0007 REQ "Endpoint
// Default Lease".

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/lease"
	"github.com/stump-wtf/switchboard/internal/store"
)

// storedLease reads an endpoint's default lease back from the store.
func storedLease(t *testing.T, st *store.Store, ctx context.Context, id string) *int {
	t.Helper()
	d, err := st.EndpointDefaultLease(ctx, id)
	if err != nil {
		t.Fatalf("read default lease: %v", err)
	}
	return d
}

func onlyCard(t *testing.T, st *store.Store, ctx context.Context, humanID string) store.EndpointCard {
	t.Helper()
	cards, err := st.ListEndpointCards(ctx, humanID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards = %d, %v; want exactly one", len(cards), err)
	}
	return cards[0]
}

func TestQuickVendTakesADefaultLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|lease-quick", "Joe Stump", "joe@example.com")
	c := newWizClient(t, r, session)
	page := c.get("/endpoints/quick").Body.String()
	for _, want := range []string{`name="default_lease"`, "Default claim lease", "server default (300s)", "60s to 86400s"} {
		if !strings.Contains(page, want) {
			t.Errorf("quick page: missing %q", want)
		}
	}
	csrf := scrapeCSRF(t, page)
	form := func(lease string) url.Values {
		return url.Values{"csrf_token": {csrf}, "name": {"slow-reviewer"}, "queues_extra": {"reviews"},
			"verbs": {"claim_next", "heartbeat"}, "default_lease": {lease}}
	}

	for _, bad := range []string{"59", "86401", "1.5s", "soon"} {
		rec := c.do(http.MethodPost, "/endpoints/quick", form(bad))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("default_lease %q: got %d, want 400", bad, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "data-sb-quickvend-error") || !strings.Contains(body, `name="default_lease" value="`+bad+`"`) {
			t.Errorf("default_lease %q: the refusal did not re-render inline with the value kept: %.400s", bad, body)
		}
	}
	if cards, _ := st.ListEndpointCards(ctx, human.ID); len(cards) != 0 {
		t.Fatalf("refused quick vends minted %d endpoint(s)", len(cards))
	}

	rec := c.do(http.MethodPost, "/endpoints/quick", form("1h"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-sb-reveal-lease") ||
		!strings.Contains(rec.Body.String(), "1h (3600s)") {
		t.Fatalf("quick vend with 1h: %d, reveal lacks the lease (%.300s)", rec.Code, rec.Body.String())
	}
	if d := storedLease(t, st, ctx, onlyCard(t, st, ctx, human.ID).ID); d == nil || *d != 3600 {
		t.Fatalf("stored default = %v, want 3600", d)
	}
}

func TestVendWizardTakesAndSeedsADefaultLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|lease-wiz", "Alice Ames", "alice@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/endpoints").Body.String())

	followTo(t, c.get("/endpoints/vend"), "/endpoints/vend/agent")
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/agent", url.Values{"csrf_token": {csrf}, "name": {"wiz-bot"}}), "/endpoints/vend/queues")
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/queues", url.Values{"csrf_token": {csrf}, "queues_extra": {"reviews"}}), "/endpoints/vend/verbs")
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/verbs", url.Values{"csrf_token": {csrf}, "verbs": {"claim"}}), "/endpoints/vend/webhooks")
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/webhooks", url.Values{"csrf_token": {csrf}}), "/endpoints/vend/lifetime")

	if step := c.get("/endpoints/vend/lifetime").Body.String(); !strings.Contains(step, `name="default_lease"`) {
		t.Fatal("lifetime step has no default lease field")
	}
	// Out of range: the same step re-renders with the error and both entered values.
	rec := c.do(http.MethodPost, "/endpoints/vend/lifetime",
		url.Values{"csrf_token": {csrf}, "lifetime": {"7d"}, "default_lease": {"86401"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-sb-wiz-error") ||
		!strings.Contains(rec.Body.String(), `value="86401"`) || !strings.Contains(rec.Body.String(), `name="lifetime" value="7d" checked`) {
		t.Fatalf("bad lease on the lifetime step: %d %.400s", rec.Code, rec.Body.String())
	}
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/lifetime",
		url.Values{"csrf_token": {csrf}, "lifetime": {""}, "default_lease": {"45m"}}), "/endpoints/vend/confirm")
	if back := c.get("/endpoints/vend/lifetime").Body.String(); !strings.Contains(back, `name="default_lease" value="45m"`) {
		t.Error("back nav: lifetime step lost the default lease")
	}
	confirm := c.get("/endpoints/vend/confirm").Body.String()
	if !strings.Contains(confirm, "data-sb-wiz-default-lease") || !strings.Contains(confirm, "45m (2700s)") {
		t.Fatalf("confirm summary lacks the default lease: %.600s", confirm)
	}
	if rec := c.do(http.MethodPost, "/endpoints/vend/confirm", url.Values{"csrf_token": {csrf}}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d", rec.Code)
	}
	src := onlyCard(t, st, ctx, human.ID)
	if d := storedLease(t, st, ctx, src.ID); d == nil || *d != 2700 {
		t.Fatalf("wizard vend stored %v, want 2700", d)
	}

	// Re-vend seeds the lease (it is the endpoint's setting, not its expiry).
	followTo(t, c.get("/endpoints/vend?from="+src.ID), "/endpoints/vend/agent")
	if step := c.get("/endpoints/vend/lifetime").Body.String(); !strings.Contains(step, `name="default_lease" value="2700"`) {
		t.Error("re-vend did not seed the source endpoint's default lease")
	}
}

func TestEndpointCardEditsTheDefaultLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	alice, aliceSession := mintSession(t, st, ctx, "test|lease-card-a", "Alice Ames", "alice@example.com")
	_, bobSession := mintSession(t, st, ctx, "test|lease-card-b", "Bob Bell", "bob@example.com")
	ep := seedEndpoint(t, st, ctx, alice.ID, "carded", "hash-lease-card", "sbk_card…", "reviews")
	c := newWizClient(t, r, aliceSession)
	page := c.get("/endpoints").Body.String()
	for _, want := range []string{"data-sb-ep-lease-form", `action="/endpoints/` + ep.ID + `/lease"`, "5m (300s) · server default"} {
		if !strings.Contains(page, want) {
			t.Errorf("card: missing %q", want)
		}
	}
	csrf := scrapeCSRF(t, page)
	set := func(cl *wizClient, token, id, value string) (int, string) {
		rec := cl.do(http.MethodPost, "/endpoints/"+id+"/lease", url.Values{"csrf_token": {token}, "default_lease": {value}})
		return rec.Code, rec.Body.String() + rec.Header().Get("Location")
	}

	// Inclusive bounds, a duration, and the two spellings of "server default".
	for _, ok := range []struct {
		in   string
		want *int
	}{
		{strconv.Itoa(lease.MinDefaultSeconds), intp(lease.MinDefaultSeconds)},
		{strconv.Itoa(lease.MaxSeconds), intp(lease.MaxSeconds)},
		{"1h", intp(3600)},
		{"", nil},
		{"1h", intp(3600)},
		{"default", nil},
		{"45m", intp(2700)},
	} {
		code, loc := set(c, csrf, ep.ID, ok.in)
		if code != http.StatusSeeOther || !strings.HasSuffix(loc, "/endpoints#sb-ep-"+ep.ID) {
			t.Fatalf("set %q: %d %q, want 303 back to the card", ok.in, code, loc)
		}
		got := storedLease(t, st, ctx, ep.ID)
		if (got == nil) != (ok.want == nil) || (got != nil && *got != *ok.want) {
			t.Fatalf("set %q stored %v, want %v", ok.in, got, ok.want)
		}
	}
	if page := c.get("/endpoints").Body.String(); !strings.Contains(page, "45m (2700s) · set for this endpoint") ||
		!strings.Contains(page, `name="default_lease" value="2700"`) {
		t.Fatalf("card after an edit does not state the new default: %.800s", page)
	}

	// Refusals render on the card, keep what was typed, and change nothing.
	for _, bad := range []string{strconv.Itoa(lease.MinDefaultSeconds - 1), strconv.Itoa(lease.MaxSeconds + 1), "soon"} {
		code, body := set(c, csrf, ep.ID, bad)
		if code != http.StatusBadRequest || !strings.Contains(body, "data-sb-ep-lease-error") ||
			!strings.Contains(body, `value="`+bad+`"`) || !strings.Contains(body, `aria-invalid="true"`) {
			t.Fatalf("set %q: %d, want 400 with the refusal on the card (%.400s)", bad, code, body)
		}
		if got := storedLease(t, st, ctx, ep.ID); got == nil || *got != 2700 {
			t.Fatalf("a refused %q changed the default to %v", bad, got)
		}
	}

	// CSRF is required, like every session mutation.
	if code, _ := set(c, "not-the-token", ep.ID, "1h"); code != http.StatusForbidden {
		t.Fatalf("missing CSRF: got %d, want 403", code)
	}

	// Bob reaches nothing: a valid value and an invalid one are both a plain 404.
	bob := newWizClient(t, r, bobSession)
	bobCSRF := scrapeCSRF(t, bob.get("/endpoints").Body.String())
	for _, v := range []string{"1h", "59"} {
		if code, _ := set(bob, bobCSRF, ep.ID, v); code != http.StatusNotFound {
			t.Fatalf("bob setting %q on alice's endpoint: got %d, want 404", v, code)
		}
	}
	if got := storedLease(t, st, ctx, ep.ID); got == nil || *got != 2700 {
		t.Fatalf("bob changed alice's default to %v", got)
	}

	// A revoked card has no form, and a POST to it is a 409.
	if err := st.RevokeEndpoint(ctx, ep.ID, alice.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if page := c.get("/endpoints").Body.String(); strings.Contains(page, "data-sb-ep-lease-form") {
		t.Error("a revoked card still renders the lease form")
	}
	if code, _ := set(c, csrf, ep.ID, "1h"); code != http.StatusConflict {
		t.Fatalf("set on a revoked endpoint: got %d, want 409", code)
	}
}

func intp(n int) *int { return &n }
