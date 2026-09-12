package mcp

// This file is the SPEC-0005 event-history tool registry served over MCP: the four-tool contract
// `list_webhook_events`, `get_webhook_event`, `replay_webhook_event`, and `list_providers`, each
// with an SDK-declared input schema and structured output schema. Only `replay_webhook_event` may
// ever side-effect; the other three are strictly read-only. Like the SPEC-0006 agent verbs
// (tools.go), each tool is registered only when its name is in the vended endpoint's verb
// allowlist, so tools/list advertises exactly the endpoint's scope.
//
// Deterministic cursor pagination/filtering and full provider enumeration semantics land with #39,
// and replay delivery (target resolution, SSRF-hardened validation, the outbound POST, per-caller
// throttling) lands with #40 — this file pins the tool surface, schemas, and the stable error
// shape they all share.
//
// Governing: ADR-0005 (contract shape), SPEC-0005 REQ "Tool Surface and Naming",
// SPEC-0005 REQ "Stable Error Shape".

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

// Stable machine error codes completing the SPEC-0005 set (codeNotFound and codeInternal are
// shared with the SPEC-0006 verbs in tools.go). Governing: SPEC-0005 REQ "Stable Error Shape".
const (
	codeInvalidArgument = "invalid_argument"
	// codeReplayFailed is raised only for replay connection/transport failures (a downstream non-2xx
	// is reported as delivered, never raised). Governing: SPEC-0005 scenario "Downstream non-2xx is
	// reported, not raised".
	codeReplayFailed = "replay_failed"
	// codeRateLimited is returned when the per-endpoint replay budget is exhausted. The SPEC-0005
	// Security Requirements mandate replay be rate-limited per caller (it drives outbound requests
	// and could be abused for amplification/SSRF probing); a distinct machine code lets a caller
	// back off deterministically rather than confusing throttling with a bad target
	// (invalid_argument) or a transport failure (replay_failed).
	// Governing: SPEC-0005 "Rate Limiting" security requirement.
	codeRateLimited = "rate_limited"
)

const (
	defaultEventListLimit = 50
	maxEventListLimit     = 200

	// recentEventsURI is the read-only recent-events resource (#38); recentEventsLimit caps it.
	// Governing: SPEC-0005 REQ "Read-Only Recent-Events Resource".
	recentEventsURI   = "switchboard://events/recent"
	recentEventsLimit = 50
)

// eventVerbs is the SPEC-0005 event-history tool surface. Like agentVerbs, a tools/call naming
// one of these outside the endpoint's allowlist is a scope violation (stable "forbidden" code via
// scopeGuard), distinguishable from an unknown tool.
var eventVerbs = verbSet(EventVerbs())

// --- tool input/output shapes (SDK-inferred JSON schemas) ---

// eventSummaryOut is the SPEC-0005 EventSummary shape: compact rows for broad scans — no payload,
// no headers — with trust metadata always present so an agent can never mistake an unverified
// event for a signed one. Governing: SPEC-0005 REQ "Event Shape Parity and Trust Disclosure".
type eventSummaryOut struct {
	ID          int64  `json:"id" jsonschema:"the event id"`
	Provider    string `json:"provider" jsonschema:"originating provider name"`
	EventType   string `json:"event_type,omitempty" jsonschema:"provider event type, if any"`
	TrustMode   string `json:"trust_mode" jsonschema:"trust mode: signed, token, open, or queue"`
	Verified    bool   `json:"verified" jsonschema:"whether the delivery passed cryptographic verification (false for token/open/queue trust)"`
	PayloadSize int    `json:"payload_size" jsonschema:"stored payload size in bytes"`
	ReceivedAt  string `json:"received_at" jsonschema:"RFC 3339 receipt time"`
	WebhookID   string `json:"webhook_id,omitempty" jsonschema:"the self-managed webhook the delivery arrived on, if any"`
	Routing     any    `json:"routing,omitempty" jsonschema:"how the delivery was routed (SPEC-0020 trace); a drop shows action.drop=true"`
}

