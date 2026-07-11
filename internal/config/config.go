// Package config loads runtime configuration from the environment.
//
// All secrets (Postgres/Redis DSNs, provider HMAC secrets, shared-secret tokens, the OIDC client
// secret) are injected via env/deployment config and never committed; switchboard-minted credentials
// are stored hashed in PostgreSQL. There is no external secret manager (see the design record).
package config

import (
	"os"
	"strings"
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
	// RedisURL is the Redis DSN (redis:// or rediss://) for the pull-adapter family — trust is the
	// broker connection itself (auth/ACL + TLS), so credentials live in this DSN and are never
	// logged. Empty disables the Redis adapters. (SWITCHBOARD_REDIS_URL)
	// Governing: ADR-0014 (Redis reference pull adapter), SPEC-0002 REQ "Redis Reference Transport Modes".
	RedisURL string

	// --- OIDC relying-party config (ADR-0011: switchboard is an RP against Pocket ID, a passkey IdP) ---
	OIDCIssuer       string // e.g. https://pocket-id.stump.rocks
	OIDCClientID     string
	OIDCClientSecret string
	// OIDCRedirectURL defaults to BaseURL + /auth/callback when empty.
	OIDCRedirectURL string

	// DevLogin, when true, enables a loud, unauthenticated dev-only login that mints a session for a
	// fixed local human — so the vend → agent → Channels loop is exercisable without a live Pocket ID.
	// NEVER enable in production. Off by default. (SWITCHBOARD_DEV_LOGIN=1)
	DevLogin bool
}

// FromEnv builds a Config from environment variables, applying defaults.
func FromEnv() Config {
	base := strings.TrimRight(getenv("SWITCHBOARD_BASE_URL", "http://127.0.0.1:8080"), "/")
	redirect := os.Getenv("SWITCHBOARD_OIDC_REDIRECT_URL")
	if redirect == "" {
		redirect = base + "/auth/callback"
	}
	return Config{
		Addr:             getenv("SWITCHBOARD_ADDR", "127.0.0.1:8080"),
		BaseURL:          base,
		DatabaseURL:      os.Getenv("SWITCHBOARD_DATABASE_URL"),
		RedisURL:         os.Getenv("SWITCHBOARD_REDIS_URL"),
		OIDCIssuer:       os.Getenv("SWITCHBOARD_OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("SWITCHBOARD_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("SWITCHBOARD_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:  redirect,
		DevLogin:         os.Getenv("SWITCHBOARD_DEV_LOGIN") == "1",
	}
}

// OIDCConfigured reports whether the OIDC relying-party settings are present.
func (c Config) OIDCConfigured() bool {
	return c.OIDCIssuer != "" && c.OIDCClientID != "" && c.OIDCClientSecret != ""
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
