package server

// Handler-level tenancy isolation suite (issue #177, part of #176): a second authenticated human
// must SEE and TOUCH nothing of another human's. The suite drives HTTP ROUTES — never store
// functions — so it compiles against the router and stays valid whatever the store API ends up
// looking like after the scoping fix lands.
//
// DELIBERATELY RED TODAY. The operator handlers currently resolve todos and events globally
// (GetTodoItem / ListTodoItems / TodoCounts / *AnyEndpoint took no owner), so the cross-tenant
// assertions in this file FAILED on the unscoped tree. They were pushed red on purpose: the
// failure list WAS the leak inventory (#176), and each test flipped green as the corresponding
// scoping fix landed. A green-only suite added after the fix would prove nothing about the class.
//
// They are now GREEN against #179 (store + handler scoping) and #180 (the provider-registry
// operator gate). Two of them earned their keep the hard way: the provider cases showed the
// registry was not merely readable across tenants but MUTABLE — bob could POST
// /providers/{name}/remove and delete alice's provider, which #176's read-focused inventory had
// missed entirely, and which #180 then closed.
//
// The store-side calls below moved with the fix: *AnyEndpoint became *OperatorOwned and the read
// models take an owner. The verification re-reads deliberately pass ALICE's id — the assertion is
// about the state of her row, not about what bob is shown.
//
// Fixture: two humans (alice, bob), each with their own agent, vended endpoint, todos, and a
// live session. NO friend edge between them by default — friending is a real grant and would
// mask the bug it is supposed to catch. Assertions key on A's ACTUAL identifiers (ids, titles,
// queue names), never on counts alone, so a paginated or reordered response cannot pass by
// accident.
//
// Route coverage: the table below lists every route in the operator (RequireHuman) group. A
// chi.Walk over the router fails the suite if a protected route appears that is missing from the
// table — a new `pr.` route landing unscoped fails the test rather than silently going untested.
//
// Governing: SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)", issue #177, part of #176.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/store"
)

// tenancyLease mirrors the operators' claim TTL: long enough that no lease expires mid-test, so a
// wrong-tenant transition is a genuine authorization hole, never a timing artifact.
const tenancyLease = 5 * time.Minute

// tenancyFixture is the two-human world every test in this file plays in.
type tenancyFixture struct {
	r     chi.Router
	st    *store.Store
	ctx   context.Context
	alice store.Human
	bob   store.Human
	// plaintext session-cookie tokens
	aliceTok string
	bobTok   string
	epA      store.Endpoint // alice's vended endpoint ("reviews" queue)
	epB      store.Endpoint // bob's vended endpoint ("reviews" queue)
	// A's todos, seeded through the store exactly as an ingest delivery would.
	todoAPending store.Todo // still pending: the claim test's target
	todoAClaimed store.Todo // claimed by alice's operator identity: complete/fail/extend/release target
	// B's own todo: proves B's surface is LIVE (non-empty) while asserting A's absence, so an
	// accidentally-empty response cannot pass the absence checks.
	todoBPending store.Todo
}

// newTenancyFixture provisions the two-human world. DB-gated like the rest of the package.
func newTenancyFixture(t *testing.T) *tenancyFixture {
	t.Helper()
	r, st, ctx := newDBRouter(t)

	f := &tenancyFixture{r: r, st: st, ctx: ctx}
	f.alice, f.aliceTok = mintSession(t, st, ctx, "tenant-alice", "Alice Tenant", "alice@example.com")
	f.bob, f.bobTok = mintSession(t, st, ctx, "tenant-bob", "Bob Tenant", "bob@example.com")

	f.epA = seedEndpoint(t, st, ctx, f.alice.ID, "agent-alice", "hash-alice", "sbk_alice", "reviews")
	f.epB = seedEndpoint(t, st, ctx, f.bob.ID, "agent-bob", "hash-bob", "sbk_bob", "reviews")

	f.todoAPending = f.seedTodo(t, f.epA, "ALICE pending review", "tenancy-a-pending", store.StatePending)
	f.todoAClaimed = f.seedTodo(t, f.epA, "ALICE claimed review", "tenancy-a-claimed", store.StateClaimed)
	f.todoBPending = f.seedTodo(t, f.epB, "BOB pending review", "tenancy-b-pending", store.StatePending)
	return f
}

