package mcp

// tools/call integration tests for the SPEC-0006 webhook self-management verbs over MCP: the real
// SDK client and server run over HTTP against the fake store, so the ceiling enforcement, distinct
// error codes, scope-gated advertisement, and secret-never-returned contract are all exercised
// end-to-end. Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling",
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", ADR-0012.

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

// ceiling is the vended webhook ceiling for a test endpoint.
type ceiling struct {
	max    int
	types  []string
	queues []string
}

// webhookSession vends a credential with the given verbs AND webhook ceiling, then returns a
// connected session. Unlike session() it stamps the ceiling fields the webhook verbs enforce.
func webhookSession(t *testing.T, ctx context.Context, f *fakeStore, verbs []string, c ceiling) *sdk.ClientSession {
	t.Helper()
	token, hash, _, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	f.byHash[hash] = store.AuthEndpoint{
		ID: "ep-webhooks", AgentID: "ag-1", AgentName: "test-agent", OwnerHumanID: "h-1",
		Slug: "agent-a-11111111", ScopeQueues: c.queues, ScopeVerbs: verbs,
		WebhookMax: c.max, WebhookSourceTypes: c.types, WebhookQueues: c.queues,
	}
	// A stable base URL so ingest_url assertions are deterministic.
	ts, hdl := newTestServerHandler(t, f)
	hdl.SetBaseURL("https://switchboard.example")
	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

const allWebhookVerbs = "create_webhook list_webhooks rotate_webhook delete_webhook"

// TestWebhookVerbsScopeGatedAdvertisement: tools/list advertises exactly the webhook verbs in the
// endpoint's allowlist — no more. Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended
// Ceiling", SPEC-0014 REQ "Agent Tool Surface over MCP" (scope filters the advertised tools).
func TestWebhookVerbsScopeGatedAdvertisement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, []string{"create_webhook", "list_webhooks"},
		ceiling{max: 3, types: []string{"github", "generic"}, queues: []string{"reviews"}})

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		if webhookVerbs[tool.Name] {
			names = append(names, tool.Name)
		}
	}
	sort.Strings(names)
	if want := []string{"create_webhook", "list_webhooks"}; !slices.Equal(names, want) {
		t.Fatalf("advertised webhook tools = %v, want %v", names, want)
	}

	// rotate_webhook is a real verb, just not granted here: a call is a stable scope error, not an
	// unknown-tool protocol error. Governing: SPEC-0006 REQ "Scope Enforcement at the Boundary".
	callErr(t, ctx, cs, "rotate_webhook", map[string]any{"webhook_id": "wh-1"}, "forbidden")
}

// TestCreateWebhookRevealsSecretOnceSigned: for a signed-type webhook (github) create derives
// trust_mode=signed, mints the HMAC signing secret, HOLDS its own copy server-side, and reveals the
// SAME secret to the agent exactly once. A token-type webhook (generic) reveals no secret. Governing:
// SPEC-0006 scenario "Secret is revealed exactly once to the agent"; ADR-0012 (no agent trust downgrade).
func TestCreateWebhookRevealsSecretOnceSigned(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 3, types: []string{"github", "generic"}, queues: []string{"reviews"}})

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name: "create_webhook", Arguments: map[string]any{"source_type": "github", "target_queue": "reviews"}})
	if err != nil {
		t.Fatalf("create_webhook: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_webhook errored: %s", contentText(res))
	}
	var out webhookOut
	rawUnmarshal(t, res, &out)
	if out.TrustMode != "signed" {
		t.Fatalf("trust_mode = %q, want signed (derived from github)", out.TrustMode)
	}
	if out.IngestURL != "https://switchboard.example/webhooks/w/"+webhookToken(t, f, out.WebhookID) {
		t.Fatalf("ingest_url = %q, unexpected", out.IngestURL)
	}
	if out.SourceType != "github" || out.TargetQueue != "reviews" {
		t.Fatalf("create output = %+v, want github/reviews", out)
	}
	// The signed webhook's secret is revealed exactly once, high-entropy, and matches switchboard's
	// held copy so it can actually verify inbound deliveries.
	if !strings.HasPrefix(out.SigningSecret, "whsec_") {
		t.Fatalf("signing_secret = %q, want a revealed whsec_ secret", out.SigningSecret)
	}
	if held := webhookSecret(f, out.WebhookID); held != out.SigningSecret {
		t.Fatalf("revealed secret %q != server-held secret %q", out.SigningSecret, held)
	}

	// A token-type webhook is authenticated by the unguessable URL and reveals no secret.
	var tok webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "generic", "target_queue": "reviews"}, &tok)
	if tok.TrustMode != "token" {
		t.Fatalf("generic trust_mode = %q, want token", tok.TrustMode)
	}
	if tok.SigningSecret != "" {
		t.Fatalf("token webhook must reveal no secret, got %q", tok.SigningSecret)
	}
	if held := webhookSecret(f, tok.WebhookID); held != "" {
		t.Fatalf("token webhook must hold no secret, got %q", held)
	}
}

