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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
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
)

// eventVerbs is the SPEC-0005 event-history tool surface. Like agentVerbs, a tools/call naming
// one of these outside the endpoint's allowlist is a scope violation (stable "forbidden" code via
// scopeGuard), distinguishable from an unknown tool.
var eventVerbs = map[string]bool{
	"list_webhook_events": true, "get_webhook_event": true,
	"replay_webhook_event": true, "list_providers": true,
}

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
}

type listWebhookEventsIn struct {
	Provider  string `json:"provider,omitempty" jsonschema:"restrict to one provider name"`
	EventType string `json:"event_type,omitempty" jsonschema:"restrict to one event type"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum events to return (default 50, minimum 1, maximum 200)"`
}

type listWebhookEventsOut struct {
	Events []eventSummaryOut `json:"events" jsonschema:"event summaries, newest first (received_at DESC, id DESC)"`
	// NextCursor is part of the declared SPEC-0005 output shape; the opaque-cursor pagination that
	// populates it (and the since/cursor inputs) lands with #39.
	NextCursor string `json:"next_cursor,omitempty" jsonschema:"opaque cursor for the next page when the result was truncated at limit"`
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
		items, err := h.store.ListEventHistory(ctx, store.EventHistoryFilter{
			Provider: in.Provider, EventType: in.EventType, Limit: limit,
		})
		if err != nil {
			return nil, listWebhookEventsOut{}, h.mapEventStoreErr(ep, "list_webhook_events", err)
		}
		out := listWebhookEventsOut{Events: make([]eventSummaryOut, 0, len(items))}
		for _, it := range items {
			out.Events = append(out.Events, toEventSummaryOut(it))
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
	return func(_ context.Context, _ *sdk.CallToolRequest, _ listProvidersIn) (*sdk.CallToolResult, listProvidersOut, error) {
		out := listProvidersOut{Providers: []ProviderStatus{}}
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
	}
}

func toEventDetailOut(d store.EventHistoryDetail) eventDetailOut {
	out := eventDetailOut{
		ID: d.ID, Provider: d.Provider, EventType: d.EventType, TrustMode: d.TrustMode,
		Verified: d.Verified, PayloadSize: d.PayloadSize,
		ReceivedAt:   d.ReceivedAt.UTC().Format(time.RFC3339),
		VerifyDetail: d.VerifyDetail, ExternalID: d.ExternalID, ContentType: d.ContentType,
		SourceIP: d.SourceIP,
		Headers:  map[string]string{},
		Payload:  string(d.Payload),
	}
	// Headers were persisted as a sanitized JSON object at ingest; a row that fails to parse
	// yields an empty object rather than failing the read — the record itself is the contract.
	if len(d.Headers) > 0 {
		_ = json.Unmarshal(d.Headers, &out.Headers)
	}
	return out
}