// eventDetailOut is the SPEC-0005 EventDetail shape: every summary field plus the full sanitized
// record. Headers were sanitized at ingest (signature/secret values redacted) and no signing
// secret ever crosses this boundary.
type eventDetailOut struct {
	ID           int64             `json:"id" jsonschema:"the event id"`
	Provider     string            `json:"provider" jsonschema:"originating provider name"`
	EventType    string            `json:"event_type,omitempty" jsonschema:"provider event type, if any"`
	TrustMode    string            `json:"trust_mode" jsonschema:"trust mode: signed, token, open, or queue"`
	Verified     bool              `json:"verified" jsonschema:"whether the delivery passed cryptographic verification (false for token/open/queue trust)"`
	PayloadSize  int               `json:"payload_size" jsonschema:"stored payload size in bytes"`
	ReceivedAt   string            `json:"received_at" jsonschema:"RFC 3339 receipt time"`
	VerifyDetail string            `json:"verify_detail,omitempty" jsonschema:"human-readable verification result"`
	ExternalID   string            `json:"external_id,omitempty" jsonschema:"provider delivery id used for dedup, if any"`
	ContentType  string            `json:"content_type,omitempty" jsonschema:"payload content type, if recorded"`
	SourceIP     string            `json:"source_ip,omitempty" jsonschema:"delivering client IP, if recorded"`
	Headers      map[string]string `json:"headers" jsonschema:"sanitized request headers (signature/secret values redacted at ingest)"`
	Payload      string            `json:"payload" jsonschema:"the stored raw payload body"`
	WebhookID    string            `json:"webhook_id,omitempty" jsonschema:"the self-managed webhook the delivery arrived on, if any"`
	Routing      any               `json:"routing,omitempty" jsonschema:"how the delivery was routed (SPEC-0020 trace); a drop shows action.drop=true"`
}

type listWebhookEventsIn struct {
	Provider  string `json:"provider,omitempty" jsonschema:"restrict to one provider name"`
	EventType string `json:"event_type,omitempty" jsonschema:"restrict to one event type"`
	Since     string `json:"since,omitempty" jsonschema:"lower-edge bound (inclusive): an RFC 3339 timestamp or an event id"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum events to return (default 50, minimum 1, maximum 200)"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"opaque pagination cursor from a previous response's next_cursor"`
}

type listWebhookEventsOut struct {
	Events []eventSummaryOut `json:"events" jsonschema:"event summaries, newest first (received_at DESC, id DESC)"`
	// NextCursor encodes the last (received_at, id) seen; present only when the result filled the
	// limit, so the caller feeds it back as `cursor` to fetch the next, older page.
	// Governing: SPEC-0005 REQ "Deterministic Pagination and Filtering".
	NextCursor string `json:"next_cursor,omitempty" jsonschema:"opaque cursor for the next page when the result was truncated at limit"`
}

// eventCursor is the opaque pagination token: the last (received_at, id) a page returned. It is
// base64url-encoded JSON on the wire — clients MUST treat it as opaque and pass it back verbatim.
// Governing: ADR-0005 (opaque cursor), SPEC-0005 REQ "Deterministic Pagination and Filtering".
type eventCursor struct {
	ReceivedAt time.Time `json:"t"`
	ID         int64     `json:"i"`
}

func encodeEventCursor(c eventCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeEventCursor parses an opaque cursor. A malformed token is a stable invalid_argument, never
// a silent empty page. Governing: SPEC-0005 scenario "A malformed cursor MUST raise invalid_argument".
func decodeEventCursor(s string) (eventCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return eventCursor{}, err
	}
	var c eventCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return eventCursor{}, err
	}
	if c.ReceivedAt.IsZero() || c.ID <= 0 {
		return eventCursor{}, fmt.Errorf("cursor missing (received_at, id)")
	}
	return c, nil
}

// parseSince resolves the `since` lower-edge bound: an all-digit value is an event id (id >= n), an
// RFC 3339 value is a timestamp (received_at >= t). Anything else is a stable invalid_argument.
// Governing: SPEC-0005 REQ "Deterministic Pagination and Filtering".
func parseSince(since string, f *store.EventHistoryFilter) error {
	if since == "" {
		return nil
	}
	if id, err := strconv.ParseInt(since, 10, 64); err == nil {
		if id <= 0 {
			return fmt.Errorf("since id must be positive")
		}
		f.SinceID = id
		return nil
	}
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return fmt.Errorf("since must be an RFC 3339 timestamp or an event id")
	}
	f.SinceTime = t
	return nil
}

type getWebhookEventIn struct {
	ID int64 `json:"id" jsonschema:"the event id to fetch"`
}

