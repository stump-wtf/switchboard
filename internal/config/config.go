// Package config loads runtime configuration from the environment.
//
// All secrets (Postgres/Redis DSNs, provider HMAC secrets, shared-secret tokens, the OIDC client
// secret) are injected via env/deployment config and never committed; switchboard-minted credentials
// are stored hashed in PostgreSQL. There is no external secret manager (see the design record).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/stump-wtf/switchboard/internal/push"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Addr is the listen address for the HTTP surface (webhooks, web UI, SSE, agent API).
	Addr string
	// BaseURL is the externally-reachable base URL, used to build the OIDC redirect URL and the
	// vended endpoint URLs handed to agents. No trailing slash.
	BaseURL string
	// DatabaseURL is the PostgreSQL DSN (ADR-0002).
	DatabaseURL string

	// --- OIDC relying-party config (ADR-0011: switchboard is an RP against Pocket ID, a passkey IdP) ---
	OIDCIssuer       string // e.g. https://pocket-id.stump.rocks
	OIDCClientID     string
	OIDCClientSecret string
	// OIDCRedirectURL defaults to BaseURL + /auth/callback when empty.
	OIDCRedirectURL string

	// --- GitHub OAuth provider (ADR-0026: second human login provider, non-passkey) ---
	GitHubClientID     string
	GitHubClientSecret string
	// GitHubRedirectURL defaults to BaseURL + /auth/callback when empty.
	GitHubRedirectURL string

	// SecretEncryptionKey, when set, enables at-rest encryption of held secrets that switchboard must
	// keep recoverable — currently self-managed webhook HMAC signing secrets (which cannot be hashed,
	// since verification recomputes the HMAC). It is a 32-byte AES-256 key supplied as base64 or hex,
	// held OUTSIDE PostgreSQL so a DB-only compromise yields ciphertext, not live secrets. Empty leaves
	// the legacy plaintext behavior. (SWITCHBOARD_SECRET_ENCRYPTION_KEY)
	// Governing: SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", ADR-0002.
	SecretEncryptionKey string

	// DevLogin, when true, enables a loud, unauthenticated dev-only login that mints a session for a
	// fixed local human — so the vend → agent → Channels loop is exercisable without a live Pocket ID.
	// NEVER enable in production. Off by default. (SWITCHBOARD_DEV_LOGIN=1)
	DevLogin bool

	// FriendingEnabled gates the capability-scoped Friends view (SPEC-0013): the rail entry and the
	// /friends routes are hidden — the view 404s — until an operator turns the friending capability on.
	// Off by default. Governing: SPEC-0013 REQ "Friends View", REQ "Information Architecture and
	// Navigation" (views whose backing capability is not enabled are hidden, not rendered broken).
	// (SWITCHBOARD_FRIENDING=1)
	FriendingEnabled bool

	// PushAllowHTTP is the explicit operator opt-in that lets A2A push-notification webhook targets
	// use http:// instead of https://. It exists only for a documented non-production scope (local
	// testing against an http receiver); the SSRF guard otherwise requires https. NEVER enable in
	// production — an http webhook target sends the outbound delivery (and any bearer/api-key auth
	// descriptor) in cleartext. Off by default. (SWITCHBOARD_PUSH_ALLOW_HTTP=1)
	// Governing: SPEC-0019 REQ "Webhook Target Validation (SSRF Guard)".
	PushAllowHTTP bool

	// --- MVP capability gates (ADR-0023: the basics are the product; advanced surface hidden) ---

	// PersonasEnabled gates the personas capability (named agent identities, the A2A Agent Card
	// surface, and the persona slot in the vend wizard). Off by default — the MVP is the plain
	// agent registration path, and personas are an advanced capability a deliberate flag flip
	// brings back. (SWITCHBOARD_PERSONAS=1)
	// Governing: ADR-0023 REQ "Feature Flags Hide Advanced Surfaces".
	PersonasEnabled bool

	// A2AEnabled gates the native A2A task RPC surface (/a2a/*), the A2A friend-request intake,
	// and the public Agent Card route. Off by default — agents reach their todos over MCP; A2A is
	// an advanced second protocol kept in the codebase behind the flag. (SWITCHBOARD_A2A=1)
	// Governing: ADR-0023 REQ "Feature Flags Hide Advanced Surfaces".
	A2AEnabled bool

	// A2UIEnabled gates the A2UI resource surface (queue and todo detail rendered as
	// application/a2ui+json). Off by default — A2UI is an advanced, A2UI-capable-host-only
	// rendering of the same data the tools already serve. (SWITCHBOARD_A2UI=1)
	// Governing: ADR-0023 REQ "Feature Flags Hide Advanced Surfaces".
	A2UIEnabled bool

	// MetricsToken is the dedicated scrape credential for GET /metrics, presented by the scraper as
	// "Authorization: Bearer <token>". It is its own credential class: no vended endpoint token,
	// OAuth grant, or human session authorizes a scrape. Empty leaves /metrics closed (every
	// request 401), never open; when set it must be at least MinMetricsTokenLen bytes of printable
	// ASCII, or Validate fails startup. Surrounding whitespace is trimmed so a token read from a
	// file with a trailing newline still matches. (SWITCHBOARD_METRICS_TOKEN)
	// Governing: SPEC-0023 REQ-1 "The endpoint", ADR-0028.
	MetricsToken string

	// NotifyHookMax is the per-endpoint ceiling on outbound notify hooks, an instance-wide operator
	// bound. 0 is the kill switch: every create is refused, and dispatch to existing hooks stops.
	// Unset means DefaultNotifyHookMax; a non-integer or negative value fails startup (Validate).
	// (SWITCHBOARD_NOTIFY_HOOK_MAX)
	// Governing: SPEC-0024 REQ-1 "Hook Ownership and Scope", design.md "Configuration".
	NotifyHookMax int
	// NotifyHookAllowCIDRs is the operator's comma-separated CIDR allowlist exempting ranges from the
	// notify-hook SSRF guard's private-address rule, for a single-tenant or homelab install whose
	// receiver is on the LAN. Empty (the default) exempts nothing. It opens the listed ranges to EVERY
	// tenant; loopback and link-local are exempted only by entries wholly inside them, and
	// switchboard's own listen address and port stay refused. A malformed entry fails startup.
	// (SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS)
	// Governing: SPEC-0024 REQ-3, design.md "Private ranges are an operator bound, off by default".
	NotifyHookAllowCIDRs string
	// notifyHookMaxRaw keeps the env value so Validate can reject a malformed one instead of
	// silently applying the default to a typo.
	notifyHookMaxRaw string
}