// TestCreateWebhookCeilingMaxCount: at the max count, create is refused with ceiling_exceeded and no
// webhook is created. Governing: SPEC-0006 scenario "Create beyond the count ceiling is refused".
func TestCreateWebhookCeilingMaxCount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 1, types: []string{"github"}, queues: []string{"reviews"}})

	var first webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews"}, &first)

	// The second create would exceed max=1: distinct ceiling_exceeded code, nothing created.
	callErr(t, ctx, cs, "create_webhook",
		map[string]any{"source_type": "github", "target_queue": "reviews"}, "ceiling_exceeded")

	if n := webhookCount(f, "ep-webhooks"); n != 1 {
		t.Fatalf("webhook count = %d, want 1 (ceiling-exceeded create must not persist)", n)
	}
}

// TestCreateWebhookForbiddenSourceType: a source type outside the ceiling is refused with the
// distinct forbidden_source_type code. Governing: SPEC-0006 scenario "Disallowed source type is
// refused".
func TestCreateWebhookForbiddenSourceType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 5, types: []string{"github", "generic"}, queues: []string{"reviews"}})

	callErr(t, ctx, cs, "create_webhook",
		map[string]any{"source_type": "stripe", "target_queue": "reviews"}, "forbidden_source_type")
	if n := webhookCount(f, "ep-webhooks"); n != 0 {
		t.Fatalf("webhook count = %d, want 0 (forbidden source must not persist)", n)
	}
}

// TestCreateWebhookForbiddenQueue: a target queue outside the webhook-queue grant is refused with the
// generic forbidden code. Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling"
// (forbidden for a target queue outside the grant).
func TestCreateWebhookForbiddenQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 5, types: []string{"github"}, queues: []string{"reviews"}})

	callErr(t, ctx, cs, "create_webhook",
		map[string]any{"source_type": "github", "target_queue": "deploys"}, "forbidden")
	if n := webhookCount(f, "ep-webhooks"); n != 0 {
		t.Fatalf("webhook count = %d, want 0 (forbidden queue must not persist)", n)
	}
}

// TestListWebhooksReturnsCeilingAndNoSecrets: list returns metadata plus the ceiling (max, allowed
// source types, allowed queues, used) and never a secret. Governing: SPEC-0006 REQ "Webhook
// Self-Management Within a Vended Ceiling".
func TestListWebhooksReturnsCeilingAndNoSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 4, types: []string{"github", "generic"}, queues: []string{"reviews", "deploys"}})

	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews"}, &webhookOut{})
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "generic", "target_queue": "deploys"}, &webhookOut{})

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_webhooks", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_webhooks failed: %v %s", err, contentText(res))
	}
	var out listWebhooksOut
	rawUnmarshal(t, res, &out)
	if out.Ceiling.Max != 4 || out.Ceiling.Used != 2 {
		t.Fatalf("ceiling = %+v, want max 4 used 2", out.Ceiling)
	}
	if !slices.Equal(out.Ceiling.AllowedSourceTypes, []string{"github", "generic"}) {
		t.Fatalf("allowed_source_types = %v", out.Ceiling.AllowedSourceTypes)
	}
	if !slices.Equal(out.Ceiling.AllowedQueues, []string{"reviews", "deploys"}) {
		t.Fatalf("allowed_queues = %v", out.Ceiling.AllowedQueues)
	}
	if len(out.Webhooks) != 2 {
		t.Fatalf("webhooks = %d, want 2", len(out.Webhooks))
	}
	// generic derives token; github derives signed — trust mode is always present and never blank.
	for _, w := range out.Webhooks {
		if w.TrustMode == "" {
			t.Fatalf("webhook row missing trust_mode: %+v", w)
		}
	}
	raw := rawJSONOf(t, res)
	for _, banned := range []string{"secret", "sbk_"} {
		if strings.Contains(strings.ToLower(raw), banned) {
			t.Fatalf("list_webhooks leaked %q: %s", banned, raw)
		}
	}
}

// TestRotateMintsNewURLRetiresOld: rotate changes the ingest URL (a new secret is minted and the old
// retired) and returns no secret. Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification,
// and Idempotency" (rotate mints a new secret and retires the old).
func TestRotateMintsNewURLRetiresOld(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 2, types: []string{"github"}, queues: []string{"reviews"}})

	var created webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews"}, &created)

	var rotated webhookOut
	callOK(t, ctx, cs, "rotate_webhook", map[string]any{"webhook_id": created.WebhookID}, &rotated)
	if rotated.WebhookID != created.WebhookID {
		t.Fatalf("rotate changed the id: %q -> %q", created.WebhookID, rotated.WebhookID)
	}
	if rotated.IngestURL == created.IngestURL {
		t.Fatalf("rotate did not mint a new URL: still %q", rotated.IngestURL)
	}

	// Rotating an unknown (or another endpoint's) webhook is not_found.
	callErr(t, ctx, cs, "rotate_webhook", map[string]any{"webhook_id": "wh-999"}, "not_found")
}

