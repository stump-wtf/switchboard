// Operator-API endpoint revocation (POST /api/v1/endpoints/{ref}/revoke).
//
// Revocation used to be reachable only from the web UI, which made the one case that matters most
// — rotating a credential that has leaked — impossible from a headless box or from the agent that
// leaked it. These tests pin the contract the CLI's `switchboard endpoint revoke` depends on:
// address by slug or id, refuse anything that is not yours, and never report a kill that did not
// happen. Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: SPEC-0007 REQ "Revoke = Kill
// the Endpoint"; ADR-0008; ADR-0023.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
)

// mintOperatorBearer issues a live operator OAuth access token bound to humanID — the same shape
// `switchboard login` ends up holding, so these tests exercise the real guard rather than a
// test-only bypass.
func mintOperatorBearer(t *testing.T, st *store.Store, ctx context.Context, humanID, clientID string) string {
	t.Helper()
	access, accessHash, err := oauthsrv.MintToken()
	if err != nil {
		t.Fatalf("mint access token: %v", err)
	}
	_, refreshHash, err := oauthsrv.MintToken()
	if err != nil {
		t.Fatalf("mint refresh token: %v", err)
	}
	if _, err := st.CreateOAuthClient(ctx, clientID, "operator cli", []string{"http://127.0.0.1/callback"}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	if _, err := st.CreateOAuthToken(ctx, accessHash, refreshHash, clientID, "", humanID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create operator oauth token: %v", err)
	}
	return access
}

// revokeVia POSTs the revoke route as the given operator bearer and returns status + decoded body.
func revokeVia(t *testing.T, r http.Handler, bearer, ref string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/"+ref+"/revoke", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc
}

func TestAPIRevokeEndpointBySlugAndID(t *testing.T) {
	for _, addressBy := range []string{"slug", "id"} {
		t.Run("by "+addressBy, func(t *testing.T) {
			r, st, ctx := newDBRouter(t)
			human, _ := mintSession(t, st, ctx, "test|revoke-"+addressBy, "Joe Stump", "joe@example.com")
			bearer := mintOperatorBearer(t, st, ctx, human.ID, "cid-revoke-"+addressBy)
			ep := consentFixture(t, st, ctx, human.ID, "doomed-"+addressBy, "doomed-"+addressBy+"-ab12cd34", "cid-fixture-"+addressBy)

			ref := ep.Slug
			if addressBy == "id" {
				ref = ep.ID
			}
			code, doc := revokeVia(t, r, bearer, ref)
			if code != http.StatusOK {
				t.Fatalf("revoke: got %d, want 200 (body %v)", code, doc)
			}
			if doc["state"] != "revoked" || doc["slug"] != ep.Slug {
				t.Fatalf("revoke response = %v, want the endpoint reported revoked", doc)
			}

			// The state actually changed in the store, not just in the response.
			cards, err := st.ListEndpointCards(ctx, human.ID)
			if err != nil {
				t.Fatalf("list cards: %v", err)
			}
			for _, c := range cards {
				if c.ID == ep.ID && c.State != "revoked" {
					t.Fatalf("endpoint state = %q, want revoked", c.State)
				}
			}
		})
	}
}

// Re-revoking is a conflict, not a success. A rotation script must be able to tell "I killed it"
// from "it was already dead" — those mean different things when a credential has leaked.
func TestAPIRevokeAlreadyRevokedIsConflict(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, _ := mintSession(t, st, ctx, "test|revoke-twice", "Joe Stump", "joe@example.com")
	bearer := mintOperatorBearer(t, st, ctx, human.ID, "cid-revoke-twice")
	ep := consentFixture(t, st, ctx, human.ID, "twice", "twice-ab12cd34", "cid-fixture-twice")

	if code, doc := revokeVia(t, r, bearer, ep.Slug); code != http.StatusOK {
		t.Fatalf("first revoke: got %d (body %v)", code, doc)
	}
	code, doc := revokeVia(t, r, bearer, ep.Slug)
	if code != http.StatusConflict {
		t.Fatalf("second revoke: got %d, want 409 (body %v)", code, doc)
	}
	if doc["state"] != "revoked" {
		t.Fatalf("conflict body = %v, want it to name the current state", doc)
	}
}

// Another operator's endpoint and a nonexistent one are the same 404: the caller learns nothing
// about endpoints that are not theirs, and cannot revoke across the tenant boundary.
func TestAPIRevokeRefusesForeignAndUnknownEndpoints(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|revoke-owner", "Owner", "owner@example.com")
	other, _ := mintSession(t, st, ctx, "test|revoke-other", "Other", "other@example.com")
	victim := consentFixture(t, st, ctx, owner.ID, "victim", "victim-ab12cd34", "cid-victim")
	attacker := mintOperatorBearer(t, st, ctx, other.ID, "cid-attacker")

	for _, ref := range []string{victim.Slug, victim.ID, "no-such-endpoint"} {
		code, _ := revokeVia(t, r, attacker, ref)
		if code != http.StatusNotFound {
			t.Errorf("revoke %q as a non-owner: got %d, want 404", ref, code)
		}
	}

	// And it really is still alive for its owner.
	cards, err := st.ListEndpointCards(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list cards: %v", err)
	}
	for _, c := range cards {
		if c.ID == victim.ID && c.State != "active" {
			t.Fatalf("victim endpoint state = %q, want it untouched", c.State)
		}
	}
}

// The route is behind the same operator-OAuth guard as the rest of /api/v1 — an anonymous or
// endpoint-scoped bearer cannot revoke.
func TestAPIRevokeRequiresOperatorBearer(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, _ := mintSession(t, st, ctx, "test|revoke-anon", "Joe Stump", "joe@example.com")
	ep := consentFixture(t, st, ctx, human.ID, "guarded", "guarded-ab12cd34", "cid-guarded")

	for _, bearer := range []string{"", "sbk_" + "x", "not-a-real-token"} {
		if code, _ := revokeVia(t, r, bearer, ep.Slug); code != http.StatusUnauthorized {
			t.Errorf("bearer %q: got %d, want 401", bearer, code)
		}
	}
}
