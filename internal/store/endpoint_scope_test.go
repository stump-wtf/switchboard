// Add-only endpoint scope widening: the mechanism, its audit trail, and the failure that motivated it
// (issue #590, ADR-0048 proposed).
//
// The report is concrete: a rule save was refused with "queue merge is not in the webhook owner's
// allowed webhook queues", and the only way past it was to re-vend — a new endpoint, a new webhook, a
// new ingest URL and signing secret — just to let one more lane receive events. These tests hold the two
// halves of the fix: the grant the router consults really does move, and it moves only outward.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0048 (proposed), ADR-0012,
// SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", ADR-0022 (tenant isolation).
//
// @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/routing"
)

// seedWidenTarget vends one endpoint with an explicit webhook ceiling, which is the part of a vend the
// widen moves. It returns the operator who owns it and the endpoint.
func seedWidenTarget(t testing.TB, s *Store, ctx context.Context, label string, webhookQueues []string, webhookMax int) (string, string) {
	t.Helper()
	n := seedEndpointCounter.Add(1)
	uniq := fmt.Sprintf("%s-%d", label, n)
	h, err := s.UpsertHuman(ctx, "pocket|"+uniq, label, uniq+"@example.com")
	if err != nil {
		t.Fatalf("seed human: %v", err)
	}
	slug, err := MintSlug(label)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	res, err := s.VendAgentEndpoint(ctx, VendParams{
		OwnerHumanID:       h.ID,
		Name:               label,
		CredHash:           "widen-credhash-" + uniq,
		CredPrefix:         "sbk_" + uniq,
		Slug:               slug,
		Queues:             []string{"lane-a"},
		Verbs:              []string{"list_todos", "claim", "complete"},
		WebhookMax:         webhookMax,
		WebhookSourceTypes: []string{"generic"},
		WebhookQueues:      webhookQueues,
	})
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	return h.ID, res.Endpoint.ID
}

// The report's failure, end to end: a rule that routes to a queue outside the grant is refused, and the
// same rule validates the moment an operator adds that queue — with no new endpoint, no new webhook and
// no new credential.
func TestWidenQueueOpensTheRouteThat403d(t *testing.T) {
	s, ctx := testStore(t)
	humanID, endpointID := seedWidenTarget(t, s, ctx, "widener", []string{"lane-a"}, 5)

	wh, err := s.CreateWebhook(ctx, endpointID, "generic", "lane-a", "token", "tok-widen-route", "", 5)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	granted := func(queue string) bool {
		wr, err := s.WebhookRoutingForEndpoint(ctx, wh.ID, endpointID)
		if err != nil {
			t.Fatalf("read routing: %v", err)
		}
		return routing.QueueGranted(queue, routing.Grant{TargetQueue: wr.TargetQueue, Queues: wr.WebhookQueues})
	}
	if !granted("lane-a") {
		t.Fatalf("the vended queue is not granted before any widen")
	}
	if granted("merge") {
		t.Fatalf("merge is granted before a widen; the report's 403 cannot reproduce")
	}

	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID,
		ScopeAdditions{WebhookQueues: []string{"merge"}}); err != nil {
		t.Fatalf("widen: %v", err)
	}
	if !granted("merge") {
		t.Fatalf("after add-queue the router still refuses merge: the enforcement point does not honor a widen")
	}
	if !granted("lane-a") {
		t.Fatalf("a widen dropped the queue the endpoint already had")
	}
}

func TestWidenScopeAuditsOldAndNew(t *testing.T) {
	s, ctx := testStore(t)
	humanID, endpointID := seedWidenTarget(t, s, ctx, "auditor", []string{"lane-a"}, 2)

	max := 4
	res, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID, ScopeAdditions{
		WebhookQueues:      []string{"merge", "lane-a"},
		WebhookSourceTypes: []string{"github"},
		WebhookVerbs:       []string{"list_todos"},
		WebhookMax:         &max,
	})
	if err != nil {
		t.Fatalf("widen: %v", err)
	}
	// lane-a was already granted and list_todos was already in the vend: neither is a change, so the
	// trail names the three fields that actually moved.
	got := []string{}
	for _, w := range res.Widened {
		got = append(got, w.Field)
	}
	if want := []string{"webhook_queues", "webhook_source_types", "webhook_max"}; !slices.Equal(got, want) {
		t.Fatalf("audit fields = %q, want %q", got, want)
	}
	for _, w := range res.Widened {
		if w.Field == "webhook_max" {
			if w.Before != int64(2) || w.After != int64(4) {
				t.Errorf("webhook_max audit = %v → %v, want 2 → 4", w.Before, w.After)
			}
			continue
		}
		before, ok := w.Before.([]string)
		if !ok {
			t.Fatalf("%s before is %T, want []string", w.Field, w.Before)
		}
		after, ok := w.After.([]string)
		if !ok {
			t.Fatalf("%s after is %T, want []string", w.Field, w.After)
		}
		for _, b := range before {
			if !slices.Contains(after, b) {
				t.Errorf("%s: %q was in the grant before and is gone after: %q", w.Field, b, after)
			}
		}
	}

	history, err := s.ListEndpointScopeWidenings(ctx, endpointID, humanID)
	if err != nil {
		t.Fatalf("list widenings: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d entries, want 3: %+v", len(history), history)
	}
	if history[0].Field != "webhook_queues" || !slices.Contains(history[0].After.([]string), "merge") {
		t.Errorf("oldest entry = %+v, want webhook_queues gaining merge", history[0])
	}
}

