package server

// The endpoint default lease on the human API: vend takes and returns it, the listing returns it with
// the effective value, PATCH /api/v1/endpoints/{ref} edits it within the bounds (60 and 86400 kept,
// 59 and 86401 refused in SPEC-0035's error shape, null resets, absent and unknown fields refused),
// another human's endpoint is the same 404 as an unknown one, and a revoked endpoint is 409.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL.
//
// Governing: ADR-0043, SPEC-0007 REQ "Endpoint Default Lease", SPEC-0035 REQ "Reach on Every Route".

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/lease"
)

type leaseRow struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	State     string `json:"state"`
	Default   *int   `json:"default_lease_ttl_seconds"`
	Effective int    `json:"effective_lease_ttl_seconds"`
	Code      string `json:"code"`
	Error     string `json:"error"`
}

func decodeLeaseRow(t *testing.T, body []byte) leaseRow {
	t.Helper()
	var row leaseRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return row
}

func TestAPIVendTakesAndReturnsDefaultLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|lease-vend", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-lease-vend")

	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, `{"name":"slow-reviewer","default_lease_ttl_seconds":3600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("vend: %d %s", rec.Code, rec.Body.String())
	}
	if row := decodeLeaseRow(t, rec.Body.Bytes()); row.Default == nil || *row.Default != 3600 || row.Effective != 3600 {
		t.Fatalf("vend answered default %v effective %d, want 3600/3600", row.Default, row.Effective)
	}
	rec = apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, `{"name":"quick-worker"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("vend plain: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"default_lease_ttl_seconds":null`) {
		t.Fatalf("a plain vend must say null for the default, not omit it: %s", rec.Body.String())
	}
	if row := decodeLeaseRow(t, rec.Body.Bytes()); row.Effective != lease.DefaultSeconds {
		t.Fatalf("plain vend effective = %d, want %d", row.Effective, lease.DefaultSeconds)
	}

	list := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", bearer, "")
	var rows []leaseRow
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("list: %v %s", err, list.Body.String())
	}
	byDefault := map[string]leaseRow{}
	for _, row := range rows {
		byDefault[strconv.Itoa(row.Effective)] = row
	}
	if row, ok := byDefault["3600"]; !ok || row.Default == nil || *row.Default != 3600 {
		t.Fatalf("list lacks the configured endpoint: %s", list.Body.String())
	}
	if row, ok := byDefault[strconv.Itoa(lease.DefaultSeconds)]; !ok || row.Default != nil {
		t.Fatalf("list lacks the unconfigured endpoint with a null default: %s", list.Body.String())
	}
}

// TestAPIVendRefusesOutOfRangeDefaultLease: 59 and 86401 are 400 invalid_argument and mint nothing.
func TestAPIVendRefusesOutOfRangeDefaultLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|lease-vend-bad", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-lease-vend-bad")
	for _, bad := range []int{lease.MinDefaultSeconds - 1, lease.MaxSeconds + 1} {
		rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer,
			`{"name":"bad","default_lease_ttl_seconds":`+strconv.Itoa(bad)+`}`)
		if rec.Code != http.StatusBadRequest || decodeLeaseRow(t, rec.Body.Bytes()).Code != "invalid_argument" {
			t.Fatalf("vend with %d: %d %s, want 400 invalid_argument", bad, rec.Code, rec.Body.String())
		}
	}
	if cards, err := st.ListEndpointCards(ctx, owner.ID); err != nil || len(cards) != 0 {
		t.Fatalf("a refused vend minted %d endpoint(s) (%v)", len(cards), err)
	}
	if agents, err := st.ListAgents(ctx, owner.ID); err != nil || len(agents) != 0 {
		t.Fatalf("a refused vend minted %d agent(s) (%v)", len(agents), err)
	}
}

