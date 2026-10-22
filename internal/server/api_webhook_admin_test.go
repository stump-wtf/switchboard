package server

// The webhook lifecycle routes and the scripted vend's scope fields, against the real store: an operator
// creates a webhook on an endpoint they own inside its ceiling and is handed the secret once, the second
// create past webhook_max is the same 409 the MCP verb answers, rotate reveals a fresh secret and delete
// removes the row, and a vend whose scope the wizard would refuse mints nothing. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL.
//
// Governing: SPEC-0035 REQ "Webhook Management", REQ "CLI Parity", REQ "Shared Implementation With
// MCP"; SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling"; SPEC-0007 (one-time reveal,
// immutable scope); SPEC-0016 REQ "Credential Lifetime".
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// vendScoped vends an endpoint with the given body and returns the API's response document.
func vendScoped(t *testing.T, r chi.Router, bearer, body string) map[string]any {
	t.Helper()
	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("vend %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode vend: %v", err)
	}
	return out
}

func TestAPIWebhookLifecycleOnAnOwnedEndpoint(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|wh-lifecycle", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-wh-lifecycle")

	// The router-shaped vend: three gitea-capable webhooks, delivered only to "reviews". The vend mints
	// one of its own, so two more fit under the ceiling and the third does not.
	vend := vendScoped(t, r, bearer, `{"name":"lifecycle-router","queue":"reviews","verbs":["todo","create_webhook"],
		"source_types":["gitea"],"allowed_queues":["reviews"],"webhook_max":3,"ttl":"7d"}`)
	slug, _ := vend["slug"].(string)
	if slug == "" {
		t.Fatalf("vend returned no slug: %v", vend)
	}
	if vend["expires_at"] == nil || vend["expires_at"] == "" {
		t.Errorf("a vend with ttl answered no expires_at: %v", vend)
	}

	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearer,
		`{"source_type":"gitea","target_queue":"reviews"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	secret, _ := created["signing_secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("create revealed %q, want the minted secret once", secret)
	}
	if created["secret_origin"] != "minted" || created["secret_set"] != true {
		t.Errorf("create answered %v/%v, want minted and set", created["secret_origin"], created["secret_set"])
	}
	hookID, _ := created["webhook_id"].(string)

	// The listing carries no secret: the reveal is once, on the create.
	rec = apiCall(t, r, http.MethodGet, "/api/v1/webhooks", bearer, "")
	if strings.Contains(rec.Body.String(), strings.TrimPrefix(secret, "whsec_")) {
		t.Errorf("the webhook listing repeated the signing secret: %s", rec.Body.String())
	}

	// The vend's own webhook plus these two fill the ceiling; the next is the store's refusal, in the MCP
	// verb's code.
	rec = apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearer,
		`{"source_type":"gitea","target_queue":"reviews"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create 2: %d %s", rec.Code, rec.Body.String())
	}
	rec = apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearer,
		`{"source_type":"gitea","target_queue":"reviews"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"ceiling_exceeded"`) {
		t.Fatalf("create past webhook_max: %d %s, want 409 ceiling_exceeded", rec.Code, rec.Body.String())
	}

	// A source type the ceiling never granted is named as the reason, because widening it is the fix.
	rec = apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearer,
		`{"source_type":"github","target_queue":"reviews"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"forbidden_source_type"`) {
		t.Fatalf("create outside the ceiling: %d %s, want 403 forbidden_source_type", rec.Code, rec.Body.String())
	}

	rec = apiCall(t, r, http.MethodPost, "/api/v1/webhooks/"+hookID+"/rotate", bearer, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var rotated map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	newSecret, _ := rotated["signing_secret"].(string)
	if newSecret == "" || newSecret == secret {
		t.Errorf("rotate revealed %q, want a fresh secret", newSecret)
	}

	rec = apiCall(t, r, http.MethodDelete, "/api/v1/webhooks/"+hookID, bearer, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Errorf("delete answered %s, want deleted true", rec.Body.String())
	}
	rec = apiCall(t, r, http.MethodDelete, "/api/v1/webhooks/"+hookID, bearer, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice: %d %s, want 404 the second time", rec.Code, rec.Body.String())
	}
}

func TestAPIWebhookLifecycleRespectsReach(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|wh-reach-a", "A", "a@example.com")
	stranger, _ := mintSession(t, st, ctx, "test|wh-reach-b", "B", "b@example.com")
	bearerA := mintOperatorBearer(t, st, ctx, owner.ID, "cid-wh-reach-a")
	bearerB := mintOperatorBearer(t, st, ctx, stranger.ID, "cid-wh-reach-b")

	vend := vendScoped(t, r, bearerA, `{"name":"reach-router","queue":"inbox","verbs":["todo"],
		"source_types":["generic"],"allowed_queues":["inbox"],"webhook_max":2}`)
	slug, _ := vend["slug"].(string)
	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearerA,
		`{"source_type":"generic","target_queue":"inbox"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	hookID, _ := created["webhook_id"].(string)

	// Another human's endpoint and another human's webhook are both simply not found: the API is not an
	// existence oracle.
	rec = apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearerB,
		`{"source_type":"generic","target_queue":"inbox"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("create on another human's endpoint: %d %s, want 404", rec.Code, rec.Body.String())
	}
	rec = apiCall(t, r, http.MethodPost, "/api/v1/webhooks/"+hookID+"/rotate", bearerB, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("rotate of another human's webhook: %d %s, want 404", rec.Code, rec.Body.String())
	}
	rec = apiCall(t, r, http.MethodDelete, "/api/v1/webhooks/"+hookID, bearerB, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete of another human's webhook: %d %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestAPIVendRefusesAScopeTheWizardWouldRefuse(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|vend-scope", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-vend-scope")

	for _, tcase := range []struct{ name, body, want string }{
		{"an unsupported source type", `{"name":"a","source_types":["carrier-pigeon"]}`, "not supported"},
		{"a known but unverifiable source type", `{"name":"b","source_types":["linear"]}`, "not supported"},
		{"the reserved queue", `{"name":"c","allowed_queues":["quarantine"]}`, "quarantine"},
		{"a negative webhook_max", `{"name":"d","webhook_max":-1}`, "negative"},
		{"an unreadable ttl", `{"name":"e","ttl":"1.5d"}`, "ttl"},
	} {
		rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, tcase.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", tcase.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), tcase.want) {
			t.Errorf("%s: %s, want it to say %q", tcase.name, rec.Body.String(), tcase.want)
		}
	}
}

func TestAPIVendWebhookMaxZeroVendsAnMCPOnlyEndpoint(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|vend-zero", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-vend-zero")

	vend := vendScoped(t, r, bearer, `{"name":"mcp-only","queue":"inbox","webhook_max":0}`)
	wh, _ := vend["webhook"].(map[string]any)
	if url, _ := wh["ingest_url"].(string); url != "" {
		t.Errorf("a vend with webhook_max 0 minted an ingest URL: %v", wh)
	}
	if tok, _ := vend["token"].(string); tok == "" {
		t.Error("an MCP-only vend carried no token, so there is nothing to call MCP with")
	}
	slug, _ := vend["slug"].(string)
	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints/"+slug+"/webhooks", bearer,
		`{"source_type":"generic","target_queue":"inbox"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("create on an endpoint vended with webhook_max 0: %d %s, want 409 ceiling_exceeded", rec.Code, rec.Body.String())
	}
}
