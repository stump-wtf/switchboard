# Routing rule packs

A rule pack is a routing configuration checked into the repo so it can be reviewed, tested, and installed the same way everywhere. A pack file is **exactly** a `set_webhook_rules` body minus `webhook_id`: `{"rules": [...], "default_action": {...}, "params": {...}}` (`default_action` may be omitted). The Go tests that exercise the packs reject unknown fields.

Governing: [ADR-0025](../../adrs/ADR-0025-handoff-work-orders-and-difficulty-lanes.md), [SPEC-0020](../../openspec/specs/event-routing/spec.md). Operator runbook: [guide 08](../../guides/08-handoff-lanes.md).

**Changing params.** Call `set_webhook_rules` with the same `rules` and `default_action` and the new `params`. `set` replaces all three; omitting `params` clears them, and both packs then fail closed. Every allowlist is a JSON list of strings. Save-time validation refuses any params value that is not a string, number, boolean, or homogeneous list, and routing fails closed: a rule that faults stops evaluation, and the delivery is recorded as `faulted` and held in the owner's quarantine (SPEC-0026). The fleet pack still guards each allowlist with `| arrays` / `| strings`, so a mistyped one (`"cairn_actors": "joestump-agent"`) evaluates cleanly and admits no one instead of faulting every delivery. `TestFleetPackFailsClosedWithMistypedParams` holds every allowlist to that. Write any new allowlist rule the same way.

## `fleet.json` — handoff work orders and difficulty lanes

Install `fleet.json` on the **router** webhooks: one per Gitea org, one GitHub, and one cairn. Never install it on per-identity pool hooks, because dedup is per webhook and a second copy is a second work order.

### Lanes

Lanes are by difficulty. Each lane is one queue served by one pool endpoint scoped to exactly that queue; work lanes run on tars as `joestump-agent`. Every lane action is `{"queue": …, "exclusive": true, "once": true, "work_order": true}`.

| Queue | Takes | Workers |
|---|---|---|
| `lane-s` | `size/S`; cairn `lane:s`, or `size:s` when unpinned | Qwen3.8-27B, local via LiteLLM → vLLM |
| `lane-m` | `size/M`; cairn `lane:m`, or `size:m` when unpinned | `zai/glm-5.3-flash` direct and `hyper/glm-5.3-flash` direct |
| `lane-l` | `size/L`; cairn `lane:l`, or `size:l` when unpinned | `zai/glm-5.3` direct and `hyper/glm-5.3` direct |
| `lane-vision` | cairn `lane:vision` | `hyper/deepseek-v4.1-flash` direct |
| `triage` | unsized issues opened or reopened, and unsized, unpinned cairn handoffs. The worker sizes the item with a label; the label event re-routes it. | Qwen |
| `hold` | `size/XL`, `HUMAN`, more than one `size/*` label, cairn `size:xl`, and more than one `lane:` or `size:` tag. Surfaced for Joe, never auto-executed. | none |

Several workers on one queue, on different provider accounts, are competing consumers: they claim with `FOR UPDATE SKIP LOCKED`, so each todo runs once, and when one provider's quota walls the other keeps draining. Only the local Qwen goes through LiteLLM.

"Unpinned" means the handoff has no `lane:s|m|l|vision` tag — `lane:auto`, no `lane:` tag, or an unknown one.

### Params

| Param | Meaning |
|---|---|
| `require_verified` | `true` (default): a delivery whose signature did not verify is never a work order. |
| `trusted_humans` | Forge logins of trusted people. |
| `trusted_agents` | Forge logins of trusted agent identities. |
| `cairn_actors` | Cairn `actor_id`s allowed to hand off work, exactly as cairn records them: a `CAIRN_API_TOKENS` entry's actor, a PAT's owner, or the OIDC login (email or sub) for an MCP OAuth client. The checked-in values match only static-token actors; a harness on a PAT records the owner's login, so replace them with the ids cairn actually records ([guide 08](../../guides/08-handoff-lanes.md), step 5) or every handoff drops. |
| `repo_prefixes` | `owner/` or `owner/repo` prefixes whose issues may route. |

Issue authors, and labelers on label events, must be in `trusted_humans ∪ trusted_agents`. Tags, labels, and cairn's `on_behalf_of` never grant trust. `on_behalf_of` is the MCP client's self-reported `name/version`, e.g. `claude-code/2.1.0`.

### Rule order

First match wins, and the default is `drop`.

| # | id | Effect |
|---|---|---|
| 1 | `unverified` | drop unless the signature verified |
| 2 | `cairn-not-handoff` | drop cairn artifacts whose `tags` lack `handoff` |
| 3 | `cairn-untrusted-actor` | drop cairn handoffs whose `actor_id` is not in `cairn_actors` |
| 4 | `cairn-ambiguous` | hold handoffs with more than one `lane:` tag or more than one `size:` tag |
| 5 | `cairn-size-xl` | hold `size:xl` handoffs, even when pinned |
| 6–9 | `cairn-lane-{s,m,l,vision}` | route to the pinned lane |
| 10–12 | `cairn-size-{s,m,l}` | unpinned handoffs by size |
| 13 | `cairn-triage` | remaining handoffs to triage |
| 14 | `not-an-issue` | drop everything that is not an issue event, pull requests and review requests included |
| 15 | `repo-not-allowlisted` | drop issues outside `repo_prefixes` |
| 16 | `bot-issue` | drop Renovate/`[bot]` authors, "Dependency Dashboard", and the `BOT` verdict |
| 17 | `untrusted-author` | drop issues by untrusted authors |
| 18 | `untrusted-labeler` | drop label events made by untrusted senders |
| 19 | `not-open` | drop closed issues |
| 20 | `not-routable-action` | drop everything but `opened`, `reopened`, `labeled`, `label_updated` |
| 21 | `human-verdict` | hold `HUMAN` |
| 22 | `size-xl` | hold `size/XL` |
| 23 | `ambiguous-size` | hold more than one `size/*` label |
| 24–26 | `size-s`, `size-m`, `size-l` | route to `lane-s`, `lane-m`, `lane-l` |
| 27 | `unsized-new-issue` | opened/reopened with no size label goes to triage |

