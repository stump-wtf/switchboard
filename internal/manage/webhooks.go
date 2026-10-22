package manage

// Webhook Lifecycle Management: Create, Rotate, Delete
//
// Three operations an owner performs on a webhook it holds: create one inside a vended ceiling, rotate
// its ingest URL and signing secret, and delete it. They lived only in internal/mcp's verb bodies, so
// the only way to do any of them was to hold the owning endpoint's credential, which meant a webhook
// owned by an endpoint whose credential nobody kept could never be rotated after a leak, and a router
// endpoint a deployment needed for per-stage routing could only be finished by hand in the browser.
// This file is the one implementation both surfaces run (SPEC-0035 REQ "Shared Implementation With
// MCP"): internal/mcp's create_webhook, rotate_webhook and delete_webhook verbs drive it with an
// endpoint principal and the ceiling off the authenticated endpoint row, and the human API drives it
// with a human principal and the ceiling read from the caller's own endpoint.
//
// What one implementation buys is the part that matters: the same ceiling checks, the same rule for
// where a signing secret may come from, the same minting, and the same store call under the same row
// lock. The surfaces differ in exactly two ways:
//
//   - REACH. An ENDPOINT principal acts on its own webhooks, and the store's WHERE clause
//     (id AND endpoint_id) is what enforces it: another endpoint's id is indistinguishable from an
//     unknown one, so neither verb is an existence oracle. A HUMAN principal acts on every webhook whose
//     owning endpoint hangs off one of their agents, resolved through store.WebhookOwnerForHuman, and
//     everything outside that reach is the same not_found.
//   - THE REVEAL. A create and a rotate mint a signing secret that exists exactly once, in the response.
//     This layer returns it as Created.Secret and never prints or logs it; each surface decides where
//     it goes (an MCP tool result field, an API JSON body, or a CLI file the caller named). A
//     surface that prints it to a terminal has not implemented the "revealed once" requirement: it has
//     printed a credential into a terminal, a scrollback and whatever log ships terminal output.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", REQ "Structured Output
// and Stable Error Shape"; SPEC-0035 REQ "Webhook Management", REQ "Shared Implementation With MCP",
// REQ "Reach on Every Route"; SPEC-0032 REQ-1, REQ-2, REQ-3, REQ-13; SPEC-0026 REQ-5, REQ-6;
// ADR-0012, ADR-0022.
//
// @joestump-agent 10/22/2026 - Extracted from internal/mcp/webhooks.go (the three verb bodies) and
// parameterized by a Principal and a Ceiling, so the human API runs the verbs' checks rather than a
// copy of them (stump.wtf/switchboard#584).
//
// @joestump-agent 10/22/2026 - Create refuses a non-active endpoint itself, so a future surface
// cannot mint on a revoked or paused endpoint by forgetting the handler-level check (#607 review).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/sourcekind"
	"github.com/stump-wtf/switchboard/internal/store"
)

// The three codes these operations answer with that the rule and route kinds do not cover. Their values
// are the codes the MCP verbs already answer with, so a script can treat both surfaces alike.
const (
	// ErrCeilingExceeded is the count cap: the endpoint already holds webhook_max webhooks. It is a
	// conflict, because the endpoint's state is what refuses the create.
	ErrCeilingExceeded Kind = "ceiling_exceeded"
	// ErrForbiddenSourceType names the source type specifically, not the grant generally: an agent told its
	// ceiling does not cover `gitea` should know that is the reason, because widening it is the fix.
	ErrForbiddenSourceType Kind = "forbidden_source_type"
	// ErrEncryptionRequired is SPEC-0032 REQ-12's sentinel: a caller-supplied secret arrived with no
	// at-rest key configured, so nothing was stored.
	ErrEncryptionRequired Kind = "encryption_required"
)

// WebhookStore is the store surface webhook lifecycle management needs: the three writes, plus the read
// a rotate needs to gate its reveal on the row's real trust mode.
type WebhookStore interface {
	CreateWebhookWithOrigin(ctx context.Context, in store.CreateWebhookInput) (store.Webhook, error)
	RotateWebhookSecret(ctx context.Context, id, endpointID, newSecret, newIngestToken string) (store.Webhook, error)
	DeleteWebhook(ctx context.Context, id, endpointID string) error
	WebhookForEndpoint(ctx context.Context, id, endpointID string) (store.Webhook, error)
}

