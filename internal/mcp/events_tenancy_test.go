package mcp

// Cross-tenant isolation for the SPEC-0005 event-history surface, over the real SDK client and the
// REAL store and PostgreSQL: the tenant boundary is the owner predicate in the store's SQL, which a
// fake could only echo back. Each case asserts on human A's actual event id and payload, never on a
// count alone. Skips cleanly without SWITCHBOARD_TEST_DATABASE_URL, in the house style.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owner-Scoped History Reads" — scenarios "Human B lists events
// (the #194 reproduction)" and "Replay of a foreign event" — and REQ "Closing the Audited Surfaces"
// scenario "Dedup cannot cross owners (F14)".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

// historyFixture is the route fixture plus one delivery on each human's webhook, recorded through
// the same store path the self-managed receiver uses, and an event-verb session for each endpoint.
type historyFixture struct {
	*routeFixture
	eventA, eventB int64
	sessA1, sessA2 *sdk.ClientSession // human A's two endpoints
	sessB          *sdk.ClientSession // human B
}

const secretA = `{"secret":"a-payload-only-A-may-read"}`

func newHistoryFixture(t *testing.T) (context.Context, *historyFixture) {
	t.Helper()
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(cancel)
	f := &historyFixture{routeFixture: newRouteFixture(t, ctx, pool)}

	deliver := func(webhookID, endpointID, key, payload string) int64 {
		t.Helper()
		// The same external id on both webhooks: F14 says neither may be answered with the other's.
		id, _, _, err := f.st.CreateRoutedEventTodos(ctx, store.EventInput{
			Source: "github", Family: "webhook", EventType: "push", ExternalID: key,
			TrustMode: "signed", Verified: true, Payload: []byte(payload),
			WebhookID: webhookID, EndpointID: endpointID,
		}, false, []string{endpointID}, store.CreateTodoParams{
			Queue: "reviews", Source: "github", Kind: "push", Title: "t", IdempotencyKey: key})
		if err != nil {
			t.Fatalf("deliver on %s: %v", webhookID, err)
		}
		return id
	}
	f.eventA = deliver(f.webhookA, f.epA1, "delivery-1", secretA)
	f.eventB = deliver(f.webhookB, f.epB, "delivery-1", `{"secret":"b"}`)

	open := func(agent, slug string) *sdk.ClientSession {
		_, token := mustEndpoint(t, ctx, f.st, agent, slug, eventVerbNames)
		return routeSession(t, ctx, f.st, slug, token)
	}
	f.sessA1 = open(f.agentA1, "hist-a1-66666666")
	f.sessA2 = open(f.agentA2, "hist-a2-77777777")
	f.sessB = open(f.agentB, "hist-b-88888888")
	return ctx, f
}

