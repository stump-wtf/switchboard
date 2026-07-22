package ingest

// Deterministic route fan-out: one verified delivery becomes exactly one todo PER resolved target
// endpoint, each pinned to that target, each independently claimable, and the event plus all N
// todos commit atomically or not at all.
//
// This is a security regression suite for the cross-tenant leak ADR-0022 closes. Before the
// endpoint dimension existed the todos table had a free-form global `queue` string and no link to
// an endpoint, so two endpoints that shared a queue name — the common case, e.g. both "reviews" —
// shared a single todo and each other's doorbell, ACROSS HUMANS. Every assertion below is written
// so that reintroducing a shared-todo (or shared-visibility) design fails it: the tests do not
// merely count rows, they check WHICH tenant each row belongs to and prove that acting on one
// tenant's todo is invisible to the other.
//
// The fan-out is TOKEN-FREE. Nothing in the delivery — no header, no query parameter, no field of
// the payload — selects a target. The set is {webhook owner} ∪ webhook_routes, and webhook_routes
// is written only by human-approved actions, so a producer cannot steer a delivery at an endpoint
// it was not granted.
//
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0001 REQ "Enqueue Accepted Delivery as Endpoint-Owned Todo",
// SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)",
// SPEC-0002/0004 REQ atomic ingestion (generalized to N targets).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joestump/switchboard/internal/store"
)

// fanoutLease is the claim TTL these tests use. Long enough that no lease expires mid-test, so a
// todo that turns out to be claimable by the wrong tenant is a genuine authorization hole and never
// a timing artifact.
const fanoutLease = 5 * time.Minute

// listQueue reads one endpoint's todos on a queue through the PUBLIC pull path (ListTodos), which
// is what an agent's list_todos verb bottoms out in. Asserting through it rather than raw SQL is
// deliberate: the leak this suite guards was a pull-path leak as much as a push-path one, so the
// tests have to exercise the same tenant-scoped read an agent actually gets.
func listQueue(t *testing.T, ctx context.Context, st *store.Store, endpointID, queue string) []store.Todo {
	t.Helper()
	todos, err := st.ListTodos(ctx, endpointID, []string{queue}, "", 100)
	if err != nil {
		t.Fatalf("list todos (endpoint %s): %v", endpointID, err)
	}
	return todos
}

