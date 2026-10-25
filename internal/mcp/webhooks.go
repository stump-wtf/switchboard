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
	"log/slog"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/redact"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/sourcekind"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Stable machine error codes specific to the webhook ceiling (SPEC-0006 REQ "Structured Output and
// Stable Error Shape"). codeForbidden, codeNotFound, codeInternal are shared (tools.go);
// codeInvalidArgument is shared (events.go).
const (
	codeCeilingExceeded     = "ceiling_exceeded"
	codeForbiddenSourceType = "forbidden_source_type"
	// codeEncryptionRequired is SPEC-0032 REQ-12's sentinel in the stable error shape: no at-rest key is
	// configured, so a caller-supplied secret cannot be stored and nothing was written.
	codeEncryptionRequired = "encryption_required"
)

// trustModeSigned is the switchboard-derived trust mode for HMAC-signed providers (github/stripe/
// slack). Only a signed webhook mints/holds a signing secret and reveals it once; a token webhook is
// authenticated by the unguessable ingest URL and needs none.
const trustModeSigned = "signed"

// webhookVerbs is the SPEC-0006 webhook self-management surface. Like agentVerbs/eventVerbs, a
// tools/call naming one of these outside the endpoint's allowlist is a scope violation (stable
// "forbidden" code via scopeGuard), distinguishable from an unknown tool.
var webhookVerbs = verbSet(WebhookVerbs())

// webhookTrustModes is gone: its one column became SPEC-0032 REQ-1's per-source descriptor, which
// lives in internal/sourcekind so the MCP layer, the store and the delivery receiver read the same
// answer about who issues a secret. The mapping is still switchboard's alone: the agent never supplies
// a trust mode, so a self-created signed webhook can never be downgraded to token/open, and a source
// type outside the table is unsupported even if a ceiling names it.
// Governing: ADR-0012 (switchboard owns verification; no agent trust downgrade), ADR-0003 (per-source
// trust model), ADR-0024, ADR-0037, SPEC-0032 REQ-1.

// WebhookSourceTypes returns every source type create_webhook accepts — the selectable rows of the
// SPEC-0032 descriptor table — sorted, as a fresh slice the caller may keep. The vend wizard
// (internal/web) offers exactly these as its source-type chips, so a type made selectable in
// internal/sourcekind is pickable in the ceiling step without a second, drift-prone list.
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0015 REQ
// "Endpoints View And Vend Wizard", ADR-0012.
func WebhookSourceTypes() []string { return sourcekind.Selectable() }

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
	// SigningSecret is the producer's own signing secret, for a source whose deliveries it signs with a
	// value IT chose (stripe, slack). Write-only: no response ever carries it back. It is accepted only
	// where SPEC-0032 REQ-1 allows a supplied secret — a provider-origin type always, a minted-origin
	// type only once the operator sets SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET=true.
	// Governing: SPEC-0032 REQ-1, REQ-3.
	SigningSecret *string `json:"signing_secret,omitempty" jsonschema:"stripe/slack only: the producer's own signing secret, exactly as it displayed it (stripe's starts with whsec_); refused for a source whose secret switchboard mints unless the operator opted into supplied secrets"`
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
	SigningSecret string `json:"signing_secret,omitempty" jsonschema:"signed webhooks whose secret switchboard mints only: the HMAC signing secret, revealed once — paste it into the producer's webhook config; switchboard will not show it again"`
	// SecretOrigin says who issues this webhook's signing secret: minted (switchboard generates and
	// reveals it once) or provider (the producer signs with a value only it knows, which someone must
	// paste in). SecretSet is whether one is stored: false means the webhook is awaiting its secret and
	// every delivery to it answers 503. Fingerprint and SetAt describe the stored secret without being
	// it. Governing: SPEC-0032 REQ-2, REQ-3.
	SecretOrigin      string `json:"secret_origin" jsonschema:"who issues the signing secret: minted, provider, or none"`
	SecretSet         bool   `json:"secret_set" jsonschema:"false for a provider-origin webhook whose secret has not been set yet — it is awaiting_secret and refuses deliveries until it is"`
	SecretFingerprint string `json:"secret_fingerprint,omitempty" jsonschema:"first 12 hex of the stored signing secret's SHA-256; how to confirm which secret is in place without revealing it"`
	SecretSetAt       string `json:"secret_set_at,omitempty" jsonschema:"RFC 3339 time the current signing secret was written"`
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
	// The secret state, reported so an agent can tell "working" from "waiting for me to paste Stripe's
	// secret" without sending a test delivery. secret_supplied marks a secret that came from its caller
	// (a provider kind's normal state, or a minted kind under the operator's opt-in); secret_set_by
	// names who last wrote a supplied one. None of these is the secret. Governing: SPEC-0032 REQ-1,
	// REQ-2, REQ-13.
	SecretOrigin      string       `json:"secret_origin" jsonschema:"who issues the signing secret: minted, provider, or none"`
	SecretSet         bool         `json:"secret_set" jsonschema:"false for a provider-origin webhook awaiting its secret: deliveries answer 503 until it is set"`
	SecretSupplied    bool         `json:"secret_supplied" jsonschema:"true when the stored signing secret was supplied by its caller rather than minted by switchboard (a provider kind's normal state; a minted kind's only under SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET); rotate clears it"`
	SecretFingerprint string       `json:"secret_fingerprint,omitempty" jsonschema:"first 12 hex of the stored secret's SHA-256"`
	SecretSetAt       string       `json:"secret_set_at,omitempty" jsonschema:"RFC 3339 time the current secret was written"`
	SecretSetBy       *secretSetBy `json:"secret_set_by,omitempty" jsonschema:"who wrote the current secret, for a webhook whose secret a caller supplied (SPEC-0032 REQ-13)"`
	// The reconcile-read connection (#614): where this webhook's provider lives, and whether its
	// read-only credential is in place. Reported so an agent can tell a webhook that will never be
	// reconciled from one waiting for its owner to provision a credential (#615). The credential itself
	// is never included, and no agent surface writes any of it (ADR-0052).
	ProviderConnection *providerConnectionOut `json:"provider_connection,omitempty" jsonschema:"provider read configuration for this webhook, without its credential"`
}