func listedIDs(t *testing.T, ctx context.Context, cs *sdk.ClientSession) []int64 {
	t.Helper()
	var out listWebhookEventsOut
	callOK(t, ctx, cs, "list_webhook_events", map[string]any{}, &out)
	ids := make([]int64, 0, len(out.Events))
	for _, e := range out.Events {
		ids = append(ids, e.ID)
	}
	return ids
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Scenario "Human B lists events (the #194 reproduction)", plus "Dedup cannot cross owners": both
// deliveries share (source, external_id) and still became two events, each visible only to its
// owner. A's second endpoint stands in for "Team member reads team deliveries" until #415.
func TestEventHistoryListIsOwnerScoped(t *testing.T) {
	ctx, f := newHistoryFixture(t)
	if f.eventA == f.eventB {
		t.Fatalf("two owners' deliveries with the same (source, external_id) collapsed onto event %d", f.eventA)
	}

	b := listedIDs(t, ctx, f.sessB)
	if containsID(b, f.eventA) {
		t.Fatalf("human B listed human A's event %d: %v", f.eventA, b)
	}
	if !containsID(b, f.eventB) {
		t.Fatalf("human B cannot list its own event %d: %v", f.eventB, b)
	}
	for name, cs := range map[string]*sdk.ClientSession{"A1": f.sessA1, "A2": f.sessA2} {
		a := listedIDs(t, ctx, cs)
		if !containsID(a, f.eventA) || containsID(a, f.eventB) {
			t.Fatalf("endpoint %s listed %v, want A's %d and not B's %d", name, a, f.eventA, f.eventB)
		}
	}

	// Filters and cursors never widen the scope: a since-id bound at B's event and a provider filter
	// still return nothing of A's.
	var out listWebhookEventsOut
	callOK(t, ctx, f.sessB, "list_webhook_events",
		map[string]any{"provider": "github", "since": "1", "limit": 200}, &out)
	for _, e := range out.Events {
		if e.ID == f.eventA {
			t.Fatalf("filtered list leaked A's event %d to B", f.eventA)
		}
	}
}

// get_webhook_event: another owner's id is not_found, byte-identical to an id that does not exist.
func TestEventHistoryGetIsOwnerScoped(t *testing.T) {
	ctx, f := newHistoryFixture(t)

	foreign := callErr(t, ctx, f.sessB, "get_webhook_event", map[string]any{"id": f.eventA}, "not_found")
	unknown := callErr(t, ctx, f.sessB, "get_webhook_event", map[string]any{"id": f.eventA + 100000}, "not_found")
	if foreign != unknown {
		t.Fatalf("get_webhook_event leaks existence: foreign %q vs unknown %q", foreign, unknown)
	}
	if strings.Contains(foreign, "a-payload-only-A-may-read") {
		t.Fatalf("refusal carried A's payload: %q", foreign)
	}

	var d eventDetailOut
	callOK(t, ctx, f.sessA2, "get_webhook_event", map[string]any{"id": f.eventA}, &d)
	if d.Payload != secretA {
		t.Fatalf("A's second endpoint read payload %q, want A's", d.Payload)
	}
}

// Scenario "Replay of a foreign event": not_found, and no outbound request is made. The target is a
// live listener that counts hits, so "no request" is observed, not assumed.
func TestEventHistoryReplayOfForeignEventIsNotFound(t *testing.T) {
	ctx, f := newHistoryFixture(t)
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	foreign := callErr(t, ctx, f.sessB, "replay_webhook_event",
		map[string]any{"id": f.eventA, "target_url": target.URL}, "not_found")
	unknown := callErr(t, ctx, f.sessB, "replay_webhook_event",
		map[string]any{"id": f.eventA + 100000, "target_url": target.URL}, "not_found")
	if foreign != unknown {
		t.Fatalf("replay leaks existence: foreign %q vs unknown %q", foreign, unknown)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("a refused replay still made %d outbound request(s)", n)
	}

	// The owner's own replay passes the owner check and reaches target validation: loopback is
	// refused by the SSRF guard, which proves the call got past not_found.
	res, err := f.sessA1.CallTool(ctx, &sdk.CallToolParams{Name: "replay_webhook_event",
		Arguments: map[string]any{"id": f.eventA, "target_url": target.URL}})
	if err != nil {
		t.Fatalf("owner replay: %v", err)
	}
	if res.IsError && strings.HasPrefix(contentText(res), "not_found:") {
		t.Fatalf("owner's own replay was refused as not_found: %s", contentText(res))
	}
}

// The switchboard://events/recent resource filters exactly as list_webhook_events does.
func TestEventHistoryRecentResourceIsOwnerScoped(t *testing.T) {
	ctx, f := newHistoryFixture(t)

	read := func(cs *sdk.ClientSession) []int64 {
		t.Helper()
		res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: recentEventsURI})
		if err != nil {
			t.Fatalf("read %s: %v", recentEventsURI, err)
		}
		if len(res.Contents) != 1 {
			t.Fatalf("resource contents = %d, want 1", len(res.Contents))
		}
		var out listWebhookEventsOut
		if err := json.Unmarshal([]byte(res.Contents[0].Text), &out); err != nil {
			t.Fatalf("decode resource: %v", err)
		}
		ids := make([]int64, 0, len(out.Events))
		for _, e := range out.Events {
			ids = append(ids, e.ID)
		}
		return ids
	}
	if b := read(f.sessB); containsID(b, f.eventA) || !containsID(b, f.eventB) {
		t.Fatalf("B's recent-events resource = %v, want B's %d and never A's %d", b, f.eventB, f.eventA)
	}
	if a := read(f.sessA1); !containsID(a, f.eventA) || containsID(a, f.eventB) {
		t.Fatalf("A's recent-events resource = %v, want A's %d and never B's %d", a, f.eventA, f.eventB)
	}
}