// A webhook with NO rows in webhook_routes fans out to the singleton {owner}: one delivery mints
// exactly ONE todo, pinned to the webhook's owning endpoint. This is the common case — the vast
// majority of webhooks are never routed anywhere — so the fallback has to be exactly as tight as
// the routed path.
//
// A second endpoint scoped to the SAME queue string is seeded and left unrouted. It is the whole
// point of the test: without it, "exactly one todo" would pass just as well under the old global
// -queue design, and the assertion would be vacuous. With it, any regression that resolves targets
// by queue name instead of by route mints a second todo (or hands this one to both tenants) and
// fails here.
// Governing: SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)" (singleton-owner fallback).
func TestRouteFanOutSingletonOwnerWhenNoRoutes(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_singleton"
	_, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "fanout-singleton", secret)
	// A co-tenant on the identical queue string with no route to this webhook: the exact collision
	// that used to leak.
	_, stranger := seedEndpoint(t, st, ctx, "singleton-stranger", []string{"reviews"})

	// Precondition: the fan-out under test really is the no-routes fallback, not a routed set that
	// happens to have one member.
	routes, err := st.ListWebhookRoutes(ctx, wh.ID)
	if err != nil {
		t.Fatalf("list webhook routes: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("fixture must have no explicit routes, got %d", len(routes))
	}

	ownerCh, cancelOwner := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancelOwner()
	strangerCh, cancelStranger := hub.Subscribe(stranger.ID, []string{"reviews"})
	defer cancelStranger()

	body := `{"action":"opened","number":1}`
	hdr := map[string]string{"X-GitHub-Delivery": "guid-singleton", "X-Hub-Signature-256": githubSig(secret, body)}
	todos, created := fanOut202(t, postSelfManaged(ing, "fanout-singleton", body, hdr))

	if len(todos) != 1 || created != 1 {
		t.Fatalf("no-routes fan-out = %d todos (%d created), want 1/1: %+v", len(todos), created, todos)
	}
	if todos[0].EndpointID != owner.ID {
		t.Fatalf("todo endpoint_id = %q, want the webhook owner %q", todos[0].EndpointID, owner.ID)
	}
	// Exactly one row exists on the queue at all — no shadow copy for anyone else.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 1 {
		t.Fatalf("todos on reviews = %d, want 1 (singleton-owner fallback)", n)
	}

	// Pull path: the owner sees its todo, the co-tenant on the same queue sees nothing.
	if got := listQueue(t, ctx, st, owner.ID, "reviews"); len(got) != 1 || got[0].ID != todos[0].ID {
		t.Fatalf("owner list_todos = %+v, want exactly the minted todo %q", got, todos[0].ID)
	}
	if got := listQueue(t, ctx, st, stranger.ID, "reviews"); len(got) != 0 {
		t.Fatalf("unrouted co-tenant sharing the queue string sees %d todos, want 0 — cross-tenant leak", len(got))
	}
	// Claim path: the co-tenant cannot claim the owner's todo even naming its id directly.
	if _, err := st.ClaimTodo(ctx, stranger.ID, todos[0].ID, "agent:stranger", fanoutLease); err == nil {
		t.Fatal("an unrouted endpoint claimed the owner's todo — cross-tenant leak")
	}
	// Push path: only the owner's doorbell rang.
	if n := drainHub(ownerCh); n != 1 {
		t.Fatalf("owner doorbell = %d, want 1", n)
	}
	if n := drainHub(strangerCh); n != 0 {
		t.Fatalf("unrouted co-tenant doorbell = %d, want 0 — cross-tenant leak", n)
	}
}

