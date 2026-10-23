// The widen surface on the human API: what a preview says, what confirm applies, what the trail keeps,
// and the refusals that keep a widen additive. Issue #590.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0049 (proposed), ADR-0008,
// SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0035 REQ "Reach on Every Route".
//
// @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stump-wtf/switchboard/internal/store"
)

type widenResponse struct {
	ID                   string `json:"endpoint_id"`
	Slug                 string `json:"slug"`
	Applied              bool   `json:"applied"`
	RequiresConfirmation bool   `json:"requires_confirmation"`
	Changed              bool   `json:"changed"`
	Before               struct {
		WebhookQueues []string `json:"webhook_queues"`
		WebhookMax    int      `json:"webhook_max"`
		ScopeVerbs    []string `json:"scope_verbs"`
	} `json:"before"`
	After struct {
		WebhookQueues []string `json:"webhook_queues"`
		WebhookMax    int      `json:"webhook_max"`
		ScopeVerbs    []string `json:"scope_verbs"`
	} `json:"after"`
	Widenings []struct {
		Field     string `json:"field"`
		CreatedAt string `json:"created_at"`
	} `json:"widenings"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

type historyResponse struct {
	Widenings []struct {
		Field     string `json:"field"`
		Before    any    `json:"before"`
		After     any    `json:"after"`
		CreatedAt string `json:"created_at"`
	} `json:"widenings"`
}

// widenVend vends an endpoint through the API and returns its slug, its owner's bearer and the owner.
func widenVend(t *testing.T, r chi.Router, st *store.Store, ctx context.Context, name, queue string) (string, string, store.Human) {
	t.Helper()
	owner, _ := mintSession(t, st, ctx, name+"|subject", name, name+"@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-"+name)
	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer,
		`{"name":"`+name+`","queue":"`+queue+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("vend: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode vend: %v", err)
	}
	return out.Slug, bearer, owner
}

// A widen that has not been confirmed shows the whole change and touches nothing; the same call with
// confirm applies it and writes the trail.
func TestAPIScopeWidenPreviewThenConfirm(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	slug, bearer, _ := widenVend(t, r, st, ctx, "widen-owner", "lane-a")

	preview := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/scope/widen", bearer,
		`{"add_webhook_queues":["merge"]}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	var p widenResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if p.Applied || !p.RequiresConfirmation || !p.Changed {
		t.Fatalf("preview flags: applied=%v requires_confirmation=%v changed=%v", p.Applied, p.RequiresConfirmation, p.Changed)
	}
	if slices.Contains(p.Before.WebhookQueues, "merge") || !slices.Contains(p.After.WebhookQueues, "merge") {
		t.Errorf("preview scope = %q → %q, want merge only on the right", p.Before.WebhookQueues, p.After.WebhookQueues)
	}
	if h := historyCall(t, r, slug, bearer); len(h.Widenings) != 0 {
		t.Fatalf("a preview wrote history: %+v", h.Widenings)
	}

	applied := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/scope/widen", bearer,
		`{"add_webhook_queues":["merge"],"confirm":true}`)
	if applied.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", applied.Code, applied.Body.String())
	}
	var a widenResponse
	if err := json.Unmarshal(applied.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	if !a.Applied || a.RequiresConfirmation || len(a.Widenings) != 1 || a.Widenings[0].Field != "webhook_queues" {
		t.Fatalf("apply result: %s", applied.Body.String())
	}
	if a.Widenings[0].CreatedAt == "" {
		t.Errorf("an applied widening carries no timestamp: %s", applied.Body.String())
	}

	h := historyCall(t, r, slug, bearer)
	if len(h.Widenings) != 1 {
		t.Fatalf("history after one widen: %+v", h.Widenings)
	}
	if h.Widenings[0].Field != "webhook_queues" || h.Widenings[0].CreatedAt == "" {
		t.Errorf("history entry = %+v", h.Widenings[0])
	}

	// A second widen adds to the first: the queue granted earlier is still there, which is the whole
	// difference between widening a grant and setting it.
	second := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/scope/widen", bearer,
		`{"add_webhook_queues":["lane-b"]}`)
	var s2 widenResponse
	if err := json.Unmarshal(second.Body.Bytes(), &s2); err != nil {
		t.Fatalf("decode second preview: %v", err)
	}
	if !slices.Contains(s2.Before.WebhookQueues, "merge") || !slices.Contains(s2.After.WebhookQueues, "merge") ||
		!slices.Contains(s2.After.WebhookQueues, "lane-b") {
		t.Errorf("second widen = %q → %q, want merge kept and lane-b added", s2.Before.WebhookQueues, s2.After.WebhookQueues)
	}

	after := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", bearer, "")
	if after.Code != http.StatusOK {
		t.Fatalf("list endpoints: %d", after.Code)
	}
	var list []struct {
		Slug  string   `json:"slug"`
		Verbs []string `json:"verbs"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, e := range list {
		if e.Slug == slug && !slices.Equal(e.Verbs, a.After.ScopeVerbs) {
			t.Errorf("the endpoint's verbs %q disagree with the widen result %q", e.Verbs, a.After.ScopeVerbs)
		}
	}
}

