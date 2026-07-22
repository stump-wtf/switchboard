package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/joestump/switchboard/internal/adapter"
	"github.com/joestump/switchboard/internal/adapter/redis"
	"github.com/joestump/switchboard/internal/store"
)

// queueAdapterStore is the narrow slice of the store the queue-adapter wiring needs: the registry
// listing that says which pull adapters exist, plus the atomic event+todo transaction each
// adapter's StoreSink delivers into. *store.Store satisfies it; tests fake it in memory.
type queueAdapterStore interface {
	ListAdaptersByFamily(ctx context.Context, family string) ([]store.Adapter, error)
	adapter.EventTodoCreator
}

// adapterAdder is the runner slice the wiring needs (*runner.Runner satisfies it): attach one
// adapter + sink pair per registry row; the runner owns the poll-loop lifecycle from there.
type adapterAdder interface {
	Add(a adapter.Adapter, sink adapter.Sink, config []byte) error
}

// registerQueueAdapters loads the queue-family adapter registry (adapters table rows with
// family='queue') and attaches one poll-loop worker per Redis-transport row to the runner, closing
// the SPEC-0002 chain end to end: registry row → transport front half (consume) → StoreSink back
// half (store-then-ack) → todo. The returned close func releases the shared broker client and MUST
// be called only after the runner has fully stopped (it is a no-op when no adapter was attached).
//
// Enable/disable semantics (SPEC-0002 REQ "Adapter Interface and Trust Mode"):
//
//   - The row's enabled flag is honored AT RUNTIME by the runner: flipping it false parks the loop
//     (and cancels an in-flight consume) within the runner's EnabledInterval; flipping it back true
//     resumes consuming — no restart needed. Disabled rows are therefore still attached here.
//   - ADDING or REMOVING a row, or EDITING its config (mode, stream/list/channel, queue, …), takes
//     effect on the NEXT server restart: the registry is read once at startup. Restart-required is
//     the documented contract for membership/config changes; the enabled flag alone is the runtime
//     kill switch.
//
// Config split (ADR-0014): the registry row carries ONLY non-secret consume topology
// (redis.RegistryConfig); the broker DSN — the secret — comes from SWITCHBOARD_REDIS_URL and never
// touches the database or logs. A malformed DSN fails startup loudly (env misconfiguration, same
// posture as SWITCHBOARD_GENERIC_PROVIDERS); a malformed registry ROW is logged loudly and skipped
// instead — one bad row degrades that adapter, never the whole server, and unlike the generic
// providers a skipped pull adapter fails closed (nothing is consumed, nothing is exposed).
//
// The adapters table these rows live in is now the provider REGISTRY both ingestion families
// resolve from (ADR-0020, SPEC-0017 REQ "Runtime Provider Registry"): the runner side already
// resolves the enabled flag from it at poll time and stamps health back onto it, which is the
// pull-family half of "registry changes take effect without restart".
//
// Governing: ADR-0014 (adapter registry; secrets via environment), ADR-0020, SPEC-0002 REQ
// "Adapter Interface and Trust Mode", REQ "Poll-Loop Lifecycle — Concurrency Safety".
// legacyEndpointID is the operator-designated endpoint that owns todos minted by adapters whose
// registry row names none of its own — the pull-side twin of ingest.Config.LegacyEndpointID and,
// like it, INTERIM and revisited in PR 2. An adapter with neither its own nor a fallback endpoint
// fails soft: it stays dark and loudly logged, never consuming (ADR-0022).
func registerQueueAdapters(ctx context.Context, st queueAdapterStore, run adapterAdder, redisURL, legacyEndpointID string, log *slog.Logger) (func() error, error) {
	noop := func() error { return nil }
	rows, err := st.ListAdaptersByFamily(ctx, adapter.Family)
	if err != nil {
		return noop, fmt.Errorf("list queue adapters: %w", err)
	}
	if len(rows) == 0 {
		return noop, nil
	}
	if redisURL == "" {
		// Rows exist but no broker DSN is configured: pull ingestion stays off. Loud, not fatal —
		// the operator may be mid-rollout, and the push/HTTP surface is independent of the broker.
		log.Warn("queue adapters registered but SWITCHBOARD_REDIS_URL is unset; pull ingestion disabled",
			"adapters", len(rows))
		return noop, nil
	}

	factory, err := redis.NewFactory(redisURL, log)
	if err != nil {
		return noop, err // redacted by NewFactory; a malformed DSN is env misconfiguration → fail startup
	}
	attached := 0
	for _, row := range rows {
		a, cfg, err := factory.FromRegistry(row.Name, row.Config)
		if errors.Is(err, redis.ErrForeignTransport) {
			// A row for a transport this build doesn't implement (future SQS/NATS/AMQP) — skip
			// quietly-but-visibly; it is configuration for someone else, not an error here.
			log.Warn("queue adapter names an unimplemented transport; skipping", "adapter", row.Name)
			continue
		}
		if err != nil {
			// Fail-soft per row (see the function comment): the adapter stays dark and loudly
			// logged; consuming nothing is the safe failure mode for a pull adapter.
			log.Error("queue adapter config invalid; adapter not started", "adapter", row.Name, "err", err)
			continue
		}
		// Ownership is per registry row where the row states it, falling back to the operator's
		// designated legacy endpoint. NewStoreSink rejects an empty id, so a row that states
		// neither leaves the adapter dark rather than failing on every insert (ADR-0022).
		endpointID := cfg.EndpointID
		if endpointID == "" {
			endpointID = legacyEndpointID
		}
		sink, err := adapter.NewStoreSink(st, log, adapter.StoreSinkConfig{
			Queue:       cfg.Queue,
			TrustDetail: a.TrustDetail(),
			EndpointID:  endpointID,
		})
		if err != nil {
			log.Error("queue adapter sink construction failed; adapter not started", "adapter", row.Name, "err", err)
			continue
		}
		if err := run.Add(a, sink, row.Config); err != nil {
			log.Error("queue adapter runner registration failed; adapter not started", "adapter", row.Name, "err", err)
			continue
		}
		attached++
		log.Info("queue adapter attached", "adapter", row.Name, "mode", cfg.Mode,
			"queue", cfg.Queue, "enabled", row.Enabled)
	}
	if attached == 0 {
		// Nothing consumes from the broker; don't hold its connection pool open.
		return noop, factory.Close()
	}
	return factory.Close, nil
}
