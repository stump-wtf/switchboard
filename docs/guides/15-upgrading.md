---
title: Upgrading
---

# Upgrading

This page documents the changes that need action when you move between Switchboard
releases. It is written for people running their own Switchboard; if you are on the
published image, check which release you are actually running first.

To find your version, run `switchboard version`, or read it from `/healthz`. Newer builds
also report it over MCP as `serverInfo.version`.

## Upgrading to Unreleased

### Replay targets are owned by the endpoint

**What breaks.** The instance settings `replay_default_target` and `replay_allowed_targets`
are removed. Migration `0026_owned_replay_targets` deletes both rows from `settings`; after
the upgrade nothing reads them and nothing warns about them at startup. Their targets were
exempt from the SSRF checks for every tenant's replay, which is why they are gone rather
than kept for compatibility.

`replay_webhook_event` now:

- replays to the `target_url` the call names, or else to the calling endpoint's first
  **owned** replay target;
- fails with the new error code `replay_target_required` when it has neither;
- sends every target, owned or not, through the shared SSRF guard, both when the call is
  made and again when it connects: the target must be `https` and every address its host
  resolves to must be public. Loopback, private (RFC 1918 and IPv6 unique-local),
  link-local and cloud-metadata, and shared (100.64.0.0/10) addresses are refused, and so
  is plain `http`.

**Who is affected.** Anyone who set either setting, or who replays to a consumer on
localhost, a private network, or plain `http`. An agent that called `replay_webhook_event`
without `target_url` now gets `replay_target_required` until its endpoint owns a target.

**What to do.**

1. Back up first. The migration deletes the two rows and cannot put them back:

   ```sh
   pg_dump --format=custom --file=switchboard-pre-replay-targets.dump "$SWITCHBOARD_DATABASE_URL"
   ```

2. For each endpoint that should replay by default, vend a replacement that owns its
   targets. Replay targets are part of an endpoint's scope, which is fixed at vend time:

   ```sh
   curl -sS -X POST "$SWITCHBOARD_URL/api/v1/endpoints" \
     -H "Authorization: Bearer $OPERATOR_TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"my-agent","replay_targets":["https://consumer.example.com/hook"]}'
   ```

   A target the guard refuses fails the vend with `400` and mints nothing. Otherwise, pass
   `target_url` on each replay call.

3. Move any replay consumer that lived on localhost or a private network to a public
   `https` URL, or stop replaying to it. There is no allowlist for internal hosts.

**Verify.**

- `GET /api/v1/endpoints` lists `replay_targets` for each of your endpoints.
- `replay_webhook_event` with no `target_url` on an endpoint without targets returns
  `replay_target_required`.
- Nothing is left in `settings`. This should print `0`:

  ```sh
  psql "$SWITCHBOARD_DATABASE_URL" -Atc \
    "SELECT count(*) FROM settings WHERE key IN ('replay_default_target','replay_allowed_targets')"
  ```

## Upgrading to v0.3.0

### Read this first

**v0.3.0 is a breaking change for any deployment that receives webhooks.** The
instance-wide receivers configured through environment variables have been removed. After
you upgrade:

- the environment variables listed below are **ignored, with no warning and no message at
  startup**;
- deliveries to `/webhooks/github`, `/webhooks/gitea`, `/webhooks/stripe`,
  `/webhooks/slack` and `/webhooks/generic/*` will fail, because those routes no longer
  exist;
- the migration that drops the old provider rows **cannot be reversed**.

Back up your database before you upgrade. The backup is the only way back.

### What breaks, and who is affected

You are affected if any of these is true:

- you set `SWITCHBOARD_GITHUB_SECRET`, `SWITCHBOARD_GITEA_SECRET`,
  `SWITCHBOARD_STRIPE_SECRET` or `SWITCHBOARD_SLACK_SECRET`; or
- you set `SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID`; or
- a sender delivers to `POST /webhooks/<provider>` rather than to a per-webhook URL that
  contains a token.

You are **not** affected if every sender already delivers to a webhook you created with
`create_webhook` or the vend wizard, and you never set those variables. In that case the
upgrade needs no changes from you; skip to "Verify".

### Why this changed

The old receivers were instance-wide: one secret and one URL shared by every sender, and
every delivery landed in a registry the operator owned. That meant Switchboard could not
name the endpoint that owned the todos a delivery minted, so the todos belonged to
nobody, the operator board could not attribute them, and the provider registry held
secrets with nothing able to rotate or remove them.

