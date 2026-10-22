package server

// Human API: Creating, Rotating And Deleting Webhooks
//
// Three writes on a webhook a human owns, on /api/v1 beside the rule and route routes: create one for
// an endpoint the caller owns, rotate its ingest URL and signing secret, and delete it. SPEC-0035
// REQ "Webhook Management" already specified all three; this file is that slice. Until now the only
// way to make any of them was the owning endpoint's MCP credential with the matching verb, which left two
// real gaps: an endpoint vended before those verbs existed could never rotate a leaked secret, and a
// deployment that needed a router endpoint for per-stage routing could not finish it from a terminal
// (stump.wtf/switchboard#584).
//
// Both routes run internal/manage's lifecycle code with the calling human, so the ceiling an agent
// meets is the ceiling a human meets: source type within the endpoint's allowed types, target queue
// within its allowed queues, count within webhook_max under the endpoint's row lock, and the same rule
// for where a signing secret may come from. Nothing is reimplemented here, which is the point of the
// SPEC-0035 requirement that the two surfaces share one implementation: a check added for one surface cannot quietly skip the other.
//
// Reach is SPEC-0033's: an endpoint ref resolves only among the caller's own endpoints, and a webhook
// resolves only when its owning endpoint hangs off one of their agents. Unknown, malformed and
// out-of-reach are one not_found, so these routes enumerate nothing. A REFUSED WRITE LEAVES NOTHING:
// manage validates before it mints, and the store's insert is what enforces the count cap, so a 403
// or a 409 creates no row and orphans no secret.
//
// A secret travels once. A create's and a rotate's response carries the minted signing secret; no list,
// read or later response ever does (SPEC-0032 REQ-3). The CLI therefore writes it to a file the
// operator named rather than printing it, which is what lets a scripted vend stay out of the terminal,
// the scrollback and whatever ships terminal output.
//
// Governing: SPEC-0035 REQ "Webhook Management", REQ "Reach on Every Route", REQ "Shared Implementation
// With MCP", REQ "Error Handling Standards"; SPEC-0006 REQ "Webhook Self-Management Within a Vended
// Ceiling"; SPEC-0032 REQ-1, REQ-2, REQ-3, REQ-13; SPEC-0026 REQ-5; ADR-0012, ADR-0022.
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584: POST /endpoints/{ref}/webhooks,
// POST /webhooks/{webhook_id}/rotate and DELETE /webhooks/{webhook_id}.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/manage"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/sourcekind"
	"github.com/stump-wtf/switchboard/internal/store"
)

// createWebhookIn is POST /api/v1/endpoints/{ref}/webhooks' body, create_webhook's arguments.
// SigningSecret is a pointer so an absent key ("switchboard mints one") reads apart from "" (also
// mints) and from a value (the producer's own, where SPEC-0032 REQ-1 allows it). Every other field of
// a create response is ignored, so a document read from one is a valid body as it stands.
type createWebhookIn struct {
	SourceType    string                      `json:"source_type"`
	TargetQueue   string                      `json:"target_queue"`
	TrustedActors *routing.TrustedActorsInput `json:"trusted_actors,omitempty"`
	SigningSecret *string                     `json:"signing_secret,omitempty"`
}

// webhookOut is a create's or rotate's response: the row's metadata, plus the one-time secret, and the
// secret state that tells "working" from "waiting for someone to paste Stripe's secret". It is
// create_webhook and rotate_webhook's shape on purpose: an operator scripting either surface reads one
// set of names. Governing: SPEC-0032 REQ-2.
type webhookOut struct {
	WebhookID         string                 `json:"webhook_id"`
	EndpointID        string                 `json:"endpoint_id"`
	EndpointSlug      string                 `json:"endpoint_slug,omitempty"`
	IngestURL         string                 `json:"ingest_url"`
	SourceType        string                 `json:"source_type"`
	TargetQueue       string                 `json:"target_queue"`
	TrustMode         string                 `json:"trust_mode"`
	SigningSecret     string                 `json:"signing_secret,omitempty"`
	SecretOrigin      string                 `json:"secret_origin"`
	SecretSet         bool                   `json:"secret_set"`
	SecretFingerprint string                 `json:"secret_fingerprint,omitempty"`
	SecretSetAt       string                 `json:"secret_set_at,omitempty"`
	TrustedActors     *routing.TrustedActors `json:"trusted_actors,omitempty"`
	RotatedAt         string                 `json:"rotated_at,omitempty"`
	Warning           string                 `json:"warning,omitempty"`
}

