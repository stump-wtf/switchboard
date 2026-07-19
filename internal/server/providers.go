package server

// Boot-time provider seeding and registry-backed provider enumeration (ADR-0020, SPEC-0017).
// Env/deployment config stops being the runtime authority for providers: Run imports it into the
// provider registry (extended adapters table) once per boot as a create-if-absent seed, and every
// read surface — providerStatuses()/list_providers today, the Providers view next — reads the
// registry instead of the process environment.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/joestump/switchboard/internal/ingest"
	mcpsrv "github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/store"
)

// providerSeeder is the store slice the boot seed needs; *store.Store satisfies it, tests fake it.
type providerSeeder interface {
	SeedProvider(ctx context.Context, seed store.ProviderSeed) (bool, error)
}

// seedEnvProviders imports every env-configured webhook provider into the provider registry as an
// idempotent boot seed: create-if-absent, NEVER clobber. A provider the registry already holds is
// left untouched — the registry is authoritative after the first import, so env drift (a rotated
// env secret, a changed queue) does not silently override an operator's registry edits; a fresh
// deployment still comes up fully configured from env alone. Signed providers (github/stripe/slack)
// seed only when their env secret is set (an unconfigured signed route rejects everything, so there
// is nothing to import); generic providers seed exactly as declared, disabled-until-token included.
// cfg MUST be Normalized() so the seeded queues match what the receivers would default to.
//
// Errors fail startup loudly: the database just migrated, so a seed failure is a real fault, and
// booting with half-imported providers would be silent misconfiguration.
//
// Governing: ADR-0020 (env becomes a seed, not the authority), SPEC-0017 REQ "Environment Config
// Import" (scenario "Boot with existing registry" — registry row wins, no duplicate).
func seedEnvProviders(ctx context.Context, st providerSeeder, cfg ingest.Config, log *slog.Logger) error {
	seed := func(s store.ProviderSeed) error {
		created, err := st.SeedProvider(ctx, s)
		if err != nil {
			return fmt.Errorf("seed provider %s: %w", s.Name, err)
		}
		if created {
			log.Info("provider imported from env into registry",
				"provider", s.Name, "kind", s.Kind, "trust_mode", s.TrustMode)
		}
		return nil
	}

	signed := []struct{ name, secret, queue string }{
		{"github", cfg.GitHubSecret, cfg.GitHubQueue},
		{"stripe", cfg.StripeSecret, cfg.StripeQueue},
		{"slack", cfg.SlackSecret, cfg.SlackQueue},
	}
	for _, sp := range signed {
		if sp.secret == "" {
			continue // not configured in this deployment — nothing to import
		}
		cfgJSON, err := json.Marshal(map[string]string{"queue": sp.queue})
		if err != nil {
			return fmt.Errorf("seed provider %s: marshal config: %w", sp.name, err)
		}
		if err := seed(store.ProviderSeed{
			Name: sp.name, Family: "webhook", Kind: sp.name, TrustMode: "signed",
			Secret: sp.secret, Config: cfgJSON,
		}); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Generic)) {
		gp := cfg.Generic[name]
		cfgJSON, err := json.Marshal(map[string]string{"queue": gp.Queue})
		if err != nil {
			return fmt.Errorf("seed provider %s: marshal config: %w", name, err)
		}
		if err := seed(store.ProviderSeed{
			Name: name, Family: "webhook", Kind: "generic", TrustMode: gp.Mode,
			Secret: gp.Token, Config: cfgJSON,
		}); err != nil {
			return err
		}
	}
	return nil
}

// providerStatuses projects provider REGISTRY rows into the SPEC-0005 list_providers shape — the
// output shape (and its no-secrets rule) is unchanged from the env-driven era; only the source
// moved to the registry, so runtime-created providers enumerate without a restart. Rows carry only
// a secret-presence flag, never material, so nothing here can reach the wire.
//
// Advertising rules, matching the previous behavior: a signed or token webhook provider with no
// held secret rejects every delivery, so it is an unreachable route and is skipped rather than
// misrepresented as inventory; open providers exist by explicit opt-in and carry none-by-design.
// Queue-family rows (the pull adapters ADR-0014 put in this same table) now join the enumeration —
// their broker DSN lives in env, never the registry, hence none-by-design likewise.
//
// Governing: SPEC-0005 REQ "Provider Enumeration Without Secrets"; ADR-0020, SPEC-0017 REQ
// "Runtime Provider Registry".
func providerStatuses(rows []store.Adapter) []mcpsrv.ProviderStatus {
	var out []mcpsrv.ProviderStatus
	for _, row := range rows {
		ps := mcpsrv.ProviderStatus{
			Name: row.Name, Family: row.Family, TrustMode: row.TrustMode, Enabled: row.Enabled,
		}
		switch row.Family {
		case "webhook":
			switch row.TrustMode {
			case "signed":
				if !row.SecretConfigured {
					continue // no secret held anywhere → the route 503s; don't advertise it
				}
				ps.SecretStatus = "configured"
				ps.Path = "/webhooks/" + row.Name
			case "token":
				if !row.SecretConfigured {
					continue // disabled-until-token (403s everything) → unreachable, skip
				}
				ps.SecretStatus = "configured"
				ps.Path = "/webhooks/generic/" + row.Name
			case "open":
				ps.SecretStatus = "none-by-design"
				ps.Path = "/webhooks/generic/" + row.Name
			default:
				continue // unknown trust mode: never enumerate what dispatch would not serve
			}
		case "queue":
			ps.SecretStatus = "none-by-design"
			ps.Channel = queueChannel(row.Config)
		default:
			continue
		}
		out = append(out, ps)
	}
	return out
}

// queueChannel extracts the consume topology label for a queue-family row from its non-secret
// config jsonb — the stream, list, or pubsub channel name, whichever the row's mode uses (the
// redis.RegistryConfig field names). Empty when the config carries none.
func queueChannel(config []byte) string {
	var c struct {
		Stream  string `json:"stream"`
		List    string `json:"list"`
		Channel string `json:"channel"`
	}
	_ = json.Unmarshal(config, &c)
	for _, v := range []string{c.Stream, c.List, c.Channel} {
		if v != "" {
			return v
		}
	}
	return ""
}
