package redis

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	goredis "github.com/redis/go-redis/v9"

	"github.com/joestump/switchboard/internal/adapter"
)

// Transport is the adapters.config "transport" value that assigns a queue-family registry row to
// this package's Redis transports. Future pull transports (SQS, NATS, AMQP — ADR-0014) claim their
// own value; rows naming a transport nobody implements are skipped by the server wiring, not errors.
const Transport = "redis"

// ErrForeignTransport reports a registry row whose config names a transport other than "redis".
// The server wiring uses it (via errors.Is) to skip rows meant for future transport packages
// without treating them as misconfiguration.
var ErrForeignTransport = errors.New("redis: registry config names another transport")

// RegistryConfig is the NON-SECRET adapters.config jsonb shape for a Redis pull adapter. It carries
// only consume topology — which stream/list/channel to consume, as which group/consumer, into which
// todo queue. The broker DSN (the secret) is deliberately NOT part of this shape: it comes from
// SWITCHBOARD_REDIS_URL (internal/config) and is never stored in the database or logged.
//
// The fields mirror the transport constructors' configs (StreamConfig / ListConfig / PubSubConfig)
// one-to-one — the registry row is a serialization of what the library already expects, not a
// second config scheme.
//
// Governing: ADR-0014 ("queue connection URLs are injected via environment/config"; registry rows
// hold runtime enable/disable + non-secret config), SPEC-0002 REQ "Adapter Interface and Trust Mode".
type RegistryConfig struct {
	// Transport selects the transport package; MUST be "redis" for this package to claim the row.
	Transport string `json:"transport"`
	// Mode selects the Redis transport mode: "stream" (RECOMMENDED, durable), "list" (acceptable
	// ack-capable alternative), or "pubsub" (fire-and-forget — loss-tolerant work ONLY, see PubSub).
	Mode string `json:"mode"`

	// Stream mode (see StreamConfig).
	Stream string `json:"stream,omitempty"`
	// Group is the stream consumer group. Defaults to "switchboard".
	Group string `json:"group,omitempty"`
	// Consumer is this instance's consumer name within the group. Defaults to the host name
	// (single-binary deployment, ADR-0001), falling back to "switchboard".
	Consumer string `json:"consumer,omitempty"`

	// List mode (see ListConfig).
	List       string `json:"list,omitempty"`
	Processing string `json:"processing,omitempty"`

	// PubSub mode (see PubSubConfig).
	Channel string `json:"channel,omitempty"`

	// Queue is the target todo queue. Empty routes each message to its envelope Name (the
	// stream/list/channel it was consumed from) — StoreSinkConfig.Queue semantics.
	Queue string `json:"queue,omitempty"`
	// TrustDetail names the broker/ACL identity for the event's verify_detail (ADR-0003). Defaults
	// to an identity derived from the DSN's ACL username ("redis acl: {user}", or "redis connection"
	// for the default user) — the row may override it with a more descriptive label, never a secret.
	TrustDetail string `json:"trust_detail,omitempty"`
}

// Factory builds registry-configured Redis adapters over one shared client. One factory per broker
// DSN: every adapter row shares the connection pool and the connection's ACL identity.
type Factory struct {
	client   *goredis.Client
	identity string
	log      *slog.Logger
}

// NewFactory parses the broker DSN (SWITCHBOARD_REDIS_URL — may embed credentials, so parse errors
// are redacted exactly like NewClient's) and derives the connection's trust identity from the DSN's
// ACL username. The DSN itself never leaves the factory.
func NewFactory(dsn string, log *slog.Logger) (*Factory, error) {
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		return nil, errors.New("redis: parse dsn: " + redactDSN(err.Error(), dsn))
	}
	identity := "redis connection"
	if opts.Username != "" {
		identity = "redis acl: " + opts.Username
	}
	if log == nil {
		log = slog.Default()
	}
	return &Factory{client: goredis.NewClient(opts), identity: identity, log: log}, nil
}

// Close releases the factory's shared client (and its connection pool). Call it only after every
// adapter built from this factory has stopped consuming.
func (f *Factory) Close() error { return f.client.Close() }

// FromRegistry builds the transport adapter an adapters-table row describes: parse the row's
// non-secret config jsonb, validate it, and construct the matching mode over the factory's shared
// client. name is the row's primary key and becomes the adapter's Name verbatim, so the runner's
// registration/enabled-flag checks hit the SAME row the operator configured — a derived name would
// silently fork the registry.
//
// Rows for other transports return ErrForeignTransport (skip, not misconfiguration); anything else
// wrong with the config returns a descriptive error naming the row.
//
// Governing: ADR-0014 (adapter registry), SPEC-0002 REQ "Adapter Interface and Trust Mode",
// REQ "Redis Reference Transport Modes".
func (f *Factory) FromRegistry(name string, rawConfig []byte) (adapter.Adapter, RegistryConfig, error) {
	var cfg RegistryConfig
	if len(rawConfig) == 0 {
		return nil, cfg, fmt.Errorf("redis: adapter %q: registry row has no config", name)
	}
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, cfg, fmt.Errorf("redis: adapter %q: parse registry config: %w", name, err)
	}
	if cfg.Transport != Transport {
		return nil, cfg, fmt.Errorf("redis: adapter %q: transport %q: %w", name, cfg.Transport, ErrForeignTransport)
	}
	if cfg.TrustDetail == "" {
		cfg.TrustDetail = f.identity
	}

	var (
		a   adapter.Adapter
		err error
	)
	switch cfg.Mode {
	case "stream":
		group := cfg.Group
		if group == "" {
			group = "switchboard"
		}
		consumer := cfg.Consumer
		if consumer == "" {
			if host, herr := os.Hostname(); herr == nil && host != "" {
				consumer = host
			} else {
				consumer = "switchboard"
			}
		}
		a, err = NewStream(f.client, f.log, StreamConfig{
			Name: name, Stream: cfg.Stream, Group: group, Consumer: consumer, TrustDetail: cfg.TrustDetail,
		})
	case "list":
		a, err = NewList(f.client, f.log, ListConfig{
			Name: name, List: cfg.List, Processing: cfg.Processing, TrustDetail: cfg.TrustDetail,
		})
	case "pubsub":
		// Fire-and-forget: no ack, no redelivery — the operator opted into loss tolerance by
		// selecting this mode (SPEC-0002 scenario "Pub/sub is loss-tolerant only"; see PubSub).
		a, err = NewPubSub(SubscriberClient{Client: f.client}, f.log, PubSubConfig{
			Name: name, Channel: cfg.Channel, TrustDetail: cfg.TrustDetail,
		})
	default:
		return nil, cfg, fmt.Errorf("redis: adapter %q: unknown mode %q (want stream, list, or pubsub)", name, cfg.Mode)
	}
	if err != nil {
		return nil, cfg, fmt.Errorf("redis: adapter %q: %w", name, err)
	}
	return a, cfg, nil
}