// CreateWebhookForEndpoint is POST /api/v1/endpoints/{ref}/webhooks: create_webhook as the endpoint's
// human. The endpoint is named by slug or id and must be the caller's own; a revoked endpoint is 409,
// because minting a delivery URL nothing delivers to is the failure mode to avoid.
func (a *apiHandler) CreateWebhookForEndpoint(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	ref := strings.TrimSpace(chi.URLParam(r, "ref"))
	ceiling, err := a.st.EndpointCeilingForHuman(r.Context(), ref, human.ID)
	if err != nil {
		a.ceilingErr(w, r, "create webhook", ref, err)
		return
	}
	if ceiling.State != "active" {
		writeAPIError(w, http.StatusConflict, string(manage.ErrConflict),
			"the endpoint is not active, so a webhook on it would deliver nothing",
			map[string]any{"state": ceiling.State})
		return
	}
	var in createWebhookIn
	if !decodeAPIBody(w, r, &in) {
		return
	}
	created, err := a.webhooks.Create(r.Context(), manage.HumanPrincipal(human.ID),
		manage.Ceiling{
			EndpointID: ceiling.ID, EndpointState: ceiling.State, WebhookMax: ceiling.WebhookMax,
			SourceTypes: ceiling.SourceTypes, WebhookQueues: ceiling.WebhookQueues,
		},
		// The audit row names the human and the surface, not an endpoint: an endpoint's scope is immutable, so
		// naming the endpoint here would credit a credential that did not make the write. Governing:
		// SPEC-0032 REQ-13.
		store.SecretActor{Kind: store.SecretActorHuman, ID: human.ID, Path: store.SecretPathAPI},
		manage.CreateIn{
			SourceType: in.SourceType, TargetQueue: in.TargetQueue,
			TrustedActors: in.TrustedActors, SigningSecret: in.SigningSecret,
		})
	if err != nil {
		a.manageErr(w, r, "create webhook", created.Webhook.ID, err)
		return
	}
	a.log.Info("api webhook created", "human", human.ID, "webhook", created.Webhook.ID,
		"endpoint", ceiling.ID, "source_type", created.Webhook.SourceType, "surface", "api",
		"secret_origin", created.Webhook.SecretOrigin)
	writeJSON(w, http.StatusCreated, webhookOutFrom(created, ceiling.Slug, a.base))
}

// RotateWebhook is POST /api/v1/webhooks/{webhook_id}/rotate: rotate_webhook as the webhook's human.
// This is the route that turns "a secret leaked" back into a working webhook without the agent's
// credential, which is why it is reachable for a webhook on a revoked endpoint too.
func (a *apiHandler) RotateWebhook(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := strings.TrimSpace(chi.URLParam(r, "webhook_id"))
	owner, err := a.st.WebhookOwnerForHuman(r.Context(), id, human.ID)
	if err != nil {
		a.ceilingErr(w, r, "rotate webhook", id, err)
		return
	}
	created, err := a.webhooks.Rotate(r.Context(), manage.HumanPrincipal(human.ID), id)
	if err != nil {
		a.manageErr(w, r, "rotate webhook", id, err)
		return
	}
	a.log.Info("api webhook rotated", "human", human.ID, "webhook", id, "endpoint", owner.EndpointID,
		"surface", "api", "secret_origin", created.Webhook.SecretOrigin)
	out := webhookOutFrom(created, owner.EndpointSlug, a.base)
	if created.Webhook.SecretOrigin == sourcekind.OriginProvider && !created.Webhook.SecretSet {
		out.Warning = "provider-issued signing secret unchanged; rotate replaced the ingest URL only"
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteWebhook is DELETE /api/v1/webhooks/{webhook_id}: delete_webhook as the webhook's human. Its
// deliveries stop at once, and routes aimed at it go with it.
func (a *apiHandler) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	id := strings.TrimSpace(chi.URLParam(r, "webhook_id"))
	if _, err := a.st.WebhookOwnerForHuman(r.Context(), id, human.ID); err != nil {
		a.ceilingErr(w, r, "delete webhook", id, err)
		return
	}
	if err := a.webhooks.Delete(r.Context(), manage.HumanPrincipal(human.ID), id); err != nil {
		a.manageErr(w, r, "delete webhook", id, err)
		return
	}
	a.log.Info("api webhook deleted", "human", human.ID, "webhook", id, "surface", "api")
	writeJSON(w, http.StatusOK, map[string]any{"webhook_id": id, "deleted": true})
}

// webhookOutFrom projects a create or rotate result into the response. The secret rides along only when
// the layer minted one, and the ingest URL is rebuilt from the row's routing token: the row itself can
// not carry either, which is what makes an accidental later read safe. Governing: SPEC-0032 REQ-3.
func webhookOutFrom(created manage.Created, endpointSlug, base string) webhookOut {
	row := created.Webhook
	out := webhookOut{
		WebhookID: row.ID, EndpointID: row.EndpointID, EndpointSlug: endpointSlug,
		IngestURL:  base + "/webhooks/w/" + row.IngestToken,
		SourceType: row.SourceType, TargetQueue: row.TargetQueue, TrustMode: row.TrustMode,
		SigningSecret: created.Secret,
		SecretOrigin:  row.SecretOrigin, SecretSet: row.SecretSet, SecretFingerprint: row.SecretFingerprint,
	}
	if row.SecretSetAt != nil {
		out.SecretSetAt = row.SecretSetAt.UTC().Format(time.RFC3339)
	}
	if row.RotatedAt != nil {
		out.RotatedAt = row.RotatedAt.UTC().Format(time.RFC3339)
	}
	if actors, ok := routing.DecodeTrustedActors(row.SourceType, row.TrustedActors); ok {
		out.TrustedActors = &actors
	}
	return out
}

// ceilingErr maps a failed reach resolution (an endpoint ref or a webhook owner) onto SPEC-0035's error
// shape. An unknown, malformed or out-of-reach ref is one 404 that names nothing; anything else is an
// internal failure logged with the ref, which is safe to log because a ref is not a secret.
func (a *apiHandler) ceilingErr(w http.ResponseWriter, r *http.Request, what, ref string, err error) {
	human, _ := operatorFromContext(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "not_found",
			"no webhook or endpoint by that name or id belongs to you", nil)
		return
	}
	a.log.Error("api "+what+" resolution", "human", human.ID, "ref", ref, "err", err)
	writeAPIError(w, http.StatusInternalServerError, "internal", "internal error", nil)
}