// providerConnectionOut is the read-only view of a webhook's provider connection config
// (stump.wtf/switchboard#614). state is "unset" (no address configured), "awaiting_credential" (an
// address whose read credential has not been provisioned yet), or "ready".
type providerConnectionOut struct {
	BaseURL      string `json:"base_url,omitempty" jsonschema:"provider API root a reconcile read would call"`
	Repo         string `json:"repo,omitempty" jsonschema:"owner/name a reconcile read would list"`
	ReadTokenSet bool   `json:"read_token_set" jsonschema:"true when a read-only provider credential is stored; the credential itself is never returned"`
	State        string `json:"state" jsonschema:"unset, awaiting_credential, or ready"`
	SetAt        string `json:"set_at,omitempty" jsonschema:"RFC 3339 time the address or credential was last written"`
}

// secretSetBy is the actor half of an audit row, shaped for a list response: the kind and id of the
// endpoint or human whose write put the current supplied secret in place.
type secretSetBy struct {
	Kind string `json:"kind" jsonschema:"endpoint or human"`
	ID   string `json:"id" jsonschema:"the endpoint or human id"`
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
		// A debug line for the arguments this call arrived with, run through the field-name redaction so
		// a signing_secret can never reach a log line however this tool grows. Governing: SPEC-0032
		// REQ-3 (a secret appears in no log line, trace or span).
		if h.log.Enabled(ctx, slog.LevelDebug) {
			h.log.Debug("mcp create_webhook args", "slug", ep.Slug, "tool", "create_webhook",
				"args", redact.Secrets(map[string]any{
					"source_type": sourceType, "target_queue": targetQueue, "signing_secret": in.SigningSecret,
				}))
		}
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
		// Trust mode and secret origin are switchboard's to derive; an allowed-but-unsupported type
		// cannot be verified. A declared-but-not-yet-verifiable type (linear, plain) is refused rather
		// than created, because a webhook that cannot verify a delivery is worse than no webhook.
		kind, ok := sourcekind.Lookup(sourceType)
		if !ok || !kind.Selectable {
			return nil, webhookOut{}, &toolError{codeInvalidArgument, "source type " + sourceType + " is not supported"}
		}
		trustMode := kind.TrustMode
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

		// A secret the caller supplies is accepted only where SPEC-0032 REQ-1 says it may be: always for
		// a provider-origin source (Stripe and Slack sign with a secret only they issue), never for a
		// source authenticated by its URL, and for a minted-origin source only if the operator opted in.
		// Checked before anything is written or minted.
		var suppliedSecret string
		if in.SigningSecret != nil {
			suppliedSecret = strings.TrimSpace(*in.SigningSecret)
		}
		if suppliedSecret != "" {
			switch kind.Origin {
			case sourcekind.OriginNone:
				return nil, webhookOut{}, &toolError{codeInvalidArgument,
					"source type " + sourceType + " is authenticated by its ingest URL and takes no signing_secret"}
			case sourcekind.OriginMinted:
				if !h.allowSuppliedSecret.Load() {
					return nil, webhookOut{}, &toolError{codeInvalidArgument,
						"signing_secret is not accepted for source type " + sourceType +
							": switchboard mints its signing secret (an operator can allow supplied secrets with SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET=true)"}
				}
			}
			if err := sourcekind.CheckSecret(sourceType, suppliedSecret); err != nil {
				return nil, webhookOut{}, &toolError{codeInvalidArgument, err.Error()}
			}
		}

		// For a minted-origin signed webhook switchboard mints the HMAC signing secret and HOLDS it
		// server-side (it recomputes the HMAC over inbound bodies to verify per SPEC-0003) — and reveals
		// it to the agent exactly once below. A provider-origin webhook is created with NO secret:
		// minting one there would store a value the provider has never signed with, which looks like a
		// working webhook and fails every delivery with 401. A token source needs none either. Governing:
		// SPEC-0032 REQ-1, REQ-2.
		secret := ""
		if suppliedSecret == "" && kind.Origin == sourcekind.OriginMinted {
			var err error
			if secret, err = mintWebhookSecret(); err != nil {
				h.log.Error("mcp create_webhook mint secret", "slug", ep.Slug, "err", err)
				return nil, webhookOut{}, &toolError{codeInternal, "internal error"}
			}
		}
		ingestToken, err := mintIngestToken()
		if err != nil {
			h.log.Error("mcp create_webhook mint token", "slug", ep.Slug, "err", err)
			return nil, webhookOut{}, &toolError{codeInternal, "internal error"}
		}

		w, err := h.store.CreateWebhookWithOrigin(ctx, store.CreateWebhookInput{
			EndpointID: ep.ID, SourceType: sourceType, TargetQueue: targetQueue, TrustMode: trustMode,
			IngestToken: ingestToken, Secret: orSupplied(secret, suppliedSecret), Max: ep.WebhookMax,
			TrustedActors: trustRaw, SecretSupplied: suppliedSecret != "",
			// The audit row names the endpoint that wrote the secret and the surface it arrived on
			// (SPEC-0032 REQ-13). An endpoint's scope is immutable, so its id is the actor's id.
			Actor: store.SecretActor{Kind: store.SecretActorEndpoint, ID: ep.ID, Path: store.SecretPathMCP},
		})
		if err != nil {
			return nil, webhookOut{}, h.mapWebhookErr(ep, "create_webhook", err)
		}
		out := webhookOut{
			WebhookID: w.ID, IngestURL: h.ingestURL(w.IngestToken),
			SourceType: w.SourceType, TargetQueue: w.TargetQueue, TrustMode: w.TrustMode,
			TrustedActors: trustedActorsView(w),
		}
		fillSecretState(&out, w)
		if out.TrustedActors != nil {
			out.Warning = trustWarning(*out.TrustedActors)
			// Endpoint scope is immutable (SPEC-0007): an endpoint vended before the trust verbs
			// existed cannot call them, so the empty-list advice to use set_trusted_actors would be a
			// dead end. Say what does work. Governing: SPEC-0026 REQ-5.
			if out.Warning == trustEmptyWarning && !hasScope(ep.ScopeVerbs, "set_trusted_actors") {
				out.Warning += ". " + trustNoSetVerbWarning
			}
		}
		// Reveal the minted secret exactly once. A secret the caller supplied is never echoed back — it
		// arrived from that caller, and echoing a secret is how one ends up in a transcript — and a
		// provider-origin webhook simply has none yet. Every later read (list_webhooks) omits it.
		if suppliedSecret == "" && secret != "" {
			out.SigningSecret = secret
		}
		return nil, out, nil
	}
}