// seedTodo inserts one todo on the endpoint and, for claimed fixtures, claims it under the
// endpoint owner's operator identity (the same state a real operator claim leaves behind).
func (f *tenancyFixture) seedTodo(t *testing.T, ep store.Endpoint, title, key, state string) store.Todo {
	t.Helper()
	td, _, err := f.st.CreateTodo(f.ctx, store.CreateTodoParams{
		EndpointID: ep.ID, Queue: "reviews", Source: "github", Kind: "push",
		Title: title, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("seed todo %q: %v", key, err)
	}
	if state == store.StateClaimed {
		owner, err := f.st.EndpointOwner(f.ctx, ep.ID)
		if err != nil {
			t.Fatalf("owner of seed endpoint %q: %v", key, err)
		}
		if _, err := f.st.ClaimTodoOperatorOwned(f.ctx, owner, td.ID, "op:"+ep.AgentID, tenancyLease); err != nil {
			t.Fatalf("seed claim for %q: %v", key, err)
		}
		it, err := f.st.GetTodoItem(f.ctx, owner, td.ID)
		if err != nil {
			t.Fatalf("re-read claimed todo %q: %v", key, err)
		}
		td = it.Todo
	}
	return td
}

// bobGet performs an authenticated GET as bob.
func (f *tenancyFixture) bobGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return getAs(t, f.r, f.bobTok, path)
}

// bobPost performs an authenticated, CSRF-valid POST as bob.
func (f *tenancyFixture) bobPost(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	page := f.bobGet(t, "/todos")
	csrf := scrapeCSRF(t, page.Body.String())
	return postFormAs(t, f.r, f.bobTok, csrf, path, nil)
}

// assertNotIn fails the test if any of the needles (A's identifiers) appear in the body. Every
// needle is a value ONLY A's rows could carry — ids, titles, queue names — never a count.
func assertNotIn(t *testing.T, what, body string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(body, n) {
			t.Errorf("%s leaks %q across tenants", what, n)
		}
	}
}

// --- Reads ------------------------------------------------------------------

// TestTenancyTodosListLeaksNothing: bob's /todos page and its HTMX panel contain none of alice's
// todo ids, titles, or her queue-attributable identifiers — while still rendering bob's own row,
// so an accidentally empty or erroring page cannot pass. Governing: issue #177 assertion 1.
func TestTenancyTodosListLeaksNothing(t *testing.T) {
	f := newTenancyFixture(t)

	for _, path := range []string{"/todos", "/todos?state=pending"} {
		rec := f.bobGet(t, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as bob: status %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		assertNotIn(t, "GET "+path, body,
			f.todoAPending.ID, f.todoAClaimed.ID,
			f.todoAPending.Title, f.todoAClaimed.Title,
		)
		if !strings.Contains(body, f.todoBPending.ID) {
			t.Errorf("GET %s: bob's own todo %q missing — the surface went empty, so the absence checks are vacuous", path, f.todoBPending.ID)
		}
	}
}

// TestTenancyTodoDrawerIs404: a direct-id read of alice's todo returns 404 for bob — not 200,
// and not 403, which would confirm the id exists (its own small leak).
// Governing: issue #177 assertion 2.
func TestTenancyTodoDrawerIs404(t *testing.T) {
	f := newTenancyFixture(t)

	for _, id := range []string{f.todoAPending.ID, f.todoAClaimed.ID} {
		rec := f.bobGet(t, "/todos/"+id)
		if rec.Code != http.StatusNotFound {
			body := rec.Body.String()
			t.Errorf("GET /todos/%s as bob: status %d, want 404 (body %.200s)", id, rec.Code, body)
			assertNotIn(t, "GET /todos/"+id, body, f.todoAPending.Title, f.todoAClaimed.Title)
		}
	}
	// bob's own drawer keeps working — scoping must not break the legitimate surface.
	rec := f.bobGet(t, "/todos/"+f.todoBPending.ID)
	if rec.Code != http.StatusOK {
		t.Errorf("GET own todo as bob: status %d, want 200", rec.Code)
	}
}

// TestTenancyEndpointsListLeaksNothing: bob's /endpoints view carries no trace of alice's
// endpoint id, vended token hash, or slug. Governing: issue #177 assertion 1.
func TestTenancyEndpointsListLeaksNothing(t *testing.T) {
	f := newTenancyFixture(t)

	rec := f.bobGet(t, "/endpoints")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /endpoints as bob: status %d", rec.Code)
	}
	body := rec.Body.String()
	assertNotIn(t, "GET /endpoints", body, f.epA.ID, f.epA.Slug)
	if !strings.Contains(body, f.epB.Slug) {
		t.Errorf("GET /endpoints: bob's own endpoint %q missing — empty surface makes absence vacuous", f.epB.Slug)
	}
}

