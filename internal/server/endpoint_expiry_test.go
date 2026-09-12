// End-to-end credential-lifetime tests through the real router + PostgreSQL: the vend form accepts
// an optional lifetime (persisting endpoints.expires_at), rejects a malformed one before minting
// anything, exposes the expiry as countdown DATA on the Endpoints view (data-sb-expires-at — card
// styling lands with the SPEC-0015 stories), and the MCP bearer surface answers an expired
// credential with the same 401 as a revoked one. Skipped without SWITCHBOARD_TEST_DATABASE_URL,
// like the other DB-backed server tests.
// Governing: SPEC-0016 REQ "Credential Lifetime" (scenario "Expiry enforcement"), ADR-0019,
// SPEC-0007 revocation semantics reused.
package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/store"
)

// TestVendWithLifetimePersistsExpiryAndExposesCountdownData: a vend submitting lifetime=24h mints
// an endpoint whose expires_at sits ~24h out, and the Endpoints view carries the machine-readable
// countdown data for that card. A vend without a lifetime persists no expiry and renders no
// countdown data.
func TestVendWithLifetimePersistsExpiryAndExposesCountdownData(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	csrf := scrapeCSRF(t, getAs(t, r, token, "/endpoints").Body.String())

	rec := postForm(t, r, token, "/endpoints/vend", url.Values{
		"csrf_token": {csrf}, "name": {"short-lived-bot"}, "queues": {"reviews"},
		"verbs": {"claim"}, "lifetime": {"24h"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("vend with lifetime: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(cards))
	}
	if cards[0].ExpiresAt == nil {
		t.Fatal("vended endpoint has no expires_at despite lifetime=24h")
	}
	want := time.Now().Add(24 * time.Hour)
	if d := cards[0].ExpiresAt.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("expires_at %v is not ~24h out (drift %v)", cards[0].ExpiresAt, d)
	}

	// The Endpoints view exposes the expiry as countdown data on the card.
	body := getAs(t, r, token, "/endpoints").Body.String()
	if !strings.Contains(body, "data-sb-expires-at=") {
		t.Error("endpoints view missing data-sb-expires-at countdown data for the expiring card")
	}
	if !strings.Contains(body, "sb-ep-expiry-"+cards[0].ID) {
		t.Error("endpoints view missing the per-card expiry element id")
	}

	// A no-lifetime vend stays NULL and renders no countdown data on its card.
	csrf2 := scrapeCSRF(t, getAs(t, r, token, "/endpoints").Body.String())
	rec2 := postForm(t, r, token, "/endpoints/vend", url.Values{
		"csrf_token": {csrf2}, "name": {"forever-bot"}, "queues": {"reviews"}, "verbs": {"claim"},
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("vend without lifetime: got %d, want 200", rec2.Code)
	}
	cards, err = st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	for _, c := range cards {
		if c.AgentName == "forever-bot" && c.ExpiresAt != nil {
			t.Errorf("no-lifetime vend persisted expires_at %v; want NULL (valid until revoked)", c.ExpiresAt)
		}
	}
}

// TestVendRejectsMalformedLifetimeAndMintsNothing: a lifetime the server cannot parse — or a
// non-positive one — is a 400 that mints neither agent nor endpoint, exactly like the scope gate.
func TestVendRejectsMalformedLifetimeAndMintsNothing(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	csrf := scrapeCSRF(t, getAs(t, r, token, "/endpoints").Body.String())

	for name, lifetime := range map[string]string{
		"garbage":      "soon",
		"negative":     "-1h",
		"zero":         "0h",
		"bad day form": "1.5d",
	} {
		rec := postForm(t, r, token, "/endpoints/vend", url.Values{
			"csrf_token": {csrf}, "name": {"bot"}, "queues": {"reviews"},
			"verbs": {"claim"}, "lifetime": {lifetime},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s lifetime %q: got %d, want 400", name, lifetime, rec.Code)
		}
	}

	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("rejected vends minted %d endpoint(s); want 0", len(cards))
	}
	agents, err := st.ListAgents(ctx, human.ID)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("rejected vends created %d agent(s); want 0", len(agents))
	}
}

// mcpPost fires a bearer-authenticated POST at the vended MCP mount and returns the recorder. The
// empty body means an authenticated request still fails protocol-wise (400) — these tests bind to
// the AUTH answer only: 401 means the credential is dead.
func mcpPost(t *testing.T, mcpRoutes http.Handler, slug, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+slug+"/", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	mcpRoutes.ServeHTTP(rec, req)
	return rec
}

// vendWithExpiry mints an agent-level endpoint through the store's vend transaction with the given
// optional expiry, returning the plaintext bearer and the endpoint.
func vendWithExpiry(t *testing.T, st *store.Store, ctx context.Context, humanID, name string, expiresAt *time.Time) (string, store.Endpoint) {
	t.Helper()
	tok, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	slug, err := store.MintSlug(name)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	res, err := st.VendAgentEndpoint(ctx, store.VendParams{
		OwnerHumanID: humanID, Name: name, CredHash: hash, CredPrefix: prefix, Slug: slug,
		Queues: []string{"reviews"}, Verbs: []string{"claim"}, ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("vend %s: %v", name, err)
	}
	return tok, res.Endpoint
}

// TestExpiredCredentialFailsMCPAuthIdenticallyToRevocation: SPEC-0016 scenario "Expiry
// enforcement" — once an endpoint's expiry passes, bearer access through the real MCP auth stack
// answers 401 exactly like a revoked endpoint's credential, with no reaper tick required (the row
// is still state='active'); before expiry the same auth path passes. The live-session teardown
// half of the scenario is bound in TestReaperClosesSessionsForExpiredEndpoints (reaper →
// CloseEndpointSessions wiring).
func TestExpiredCredentialFailsMCPAuthIdenticallyToRevocation(t *testing.T) {
	_, st, ctx := newDBRouter(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	routes := mcph.Routes()

	human, _ := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")

	// Live: expiry an hour out — the credential authenticates (the empty body then fails
	// PROTOCOL-wise, never auth-wise).
	future := time.Now().Add(time.Hour).UTC()
	liveTok, liveEP := vendWithExpiry(t, st, ctx, human.ID, "live-bot", &future)
	if rec := mcpPost(t, routes, liveEP.Slug, liveTok); rec.Code == http.StatusUnauthorized {
		t.Fatalf("unexpired credential answered 401: %q", rec.Body.String())
	}

	// Expired: the endpoint's expiry has passed while the row is STILL active — auth answers 401
	// with no reaper involvement.
	past := time.Now().Add(-time.Second).UTC()
	expiredTok, expiredEP := vendWithExpiry(t, st, ctx, human.ID, "expired-bot", &past)
	expiredRec := mcpPost(t, routes, expiredEP.Slug, expiredTok)
	if expiredRec.Code != http.StatusUnauthorized {
		t.Fatalf("expired credential: got %d, want 401", expiredRec.Code)
	}

	// Revoked sibling: expiry is indistinguishable from revocation at the auth boundary — same
	// status, same body.
	revokedTok, revokedEP := vendWithExpiry(t, st, ctx, human.ID, "revoked-bot", nil)
	if err := st.RevokeEndpoint(ctx, revokedEP.ID, human.ID); err != nil {
		t.Fatalf("revoke endpoint: %v", err)
	}
	revokedRec := mcpPost(t, routes, revokedEP.Slug, revokedTok)
	if revokedRec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential: got %d, want 401", revokedRec.Code)
	}
	if expiredRec.Body.String() != revokedRec.Body.String() {
		t.Errorf("expired (%q) and revoked (%q) 401 bodies differ; expiry must fail identically to revocation",
			expiredRec.Body.String(), revokedRec.Body.String())
	}
}