// Webhooks runs webhook lifecycle management. Humans may be nil, exactly as in Routes: an endpoint
// principal never resolves through it, and a human principal resolves nothing without it.
type Webhooks struct {
	Store  WebhookStore
	Humans HumanReach
	// AllowSuppliedSecret reports whether the operator opted into accepting a caller-supplied signing
	// secret for a source whose secret switchboard would otherwise mint (SPEC-0032 REQ-1). It is a
	// function because the flag is read live, not captured at startup. Nil means "not allowed", the safe
	// answer for a surface that forgot to wire it.
	AllowSuppliedSecret func() bool
}

// Ceiling is the vend-time webhook ceiling a create runs against: the source types it may create, the
// queues those webhooks may route to, and how many the endpoint may hold. The MCP surface fills it
// from the authenticated endpoint row; the human API reads it from the endpoint the caller named, having
// first proved it belongs to that caller.
type Ceiling struct {
	EndpointID    string
	EndpointState string
	WebhookMax    int
	SourceTypes   []string
	WebhookQueues []string
}

// CreateIn is a create's input. SigningSecret is a pointer so "absent" (switchboard mints, where that
// is allowed) reads differently from "" (also mints) and from a value (the caller's own). TrustedActors
// is github, gitea and cairn only; omitted, the store writes the source's empty list, which trusts no
// one. Governing: SPEC-0026 REQ-5.
type CreateIn struct {
	SourceType    string
	TargetQueue   string
	TrustedActors *routing.TrustedActorsInput
	SigningSecret *string
}

// Created is a create's or a rotate's result: the stored row, plus Secret, the minted signing secret,
// which the row itself can never carry (store.Webhook holds metadata on purpose). Secret is "" when
// nothing was minted: a provider-origin webhook, a token source, or a create under a caller-supplied
// secret, none of which has a minted value to hand over.
type Created struct {
	Webhook store.Webhook
	Secret  string
}

