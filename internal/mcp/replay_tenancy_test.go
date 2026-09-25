package mcp

// Owned replay targets across tenants, over the REAL store and PostgreSQL and the PRODUCTION SSRF
// guard: a target list belongs to the endpoint that owns it, another tenant can neither read it nor
// fall back to it, and owning a target never exempts it from the guard, so a target that reached
// the database without passing the guard is still never dialed. Skips cleanly without
// SWITCHBOARD_TEST_DATABASE_URL, in the house style.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets" (F9) and its scenario "Instance trusted
// target is gone (F9)".

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

func TestReplayTargetsAreOwnedAcrossTenants(t *testing.T) {
	ctx, f := newHistoryFixture(t)

	// An internal listener stands in for the host an instance-wide allowlist used to trust. It
	// counts hits, so "never dialed" is observed rather than assumed.
	var hits atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(internal.Close)

	// Human A vends an endpoint that owns two targets: the internal host first (it reaches the row
	// only because the store does not validate; the API refuses it at vend), then a public one.
	owned := []string{internal.URL + "/hook", "https://203.0.113.10/hook"}
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	res, err := f.st.VendAgentEndpoint(ctx, store.VendParams{
		OwnerHumanID: f.humanA, Name: "replayer", CredHash: hash, CredPrefix: prefix,
		Slug: "replayer-a-99999999", Queues: []string{"reviews"}, Verbs: eventVerbNames,
		ReplayTargets: owned,
	})
	if err != nil {
		t.Fatalf("vend with replay targets: %v", err)
	}
	epA := res.Endpoint.ID

	got, err := f.st.EndpointReplayTargets(ctx, epA)
	if err != nil || !slices.Equal(got, owned) {
		t.Fatalf("A's replay targets = %v, %v; want %v in vend order", got, err, owned)
	}
	if got, err := f.st.EndpointReplayTargets(ctx, f.epB); err != nil || len(got) != 0 {
		t.Fatalf("B's replay targets = %v, %v; want none", got, err)
	}

	// B cannot read A's targets through the owner-scoped endpoint listing.
	cardsB, err := f.st.ListEndpointCards(ctx, f.humanB)
	if err != nil {
		t.Fatalf("list B's cards: %v", err)
	}
	for _, c := range cardsB {
		if c.ID == epA || len(c.ReplayTargets) != 0 {
			t.Fatalf("B's endpoint listing exposed %s with targets %v", c.ID, c.ReplayTargets)
		}
	}
	cardsA, err := f.st.ListEndpointCards(ctx, f.humanA)
	if err != nil {
		t.Fatalf("list A's cards: %v", err)
	}
	if i := slices.IndexFunc(cardsA, func(c store.EndpointCard) bool { return c.ID == epA }); i < 0 ||
		!slices.Equal(cardsA[i].ReplayTargets, owned) {
		t.Fatalf("A's listing does not show A's own targets: %+v", cardsA)
	}

	// B replays its own event with no target: A's list is never its default.
	callErr(t, ctx, f.sessB, "replay_webhook_event", map[string]any{"id": f.eventB}, codeReplayTargetRequired)

	// A's endpoint replays with no target: its first owned target is the default, and the guard
	// still refuses it. Owning a target is not an exemption.
	sessA := routeSession(t, ctx, f.st, "replayer-a-99999999", token)
	callErr(t, ctx, sessA, "replay_webhook_event", map[string]any{"id": f.eventA}, codeInvalidArgument)
	callErr(t, ctx, sessA, "replay_webhook_event",
		map[string]any{"id": f.eventA, "target_url": internal.URL + "/named"}, codeInvalidArgument)

	if n := hits.Load(); n != 0 {
		t.Fatalf("the internal host received %d replay request(s)", n)
	}
}
