package mcp

// This file is the SPEC-0006 webhook self-management verb registry served over MCP: create_webhook,
// list_webhooks, rotate_webhook, delete_webhook. An agent stands up, rotates, and tears down its own
// ingestion webhooks — but strictly within the human-vended ceiling (max count, allowed source
// types, allowed target queues) carried on the endpoint. Switchboard DERIVES the trust mode from the
// source type; the agent supplies only the non-secret shape (source type + target queue). For a
// signed-type webhook switchboard MINTS the HMAC signing secret, HOLDS it server-side (so it can
// verify inbound deliveries per SPEC-0003), and REVEALS it to the agent exactly once in the create/
// rotate result — the agent pastes it into the producer (GitHub/Stripe/Slack). This is how
// self-management changes *who created* a webhook without ever changing *how it is verified* (no
// trust downgrade): a valid HMAC yields verified=true, identical to a human-configured signed webhook.
//
// Scope is enforced at the boundary, before any store mutation: verb allowlist (scopeGuard), then
// source-type membership, then target-queue grant, then the count ceiling (enforced atomically by
// the store to close the check-then-act race). Each ceiling violation returns its own stable code.
//
// Governing: ADR-0012 (agents self-manage webhooks within a vended ceiling),
// SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling",
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency",
// SPEC-0006 REQ "Structured Output and Stable Error Shape", ADR-0003 (per-source trust model).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Stable machine error codes specific to the webhook ceiling (SPEC-0006 REQ "Structured Output and
// Stable Error Shape"). codeForbidden, codeNotFound, codeInternal are shared (tools.go);
// codeInvalidArgument is shared (events.go).
const (
	codeCeilingExceeded     = "ceiling_exceeded"
	codeForbiddenSourceType = "forbidden_source_type"
)

// trustModeSigned is the switchboard-derived trust mode for HMAC-signed providers (github/stripe/
// slack). Only a signed webhook mints/holds a signing secret and reveals it once; a token webhook is
// authenticated by the unguessable ingest URL and needs none.
const trustModeSigned = "signed"

// webhookVerbs is the SPEC-0006 webhook self-management surface. Like agentVerbs/eventVerbs, a
// tools/call naming one of these outside the endpoint's allowlist is a scope violation (stable
// "forbidden" code via scopeGuard), distinguishable from an unknown tool.
var webhookVerbs = verbSet(WebhookVerbs())

// webhookTrustModes maps a source type to the trust mode switchboard verifies it under. The mapping
// is switchboard's alone: the agent never supplies a trust mode, so a self-created `signed` webhook
// (github/stripe/slack) can never be downgraded to token/open, and a `generic` webhook is token, not
// open (open is operator-only, out of reach of self-management). A source type outside this map is
// unsupported even if a ceiling names it. cairn is signed: its outbound webhooks carry an HMAC over
// the body and a signed event_id/created_at (cairn SPEC-0012; ingest/routing.go verifyCairn).
// Governing: ADR-0012 (switchboard owns verification; no agent trust downgrade), ADR-0003 (per-source
// trust model), ADR-0024.
var webhookTrustModes = map[string]string{
	"github":  "signed",
	"stripe":  "signed",
	"slack":   "signed",
	"gitea":   "signed",
	"cairn":   "signed",
	"generic": "token",
}

// WebhookSourceTypes returns every source type create_webhook accepts — the keys of
// webhookTrustModes — sorted, as a fresh slice the caller may keep. The vend wizard (internal/web)
// offers exactly these as its source-type chips, so a type added to webhookTrustModes is pickable
// in the ceiling step without a second, drift-prone list.
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0015 REQ
// "Endpoints View And Vend Wizard", ADR-0012.
func WebhookSourceTypes() []string { return slices.Sorted(maps.Keys(webhookTrustModes)) }

