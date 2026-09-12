package server

// The provider registry is instance-wide (`adapters` has no owner column), so it is gated on
// cfg.OperatorSubjects rather than scoped by owner. These drive the REAL router through real
// sessions, so they exercise the middleware wiring rather than the predicate in isolation — the
// gap that made this necessary was a route nobody had wired the gate onto, not a wrong predicate.
//
// Governing: SPEC-0017 REQ "Provider Lifecycle"; SPEC-0007 REQ "Human as Accountable Principal".

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

// Every administrative provider route refuses a signed-in NON-operator with 404.
//
// 404 rather than 403 throughout: a 403 confirms the named provider — and the registry itself —
// exists on this deployment, the same existence oracle the todo drawer avoids.
func TestProviderAdminRoutesRefuseNonOperators(t *testing.T) {
	r, st, ctx, _ := newReceiverDBRouter(t)
	// A perfectly ordinary invited user: authenticated, and not in testOperatorSubjects.
	_, guest := mintSession(t, st, ctx, "prov-guest", "Guest", "guest@example.com")
	csrf := scrapeCSRF(t, getAs(t, r, guest, "/todos").Body.String())

	t.Run("lifecycle POSTs", func(t *testing.T) {
		for _, action := range []string{"disable", "enable", "rotate", "remove"} {
			rec := postFormAs(t, r, guest, csrf, "/providers/gitea/"+action, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST /providers/gitea/%s as a non-operator = %d, want 404 "+
					"(a 2xx here deletes or disables another tenant's receiver)", action, rec.Code)
			}
		}
	})

	// The wizard is the CREATE path into the same registry — its terminal step seeds a row — so it
	// carries the same gate. Gating only the final submit would leave the earlier steps as a
	// working directory of what this deployment can ingest, and a non-operator reaching the end
	// could register a new enabled receiver: an open-trust webhook, or a queue provider pointed at
	// a target of their choosing. Worse than the removal the gate was written for, and missed on
	// the first pass — found in review of #180.
	t.Run("connect wizard", func(t *testing.T) {
		for _, path := range []string{"/providers/connect", "/providers/connect/kind"} {
			if rec := getAs(t, r, guest, path); rec.Code != http.StatusNotFound {
				t.Errorf("GET %s as a non-operator = %d, want 404", path, rec.Code)
			}
		}
		if rec := postFormAs(t, r, guest, csrf, "/providers/connect/kind", nil); rec.Code != http.StatusNotFound {
			t.Errorf("POST /providers/connect/kind as a non-operator = %d, want 404", rec.Code)
		}
	})

	// And the page itself neither names this deployment's receivers nor advertises the create path.
	t.Run("panel reveals nothing", func(t *testing.T) {
		body := getAs(t, r, guest, "/providers").Body.String()
		for _, needle := range []string{"sb-pr-gitea", "sb-pr-github", "/webhooks/gitea", "/providers/connect"} {
			if strings.Contains(body, needle) {
				t.Errorf("/providers as a non-operator reveals %q", needle)
			}
		}
	})
}

// An operator keeps the full surface — the gate must not be a feature removal for the human it
// exists to serve.
func TestProviderAdminRoutesAllowOperators(t *testing.T) {
	r, st, ctx, _ := newReceiverDBRouter(t)
	_, op := mintSession(t, st, ctx, testOperatorSubjects[0], "Prov Op", "op@example.com")

	// Seed a registry row so "shows configured receivers" is a real assertion: this fixture starts
	// with an empty adapters table, and an empty panel would pass a looser check for the wrong
	// reason.
	if _, err := st.SeedProvider(ctx, store.ProviderSeed{
		Name: "gate-fixture", Family: "webhook", Kind: "generic", TrustMode: "token",
		Secret: "tok-gate-fixture", Config: []byte(`{"queue":"reviews"}`),
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	body := getAs(t, r, op, "/providers").Body.String()
	if !strings.Contains(body, "gate-fixture") {
		t.Fatal("an operator's /providers shows no configured receivers — the gate over-reached")
	}
	if !strings.Contains(body, "/providers/connect") {
		t.Fatal("an operator is not offered the connect path — the gate over-reached")
	}
	if rec := getAs(t, r, op, "/providers/connect"); rec.Code == http.StatusNotFound {
		t.Fatal("an operator cannot reach the connect wizard — the gate over-reached")
	}
}