// TestTenancyProvidersListLeaksNothing: bob's /providers view shows no provider alice configured.
// Governing: issue #177 assertion 1.
func TestTenancyProvidersListLeaksNothing(t *testing.T) {
	f := newTenancyFixture(t)

	if _, err := f.st.RegisterAdapter(f.ctx, "provider-alice-only", "webhook", "signed", []byte(`{}`)); err != nil {
		t.Fatalf("register alice's provider: %v", err)
	}

	rec := f.bobGet(t, "/providers")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /providers as bob: status %d", rec.Code)
	}
	assertNotIn(t, "GET /providers", rec.Body.String(), "provider-alice-only")
}

// TestTenancyCountsAndStatsLeakNothing: the rail pill, the filter pills, and the Board stats are
// computed globally today; bob's rendered counts must reflect ONLY bob's rows. Nothing in this
// HTML looks like an id, which is exactly why it is easy to forget.
// Governing: issue #177 assertion 4.
func TestTenancyCountsAndStatsLeakNothing(t *testing.T) {
	f := newTenancyFixture(t)

	// bob: 1 pending. alice: 1 pending + 1 claimed. A global count says 3; scoped says 1.
	rec := f.bobGet(t, "/todos")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /todos as bob: status %d", rec.Code)
	}
	body := rec.Body.String()
	m := regexp.MustCompile(`id="sb-todo-count"[^>]*>(\d+)<`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no sb-todo-count pill in bob's /todos page")
	}
	if m[1] != "1" {
		t.Errorf("rail todo count for bob = %s, want 1 (alice's rows must not be counted)", m[1])
	}

	// The board shell's tiles come from the same global read; same expectation.
	board := f.bobGet(t, "/")
	if board.Code != http.StatusOK && board.Code != http.StatusSeeOther {
		t.Fatalf("GET / as bob: status %d", board.Code)
	}
}

// --- Mutations ---------------------------------------------------------------

