// Per-target idempotency and dedup: one delivery fanned out to N endpoints dedups INDEPENDENTLY
// within each target's own key namespace. This is the tenant-isolation half of dedup — before
// ADR-0022 the idempotency key was global, so two endpoints receiving the same delivery collapsed
// onto ONE todo and whichever tenant got there first owned work belonging to the other. These tests
// pin the layered key ("<endpoint>:<webhook>:<delivery-or-body-hash>"), the per-target collapse on
// redelivery, and the rule that only genuinely-new todos ring hooks, the doorbell, or the channel
// hub.
//
// Governing: ADR-0022, SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup",
// SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0001 REQ "Enqueue Accepted Delivery as Endpoint-Owned Todo".
package ingest

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joestump/switchboard/internal/store"
)

// perTargetDeps builds an Ingest over a store instance the TEST also holds. testIngestDeps hides its
// store, which is fine for row-count asserts but not here: the hook assertions below have to register
// SetTodoTransitionHook / SetTodoDoorbellHook on the very same *store.Store the receiver writes
// through, or they would observe nothing and pass vacuously. No legacy endpoint is seeded because
// every test in this file drives the self-managed receiver (POST /webhooks/w/{token}), which owns its
// todos through the webhook, not through Config.LegacyEndpointID.
// Governing: ADR-0022.
func perTargetDeps(t *testing.T) (*Ingest, *Hub, *store.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := ingestTestPool(t)
	st := store.New(pool)
	hub := NewHub()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, hub, log, Config{}), hub, st, pool, ctx
}

// deliveredKey is the idempotency key a self-managed delivery MUST persist on each target's todo:
// the receiver contributes "<webhook-id>:<delivery-id-or-body-hash>" and CreateEventTodos prefixes
// the owning endpoint, so the full layering is "<endpoint>:<webhook>:<delivery>". Asserting the
// literal string is deliberate — a key missing the endpoint prefix is precisely the pre-ADR-0022
// leak, and a test that only compared two keys for inequality would still pass if the prefix were
// swapped for any other per-target discriminator.
// Governing: ADR-0022, SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
func deliveredKey(endpointID, webhookID, deliveryID string) string {
	return endpointID + ":" + webhookID + ":" + deliveryID
}

// todoByEndpoint indexes a fan-out response by owning endpoint, failing when a target is missing or
// reported twice. Fan-out order is documented (owner first) but the assertions below are about WHICH
// tenant got WHAT, so they address entries by endpoint rather than by position.
func todoByEndpoint(t *testing.T, todos []acceptedTodo) map[string]acceptedTodo {
	t.Helper()
	out := make(map[string]acceptedTodo, len(todos))
	for _, td := range todos {
		if td.EndpointID == "" {
			t.Fatalf("fan-out entry has no endpoint_id: %+v", td)
		}
		if _, dup := out[td.EndpointID]; dup {
			t.Fatalf("endpoint %s appears twice in one fan-out: %+v", td.EndpointID, todos)
		}
		out[td.EndpointID] = td
	}
	return out
}

// mustGetTodo reads a todo through the endpoint-scoped public API, so the read itself re-proves
// ownership: GetTodo predicates on endpoint_id, and a todo that leaked to the wrong tenant would not
// be readable under the scope this test claims owns it.
func mustGetTodo(t *testing.T, st *store.Store, ctx context.Context, endpointID, id string) store.Todo {
	t.Helper()
	td, err := st.GetTodo(ctx, endpointID, id)
	if err != nil {
		t.Fatalf("get todo %s under endpoint %s: %v", id, endpointID, err)
	}
	return td
}

// listIDs returns the todo ids one endpoint can see on a queue, via the agent-facing list path.
func listIDs(t *testing.T, st *store.Store, ctx context.Context, endpointID, queue string) []string {
	t.Helper()
	todos, err := st.ListTodos(ctx, endpointID, []string{queue}, "", 100)
	if err != nil {
		t.Fatalf("list todos for %s: %v", endpointID, err)
	}
	ids := make([]string, 0, len(todos))
	for _, td := range todos {
		ids = append(ids, td.ID)
	}
	sort.Strings(ids)
	return ids
}

