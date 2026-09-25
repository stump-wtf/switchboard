package mcp

// This file is the SPEC-0024 notify-hook verb registry served over MCP: create_notify_hook,
// list_notify_hooks, rotate_notify_hook, delete_notify_hook. An endpoint registers outbound HTTPS
// URLs that Switchboard will POST a signed, payload-free notification to when a push-eligible todo
// it owns becomes ready (the dispatcher is a separate story; these verbs only manage the hooks).
//
// Boundary order, before any store write: the verb allowlist (scopeGuard), then argument shape,
// then the queues ⊆ scope check, then the operator ceiling (0 = off, no DNS lookup made), then the
// SSRF guard (notifyhook.ValidateURL, which resolves the host through the shared push.Validator),
// then the store (which enforces the ceiling atomically and seals the minted secret).
//
// Secrets and URLs: the signing secret is minted here (notifyhook.MintSecret, padded standard
// base64; never the inbound hex minter) and revealed only in the create and rotate results. URLs
// leave this layer only through notifyhook.RedactURL, so no response carries a query string.
//
// Governing: ADR-0029, SPEC-0024 REQ-2 "Management Verbs", REQ-3 "Target Validation (SSRF Guard)",
// REQ-4 "Signing", REQ-11 (no query string in any response), SPEC-0006 REQ "Structured Output and
// Stable Error Shape".

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/notifyhook"
	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

// notifyHookVerbs is the SPEC-0024 surface; scopeGuard answers an ungranted one with forbidden.
var notifyHookVerbs = verbSet(NotifyHookVerbs())

// hookIDShape is the uuid form a hook id takes. Anything else cannot name a hook, and is answered
// exactly like an unknown id (not_found) rather than reaching the store as a malformed uuid.
var hookIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NotifyHookStore is the store slice the notify-hook verbs use. Every method is endpoint-scoped: a
// hook id another endpoint owns is store.ErrNotFound. *store.Store satisfies it.
type NotifyHookStore interface {
	CreateNotifyHook(ctx context.Context, endpointID, url string, queues []string, ignorePresence bool, secret string, max int) (store.NotifyHook, error)
	ListNotifyHooks(ctx context.Context, endpointID string) ([]store.NotifyHook, error)
	RotateNotifyHookSecret(ctx context.Context, id, endpointID, newSecret string, grace time.Duration) (store.NotifyHook, error)
	DeleteNotifyHook(ctx context.Context, id, endpointID string) error
}

// NotifyHookConfig is what the verbs need from wiring: the store, the shared SSRF validator (built
// with the operator's http opt-in, CIDR allowlist and own listen address), and the per-endpoint
// ceiling (SWITCHBOARD_NOTIFY_HOOK_MAX; 0 refuses every create).
type NotifyHookConfig struct {
	Store     NotifyHookStore
	Validator *push.Validator
	Max       int
}

// SetNotifyHooks installs the notify-hook dependencies. Until it is called, a granted notify-hook
// verb answers unavailable. Safe to call concurrently with live sessions.
func (h *Handler) SetNotifyHooks(cfg NotifyHookConfig) { h.notifyHooks.Store(&cfg) }

func (h *Handler) notifyHookDeps() (NotifyHookConfig, error) {
	if p := h.notifyHooks.Load(); p != nil && p.Store != nil && p.Validator != nil {
		return *p, nil
	}
	return NotifyHookConfig{}, &toolError{codeUnavailable, "notify hooks are not enabled on this instance"}
}

// --- tool input/output shapes ---

type createNotifyHookIn struct {
	URL            string   `json:"url" jsonschema:"the https URL Switchboard will POST signed notifications to"`
	Queues         []string `json:"queues,omitempty" jsonschema:"optional: only notify for these queues (each must be in this endpoint's scope); empty means every queue the scope grants"`
	IgnorePresence bool     `json:"ignore_presence,omitempty" jsonschema:"optional: fire even while the endpoint is clocked out (for an on-demand dispatcher)"`
}

type createNotifyHookOut struct {
	HookID         string   `json:"hook_id" jsonschema:"the created hook id"`
	URL            string   `json:"url" jsonschema:"the hook URL, with any query string redacted"`
	Queues         []string `json:"queues" jsonschema:"the hook's queue filter; empty means every queue the scope grants"`
	IgnorePresence bool     `json:"ignore_presence" jsonschema:"whether the hook fires while the endpoint is clocked out"`
	Enabled        bool     `json:"enabled" jsonschema:"whether the hook is enabled"`
	SigningSecret  string   `json:"signing_secret" jsonschema:"the Standard Webhooks signing secret (whsec_ + base64), revealed once: configure the receiver with it; Switchboard will not show it again"`
}

