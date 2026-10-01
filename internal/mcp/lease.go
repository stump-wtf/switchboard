package mcp

// Endpoint Default Lease on MCP
//
// Every claim path resolves its lease the same way (internal/lease.TTL): an explicit per-call
// lease_ttl_seconds wins, clamped to the cap; else the endpoint's default lease; else the server
// default. claim, claim_next and heartbeat all apply it, so a heartbeat that names no
// lease_ttl_seconds extends by the endpoint's default rather than cutting a long lease back to 300s.
//
// The default is read from the store on each call that needs it, so an edit made over the human
// API, the web UI or set_default_lease applies to the next claim on every live session. The text an
// agent reads states the same number: the session instructions and the claim, claim_next and
// heartbeat descriptions are built from the endpoint's default when the session starts, and an edit
// re-registers those three tools on the endpoint's live sessions (tools/list_changed). Instructions
// are fixed at initialize by the protocol, so a live session's instructions keep the number they
// started with; the descriptions and get_default_lease always carry the current one.
//
// get_default_lease and set_default_lease are the endpoint's own read and write. They take no
// endpoint argument: the authenticated endpoint is the only one they can reach.
//
// Governing: ADR-0043, SPEC-0006 REQ "Lease Lifecycle and Crash Safety", REQ "Endpoint Default
// Lease Verbs", SPEC-0007 REQ "Endpoint Default Lease", SPEC-0011 REQ "Channel Capability on the
// Vended Session".
//
// @joestump-agent 10/01/2026 - Added. The lease text #552 introduced stated a hardwired 300; it now
// states the endpoint's own default.

import (
	"context"
	"errors"
	"strconv"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/lease"
	"github.com/stump-wtf/switchboard/internal/store"
)

// maxLeaseSeconds is the cap as the agent-facing text states it, in the seconds lease_ttl_seconds
// takes. It is built from lease.MaxTTL, so the text cannot drift from what lease.TTL enforces.
var maxLeaseSeconds = strconv.Itoa(lease.MaxSeconds)

// leaseVerbs is the membership set the scope guard consults for the lease verb family.
var leaseVerbs = verbSet(LeaseVerbs())

// leaseDefault is the default lease a piece of agent-facing text states: the seconds, and whose
// default it is. An endpoint without its own default states the server's.
func leaseDefault(def *int) (seconds, whose string) {
	seconds = strconv.Itoa(lease.EffectiveSeconds(def))
	if def != nil {
		return seconds, "this endpoint's default"
	}
	return seconds, "the server default"
}

// claimLeaseNote is the lease sentence claim and claim_next share: the default and maximum, the
// heartbeat that extends it, and what a lapse does. A worker that never heartbeats loses the todo
// to the reaper while it is still working, and nothing tells it so until its next call.
func claimLeaseNote(def *int) string {
	seconds, whose := leaseDefault(def)
	return "The claim holds a lease for " + seconds + " seconds (" + whose + ") unless you pass " +
		"lease_ttl_seconds (max " + maxLeaseSeconds + "); size it to the work you expect, and call heartbeat " +
		"before it runs out. A lapsed lease is reaped: the todo goes back to the queue for another worker. "
}

// heartbeatDescription states the heartbeat contract with the endpoint's default, including the
// trap the default removes: a bare heartbeat resets the lease to the default, not to whatever the
// claim asked for.
func heartbeatDescription(def *int) string {
	seconds, whose := leaseDefault(def)
	return "Extend the lease on a claimed todo this endpoint holds: it is reset to end " +
		"lease_ttl_seconds from now (default " + seconds + ", max " + maxLeaseSeconds + "). Without " +
		"lease_ttl_seconds the lease is reset to " + seconds + " seconds (" + whose + "), even when the " +
		"claim asked for longer. Call it before the lease runs out, before any slow step, and before any " +
		"irreversible action you take for the todo. A conflict means you no longer hold the todo — its " +
		"lease lapsed and it was requeued, or another attempt holds it: stop, and make no further " +
		"changes on its behalf. On a claim made with require_fence, pass its lease_token."
}