// Re-adding what the endpoint already grants is the script's retry, not a new event: it succeeds, and it
// writes no history.
func TestWidenIsIdempotentAndWritesNothing(t *testing.T) {
	s, ctx := testStore(t)
	humanID, endpointID := seedWidenTarget(t, s, ctx, "repeater", []string{"lane-a"}, 3)

	res, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID, ScopeAdditions{WebhookQueues: []string{"lane-a"}})
	if err != nil {
		t.Fatalf("repeat widen: %v", err)
	}
	if len(res.Widened) != 0 {
		t.Fatalf("a no-op widen wrote audit entries: %+v", res.Widened)
	}
	if !slices.Equal(res.After.WebhookQueues, []string{"lane-a"}) {
		t.Fatalf("queues after a no-op widen = %q", res.After.WebhookQueues)
	}
	history, err := s.ListEndpointScopeWidenings(ctx, endpointID, humanID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("a no-op widen wrote history: %+v", history)
	}
}

// The widen has no vocabulary for taking anything away: arrays are unioned, and the one field where a
// bare value could express a reduction is refused.
func TestWidenCannotNarrow(t *testing.T) {
	s, ctx := testStore(t)
	humanID, endpointID := seedWidenTarget(t, s, ctx, "narrower", []string{"lane-a", "lane-b"}, 6)

	lower := 3
	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID, ScopeAdditions{WebhookMax: &lower}); !errors.Is(err, ErrScopeNarrowing) {
		t.Fatalf("lowering webhook_max err = %v, want ErrScopeNarrowing", err)
	}
	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID, ScopeAdditions{}); !errors.Is(err, ErrNothingToWiden) {
		t.Fatalf("empty additions err = %v, want ErrNothingToWiden", err)
	}
	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID,
		ScopeAdditions{WebhookQueues: []string{QueueQuarantine}}); !errors.Is(err, ErrReservedQueue) {
		t.Fatalf("reserved queue err = %v, want ErrReservedQueue", err)
	}

	sc, err := s.EndpointScopeForHuman(ctx, endpointID, humanID)
	if err != nil {
		t.Fatalf("read scope: %v", err)
	}
	if sc.WebhookMax != 6 || !slices.Contains(sc.WebhookQueues, "lane-b") {
		t.Fatalf("a refused widen changed the grant: %+v", sc)
	}

	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID, ScopeAdditions{WebhookQueues: []string{"  "}}); !errors.Is(err, ErrNothingToWiden) {
		t.Fatalf("blank queue name err = %v, want ErrNothingToWiden", err)
	}
}

