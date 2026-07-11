package mcp

// Integration tests for the SPEC-0005 event-history tool surface (events.go): scope-gated
// advertisement of the four-tool contract, EventSummary-vs-EventDetail shapes, and the stable
// error codes — driven through the real SDK client and server over HTTP against the fake store.
//
// Governing: SPEC-0005 REQ "Tool Surface and Naming", REQ "Stable Error Shape".

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
)

// eventVerbNames is the full SPEC-0005 tool surface, used to vend all-granted test endpoints.
var eventVerbNames = []string{
	"list_webhook_events", "get_webhook_event", "replay_webhook_event", "list_providers",
}

// --- fakeStore event-history methods (the struct lives in mcp_test.go) ---

func (f *fakeStore) ListEventHistory(_ context.Context, flt store.EventHistoryFilter) ([]store.EventHistoryItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return nil, f.failErr
	}
	limit := flt.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	// olderThan reports whether (ta, ia) sorts strictly after (tb, ib) under received_at DESC, id
	// DESC — i.e. a is "older" and belongs on a later page than the cursor tuple (tb, ib).
	olderThan := func(ta time.Time, ia int64, tb time.Time, ib int64) bool {
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return ia < ib
	}
	var out []store.EventHistoryItem
	for _, e := range f.events {
		it := e.EventHistoryItem
		if flt.Provider != "" && it.Provider != flt.Provider {
			continue
		}
		if flt.EventType != "" && it.EventType != flt.EventType {
			continue
		}
		if !flt.SinceTime.IsZero() && it.ReceivedAt.Before(flt.SinceTime) {
			continue
		}
		if flt.SinceID > 0 && it.ID < flt.SinceID {
			continue
		}
		if !flt.CursorTime.IsZero() && !olderThan(it.ReceivedAt, it.ID, flt.CursorTime, flt.CursorID) {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ReceivedAt.After(out[j].ReceivedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) EventHistoryByID(_ context.Context, id int64) (store.EventHistoryDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return store.EventHistoryDetail{}, f.failErr
	}
	e, ok := f.events[id]
	if !ok {
		return store.EventHistoryDetail{}, store.ErrNotFound
	}
	return e, nil
}

// putEvent seeds an event row.
func (f *fakeStore) putEvent(e store.EventHistoryDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now()
	}
	f.events[e.ID] = e
}

// rawStructured returns a tool's structured output as a raw map, for asserting which keys are
// (and are not) present on the wire.
func rawStructured(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("tools/call %s returned tool error: %s", tool, contentText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	return out
}

// TestEventToolsAdvertisedByScope: tools/list advertises exactly the endpoint's allowlisted
// SPEC-0005 tools — the full four-tool contract when all are granted, a subset otherwise — and a
// known event tool outside the allowlist is a stable scope error, not an unknown tool.
// Governing: SPEC-0005 scenario "Tool discovery lists the contract set", SPEC-0014 scenario
// "Scope filters the advertised tools".
func TestEventToolsAdvertisedByScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		// Every tool in the contract declares an input schema and a structured output schema.
		if tool.InputSchema == nil {
			t.Fatalf("tool %s advertises no input schema", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Fatalf("tool %s advertises no output schema", tool.Name)
		}
	}
	sort.Strings(names)
	want := []string{"get_webhook_event", "list_providers", "list_webhook_events", "replay_webhook_event"}
	if !slices.Equal(names, want) {
		t.Fatalf("advertised tools = %v, want %v", names, want)
	}

	// A narrower grant advertises the subset, and calling an ungranted (but known) event tool is
	// the stable scope error.
	f2 := newFakeStore()
	cs2 := session(t, ctx, f2, []string{"reviews"}, []string{"list_webhook_events"})
	tools2, err := cs2.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools2.Tools) != 1 || tools2.Tools[0].Name != "list_webhook_events" {
		t.Fatalf("advertised tools = %+v, want exactly list_webhook_events", tools2.Tools)
	}
	callErr(t, ctx, cs2, "get_webhook_event", map[string]any{"id": 1}, "forbidden")
}