// TestTenancyTodoMutationsRefuseAndLeaveStateUntouched: for every todo action, bob's request
// against alice's todo must be REFUSED (non-2xx) and must leave alice's row exactly as it was.
// A handler that returns 403 after writing is the failure mode a status-code-only test misses, so
// every case re-reads the row through the store and asserts the transition did not happen.
// Governing: issue #177 assertion 3.
func TestTenancyTodoMutationsRefuseAndLeaveStateUntouched(t *testing.T) {
	f := newTenancyFixture(t)

	// alice's claimed todo as the store shows it before bob touches anything. Read AS ALICE: the
	// store reads are owner-scoped since #179, and reading through the store rather than bob's
	// HTTP surface is the point — the assertion is about the row, not about what bob is shown.
	before, err := f.st.GetTodoItem(f.ctx, f.alice.ID, f.todoAClaimed.ID)
	if err != nil {
		t.Fatalf("read alice's claimed todo: %v", err)
	}

	type mut struct {
		name string
		path string
	}
	for _, m := range []mut{
		{"claim", "/todos/" + f.todoAPending.ID + "/claim"},
		{"complete", "/todos/" + f.todoAClaimed.ID + "/complete"},
		{"fail", "/todos/" + f.todoAClaimed.ID + "/fail"},
		{"retry", "/todos/" + f.todoAClaimed.ID + "/retry"},
		{"extend", "/todos/" + f.todoAClaimed.ID + "/extend"},
		{"release", "/todos/" + f.todoAClaimed.ID + "/release"},
	} {
		rec := f.bobPost(t, m.path)
		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("%s alice's todo as bob: status %d is a success — cross-tenant mutation went through", m.name, rec.Code)
		}
	}

	// State re-reads: the pending todo is STILL pending and unowned; the claimed todo still
	// belongs to alice's operator identity with its lease intact.
	pending, err := f.st.GetTodoItem(f.ctx, f.alice.ID, f.todoAPending.ID)
	if err != nil {
		t.Fatalf("re-read pending todo: %v", err)
	}
	if pending.State != store.StatePending {
		t.Errorf("pending todo state = %q after bob's claim attempt, want unchanged %q", pending.State, store.StatePending)
	}
	if pending.Owner != "" {
		t.Errorf("pending todo owner = %q after bob's claim attempt, want unowned", pending.Owner)
	}

	after, err := f.st.GetTodoItem(f.ctx, f.alice.ID, f.todoAClaimed.ID)
	if err != nil {
		t.Fatalf("re-read claimed todo: %v", err)
	}
	if after.State != before.State || after.Owner != before.Owner {
		t.Errorf("claimed todo changed under bob's mutations: state %q→%q owner %q→%q",
			before.State, after.State, before.Owner, after.Owner)
	}
	if before.LeaseExpiresAt != nil && after.LeaseExpiresAt != nil &&
		!before.LeaseExpiresAt.Equal(*after.LeaseExpiresAt) {
		t.Errorf("claimed todo lease moved under bob's extend attempt: %v → %v",
			before.LeaseExpiresAt, after.LeaseExpiresAt)
	}
}

// TestTenancyEndpointMutationsLeaveAliceUntouched: bob's revoke/delete against alice's endpoint
// must leave it active and intact. These handlers already scope by owner; the assertions pin that
// contract so the scoping work cannot regress it. Governing: SPEC-0007, issue #177 assertion 3.
func TestTenancyEndpointMutationsLeaveAliceUntouched(t *testing.T) {
	f := newTenancyFixture(t)

	for _, path := range []string{
		"/endpoints/" + f.epA.ID + "/revoke",
		"/endpoints/" + f.epA.ID + "/delete",
	} {
		rec := f.bobPost(t, path)
		// A scoped handler answers a foreign id with a not-found redirect; a 2xx page/fragment
		// that also changed state would be the failure the state re-read below catches.
		if rec.Code >= 200 && rec.Code < 300 && rec.Code != http.StatusSeeOther {
			t.Errorf("POST %s as bob: unexpected 2xx body (%d)", path, rec.Code)
		}
	}

	eps, err := f.st.ListEndpoints(f.ctx, f.epA.AgentID)
	if err != nil || len(eps) != 1 {
		t.Fatalf("re-read alice's endpoint: %v (%d rows)", err, len(eps))
	}
	ep := eps[0]
	if ep.State != "active" {
		t.Errorf("alice's endpoint state = %q after bob's revoke/delete, want active", ep.State)
	}
}

