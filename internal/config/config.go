// Package config loads runtime configuration from the environment.
//
// All secrets (Postgres/Redis DSNs, provider HMAC secrets, shared-secret tokens, the OIDC client
// secret) are injected via env/deployment config and never committed; switchboard-minted credentials
// are stored hashed in PostgreSQL. There is no external secret manager (see the design record).
package config

import "os"

// Config is the resolved runtime configuration.
type Config struct {
	// Addr is the listen address for the HTTP surface (webhooks, web UI, SSE, MCP mount).
	Addr string
	// DatabaseURL is the PostgreSQL DSN (ADR-002). Empty until the code session wires persistence.
	DatabaseURL string
}

// FromEnv builds a Config from environment variables, applying defaults.
func FromEnv() Config {
	return Config{
		Addr:        getenv("SWITCHBOARD_ADDR", "127.0.0.1:8080"),
		DatabaseURL: os.Getenv("SWITCHBOARD_DATABASE_URL"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