// TestListWebhookEventsSummaries: list_webhook_events returns compact newest-first summaries that
// always carry trust metadata and never the raw payload or headers.
// Governing: SPEC-0005 scenarios "Summary omits heavy fields" and "Trust metadata always present".
func TestListWebhookEventsSummaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now()
	f := newFakeStore()
	f.putEvent(store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{ID: 1, Provider: "github", EventType: "push",
			TrustMode: "signed", Verified: true, PayloadSize: 11, ReceivedAt: now.Add(-time.Minute)},
		Headers: []byte(`{"X-GitHub-Event":"push"}`), Payload: []byte(`{"zen":"go"}`),
	})
	f.putEvent(store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{ID: 2, Provider: "dockerhub", EventType: "push",
			TrustMode: "token", Verified: false, PayloadSize: 2, ReceivedAt: now},
		Payload: []byte(`{}`),
	})
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	out := rawStructured(t, ctx, cs, "list_webhook_events", map[string]any{})
	events, ok := out["events"].([]any)
	if !ok || len(events) != 2 {
		t.Fatalf("events = %v, want 2 rows", out["events"])
	}
	first := events[0].(map[string]any)
	if first["id"] != float64(2) {
		t.Fatalf("first event id = %v, want 2 (newest first)", first["id"])
	}
	// Trust metadata is always present, and a token-trust event reports verified:false.
	if first["trust_mode"] != "token" || first["verified"] != false {
		t.Fatalf("token event trust = %v/%v, want token/false", first["trust_mode"], first["verified"])
	}
	second := events[1].(map[string]any)
	if second["trust_mode"] != "signed" || second["verified"] != true {
		t.Fatalf("signed event trust = %v/%v, want signed/true", second["trust_mode"], second["verified"])
	}
	// Summaries omit the heavy fields entirely.
	for _, row := range []map[string]any{first, second} {
		if _, has := row["payload"]; has {
			t.Fatalf("summary row leaked payload: %v", row)
		}
		if _, has := row["headers"]; has {
			t.Fatalf("summary row leaked headers: %v", row)
		}
	}

	// Provider filter narrows the scan.
	out = rawStructured(t, ctx, cs, "list_webhook_events", map[string]any{"provider": "github"})
	if events := out["events"].([]any); len(events) != 1 {
		t.Fatalf("provider-filtered events = %v, want 1 row", out["events"])
	}

	// An out-of-range limit is the stable invalid_argument, never an unbounded response
	// (SPEC-0005 scenario "Limit is clamped").
	callErr(t, ctx, cs, "list_webhook_events", map[string]any{"limit": 500}, "invalid_argument")
}

// TestGetWebhookEventDetail: get_webhook_event returns the full sanitized record — summary fields
// plus verify_detail, external_id, content_type, source_ip, sanitized headers, and the raw
// payload — and unknown ids raise the stable not_found code.
// Governing: SPEC-0005 REQ "Event Shape Parity and Trust Disclosure", scenario "Unknown id raises
// not_found".
func TestGetWebhookEventDetail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putEvent(store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{ID: 7, Provider: "github", EventType: "push",
			TrustMode: "signed", Verified: true, PayloadSize: 12},
		VerifyDetail: "hmac-sha256 ok", ExternalID: "dlv-1", ContentType: "application/json",
		SourceIP: "10.0.0.9",
		Headers:  []byte(`{"X-Hub-Signature-256":"«redacted»","X-GitHub-Event":"push"}`),
		Payload:  []byte(`{"action":1}`),
	})
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	var out struct {
		ID           int64             `json:"id"`
		Provider     string            `json:"provider"`
		TrustMode    string            `json:"trust_mode"`
		Verified     bool              `json:"verified"`
		VerifyDetail string            `json:"verify_detail"`
		ExternalID   string            `json:"external_id"`
		ContentType  string            `json:"content_type"`
		SourceIP     string            `json:"source_ip"`
		Headers      map[string]string `json:"headers"`
		Payload      string            `json:"payload"`
	}
	callOK(t, ctx, cs, "get_webhook_event", map[string]any{"id": 7}, &out)
	if out.ID != 7 || out.Provider != "github" || !out.Verified || out.TrustMode != "signed" {
		t.Fatalf("detail = %+v, want id 7 github signed verified", out)
	}
	if out.VerifyDetail != "hmac-sha256 ok" || out.ExternalID != "dlv-1" ||
		out.ContentType != "application/json" || out.SourceIP != "10.0.0.9" {
		t.Fatalf("detail metadata = %+v", out)
	}
	if out.Payload != `{"action":1}` {
		t.Fatalf("payload = %q, want the stored raw body", out.Payload)
	}
	// The sanitized headers come through with the ingest-time redaction intact.
	if out.Headers["X-GitHub-Event"] != "push" || out.Headers["X-Hub-Signature-256"] != "«redacted»" {
		t.Fatalf("headers = %v, want sanitized ingest headers", out.Headers)
	}

	callErr(t, ctx, cs, "get_webhook_event", map[string]any{"id": 999}, "not_found")
	callErr(t, ctx, cs, "get_webhook_event", map[string]any{"id": 0}, "invalid_argument")
}