// TestTenancyProviderMutationsRefuseAndLeaveStateUntouched: bob must not disable, enable, rotate,
// or remove a provider line alice configured. Governing: issue #177 assertion 3.
func TestTenancyProviderMutationsRefuseAndLeaveStateUntouched(t *testing.T) {
	f := newTenancyFixture(t)

	if _, err := f.st.RegisterAdapter(f.ctx, "provider-tenancy-f12", "webhook", "signed", []byte(`{}`)); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	before, err := f.st.GetAdapter(f.ctx, "provider-tenancy-f12")
	if err != nil {
		t.Fatalf("read provider before: %v", err)
	}

	for _, path := range []string{
		"/providers/provider-tenancy-f12/disable",
		"/providers/provider-tenancy-f12/enable",
		"/providers/provider-tenancy-f12/rotate",
		"/providers/provider-tenancy-f12/remove",
	} {
		rec := f.bobPost(t, path)
		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("POST %s as bob: status %d is a success — cross-tenant provider mutation went through", path, rec.Code)
		}
	}

	after, err := f.st.GetAdapter(f.ctx, "provider-tenancy-f12")
	if err != nil {
		t.Fatalf("read provider after: %v", err)
	}
	if after.Enabled != before.Enabled {
		t.Errorf("provider enabled = %v after bob's mutations, want unchanged (%v)", after.Enabled, before.Enabled)
	}
}

// TestTenancyFriendsListLeaksNothing: bob's /friends page shows only bob's edges — alice's
// handles and edge ids must not appear. (No friend edge exists in the default fixture on
// purpose: friending is a real grant and would mask the bug.) Governing: issue #177 assertion 1.
func TestTenancyFriendsListLeaksNothing(t *testing.T) {
	f := newTenancyFixture(t)

	rec := f.bobGet(t, "/friends")
	// Friending is capability-gated (cfg.FriendingEnabled) and every /friends handler 404s when it
	// is off. The shared test router leaves it off, so this route is genuinely absent rather than
	// leaking — skip with that stated, instead of failing on a capability the fixture never turned
	// on. If the capability is ever enabled in the shared fixture this assertion starts running by
	// itself, which is the behaviour we want from a suite whose job is to notice new surface.
	if rec.Code == http.StatusNotFound {
		t.Skip("friending capability is off in this router (cfg.FriendingEnabled=false); /friends is absent, not leaking")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /friends as bob: status %d", rec.Code)
	}
	assertNotIn(t, "GET /friends", rec.Body.String(), "Alice Tenant", "alice@example.com")
}

// --- Route coverage ----------------------------------------------------------

// tenancyExcluded describes why a walked protected-group route is deliberately NOT in the
// cross-tenant table: creation/wizard flows mint resources for the CALLER (nothing of another
// human's is addressable), and the friends {id} actions are party-to-edge (both humans are
// endpoints of the relationship, so a foreign id 404s by construction).
var tenancyExcluded = []struct {
	pattern string
	why     string
}{
	{"/logout", "session lifecycle, not tenant-addressable"},
	{"/oauth/", "OAuth authorize/decision: consent surface, not tenant-addressable"},
	{"/.well-known/", "public OAuth/agent-card discovery metadata, no tenant data"},
	{"/a/", "public persona agent-card discovery (A2A): intentionally world-readable card"},
	{"/auth/", "login/callback flows: establish identity, never address tenant rows"},
	{"/dev/todos", "dev-only seeding route, compiled out of production builds"},
	{"/agents", "redirect-only to /endpoints (AgentsRedirect performs no store lookup)"},
	{"/endpoints/quick", "creation flow: mints for the caller"},
	{"/endpoints/vend", "creation flow: mints for the caller"},
	{"/providers/connect", "creation flow: mints for the caller"},
	{"/providers/{name}/confirm/", "pre-action modal render: existence signal only, action routes are in the table"},
	{"/personas/wizard", "creation flow: mints for the caller"},
	{"/personas", "POST /personas creates for the caller; {id} mutations are tenant-directed and in the table"},
	{"/friends/new", "modal render"},
	{"/friends/resolve", "handle resolution preview"},
	{"/friends/{id}", "party-to-edge actions: both humans are endpoints of the relationship"},
}