type listNotifyHooksIn struct{}

type notifyHookOut struct {
	HookID              string   `json:"hook_id" jsonschema:"the hook id"`
	URL                 string   `json:"url" jsonschema:"the hook URL, with any query string redacted"`
	Queues              []string `json:"queues" jsonschema:"the hook's queue filter; empty means every queue the scope grants"`
	IgnorePresence      bool     `json:"ignore_presence" jsonschema:"whether the hook fires while the endpoint is clocked out"`
	Enabled             bool     `json:"enabled" jsonschema:"whether the hook is enabled"`
	DisabledReason      *string  `json:"disabled_reason" jsonschema:"why the hook is disabled: consecutive_failures or operator; null when enabled"`
	ConsecutiveFailures int      `json:"consecutive_failures" jsonschema:"failed deliveries in a row"`
	LastStatus          *int     `json:"last_status" jsonschema:"HTTP status of the last delivery, or null"`
	LastError           *string  `json:"last_error" jsonschema:"classified reason for the last failure, or null"`
	LastAttemptAt       *string  `json:"last_attempt_at" jsonschema:"RFC 3339 time of the last delivery attempt, or null"`
	CreatedAt           string   `json:"created_at" jsonschema:"RFC 3339 creation time"`
	RotatedAt           *string  `json:"rotated_at" jsonschema:"RFC 3339 time of the last secret rotation, or null"`
}

type notifyHookCeilingOut struct {
	Max  int `json:"max" jsonschema:"the per-endpoint hook ceiling"`
	Used int `json:"used" jsonschema:"hooks this endpoint has"`
}

type listNotifyHooksOut struct {
	Hooks   []notifyHookOut      `json:"hooks" jsonschema:"this endpoint's notify hooks with their health; never any secret"`
	Ceiling notifyHookCeilingOut `json:"ceiling" jsonschema:"the ceiling and current usage"`
}

type rotateNotifyHookIn struct {
	HookID string `json:"hook_id" jsonschema:"the hook to rotate"`
}

type rotateNotifyHookOut struct {
	HookID                   string `json:"hook_id" jsonschema:"the rotated hook id"`
	SigningSecret            string `json:"signing_secret" jsonschema:"the new signing secret, revealed once"`
	PreviousSecretValidUntil string `json:"previous_secret_valid_until" jsonschema:"RFC 3339 time until which every notification is also signed with the previous secret"`
	Enabled                  bool   `json:"enabled" jsonschema:"whether the hook is enabled (rotation re-enables a hook auto-disabled by failures)"`
}

type deleteNotifyHookIn struct {
	HookID string `json:"hook_id" jsonschema:"the hook to delete"`
}

type deleteNotifyHookOut struct {
	HookID  string `json:"hook_id" jsonschema:"the deleted hook id"`
	Deleted bool   `json:"deleted" jsonschema:"always true on success"`
}

// registerNotifyHookTools installs the endpoint's allowlisted notify-hook verbs; tools/list
// advertises exactly the allowlist. Governing: SPEC-0024 REQ-2, SPEC-0014 REQ "Agent Tool Surface
// over MCP".
func (h *Handler) registerNotifyHookTools(srv *sdk.Server, ep store.AuthEndpoint) {
	if hasScope(ep.ScopeVerbs, "create_notify_hook") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "create_notify_hook",
			Description: "Register an https URL that Switchboard POSTs a signed (Standard Webhooks) notification to when a todo " +
				"for this endpoint becomes ready. The body carries ids and a one-line summary, never the payload: claim the todo " +
				"to get the work. Returns the signing secret exactly once.",
		}, h.createNotifyHookTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "list_notify_hooks") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "list_notify_hooks",
			Description: "List this endpoint's notify hooks with their delivery health and the ceiling. Never returns a secret.",
		}, h.listNotifyHooksTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "rotate_notify_hook") {
		sdk.AddTool(srv, &sdk.Tool{
			Name: "rotate_notify_hook",
			Description: "Mint a new signing secret for a notify hook, returned once. For 24 hours every notification is signed " +
				"with both the new and the previous secret. Re-enables a hook that was disabled after repeated failures.",
		}, h.rotateNotifyHookTool(ep))
	}
	if hasScope(ep.ScopeVerbs, "delete_notify_hook") {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "delete_notify_hook",
			Description: "Delete one of this endpoint's notify hooks and its secrets.",
		}, h.deleteNotifyHookTool(ep))
	}
}