// SetBaseURL installs the externally-reachable origin used to build the ingest_url returned by
// create_webhook/rotate_webhook. Called at wiring time (internal/server) with cfg.BaseURL; safe to
// call concurrently with live sessions (atomic swap).
func (h *Handler) SetBaseURL(u string) { h.baseURL.Store(&u) }

// ingestURL builds the delivery URL an agent hands to its producer, from the webhook's non-secret
// ingest token. The token — not the webhook id — is what routes an inbound delivery to exactly one
// webhook, so the id is never exposed in a URL.
func (h *Handler) ingestURL(token string) string {
	base := ""
	if p := h.baseURL.Load(); p != nil {
		base = strings.TrimRight(*p, "/")
	}
	return base + "/webhooks/w/" + token
}

// --- tool input/output shapes (SDK-inferred JSON schemas) ---

type createWebhookIn struct {
	SourceType  string `json:"source_type" jsonschema:"the ingestion source type to create (must be within this endpoint's allowed source types)"`
	TargetQueue string `json:"target_queue" jsonschema:"the todo queue delivered events route to (must be within this endpoint's allowed webhook queues)"`
	// TrustedActors is who may start work through a github, gitea or cairn webhook (SPEC-0026
	// REQ-5). Omitted, the list is empty and trusts no one.
	TrustedActors *routing.TrustedActorsInput `json:"trusted_actors,omitempty" jsonschema:"github/gitea/cairn only: who may start work here ({logins, match} for github/gitea, {actor_ids} for cairn, or {allow_all: true}); omitted means an empty list, which holds every delivery until actors are set"`
}

// webhookOut is the create/rotate result: the ingest URL, trust mode, and — for a signed-type
// webhook — the minted signing secret revealed EXACTLY ONCE. Switchboard holds its own copy to verify
// inbound deliveries; this reveal is the only time the agent sees it, to paste into the producer
// (GitHub/Stripe/Slack). A token-type webhook needs no secret, so SigningSecret is omitted. Governing:
// SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency" (secret revealed once).
type webhookOut struct {
	WebhookID     string `json:"webhook_id" jsonschema:"the created/rotated webhook id"`
	IngestURL     string `json:"ingest_url" jsonschema:"the URL to hand to the producer; deliveries here become todos"`
	SourceType    string `json:"source_type" jsonschema:"the webhook's source type"`
	TargetQueue   string `json:"target_queue" jsonschema:"the queue delivered events route to"`
	TrustMode     string `json:"trust_mode" jsonschema:"trust mode switchboard verifies under: signed or token (derived, not agent-supplied)"`
	SigningSecret string `json:"signing_secret,omitempty" jsonschema:"signed webhooks only: the HMAC signing secret, revealed once — paste it into the producer's webhook config; switchboard will not show it again"`
	// Governing: SPEC-0026 REQ-5 (the create result says an empty list holds every delivery).
	TrustedActors *routing.TrustedActors `json:"trusted_actors,omitempty" jsonschema:"github/gitea/cairn only: the stored trust list"`
	Warning       string                 `json:"warning,omitempty" jsonschema:"what the trust list means for deliveries, when it deserves attention"`
}

type listWebhooksIn struct{}

// webhookMetaOut is one list row: metadata only, no secret. Governing: SPEC-0006 REQ "Webhook
// Self-Management Within a Vended Ceiling" (list returns metadata, no secret values).
type webhookMetaOut struct {
	WebhookID   string `json:"webhook_id" jsonschema:"the webhook id"`
	IngestURL   string `json:"ingest_url" jsonschema:"the delivery URL for this webhook"`
	SourceType  string `json:"source_type" jsonschema:"the webhook's source type"`
	TargetQueue string `json:"target_queue" jsonschema:"the queue delivered events route to"`
	TrustMode   string `json:"trust_mode" jsonschema:"trust mode: signed or token"`
	CreatedAt   string `json:"created_at" jsonschema:"RFC 3339 creation time"`
	RotatedAt   string `json:"rotated_at,omitempty" jsonschema:"RFC 3339 time of the last secret/URL rotation, if any"`
	// Governing: SPEC-0026 REQ-5 (list_webhooks echoes the field and flags allow_all).
	TrustedActors *routing.TrustedActors `json:"trusted_actors,omitempty" jsonschema:"github/gitea/cairn only: who may start work through this webhook"`
	AllowAll      bool                   `json:"allow_all" jsonschema:"true when this webhook trusts every verified sender (flagged: prefer a list)"`
	Quarantined   int                    `json:"quarantined" jsonschema:"open quarantine items this webhook's deliveries are waiting in (SPEC-0026)"`
}