type replayWebhookEventIn struct {
	ID        int64  `json:"id" jsonschema:"the stored event id to replay"`
	TargetURL string `json:"target_url,omitempty" jsonschema:"replay target URL (http/https only); defaults to the configured replay target"`
}

type replayWebhookEventOut struct {
	ID             int64  `json:"id" jsonschema:"the replayed event id"`
	TargetURL      string `json:"target_url" jsonschema:"the resolved replay target"`
	Delivered      bool   `json:"delivered" jsonschema:"whether the POST completed at all"`
	ResponseStatus *int   `json:"response_status" jsonschema:"downstream HTTP status, or null on connection failure"`
	ResponseMs     int64  `json:"response_ms" jsonschema:"elapsed milliseconds for the replay POST"`
}

// ProviderStatus is the SPEC-0005 provider enumeration row: presence/absence classification only,
// never secret material. Governing: SPEC-0005 REQ "Provider Enumeration Without Secrets".
type ProviderStatus struct {
	Name         string `json:"name" jsonschema:"provider name"`
	Family       string `json:"family" jsonschema:"provider family: webhook or queue"`
	TrustMode    string `json:"trust_mode" jsonschema:"trust mode: signed, token, open, or queue"`
	Enabled      bool   `json:"enabled" jsonschema:"whether the provider currently accepts deliveries"`
	SecretStatus string `json:"secret_status" jsonschema:"secret presence classification: configured, missing, or none-by-design"`
	Path         string `json:"path,omitempty" jsonschema:"HTTP route path (webhook providers)"`
	Channel      string `json:"channel,omitempty" jsonschema:"broker channel (queue providers)"`
}

type listProvidersIn struct{}

type listProvidersOut struct {
	Providers []ProviderStatus `json:"providers" jsonschema:"configured inbound providers, without any secret values"`
}

// SetProviders installs the configured-provider snapshot served by list_providers. Called at
// wiring time (internal/server) with statuses projected from the ingestion config; safe to call
// concurrently with live sessions (atomic swap, read-only consumers).
func (h *Handler) SetProviders(ps []ProviderStatus) {
	h.providers.Store(&ps)
}

// SetProviderSource installs a LIVE provider enumeration source consulted on each list_providers
// call — the registry-backed wiring (internal/server) uses it so providers created at runtime
// enumerate without a restart, with the SPEC-0005 output shape unchanged. Takes precedence over
// any SetProviders snapshot; passing nil reverts to the snapshot. Safe to call concurrently with
// live sessions (atomic swap). Governing: ADR-0020, SPEC-0017 REQ "Runtime Provider Registry".
func (h *Handler) SetProviderSource(fn func(context.Context) []ProviderStatus) {
	if fn == nil {
		h.providerSource.Store(nil)
		return
	}
	h.providerSource.Store(&fn)
}

// registerEventTools installs the endpoint's allowlisted SPEC-0005 event-history tools on the
// per-session server, mirroring the SPEC-0006 verb registry: tools/list advertises exactly the
// allowlist. Governing: SPEC-0005 REQ "Tool Surface and Naming", SPEC-0014 REQ "Agent Tool
// Surface over MCP" (scope filters the advertised tools).
func (h *Handler) registerEventTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "list_webhook_events") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "list_webhook_events",
			Description: "List stored webhook/queue event summaries, newest first. Read-only.",
		}, h.listWebhookEventsTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "get_webhook_event") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "get_webhook_event",
			Description: "Fetch one stored event's full record: sanitized headers, raw payload, verification detail. Read-only.",
		}, h.getWebhookEventTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "replay_webhook_event") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "replay_webhook_event",
			Description: "Re-POST a stored event's raw payload to a target URL. The only side-effecting tool.",
		}, h.replayWebhookEventTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "list_providers") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "list_providers",
			Description: "Enumerate configured inbound providers and their trust posture. Never returns secret values. Read-only.",
		}, h.listProvidersTool(ep))
	}
}

// registerEventResources installs the SPEC-0005 read-only recent-events resource on the per-session
// server. It is gated on the same `list_webhook_events` read scope as the tool it mirrors: an
// endpoint that may not scan the event log over tools must not read it as a resource either. The
// resource is strictly read-only — all mutation/replay stays in tools.
// Governing: SPEC-0005 REQ "Read-Only Recent-Events Resource".
func (h *Handler) registerEventResources(srv *sdk.Server, ep store.AuthEndpoint) {
	if !hasScope(ep.ScopeVerbs, "list_webhook_events") {
		return
	}
	srv.AddResource(&sdk.Resource{
		URI:         recentEventsURI,
		Name:        "recent_webhook_events",
		Description: "The most recent stored webhook/queue event summaries, newest first. Read-only.",
		MIMEType:    "application/json",
	}, h.recentEventsResource(ep))
}