// The widen cannot be told to take anything away: the vocabulary has no removal, the ceiling cannot go
// down, and names that do not exist are refused rather than silently ignored.
func TestAPIScopeWidenRefusals(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	slug, bearer, _ := widenVend(t, r, st, ctx, "widen-refusals", "lane-a")
	path := "/api/v1/endpoints/" + slug + "/scope/widen"

	for _, body := range []string{
		`{"remove_webhook_queues":["lane-a"]}`,
		`{"webhook_queues":["merge"]}`,
		`{"replace_verbs":["claim"]}`,
		`{}`,
		`{"add_webhook_queues":[]}`,
		`{"add_webhook_source_types":["carrier-pigeon"]}`,
		`{"add_scope_verbs":["do_everything"]}`,
		`{"webhook_max":-1}`,
	} {
		rec := apiCall(t, r, http.MethodPost, path, bearer, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}

	// Raise the ceiling once, then try to put it back down: a widen has no way to say "less".
	raise := apiCall(t, r, http.MethodPost, path, bearer, `{"webhook_max":3,"confirm":true}`)
	if raise.Code != http.StatusOK {
		t.Fatalf("raise ceiling: %d %s", raise.Code, raise.Body.String())
	}
	lower := apiCall(t, r, http.MethodPost, path, bearer, `{"webhook_max":2,"confirm":true}`)
	if lower.Code != http.StatusBadRequest {
		t.Errorf("lowering webhook_max: %d %s, want 400", lower.Code, lower.Body.String())
	}

	// None of the refusals above reached the database as a change: re-asking for the ceiling the
	// endpoint already has is a no-op, and no history was written by a refused call.
	rec := apiCall(t, r, http.MethodPost, path, bearer, `{"webhook_max":3}`)
	var p widenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if p.Changed {
		t.Errorf("re-adding the granted queue reports a change: %s", rec.Body.String())
	}
	if h := historyCall(t, r, slug, bearer); len(h.Widenings) != 1 {
		t.Errorf("history after the one ceiling raise = %d entries, want 1: %+v", len(h.Widenings), h.Widenings)
	}
}

// A widen is the operator of that endpoint's act. Another human — even one with a valid operator bearer
// — gets the same 404 as for an endpoint that does not exist, and a revoked endpoint says so.
func TestAPIScopeWidenOwnershipAndRevoked(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	slug, ownerBearer, _ := widenVend(t, r, st, ctx, "widen-victim", "lane-a")
	other, _ := mintSession(t, st, ctx, "widen-other|subject", "Other", "other@example.com")
	otherBearer := mintOperatorBearer(t, st, ctx, other.ID, "cid-widen-other")

	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/scope/widen", otherBearer,
		`{"add_webhook_queues":["merge"],"confirm":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner widen: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if h := apiCall(t, r, http.MethodGet, "/api/v1/endpoints/"+slug+"/scope/widenings", otherBearer, ""); h.Code != http.StatusNotFound {
		t.Fatalf("a non-owner read a history: %d %s, want 404", h.Code, h.Body.String())
	}

	revoked := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/revoke", ownerBearer, "")
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", revoked.Code, revoked.Body.String())
	}
	rec = apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/scope/widen", ownerBearer,
		`{"add_webhook_queues":["merge"],"confirm":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("widen of a revoked endpoint: %d %s, want 409", rec.Code, rec.Body.String())
	}
}

func historyCall(t *testing.T, r chi.Router, slug, bearer string) historyResponse {
	t.Helper()
	rec := apiCall(t, r, http.MethodGet, "/api/v1/endpoints/"+slug+"/scope/widenings", bearer, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
	var out historyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	return out
}
