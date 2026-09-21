package server

// Quick vend's webhook ceiling, end to end through the real router, PostgreSQL, and the real MCP
// create_webhook tool (issue #293). The quick page has no ceiling step, so an endpoint it vends
// with create_webhook must still be able to create a webhook — self-managed webhooks are the only
// ingestion surface — and an endpoint vended without the verb must hold no webhook scope. Skipped
// without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0023 (MVP basics); ADR-0012, SPEC-0006
// REQ "Webhook Self-Management Within a Vended Ceiling".

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/cred"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/store"
)

// quickVendToken drives one valid quick-vend submission and returns the revealed credential.
func quickVendToken(t *testing.T, c *wizClient, name string, queues, verbs []string) string {
	t.Helper()
	csrf := scrapeCSRF(t, c.get("/endpoints/quick").Body.String())
	rec := c.do(http.MethodPost, "/endpoints/quick", url.Values{
		"csrf_token": {csrf}, "name": {name}, "queues_extra": {strings.Join(queues, ", ")}, "verbs": verbs,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("quick vend %s: got %d (body %.300s)", name, rec.Code, rec.Body.String())
	}
	token := credRe.FindString(rec.Body.String())
	if token == "" {
		t.Fatalf("quick vend %s: no credential in the reveal", name)
	}
	return token
}

// bearerRT injects the vended credential the way a configured MCP client does.
type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// mcpSessionAs serves the real MCP surface over the store and connects as the vended endpoint.
func mcpSessionAs(t *testing.T, ctx context.Context, st *store.Store, slug, token string) *sdk.ClientSession {
	t.Helper()
	h := mcpsrv.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(h.Close)
	h.SetBaseURL("https://sb.example.com")
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "quickvend-webhook-test", Version: "0.0.1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint:   ts.URL + "/mcp/" + slug,
			HTTPClient: &http.Client{Transport: bearerRT{token}},
		}, nil)
	if err != nil {
		t.Fatalf("MCP handshake as %s: %v", slug, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestQuickVendCreateWebhookSucceeds: quick vend with create_webhook → the vended endpoint's
// create_webhook for the generic source type on a vended queue SUCCEEDS over MCP. Before the fix
// the endpoint held the verb with no allowed source types and a zero ceiling, so this call answered
// forbidden_source_type (and, once patched by hand, ceiling_exceeded).
func TestQuickVendCreateWebhookSucceeds(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|quick-wh", "Joe Stump", "joe@example.com")
	c := newWizClient(t, r, session)

	queues := []string{"inbox", "reviews"}
	token := quickVendToken(t, c, "hook-bot", queues, []string{"list_todos", "claim", "create_webhook", "list_webhooks"})
	ep, err := st.EndpointByCredHash(ctx, cred.Hash(token))
	if err != nil {
		t.Fatalf("look up vended endpoint: %v", err)
	}
	if ep.WebhookMax != mcpsrv.BasicWebhookMax {
		t.Errorf("webhook_max = %d, want the basics ceiling %d", ep.WebhookMax, mcpsrv.BasicWebhookMax)
	}

	cs := mcpSessionAs(t, ctx, st, ep.Slug, token)
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name: "create_webhook", Arguments: map[string]any{"source_type": "generic", "target_queue": "reviews"}})
	if err != nil {
		t.Fatalf("create_webhook: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_webhook on a quick-vended endpoint errored: %s", toolText(res))
	}
	var out struct {
		WebhookID   string `json:"webhook_id"`
		IngestURL   string `json:"ingest_url"`
		TargetQueue string `json:"target_queue"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode create_webhook result: %v (%s)", err, raw)
	}
	if out.WebhookID == "" || out.IngestURL == "" || out.TargetQueue != "reviews" {
		t.Fatalf("create_webhook result = %+v, want a minted webhook on reviews", out)
	}
	ws, err := st.ListWebhooks(ctx, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].SourceType != "generic" {
		t.Fatalf("persisted webhooks = %+v, want one generic webhook", ws)
	}
}

// TestQuickVendWithoutCreateWebhookGrantsNoScope: a quick vend that does not grant create_webhook
// grants no webhook scope — no ceiling, no source types, no webhook queues.
func TestQuickVendWithoutCreateWebhookGrantsNoScope(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|quick-nowh", "Joe Stump", "joe@example.com")
	c := newWizClient(t, r, session)

	token := quickVendToken(t, c, "drain-bot", []string{"inbox"}, []string{"list_todos", "claim", "complete"})
	ep, err := st.EndpointByCredHash(ctx, cred.Hash(token))
	if err != nil {
		t.Fatalf("look up vended endpoint: %v", err)
	}
	if ep.WebhookMax != 0 || len(ep.WebhookSourceTypes) != 0 || len(ep.WebhookQueues) != 0 {
		t.Fatalf("webhook scope = max %d sources %q queues %q, want none without create_webhook",
			ep.WebhookMax, ep.WebhookSourceTypes, ep.WebhookQueues)
	}
}

// toolText flattens a tool result's text content for failure messages.
func toolText(res *sdk.CallToolResult) string {
	s := ""
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			s += tc.Text
		}
	}
	return s
}
