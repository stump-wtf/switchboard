package server

// The ADR-0023 basics end-to-end: one machine-API call vends the whole happy path, and a webhook
// delivery through that vended URL lands as a durable todo AND rings the doorbell on the agent's
// live MCP session. This is the test that keeps the basics un-rottable — it exercises the real
// router, the real ingest receiver, the real store, and a real MCP client session over Streamable
// HTTP. DB-backed; skips cleanly without SWITCHBOARD_TEST_DATABASE_URL (Gitea CI is the gate).
//
// Governing: ADR-0023 REQ "Registration Vends the Whole Happy Path", REQ "End-to-End Proof in CI".

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/ingest"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
	"github.com/stump-wtf/switchboard/internal/web"
)

// mvpDoorbell is the decoded notifications/claude/channel frame the raw SSE client captures.
type mvpDoorbell struct {
	Method string `json:"method"`
	Params struct {
		Meta map[string]string `json:"meta"`
	} `json:"params"`
}

// TestMVPRegistrationToDoorbell is the basics, end to end:
//
//	POST /api/v1/endpoints  → agent + endpoint + queue + webhook vended in one call
//	MCP initialize          → the vended credential opens a live session
//	POST ingest_url         → 202, durable todo on the scoped queue
//	doorbell                → notifications/claude/channel on the live stream
func TestMVPRegistrationToDoorbell(t *testing.T) {
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run the MVP end-to-end test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Package-dedicated database, same provisioning pattern as the other DB-backed suites.
	const serverTestDB = "switchboard_test_server"
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (admin): %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+serverTestDB); err != nil &&
		!strings.Contains(err.Error(), "42P04") {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	u.Path = "/" + serverTestDB
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, todos, events, sessions, adapters, endpoint_webhooks, oauth_clients, oauth_codes, oauth_tokens RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	st := store.New(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Reserve the listen address first so cfg.BaseURL is the real URL the vend handler bakes
	// into the returned mcp_url + ingest_url.
	ts := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	cfg := config.Config{BaseURL: ts.URL}

	// The operator principal and its OAuth bearer: an OPERATOR grant (human-bound, minted with
	// resource = base + "/api" in the real flow). The CLI performs that flow; here the grant's
	// token row is written directly and its plaintext access token becomes the API bearer.
	human, err := st.UpsertHuman(ctx, "mvp-e2e-operator", "Operator", "operator@mvp.test")
	if err != nil {
		t.Fatalf("upsert operator human: %v", err)
	}
	access, accessHash, err := oauthsrv.MintToken()
	if err != nil {
		t.Fatalf("mint access token: %v", err)
	}
	_, refreshHash, err := oauthsrv.MintToken()
	if err != nil {
		t.Fatalf("mint refresh token: %v", err)
	}
	if _, err := st.CreateOAuthClient(ctx, "cid-mvp-e2e", "mvp-e2e", []string{"http://127.0.0.1/callback"}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	if _, err := st.CreateOAuthToken(ctx, accessHash, refreshHash, "cid-mvp-e2e", "", human.ID,
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create operator oauth token: %v", err)
	}
	apiToken := access

	authr, err := auth.New(ctx, cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	hub := ingest.NewHub()
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	// The one Run-wired hook the basics depend on: committed todo → live doorbell.
	st.SetTodoDoorbellHook(mcph.PublishTodoReady)

	r := newRouter(routerDeps{ //nolint: all // cfg now carries the real BaseURL
		cfg:   cfg,
		st:    st,
		authr: authr,
		webh:  webh,
		ing:   ingest.New(st, hub, log, ingest.Config{}),
		mcp:   mcph,
		oauth: oauthsrv.New(st, cfg.BaseURL, log),
		ping:  pool.Ping,
		log:   log,
	})
	// Swap the reserved server's handler for the real router (httptest reads Config.Handler per
	// request, so this is safe; no request has been served yet).
	ts.Config.Handler = r

	// --- 1. Registration vends everything ---
	vendBody := strings.NewReader(`{"name":"wake-agent","queue":"inbox"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/v1/endpoints", vendBody)
	if err != nil {
		t.Fatalf("build vend request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("vend POST: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("vend status = %d, want 201 (%s)", resp.StatusCode, body)
	}
	var vended struct {
		AgentName string `json:"agent_name"`
		Slug      string `json:"slug"`
		MCPURL    string `json:"mcp_url"`
		Token     string `json:"token"`
		Queue     string `json:"queue"`
		Webhook   struct {
			WebhookID string `json:"webhook_id"`
			IngestURL string `json:"ingest_url"`
			TrustMode string `json:"trust_mode"`
		} `json:"webhook"`
		MCPJSON json.RawMessage `json:"mcp_json"`
	}
	if err := json.Unmarshal(body, &vended); err != nil {
		t.Fatalf("decode vend response: %v", err)
	}
	if vended.Token == "" || vended.MCPURL == "" || vended.Webhook.IngestURL == "" {
		t.Fatalf("vend response missing credentials: %s", body)
	}
	// The paste-ready client wiring rides along, and it is the SAME stanza the web reveal renders
	// (one renderer in internal/mcp), so the CLI can print it verbatim.
	var wantWiring bytes.Buffer
	if err := json.Compact(&wantWiring, []byte(mcpsrv.ClientConfigJSON(ts.URL, vended.Slug, vended.Token))); err != nil {
		t.Fatalf("compact wiring: %v", err)
	}
	if strings.TrimSpace(string(vended.MCPJSON)) != wantWiring.String() {
		t.Fatalf("mcp_json = %s, want %s", vended.MCPJSON, wantWiring.String())
	}
	if vended.Queue != "inbox" || vended.Webhook.TrustMode != "token" {
		t.Fatalf("vend response wrong queue/trust: %s", body)
	}

	// --- 2. The vended credential opens a live MCP session ---
	initResp, err := mvpPost(ctx, vended.MCPURL, vended.Token, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mvp-e2e","version":"0"}}}`)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if initResp.statusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", initResp.statusCode)
	}
	sessionID := initResp.headers.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}
	if _, err := mvpPost(ctx, vended.MCPURL, vended.Token, sessionID,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		t.Fatalf("initialized notification: %v", err)
	}

	// Open the notification stream (the doorbell wire).
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, vended.MCPURL, nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamReq.Header.Set("Accept", "text/event-stream")
	streamReq.Header.Set("Authorization", "Bearer "+vended.Token)
	streamReq.Header.Set("Mcp-Session-Id", sessionID)
	stream, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { _ = stream.Body.Close() })
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", stream.StatusCode)
	}
	doorbells := make(chan mvpDoorbell, 8)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var data strings.Builder
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			case line == "" && data.Len() > 0:
				var n mvpDoorbell
				if json.Unmarshal([]byte(data.String()), &n) == nil && n.Method == "notifications/claude/channel" {
					select {
					case doorbells <- n:
					default:
					}
				}
				data.Reset()
			}
		}
	}()

	// --- 3. A webhook delivery through the vended URL becomes a durable todo ---
	delivery := strings.NewReader(`{"title":"Review the overnight MVP","body":"posted by the producer"}`)
	deliveryReq, err := http.NewRequestWithContext(ctx, http.MethodPost, vended.Webhook.IngestURL, delivery)
	if err != nil {
		t.Fatalf("build delivery request: %v", err)
	}
	deliveryReq.Header.Set("Content-Type", "application/json")
	deliveryResp, err := http.DefaultClient.Do(deliveryReq)
	if err != nil {
		t.Fatalf("delivery POST: %v", err)
	}
	_ = deliveryResp.Body.Close()
	if deliveryResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery status = %d, want 202", deliveryResp.StatusCode)
	}

	// --- 4. The doorbell rings on the live session ---
	select {
	case n := <-doorbells:
		if n.Params.Meta["queue"] != "inbox" {
			t.Fatalf("doorbell queue = %q, want inbox", n.Params.Meta["queue"])
		}
		if n.Params.Meta["todo_id"] == "" {
			t.Fatal("doorbell missing todo_id")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no doorbell arrived on the live stream within 10s")
	}

	// And the durable ledger holds the todo the doorbell announced: the agent can drain it.
	var endpointID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM endpoints WHERE slug = $1`, vended.Slug).Scan(&endpointID); err != nil {
		t.Fatalf("resolve vended endpoint: %v", err)
	}
	todos, lerr := st.ListTodos(ctx, endpointID, []string{"inbox"}, "pending", 10)
	if lerr != nil {
		t.Fatalf("list todos: %v", lerr)
	}
	if len(todos) == 0 {
		t.Fatal("delivered webhook produced no durable todo on the scoped queue")
	}
}

// mvpPost is the raw JSON-RPC POST the MCP Streamable HTTP handshake needs.
func mvpPost(ctx context.Context, url, token, sessionID, body string) (*struct {
	statusCode int
	headers    http.Header
}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return &struct {
		statusCode int
		headers    http.Header
	}{resp.StatusCode, resp.Header}, nil
}