// registerLeaseBoundTools registers the three verbs whose descriptions state the endpoint's default
// lease. Registering a tool that already exists replaces it and notifies the session
// (tools/list_changed), which is how RefreshEndpointLease updates a live session.
func (h *Handler) registerLeaseBoundTools(srv *sdk.Server, ep store.AuthEndpoint) {
	def := ep.DefaultLeaseTTLSeconds
	if hasScope(ep.ScopeVerbs, "claim") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "claim",
			Description: "Atomically claim a pending todo by id. " + claimLeaseNote(def) + "A conflict means " +
				"you did not get the todo: leave it alone. The response carries the new attempt's " +
				"attempt_seq and the todo's earlier attempts as prior_attempts; " + priorAttemptsWarning,
		}, h.claimTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "claim_next") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "claim_next",
			Description: "Atomically claim the oldest available todo from this endpoint's granted queues. " +
				"Returns empty=true when there is no work — that is the normal idle answer, not an error. " +
				claimLeaseNote(def) + "Safe to call concurrently from several workers sharing this endpoint: " +
				"each caller receives a different todo; pass require_fence so a worker whose lease lapsed " +
				"cannot act on a todo another worker has since claimed. A claim carries the new attempt's " +
				"attempt_seq and the todo's earlier attempts as prior_attempts; " + priorAttemptsWarning,
		}, h.claimNextTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "heartbeat") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "heartbeat",
			Description: heartbeatDescription(def),
		}, h.heartbeatTool(ep))
	}
}

// leaseFor resolves a claim's or heartbeat's lease. An explicit lease_ttl_seconds needs no read;
// without one the endpoint's live default is read, so an edit applies from the next call.
func (h *Handler) leaseFor(ctx context.Context, ep store.AuthEndpoint, tool string, perCall int) (time.Duration, error) {
	if perCall > 0 {
		return lease.TTL(perCall, nil), nil
	}
	def, err := h.store.EndpointDefaultLease(ctx, ep.ID)
	if err != nil {
		return 0, h.mapEndpointErr(ep, tool, err)
	}
	return lease.TTL(0, def), nil
}

// mapEndpointErr maps a failed read or write of the caller's own endpoint row. Not found means the
// endpoint is gone or revoked; anything else is internal, logged with the slug and the tool.
func (h *Handler) mapEndpointErr(ep store.AuthEndpoint, tool string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return &toolError{codeNotFound, "endpoint not found or no longer active"}
	}
	h.log.Error("mcp endpoint lease read failed", "slug", ep.Slug, "tool", tool, "err", err)
	return &toolError{codeInternal, "internal error"}
}

// --- get_default_lease / set_default_lease ---

type getDefaultLeaseIn struct{}

type setDefaultLeaseIn struct {
	// A pointer without omitempty: the key is required, and null is the explicit reset. An absent
	// key is refused by schema validation rather than read as a reset.
	DefaultLeaseTTLSeconds *int `json:"default_lease_ttl_seconds" jsonschema:"the new default lease in seconds, from 60 to 86400, or null to use the server default (300); applies to claim, claim_next and heartbeat calls that pass no lease_ttl_seconds, from the next call on"`
}

// defaultLeaseOut is the shape both lease verbs return.
type defaultLeaseOut struct {
	DefaultLeaseTTLSeconds   *int `json:"default_lease_ttl_seconds" jsonschema:"this endpoint's default lease in seconds, or null when it uses the server default"`
	EffectiveLeaseTTLSeconds int  `json:"effective_lease_ttl_seconds" jsonschema:"the lease in seconds a claim or heartbeat that passes no lease_ttl_seconds gets"`
	ServerDefaultSeconds     int  `json:"server_default_lease_ttl_seconds" jsonschema:"the server default lease in seconds, used when this endpoint sets none"`
	MinDefaultSeconds        int  `json:"min_default_lease_ttl_seconds" jsonschema:"the shortest default lease this endpoint may set, in seconds"`
	MaxSeconds               int  `json:"max_lease_ttl_seconds" jsonschema:"the longest lease, default or per-call, in seconds"`
}