func (h *Handler) createNotifyHookTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[createNotifyHookIn, createNotifyHookOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in createNotifyHookIn) (*sdk.CallToolResult, createNotifyHookOut, error) {
		deps, err := h.notifyHookDeps()
		if err != nil {
			return nil, createNotifyHookOut{}, err
		}
		queues, err := hookQueues(ep, in.Queues)
		if err != nil {
			return nil, createNotifyHookOut{}, err
		}
		// The kill switch answers before any lookup: with the ceiling at 0 no create can succeed, so
		// the URL's host is not even resolved.
		if deps.Max <= 0 {
			return nil, createNotifyHookOut{}, &toolError{codeCeilingExceeded, "notify hooks are disabled on this instance (ceiling 0)"}
		}
		target, err := notifyhook.ValidateURL(ctx, deps.Validator, in.URL)
		if err != nil {
			return nil, createNotifyHookOut{}, &toolError{codeInvalidArgument, hookURLRejection(err)}
		}
		secret, err := notifyhook.MintSecret()
		if err != nil {
			h.log.Error("mcp create_notify_hook mint secret", "slug", ep.Slug, "err", err)
			return nil, createNotifyHookOut{}, &toolError{codeInternal, "internal error"}
		}
		hook, err := deps.Store.CreateNotifyHook(ctx, ep.ID, target.URL.String(), queues, in.IgnorePresence, secret, deps.Max)
		if err != nil {
			return nil, createNotifyHookOut{}, h.mapNotifyHookErr(ep, "create_notify_hook", "", err)
		}
		return nil, createNotifyHookOut{
			HookID: hook.ID, URL: notifyhook.RedactURL(hook.URL), Queues: nonNil(hook.Queues),
			IgnorePresence: hook.IgnorePresence, Enabled: hook.Enabled, SigningSecret: secret,
		}, nil
	}
}

func (h *Handler) listNotifyHooksTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[listNotifyHooksIn, listNotifyHooksOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, _ listNotifyHooksIn) (*sdk.CallToolResult, listNotifyHooksOut, error) {
		deps, err := h.notifyHookDeps()
		if err != nil {
			return nil, listNotifyHooksOut{}, err
		}
		hooks, err := deps.Store.ListNotifyHooks(ctx, ep.ID)
		if err != nil {
			return nil, listNotifyHooksOut{}, h.mapNotifyHookErr(ep, "list_notify_hooks", "", err)
		}
		out := listNotifyHooksOut{
			Hooks:   make([]notifyHookOut, 0, len(hooks)),
			Ceiling: notifyHookCeilingOut{Max: max(deps.Max, 0), Used: len(hooks)},
		}
		for _, hk := range hooks {
			out.Hooks = append(out.Hooks, notifyHookOut{
				HookID: hk.ID, URL: notifyhook.RedactURL(hk.URL), Queues: nonNil(hk.Queues),
				IgnorePresence: hk.IgnorePresence, Enabled: hk.Enabled, DisabledReason: hk.DisabledReason,
				ConsecutiveFailures: hk.ConsecutiveFailures, LastStatus: hk.LastStatus, LastError: hk.LastError,
				LastAttemptAt: rfc3339(hk.LastAttemptAt), CreatedAt: hk.CreatedAt.UTC().Format(time.RFC3339),
				RotatedAt: rfc3339(hk.RotatedAt),
			})
		}
		return nil, out, nil
	}
}

func (h *Handler) rotateNotifyHookTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[rotateNotifyHookIn, rotateNotifyHookOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in rotateNotifyHookIn) (*sdk.CallToolResult, rotateNotifyHookOut, error) {
		deps, err := h.notifyHookDeps()
		if err != nil {
			return nil, rotateNotifyHookOut{}, err
		}
		id, err := hookID(in.HookID)
		if err != nil {
			return nil, rotateNotifyHookOut{}, err
		}
		secret, err := notifyhook.MintSecret()
		if err != nil {
			h.log.Error("mcp rotate_notify_hook mint secret", "slug", ep.Slug, "hook", id, "err", err)
			return nil, rotateNotifyHookOut{}, &toolError{codeInternal, "internal error"}
		}
		hook, err := deps.Store.RotateNotifyHookSecret(ctx, id, ep.ID, secret, notifyhook.RotationGrace)
		if err != nil {
			return nil, rotateNotifyHookOut{}, h.mapNotifyHookErr(ep, "rotate_notify_hook", id, err)
		}
		out := rotateNotifyHookOut{HookID: hook.ID, SigningSecret: secret, Enabled: hook.Enabled}
		if hook.PrevSecretExpiresAt != nil {
			out.PreviousSecretValidUntil = hook.PrevSecretExpiresAt.UTC().Format(time.RFC3339)
		}
		return nil, out, nil
	}
}

