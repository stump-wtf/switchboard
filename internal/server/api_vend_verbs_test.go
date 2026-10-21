// The verb grant on the human API's one-call vend: what a vend that names nothing hands out, how a
// spec of verb names and presets is resolved, and that an unknown name is refused before the
// deployment mints an agent, an endpoint, a credential and a webhook. Issue #589.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0047, SPEC-0006 REQ "Todo Drain Verbs",
// SPEC-0035 REQ "Reach on Every Route".
package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/stump-wtf/switchboard/internal/mcp"
)

type vendVerbsRow struct {
	Slug    string   `json:"slug"`
	Verbs   []string `json:"verbs"`
	Webhook struct {
		IngestURL string `json:"ingest_url"`
	} `json:"webhook"`
	Code  string `json:"code"`
	Error string `json:"error"`
}

func decodeVendVerbs(t *testing.T, body []byte) vendVerbsRow {
	t.Helper()
	var row vendVerbsRow
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return row
}

// A scripted vend that names no verbs is a worker lane: it drains its queue, and it holds nothing
// that can rotate a webhook, rewrite routing or replay an event. The ingestion webhook the vend mints
// on the operator's behalf still arrives, because feeding the queue is the point of the call.
func TestAPIVendDefaultsToTheWorkerSurface(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|verbs-default", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-verbs-default")

	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, `{"name":"lane-worker","queue":"lane-m"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("vend: %d %s", rec.Code, rec.Body.String())
	}
	row := decodeVendVerbs(t, rec.Body.Bytes())
	if !slices.Equal(row.Verbs, mcp.DefaultVendVerbs()) {
		t.Errorf("default grant = %q, want %q", row.Verbs, mcp.DefaultVendVerbs())
	}
	if mcp.GrantHasWebhookAdmin(row.Verbs) {
		t.Errorf("the default grant %q holds webhook-admin verbs", row.Verbs)
	}
	if row.Webhook.IngestURL == "" {
		t.Errorf("a narrow vend minted no ingestion webhook: %s", rec.Body.String())
	}

	list := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", bearer, "")
	var rows []vendVerbsRow
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %s", err, list.Body.String())
	}
	if !slices.Equal(rows[0].Verbs, mcp.DefaultVendVerbs()) {
		t.Errorf("the listing echoes %q, want the grant it was vended with", rows[0].Verbs)
	}
}

func TestAPIVendResolvesVerbNamesAndPresets(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|verbs-spec", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-verbs-spec")

	for name, body := range map[string]string{
		"all preset":            `{"name":"router","verbs":["all"]}`,
		"admin preset":          `{"name":"webhook-manager","verbs":["webhook-admin"]}`,
		"todo preset":           `{"name":"drainer","verbs":["todo"]}`,
		"names beside a preset": `{"name":"mixed","verbs":["add_webhook_route","todo"]}`,
		"notify verb by name":   `{"name":"relayer","verbs":["todo","create_notify_hook"]}`,
	} {
		rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		granted := decodeVendVerbs(t, rec.Body.Bytes()).Verbs
		spec := specOf(t, body)
		want, err := mcp.ResolveVerbSpec(spec)
		if err != nil {
			t.Fatalf("%s: resolve %q: %v", name, spec, err)
		}
		if !slices.Equal(granted, want) {
			t.Errorf("%s: grant = %q, want %q", name, granted, want)
		}
		if !slices.IsSortedFunc(granted, func(a, b string) int {
			return slices.Index(mcp.VerbOrder(), a) - slices.Index(mcp.VerbOrder(), b)
		}) {
			t.Errorf("%s: grant %q is not in canonical order", name, granted)
		}
	}
}

// A verb name the server does not know is refused in SPEC-0035's error shape, and the refusal mints
// nothing: a typo must never leave a quieter credential behind than the operator asked for.
func TestAPIVendRefusesUnknownVerbAndMintsNothing(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|verbs-refuse", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-verbs-refuse")

	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer, `{"name":"typo-lane","verbs":["claim_nextt"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown verb: %d %s, want 400", rec.Code, rec.Body.String())
	}
	row := decodeVendVerbs(t, rec.Body.Bytes())
	if row.Code != "invalid_argument" || row.Error == "" {
		t.Fatalf("error shape = %+v, want invalid_argument with a message", row)
	}
	list := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", bearer, "")
	var rows []vendVerbsRow
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
		t.Fatalf("list: %v %s", err, list.Body.String())
	}
	if len(rows) != 0 {
		t.Fatalf("a refused vend minted an endpoint: %s", list.Body.String())
	}
	cards, err := st.ListEndpointCards(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("a refused vend left %d endpoint(s) behind", len(cards))
	}
	agents, err := st.ListAgents(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("a refused vend registered an agent (%d)", len(agents))
	}
}

// specOf pulls the verbs array back out of a request body so the test compares the grant against the
// same resolution the server ran, rather than against a verb list copied into the test.
func specOf(t *testing.T, body string) []string {
	t.Helper()
	var in struct {
		Verbs []string `json:"verbs"`
	}
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return in.Verbs
}