// ceilingOut is the endpoint's vended webhook ceiling plus current usage, so an agent can see how
// much headroom it has. Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling"
// (list returns the ceiling: max, allowed source types, allowed queues, used).
type ceilingOut struct {
	Max                int      `json:"max" jsonschema:"maximum number of self-managed webhooks"`
	AllowedSourceTypes []string `json:"allowed_source_types" jsonschema:"source types this endpoint may create"`
	AllowedQueues      []string `json:"allowed_queues" jsonschema:"target queues created webhooks may route to"`
	Used               int      `json:"used" jsonschema:"webhooks currently created against this endpoint"`
}

type listWebhooksOut struct {
	Webhooks []webhookMetaOut `json:"webhooks" jsonschema:"this endpoint's self-managed webhooks, newest first"`
	Ceiling  ceilingOut       `json:"ceiling" jsonschema:"the vended ceiling and current usage"`
}

type rotateWebhookIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"the webhook id to rotate (mints a new secret and URL, retiring the old)"`
}

type deleteWebhookIn struct {
	WebhookID string `json:"webhook_id" jsonschema:"the webhook id to delete"`
}

type deleteWebhookOut struct {
	WebhookID string `json:"webhook_id" jsonschema:"the deleted webhook id"`
	Deleted   bool   `json:"deleted" jsonschema:"always true on success"`
}

// registerWebhookTools installs the endpoint's allowlisted SPEC-0006 webhook self-management verbs
// on the per-session server, mirroring the SPEC-0006 drain-verb registry (tools.go): tools/list
// advertises exactly the allowlist. Governing: SPEC-0006 REQ "Webhook Self-Management Within a
// Vended Ceiling", SPEC-0014 REQ "Agent Tool Surface over MCP" (scope filters the advertised tools).
func (h *Handler) registerWebhookTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "create_webhook") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "create_webhook",
			Description: "Create an ingestion webhook within this endpoint's ceiling. Returns the ingest URL and trust mode; for a signed-type webhook it also reveals the HMAC signing secret exactly once — paste it into the producer.",
		}, h.createWebhookTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "list_webhooks") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "list_webhooks",
			Description: "List this endpoint's self-managed webhooks and its vended ceiling. Never returns secret values.",
		}, h.listWebhooksTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "rotate_webhook") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "rotate_webhook",
			Description: "Rotate a webhook's signing secret and URL, retiring the old. Returns the new ingest URL; for a signed-type webhook it reveals the new signing secret exactly once.",
		}, h.rotateWebhookTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "delete_webhook") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "delete_webhook",
			Description: "Delete one of this endpoint's webhooks.",
		}, h.deleteWebhookTool(ep))
	}
}

// --- verb handlers ---

// createWebhookTool enforces the ceiling at the boundary — source-type membership, then target-queue
// grant, then (atomically in the store) the max-count cap — before minting the signing secret and
// persisting it server-side. For a signed-type webhook the minted secret is revealed exactly once in
// the result. Each violation returns its own stable code. Governing: SPEC-0006 REQ "Webhook
// Self-Management Within a Vended Ceiling", REQ "Switchboard Owns Secrets, Verification, and Idempotency".
func (h *Handler) createWebhookTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[createWebhookIn, webhookOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in createWebhookIn) (*sdk.CallToolResult, webhookOut, error) {
		sourceType := strings.TrimSpace(in.SourceType)
		targetQueue := strings.TrimSpace(in.TargetQueue)
		if sourceType == "" {
			return nil, webhookOut{}, &toolError{codeInvalidArgument, "source_type is required"}
		}
		if targetQueue == "" {
			return nil, webhookOut{}, &toolError{codeInvalidArgument, "target_queue is required"}
		}
		// SPEC-0026 REQ-6 scenario "Reserved name refused": held deliveries land on quarantine by
		// switchboard's decision, never by being routed there.
		if store.CheckQueueNames(targetQueue) != nil {
			return nil, webhookOut{}, &toolError{codeInvalidArgument, `target_queue "quarantine" is reserved for held deliveries`}
		}
		// Source type must be within the vended ceiling. A type the ceiling does not allow is refused
		// with the distinct forbidden_source_type code, never the generic forbidden.
		if !hasScope(ep.WebhookSourceTypes, sourceType) {
			return nil, webhookOut{}, &toolError{codeForbiddenSourceType, "source type " + sourceType + " is not in this endpoint's allowed source types"}
		}
		// Trust mode is switchboard's to derive; an allowed-but-unsupported type cannot be verified.
		trustMode, ok := webhookTrustModes[sourceType]
		if !ok {
			return nil, webhookOut{}, &toolError{codeInvalidArgument, "source type " + sourceType + " is not supported"}
		}
		// Target queue must be within the endpoint's webhook-queue grant.
		if !hasScope(ep.WebhookQueues, targetQueue) {
			return nil, webhookOut{}, &toolError{codeForbidden, "target queue " + targetQueue + " is not in this endpoint's allowed webhook queues"}
		}
		// The trust list, when given, is validated against the source before anything is minted. On a
		// source with no actor projection it is refused, saying why. Omitted, the store writes the
		// source's empty list (trusts no one). Governing: SPEC-0026 REQ-5.
		var trustRaw []byte
		if in.TrustedActors != nil {
			ta, err := routing.ParseTrustedActors(sourceType, *in.TrustedActors)
			if err != nil {
				return nil, webhookOut{}, &toolError{codeInvalidArgument, err.Error()}
			}
			if trustRaw, err = json.Marshal(ta); err != nil {
				return nil, webhookOut{}, h.mapWebhookErr(ep, "create_webhook", err)
			}
		}

		// For a signed-type webhook switchboard mints the HMAC signing secret and HOLDS it server-side
		// (it recomputes the HMAC over inbound bodies to verify per SPEC-0003) — and reveals it to the
		// agent exactly once below. A token-type webhook is authenticated by the unguessable ingest URL
		// and needs no secret. A minted, unique, non-secret token routes deliveries.
		secret, err := mintSecretFor(trustMode)
		if err != nil {
			h.log.Error("mcp create_webhook mint secret", "slug", ep.Slug, "err", err)
			return nil, webhookOut{}, &toolError{codeInternal, "internal error"}
		}
		ingestToken, err := mintIngestToken()
		if err != nil {
			h.log.Error("mcp create_webhook mint token", "slug", ep.Slug, "err", err)
			return nil, webhookOut{}, &toolError{codeInternal, "internal error"}
		}

		w, err := h.store.CreateWebhookWithTrust(ctx, ep.ID, sourceType, targetQueue, trustMode, ingestToken, secret, ep.WebhookMax, trustRaw)
		if err != nil {
			return nil, webhookOut{}, h.mapWebhookErr(ep, "create_webhook", err)
		}
		out := webhookOut{
			WebhookID: w.ID, IngestURL: h.ingestURL(w.IngestToken),
			SourceType: w.SourceType, TargetQueue: w.TargetQueue, TrustMode: w.TrustMode,
			TrustedActors: trustedActorsView(w),
		}
		if out.TrustedActors != nil {
			out.Warning = trustWarning(*out.TrustedActors)
			// Endpoint scope is immutable (SPEC-0007): an endpoint vended before the trust verbs
			// existed cannot call them, so the empty-list advice to use set_trusted_actors would be a
			// dead end. Say what does work. Governing: SPEC-0026 REQ-5.
			if out.Warning == trustEmptyWarning && !hasScope(ep.ScopeVerbs, "set_trusted_actors") {
				out.Warning += ". " + trustNoSetVerbWarning
			}
		}
		// Reveal the secret exactly once (signed only). It is returned solely here; every later read
		// (list_webhooks) omits it.
		if w.TrustMode == trustModeSigned {
			out.SigningSecret = secret
		}
		return nil, out, nil
	}
}

func (h *Handler) listWebhooksTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[listWebhooksIn, listWebhooksOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ listWebhooksIn) (*sdk.CallToolResult, listWebhooksOut, error) {
		ws, err := h.store.ListWebhooks(ctx, ep.ID)
		if err != nil {
			return nil, listWebhooksOut{}, h.mapWebhookErr(ep, "list_webhooks", err)
		}
		held, err := h.store.QuarantineCounts(ctx, ep.ID)
		if err != nil {
			return nil, listWebhooksOut{}, h.mapWebhookErr(ep, "list_webhooks", err)
		}
		out := listWebhooksOut{
			Webhooks: make([]webhookMetaOut, 0, len(ws)),
			Ceiling: ceilingOut{
				Max:                ep.WebhookMax,
				AllowedSourceTypes: nonNil(ep.WebhookSourceTypes),
				AllowedQueues:      nonNil(ep.WebhookQueues),
				Used:               len(ws),
			},
		}
		for _, w := range ws {
			row := webhookMetaOut{
				WebhookID: w.ID, IngestURL: h.ingestURL(w.IngestToken),
				SourceType: w.SourceType, TargetQueue: w.TargetQueue, TrustMode: w.TrustMode,
				CreatedAt:     w.CreatedAt.UTC().Format(time.RFC3339),
				TrustedActors: trustedActorsView(w),
				Quarantined:   held[w.ID],
			}
			row.AllowAll = row.TrustedActors != nil && row.TrustedActors.AllowAll
			if w.RotatedAt != nil {
				row.RotatedAt = w.RotatedAt.UTC().Format(time.RFC3339)
			}
			out.Webhooks = append(out.Webhooks, row)
		}
		return nil, out, nil
	}
}

// rotateWebhookTool mints a fresh signing secret and ingest token, retiring the old, for a webhook
// this endpoint owns. The new ingest URL is returned; for a signed-type webhook the new secret is
// revealed exactly once (the trust mode is fixed by the source type at create, so a signed webhook
// stays signed across rotation). Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification,
// and Idempotency" (rotate mints a new secret, retires old).
func (h *Handler) rotateWebhookTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[rotateWebhookIn, webhookOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in rotateWebhookIn) (*sdk.CallToolResult, webhookOut, error) {
		id := strings.TrimSpace(in.WebhookID)
		if id == "" {
			return nil, webhookOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		// Mint a fresh secret; the store persists it and returns the row (with the switchboard-derived
		// trust mode) so the reveal below is gated on the webhook's actual trust mode. A token webhook's
		// stored secret stays NULL because the store NULLIFs an empty string.
		secret, err := mintWebhookSecret()
		if err != nil {
			h.log.Error("mcp rotate_webhook mint secret", "slug", ep.Slug, "err", err)
			return nil, webhookOut{}, &toolError{codeInternal, "internal error"}
		}
		ingestToken, err := mintIngestToken()
		if err != nil {
			h.log.Error("mcp rotate_webhook mint token", "slug", ep.Slug, "err", err)
			return nil, webhookOut{}, &toolError{codeInternal, "internal error"}
		}
		// Ownership is enforced in the store's WHERE clause (id AND endpoint_id): another endpoint's
		// id is indistinguishable from an unknown one — both return not_found.
		w, err := h.store.RotateWebhookSecret(ctx, id, ep.ID, secret, ingestToken)
		if err != nil {
			return nil, webhookOut{}, h.mapWebhookErr(ep, "rotate_webhook", err)
		}
		out := webhookOut{
			WebhookID: w.ID, IngestURL: h.ingestURL(w.IngestToken),
			SourceType: w.SourceType, TargetQueue: w.TargetQueue, TrustMode: w.TrustMode,
		}
		if w.TrustMode == trustModeSigned {
			out.SigningSecret = secret
		}
		return nil, out, nil
	}
}

func (h *Handler) deleteWebhookTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[deleteWebhookIn, deleteWebhookOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deleteWebhookIn) (*sdk.CallToolResult, deleteWebhookOut, error) {
		id := strings.TrimSpace(in.WebhookID)
		if id == "" {
			return nil, deleteWebhookOut{}, &toolError{codeInvalidArgument, "webhook_id is required"}
		}
		if err := h.store.DeleteWebhook(ctx, id, ep.ID); err != nil {
			return nil, deleteWebhookOut{}, h.mapWebhookErr(ep, "delete_webhook", err)
		}
		return nil, deleteWebhookOut{WebhookID: id, Deleted: true}, nil
	}
}

// mapWebhookErr maps store sentinels onto the stable SPEC-0006 codes for the webhook verbs.
// ErrCeilingExceeded is the count-cap violation (distinct from the boundary source-type/queue
// codes); ErrNotFound is an unknown/not-owned webhook. Unexpected failures reach the client only as
// "internal" while the log records slug, verb, and the wrapped error — never a secret (none enters
// this layer). Governing: SPEC-0006 REQ "Error Handling Standards".
func (h *Handler) mapWebhookErr(ep store.AuthEndpoint, tool string, err error) error {
	switch {
	case errors.Is(err, store.ErrCeilingExceeded):
		return &toolError{codeCeilingExceeded, "webhook ceiling reached for this endpoint"}
	case errors.Is(err, store.ErrNotFound):
		return &toolError{codeNotFound, "webhook not found"}
	case errors.Is(err, store.ErrReservedQueue):
		return &toolError{codeInvalidArgument, `the queue name "quarantine" is reserved`}
	default:
		h.log.Error("mcp webhook store failure", "slug", ep.Slug, "tool", tool,
			"err", fmt.Errorf("tools/call %s: %w", tool, err))
		return &toolError{codeInternal, "internal error"}
	}
}

// --- helpers ---

// mintIngestToken returns a high-entropy, URL-safe, non-secret token that routes inbound deliveries
// to exactly one webhook. It is not a credential (verification uses the separately-minted signing
// secret), but it must be unguessable so a delivery URL cannot be brute-forced.
func mintIngestToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mcp: mint ingest token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// mintWebhookSecret returns a high-entropy CSPRNG HMAC signing secret switchboard holds server-side
// and reveals to the agent exactly once. Unlike a vended credential (cred.Mint, stored hashed) this
// secret is stored in plaintext because switchboard must recompute the provider HMAC over each
// inbound body to verify it per SPEC-0003 — a one-way hash could not. 256 bits of entropy; the
// `whsec_` prefix mirrors the provider convention for a webhook signing secret.
func mintWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mcp: mint webhook secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(b), nil
}

// mintSecretFor mints a signing secret only for a signed-type webhook; a token/open webhook is
// authenticated by the unguessable ingest URL and needs none, so it returns "" (which the store
// persists as NULL). Callers reveal the returned secret only when it is non-empty.
func mintSecretFor(trustMode string) (string, error) {
	if trustMode != trustModeSigned {
		return "", nil
	}
	return mintWebhookSecret()
}

// nonNil normalizes a nil slice to an empty one so the structured ceiling always serializes allowed
// lists as [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
