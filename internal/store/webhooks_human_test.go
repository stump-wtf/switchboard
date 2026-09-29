package store

// Tenancy for the human API's webhook reads (SPEC-0035 REQ "Reach on Every Route"): human B learns
// nothing of human A's webhooks, whether listing or resolving an owner by id. Asserted on A's actual
// ids, in this package's "B must learn nothing of A's" style (tenancy_isolation_test.go).

import (
	"errors"
	"slices"
	"testing"

	"github.com/stump-wtf/switchboard/internal/routing"
)

func TestTenancyWebhooksForHumanStayInReach(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, epB := tenants(t, s, ctx)

	// A second endpoint for A, on a second agent: reach is the human, not one endpoint.
	ag, err := s.CreateAgent(ctx, humanA, "tenant-a-second", "")
	if err != nil {
		t.Fatalf("second agent: %v", err)
	}
	slug, err := MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	epA2, err := s.CreateEndpoint(ctx, ag.ID, "credhash-a2-webhooks", "sbk_a2", slug, []string{"q"}, []string{"list_todos"})
	if err != nil {
		t.Fatalf("second endpoint: %v", err)
	}

	hook := func(ep, token string) string {
		w, err := s.CreateWebhook(ctx, ep, "github", "q", "signed", token, "whsec_never_listed", 10)
		if err != nil {
			t.Fatalf("webhook on %s: %v", ep, err)
		}
		return w.ID
	}
	whA := hook(epA, "tok-human-a")
	whA2 := hook(epA2.ID, "tok-human-a2")
	whB := hook(epB, "tok-human-b")
	if _, err := s.UpdateWebhookRouting(ctx, whA, epA, func(WebhookRouting) (routing.Config, error) {
		return routing.Config{
			Rules:   []routing.Rule{{ID: "a", Expr: "true", Action: routing.Action{Drop: true}}, {ID: "b", Expr: "false", Action: routing.Action{Drop: true}}},
			Default: &routing.Action{Drop: true}, Params: map[string]any{"k": "v"},
		}, nil
	}); err != nil {
		t.Fatalf("seed routing: %v", err)
	}

	ids := func(rows []HumanWebhook) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	rowsA, err := s.ListWebhooksForHuman(ctx, humanA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	wantA := []string{whA, whA2}
	slices.Sort(wantA)
	if got := ids(rowsA); !slices.Equal(got, wantA) {
		t.Fatalf("A's webhooks = %v, want %v", got, wantA)
	}
	for _, r := range rowsA {
		switch r.ID {
		case whA:
			if r.EndpointID != epA || r.RuleCount != 2 || !r.HasDefault || !r.HasParams || r.EndpointState != "active" {
				t.Fatalf("A's routed webhook = %+v", r)
			}
		case whA2:
			if r.EndpointID != epA2.ID || r.EndpointSlug != slug || r.RuleCount != 0 || r.HasDefault || r.HasParams {
				t.Fatalf("A's plain webhook = %+v", r)
			}
		}
	}
	rowsB, err := s.ListWebhooksForHuman(ctx, humanB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if got := ids(rowsB); !slices.Equal(got, []string{whB}) {
		t.Fatalf("B's webhooks = %v, want only %s", got, whB)
	}
	if rows, err := s.ListWebhooksForHuman(ctx, "not-a-uuid"); err != nil || len(rows) != 0 {
		t.Fatalf("malformed human = %v, %v; want nothing", rows, err)
	}

	// Owner resolution: A's own webhooks resolve to their owning endpoints; B's, an unknown id and a
	// malformed one are the same ErrNotFound.
	if o, err := s.WebhookOwnerForHuman(ctx, whA2, humanA); err != nil || o.EndpointID != epA2.ID || o.EndpointSlug != slug || o.EndpointState != "active" {
		t.Fatalf("owner of A2 = %+v, %v", o, err)
	}
	for _, c := range []struct{ webhook, human string }{
		{whB, humanA}, {whA, humanB}, {"6ba7b810-9dad-11d1-80b4-00c04fd430c8", humanA}, {"nope", humanA}, {whA, "nope"},
	} {
		if o, err := s.WebhookOwnerForHuman(ctx, c.webhook, c.human); !errors.Is(err, ErrNotFound) || o != (WebhookOwner{}) {
			t.Fatalf("owner of %s for %s = %+v, %v; want ErrNotFound", c.webhook, c.human, o, err)
		}
	}

	// A revoked endpoint's webhook stays listed and resolvable, carrying the state.
	if err := s.RevokeEndpoint(ctx, epA2.ID, humanA); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if o, err := s.WebhookOwnerForHuman(ctx, whA2, humanA); err != nil || o.EndpointState != "revoked" {
		t.Fatalf("owner of revoked A2 = %+v, %v", o, err)
	}
	rowsA, _ = s.ListWebhooksForHuman(ctx, humanA)
	if len(rowsA) != 2 || rowsA[0].ID != whA || rowsA[1].ID != whA2 || rowsA[1].EndpointState != "revoked" {
		t.Fatalf("A's list after revoke = %+v, want the active endpoint's webhook first and A2 revoked", rowsA)
	}
}