// Widening is the operator's act, on their own endpoint: another human's endpoint answers like one that
// does not exist, and a revoked one says so instead of pretending to widen.
func TestWidenIsRefusedOutsideOwnership(t *testing.T) {
	s, ctx := testStore(t)
	_, endpointID := seedWidenTarget(t, s, ctx, "owner", []string{"lane-a"}, 2)
	otherHuman, err := s.UpsertHuman(ctx, "pocket|not-the-owner", "Other", "other@example.com")
	if err != nil {
		t.Fatalf("seed other human: %v", err)
	}

	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, otherHuman.ID,
		ScopeAdditions{WebhookQueues: []string{"merge"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner err = %v, want ErrNotFound", err)
	}
	if _, err := s.ListEndpointScopeWidenings(ctx, endpointID, otherHuman.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner history err = %v, want ErrNotFound", err)
	}

	ownerID, _ := s.EndpointOwner(ctx, endpointID)
	if err := s.RevokeEndpoint(ctx, endpointID, ownerID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, ownerID,
		ScopeAdditions{WebhookQueues: []string{"merge"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked endpoint err = %v, want ErrConflict", err)
	}
}

// The point of a widen over a re-vend is that the credential does not move: the token that authenticates
// the endpoint today must still authenticate it after the grant widens, or the operator is back to
// editing the source system's hook configuration.
func TestWidenLeavesTheCredentialAlone(t *testing.T) {
	s, ctx := testStore(t)
	humanID, endpointID := seedWidenTargetWithCred(t, s, ctx, "cred-keeper", "widen-credhash-untouched", []string{"lane-a"}, 2)
	before, err := s.EndpointByCredHash(ctx, "widen-credhash-untouched")
	if err != nil {
		t.Fatalf("the vended credential does not authenticate: %v", err)
	}
	if before.ID != endpointID {
		t.Fatalf("the seeded credential resolves to %s, want %s", before.ID, endpointID)
	}

	if _, err := s.WidenEndpointScopeForHuman(ctx, endpointID, humanID,
		ScopeAdditions{WebhookQueues: []string{"merge"}}); err != nil {
		t.Fatalf("widen: %v", err)
	}

	after, err := s.EndpointByCredHash(ctx, "widen-credhash-untouched")
	if err != nil {
		t.Fatalf("the same credential stopped authenticating after a widen: %v", err)
	}
	if after.ID != before.ID || after.Slug != before.Slug {
		t.Fatalf("a widen moved the endpoint itself: %+v → %+v", before, after)
	}
	if !slices.Contains(after.WebhookQueues, "merge") || !slices.Contains(after.WebhookQueues, "lane-a") {
		t.Fatalf("the widened grant is not the one the credential authenticates to: %q", after.WebhookQueues)
	}
}

// seedWidenTargetWithCred is seedWidenTarget with a caller-chosen credential hash, so a test can present
// that credential again after a widen and see that it still works.
func seedWidenTargetWithCred(t testing.TB, s *Store, ctx context.Context, label, credHash string, webhookQueues []string, webhookMax int) (string, string) {
	t.Helper()
	h, err := s.UpsertHuman(ctx, "pocket|"+credHash, label, credHash+"@example.com")
	if err != nil {
		t.Fatalf("seed human: %v", err)
	}
	slug, err := MintSlug(label)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	res, err := s.VendAgentEndpoint(ctx, VendParams{
		OwnerHumanID: h.ID, Name: label,
		CredHash: credHash, CredPrefix: "sbk_" + label, Slug: slug,
		Queues: []string{"lane-a"}, Verbs: []string{"list_todos", "claim", "complete"},
		WebhookMax: webhookMax, WebhookSourceTypes: []string{"generic"}, WebhookQueues: webhookQueues,
	})
	if err != nil {
		t.Fatalf("vend: %v", err)
	}
	return h.ID, res.Endpoint.ID
}

// WidenScope is the pure half: what the operator confirms is the same computation the write applies, so
// a preview can never promise a different change than the one that lands.
func TestWidenScopePreviewMatchesTheWrite(t *testing.T) {
	before := EndpointScope{ScopeQueues: []string{"lane-a"}, ScopeVerbs: []string{"claim"},
		WebhookMax: 1, WebhookSourceTypes: []string{"generic"}, WebhookQueues: []string{"lane-a"}}
	after, err := WidenScope(before, ScopeAdditions{WebhookQueues: []string{"merge", "lane-a"}, WebhookVerbs: []string{"list_todos"}})
	if err != nil {
		t.Fatalf("WidenScope: %v", err)
	}
	if !slices.Equal(after.WebhookQueues, []string{"lane-a", "merge"}) {
		t.Errorf("webhook_queues = %q, want lane-a then merge", after.WebhookQueues)
	}
	if !slices.Equal(after.ScopeVerbs, []string{"claim", "list_todos"}) {
		t.Errorf("scope_verbs = %q", after.ScopeVerbs)
	}
	if !slices.Equal(after.ScopeQueues, before.ScopeQueues) || after.WebhookMax != before.WebhookMax {
		t.Errorf("a preview touched what a widen does not own: %+v", after)
	}
	if len(ScopeWidenings(before, after)) != 2 {
		t.Errorf("preview describes %d fields, want 2", len(ScopeWidenings(before, after)))
	}
	if len(ScopeWidenings(before, before)) != 0 {
		t.Error("an unchanged scope describes widenings")
	}
	if _, err := WidenScope(before, ScopeAdditions{WebhookQueues: []string{strings.TrimSpace("lane-a"), ""}}); err != nil {
		t.Errorf("a preview of an already-granted queue failed: %v", err)
	}
}