// One verified delivery to a webhook routed to endpoints A (owner) and B mints TWO todos that are
// INDEPENDENTLY claimable: claiming A's neither hides, locks, nor mutates B's.
//
// This independence is the whole reason a shared-todo-with-an-ACL design was rejected. Under a
// shared row, A claiming the work would flip the single row to `claimed` and B would find nothing
// to do — the fan-out would silently deliver to one tenant instead of two. The test therefore
// asserts B's row survives A's claim in state `pending`, is still returned by B's list_todos, and
// is still claimable by B afterwards.
//
// It also asserts the negative direction in both directions: A cannot claim B's todo and B cannot
// claim A's, even holding the other's todo id. A regression that resolved the claim by todo id
// alone (dropping the endpoint predicate) would pass every positive assertion here and fail only
// these.
// Governing: SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func TestRouteFanOutTargetsAreIndependentlyClaimable(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_independent"
	ownerHuman, a, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "fanout-independent", secret)
	bHuman, b := seedEndpoint(t, st, ctx, "independent-b", []string{"reviews"})
	grantFriendEdge(t, st, ctx, "independent", ownerHuman.ID, bHuman.ID)
	if err := st.AddWebhookRoute(ctx, wh.ID, b.ID, ownerHuman.ID); err != nil {
		t.Fatalf("add webhook route: %v", err)
	}

	body := `{"action":"opened","number":2}`
	hdr := map[string]string{"X-GitHub-Delivery": "guid-independent", "X-Hub-Signature-256": githubSig(secret, body)}
	todos, created := fanOut202(t, postSelfManaged(ing, "fanout-independent", body, hdr))
	if len(todos) != 2 || created != 2 {
		t.Fatalf("fan-out = %d todos (%d created), want 2/2: %+v", len(todos), created, todos)
	}
	todoA, todoB := todos[0], todos[1]
	if todoA.EndpointID != a.ID || todoB.EndpointID != b.ID {
		t.Fatalf("fan-out pinned to (%q, %q), want owner %q then routed target %q",
			todoA.EndpointID, todoB.EndpointID, a.ID, b.ID)
	}
	if todoA.ID == todoB.ID {
		t.Fatalf("fan-out must mint a DISTINCT todo per target, got the same id %q", todoA.ID)
	}

	// Each tenant's pull path shows exactly its own row and never the other's.
	listA := listQueue(t, ctx, st, a.ID, "reviews")
	listB := listQueue(t, ctx, st, b.ID, "reviews")
	if len(listA) != 1 || listA[0].ID != todoA.ID {
		t.Fatalf("A list_todos = %+v, want exactly %q", listA, todoA.ID)
	}
	if len(listB) != 1 || listB[0].ID != todoB.ID {
		t.Fatalf("B list_todos = %+v, want exactly %q", listB, todoB.ID)
	}

	// A claims ITS todo. Under the rejected shared-row design this is the step that would erase B's
	// work.
	claimedA, err := st.ClaimTodo(ctx, a.ID, todoA.ID, "agent:a", fanoutLease)
	if err != nil {
		t.Fatalf("A claim its own todo: %v", err)
	}
	if claimedA.State != "claimed" || claimedA.EndpointID != a.ID {
		t.Fatalf("A claimed = (state %q endpoint %q), want claimed/%s", claimedA.State, claimedA.EndpointID, a.ID)
	}

	// B's row is untouched: still pending, still listed, still claimable — by B and only by B.
	stillB, err := st.GetTodo(ctx, b.ID, todoB.ID)
	if err != nil {
		t.Fatalf("B read its own todo after A claimed: %v", err)
	}
	if stillB.State != "pending" {
		t.Fatalf("B todo state = %q after A's claim, want pending (claims must not be shared)", stillB.State)
	}
	if got := listQueue(t, ctx, st, b.ID, "reviews"); len(got) != 1 || got[0].ID != todoB.ID {
		t.Fatalf("B list_todos after A's claim = %+v, want its own todo still visible", got)
	}
	// ClaimNext is the blind "give me work" path an idle agent uses; B must still find its row.
	nextB, err := st.ClaimNext(ctx, b.ID, []string{"reviews"}, "agent:b", fanoutLease)
	if err != nil {
		t.Fatalf("B claim_next after A claimed: %v (B's work must survive A's claim)", err)
	}
	if nextB.ID != todoB.ID || nextB.EndpointID != b.ID {
		t.Fatalf("B claim_next got todo %q owned by %q, want its own %q/%q",
			nextB.ID, nextB.EndpointID, todoB.ID, b.ID)
	}

	// Neither tenant may act on the other's row, even holding its id.
	if _, err := st.ClaimTodo(ctx, a.ID, todoB.ID, "agent:a", fanoutLease); err == nil {
		t.Fatal("A claimed B's todo — cross-tenant leak")
	}
	if _, err := st.GetTodo(ctx, a.ID, todoB.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("A read B's todo: err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetTodo(ctx, b.ID, todoA.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B read A's todo: err = %v, want ErrNotFound", err)
	}
	// And completing one does not complete the other.
	if _, err := st.CompleteTodo(ctx, a.ID, todoA.ID, "agent:a", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("A complete its own todo: %v", err)
	}
	doneB, err := st.GetTodo(ctx, b.ID, todoB.ID)
	if err != nil {
		t.Fatalf("B read its todo after A completed: %v", err)
	}
	if doneB.State == "done" {
		t.Fatal("completing A's todo also completed B's — the rows are not independent")
	}
}