func defaultLeaseResult(def *int) defaultLeaseOut {
	return defaultLeaseOut{
		DefaultLeaseTTLSeconds:   def,
		EffectiveLeaseTTLSeconds: lease.EffectiveSeconds(def),
		ServerDefaultSeconds:     lease.DefaultSeconds,
		MinDefaultSeconds:        lease.MinDefaultSeconds,
		MaxSeconds:               lease.MaxSeconds,
	}
}

// registerLeaseVerbs registers the endpoint's own lease read and write, each only when its verb is
// in the endpoint's allowlist.
func (h *Handler) registerLeaseVerbs(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "get_default_lease") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "get_default_lease",
			Description: "Read this endpoint's default lease: the lease in seconds a claim, claim_next or " +
				"heartbeat that passes no lease_ttl_seconds gets, whether it is this endpoint's own or the " +
				"server default, and the bounds a new default must fall within.",
		}, h.getDefaultLeaseTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "set_default_lease") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "set_default_lease",
			Description: "Set this endpoint's default lease, in seconds from " + strconv.Itoa(lease.MinDefaultSeconds) +
				" to " + maxLeaseSeconds + ", or null to go back to the server default (" +
				strconv.Itoa(lease.DefaultSeconds) + "). It applies to claim, claim_next and heartbeat calls " +
				"that pass no lease_ttl_seconds, from the next call on; leases already granted keep their " +
				"expiry. A value out of range is refused with invalid_argument, never clamped. It changes " +
				"this endpoint only, for every worker sharing its credential.",
		}, h.setDefaultLeaseTool(ep))
	}
}

func (h *Handler) getDefaultLeaseTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[getDefaultLeaseIn, defaultLeaseOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ getDefaultLeaseIn) (*sdk.CallToolResult, defaultLeaseOut, error) {
		def, err := h.store.EndpointDefaultLease(ctx, ep.ID)
		if err != nil {
			return nil, defaultLeaseOut{}, h.mapEndpointErr(ep, "get_default_lease", err)
		}
		return nil, defaultLeaseResult(def), nil
	}
}

func (h *Handler) setDefaultLeaseTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[setDefaultLeaseIn, defaultLeaseOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in setDefaultLeaseIn) (*sdk.CallToolResult, defaultLeaseOut, error) {
		// Always the authenticated endpoint's own id: the input names no endpoint, so this verb can
		// reach no other.
		stored, err := h.store.SetEndpointDefaultLease(ctx, ep.ID, in.DefaultLeaseTTLSeconds)
		if errors.Is(err, lease.ErrOutOfRange) {
			return nil, defaultLeaseOut{}, &toolError{codeInvalidArgument, err.Error()}
		}
		if err != nil {
			return nil, defaultLeaseOut{}, h.mapEndpointErr(ep, "set_default_lease", err)
		}
		h.log.Info("mcp default lease set", "slug", ep.Slug, "endpoint", ep.ID,
			"default_lease_ttl_seconds", logLease(stored))
		h.RefreshEndpointLease(ep.ID, stored)
		return nil, defaultLeaseResult(stored), nil
	}
}

// logLease renders a stored default for a log record: the seconds, or "default".
func logLease(def *int) string {
	if def == nil {
		return "default"
	}
	return strconv.Itoa(*def)
}

// RefreshEndpointLease re-registers claim, claim_next and heartbeat on every live session of the
// endpoint so their descriptions state the new default (the SDK sends tools/list_changed). Wired to
// the human API's PATCH and the web UI's lease form, and called by set_default_lease itself. The
// lease the verbs apply does not depend on it: they read the live default on every call.
func (h *Handler) RefreshEndpointLease(endpointID string, def *int) {
	h.mu.Lock()
	var live []*mcpSession
	for _, s := range h.sessions {
		if s.endpointID == endpointID && s.server != nil {
			live = append(live, s)
		}
	}
	h.mu.Unlock()
	for _, s := range live {
		ep := s.ep
		ep.DefaultLeaseTTLSeconds = def
		h.registerLeaseBoundTools(s.server, ep)
	}
}