// TestReplayWebhookEventSurface: the replay verb resolves its id (unknown → not_found) before any
// outbound thought, and with #40's delivery landed a valid id with neither an explicit target nor a
// configured default is the hard invalid_argument (never a guessed target). The full SSRF/delivery
// behaviour is exercised in replay_test.go.
// Governing: SPEC-0005 scenario "Unknown id raises not_found", "No target and no default is an error".
func TestReplayWebhookEventSurface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putEvent(store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{ID: 3, Provider: "github", TrustMode: "signed", Verified: true},
		Payload:          []byte(`{}`),
	})
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 999}, "not_found")
	// No explicit target and no configured replay_default_target: a hard invalid_argument.
	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 3}, "invalid_argument")
}

// TestListProviders: list_providers reports the configured snapshot — name, family, trust mode,
// enabled, secret_status, path — and the raw response never carries secret material.
// Governing: SPEC-0005 scenario "Secret status without the secret".
func TestListProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, eventVerbNames)
	h := New(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(h.Close)
	h.SetProviders([]ProviderStatus{
		{Name: "github", Family: "webhook", TrustMode: "signed", Enabled: true,
			SecretStatus: "configured", Path: "/webhooks/github"},
		{Name: "homelab", Family: "webhook", TrustMode: "open", Enabled: true,
			SecretStatus: "none-by-design", Path: "/webhooks/generic/homelab"},
	})
	ts := httptest.NewServer(routesFor(h))
	t.Cleanup(ts.Close)
	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_providers", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("tools/call list_providers: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_providers tool error: %s", contentText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out listProvidersOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if len(out.Providers) != 2 {
		t.Fatalf("providers = %+v, want 2", out.Providers)
	}
	gh := out.Providers[0]
	if gh.Name != "github" || gh.Family != "webhook" || gh.TrustMode != "signed" ||
		!gh.Enabled || gh.SecretStatus != "configured" || gh.Path != "/webhooks/github" {
		t.Fatalf("github provider = %+v", gh)
	}
	if out.Providers[1].SecretStatus != "none-by-design" {
		t.Fatalf("open provider secret_status = %q, want none-by-design", out.Providers[1].SecretStatus)
	}
	// The classification is the whole story: no key or value on the wire resembles a secret.
	if s := string(raw); strings.Contains(s, "secret\":") && !strings.Contains(s, "secret_status") {
		t.Fatalf("provider response carries a secret-like field: %s", s)
	}
}