// fillSecretState copies the secret state a webhook row reports into a create/rotate result: who issues
// the secret, whether one is stored, its fingerprint and moment. Never a value. Governing: SPEC-0032
// REQ-2, REQ-3.
func fillSecretState(out *webhookOut, w store.Webhook) {
	out.SecretOrigin = w.SecretOrigin
	out.SecretSet = w.SecretSet
	out.SecretFingerprint = w.SecretFingerprint
	if w.SecretSetAt != nil {
		out.SecretSetAt = w.SecretSetAt.UTC().Format(time.RFC3339)
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
				// The secret state an agent needs to tell a working webhook from one still waiting for
				// its provider's secret. None of it is the secret. Governing: SPEC-0032 REQ-2, REQ-13.
				SecretOrigin: w.SecretOrigin, SecretSet: w.SecretSet, SecretSupplied: w.SecretSupplied,
				SecretFingerprint: w.SecretFingerprint,
			}
			if w.SecretSetAt != nil {
				row.SecretSetAt = w.SecretSetAt.UTC().Format(time.RFC3339)
			}
			if w.SecretSetByKind != "" {
				row.SecretSetBy = &secretSetBy{Kind: w.SecretSetByKind, ID: w.SecretSetByID}
			}
			row.AllowAll = row.TrustedActors != nil && row.TrustedActors.AllowAll
			if w.RotatedAt != nil {
				row.RotatedAt = w.RotatedAt.UTC().Format(time.RFC3339)
			}
			row.ProviderConnection = &providerConnectionOut{
				BaseURL:      w.ProviderBaseURL,
				Repo:         w.ProviderRepo,
				ReadTokenSet: w.ProviderReadTokenSet,
				State:        w.ProviderConnectionState(),
			}
			if w.ProviderConnectionSetAt != nil {
				row.ProviderConnection.SetAt = w.ProviderConnectionSetAt.UTC().Format(time.RFC3339)
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
		fillSecretState(&out, w)
		if w.SecretOrigin == sourcekind.OriginProvider {
			// Rotation replaced the URL only: the provider-issued secret stays, because minting a fresh
			// one there would swap a value the provider signs with for one it has never heard of.
			out.Warning = "provider-issued signing secret unchanged; rotate replaced the ingest URL only"
			return nil, out, nil
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
	case errors.Is(err, store.ErrEncryptionRequired):
		return &toolError{codeEncryptionRequired,
			"a supplied signing_secret needs SWITCHBOARD_SECRET_ENCRYPTION_KEY; nothing was stored"}
	case errors.Is(err, store.ErrInvalidArgument):
		// Reached only by a path that skipped the layer's own check; the detail stays in the log, and
		// the client is told which field failed without the value (a secret quoted in an error has
		// leaked through the response, the log and the trace at once).
		return &toolError{codeInvalidArgument, "the signing_secret is not usable for this source type"}
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

// orSupplied is the secret a row ends up holding: the caller's, when it supplied one, else the minted
// one (which is "" for a provider-origin webhook, and the store writes NULL for it).
func orSupplied(minted, supplied string) string {
	if supplied != "" {
		return supplied
	}
	return minted
}

// nonNil normalizes a nil slice to an empty one so the structured ceiling always serializes allowed
// lists as [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