// recentEventsResource serves switchboard://events/recent: the same EventSummary shape as
// list_webhook_events (trust metadata always present), newest-first and capped, with no filters and
// no mutation. Governing: SPEC-0005 REQ "Read-Only Recent-Events Resource", scenario "Resource
// returns summaries, never mutates".
func (h *Handler) recentEventsResource(ep store.AuthEndpoint) sdk.ResourceHandler {
	return func(ctx context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		items, err := h.store.ListEventHistory(ctx, store.EventHistoryFilter{Limit: recentEventsLimit})
		if err != nil {
			return nil, h.mapEventStoreErr(ep, "resources/read events/recent", err)
		}
		body := listWebhookEventsOut{Events: make([]eventSummaryOut, 0, len(items))}
		for _, it := range items {
			body.Events = append(body.Events, toEventSummaryOut(it))
		}
		payload, err := json.Marshal(body)
		if err != nil {
			h.log.Error("mcp recent-events resource marshal", "slug", ep.Slug, "err", err)
			return nil, &toolError{codeInternal, "internal error"}
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
			URI:      recentEventsURI,
			MIMEType: "application/json",
			Text:     string(payload),
		}}}, nil
	}
}

// --- tool handlers (the three reads are thin wrappers over internal/store and mutate nothing) ---

func (h *Handler) listWebhookEventsTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[listWebhookEventsIn, listWebhookEventsOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in listWebhookEventsIn) (*sdk.CallToolResult, listWebhookEventsOut, error) {
		// Governing: SPEC-0005 scenario "Limit is clamped" — an out-of-range limit is a stable
		// invalid_argument, never an unbounded response.
		if in.Limit < 0 || in.Limit > maxEventListLimit {
			return nil, listWebhookEventsOut{}, &toolError{codeInvalidArgument, "limit must be between 1 and 200"}
		}
		limit := in.Limit
		if limit == 0 {
			limit = defaultEventListLimit
		}
		filter := store.EventHistoryFilter{Provider: in.Provider, EventType: in.EventType, Limit: limit}
		if err := parseSince(in.Since, &filter); err != nil {
			return nil, listWebhookEventsOut{}, &toolError{codeInvalidArgument, err.Error()}
		}
		if in.Cursor != "" {
			c, err := decodeEventCursor(in.Cursor)
			if err != nil {
				// Governing: SPEC-0005 scenario "A malformed cursor MUST raise invalid_argument".
				return nil, listWebhookEventsOut{}, &toolError{codeInvalidArgument, "malformed cursor"}
			}
			filter.CursorTime, filter.CursorID = c.ReceivedAt, c.ID
		}
		items, err := h.store.ListEventHistory(ctx, filter)
		if err != nil {
			return nil, listWebhookEventsOut{}, h.mapEventStoreErr(ep, "list_webhook_events", err)
		}
		out := listWebhookEventsOut{Events: make([]eventSummaryOut, 0, len(items))}
		for _, it := range items {
			out.Events = append(out.Events, toEventSummaryOut(it))
		}
		// A full page means there may be more: emit the keyset cursor for the last (received_at, id)
		// so the next request continues strictly older, with no duplicates or gaps under concurrent
		// ingest. Governing: SPEC-0005 scenario "Cursor round-trip has no duplicates or gaps".
		if len(items) == limit {
			last := items[len(items)-1]
			out.NextCursor = encodeEventCursor(eventCursor{ReceivedAt: last.ReceivedAt, ID: last.ID})
		}
		return nil, out, nil
	}
}

func (h *Handler) getWebhookEventTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[getWebhookEventIn, eventDetailOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in getWebhookEventIn) (*sdk.CallToolResult, eventDetailOut, error) {
		if in.ID <= 0 {
			return nil, eventDetailOut{}, &toolError{codeInvalidArgument, "id must be a positive event id"}
		}
		d, err := h.store.EventHistoryByID(ctx, in.ID)
		if err != nil {
			return nil, eventDetailOut{}, h.mapEventStoreErr(ep, "get_webhook_event", err)
		}
		return nil, toEventDetailOut(d), nil
	}
}