// DefaultNotifyHookMax is the per-endpoint notify-hook ceiling when SWITCHBOARD_NOTIFY_HOOK_MAX is
// unset (SPEC-0024 REQ-1).
const DefaultNotifyHookMax = 5

// MinMetricsTokenLen is the shortest scrape token Validate accepts. The bound is on length only; the
// documented generator (openssl rand -hex 32) yields 64 characters carrying 256 random bits.
const MinMetricsTokenLen = 32

// FromEnv builds a Config from environment variables, applying defaults.
func FromEnv() Config {
	base := strings.TrimRight(getenv("SWITCHBOARD_BASE_URL", "http://127.0.0.1:8080"), "/")
	redirect := os.Getenv("SWITCHBOARD_OIDC_REDIRECT_URL")
	if redirect == "" {
		redirect = base + "/auth/callback"
	}
	notifyHookMaxRaw := strings.TrimSpace(os.Getenv("SWITCHBOARD_NOTIFY_HOOK_MAX"))
	notifyHookMax := parseNotifyHookMax(notifyHookMaxRaw)
	githubRedirect := os.Getenv("SWITCHBOARD_GITHUB_REDIRECT_URL")
	if githubRedirect == "" {
		githubRedirect = base + "/auth/callback"
	}
	return Config{
		Addr:                 getenv("SWITCHBOARD_ADDR", "127.0.0.1:8080"),
		BaseURL:              base,
		DatabaseURL:          os.Getenv("SWITCHBOARD_DATABASE_URL"),
		OIDCIssuer:           os.Getenv("SWITCHBOARD_OIDC_ISSUER"),
		OIDCClientID:         os.Getenv("SWITCHBOARD_OIDC_CLIENT_ID"),
		OIDCClientSecret:     os.Getenv("SWITCHBOARD_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:      redirect,
		GitHubClientID:       os.Getenv("SWITCHBOARD_GITHUB_CLIENT_ID"),
		GitHubClientSecret:   os.Getenv("SWITCHBOARD_GITHUB_CLIENT_SECRET"),
		GitHubRedirectURL:    githubRedirect,
		SecretEncryptionKey:  os.Getenv("SWITCHBOARD_SECRET_ENCRYPTION_KEY"),
		DevLogin:             os.Getenv("SWITCHBOARD_DEV_LOGIN") == "1",
		FriendingEnabled:     os.Getenv("SWITCHBOARD_FRIENDING") == "1",
		PushAllowHTTP:        os.Getenv("SWITCHBOARD_PUSH_ALLOW_HTTP") == "1",
		PersonasEnabled:      os.Getenv("SWITCHBOARD_PERSONAS") == "1",
		A2AEnabled:           os.Getenv("SWITCHBOARD_A2A") == "1",
		A2UIEnabled:          os.Getenv("SWITCHBOARD_A2UI") == "1",
		MetricsToken:         strings.TrimSpace(os.Getenv("SWITCHBOARD_METRICS_TOKEN")),
		NotifyHookMax:        notifyHookMax,
		NotifyHookAllowCIDRs: strings.TrimSpace(os.Getenv("SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS")),
		notifyHookMaxRaw:     notifyHookMaxRaw,
	}
}

// parseNotifyHookMax reads SWITCHBOARD_NOTIFY_HOOK_MAX: unset is the default, anything else must be
// a non-negative integer. A bad value maps to -1, which Validate turns into a startup error.
func parseNotifyHookMax(raw string) int {
	if raw == "" {
		return DefaultNotifyHookMax
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// Validate rejects configuration that must fail startup rather than run degraded. It never echoes
// a secret value into its error.
func (c Config) Validate() error {
	if c.MetricsToken != "" {
		if len(c.MetricsToken) < MinMetricsTokenLen {
			return fmt.Errorf("config: SWITCHBOARD_METRICS_TOKEN is %d bytes; it must be at least %d (generate one with: openssl rand -hex 32)",
				len(c.MetricsToken), MinMetricsTokenLen)
		}
		// A bearer token travels in an HTTP header, so it must be printable ASCII with no spaces:
		// anything else is either mangled in transit or can never match the presented credential.
		for i := 0; i < len(c.MetricsToken); i++ {
			if b := c.MetricsToken[i]; b <= ' ' || b > '~' {
				return errors.New("config: SWITCHBOARD_METRICS_TOKEN must be printable ASCII with no whitespace")
			}
		}
	}
	if _, err := push.ParseCIDRList(c.NotifyHookAllowCIDRs); err != nil {
		return fmt.Errorf("config: SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS: %w", err)
	}
	if c.NotifyHookMax < 0 {
		return fmt.Errorf("config: SWITCHBOARD_NOTIFY_HOOK_MAX=%q must be a non-negative integer (0 disables notify hooks)", c.notifyHookMaxRaw)
	}
	return nil
}

// OIDCConfigured reports whether the OIDC relying-party settings are present.
func (c Config) OIDCConfigured() bool {
	return c.OIDCIssuer != "" && c.OIDCClientID != "" && c.OIDCClientSecret != ""
}

// GitHubConfigured reports whether the GitHub OAuth provider settings are present.
func (c Config) GitHubConfigured() bool {
	return c.GitHubClientID != "" && c.GitHubClientSecret != ""
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
