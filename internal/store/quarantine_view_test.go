package store

// DB-backed coverage of the Quarantine view's reads and the trust-this-actor write (ADR-0031,
// SPEC-0026 REQ-9, REQ-5, "Tenancy"): the list and count are the owner's only, a second human reads
// nothing; the webhook signals count open items and 24-hour faults per webhook; and adding an actor
// grows the list once, keeps the match mode, leaves allow_all alone and refuses a foreign webhook.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// holdOnWebhook quarantines a verified delivery that arrived on webhook wh of endpoint ep.
func holdOnWebhook(t *testing.T, s *Store, ctx context.Context, ep, wh, disposition, reason string) Todo {
	t.Helper()
	key := fmt.Sprintf("view-hold-%d", heldSeq.Add(1))
	_, out, _, err := s.CreateIntakeEventTodos(ctx, EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed",
		Verified: true, Payload: []byte(`{"n":1}`), WebhookID: wh, Disposition: disposition,
	}, []string{ep}, CreateTodoParams{Source: "github", Kind: "issues", Title: "held " + key, Payload: []byte(`{"n":1}`),
		IdempotencyKey: key, QuarantineReason: reason, QuarantineDetail: []byte(`{"actor":{"sender":"mallory"}}`)})
	if err != nil || len(out) != 1 {
		t.Fatalf("hold: %v (%d todos)", err, len(out))
	}
	return out[0].Todo
}

func TestQuarantineViewReadsAreOwnerScoped(t *testing.T) {
	s, ctx := testStore(t)
	epA := seedEndpoint(t, s, ctx, "view-a", "q")
	epB := seedEndpoint(t, s, ctx, "view-b", "q")
	humanA, humanB := ownerOf(t, s, ctx, epA), ownerOf(t, s, ctx, epB)
	whA, err := s.CreateWebhook(ctx, epA, "github", "q", "signed", fmt.Sprintf("tok-view-a-%d", heldSeq.Add(1)), "whsec_v", 5)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	first := holdOnWebhook(t, s, ctx, epA, whA.ID, DispositionQuarantined, "untrusted_actor")
	second := holdOnWebhook(t, s, ctx, epA, whA.ID, DispositionFaulted, "rule_fault")
	gone := holdOnWebhook(t, s, ctx, epA, whA.ID, DispositionQuarantined, "untrusted_actor")
	if _, err := s.DiscardQuarantined(ctx, humanA, gone.ID, "human:"+humanA, "spam"); err != nil {
		t.Fatalf("discard: %v", err)
	}

	items, err := s.ListQuarantinedForHuman(ctx, humanA, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.Todo.ID)
		if it.Event.WebhookID != whA.ID || string(it.Event.Payload) != `{"n":1}` {
			t.Fatalf("item %s event = %+v, want the held delivery on %s", it.Todo.ID, it.Event, whA.ID)
		}
	}
	if !slices.Equal(ids, []string{second.ID, first.ID}) {
		t.Fatalf("list = %v, want the two open items newest first (%s, %s), without the discarded one", ids, second.ID, first.ID)
	}
	if n, err := s.CountQuarantinedForHuman(ctx, humanA); err != nil || n != 2 {
		t.Fatalf("count = %d (%v), want 2", n, err)
	}

	// Human B reads nothing of A's.
	if items, err := s.ListQuarantinedForHuman(ctx, humanB, 0); err != nil || len(items) != 0 {
		t.Fatalf("B's list = %+v (%v), want empty", items, err)
	}
	if n, err := s.CountQuarantinedForHuman(ctx, humanB); err != nil || n != 0 {
		t.Fatalf("B's count = %d (%v), want 0", n, err)
	}
	if sigs, err := s.WebhookSignalsForHuman(ctx, humanB); err != nil || len(sigs) != 0 {
		t.Fatalf("B's webhook signals = %+v (%v), want none", sigs, err)
	}

	// A's webhook card: two open items, one fault in 24h. An old fault falls outside the window.
	old := holdOnWebhook(t, s, ctx, epA, whA.ID, DispositionFaulted, "rule_fault")
	if _, err := s.pool.Exec(ctx, `UPDATE events SET received_at = now() - interval '25 hours'
		WHERE id = $1`, *old.EventID); err != nil {
		t.Fatalf("age the fault: %v", err)
	}
	sigs, err := s.WebhookSignalsForHuman(ctx, humanA)
	if err != nil || len(sigs) != 1 {
		t.Fatalf("signals = %+v (%v), want A's one webhook", sigs, err)
	}
	if got := sigs[0]; got.WebhookID != whA.ID || got.EndpointID != epA || got.Quarantined != 3 || got.Faults24h != 1 || got.AllowAll {
		t.Fatalf("signal = %+v, want 3 open items, 1 fault in 24h, not allow_all", got)
	}
	if _, err := s.SetWebhookTrustedActors(ctx, whA.ID, epA, []byte(`{"allow_all":true}`)); err != nil {
		t.Fatalf("allow_all: %v", err)
	}
	if sigs, _ := s.WebhookSignalsForHuman(ctx, humanA); len(sigs) != 1 || !sigs[0].AllowAll {
		t.Fatalf("signals after allow_all = %+v, want the flag", sigs)
	}
}