// TestEventStoreFailureIsGenericToClient: a DB failure inside an event read reaches the client
// only as the generic "internal" code while the log records slug, tool, and the error chain.
// Governing: SPEC-0005 REQ "Stable Error Shape", SPEC-0014 REQ "Error Handling Standards".
func TestEventStoreFailureIsGenericToClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf strings.Builder
	f := newFakeStore()
	f.failErr = errors.New("pg: connection refused host=db-internal-secret")
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, eventVerbNames)

	h := New(f, slog.New(slog.NewTextHandler(&buf, nil)))
	ts := httptest.NewServer(routesFor(h))
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)

	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	defer func() { _ = cs.Close() }()

	text := callErr(t, ctx, cs, "list_webhook_events", map[string]any{}, "internal")
	if strings.Contains(text, "db-internal-secret") {
		t.Fatalf("internal error detail leaked to the client: %q", text)
	}
	logs := buf.String()
	for _, want := range []string{"agent-a-11111111", "list_webhook_events", "db-internal-secret"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log missing %q; logs:\n%s", want, logs)
		}
	}
}

// TestListWebhookEventsCursorPagination: paging with each response's next_cursor as the next
// request's cursor walks the whole log newest-first with no duplicates or gaps, a full page always
// yields a cursor and the final short page yields none, and a malformed cursor is invalid_argument.
// Governing: SPEC-0005 scenario "Cursor round-trip has no duplicates or gaps", REQ "Deterministic
// Pagination and Filtering".
func TestListWebhookEventsCursorPagination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := time.Now().Truncate(time.Second)
	f := newFakeStore()
	// 5 events; ids 4 and 5 share a received_at so the id tiebreak is exercised across a page edge.
	f.putEvent(mkEvent(1, "github", "push", base.Add(1*time.Second)))
	f.putEvent(mkEvent(2, "github", "push", base.Add(2*time.Second)))
	f.putEvent(mkEvent(3, "stripe", "charge", base.Add(3*time.Second)))
	f.putEvent(mkEvent(4, "github", "pull", base.Add(4*time.Second)))
	f.putEvent(mkEvent(5, "github", "push", base.Add(4*time.Second)))
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	var seen []int64
	cursor := ""
	for page := 0; page < 10; page++ {
		args := map[string]any{"limit": 2}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var out listWebhookEventsOut
		callOK(t, ctx, cs, "list_webhook_events", args, &out)
		for _, e := range out.Events {
			seen = append(seen, e.ID)
		}
		if out.NextCursor == "" {
			break
		}
		if len(out.Events) != 2 {
			t.Fatalf("page %d returned %d events with a next_cursor set", page, len(out.Events))
		}
		cursor = out.NextCursor
	}
	// Newest-first, exhaustive, no dupes: ids 5,4 share a timestamp so id DESC breaks the tie.
	want := []int64{5, 4, 3, 2, 1}
	if !slices.Equal(seen, want) {
		t.Fatalf("paged ids = %v, want %v (newest-first, no dupes/gaps)", seen, want)
	}

	// A malformed cursor is a stable invalid_argument, never a silent empty page.
	callErr(t, ctx, cs, "list_webhook_events", map[string]any{"cursor": "not-a-cursor"}, "invalid_argument")
}

// TestListWebhookEventsFiltering: provider, event_type, and since (timestamp or id) narrow the scan
// end-to-end, and a malformed since is invalid_argument.
// Governing: SPEC-0005 REQ "Deterministic Pagination and Filtering".
func TestListWebhookEventsFiltering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := time.Now().Truncate(time.Second)
	f := newFakeStore()
	f.putEvent(mkEvent(1, "github", "push", base.Add(1*time.Second)))
	f.putEvent(mkEvent(2, "stripe", "charge", base.Add(2*time.Second)))
	f.putEvent(mkEvent(3, "github", "pull", base.Add(3*time.Second)))
	f.putEvent(mkEvent(4, "github", "push", base.Add(4*time.Second)))
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	// Provider + event_type filter.
	var out listWebhookEventsOut
	callOK(t, ctx, cs, "list_webhook_events",
		map[string]any{"provider": "github", "event_type": "push"}, &out)
	if got := idsOf(out.Events); !slices.Equal(got, []int64{4, 1}) {
		t.Fatalf("github/push ids = %v, want [4 1]", got)
	}

	// since as a timestamp bounds the lower edge inclusively.
	callOK(t, ctx, cs, "list_webhook_events",
		map[string]any{"since": base.Add(3 * time.Second).UTC().Format(time.RFC3339)}, &out)
	if got := idsOf(out.Events); !slices.Equal(got, []int64{4, 3}) {
		t.Fatalf("since-timestamp ids = %v, want [4 3]", got)
	}

	// since as an event id bounds by id >= n.
	callOK(t, ctx, cs, "list_webhook_events", map[string]any{"since": "3"}, &out)
	if got := idsOf(out.Events); !slices.Equal(got, []int64{4, 3}) {
		t.Fatalf("since-id ids = %v, want [4 3]", got)
	}

	// A since that is neither a timestamp nor an id is invalid_argument.
	callErr(t, ctx, cs, "list_webhook_events", map[string]any{"since": "yesterday"}, "invalid_argument")
}