// seedRoutedWebhook builds the two-target fixture every test below shares: a signed github webhook
// owned by endpoint A, plus a second endpoint B wired in as an explicit route target. Signed (not
// token) trust mode is load-bearing for the hook tests — the SPEC-0011 sender gate only fires the
// doorbell for a VERIFIED delivery event, so a token-mode webhook would make every doorbell
// assertion below trivially zero.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func seedRoutedWebhook(t *testing.T, st *store.Store, ctx context.Context, token, secret string) (
	owner store.Endpoint, target store.Endpoint, wh store.Webhook) {
	t.Helper()
	h, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", token, secret)
	_, target = seedEndpoint(t, st, ctx, "pt-target-"+token, []string{"reviews"})
	if err := st.AddWebhookRoute(ctx, wh.ID, target.ID, h.ID); err != nil {
		t.Fatalf("add webhook route: %v", err)
	}
	// The route must actually be in the resolved target set; without this the fan-out asserts below
	// could "pass" against a single-target webhook by reading a one-element response as expected.
	targets, err := st.ResolveWebhookTargets(ctx, wh.ID, wh.EndpointID)
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}
	if len(targets) != 2 || targets[0] != owner.ID {
		t.Fatalf("targets = %v, want [%s %s] (owner first)", targets, owner.ID, target.ID)
	}
	return owner, target, wh
}

// signedHeaders builds the headers a signed github delivery carries: the provider delivery id (the
// idempotency key's innermost layer) and a valid HMAC over the raw body.
func signedHeaders(secret, body, deliveryID string) map[string]string {
	return map[string]string{
		"X-GitHub-Delivery":   deliveryID,
		"X-Hub-Signature-256": githubSig(secret, body),
	}
}

// HAPPY: the SAME delivery id routed to endpoints A and B yields TWO todos, one per tenant, each
// carrying its own endpoint-prefixed idempotency key. The per-endpoint namespaces do not collapse
// onto each other, and neither tenant can see the other's row through the agent-facing list/get
// paths.
// Governing: SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup",
// SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func TestPerTargetDedupSameDeliveryToTwoEndpointsMintsTwoTodos(t *testing.T) {
	ing, hub, st, pool, ctx := perTargetDeps(t)
	const secret = "whsec_pertarget_two"
	owner, target, wh := seedRoutedWebhook(t, st, ctx, "pt-two-targets", secret)

	chA, cancelA := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancelA()
	chB, cancelB := hub.Subscribe(target.ID, []string{"reviews"})
	defer cancelB()

	const body = `{"action":"opened","number":11}`
	todos, created := fanOut202(t, postSelfManaged(ing, "pt-two-targets", body,
		signedHeaders(secret, body, "guid-shared")))
	if len(todos) != 2 || created != 2 {
		t.Fatalf("fan-out = %d todos / created=%d, want 2/2: %+v", len(todos), created, todos)
	}
	byEP := todoByEndpoint(t, todos)
	a, okA := byEP[owner.ID]
	b, okB := byEP[target.ID]
	if !okA || !okB {
		t.Fatalf("fan-out must name both targets (%s, %s): %+v", owner.ID, target.ID, todos)
	}
	if a.ID == b.ID {
		t.Fatalf("one delivery to two endpoints collapsed onto a single todo %s — cross-tenant leak", a.ID)
	}
	if !a.Created || !b.Created {
		t.Fatalf("both todos are new on a first delivery: a=%v b=%v", a.Created, b.Created)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("todos = %d, want exactly 2 (one per target)", n)
	}
	// Exactly one event backs both todos — the fan-out is one delivery, not two ingests.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("events = %d, want 1 (one delivery)", n)
	}

	// The layered key, asserted literally in both namespaces (see deliveredKey).
	tA := mustGetTodo(t, st, ctx, owner.ID, a.ID)
	tB := mustGetTodo(t, st, ctx, target.ID, b.ID)
	if want := deliveredKey(owner.ID, wh.ID, "guid-shared"); tA.IdempotencyKey != want {
		t.Fatalf("owner idempotency_key = %q, want %q", tA.IdempotencyKey, want)
	}
	if want := deliveredKey(target.ID, wh.ID, "guid-shared"); tB.IdempotencyKey != want {
		t.Fatalf("target idempotency_key = %q, want %q", tB.IdempotencyKey, want)
	}

	// Neither tenant can reach the other's todo: the list path shows only its own row, and the
	// scoped get for the neighbour's id is a clean not-found rather than a readable row.
	if got := listIDs(t, st, ctx, owner.ID, "reviews"); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("owner list = %v, want exactly [%s]", got, a.ID)
	}
	if got := listIDs(t, st, ctx, target.ID, "reviews"); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("target list = %v, want exactly [%s]", got, b.ID)
	}
	if _, err := st.GetTodo(ctx, owner.ID, b.ID); err == nil {
		t.Fatalf("owner endpoint read the target's todo %s — cross-tenant leak", b.ID)
	}
	if _, err := st.GetTodo(ctx, target.ID, a.ID); err == nil {
		t.Fatalf("target endpoint read the owner's todo %s — cross-tenant leak", a.ID)
	}

	// Each tenant's doorbell rang for its OWN row only.
	assertHubGot(t, "owner", chA, a.ID)
	assertHubGot(t, "target", chB, b.ID)
}