func (h *Handler) deleteNotifyHookTool(ep store.AuthEndpoint) sdk.ToolHandlerFor[deleteNotifyHookIn, deleteNotifyHookOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in deleteNotifyHookIn) (*sdk.CallToolResult, deleteNotifyHookOut, error) {
		deps, err := h.notifyHookDeps()
		if err != nil {
			return nil, deleteNotifyHookOut{}, err
		}
		id, err := hookID(in.HookID)
		if err != nil {
			return nil, deleteNotifyHookOut{}, err
		}
		if err := deps.Store.DeleteNotifyHook(ctx, id, ep.ID); err != nil {
			return nil, deleteNotifyHookOut{}, h.mapNotifyHookErr(ep, "delete_notify_hook", id, err)
		}
		return nil, deleteNotifyHookOut{HookID: id, Deleted: true}, nil
	}
}

// --- helpers ---

// hookQueues validates a create's queue filter: trimmed, de-duplicated, and each one inside the
// endpoint's queue scope. A queue outside the scope is forbidden, as for every other verb.
func hookQueues(ep store.AuthEndpoint, in []string) ([]string, error) {
	out := []string{}
	for _, q := range in {
		q = strings.TrimSpace(q)
		if q == "" {
			return nil, &toolError{codeInvalidArgument, "queues must not contain an empty name"}
		}
		if !hasScope(ep.ScopeQueues, q) {
			return nil, &toolError{codeForbidden, "queue " + q + " is not in this endpoint's scope"}
		}
		if !slices.Contains(out, q) {
			out = append(out, q)
		}
	}
	return out, nil
}

// hookID validates a hook id argument. A value that cannot be a hook id is not_found, exactly like
// an id that names no hook this endpoint owns (SPEC-0024 REQ-2).
func hookID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", &toolError{codeInvalidArgument, "hook_id is required"}
	}
	if !hookIDShape.MatchString(id) {
		return "", &toolError{codeNotFound, "notify hook not found"}
	}
	return strings.ToLower(id), nil
}

// hookURLRejection is the invalid_argument message for a refused URL. The validator's messages name
// the rule and the address class, never the query string (ValidateURL parses first and reports a
// parse failure generically), so they are safe to return; the sentinel prefixes are trimmed.
func hookURLRejection(err error) string {
	msg := err.Error()
	for _, prefix := range []string{push.ErrValidation.Error() + ": ", notifyhook.ErrInvalidURL.Error() + ": "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return "url refused: " + msg
}

// mapNotifyHookErr maps store sentinels onto the stable codes. Unexpected failures reach the client
// only as internal, while the log records the slug, verb, hook id and wrapped error (no secret or
// URL enters this layer's errors).
func (h *Handler) mapNotifyHookErr(ep store.AuthEndpoint, tool, hookID string, err error) error {
	switch {
	case errors.Is(err, store.ErrCeilingExceeded):
		return &toolError{codeCeilingExceeded, "notify hook ceiling reached for this endpoint"}
	case errors.Is(err, store.ErrNotFound):
		return &toolError{codeNotFound, "notify hook not found"}
	case errors.Is(err, store.ErrSecretCipherRequired):
		h.log.Warn("notify hook refused: no secret encryption key", "slug", ep.Slug, "tool", tool,
			"fix", "set SWITCHBOARD_SECRET_ENCRYPTION_KEY")
		return &toolError{codeUnavailable, "notify hooks need the operator to set SWITCHBOARD_SECRET_ENCRYPTION_KEY"}
	default:
		h.log.Error("mcp notify hook store failure", "slug", ep.Slug, "tool", tool, "hook", hookID,
			"err", fmt.Errorf("tools/call %s: %w", tool, err))
		return &toolError{codeInternal, "internal error"}
	}
}

// rfc3339 renders an optional timestamp, or nil.
func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