func (h *Handler) replayWebhookEventTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[replayWebhookEventIn, replayWebhookEventOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in replayWebhookEventIn) (*sdk.CallToolResult, replayWebhookEventOut, error) {
		if in.ID <= 0 {
			return nil, replayWebhookEventOut{}, &toolError{codeInvalidArgument, "id must be a positive event id"}
		}
		// Governing: SPEC-0005 scenario "Unknown id raises not_found" — the id is resolved (a pure
		// read) before any thought of an outbound request.
		d, err := h.store.EventHistoryByID(ctx, in.ID)
		if err != nil {
			return nil, replayWebhookEventOut{}, h.mapEventStoreErr(ep, "replay_webhook_event", err)
		}
		// Replay delivery — rate limit, target resolution against the configured default,
		// SSRF-hardened scheme + resolved-IP validation, the outbound POST, and mandatory logging —
		// lives in replay.go. The stored raw payload and a replay-safe header subset are replayed;
		// the target is always the explicit arg or the configured default, so the tool never replays
		// back to the originating provider (there is no back-to-provider option in the contract).
		// Governing: SPEC-0005 REQ "Replay Safety".
		out, err := h.replay(ctx, ep.Slug, ep.ID, in.ID, toEventDetailOut(d), in.TargetURL)
		if err != nil {
			return nil, replayWebhookEventOut{}, err
		}
		return nil, out, nil
	}
}

func (h *Handler) listProvidersTool(_ store.AuthEndpoint) sdk.ToolHandlerFor[listProvidersIn, listProvidersOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ listProvidersIn) (*sdk.CallToolResult, listProvidersOut, error) {
		out := listProvidersOut{Providers: []ProviderStatus{}}
		// Registry-backed source first (resolved per call, so runtime provider changes are visible
		// without restart — ADR-0020); the wiring-time snapshot is the legacy/test fallback.
		if src := h.providerSource.Load(); src != nil {
			out.Providers = append(out.Providers, (*src)(ctx)...)
			return nil, out, nil
		}
		if ps := h.providers.Load(); ps != nil {
			out.Providers = append(out.Providers, *ps...)
		}
		return nil, out, nil
	}
}

// mapEventStoreErr maps store sentinels onto the stable SPEC-0005 codes. Unexpected failures reach
// the client only as the generic "internal" code while the log records the endpoint slug, the
// tool, and the wrapped error chain — never any secret material.
// Governing: SPEC-0005 REQ "Stable Error Shape", SPEC-0014 REQ "Error Handling Standards".
func (h *Handler) mapEventStoreErr(ep store.AuthEndpoint, tool string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return &toolError{codeNotFound, "event not found"}
	}
	h.log.Error("mcp tool store failure", "slug", ep.Slug, "tool", tool,
		"err", fmt.Errorf("tools/call %s: %w", tool, err))
	return &toolError{codeInternal, "internal error"}
}

// --- projection helpers ---

func toEventSummaryOut(e store.EventHistoryItem) eventSummaryOut {
	return eventSummaryOut{
		ID: e.ID, Provider: e.Provider, EventType: e.EventType, TrustMode: e.TrustMode,
		Verified: e.Verified, PayloadSize: e.PayloadSize,
		ReceivedAt: e.ReceivedAt.UTC().Format(time.RFC3339),
		WebhookID:  e.WebhookID, Routing: decodeTrace(e.RoutingTrace),
	}
}

func toEventDetailOut(d store.EventHistoryDetail) eventDetailOut {
	out := eventDetailOut{
		ID: d.ID, Provider: d.Provider, EventType: d.EventType, TrustMode: d.TrustMode,
		Verified: d.Verified, PayloadSize: d.PayloadSize,
		ReceivedAt:   d.ReceivedAt.UTC().Format(time.RFC3339),
		VerifyDetail: d.VerifyDetail, ExternalID: d.ExternalID, ContentType: d.ContentType,
		SourceIP:  d.SourceIP,
		Headers:   map[string]string{},
		Payload:   string(d.Payload),
		WebhookID: d.WebhookID, Routing: decodeTrace(d.RoutingTrace),
	}
	// Headers were persisted as a sanitized JSON object at ingest; a row that fails to parse
	// yields an empty object rather than failing the read — the record itself is the contract.
	if len(d.Headers) > 0 {
		_ = json.Unmarshal(d.Headers, &out.Headers)
	}
	return out
}