A webhook now belongs to an endpoint, carries its own signing secret, and mints todos
owned by that endpoint's owner. The trade is that each sender gets its own URL and
secret, and nothing is shared. This is the same reason the "receiver not configured" 503
and the empty providers page existed; both are gone with the old path.

### Before you upgrade: back up

Migration `0021_drop_adapters` deletes the old provider rows. It is not reversible, and no
downgrade can restore them. Take a dump first:

```sh
pg_dump --format=custom --file=switchboard-pre-v0.3.0.dump "$SWITCHBOARD_DATABASE_URL"
```

Keep that file somewhere other than the database host until you have confirmed the
upgrade works.

### The variables that are now ignored

Each is ignored silently. The replacement is the same in every case: a webhook created
through `create_webhook` (or the vend wizard), which returns the ingest URL and signing
secret that sender should use instead.

| Retired variable | Replacement |
|---|---|
| `SWITCHBOARD_GITHUB_SECRET` | A webhook created with `create_webhook` for the GitHub sender; paste its `ingest_url` and `signing_secret` into the GitHub webhook settings. |
| `SWITCHBOARD_GITEA_SECRET` | Same, for the Gitea sender. |
| `SWITCHBOARD_STRIPE_SECRET` | Same, for the Stripe sender. |
| `SWITCHBOARD_SLACK_SECRET` | Same, for the Slack sender. |
| `SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID` | Not replaced. It named the endpoint that claimed every legacy delivery; ownership now comes from the webhook itself, so `create_webhook` takes `as_endpoint` instead. |

Remove the variables from your environment once the senders have moved. Leaving them set
is harmless, but it hides the fact that the migration is unfinished.

### Move each sender to its own webhook

Do this per sender. Work through them one at a time and confirm each before starting the
next, so a failure is attributable.

**Before** — one shared secret, one shared URL for every sender:

```sh
SWITCHBOARD_GITHUB_SECRET=<one secret, shared>
# GitHub delivers to: https://switchboard.example.com/webhooks/github
```

**After** — one webhook per sender, each with its own token in the URL and its own secret:

```sh
# No SWITCHBOARD_*_SECRET variables. The secret lives with the webhook, not the process.
```

For each sender:

1. Have the agent or operator that owns the target endpoint create the webhook. The
   ingest URL embeds a per-webhook token, so it differs for every sender:

   ```
   create_webhook(name="github-push", as_endpoint=<your endpoint>)
   → ingest_url:    https://switchboard.example.com/webhooks/w/<token>
   → signing_secret: <secret, shown once>
   ```

2. Paste both values into the sender's webhook configuration, replacing the old
   `/webhooks/<provider>` URL and the shared secret. Set the same content type the sender
   used before (GitHub and Gitea send `application/json`).
3. Send one real delivery from that sender, and confirm it arrives as a todo on the queue
   you expect.

### Verify

- Send a test delivery from every sender that was migrated, and confirm each one appears
  as a todo (`list_todos`, or the operator board).
- Confirm nothing still reads the retired variables. This should print nothing:

  ```sh
  env | cut -d= -f1 | grep -E '^SWITCHBOARD_((GITHUB|GITEA|STRIPE|SLACK)_SECRET|LEGACY_RECEIVER_ENDPOINT_ID)$'
  ```

- Confirm the build reports the new version (`switchboard version`, or `/healthz`), so you
  know the upgrade actually took rather than failing back to the old binary.

### If you need to go back

Restore the dump:

```sh
pg_restore --clean --if-exists --dbname "$SWITCHBOARD_DATABASE_URL" switchboard-pre-v0.3.0.dump
```

Then run the previous image. Anything ingested after the upgrade is lost, because the
rows that referenced the dropped provider rows cannot be reconstructed. This is why the
backup comes first.

### About the published image

`ghcr.io/stump-wtf/switchboard:latest` moves only when a `v*` tag is pushed. Before
v0.3.0 was tagged it resolved to the v0.2.0 build, so a deployment pulling `:latest` could
have been running a release two weeks behind `main` without saying so. Tag `latest` by
its digest rather than trusting the name, and pin a version tag
(`ghcr.io/stump-wtf/switchboard:0.3.0`) if you want an upgrade to be something you choose
rather than something that happens.

## Upgrading to v0.2.0

`v0.2.0` is the first release you can run from a published image. There is no upgrade
path from anything earlier, because there was no earlier release; fresh deployments
follow [Self-hosting](./14-self-hosting.md).