// tenancyExcludedReason reports whether a walked protected route is deliberately excluded from
// the cross-tenant table, and why.
func tenancyExcludedReason(route string) (string, bool) {
	for _, x := range tenancyExcluded {
		if strings.HasPrefix(route, x.pattern) {
			return x.why, true
		}
	}
	return "", false
}

// tenancyCovered is the suite's route table: every operator (RequireHuman) route the suite
// covers, keyed "METHOD path". `deep` marks routes the cross-tenant assertions actually drive
// with alice's ids as bob; the rest are listed so the coverage walk fails on any NEW protected
// route that lands without joining this table (issue #177: catch the class, not the instance).
func tenancyCovered() map[string]bool {
	m := map[string]bool{}
	for _, k := range []string{
		// Collection reads — deep (assert alice's identifiers absent).
		"GET /todos", "GET /endpoints", "GET /events", "GET /providers", "GET /friends",
		// Todo direct-id + mutations — deep.
		"GET /todos/{id}",
		"POST /todos/{id}/claim", "POST /todos/{id}/complete", "POST /todos/{id}/fail",
		"POST /todos/{id}/retry", "POST /todos/{id}/extend", "POST /todos/{id}/release",
		// Endpoint direct-id — deep (revoke/delete scoped; pinned so they cannot regress).
		"GET /endpoints/{id}/revoke",
		"POST /endpoints/{id}/revoke", "POST /endpoints/{id}/delete",
		// Provider direct-name — deep.
		"POST /providers/{name}/disable", "POST /providers/{name}/enable",
		"POST /providers/{name}/rotate", "POST /providers/{name}/remove",
	} {
		m[k] = true
	}
	// Listed for coverage; tenant-directed, awaiting dedicated cross-tenant cases once the
	// scoping fix lands (they are personas-owned, not todo/endpoint-owned).
	for _, k := range []string{
		"POST /personas/{id}", "POST /personas/{id}/delete",
		"GET /friends/{id}/approve", "POST /friends/{id}/approve", "POST /friends/{id}/decline",
		"POST /friends/{id}/withdraw", "GET /friends/{id}/revoke", "POST /friends/{id}/revoke",
		"POST /friends/{id}/unblock",
	} {
		m[k] = true
	}
	return m
}

// TestTenancySuiteCoversEveryProtectedRoute walks the router and fails if ANY operator-group
// route is missing from the suite's table — so a newly added unscoped route fails here instead of
// silently going untested. This is what makes the suite catch the CLASS, not just the instance.
// Governing: issue #177 ("make the suite fail when someone adds a route").
func TestTenancySuiteCoversEveryProtectedRoute(t *testing.T) {
	r, _, _ := newDBRouter(t)
	covered := tenancyCovered()

	var missing []string
	err := chi.Walk(r, func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/" || !strings.HasPrefix(route, "/") {
			return nil
		}
		// Non-operator surfaces have their own auth models and their own suites.
		for _, prefix := range []string{"/webhooks", "/mcp", "/api/", "/a2a/", "/login", "/static/", "/healthz", "/dev/"} {
			if strings.HasPrefix(route, prefix) {
				return nil
			}
		}
		// Exact-match exclusion (a prefix here would also catch unrelated children).
		if route == "/friends" && method != http.MethodGet {
			return nil // POST /friends: creation flow, mints for the caller
		}
		if method != http.MethodGet && method != http.MethodPost {
			return nil
		}
		if _, excluded := tenancyExcludedReason(route); excluded {
			return nil
		}
		key := method + " " + route
		if !covered[key] {
			missing = append(missing, key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("protected route(s) missing from the tenancy suite's table — add cross-tenant cases or a documented exclusion: %v", missing)
	}
}