// Deleting a webhook keeps its events visible to the endpoint's owner, and to no one else: the
// owner column survives the webhook_id reference being nulled.
func TestEventHistorySurvivesWebhookDelete(t *testing.T) {
	ctx, f := newHistoryFixture(t)
	if err := f.st.DeleteWebhook(ctx, f.webhookA, f.epA1); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	if a := listedIDs(t, ctx, f.sessA1); !containsID(a, f.eventA) {
		t.Fatalf("A lost its event %d when the webhook was deleted: %v", f.eventA, a)
	}
	if b := listedIDs(t, ctx, f.sessB); containsID(b, f.eventA) {
		t.Fatalf("B gained A's event %d after the webhook was deleted: %v", f.eventA, b)
	}
}

// A friend approval mints the remote peer's endpoint onto one of the APPROVER's agents, so the
// endpoint's owner is the approver. B's peer asks A for the event verbs, and A approves with no
// narrowing: the friend endpoint is advertised the tools, and every one of them reads nothing of
// A's. The list and the resource are empty, get and replay are not_found exactly like an unknown
// id, and the refused replay makes no outbound request. Until #420 (F3) gives friend endpoints
// their own authority, their history reach is empty.
// Governing: ADR-0038, SPEC-0033 REQ "Owner-Scoped History Reads", REQ "Closing the Audited
// Surfaces" (F3).
func TestEventHistoryFriendEndpointReadsNothing(t *testing.T) {
	ctx, f := newHistoryFixture(t)
	edge, err := f.st.CreateFriendRequest(ctx, store.CreateFriendRequestParams{
		FromPersona: "peer-b@remote.example", ToPersona: "agent-a1@local", FromHuman: f.humanB, ToHuman: f.humanA,
		RequestedQueues: []string{"reviews"},
		RequestedVerbs:  append([]string{"create_for"}, eventVerbNames...),
	})
	if err != nil {
		t.Fatalf("friend request: %v", err)
	}
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	slug, err := store.MintSlug("agent-a1")
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	_, ep, err := f.st.ApproveFriendRequest(ctx, store.ApproveFriendRequestParams{
		EdgeID: edge.ID, OwnerHumanID: f.humanA, AgentID: f.agentA1,
		CredentialHash: hash, CredentialPrefix: prefix, Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve friend request: %v", err)
	}
	for _, v := range eventVerbNames {
		if !slices.Contains(ep.ScopeVerbs, v) {
			t.Fatalf("fixture: friend grant %v lacks %s, so the test would pass on scope alone", ep.ScopeVerbs, v)
		}
	}
	friend := routeSession(t, ctx, f.st, slug, token)

	if ids := listedIDs(t, ctx, friend); len(ids) != 0 {
		t.Fatalf("friend endpoint listed %v, want none of the approver's events", ids)
	}
	res, err := friend.ReadResource(ctx, &sdk.ReadResourceParams{URI: recentEventsURI})
	if err != nil {
		t.Fatalf("read %s: %v", recentEventsURI, err)
	}
	if len(res.Contents) != 1 || strings.Contains(res.Contents[0].Text, `"id"`) {
		t.Fatalf("friend endpoint's recent-events resource = %+v, want no events", res.Contents)
	}

	foreign := callErr(t, ctx, friend, "get_webhook_event", map[string]any{"id": f.eventA}, "not_found")
	unknown := callErr(t, ctx, friend, "get_webhook_event", map[string]any{"id": f.eventA + 100000}, "not_found")
	if foreign != unknown || strings.Contains(foreign, "a-payload-only-A-may-read") {
		t.Fatalf("friend get_webhook_event leaks: foreign %q vs unknown %q", foreign, unknown)
	}

	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	callErr(t, ctx, friend, "replay_webhook_event", map[string]any{"id": f.eventA, "target_url": target.URL}, "not_found")
	if n := hits.Load(); n != 0 {
		t.Fatalf("a refused friend replay still made %d outbound request(s)", n)
	}

	// Positive control: A's own endpoint on the same agent still reads the event.
	var d eventDetailOut
	callOK(t, ctx, f.sessA1, "get_webhook_event", map[string]any{"id": f.eventA}, &d)
	if d.Payload != secretA {
		t.Fatalf("A's own endpoint read payload %q, want A's", d.Payload)
	}
}