// assertHubGot drains a hub subscription and requires exactly one buffered todo with the given id.
// Asserting the id (not just the count) is what makes the endpoint filter meaningful: a Publish that
// ignored the owning endpoint would deliver BOTH targets' todos to both subscribers, and a count-only
// check could still read 1 if the wrong row arrived first.
func assertHubGot(t *testing.T, label string, ch <-chan store.Todo, wantID string) {
	t.Helper()
	var got []store.Todo
	for {
		select {
		case td := <-ch:
			got = append(got, td)
			continue
		default:
		}
		break
	}
	if len(got) != 1 {
		t.Fatalf("%s hub publishes = %d, want 1 (%+v)", label, len(got), got)
	}
	if got[0].ID != wantID {
		t.Fatalf("%s hub received todo %s, want %s (endpoint-scoped fan-out)", label, got[0].ID, wantID)
	}
}

// HAPPY: REDELIVERY of the same delivery to a 2-route webhook stays at exactly TWO todos, not four —
// each target dedups independently inside its own namespace and returns its own pre-existing row.
// Governing: SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
func TestPerTargetDedupRedeliveryToTwoTargetsStaysTwoTodos(t *testing.T) {
	ing, hub, st, pool, ctx := perTargetDeps(t)
	const secret = "whsec_pertarget_redeliver"
	owner, target, _ := seedRoutedWebhook(t, st, ctx, "pt-redeliver", secret)

	chA, cancelA := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancelA()
	chB, cancelB := hub.Subscribe(target.ID, []string{"reviews"})
	defer cancelB()

	const body = `{"action":"synchronize","number":12}`
	hdr := signedHeaders(secret, body, "guid-redeliver")

	first, createdFirst := fanOut202(t, postSelfManaged(ing, "pt-redeliver", body, hdr))
	if createdFirst != 2 {
		t.Fatalf("first delivery created=%d, want 2", createdFirst)
	}
	firstByEP := todoByEndpoint(t, first)
	assertHubGot(t, "owner (first)", chA, firstByEP[owner.ID].ID)
	assertHubGot(t, "target (first)", chB, firstByEP[target.ID].ID)

	// Redelivery: every target collapses onto its own live row.
	second, createdSecond := fanOut202(t, postSelfManaged(ing, "pt-redeliver", body, hdr))
	if createdSecond != 0 {
		t.Fatalf("redelivery created=%d, want 0 (both targets dedup)", createdSecond)
	}
	secondByEP := todoByEndpoint(t, second)
	for _, ep := range []string{owner.ID, target.ID} {
		if secondByEP[ep].Created {
			t.Fatalf("endpoint %s reported created on a redelivery", ep)
		}
		if secondByEP[ep].ID != firstByEP[ep].ID {
			t.Fatalf("endpoint %s redelivery returned %s, want the existing %s",
				ep, secondByEP[ep].ID, firstByEP[ep].ID)
		}
	}
	// Two, not four: the count is the whole point — a per-target namespace that failed to dedup
	// would double, and a global namespace that over-dedups would have produced 1 on the first pass.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("todos after redelivery = %d, want 2", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("events after redelivery = %d, want 1", n)
	}
	// Each tenant still sees exactly its own single row.
	if got := listIDs(t, st, ctx, owner.ID, "reviews"); len(got) != 1 || got[0] != firstByEP[owner.ID].ID {
		t.Fatalf("owner list after redelivery = %v, want [%s]", got, firstByEP[owner.ID].ID)
	}
	if got := listIDs(t, st, ctx, target.ID, "reviews"); len(got) != 1 || got[0] != firstByEP[target.ID].ID {
		t.Fatalf("target list after redelivery = %v, want [%s]", got, firstByEP[target.ID].ID)
	}
	// A redelivery is not news for anyone.
	if n := drainHub(chA); n != 0 {
		t.Fatalf("owner hub publishes on redelivery = %d, want 0", n)
	}
	if n := drainHub(chB); n != 0 {
		t.Fatalf("target hub publishes on redelivery = %d, want 0", n)
	}
}