func TestAPIPatchEndpointDefaultLease(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|lease-patch", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-lease-patch")
	ep := consentFixture(t, st, ctx, owner.ID, "patched", "patched-ab12cd34", "cid-lease-patch-fx")

	patch := func(ref, body string) (int, leaseRow) {
		rec := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ref, bearer, body)
		return rec.Code, decodeLeaseRow(t, rec.Body.Bytes())
	}
	stored := func() *int {
		d, err := st.EndpointDefaultLease(ctx, ep.ID)
		if err != nil {
			t.Fatalf("read default: %v", err)
		}
		return d
	}

	// The bounds are inclusive, by slug and by id.
	for i, ok := range []int{lease.MinDefaultSeconds, lease.MaxSeconds} {
		ref := ep.Slug
		if i == 1 {
			ref = ep.ID
		}
		code, row := patch(ref, `{"default_lease_ttl_seconds":`+strconv.Itoa(ok)+`}`)
		if code != http.StatusOK || row.Default == nil || *row.Default != ok || row.Effective != ok || row.Slug != ep.Slug {
			t.Fatalf("PATCH %d via %s: %d %+v", ok, ref, code, row)
		}
	}
	// Out of range: 400 invalid_argument, stored value unchanged.
	for _, bad := range []string{strconv.Itoa(lease.MinDefaultSeconds - 1), strconv.Itoa(lease.MaxSeconds + 1), `"1h"`, `3600.5`} {
		code, row := patch(ep.Slug, `{"default_lease_ttl_seconds":`+bad+`}`)
		if code != http.StatusBadRequest || row.Code != "invalid_argument" {
			t.Fatalf("PATCH %s: %d %+v, want 400 invalid_argument", bad, code, row)
		}
		if d := stored(); d == nil || *d != lease.MaxSeconds {
			t.Fatalf("a refused PATCH %s changed the default to %v", bad, d)
		}
	}
	// An absent key and a scope field are refused, not read as a reset or silently ignored.
	for _, body := range []string{`{}`, `{"queues":["other"]}`, `{"default_lease_ttl_seconds":600,"verbs":["claim"]}`, `not json`} {
		if code, row := patch(ep.Slug, body); code != http.StatusBadRequest || row.Code != "invalid_argument" {
			t.Fatalf("PATCH %s: %d %+v, want 400 invalid_argument", body, code, row)
		}
	}
	if d := stored(); d == nil || *d != lease.MaxSeconds {
		t.Fatalf("a refused PATCH changed the default to %v", d)
	}
	// null resets to the server default.
	code, row := patch(ep.Slug, `{"default_lease_ttl_seconds":null}`)
	if code != http.StatusOK || row.Default != nil || row.Effective != lease.DefaultSeconds {
		t.Fatalf("PATCH null: %d %+v", code, row)
	}
	if !strings.Contains(apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ep.Slug, bearer, `{"default_lease_ttl_seconds":null}`).Body.String(),
		`"default_lease_ttl_seconds":null`) {
		t.Fatal("a reset endpoint must report null, not omit the field")
	}
	if d := stored(); d != nil {
		t.Fatalf("after reset the stored default is %d", *d)
	}
}

// TestAPIPatchEndpointReachAndRevoked: another human's endpoint answers exactly what an unknown one
// does and changes nothing; the owner's revoked endpoint is 409 conflict with its state.
func TestAPIPatchEndpointReachAndRevoked(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	alice, _ := mintSession(t, st, ctx, "test|lease-alice", "Alice", "a@example.com")
	bob, _ := mintSession(t, st, ctx, "test|lease-bob", "Bob", "b@example.com")
	aliceBearer := mintOperatorBearer(t, st, ctx, alice.ID, "cid-lease-alice")
	bobBearer := mintOperatorBearer(t, st, ctx, bob.ID, "cid-lease-bob")
	ep := consentFixture(t, st, ctx, alice.ID, "alices", "alices-ab12cd34", "cid-lease-alice-fx")

	foreign := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ep.ID, bobBearer, `{"default_lease_ttl_seconds":3600}`)
	unknown := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/00000000-0000-0000-0000-000000000000", bobBearer, `{"default_lease_ttl_seconds":3600}`)
	if foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound || foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("foreign = %d %s, unknown = %d %s; want identical 404s", foreign.Code, foreign.Body.String(), unknown.Code, unknown.Body.String())
	}
	if row := decodeLeaseRow(t, foreign.Body.Bytes()); row.Code != "not_found" {
		t.Fatalf("404 body %s lacks code not_found", foreign.Body.String())
	}
	if d, _ := st.EndpointDefaultLease(ctx, ep.ID); d != nil {
		t.Fatalf("bob's PATCH changed alice's default to %d", *d)
	}

	if err := st.RevokeEndpoint(ctx, ep.ID, alice.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ep.Slug, aliceBearer, `{"default_lease_ttl_seconds":3600}`)
	row := decodeLeaseRow(t, rec.Body.Bytes())
	if rec.Code != http.StatusConflict || row.Code != "conflict" || row.State != "revoked" {
		t.Fatalf("PATCH a revoked endpoint: %d %s, want 409 conflict with state revoked", rec.Code, rec.Body.String())
	}
	// No bearer, and an endpoint credential: the human guard refuses both.
	if rec := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ep.Slug, "", `{"default_lease_ttl_seconds":3600}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous PATCH: %d, want 401", rec.Code)
	}
	if rec := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ep.Slug, "sbk_notahuman", `{"default_lease_ttl_seconds":3600}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("endpoint-credential PATCH: %d, want 401", rec.Code)
	}
}

// TestAPIPatchEndpointIsRateLimitedPerHuman: the route draws from the per-human write bucket and
// answers 429 with Retry-After in the API's error shape once it is spent.
func TestAPIPatchEndpointIsRateLimitedPerHuman(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|lease-rl", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-lease-rl")
	ep := consentFixture(t, st, ctx, owner.ID, "limited", "limited-ab12cd34", "cid-lease-rl-fx")
	for i := 0; i < 40; i++ {
		rec := apiCall(t, r, http.MethodPatch, "/api/v1/endpoints/"+ep.Slug, bearer, `{"default_lease_ttl_seconds":600}`)
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") == "" || decodeLeaseRow(t, rec.Body.Bytes()).Code != "rate_limited" {
				t.Fatalf("429 without Retry-After or the rate_limited code: %v %s", rec.Header(), rec.Body.String())
			}
			return
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	t.Fatal("40 PATCHes in a burst were never rate limited")
}
