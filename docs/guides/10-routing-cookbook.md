---
title: Routing cookbook
---

# Routing cookbook

Tested recipes for the jq routing rules a webhook's owner sets. Every titled rule block on this
page is the body of one `set_webhook_rules` call, and switchboard's own test suite reads this page
and runs each block through the real rule evaluator against real GitHub, Gitea, and Cairn payload
shapes. If a recipe here stops doing what it says, the build fails.

This page is the practical companion to [Route events with jq rules](/guides/routing-rules), which
is the reference for every field.

## Rule anatomy in one minute

A webhook's routing configuration is an ordered list of rules, an optional default, and optional
parameters:

```json
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "ci-noise", "name": "CI runs", "expr": ".kind == \"workflow_run\"", "action": {"drop": true}}
  ],
  "default_action": {"queue": "inbox"},
  "params": {}
}
```

- **`expr`** is a jq filter over the [routing envelope](/guides/routing-rules#what-a-rule-sees).
  Its first output decides the match: anything except `false` and `null` matches, and a filter
  that outputs nothing does not.
- **First match wins.** Later rules are not evaluated once one matches.
- **`default_action`** applies when nothing matches. With no default, the delivery lands on the
  webhook's own target queue, exactly as it would with no rules at all.
- **`params`** is an object you save with the rules and read in every expression as `$params`.
  Keep allowlists and identities there, so changing who is trusted doesn't mean rewriting
  expressions. A delivery can never change it.

An **action** is one of:

| Action | Effect |
|---|---|
| `{"drop": true}` | Record the event, create no todo, ring no doorbell. |
| `{"queue": "q"}` | A todo on `q` for **every** delivery target (the webhook's endpoint plus its routes). |
| `{"queue": "q", "endpoints": ["<id>"]}` | Only on those targets. |
| `{"queue": "q", "exclusive": true}` | On exactly **one** target: the first whose endpoint is scoped to `q`. |
| `{"queue": "q", "once": true}` | At most one todo per subject (an issue, a Cairn artifact) per queue. A later delivery about the same subject for the same queue is recorded as a repeat and makes no todo. |
| `{"queue": "q", "work_order": true}` | Attach a `work_order` to the todo: the verified provenance that made it eligible (signature, rule, subject). It grants nothing; it lets the worker check where the task came from. |

`endpoints`, `exclusive`, `once`, and `work_order` combine, and all of them need `queue`.

Two envelope projections save a lot of path-hunting:

- **`.issue`** is set for GitHub and Gitea `issues` events (not pull requests), with the same shape
  on both: `action`, `repo`, `number`, `title`, `url`, `state`, `author`, `sender`, `labels` (a list
  of names), and `label_event` (true for a label change on either forge).
- **`.artifact`** is set for Cairn deliveries: `id`, `handle` (`mcp://cairn/<id>`), `url`, `title`,
  `share_type`, `actor_id`, `tags`, and more.

Rules are validated when you save them, and a failed save names the offending rule and leaves the
previous rules in force:

| Check | Limit |
|---|---|
| Rule count | at most 32 |
| Expression size | at most 4096 bytes |
| Rule `id` | 1–64 characters of `A-Z a-z 0-9 _ -`, unique in the list |
| Rule `name` | at most 128 bytes |
| `params` | at most 16 KiB |
| `endpoints` per action | at most 16 |
| Queue | the webhook's target queue, or one of the endpoint's allowed webhook queues |
| Endpoints | each must already be a delivery target of the webhook (`add_webhook_route`) |
| `exclusive` | at least one delivery target must be scoped to the queue |
| Functions | `env`, `$ENV`, `input`, `inputs`, `input_filename`, `debug`, `stderr`, `halt`, `halt_error`, `now`, `localtime`, `strflocaltime`, `import`, `include` are refused |

At delivery time each rule gets 50 ms and the whole list 250 ms, in a separate, memory-capped
process. A rule that errors, times out, or runs out of budget **faults**. Evaluation stops there, no
later rule or default applies, and the delivery is recorded as `faulted` and held in the owner's
quarantine, where no agent is handed it. Rules fail closed.

> **Guard allowlists anyway.** The engine no longer lets a faulting trust rule wave a delivery
> through, and `params` are type-checked at save time. But a faulting rule still means that
> delivery routes **nowhere**, which quietly starves the lane behind it. Read list params through
> `arrays` (`($params.trusted | arrays) // []`), so a missing list evaluates cleanly and trusts no
> one. Every allowlist on this page does.

> **Grant queues twice.** A rule can only name a queue the endpoint was granted as a *webhook
> queue* (the vend wizard's webhooks step). The endpoint's worker only sees todos on its *scoped
> queues* (the queues step). Put every queue your rules route to in both lists. A plain `queue`
> action creates a todo on every target even if that target's scope lacks the queue, and nobody can
> see those todos. `exclusive` is the exception: it only picks a target scoped to the queue.

## Dry-run before you save

`test_webhook_rules` routes a sample through candidate rules and saves nothing. Candidates are
validated exactly like a save, so a dry-run that passes is a save that will pass.

```json
{
  "webhook_id": "<webhook id>",
  "payload": {"action": "opened", "issue": {"title": "flaky test", "labels": [{"name": "size/S"}], "user": {"login": "alice"}}, "sender": {"login": "alice"}},
  "headers": {"X-GitHub-Event": "issues"},
  "rules": [
    {"id": "size-s", "expr": "any(.issue.labels[]?; . == \"size/S\")", "action": {"queue": "small", "once": true}}
  ],
  "default_action": {"drop": true},
  "params": {}
}
```

The result carries the `decision`, the `trace`, the `envelope` the rules saw, and, for `once` and
`work_order` actions, a preview of the `once_key` and `work_order`. Three habits save a lot of
guessing:

- **Give every candidate rule an `id`.** A save mints ids for rules that lack one; a dry-run
  rejects them with `invalid_rule`.
- **Pass the event header.** `.kind` and `.issue` come from the `X-GitHub-Event` / `X-Gitea-Event`
  header, so a dry-run `payload` without `headers` has `.kind == null` and every `.kind == "…"` rule
  quietly misses.
- **Prefer a real delivery.** Pass `event_id` (from `list_webhook_events`) instead of `payload` to
  replay one of this webhook's stored events, headers and all. Then write paths against the
  returned `envelope`, not against a guess.

## Read the trace

Every todo a routed delivery creates carries a `routing` trace, and so does the stored event
(`list_webhook_events`). It says which rule fired, or why none did:

| `stage` | `cause` | Meaning |
|---|---|---|
| `rule` | — | A rule matched. `rule_index`, `rule_id`, and `rule_name` name it. |
| `default` | `no_match_default` | No rule matched; the default action applied. |
| `default` | `rule_not_granted` | A rule matched, but its queue or endpoint is no longer reachable (a route was removed, the endpoint revoked). The default applied instead, and later rules were **not** tried. `rule_id` names the rule. |
| `default` | `default_not_granted` | The default itself is unreachable, so the delivery fell back to the webhook's target queue on every target. |
| `fault` | `error`, `timeout`, `compile_error`, `budget_exhausted` | A rule could not be evaluated. Evaluation stopped at `rule_index` / `rule_id`, and the delivery was recorded as `faulted` and held in quarantine (`rule_fault`). `faults` carries the detail. |
| `trust_gate` | `untrusted_actor` | The trust gate held the delivery before any rule ran. `actor` carries the verdict, and the delivery waits in quarantine (`untrusted_actor`). |

When the evaluator itself cannot run (`sandbox_failure`, `sandbox_busy`), nothing is recorded: the
producer gets `503 routing unavailable` and retries. A dropped delivery, or a `once` repeat, creates
no todo, so its trace lives on the event only. A held one creates only its quarantine item. Filter
for them with `list_webhook_events {"disposition": "faulted"}` (or `"quarantined"`, `"dropped"`).

## Recipes

Order a real rule list the same way every time: **drops first** (untrusted senders, bots, noise),
**then routes**, then a catch-all, then a `default_action` of `{"drop": true}` so anything you did
not anticipate is recorded rather than turned into work.

Payload paths below are the same on GitHub and Gitea: both send `issue.user.login`,
`sender.login`, `pull_request.user.login`, and `requested_reviewer.login`.

### Drop the hook ping and CI noise

Subscribing to fewer events at the source is the real fix (see
[Receive your first webhook](/getting-started/first-webhook)). Keep a rule as the backstop for
the day someone ticks "send me everything":

```json title="set_webhook_rules · drop-ci-noise"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "ping", "name": "the ping a forge sends when the hook is created",
     "expr": ".kind == \"ping\"",
     "action": {"drop": true}},
    {"id": "ci-noise", "name": "CI runs, jobs, checks, and commit statuses",
     "expr": ".kind | IN(\"workflow_run\", \"workflow_job\", \"check_run\", \"check_suite\", \"status\")",
     "action": {"drop": true}}
  ]
}
```

There is no `default_action`, so everything else lands on the webhook's target queue as before.
`workflow_run` alone fires twice per run (`requested` and `completed`), so on a busy repo this
recipe is often most of the queue.

### Route issues by `size/*` label

Send sized issues to a queue per difficulty, park epics, and put new unsized issues in `triage`:

```json title="set_webhook_rules · size-lanes"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "size-s", "name": "size/S",
     "expr": "(.issue.action // \"\" | IN(\"opened\", \"reopened\", \"labeled\", \"label_updated\")) and any(.issue.labels[]?; . == \"size/S\")",
     "action": {"queue": "small", "once": true}},
    {"id": "size-m", "name": "size/M",
     "expr": "(.issue.action // \"\" | IN(\"opened\", \"reopened\", \"labeled\", \"label_updated\")) and any(.issue.labels[]?; . == \"size/M\")",
     "action": {"queue": "medium", "once": true}},
    {"id": "size-l", "name": "size/L",
     "expr": "(.issue.action // \"\" | IN(\"opened\", \"reopened\", \"labeled\", \"label_updated\")) and any(.issue.labels[]?; . == \"size/L\")",
     "action": {"queue": "large", "once": true}},
    {"id": "size-xl", "name": "size/XL is for a human",
     "expr": "(.issue.action // \"\" | IN(\"opened\", \"reopened\", \"labeled\", \"label_updated\")) and any(.issue.labels[]?; . == \"size/XL\")",
     "action": {"queue": "hold", "once": true}},
    {"id": "unsized", "name": "new issues with no size label",
     "expr": "(.issue.action // \"\" | IN(\"opened\", \"reopened\")) and ([.issue.labels[]? | select(startswith(\"size/\"))] | length) == 0",
     "action": {"queue": "triage", "once": true}}
  ],
  "default_action": {"drop": true}
}
```

`.issue` reads the same on both forges, even though GitHub reports a label change as `labeled` and
Gitea as `label_updated`. Its `labels` are the issue's current labels, so the rules check the list
rather than the label that changed. Things to know:

- **`once` stops label churn becoming duplicate work.** Adding a second, unrelated label sends
  another `issues` event with the size label still present. `once` records that delivery as a
  repeat (it answers `{"repeat": true}`) instead of creating a second todo. Re-sizing an issue to a
  different size is a different queue, so it routes again.
- **The first matching size wins.** An issue carrying both `size/S` and `size/L` goes to `small`.
- **A non-size label on an unsized issue is dropped.** Only `opened` and `reopened` go to triage,
  so a triage worker adding its own label can't loop the issue back to itself.

### Only act on trusted people

> **Prefer `trusted_actors`.** A `github`, `gitea` or `cairn` webhook has a first-class trust list,
> checked in Go before any rule runs, and a stranger's delivery waits in quarantine instead of being
> dropped. Set it with `set_trusted_actors` (see
> [Trusted actors](/guides/routing-rules#trusted-actors-who-may-start-work)), and read the verdict
> in rules as `.actor`. The jq rule below still works as an extra layer, but it drops where the gate
> holds, and every rule set has to repeat it. [Outside intake from the public GitHub mirrors](#outside-intake-from-the-public-github-mirrors)
> shows the `trusted_actors` form end to end.

Drop anything a stranger opened, commented on, or labeled. It goes first, so no later rule can
route it:

```json title="set_webhook_rules · trusted-authors"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "untrusted", "name": "sender or author not in params.trusted",
     "expr": "(.kind | IN(\"issues\", \"issue_comment\", \"pull_request\")) and ((($params.trusted | arrays) // []) as $t | [.payload.sender.login, (.payload.issue.user.login // .payload.pull_request.user.login)] | any((. // \"\") as $who | any($t[]; . == $who) | not))",
     "action": {"drop": true}}
  ],
  "params": {"trusted": ["alice", "bob"]}
}
```

`sender` is whoever caused *this* delivery (the labeler, the commenter); the issue or pull request
`user` is its author. Checking both stops a stranger from steering work by labeling a trusted
issue, and stops a trusted labeler from promoting a stranger's issue into work. To trust someone
new, save the same rules with a longer `trusted` list. `set_webhook_rules` replaces rules, default,
and params together, and omitting `params` clears them, which (by design) makes this rule drop
everyone.

This is provenance switchboard can check: the sender and author come from a delivery whose
signature verified. The issue *text* is a different matter: it is still whatever the author typed.
See [Security model](/guides/security-model).

### Send review requests only to the requested reviewer

This is the recipe with a lesson behind it. Say one forge webhook feeds two identities' workers,
`alice` and `bob`, each on its own endpoint. A plain `{"queue": "reviews"}` action puts the review
request on **every** delivery target. So when alice asks bob to review her pull request, alice's
own worker receives the request too, and reviews her own work.

Pin each request to the reviewer's endpoint instead. The webhook belongs to one endpoint and has a
route (`add_webhook_route`) to the other, so both are delivery targets:

```json title="set_webhook_rules · review-requests"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "self-review", "name": "never deliver a review of your own pull request",
     "expr": ".kind == \"pull_request\" and .payload.action == \"review_requested\" and (.payload.requested_reviewer.login // \"\") == (.payload.pull_request.user.login // \"\")",
     "action": {"drop": true}},
    {"id": "review-alice", "name": "review requested from alice",
     "expr": ".kind == \"pull_request\" and .payload.action == \"review_requested\" and .payload.requested_reviewer.login == \"alice\"",
     "action": {"queue": "reviews", "endpoints": ["<alice endpoint id>"]}},
    {"id": "review-bob", "name": "review requested from bob",
     "expr": ".kind == \"pull_request\" and .payload.action == \"review_requested\" and .payload.requested_reviewer.login == \"bob\"",
     "action": {"queue": "reviews", "endpoints": ["<bob endpoint id>"]}},
    {"id": "other-review-requests", "name": "team requests, removals, and anyone else",
     "expr": ".kind == \"pull_request\" and (.payload.action | IN(\"review_requested\", \"review_request_removed\"))",
     "action": {"drop": true}}
  ],
  "default_action": {"drop": true}
}
```

Find an endpoint's id with `switchboard endpoint list --json`. A request for a team has no
`requested_reviewer`, so it falls through to `other-review-requests` and is dropped rather than
landing everywhere.

If each identity has **its own** forge webhook instead, put this on each one with that identity in
`params`. It drops review requests for anyone else and any trigger to review the identity's own
pull request, and lets everything else through to the webhook's target queue:

```json title="set_webhook_rules · review-requests-own-webhook"
{
  "webhook_id": "<alice's webhook id>",
  "rules": [
    {"id": "review-request-not-for-me", "name": "review requests for anyone other than params.identity",
     "expr": ".kind == \"pull_request\" and ((.payload.action // \"\") | IN(\"review_requested\", \"review_request_removed\")) and ((($params.identity // \"\") == \"\") or ((.payload.requested_reviewer.login // \"\") != $params.identity))",
     "action": {"drop": true}},
    {"id": "own-pr-review-trigger", "name": "never review a pull request params.identity authored",
     "expr": ".kind == \"pull_request\" and ($params.identity // \"\") != \"\" and ((.payload.pull_request.user.login // \"\") == $params.identity) and ((.payload.action // \"\") | IN(\"opened\", \"reopened\", \"synchronized\", \"synchronize\", \"edited\", \"ready_for_review\", \"review_requested\"))",
     "action": {"drop": true}}
  ],
  "params": {"identity": "alice"}
}
```

With no `identity`, every review request is dropped, so a copy installed without its params fails
closed.

### Drop Renovate's Dependency Dashboard

Renovate keeps one issue open per repository and edits it constantly. Every edit is a delivery:

```json title="set_webhook_rules · renovate-dashboard"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "renovate-dashboard", "name": "Renovate's dashboard issue and its bot account",
     "expr": "(.kind | IN(\"issues\", \"issue_comment\")) and (((.payload.issue.title // \"\") == \"Dependency Dashboard\") or ((.payload.issue.user.login // \"\") | test(\"^renovate\")))",
     "action": {"drop": true}}
  ]
}
```

The bot account is `renovate[bot]` on GitHub and usually `renovate` or `renovate-bot` on a
self-hosted Gitea, which `^renovate` covers. To drop Renovate's pull requests too, add
`.payload.pull_request.user.login` to the same test.

### Route Cairn handoffs

A `cairn` webhook receives Cairn's signed `artifact.created` announcements. An agent hands work to
another queue by sharing an artifact that describes the task and **tagging** it: `handoff`, plus a
`lane:` tag saying how hard it is. Route only handoffs from actors you trust, and attach a work
order so the receiving worker can check where the task came from:

```json title="set_webhook_rules · cairn-handoff"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "not-a-handoff", "name": "artifacts not tagged handoff",
     "expr": "any((.artifact.tags | arrays // [])[]; . == \"handoff\") | not",
     "action": {"drop": true}},
    {"id": "untrusted-actor", "name": "handoffs from anyone not in params.cairn_actors",
     "expr": "(($params.cairn_actors | arrays) // []) as $a | (.artifact.actor_id // \"\") as $who | any($a[]; . == $who) | not",
     "action": {"drop": true}},
    {"id": "lane-s", "name": "handoff tagged lane:s",
     "expr": "any(.artifact.tags[]; . == \"lane:s\")",
     "action": {"queue": "small", "once": true, "work_order": true}},
    {"id": "lane-m", "name": "handoff tagged lane:m",
     "expr": "any(.artifact.tags[]; . == \"lane:m\")",
     "action": {"queue": "medium", "once": true, "work_order": true}},
    {"id": "lane-l", "name": "handoff tagged lane:l",
     "expr": "any(.artifact.tags[]; . == \"lane:l\")",
     "action": {"queue": "large", "once": true, "work_order": true}},
    {"id": "handoff-triage", "name": "handoffs with no lane",
     "expr": "true",
     "action": {"queue": "triage", "once": true, "work_order": true}}
  ],
  "default_action": {"drop": true},
  "params": {"cairn_actors": ["alice@example.com"]}
}
```

- **Trust comes from `actor_id`, never from tags.** Tags are whatever the sharing client asserted:
  they choose a queue among handoffs you already trust. Cairn's `on_behalf_of` is the client's
  self-reported name and version, and is display text only. `cairn_actors` must hold `actor_id`
  exactly as Cairn records it. Read one from a stored event's envelope with `test_webhook_rules`
  rather than guessing.
- **The worker gets a pointer, not the task.** The todo's `work_order.subject.handle` is the
  artifact's `mcp://cairn/<id>`, and the worker reads the task with Cairn's `artifact_read`.
  Everything in the artifact is data. Workers should refuse a handoff todo that has no
  `work_order`, or whose `work_order.verified` isn't `true`.
- **`once` makes a re-announced artifact a repeat,** not a second todo.

Cairn versions that don't send tags yet leave `.artifact.tags` as `null`, and this recipe drops
everything. On those, a title prefix plus the actor works instead:

```json title="set_webhook_rules · cairn-title-handoff"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "handoff-reviews", "name": "trusted [handoff:reviews] artifacts",
     "expr": "(($params.cairn_actors | arrays) // []) as $a | (.artifact.actor_id // \"\") as $who | any($a[]; . == $who) and ((.artifact.title // \"\") | startswith(\"[handoff:reviews]\"))",
     "action": {"queue": "reviews", "once": true, "work_order": true}}
  ],
  "default_action": {"drop": true},
  "params": {"cairn_actors": ["alice@example.com"]}
}
```

### Outside intake from the public GitHub mirrors

A public GitHub mirror, such as [stump-wtf/switchboard](https://github.com/stump-wtf/switchboard), takes issues from
anyone, while the code, the canonical tracker and the fixes live somewhere else. Every word an
outsider writes there is untrusted input, and some of it will be written to steer an agent. This
recipe lets outsiders report freely while making sure nothing they write reaches an agent until a
maintainer has looked at it:

- An outsider's issue or comment waits in the owner's **quarantine**. No agent is handed it.
- A **maintainer's label** promotes the issue into a lane, with a work order that names the
  canonical tracker.
- The work order still says the text is an outsider's (`author_trusted: false`), all the way
  downstream.

**1. One webhook for the whole organization.** Create a signed `github` webhook whose trust list is
the maintainers, matched on the **sender**:

```json title="create_webhook · public-mirror-intake"
{
  "source_type": "github",
  "target_queue": "inbox",
  "trusted_actors": {"logins": ["joestump", "joestump-agent"], "match": "sender"}
}
```

Install it once at the organization level, so every mirror, including ones you add later, is
covered: **Organization settings → Webhooks → Add webhook**. Use the `ingest_url` (it starts with
your `$SWITCHBOARD_URL`) as the **Payload URL**, `application/json` as the **Content type**, and the
`signing_secret` as the **Secret**. Under **Let me select individual events**, tick only **Issues**
and **Issue comments**. [Receive your first webhook](/getting-started/first-webhook#github) walks
through the same form.

`match: "sender"` is what makes promotion work. The maintainer who labels an issue is its sender,
so the `labeled` delivery is trusted even though an outsider wrote the issue.

**2. The rules.** Route a trusted size label to its lane, drop what has no issue, and leave
everything else in quarantine:

```json title="set_webhook_rules · public-mirror-intake"
{
  "webhook_id": "<webhook id>",
  "rules": [
    {"id": "not-issue", "name": "trusted deliveries with no issue subject: maintainers' own comments, the hook ping",
     "expr": ".issue == null",
     "action": {"drop": true}},
    {"id": "switchboard-s", "name": "canonical tracker: stump.wtf/switchboard",
     "expr": ".issue.repo == \"stump-wtf/switchboard\" and .issue.action == \"labeled\" and .issue.label == \"size/S\"",
     "action": {"queue": "lane-s", "exclusive": true, "once": true, "work_order": true}},
    {"id": "switchboard-m", "name": "canonical tracker: stump.wtf/switchboard",
     "expr": ".issue.repo == \"stump-wtf/switchboard\" and .issue.action == \"labeled\" and .issue.label == \"size/M\"",
     "action": {"queue": "lane-m", "exclusive": true, "once": true, "work_order": true}}
  ],
  "default_action": {"quarantine": true}
}
```

The trust gate runs before any of these rules, so they only ever see a maintainer's deliveries:

| Delivery | Outcome |
|---|---|
| An outsider opens an issue, or comments on one | Quarantined by the trust gate (`untrusted_actor`). No rule runs. |
| A maintainer adds `size/S` or `size/M` to an issue on a mapped mirror | Routed to `lane-s` or `lane-m` on one endpoint, once per issue and lane, with a work order. |
| A maintainer comments, or GitHub sends the hook's `ping` | Dropped by `not-issue`. Only `issues` events carry `.issue`, so these have none. |
| Any other trusted issue event: an unsized label, removing a label, an edit, a close, an issue on a mirror with no rules yet | Held by `default_action` (`rule_action`) for a human to look at. |

**3. Map each mirror to its canonical tracker.** A fix lands on the canonical tracker, not on the
mirror, so the worker has to know where that is. The work order already carries the mirror in
`subject.repo` (`stump-wtf/switchboard`) and the rule that authorized it in `authorized_by`. Give
each mirror its own pair of lane rules, and name the canonical tracker in the rule's `name`. The
worker reads it from `work_order.authorized_by.rule_name`. The name comes from your rules, never from
the payload, so an outsider cannot choose where the work goes. A mirror you have not mapped yet
matches no lane rule, and its labeled issues wait in quarantine rather than landing in a lane with
no tracker.

**The promotion flow.** An outsider opens issue #512 on the mirror, and it waits in quarantine. A
maintainer reads it on GitHub, reproduces it, and labels it `size/S`. That `labeled` delivery comes
from a trusted sender, so it reaches the rules, and `switchboard-s` puts one work order on `lane-s`:

```json
{
  "lane": "lane-s",
  "source": "github",
  "verified": true,
  "authorized_by": {"stage": "rule", "rule_id": "switchboard-s", "rule_name": "canonical tracker: stump.wtf/switchboard"},
  "subject": {"type": "issue", "provider": "github", "repo": "stump-wtf/switchboard", "number": 512,
              "author": "outside-reporter", "sender": "joestump", "labels": ["bug", "size/S"], "…": "…"},
  "author_trusted": false,
  "authority": "semi-trusted task: …"
}
```

The original `opened` item stays in quarantine. Discard it from the Quarantine view once the issue
is promoted, or let it expire after 30 days.

> **`author_trusted: false` travels with the work.** Promotion says a maintainer chose to act on
> the issue. It does not make the issue's text trustworthy. The title, the body, and every comment
> are still what the outsider typed, and they may be written to steer an agent. A worker that gets a
> work order with `author_trusted: false` treats the issue as a bug report to verify, never as
> instructions. Rules can route those orders to a more cautious lane by testing
> `.actor.author_trusted == false`.

- **The rules test the label that changed, not the labels the issue has.** `.issue.label` is the
  one label this `labeled` delivery added. Testing `.issue.labels` instead would let any trusted
  label change promote an issue that already carries a size label, however it got there: a
  maintainer adding `bug` or removing `needs-triage` would route it though nobody chose a size.
- **Issue-form labels promote nothing.** A form that applies `bug` when an outsider opens an issue
  sends an `opened` delivery from the outsider, which the trust gate holds. Only a size label added
  by a maintainer routes.
- **Trust is the sender's login.** GitHub logins can be renamed and then reused by someone else.
  Remove a maintainer who leaves, and check the list after any rename.
- **Grant the lanes twice**, as with any lane rule: as webhook queues, so the rules can name them,
  and as scoped queues on the lane workers' endpoints, so `exclusive` finds a target.