// TestDeleteWebhookTearsDown: delete removes the webhook; deleting an unknown id is not_found.
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling" (delete tears down).
func TestDeleteWebhookTearsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 2, types: []string{"github"}, queues: []string{"reviews"}})

	var created webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "github", "target_queue": "reviews"}, &created)

	var del struct {
		Deleted bool `json:"deleted"`
	}
	callOK(t, ctx, cs, "delete_webhook", map[string]any{"webhook_id": created.WebhookID}, &del)
	if !del.Deleted {
		t.Fatal("delete_webhook did not report deleted")
	}
	if n := webhookCount(f, "ep-webhooks"); n != 0 {
		t.Fatalf("webhook count = %d, want 0 after delete", n)
	}
	callErr(t, ctx, cs, "delete_webhook", map[string]any{"webhook_id": created.WebhookID}, "not_found")
}

// --- test helpers ---

func rawUnmarshal(t *testing.T, res *sdk.CallToolResult, out any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
}

func rawJSONOf(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	return string(b)
}

func webhookCount(f *fakeStore, endpointID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.webhooks {
		if w.EndpointID == endpointID {
			n++
		}
	}
	return n
}

func webhookToken(t *testing.T, f *fakeStore, id string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.webhooks[id]
	if !ok {
		t.Fatalf("no webhook %q in fake store", id)
	}
	return w.IngestToken
}

// webhookSecret returns the signing secret switchboard holds server-side for a webhook (empty for a
// token webhook). Mirrors the store's signing_secret column, read only in tests to prove the revealed
// secret equals the held one.
func webhookSecret(f *fakeStore, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.webhookSecrets[id]
}

// TestCreateWebhookGiteaSourceType: a gitea source type is accepted when in the ceiling and
// derives trust_mode=signed with a revealed signing secret, identical to github.
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling".
func TestCreateWebhookGiteaSourceType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 5, types: []string{"gitea"}, queues: []string{"reviews"}})

	var out webhookOut
	callOK(t, ctx, cs, "create_webhook",
		map[string]any{"source_type": "gitea", "target_queue": "reviews"}, &out)
	if out.TrustMode != "signed" {
		t.Fatalf("trust_mode = %q, want signed (derived from gitea)", out.TrustMode)
	}
	if out.SourceType != "gitea" || out.TargetQueue != "reviews" {
		t.Fatalf("create output = %+v, want gitea/reviews", out)
	}
	if !strings.HasPrefix(out.SigningSecret, "whsec_") {
		t.Fatalf("signing_secret = %q, want a revealed whsec_ secret", out.SigningSecret)
	}
	if held := webhookSecret(f, out.WebhookID); held != out.SigningSecret {
		t.Fatalf("revealed secret %q != server-held secret %q", out.SigningSecret, held)
	}
}

// TestWebhookSourceTypesAreMetricSources: every source type create_webhook accepts is one the todo
// created counter labels by name. The store keeps its own allowlist (it cannot import this
// package), so a source type added to webhookTrustModes alone would count every todo it mints as
// source="__other__"; this fails first.
// Governing: SPEC-0023 REQ-5 "Cardinality", ADR-0028.
func TestWebhookSourceTypesAreMetricSources(t *testing.T) {
	for sourceType := range webhookTrustModes {
		if !store.IsMetricSource(sourceType) {
			t.Errorf("webhook source type %q is missing from store's todoMetricSources: its todos would count as source=\"__other__\"", sourceType)
		}
	}
}

// TestWebhookSourceTypesMatchesTrustModes: the exported list the vend wizard offers as source-type
// chips is exactly the set create_webhook accepts, sorted — no type missing (unpickable in the
// wizard), none extra (a chip the server refuses as unsupported).
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0015 REQ
// "Endpoints View And Vend Wizard".
func TestWebhookSourceTypesMatchesTrustModes(t *testing.T) {
	got := WebhookSourceTypes()
	if len(got) != len(webhookTrustModes) {
		t.Fatalf("WebhookSourceTypes() = %v, want the %d keys of webhookTrustModes", got, len(webhookTrustModes))
	}
	for sourceType := range webhookTrustModes {
		if !slices.Contains(got, sourceType) {
			t.Errorf("WebhookSourceTypes() = %v, missing accepted source type %q", got, sourceType)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("WebhookSourceTypes() = %v, want sorted", got)
	}
}

// TestCreateWebhookGiteaForbiddenWhenNotInCeiling: gitea outside the ceiling is refused with
// forbidden_source_type, same as any other unsupported type.
func TestCreateWebhookGiteaForbiddenWhenNotInCeiling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := webhookSession(t, ctx, f, strings.Fields(allWebhookVerbs),
		ceiling{max: 5, types: []string{"github"}, queues: []string{"reviews"}})

	callErr(t, ctx, cs, "create_webhook",
		map[string]any{"source_type": "gitea", "target_queue": "reviews"}, "forbidden_source_type")
	if n := webhookCount(f, "ep-webhooks"); n != 0 {
		t.Fatalf("webhook count = %d, want 0 (forbidden source must not persist)", n)
	}
}