// HAPPY: within ONE endpoint, a redelivery that arrives while the todo is parked in a retry backoff
// window collapses onto that parked row rather than minting a duplicate active todo. The dedup index
// covers LIVE rows — pending, claimed, and `failed` with an open next_retry_at — so an
// endpoint-scoped key does not quietly narrow the pre-existing live-row contract.
// Governing: SPEC-0003 REQ "Idempotent Enqueue and Dedup", REQ "Bounded Retries via max_attempts",
// REQ "Per-Endpoint Idempotency and Dedup".
func TestPerTargetDedupRedeliveryDuringRetryBackoffCollapsesOntoParkedTodo(t *testing.T) {
	ing, _, st, pool, ctx := perTargetDeps(t)
	const secret = "whsec_pertarget_backoff"
	_, owner, wh := seedWebhook(t, st, ctx, "github", "signed", "reviews", "pt-backoff", secret)

	const body = `{"action":"opened","number":13}`
	hdr := signedHeaders(secret, body, "guid-backoff")
	todos, created := fanOut202(t, postSelfManaged(ing, "pt-backoff", body, hdr))
	if len(todos) != 1 || created != 1 {
		t.Fatalf("single-target delivery = %d todos / created=%d, want 1/1", len(todos), created)
	}
	id := todos[0].ID

	// Drive the todo into a PARKED retry: claim, then fail below the attempt cap.
	if _, err := st.ClaimTodo(ctx, owner.ID, id, "worker-1", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	failed, err := st.FailTodo(ctx, owner.ID, id, "worker-1", nil)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	// Assert the precondition explicitly: without an OPEN retry window this test would be exercising
	// the ordinary pending-row path and would prove nothing about parked retries.
	if failed.State != "failed" || failed.NextRetryAt == nil {
		t.Fatalf("todo must be parked for retry: state=%s next_retry_at=%v", failed.State, failed.NextRetryAt)
	}

	// Redelivery during the open backoff window: same row, nothing new.
	again, createdAgain := fanOut202(t, postSelfManaged(ing, "pt-backoff", body, hdr))
	if createdAgain != 0 {
		t.Fatalf("redelivery during backoff created=%d, want 0", createdAgain)
	}
	if again[0].ID != id {
		t.Fatalf("redelivery during backoff returned %s, want the parked %s", again[0].ID, id)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 1 {
		t.Fatalf("todos = %d, want 1 (redelivery must not mint a duplicate active todo)", n)
	}
	// The parked row is untouched — still failed, still holding its dedup slot and its key.
	parked := mustGetTodo(t, st, ctx, owner.ID, id)
	if parked.State != "failed" || parked.NextRetryAt == nil {
		t.Fatalf("parked todo mutated by redelivery: state=%s next_retry_at=%v", parked.State, parked.NextRetryAt)
	}
	if want := deliveredKey(owner.ID, wh.ID, "guid-backoff"); parked.IdempotencyKey != want {
		t.Fatalf("parked idempotency_key = %q, want %q", parked.IdempotencyKey, want)
	}
}

// UNHAPPY: an EMPTY idempotency key opts OUT of dedup entirely and must not become a collision
// surface across targets. The naive per-target implementation ("key = endpoint + ':' + key") turns
// "" into "<endpoint>:" — a non-empty key that then dedups every keyless delivery for that endpoint
// onto one row, silently swallowing distinct work. Asserting the persisted key is still EMPTY is what
// catches that; the row counts alone would not, since the first delivery looks identical either way.
// Governing: SPEC-0003 REQ "Idempotent Enqueue and Dedup" (null key never dedups),
// REQ "Per-Endpoint Idempotency and Dedup".
func TestPerTargetDedupEmptyIdempotencyKeyOptsOutAcrossTargets(t *testing.T) {
	_, _, st, pool, ctx := perTargetDeps(t)
	_, epA := seedEndpoint(t, st, ctx, "pt-nokey-a", []string{"reviews"})
	_, epB := seedEndpoint(t, st, ctx, "pt-nokey-b", []string{"reviews"})
	targets := []string{epA.ID, epB.ID}

	deliver := func(externalID string) []store.CreatedTodo {
		t.Helper()
		_, out, err := st.CreateEventTodos(ctx,
			store.EventInput{Source: "github", Family: "webhook", ExternalID: externalID,
				TrustMode: "signed", Verified: true, Payload: []byte(`{"n":1}`)},
			targets,
			store.CreateTodoParams{Queue: "reviews", Source: "github", Kind: "webhook",
				Title: "keyless delivery", Payload: []byte(`{"n":1}`)})
		if err != nil {
			t.Fatalf("create event todos (%s): %v", externalID, err)
		}
		if len(out) != 2 {
			t.Fatalf("fan-out = %d, want 2", len(out))
		}
		return out
	}

	first := deliver("ext-1")
	second := deliver("ext-2")

	seen := map[string]bool{}
	for _, ct := range append(append([]store.CreatedTodo{}, first...), second...) {
		if !ct.New {
			t.Fatalf("a keyless enqueue must always mint a new todo, got a dedup on %s", ct.Todo.ID)
		}
		if seen[ct.Todo.ID] {
			t.Fatalf("keyless enqueues collapsed onto todo %s", ct.Todo.ID)
		}
		seen[ct.Todo.ID] = true
		// The stored key stays EMPTY — never rewritten to "<endpoint>:".
		if ct.Todo.IdempotencyKey != "" {
			t.Fatalf("keyless todo %s persisted idempotency_key %q, want empty (dedup opt-out)",
				ct.Todo.ID, ct.Todo.IdempotencyKey)
		}
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 4 {
		t.Fatalf("todos = %d, want 4 (2 targets x 2 keyless deliveries)", n)
	}
	// Two per tenant, and each tenant sees only its own pair.
	for _, ep := range targets {
		if got := listIDs(t, st, ctx, ep, "reviews"); len(got) != 2 {
			t.Fatalf("endpoint %s sees %d todos, want 2: %v", ep, len(got), got)
		}
	}
}

// UNHAPPY: two endpoints that legitimately share the SAME idempotency key keep separate todos, and a
// repeat under one endpoint dedups to THAT endpoint's row — never to the neighbour's. This is the
// bare form of the leak: dedup on a global key alone would hand the second endpoint the first
// endpoint's todo, which the second tenant could then claim.
// Governing: ADR-0022, SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup",
// REQ "Endpoint Ownership (Tenant Isolation)".
func TestPerTargetDedupSharedKeyAcrossEndpointsKeepsSeparateTodos(t *testing.T) {
	_, _, st, pool, ctx := perTargetDeps(t)
	_, epA := seedEndpoint(t, st, ctx, "pt-sharedkey-a", []string{"reviews"})
	_, epB := seedEndpoint(t, st, ctx, "pt-sharedkey-b", []string{"reviews"})
	const shared = "github:delivery-42"

	mk := func(ep, title string) (store.Todo, bool) {
		t.Helper()
		td, created, err := st.CreateTodo(ctx, store.CreateTodoParams{
			EndpointID: ep, Queue: "reviews", Source: "github", Kind: "webhook",
			Title: title, IdempotencyKey: shared})
		if err != nil {
			t.Fatalf("create todo (%s): %v", title, err)
		}
		return td, created
	}

	a, createdA := mk(epA.ID, "for A")
	b, createdB := mk(epB.ID, "for B")
	if !createdA || !createdB {
		t.Fatalf("both endpoints must mint their own row: a=%v b=%v", createdA, createdB)
	}
	if a.ID == b.ID {
		t.Fatalf("a shared idempotency key collapsed two tenants onto todo %s — cross-tenant leak", a.ID)
	}
	if a.EndpointID != epA.ID || b.EndpointID != epB.ID {
		t.Fatalf("todos pinned to the wrong endpoints: a=%s b=%s", a.EndpointID, b.EndpointID)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("todos = %d, want 2", n)
	}

	// A repeat under A dedups to A's row. The id assert is what makes this non-vacuous: a
	// cross-endpoint dedup would ALSO report created=false, but would hand back B's todo.
	again, createdAgain := mk(epA.ID, "for A again")
	if createdAgain {
		t.Fatal("a repeat under the same endpoint must dedup, not mint")
	}
	if again.ID != a.ID {
		t.Fatalf("repeat under A returned %s, want A's own %s (got B's? %s)", again.ID, a.ID, b.ID)
	}

	// Neither tenant can see or claim the other's row through the endpoint-scoped API.
	if _, err := st.GetTodo(ctx, epB.ID, a.ID); err == nil {
		t.Fatalf("endpoint B read A's todo %s — cross-tenant leak", a.ID)
	}
	if _, err := st.ClaimTodo(ctx, epB.ID, a.ID, "thief", time.Minute); err == nil {
		t.Fatalf("endpoint B claimed A's todo %s — cross-tenant leak", a.ID)
	}
	if got := listIDs(t, st, ctx, epA.ID, "reviews"); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("A list = %v, want [%s]", got, a.ID)
	}
	if got := listIDs(t, st, ctx, epB.ID, "reviews"); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("B list = %v, want [%s]", got, b.ID)
	}
}

// UNHAPPY: on a PARTIALLY-deduped delivery (2 targets, 1 of them already holding a live todo under
// its own layered key) the "created" lifecycle hook, the SPEC-0011 doorbell, and the channel hub
// fire for the NEW todo ONLY. Firing per returned todo instead of per newly-minted todo pushes a
// spurious `todo created` SSE frame to the operator Board — and an unwarranted channel push to the
// tenant that merely received a redelivery. fireTodoHook is not covered by the server's
// doorbellGate, so nothing downstream absorbs the duplicate.
// Governing: ADR-0022, SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup",
// SPEC-0015 REQ "Patch Panel Board", SPEC-0011 REQ "Sender Gate and Injection Safety".
func TestPerTargetDedupPartialDeliveryFiresHooksOnlyForNewTodos(t *testing.T) {
	ing, hub, st, pool, ctx := perTargetDeps(t)
	const secret = "whsec_pertarget_partial"
	owner, target, wh := seedRoutedWebhook(t, st, ctx, "pt-partial", secret)

	// Pre-mint the TARGET's todo under exactly the key the receiver will derive for it, so the
	// delivery below is new for the owner and a redelivery for the target. Constructing the key by
	// hand pins the layering: if the endpoint prefix were dropped this pre-mint would no longer
	// collide, the delivery would create two todos, and the created-count assert fails loudly.
	pre, preCreated, err := st.CreateTodo(ctx, store.CreateTodoParams{
		EndpointID: target.ID, Queue: "reviews", Source: "github", Kind: "webhook",
		Title: "pre-existing", IdempotencyKey: deliveredKey(target.ID, wh.ID, "guid-partial")})
	if err != nil || !preCreated {
		t.Fatalf("pre-mint target todo: created=%v err=%v", preCreated, err)
	}

	// Register the observers AFTER the pre-mint so its own hook does not pollute the counts.
	var mu sync.Mutex
	var hookVerbs []string
	var hookTodos []store.Todo
	var doorbells []store.Todo
	st.SetTodoTransitionHook(func(verb string, td store.Todo) {
		mu.Lock()
		defer mu.Unlock()
		hookVerbs = append(hookVerbs, verb)
		hookTodos = append(hookTodos, td)
	})
	st.SetTodoDoorbellHook(func(td store.Todo) {
		mu.Lock()
		defer mu.Unlock()
		doorbells = append(doorbells, td)
	})
	t.Cleanup(func() { st.SetTodoTransitionHook(nil); st.SetTodoDoorbellHook(nil) })

	chA, cancelA := hub.Subscribe(owner.ID, []string{"reviews"})
	defer cancelA()
	chB, cancelB := hub.Subscribe(target.ID, []string{"reviews"})
	defer cancelB()

	const body = `{"action":"opened","number":14}`
	todos, created := fanOut202(t, postSelfManaged(ing, "pt-partial", body,
		signedHeaders(secret, body, "guid-partial")))
	if len(todos) != 2 {
		t.Fatalf("fan-out = %d todos, want 2: %+v", len(todos), todos)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1 (owner new, target deduped): %+v", created, todos)
	}
	byEP := todoByEndpoint(t, todos)
	if !byEP[owner.ID].Created {
		t.Fatal("owner's todo must be reported as newly created")
	}
	if byEP[target.ID].Created {
		t.Fatal("target's todo already existed; it must not be reported as created")
	}
	if byEP[target.ID].ID != pre.ID {
		t.Fatalf("target returned %s, want the pre-existing %s (key layering changed?)",
			byEP[target.ID].ID, pre.ID)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("todos = %d, want 2 (one pre-existing + one new)", n)
	}

	mu.Lock()
	defer mu.Unlock()
	// Exactly ONE "created" lifecycle frame, and it names the OWNER's brand-new todo.
	if len(hookVerbs) != 1 {
		t.Fatalf("todo transition hooks = %v (%d), want exactly one 'created' for the new todo",
			hookVerbs, len(hookVerbs))
	}
	if hookVerbs[0] != "created" {
		t.Fatalf("hook verb = %q, want created", hookVerbs[0])
	}
	if hookTodos[0].ID != byEP[owner.ID].ID || hookTodos[0].EndpointID != owner.ID {
		t.Fatalf("hook fired for todo %s (endpoint %s), want the owner's new %s (%s)",
			hookTodos[0].ID, hookTodos[0].EndpointID, byEP[owner.ID].ID, owner.ID)
	}
	// Same for the doorbell: the target endpoint must NOT be rung for work it already had.
	if len(doorbells) != 1 {
		t.Fatalf("doorbell fires = %d, want 1 (only the newly-minted todo)", len(doorbells))
	}
	if doorbells[0].EndpointID != owner.ID {
		t.Fatalf("doorbell rang endpoint %s, want the owner %s", doorbells[0].EndpointID, owner.ID)
	}
	// And the channel hub: one publish for the owner, none for the target.
	assertHubGot(t, "owner", chA, byEP[owner.ID].ID)
	if n := drainHub(chB); n != 0 {
		t.Fatalf("target hub publishes = %d, want 0 (its todo was a redelivery)", n)
	}
}