// TestRecentEventsResource: the read-only recent-events resource is advertised, returns the same
// newest-first EventSummary shape as list_webhook_events with trust metadata present, and is gated
// on the list_webhook_events read scope.
// Governing: SPEC-0005 REQ "Read-Only Recent-Events Resource", scenario "Resource returns
// summaries, never mutates".
func TestRecentEventsResource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := time.Now().Truncate(time.Second)
	f := newFakeStore()
	f.putEvent(mkEvent(1, "github", "push", base.Add(1*time.Second)))
	f.putEvent(mkEvent(2, "dockerhub", "push", base.Add(2*time.Second)))
	// Give event 2 token trust so we can assert verified:false travels with the resource.
	e2 := f.events[2]
	e2.TrustMode, e2.Verified = "token", false
	f.events[2] = e2
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)

	resources, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].URI != recentEventsURI {
		t.Fatalf("advertised resources = %+v, want exactly %s", resources.Resources, recentEventsURI)
	}
	if resources.Resources[0].MIMEType != "application/json" {
		t.Fatalf("resource MIME = %q, want application/json", resources.Resources[0].MIMEType)
	}

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: recentEventsURI})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("resource contents = %+v", res.Contents)
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &body); err != nil {
		t.Fatalf("unmarshal resource body: %v", err)
	}
	if len(body.Events) != 2 || body.Events[0]["id"] != float64(2) {
		t.Fatalf("resource events = %v, want 2 rows newest-first", body.Events)
	}
	// Trust metadata always travels with the resource, and a token event reports verified:false.
	if body.Events[0]["trust_mode"] != "token" || body.Events[0]["verified"] != false {
		t.Fatalf("resource trust = %v/%v, want token/false", body.Events[0]["trust_mode"], body.Events[0]["verified"])
	}
	// Summaries omit heavy fields on the resource too.
	if _, has := body.Events[0]["payload"]; has {
		t.Fatalf("resource summary leaked payload: %v", body.Events[0])
	}

	// An endpoint without the list_webhook_events read scope is not offered the resource.
	f2 := newFakeStore()
	cs2 := session(t, ctx, f2, []string{"reviews"}, []string{"list_todos"})
	r2, err := cs2.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("resources/list (narrow scope): %v", err)
	}
	if len(r2.Resources) != 0 {
		t.Fatalf("narrow-scope endpoint advertised resources = %+v, want none", r2.Resources)
	}
}

// mkEvent builds a minimal signed EventHistoryDetail for the pagination/filter/resource tests.
func mkEvent(id int64, provider, eventType string, receivedAt time.Time) store.EventHistoryDetail {
	return store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{
			ID: id, Provider: provider, EventType: eventType, TrustMode: "signed",
			Verified: true, PayloadSize: 2, ReceivedAt: receivedAt,
		},
		Payload: []byte(`{}`),
	}
}

func idsOf(events []eventSummaryOut) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

// routesFor mounts a handler under /mcp exactly as internal/server does.
func routesFor(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	return r
}