// Create enforces the ceiling at the boundary: source-type membership, then the target-queue grant, then
// the count cap under the store's row lock, all before minting anything. It persists the minted secret
// server-side so inbound deliveries can be HMAC-verified. Every refusal happens before a write, so a
// create that fails leaves no row and no orphaned secret. Governing: SPEC-0006 REQ "Webhook
// Self-Management Within a Vended Ceiling", SPEC-0032 REQ-1, REQ-2.
func (w Webhooks) Create(ctx context.Context, p Principal, c Ceiling, actor store.SecretActor, in CreateIn) (Created, error) {
	sourceType := strings.TrimSpace(in.SourceType)
	targetQueue := strings.TrimSpace(in.TargetQueue)
	switch {
	case sourceType == "":
		return Created{}, fail(ErrInvalidArgument, "source_type is required")
	case targetQueue == "":
		return Created{}, fail(ErrInvalidArgument, "target_queue is required")
	}
	// SPEC-0026 REQ-6 scenario "Reserved name refused": a held delivery lands on quarantine by
	// switchboard's decision, never by being routed there.
	if store.CheckQueueNames(targetQueue) != nil {
		return Created{}, fail(ErrInvalidArgument, `target_queue "quarantine" is reserved for held deliveries`)
	}
	// The endpoint's state is part of the ceiling: a revoked or paused endpoint has no working delivery
	// target, so minting on it fails closed here rather than trusting every surface to remember the
	// handler-level check. Governing: SPEC-0035 REQ "Shared Implementation With MCP".
	if c.EndpointState != "active" {
		return Created{}, &Error{Kind: ErrConflict, State: c.EndpointState,
			Msg: "the endpoint is not active, so a webhook on it would deliver nothing"}
	}
	// The ceiling is the vended answer to "what may this endpoint create"; a type it never granted is
	// refused with its own code, because the fix (vend it wider) is a different one than a queue mistake.
	if !contains(c.SourceTypes, sourceType) {
		return Created{}, fail(ErrForbiddenSourceType, "source type "+sourceType+" is not in this endpoint's allowed source types")
	}
	// Trust mode and secret origin are switchboard's to derive, not the caller's to choose. A type that
	// is allowed but not verifiable here is refused rather than created: a webhook that cannot verify a
	// delivery is worse than no webhook.
	kind, ok := sourcekind.Lookup(sourceType)
	if !ok || !kind.Selectable {
		return Created{}, fail(ErrInvalidArgument, "source type "+sourceType+" is not supported")
	}
	if !contains(c.WebhookQueues, targetQueue) {
		return Created{}, fail(ErrForbidden, "target queue "+targetQueue+" is not in this endpoint's allowed webhook queues")
	}
	// The trust list, when given, is validated against the source before anything is minted; on a source
	// with no actor projection it is refused, saying why.
	var trustRaw []byte
	if in.TrustedActors != nil {
		ta, err := routing.ParseTrustedActors(sourceType, *in.TrustedActors)
		if err != nil {
			return Created{}, fail(ErrInvalidArgument, err.Error())
		}
		if trustRaw, err = json.Marshal(ta); err != nil {
			return Created{}, mapStoreErr(err, "create webhook")
		}
	}
	// A secret the caller supplies is accepted only where SPEC-0032 REQ-1 says it may be: always for a
	// provider-origin source, never for one authenticated by its URL, and for a minted-origin source only
	// when the operator opted in. Checked before anything is written.
	var supplied string
	if in.SigningSecret != nil {
		supplied = strings.TrimSpace(*in.SigningSecret)
	}
	if supplied != "" {
		switch kind.Origin {
		case sourcekind.OriginNone:
			return Created{}, fail(ErrInvalidArgument,
				"source type "+sourceType+" is authenticated by its ingest URL and takes no signing_secret")
		case sourcekind.OriginMinted:
			if !w.allowSuppliedSecret() {
				return Created{}, fail(ErrInvalidArgument,
					"signing_secret is not accepted for source type "+sourceType+
						": switchboard mints its signing secret (an operator can allow supplied secrets with SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET=true)")
			}
		}
		if err := sourcekind.CheckSecret(sourceType, supplied); err != nil {
			return Created{}, fail(ErrInvalidArgument, err.Error())
		}
	}
	// A minted-origin signed webhook gets a fresh HMAC secret that switchboard holds server-side and
	// reveals once. A provider-origin webhook is created with NO secret: minting one there would store a
	// value the provider has never signed with, which looks healthy and fails every delivery with 401.
	// A token source needs none either. Governing: SPEC-0032 REQ-1, REQ-2.
	var secret string
	if supplied == "" && kind.Origin == sourcekind.OriginMinted {
		var err error
		if secret, err = mintSigningSecret(); err != nil {
			return Created{}, err
		}
	}
	ingestToken, err := mintIngestToken()
	if err != nil {
		return Created{}, err
	}
	row, err := w.Store.CreateWebhookWithOrigin(ctx, store.CreateWebhookInput{
		EndpointID: c.EndpointID, SourceType: sourceType, TargetQueue: targetQueue, TrustMode: kind.TrustMode,
		IngestToken: ingestToken, Secret: orSupplied(secret, supplied), Max: c.WebhookMax,
		TrustedActors: trustRaw, SecretSupplied: supplied != "",
		Actor: actor,
	})
	if err != nil {
		return Created{}, mapStoreErr(err, "create webhook")
	}
	// A secret that arrived from the caller is never handed back: it came from that caller, and echoing
	// a secret is how one ends up in a transcript. Governing: SPEC-0032 REQ-3.
	return Created{Webhook: row, Secret: secret}, nil
}

// Rotate mints a fresh signing secret and ingest URL and retires the old ones, which is the point of
// it after a leak. The reveal is gated on the ROW's trust mode and secret origin rather than on the
// request, so a token webhook reveals nothing and a provider-origin webhook keeps the secret its producer
// actually signs with. Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and
// Idempotency"; SPEC-0032 REQ-2.
func (w Webhooks) Rotate(ctx context.Context, p Principal, webhookID string) (Created, error) {
	id, err := requireID(webhookID)
	if err != nil {
		return Created{}, err
	}
	endpointID, err := w.owner(ctx, p, id)
	if err != nil {
		return Created{}, err
	}
	secret, err := mintSigningSecret()
	if err != nil {
		return Created{}, err
	}
	ingestToken, err := mintIngestToken()
	if err != nil {
		return Created{}, err
	}
	// Ownership is enforced in the store's WHERE clause (id AND endpoint_id) for an endpoint principal; for
	// a human it was resolved above, and the same clause then scopes the write to that endpoint.
	row, err := w.Store.RotateWebhookSecret(ctx, id, endpointID, secret, ingestToken)
	if err != nil {
		return Created{}, mapStoreErr(err, "rotate webhook")
	}
	if row.SecretOrigin == sourcekind.OriginProvider {
		// Rotation replaced the URL only: the provider-issued secret stays, because minting a fresh one
		// there would swap a value the provider signs with for one it has never heard of.
		return Created{Webhook: row}, nil
	}
	if row.TrustMode != sourcekind.TrustSigned {
		return Created{Webhook: row}, nil
	}
	return Created{Webhook: row, Secret: secret}, nil
}

