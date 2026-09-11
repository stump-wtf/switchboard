# Routing rule packs

A rule pack is a routing configuration checked into the repo so it can be reviewed, tested, and installed the same way everywhere. A pack file is **exactly** a `set_webhook_rules` body minus `webhook_id`: `{"rules": [...], "default_action": {...}, "params": {...}}`. The Go test that exercises it rejects unknown fields.

## `fleet.json` — handoff work orders and difficulty lanes

Governing: [ADR-0025](../../adrs/ADR-0025-handoff-work-orders-and-difficulty-lanes.md), [SPEC-0020](../../openspec/specs/event-routing/spec.md). Operator runbook: [guide 08](../../guides/08-handoff-lanes.md).

Install `fleet.json` on the **router** webhooks: one per Gitea org, one GitHub, and one cairn. Never install it on per-identity pool hooks, because dedup is per webhook and a second copy is a second work order.

### Lanes

Each lane is one queue served by one pool endpoint scoped to exactly that queue. Every lane action is `{"queue": …, "exclusive": true, "once": true, "work_order": true}`.

| Queue | Model | Takes |
|---|---|---|
| `lane-local` | Qwen3.8-27B via LiteLLM (free) | `size/S`; cairn `lane=local` or `size=S` |
| `lane-zai-flash` | `zai/glm-5.3-flash` direct | `size/M`; cairn `lane=zai-flash` or `size=M` |
| `lane-zai` | `zai/glm-5.3` direct | `size/L`; cairn `lane=zai` or `size=L` |
| `lane-hyper` | cheap Hyper flash via LiteLLM | overflow/general; cairn `lane=hyper` |
| `lane-vision` | Hyper `deepseek-v4.1-flash` (vision) | screenshots/UI; cairn `lane=vision` |
| `triage` | Qwen | unsized issues opened or reopened, and unsized, unpinned cairn handoffs. The worker sizes the item and labels it; the label event re-routes it. |
| `hold` | no worker | `size/XL`, `HUMAN`, ambiguous sizes, cairn `size=XL`, and cairn handoffs on behalf of someone untrusted. These are surfaced for Joe and never auto-executed. |

### Params

| Param | Meaning |
|---|---|
| `require_verified` | `true` (default): a delivery whose signature did not verify is never a work order. |
| `trusted_humans` | Forge logins and cairn `on_behalf_of` values of trusted people. |
| `trusted_agents` | Forge logins of trusted agent identities. |
| `cairn_actors` | Cairn `actor_id`s (the actor of a `CAIRN_API_TOKENS` entry) allowed to hand off work. |
| `repo_prefixes` | `owner/` or `owner/repo` prefixes whose issues may route. |

Issue authors, and labelers on label events, must be in `trusted_humans ∪ trusted_agents`. Labels never grant trust.

**Changing an allowlist.** Call `set_webhook_rules` with the same `rules` and `default_action` and the new `params`. `set` replaces all three; omitting `params` clears them. The pack then fails closed and nothing reaches a lane.

### Rule order

First match wins, and the default is `drop`.

| # | id | Effect |
|---|---|---|
| 1 | `unverified` | drop unless the signature verified |
| 2 | `cairn-not-handoff` | drop cairn artifacts without `labels.handoff == "true"` |
| 3 | `cairn-untrusted-actor` | drop cairn handoffs whose `actor_id` is not in `cairn_actors` |
| 4 | `cairn-on-behalf-untrusted` | hold handoffs on behalf of an untrusted principal |
| 5 | `cairn-size-xl` | hold XL handoffs |
| 6–10 | `cairn-lane-{local,zai-flash,zai,hyper,vision}` | route to the pinned lane |
| 11–13 | `cairn-size-{s,m,l}` | unpinned handoffs by size |
| 14 | `cairn-triage` | remaining handoffs to triage |
| 15 | `not-an-issue` | drop everything that is not an issue event, pull requests included |
| 16 | `repo-not-allowlisted` | drop issues outside `repo_prefixes` |
| 17 | `bot-issue` | drop Renovate/`[bot]` authors, "Dependency Dashboard", and the `BOT` verdict |
| 18 | `untrusted-author` | drop issues by untrusted authors |
| 19 | `untrusted-labeler` | drop label events made by untrusted senders |
| 20 | `not-open` | drop closed issues |
| 21 | `not-routable-action` | drop everything but `opened`, `reopened`, `labeled`, `label_updated` |
| 22 | `human-verdict` | hold `HUMAN` |
| 23 | `size-xl` | hold `size/XL` |
| 24 | `ambiguous-size` | hold more than one `size/*` label |
| 25–27 | `size-s`, `size-m`, `size-l` | route to `lane-local`, `lane-zai-flash`, `lane-zai` |
| 28 | `unsized-new-issue` | opened/reopened with no size label goes to triage |

A label event on an unsized issue matches none of 22–28, so it drops. That is what keeps triage from looping.

### Testing

- **Live:** `test_webhook_rules` with a sample `payload` and `headers`, or a stored `event_id` of the router webhook. The result shows the `decision`, the exclusive target endpoint, the `once_key`, the `work_order`, and the envelope. Candidate `params` can be tried without saving.
- **In the repo:** `internal/routing/fleet_pack_test.go` validates the pack against a lanes grant and routes every case in `internal/routing/testdata/fleet/cases.json` (real-shape Gitea, GitHub, and cairn samples). It routes them in-process and again through the sandbox child. Add a case whenever you change a rule.