// The event row and ALL of its per-target todos commit in ONE transaction: all-or-nothing, never a
// partial fan-out.
//
// The happy half proves the todos are genuinely bound to the SAME event (one event row, both todos
// carrying that non-nil event id). The unhappy half is the real assertion: CreateEventTodos is
// handed a target list whose SECOND member is a well-formed uuid that names no endpoint, so the
// first todo inserts and the second violates the endpoint foreign key. If the event were persisted
// outside the todo transaction — or the loop committed per target — the first todo and/or an
// orphaned event would survive the error. The test asserts neither does.
//
// The failure is injected at the store boundary rather than through the HTTP receiver because the
// receiver resolves its own targets from webhook_routes, whose FK to endpoints makes an unresolvable
// target unrepresentable through that path. Driving CreateEventTodos directly is the only way to
// exercise the mid-fan-out failure the atomicity guarantee exists for.
// Governing: ADR-0022, SPEC-0002/0004 REQ atomic ingestion generalized to N targets.
func TestRouteFanOutEventAndTodosAreAtomic(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_atomic"
	ownerHuman, a, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "fanout-atomic", secret)
	bHuman, b := seedEndpoint(t, st, ctx, "atomic-b", []string{"reviews"})
	grantFriendEdge(t, st, ctx, "atomic", ownerHuman.ID, bHuman.ID)
	if err := st.AddWebhookRoute(ctx, wh.ID, b.ID, ownerHuman.ID); err != nil {
		t.Fatalf("add webhook route: %v", err)
	}

	// Happy: one delivery, one event, N todos all linked to it.
	body := `{"action":"opened","number":3}`
	hdr := map[string]string{"X-GitHub-Delivery": "guid-atomic", "X-Hub-Signature-256": githubSig(secret, body)}
	todos, _ := fanOut202(t, postSelfManaged(ing, "fanout-atomic", body, hdr))
	if len(todos) != 2 {
		t.Fatalf("fan-out = %d todos, want 2", len(todos))
	}
	if n := countRows(t, ctx, pool,
		`SELECT count(*) FROM events WHERE external_id LIKE '%:guid-atomic'`); n != 1 {
		t.Fatalf("events for one delivery = %d, want exactly 1 (N todos share ONE event)", n)
	}
	gotA, err := st.GetTodo(ctx, a.ID, todos[0].ID)
	if err != nil {
		t.Fatalf("read A's todo: %v", err)
	}
	gotB, err := st.GetTodo(ctx, b.ID, todos[1].ID)
	if err != nil {
		t.Fatalf("read B's todo: %v", err)
	}
	if gotA.EventID == nil || gotB.EventID == nil {
		t.Fatalf("both fanned-out todos must reference the persisted event, got %v / %v", gotA.EventID, gotB.EventID)
	}
	if *gotA.EventID != *gotB.EventID {
		t.Fatalf("fanned-out todos reference different events (%d vs %d); one delivery is one event",
			*gotA.EventID, *gotB.EventID)
	}

	// Unhappy: the second target's todo insert fails (FK violation on a uuid that names no
	// endpoint). Nothing from this attempt may survive.
	eventsBefore := countRows(t, ctx, pool, `SELECT count(*) FROM events`)
	todosBefore := countRows(t, ctx, pool, `SELECT count(*) FROM todos`)
	ghost := uuid.NewString()
	_, _, err = st.CreateEventTodos(ctx,
		store.EventInput{
			Source: "github", Family: "webhook", ExternalID: "atomic-rollback-probe",
			TrustMode: "signed", Verified: true, VerifyDetail: "hmac-sha256 ok",
			ContentType: "application/json", Payload: []byte(body),
		},
		[]string{a.ID, ghost},
		store.CreateTodoParams{
			Queue: "reviews", Source: "github", Kind: "webhook",
			Title: "atomicity probe", Payload: []byte(body), IdempotencyKey: "atomic-rollback-probe",
		})
	if err == nil {
		t.Fatal("fan-out to a nonexistent endpoint must fail, not silently drop the target")
	}
	// No orphaned event...
	if n := countRows(t, ctx, pool,
		`SELECT count(*) FROM events WHERE external_id = 'atomic-rollback-probe'`); n != 0 {
		t.Fatalf("failed fan-out left %d event rows behind, want 0 (atomic ingestion)", n)
	}
	// ...and no partial fan-out: not even the target whose insert SUCCEEDED before the failure.
	if n := countRows(t, ctx, pool,
		`SELECT count(*) FROM todos WHERE title = 'atomicity probe'`); n != 0 {
		t.Fatalf("failed fan-out left %d todos behind, want 0 (partial fan-out is forbidden)", n)
	}
	// Belt and braces: total counts are exactly what they were before the failed attempt, so the
	// rollback did not merely rename rows out of the filters above.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != eventsBefore {
		t.Fatalf("events total = %d after failed fan-out, want %d (unchanged)", n, eventsBefore)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != todosBefore {
		t.Fatalf("todos total = %d after failed fan-out, want %d (unchanged)", n, todosBefore)
	}
}

