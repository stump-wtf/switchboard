---
title: Change webhook routing safely
---

# Change webhook routing safely

The operator workflow for editing a webhook's [routing rules](/guides/routing-rules) from the CLI:
back up what is live, dry-run the candidate against real and sample traffic, apply it, verify, and
know how to roll back. Every step saves nothing until the one that does.

```
switchboard webhook rules get WEBHOOK_ID --json > rules.backup.json   # 1. back up
$EDITOR rules.json                                                    # 2. edit a copy
switchboard webhook rules test WEBHOOK_ID --file rules.json --event 812   # 3. dry-run
switchboard webhook rules set WEBHOOK_ID --file rules.json            # 4. apply
switchboard webhook rules test WEBHOOK_ID --event 812                 # 5. verify
```

The flags for each step are in the [CLI reference](/guides/cli-reference). Rule syntax — `expr`,
actions, `params` — is [routing rules](/guides/routing-rules), and the
[routing cookbook](/guides/routing-cookbook) has tested rule recipes.

## 1. Back up the live rules

```
switchboard webhook rules get WEBHOOK_ID --json > rules.backup.json
```

`get --json` prints exactly the document `rules set --file` accepts, so this file is both your
backup and your rollback input. Name it with the date, and do not edit it — edit a separate
candidate copy. If the edit goes sideways, step 4 on the backup file puts back what was live.

## 2. Dry-run against real traffic

`rules test` evaluates candidate rules and prints where a delivery would go and which rule
matched. It saves nothing — no event is recorded, no todo is made, no doorbell rings.

Start from deliveries the webhook actually received. `--event` names one by its event id; pick ids
from the event history on the board, and cover each kind of traffic the webhook carries:

```
switchboard webhook rules test WEBHOOK_ID --file rules.json --event 812
switchboard webhook rules test WEBHOOK_ID --file rules.json --event 907
```

Then cover the shapes that may not have arrived yet with a sample body:

```
switchboard webhook rules test WEBHOOK_ID --file rules.json \
  --payload @samples/issue-opened.json --header "X-GitHub-Event: issues" --envelope
```

`--payload` takes a path, `@path`, or `-` for stdin; `--header` is repeatable and supplies the
event-kind headers a provider would. `--envelope` prints the envelope the rules evaluated, which
is the fastest way to see why a jq expression missed.

**A `FAULTED (blocking)` result is a stop.** That delivery would be recorded and routed nowhere —
fix the expression or narrow it before going on.

## 3. Apply

```
switchboard webhook rules set WEBHOOK_ID --file rules.json
```

`set` replaces the whole configuration, and it is guarded twice: the document is validated exactly
as the MCP verb validates it, then dry-run against the webhook's 50 latest deliveries. Any failure
— a typo, a queue outside the grant, a rule that faults on real traffic — names the problem and
leaves the current rules in force.

**Params are kept unless you say otherwise.** A file without a `params` key keeps the stored
params, and `set` says so. To remove them, put `"params": null` (or `{}`) in the file.

## 4. Verify against the saved rules

```
switchboard webhook rules test WEBHOOK_ID --event 812
```

Without `--file`, `rules test` evaluates the rules as saved — the same delivery should now route
where the dry-run predicted. Re-run the rest of your step-2 set too; if anything faults, roll back
and fix the candidate.

## 5. Roll back

```
switchboard webhook rules set WEBHOOK_ID --file rules.backup.json
```

The backup is a valid `set` input as it stands. Verify the same way: `rules test --event` against
the saved rules.

## Edge cases

- **A webhook whose endpoint was revoked** stays readable and testable, but `set` answers `409`.
  Re-vend first — see [Revoking an endpoint](/guides/operator-cli#revoking-an-endpoint).
- **Another human's webhook** is `not found`, exactly like an unknown id.
- **The queues an action may name** are bounded by the webhook's `grant`, printed by
  `rules get`. A queue outside it fails validation, not the delivery.
- Doing this **over MCP instead**: only an endpoint vended with the rule verbs can edit its own
  webhook's rules, and only its own. From the CLI you manage them as the human who owns the
  endpoint, whatever its scope.