func TestAddWebhookTrustedActors(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "trust-add", "q")
	other := seedEndpoint(t, s, ctx, "trust-add-other", "q")
	gh, err := s.CreateWebhookWithTrust(ctx, ep, "github", "q", "signed", fmt.Sprintf("tok-add-gh-%d", heldSeq.Add(1)), "whsec_a", 5,
		[]byte(`{"logins":["joestump"],"match":"author"}`))
	if err != nil {
		t.Fatalf("github webhook: %v", err)
	}

	// SPEC-0026 REQ-9 scenario "Trust this actor": ["joestump"] becomes ["joestump", "newcontributor"].
	w, changed, err := s.AddWebhookTrustedActors(ctx, gh.ID, ep, []string{"newcontributor"})
	if err != nil || !changed || !sameJSON(t, w.TrustedActors, []byte(`{"logins":["joestump","newcontributor"],"match":"author"}`)) {
		t.Fatalf("add = %s, %v (%v)", w.TrustedActors, changed, err)
	}
	// Logins compare case-insensitively: adding a known name again changes nothing.
	if _, changed, err := s.AddWebhookTrustedActors(ctx, gh.ID, ep, []string{"NewContributor", ""}); err != nil || changed {
		t.Fatalf("re-add = %v (%v), want unchanged", changed, err)
	}
	// Another endpoint's, an unknown, and a malformed id are not found.
	for _, c := range []struct{ id, ep string }{
		{gh.ID, other}, {"00000000-0000-0000-0000-000000000000", ep}, {"not-a-uuid", ep},
	} {
		if _, _, err := s.AddWebhookTrustedActors(ctx, c.id, c.ep, []string{"x"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("add(%s, %s) = %v, want ErrNotFound", c.id, c.ep, err)
		}
	}
	// Concurrent additions both land (the row lock serialises the read-modify-write).
	var wg sync.WaitGroup
	for _, n := range []string{"alice", "bob"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.AddWebhookTrustedActors(ctx, gh.ID, ep, []string{n}); err != nil {
				t.Errorf("concurrent add %s: %v", n, err)
			}
		}()
	}
	wg.Wait()
	got, err := s.WebhookForEndpoint(ctx, gh.ID, ep)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, n := range []string{"joestump", "newcontributor", "alice", "bob"} {
		if !strings.Contains(string(got.TrustedActors), `"`+n+`"`) {
			t.Fatalf("trust list %s lost %q", got.TrustedActors, n)
		}
	}
	// An entry over the byte limit is refused, and nothing is written.
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'x'
	}
	if _, _, err := s.AddWebhookTrustedActors(ctx, gh.ID, ep, []string{string(long)}); !errors.Is(err, ErrTrustListRefused) {
		t.Fatalf("oversize add = %v, want ErrTrustListRefused", err)
	}

	// cairn trusts actor ids, compared exactly.
	cairn, err := s.CreateWebhook(ctx, ep, "cairn", "q", "signed", fmt.Sprintf("tok-add-cairn-%d", heldSeq.Add(1)), "whsec_c", 5)
	if err != nil {
		t.Fatalf("cairn webhook: %v", err)
	}
	w, changed, err = s.AddWebhookTrustedActors(ctx, cairn.ID, ep, []string{"acct_joe"})
	if err != nil || !changed || !sameJSON(t, w.TrustedActors, []byte(`{"actor_ids":["acct_joe"]}`)) {
		t.Fatalf("cairn add = %s, %v (%v)", w.TrustedActors, changed, err)
	}
	if _, changed, _ := s.AddWebhookTrustedActors(ctx, cairn.ID, ep, []string{"ACCT_JOE"}); !changed {
		t.Fatal("cairn actor ids compared case-insensitively, want exact")
	}

	// allow_all already trusts everyone: nothing to add, nothing changes.
	if _, err := s.SetWebhookTrustedActors(ctx, gh.ID, ep, []byte(`{"allow_all":true}`)); err != nil {
		t.Fatalf("allow_all: %v", err)
	}
	if w, changed, err := s.AddWebhookTrustedActors(ctx, gh.ID, ep, []string{"someone"}); err != nil || changed ||
		!sameJSON(t, w.TrustedActors, []byte(`{"allow_all":true}`)) {
		t.Fatalf("add under allow_all = %s, %v (%v), want unchanged", w.TrustedActors, changed, err)
	}

	// A source with no actor projection has no trust list to grow.
	gen, err := s.CreateWebhook(ctx, ep, "generic", "q", "token", fmt.Sprintf("tok-add-gen-%d", heldSeq.Add(1)), "", 5)
	if err != nil {
		t.Fatalf("generic webhook: %v", err)
	}
	if _, _, err := s.AddWebhookTrustedActors(ctx, gen.ID, ep, []string{"x"}); !errors.Is(err, ErrTrustListRefused) {
		t.Fatalf("generic add = %v, want ErrTrustListRefused", err)
	}
}