// Delete tears down one webhook in reach. Routes aimed at it go with it, which is why removing the
// last route first is never a prerequisite.
func (w Webhooks) Delete(ctx context.Context, p Principal, webhookID string) error {
	id, err := requireID(webhookID)
	if err != nil {
		return err
	}
	endpointID, err := w.owner(ctx, p, id)
	if err != nil {
		return err
	}
	if err := w.Store.DeleteWebhook(ctx, id, endpointID); err != nil {
		return mapStoreErr(err, "delete webhook")
	}
	return nil
}

// --- helpers ---

// owner resolves which endpoint the write is scoped to. For an endpoint principal that is the caller
// itself, and the store's WHERE clause is what refuses another endpoint's webhook without distinguishing
// it from an unknown one. For a human it is the endpoint that owns the webhook, which the reach read
// refuses to name outside the caller's own.
func (w Webhooks) owner(ctx context.Context, p Principal, webhookID string) (string, error) {
	if p.EndpointID != "" {
		return p.EndpointID, nil
	}
	if w.Humans == nil {
		return "", fail(ErrNotFound, "webhook not found")
	}
	owner, err := w.Humans.WebhookOwnerForHuman(ctx, webhookID, p.HumanID)
	if err != nil {
		return "", mapStoreErr(err, "resolve webhook owner")
	}
	return owner.EndpointID, nil
}

// allowSuppliedSecret reads the operator's opt-in, treating an unwired surface as "not allowed".
func (w Webhooks) allowSuppliedSecret() bool {
	return w.AllowSuppliedSecret != nil && w.AllowSuppliedSecret()
}

// mapStoreErr turns a store sentinel into the stable kind both surfaces answer with. Unexpected
// failures are returned wrapped for the caller's log and reach the caller as internal, because an
// unexpected message can quote a value the caller must not see.
func mapStoreErr(err error, what string) error {
	switch {
	case errors.Is(err, store.ErrCeilingExceeded):
		return fail(ErrCeilingExceeded, "webhook ceiling reached for this endpoint")
	case errors.Is(err, store.ErrNotFound):
		return fail(ErrNotFound, "webhook not found")
	case errors.Is(err, store.ErrReservedQueue):
		return fail(ErrInvalidArgument, `the queue name "quarantine" is reserved`)
	case errors.Is(err, store.ErrEncryptionRequired):
		return fail(ErrEncryptionRequired,
			"a supplied signing_secret needs SWITCHBOARD_SECRET_ENCRYPTION_KEY; nothing was stored")
	case errors.Is(err, store.ErrInvalidArgument):
		return fail(ErrInvalidArgument, "the signing_secret is not usable for this source type")
	default:
		return fmt.Errorf("manage: %s: %w", what, err)
	}
}

// mintIngestToken returns a high-entropy, URL-safe, non-secret token that routes inbound deliveries
// to exactly one webhook. It is not a credential (verification uses the separately-minted signing
// secret) but must be unguessable, so a delivery URL cannot be brute-forced.
func mintIngestToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("manage: mint ingest token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// mintSigningSecret returns 256 bits of CSPRNG entropy as a `whsec_`-prefixed HMAC signing secret,
// which the store holds in plaintext because it must recompute the provider HMAC over each inbound body
// (SPEC-0003), since a one-way hash could not. It is revealed exactly once, by the surface that calls
// this. Governing: SPEC-0032 REQ-2.
func mintSigningSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("manage: mint signing secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(b), nil
}

// orSupplied is the secret a row ends up holding: the caller's when it supplied one, else the minted
// one (which is "" for a provider-origin webhook, and the store writes NULL for it).
func orSupplied(minted, supplied string) string {
	if supplied != "" {
		return supplied
	}
	return minted
}

// contains is the ceiling's membership test, kept local so a nil granted list reads as "grants nothing"
// rather than as a wildcard.
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