A label event on an unsized issue matches none of 21–27, so it drops. That is what keeps triage from looping.

### Cairn handoff tags

Tags are lower-case and matched exactly.

| Tag | Meaning |
|---|---|
| `handoff` | required: marks the artifact as a handoff |
| `lane:s` \| `lane:m` \| `lane:l` \| `lane:vision` \| `lane:auto` | pin a lane, or let `size:` decide |
| `size:s` \| `size:m` \| `size:l` \| `size:xl` | difficulty; `size:xl` is always held |
| `repo:owner/name`, `issue:owner/repo#n` | where the work lives |
| `source:…` | the sweep that wrote it, e.g. `source:morning-brief` |
| `reply:cairn-comment` \| `reply:signal` | where to report back: a comment on this artifact, or Signal |

## `pool-review.json` — single-identity review routing

Install `pool-review.json` on **each** identity's forge pool webhooks, with `params.identity` set to that identity (`joestump` on kitt's pools, `joestump-agent` on tars' pools). It has no `default_action`, so everything it does not drop still goes to the pool's target queue.

| # | id | Effect |
|---|---|---|
| 1 | `review-request-not-for-me` | drop `pull_request` `review_requested` / `review_request_removed` events whose `requested_reviewer.login` is not `$params.identity` |
| 2 | `own-pr-review-trigger` | drop `pull_request` `opened`, `reopened`, `synchronized`, `synchronize`, `edited`, `ready_for_review`, `review_requested` events whose `pull_request.user.login` is `$params.identity` |
| 3 | `bot-comment-noise` | drop `issue_comment` and `pull_request_comment` events whose `sender.login` is in `$params.bot_actors` |
| 4 | `bot-review-outcome` | drop `pull_request_approved` / `pull_request_rejected` events whose `sender.login` is in `$params.bot_actors` |
| 5 | `review-outcome-not-my-pr` | drop `pull_request_approved` / `pull_request_rejected` events whose `pull_request.user.login` is **not** `$params.identity` |

Human and sibling-agent comments, review requests addressed to the identity, and issue events still reach the pool. With no `identity` param, review requests and review outcomes both fail closed (rules 1 and 5).

**Rules 3 to 5 exist because queue latency is release latency.** Branch protection makes the sibling identity's approval the merge gate, so a real review request sitting behind machine chatter delays every release. Measured over 12.4h of real traffic on both pools (240 deliveries, 222 todos, ~430 todos/day), these three rules remove ~180 todos/day (42%) and leave every human signal intact.

Three things about them that look wrong until you know why:

- **They key on `sender.login`, never `comment.user.login`.** `pull_request_comment`, `pull_request_approved` and `pull_request_rejected` carry **no** `comment` object at all — the text lives in `review.content`. Across every delivery these webhooks have ever received, `sender.login` and `comment.user.login` never disagree. Do not "fix" these rules to read the comment author.
- **The sender is bound with `as $s` before `any()`.** Inside `any(gen; cond)` the `.` is the generator's element, so the obvious `any($params.bot_actors[]; . == (.payload.sender.login // ""))` indexes a *string* and faults, and a faulted rule stops evaluation, so every delivery is held in quarantine as `rule_fault`, human review requests included. `pool_review_pack_test.go` fails if anyone rewrites them that way.
- **Rule 5 keeps outcomes on the identity's own PRs.** That is the author's "your PR is approved and can merge" signal. Dropping every outcome instead removes ~41 more todos/day and costs exactly the signal branch protection depends on; it was considered and rejected. Keep the asymmetry.

`bot_actors` is an exact-match list of logins. Only `gitea-actions` appears in real traffic today — aibot posts as `gitea-actions`, and Renovate has never posted to these webhooks — but `aibot`, `renovate-bot` and `renovate` are listed so they are covered if they ever do. Like every allowlist here it is `| arrays` / `| strings` guarded, so a mistyped param drops nothing rather than faulting every delivery.

Why it exists: both identities' org webhooks deliver every pull request event to both identities' pools. On 2026-09-11 a `joestump` pool worker received review requests meant for `joestump-agent` and reviewed and merged `joestump`'s own pull requests (harness#307, dotfiles#242).

## Testing

- **Live:** `test_webhook_rules` with a sample `payload` and `headers`, or a stored `event_id` of the webhook. The result shows the `decision`, the exclusive target endpoint, the `once_key`, the `work_order`, and the envelope. Candidate `params` can be tried without saving.
- **In the repo:**
  - `internal/routing/fleet_pack_test.go` validates `fleet.json` against a lanes grant and routes every case in `internal/routing/testdata/fleet/cases.json` (real-shape Gitea, GitHub, and cairn samples), in-process and again through the sandbox child.
  - `internal/routing/pool_review_pack_test.go` routes review requests, own-PR triggers, comments, and issue events for both identities through `pool-review.json`, in-process and through the sandbox child.
  - Add a case whenever you change a rule.