// Fan-out is TOKEN-FREE: the routed target's todo exists, is visible on its pull path, and rings
// its doorbell, with zero action by that target's agent — no verb call, no credential, no token in
// the delivery. Equally, nothing the PRODUCER puts in the delivery can steer it: the target set is
// read from webhook_routes and from nowhere else.
//
// The steering half is what makes this non-vacuous. The payload names a real, seeded endpoint id
// (an unrouted co-tenant on the same queue) in a plausible field, and a header and query-shaped
// value carry it too. If any regression ever let a delivery nominate its own targets, that endpoint
// would receive a todo. It must not.
// Governing: SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func TestRouteFanOutIsTokenFreeAndUnsteerable(t *testing.T) {
	ing, hub, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_tokenfree"
	ownerHuman, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "fanout-tokenfree", secret)
	routedHuman, routed := seedEndpoint(t, st, ctx, "tokenfree-routed", []string{"reviews"})
	grantFriendEdge(t, st, ctx, "tokenfree", ownerHuman.ID, routedHuman.ID)
	// The endpoint the payload will try to name. It is real and shares the queue, so the only thing
	// keeping it out of the fan-out is the absence of a route.
	_, named := seedEndpoint(t, st, ctx, "tokenfree-named", []string{"reviews"})
	if err := st.AddWebhookRoute(ctx, wh.ID, routed.ID, ownerHuman.ID); err != nil {
		t.Fatalf("add webhook route: %v", err)
	}

	routedCh, cancelRouted := hub.Subscribe(routed.ID, []string{"reviews"})
	defer cancelRouted()
	namedCh, cancelNamed := hub.Subscribe(named.ID, []string{"reviews"})
	defer cancelNamed()

	// The producer attempts to nominate `named` as a target, three ways at once.
	body := `{"action":"opened","number":4,"endpoint_id":"` + named.ID +
		`","target_endpoint_id":"` + named.ID + `","route_to":["` + named.ID + `"]}`
	hdr := map[string]string{
		"X-GitHub-Delivery":   "guid-tokenfree",
		"X-Hub-Signature-256": githubSig(secret, body),
		"X-Switchboard-Route": named.ID,
		"X-Target-Endpoint":   named.ID,
	}
	todos, created := fanOut202(t, postSelfManaged(ing, "fanout-tokenfree", body, hdr))

	if len(todos) != 2 || created != 2 {
		t.Fatalf("fan-out = %d todos (%d created), want 2/2 — {owner, routed} and nobody else: %+v",
			len(todos), created, todos)
	}
	for _, td := range todos {
		if td.EndpointID == named.ID {
			t.Fatalf("the delivery steered a todo to endpoint %q via its payload/headers — fan-out is not token-free", named.ID)
		}
	}
	if todos[0].EndpointID != owner.ID || todos[1].EndpointID != routed.ID {
		t.Fatalf("fan-out targets = (%q, %q), want (%q, %q)",
			todos[0].EndpointID, todos[1].EndpointID, owner.ID, routed.ID)
	}

	// The routed target got real, drainable work without lifting a finger.
	got := listQueue(t, ctx, st, routed.ID, "reviews")
	if len(got) != 1 || got[0].ID != todos[1].ID {
		t.Fatalf("routed target list_todos = %+v, want the fanned-out todo %q (no agent action required)",
			got, todos[1].ID)
	}
	if got[0].State != "pending" {
		t.Fatalf("routed target's todo state = %q, want pending (immediately claimable)", got[0].State)
	}
	claimed, err := st.ClaimNext(ctx, routed.ID, []string{"reviews"}, "agent:routed", fanoutLease)
	if err != nil {
		t.Fatalf("routed target claim_next: %v (its todo must be claimable with no prior action)", err)
	}
	if claimed.ID != todos[1].ID {
		t.Fatalf("routed target claimed %q, want its own fanned-out todo %q", claimed.ID, todos[1].ID)
	}
	// Doorbell reached the routed target unbidden; the payload-named endpoint heard nothing.
	if n := drainHub(routedCh); n != 1 {
		t.Fatalf("routed target doorbell = %d, want 1 (fan-out doorbells without agent action)", n)
	}
	if n := drainHub(namedCh); n != 0 {
		t.Fatalf("payload-named endpoint doorbell = %d, want 0 — deliveries must not steer routing", n)
	}
	if got := listQueue(t, ctx, st, named.ID, "reviews"); len(got) != 0 {
		t.Fatalf("payload-named endpoint has %d todos, want 0 — deliveries must not steer routing", len(got))
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE queue='reviews'`); n != 2 {
		t.Fatalf("todos on reviews = %d, want exactly 2 (owner + routed)", n)
	}
}

// An empty resolved target set is a MISCONFIGURATION, not a no-op: the delivery is refused with 503
// so the producer retries once the webhook is coherent, and NOTHING is persisted. Answering 202
// would silently drop the delivery on the floor; answering 500 would misreport a configuration
// problem as a server fault.
//
// Two layers are asserted. The store contract: ResolveWebhookTargets returns an EMPTY slice — not a
// one-element slice holding "" — when the owner is unresolvable and no explicit routes exist, so
// the receiver's misconfiguration branch is actually reachable. The handler contract: an
// operator-configured receiver with no owning endpoint answers 503 and persists neither event nor
// todo. (That receiver is the reachable instance of "no owning endpoint" today; the self-managed
// path's owner is FK-guaranteed, so its 503 branch is unreachable through HTTP by construction —
// which is exactly why the store-level assertion is here too.)
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func TestRouteFanOutEmptyTargetSetIsRefused(t *testing.T) {
	pool, ctx := ingestTestPool(t)
	st := store.New(pool)
	const secret = "whsec_empty"
	_, _, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "fanout-empty", secret)

	// Store contract: unresolvable owner + no routes ⇒ empty set, never [""].
	targets, err := st.ResolveWebhookTargets(ctx, wh.ID, "")
	if err != nil {
		t.Fatalf("resolve webhook targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets with an unresolvable owner and no routes = %+v, want empty "+
			"(a placeholder target would 500 deep in createTodo instead of 503-ing)", targets)
	}

	// Handler contract: an operator-configured receiver with no owning endpoint refuses the
	// delivery. Built without testIngestDeps precisely so LegacyEndpointID stays unset.
	ing := New(st, NewHub(), slog.New(slog.NewTextHandler(io.Discard, nil)), Config{GitHubSecret: secret})
	body := `{"action":"opened","number":5}`
	rec := post(t, ing.GitHub, "/webhooks/github", body, map[string]string{
		"X-Hub-Signature-256": sign(secret, []byte(body)),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "guid-empty",
		"Content-Type":        "application/json",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("receiver with no owning endpoint: got %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	// Nothing persisted — the delivery was refused, not absorbed.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE external_id LIKE '%guid-empty%'`); n != 0 {
		t.Fatalf("events after a refused delivery = %d, want 0", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 0 {
		t.Fatalf("todos after a refused delivery = %d, want 0", n)
	}
}

// A route whose target endpoint is deleted must not wedge ingestion. The webhook_routes row carries
// ON DELETE CASCADE on target_endpoint_id, so deleting the endpoint takes the route with it and the
// webhook quietly falls back to the remaining target set — deliveries keep flowing.
//
// The alternative failure modes this rules out are both real: a dangling route would either fail the
// todo insert on the endpoint foreign key (turning every subsequent delivery into a 500 — ingestion
// wedged by a tenant who left) or, worse, mint todos pinned to a vanished endpoint that nobody can
// drain.
//
// The test deletes the routed target BETWEEN two deliveries of the same webhook, so it observes the
// transition rather than a fixture that never had the route: the first delivery must fan out to two
// targets and the second, after the delete, to one.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)"; SPEC-0007 REQ
// "Permanent Deletion of Revoked Endpoints".
func TestRouteFanOutSurvivesDeletedRouteTarget(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	st := store.New(pool)
	const secret = "whsec_deleted"
	ownerHuman, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "fanout-deleted", secret)
	// The routed target belongs to a DIFFERENT human, which is the case that matters: its owner can
	// tear it down at any time without the webhook's owner knowing.
	targetHuman, target := seedEndpoint(t, st, ctx, "deleted-target", []string{"reviews"})
	grantFriendEdge(t, st, ctx, "deleted", ownerHuman.ID, targetHuman.ID)
	if err := st.AddWebhookRoute(ctx, wh.ID, target.ID, ownerHuman.ID); err != nil {
		t.Fatalf("add webhook route: %v", err)
	}

	deliver := func(delivery string) ([]acceptedTodo, int) {
		t.Helper()
		body := `{"action":"opened","delivery":"` + delivery + `"}`
		return fanOut202(t, postSelfManaged(ing, "fanout-deleted", body, map[string]string{
			"X-GitHub-Delivery": delivery, "X-Hub-Signature-256": githubSig(secret, body)}))
	}

	// Before: two targets.
	before, _ := deliver("guid-before-delete")
	if len(before) != 2 {
		t.Fatalf("pre-delete fan-out = %d todos, want 2 (owner + routed target)", len(before))
	}

	// The target's own human tears it down: revoke, then permanently delete. Its todos cascade with
	// it (todos.endpoint_id ON DELETE CASCADE), which is what deletion is supposed to mean.
	if err := st.RevokeEndpoint(ctx, target.ID, targetHuman.ID); err != nil {
		t.Fatalf("revoke routed target: %v", err)
	}
	if err := st.DeleteEndpoint(ctx, target.ID, targetHuman.ID); err != nil {
		t.Fatalf("delete routed target: %v", err)
	}

	// The route cascaded away rather than dangling.
	routes, err := st.ListWebhookRoutes(ctx, wh.ID)
	if err != nil {
		t.Fatalf("list webhook routes after delete: %v", err)
	}
	for _, r := range routes {
		if r.TargetEndpointID == target.ID {
			t.Fatalf("route to deleted endpoint %q survived — ON DELETE CASCADE did not fire", target.ID)
		}
	}
	// Resolution agrees: the owner alone.
	targets, err := st.ResolveWebhookTargets(ctx, wh.ID, owner.ID)
	if err != nil {
		t.Fatalf("resolve targets after delete: %v", err)
	}
	if len(targets) != 1 || targets[0] != owner.ID {
		t.Fatalf("resolved targets after delete = %+v, want just the owner %q", targets, owner.ID)
	}

	// After: ingestion is not wedged — a NEW delivery still succeeds, now as a singleton fan-out to
	// the surviving owner, and the owner can actually drain it.
	after, created := deliver("guid-after-delete")
	if len(after) != 1 || created != 1 {
		t.Fatalf("post-delete fan-out = %d todos (%d created), want 1/1 to the surviving owner: %+v",
			len(after), created, after)
	}
	if after[0].EndpointID != owner.ID {
		t.Fatalf("post-delete todo endpoint_id = %q, want the surviving owner %q", after[0].EndpointID, owner.ID)
	}
	claimed, err := st.ClaimTodo(ctx, owner.ID, after[0].ID, "agent:owner", fanoutLease)
	if err != nil {
		t.Fatalf("owner claim after route target deleted: %v (ingestion must not be wedged)", err)
	}
	if claimed.State != "claimed" {
		t.Fatalf("post-delete todo state = %q, want claimed", claimed.State)
	}
	// And a redelivery of the PRE-delete delivery id no longer resurrects the departed tenant.
	if n := countRows(t, ctx, pool,
		`SELECT count(*) FROM todos WHERE endpoint_id NOT IN (SELECT id FROM endpoints)`); n != 0 {
		t.Fatal("todos exist pinned to an endpoint that no longer exists")
	}
}
