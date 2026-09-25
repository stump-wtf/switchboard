package mcp

// Trusted-actor verbs (ADR-0031, SPEC-0026 REQ-5): set_trusted_actors and clear_trusted_actors
// manage a webhook's first-class trust list, which the receiver evaluates in Go before any rule.
// They belong to the webhook self-management family (verbs.go), so grants, the vend wizard and
// consent treat them like the other webhook verbs. Like rotate and delete, they are scoped to the
// calling endpoint's own webhooks: another endpoint's id, an unknown id and a malformed id all answer
// not_found.
//
// The list is refused on sources whose body names no verifiable actor: generic (unsigned), stripe
// and slack. An omitted argument never clears: set_trusted_actors requires trusted_actors, and
// clearing is its own verb.
//
// Governing: ADR-0031, SPEC-0026 REQ-5 "Trusted Actors", REQ-13; SPEC-0006 REQ "Webhook
// Self-Management Within a Vended Ceiling".
//
// @joestump-agent 09/25/2026 - Added for #385.

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Warnings the trust verbs attach to their results.
const (
	trustEmptyWarning = "trusted_actors is empty: every delivery to this webhook will be quarantined " +
		"(recorded and routed nowhere) until trusted actors, or allow_all, are set with set_trusted_actors"
	trustAllowAllWarning = "allow_all trusts every verified sender: anyone who can make the producer send " +
		"(for a public repository, anyone who can open an issue) reaches your rules; prefer a list"
)

type setTrustedActorsIn struct {
	WebhookID     string                      `json:"webhook_id" jsonschema:"one of this endpoint's webhooks (github, gitea or cairn)"`
	TrustedActors *routing.TrustedActorsInput `json:"trusted_actors,omitempty" jsonschema:"REQUIRED: the complete trust list, replacing the current one: {logins, match} for github/gitea, {actor_ids} for cairn, or {allow_all: true}; to trust no one call clear_trusted_actors"`
}

type clearTrustedActorsIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"one of this endpoint's webhooks (github, gitea or cairn)"`
}

type trustedActorsOut struct {
	WebhookID     string                 `json:"webhook_id" jsonschema:"the webhook"`
	TrustedActors *routing.TrustedActors `json:"trusted_actors" jsonschema:"the stored trust list"`
	AllowAll      bool                   `json:"allow_all" jsonschema:"true when the webhook trusts every verified sender"`
	Warning       string                 `json:"warning,omitempty" jsonschema:"what this trust list means for deliveries, when it deserves attention"`
}

// registerTrustedActorTools installs the endpoint's allowlisted trust verbs.
func (h *Handler) registerTrustedActorTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "set_trusted_actors") {
		sdk.AddTool(srv, &sdk.Tool{Name: "set_trusted_actors",
			Description: "Replace a github, gitea or cairn webhook's trusted actors: who may start work through it. Deliveries from anyone else are held before any routing rule runs. github/gitea: {logins, match: sender|author|both} (logins compare case-insensitively); cairn: {actor_ids} (the signed actor_id; on_behalf_of never counts); or {allow_all: true} to trust every verified sender. trusted_actors is required."},
			h.setTrustedActorsTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "clear_trusted_actors") {
		sdk.AddTool(srv, &sdk.Tool{Name: "clear_trusted_actors",
			Description: "Reset a github, gitea or cairn webhook's trusted actors to an empty list, which trusts no one: every delivery is held until actors are set again."},
			h.clearTrustedActorsTool(ep))
	}
}

func (h *Handler) setTrustedActorsTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[setTrustedActorsIn, trustedActorsOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in setTrustedActorsIn) (*sdk.CallToolResult, trustedActorsOut, error) {
		id := strings.TrimSpace(in.WebhookID)
		if id == "" {
			return nil, trustedActorsOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		if in.TrustedActors == nil {
			// SPEC-0026 REQ-5 scenario "Omitted argument never clears".
			return nil, trustedActorsOut{}, &toolError{codeInvalidArgument,
				"trusted_actors is required; to trust no one, call clear_trusted_actors"}
		}
		w, err := h.store.WebhookForEndpoint(ctx, id, ep.ID)
		if err != nil {
			return nil, trustedActorsOut{}, h.mapWebhookErr(ep, "set_trusted_actors", err)
		}
		ta, err := routing.ParseTrustedActors(w.SourceType, *in.TrustedActors)
		if err != nil {
			return nil, trustedActorsOut{}, &toolError{codeInvalidArgument, err.Error()}
		}
		return h.storeTrustedActors(ctx, ep, "set_trusted_actors", w, ta)
	}
}

func (h *Handler) clearTrustedActorsTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[clearTrustedActorsIn, trustedActorsOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in clearTrustedActorsIn) (*sdk.CallToolResult, trustedActorsOut, error) {
		id := strings.TrimSpace(in.WebhookID)
		if id == "" {
			return nil, trustedActorsOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		w, err := h.store.WebhookForEndpoint(ctx, id, ep.ID)
		if err != nil {
			return nil, trustedActorsOut{}, h.mapWebhookErr(ep, "clear_trusted_actors", err)
		}
		empty, ok := routing.DefaultTrustedActors(w.SourceType)
		if !ok {
			_, err := routing.ParseTrustedActors(w.SourceType, routing.TrustedActorsInput{})
			return nil, trustedActorsOut{}, &toolError{codeInvalidArgument, err.Error()}
		}
		return h.storeTrustedActors(ctx, ep, "clear_trusted_actors", w, empty)
	}
}

func (h *Handler) storeTrustedActors(ctx context.Context, ep store.AuthEndpoint, tool string, w store.Webhook,
	ta routing.TrustedActors) (*sdk.CallToolResult, trustedActorsOut, error) {
	raw, err := json.Marshal(ta)
	if err != nil {
		return nil, trustedActorsOut{}, h.mapWebhookErr(ep, tool, err)
	}
	saved, err := h.store.SetWebhookTrustedActors(ctx, w.ID, ep.ID, raw)
	if err != nil {
		return nil, trustedActorsOut{}, h.mapWebhookErr(ep, tool, err)
	}
	stored, _ := routing.DecodeTrustedActors(saved.SourceType, saved.TrustedActors)
	return nil, trustedActorsOut{WebhookID: saved.ID, TrustedActors: &stored, AllowAll: stored.AllowAll,
		Warning: trustWarning(stored)}, nil
}

// trustWarning explains a trust list that deserves attention: empty (every delivery held) or
// allow_all (everyone trusted). A non-empty list needs no warning.
func trustWarning(ta routing.TrustedActors) string {
	switch {
	case ta.AllowAll:
		return trustAllowAllWarning
	case len(ta.Logins) == 0 && len(ta.ActorIDs) == 0:
		return trustEmptyWarning
	}
	return ""
}

// trustedActorsView decodes a stored trust list for display: nil for a source with no gate.
func trustedActorsView(w store.Webhook) *routing.TrustedActors {
	if !routing.HasActorProjection(w.SourceType) {
		return nil
	}
	ta, _ := routing.DecodeTrustedActors(w.SourceType, w.TrustedActors)
	return &ta
}
