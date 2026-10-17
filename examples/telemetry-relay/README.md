# An OTLP sink to telemetry relay

This Python 3.9+ example turns ERROR/FATAL logs and error spans in an OTLP JSONL
sink into Switchboard todos. It uses the standard library and runs on macOS or
Linux. The receiver must persist exports as one JSON object per line with an
OTLP JSON `body` and a numeric Unix-seconds `received` field, using `logs-*.jsonl` / `traces-*.jsonl` names. Rotated files
may end in `.jsonl.gz`.

See [the deployment guide](../../docs/guides/18-telemetry-relay.md) for grants,
worker configuration, scheduling, and end to end validation.

```sh
python3 telemetry-relay.py --dry-run
python3 telemetry-relay.py --send
python3 -m unittest discover -s . -p 'test_*.py' -v
```

| Environment | Meaning |
| --- | --- |
| `OTLP_SINK_DIR` | Export directory; default `~/.local/state/otlp-sink`. |
| `TELEMETRY_WEBHOOK_URL` | Token-bearing ingress URL from `create_webhook`; required for `--send`. Keep private. |
| `TELEMETRY_STATE` | Sender deduplication checkpoint; default `~/.local/state/telemetry-relay/seen.json`. |
| `TELEMETRY_MAX_AGE_SECONDS` | 300; exports older than five minutes or without valid receive timestamps are skipped. Zero explicitly enables replay. |
| `TELEMETRY_GROUP_SECONDS` | 900; similar failures share one task per fixed 15-minute window. Zero disables grouping. |
| `TELEMETRY_MAX_PER_HOUR` | 8; persisted rolling budget for group deliveries across scans and restarts. |
| `TELEMETRY_MIN_SEVERITY` | `ERROR` (default) or `FATAL`. Numeric log severity takes precedence. Error spans are excluded in FATAL mode. |

The relay distinguishes occurrences using timestamp and content, and sends a
stable group `X-Delivery-Id`. It checkpoints acknowledged group deliveries and
occurrence IDs. Export retries deduplicate even across group boundaries; a later
occurrence of the same failure is suppressed within its group window. Compression preserves IDs
for records without timestamps. IDs are retained for 30 days; keep sink
retention shorter than that. A lock prevents overlapping sends.

The default is a dry run: it redacts and prints candidate payloads, sends
nothing, and does not change state. Dry-run output can still contain operational
details; common credential patterns are redacted, which is not a guarantee that
arbitrary secrets in messages will be recognized. The sender includes only
bounded service, severity, summary, and log/trace identifiers, not the entire
export or its resource attributes.

Missing configuration or input, corrupt state, and failed HTTP delivery return
nonzero. Exception output omits capability URLs. HTTP 429 and transient failures
stop the current scan; the next scheduled scan retries unacknowledged IDs.
State already checkpointed remains intact. The summary reports matching signals, deliveries, filtered or previously seen
signals, stale exports, groups, and budget deferrals. The default does not replay
old retained exports; review the dry run before enabling. Budget-deferred signals
are dropped if they age beyond the freshness cutoff before capacity returns.

## Deployment validator

Save the endpoint's one-time vend response to a mode-0600 JSON file containing
`mcp_url` and `token`. Once the receiver, scheduled relay, and worker are running:

```sh
python3 validate.py --vend-record /private/path/telemetry-endpoint.json
```

The validator injects an OTLP error with service `telemetry-relay.validation` and
summary `telemetry relay deployment probe <UUID>`. It does **not** run the relay.
It waits for the scheduled job to produce exactly one todo and for the worker
to complete it with `{"validation":"acknowledged"}` (an object or JSON-encoded
string). Give the worker that probe
rule before running it. This is a validation event, not an incident to fix.
`list_todos` omits payloads, so the validator uses `get_todo` on candidate IDs.

Default wait is 300 seconds. A busy worker may need more time (`--timeout`);
resume an already injected probe with `--probe-id <UUID>` instead of injecting
another. `--mcp-url` can select the same endpoint's local MCP URL; `--otlp-url`
selects the sink's OTLP/HTTP log receiver. Credentials are read from the private
file and never printed. A missing acknowledgement, failed todo, or duplicate
todo returns nonzero.

## Volume policy

Structured tool-call errors (`agent.tool.name`) are excluded from both logs and
spans. Runtime failures remain eligible. Failure groups include service, signal,
severity, and normalized summary; numeric and long hexadecimal identifiers are
normalized. The first task includes the distinct occurrence count in that scan.
Later occurrences in the same window do not update it. A fixed-window boundary
can yield two nearby tasks, while the rolling hourly budget remains enforced.

Old raw-span/log IDs remain for deduplication when upgrading. With grouping
enabled, only new group delivery timestamps count toward the new policy's budget;
legacy deliveries do not block runtime alerts on rollout. Occurrence checkpoints
are separate from delivery timestamps. Validation probes consume the same budget.

Run the tests before installing and verify the installed source matches the
reviewed source. Receiver health alone does not prove worker access: run the
validator against the actual scheduled job and inspect its completion receipt.
