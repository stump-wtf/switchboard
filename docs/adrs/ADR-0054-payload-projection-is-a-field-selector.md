---
status: proposed
date: 2026-10-26
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0024, ADR-0038]
related: [ADR-0022, ADR-0025, ADR-0034, ADR-0042]
---

# ADR-0054: Payload Projection Is A Field Selector, Not jq

**Status note:** this ADR is **proposed**. It ships with an implementation so the choices can be
argued against working code rather than prose; nothing here is settled until Joe accepts it. It
answers [issue #585](https://gitea.stump.rocks/stump.wtf/switchboard/issues/585), imported from the
public mirror's [stump-wtf/switchboard#36](https://github.com/stump-wtf/switchboard/issues/36),
whose thread holds no maintainer decision — only the mirror acknowledgement. The three choices the
report left open (the shape of the projection syntax, whether claim keeps an escape hatch, and where
the projection runs) are decided here.

## Context and problem statement

A webhook todo carries its producer's whole delivery body, and the tools hand it back in full. For a
Gitea issue event that is roughly 8 KB per todo, almost all of it the `repository`, `owner` and
`sender` objects a consumer never opens. The report measured `list_todos` at about 450,000
characters for 50 rows, and claims at 7 to 9 KB each across several hundred agent runs, most of them
"claim one, look up live state, complete" cycles: the bytes are paid for and then ignored, because a
label snapshot in a todo is stale by the time anyone reads it. Every byte of a tool response is
model context, so this is not a cosmetic cost — it is the difference between a queue a small worker
can drain and one that wedges it (the 1.07 MB listing that filled a 196K-token window twice, which
`toRow`'s compact rows already fixed for the listing case).

The report asks for three things: a slim list (already on `main`), a claim that can return a
projection, and a projection applied at intake per rule so every consumer benefits without knowing
the feature exists. The design decisions the report leaves open:

1. **Syntax.** The report writes its intake example as a jq expression. Is jq the projection
   language, and where does it run?
2. **The escape hatch.** Does `claim` still hand back the whole payload when nobody asks for less?
3. **Failure.** When a declared projection yields nothing useful, does the consumer get an empty
   object or the body it would have got before?

## Decision

### 1. A projection is an ordered list of field selectors, in one package, used in three places

`internal/projection` owns the syntax and the walk. A selector is a dot-separated path of field
names, and `[]` after a name walks that array:

```text
action
issue.number
issue.labels[].name
repository.full_name
```

Two edges of that grammar, so a rule author meets them in the guide rather than on live traffic: a
`[]` always follows a field name (`a[][].b` is refused at parse), and a selector does not descend
into an element that is itself an array — `a[].b` over `[[{"b": 1}]]` matches nothing rather than
reaching inside it.

It appears in exactly three positions, with the same meaning in each:

* a routing rule's `action.payload_fields` — what intake **stores** on the todo;
* `payload_fields` on `claim`, `claim_next` and `get_todo` — what that **response** carries, the
  stored payload untouched;
* `payload_fields` on `list_todos` — the only way a listing carries body bytes at all, since rows
  stay payload-free unless a caller names what it wants.

The CLI mirrors them: `switchboard todo claim ID --fields issue.number,repository.full_name`, and the
same `--fields` on `todo next`, `todo get` and `todo list`. One name and one syntax in every place a
projection is declared; a second spelling per surface is how a feature stops being discoverable.

### 2. Not jq, because a projection runs where jq cannot

Routing's jq runs out of process (ADR-0024, ADR-0038) precisely because no jq subset that stays
useful also bounds memory: one builtin call can allocate gigabytes before the next context check, and
switchboard is multi-tenant. A projection has to run in the receiving process — at intake, on the
delivery path, on somebody else's document; at claim, inside the verb. Two ways to have jq there,
and both are worse:

* **A second sandbox round trip** per delivery to compute the projected body. The child protocol
  today returns a rule index and nothing else; making it return document bytes means a new protocol
  version, a new output cap, and a child whose output is data an agent will read — a wider surface
  than "which rule matched" for no byte saving a selector would not have achieved anyway.
* **jq in-process**, which reopens exactly the memory hole the sandbox exists to close, for the least
  powerful use of jq there is.

A selector list cannot allocate: it walks a document that was decoded anyway, copies pointers, and
stops at its own caps. The expressiveness it gives up is the ability to *rename* and *reshape*, and
the measured problem is bytes, not shape — a consumer that reads `issue.number` and the label names
pays the same 200 bytes either way. A document whose keys genuinely need restructuring still has the
rule's own jq to route on and its own consumer code to read from.

**Caps.** 24 selectors, each at most 256 bytes and 16 path segments; a projected document at most
256 KiB. Each cap is reported as a refusal naming the limit, never a silent truncation.

### 3. Claim keeps its escape hatch by omission; a list never re-grows

Omit `payload_fields` and the verb answers exactly as it does today: `claim` and `get_todo` return
the whole payload, `list_todos` returns compact rows. Nothing about an existing consumer changes, and
there is no `include_payload` boolean to keep in step with the field list — a listing that wants body
bytes asks for the fields it wants, which is the same sentence it writes on `claim`. `payload_size`
always reports the stored payload's size, projected or not, so a caller can still see what an
unprojected claim would cost.

### 4. Intake keeps the full body on the event, and never lets a projection lose a delivery

`EventInput.Payload` stays the raw delivery body: replay, the Board, quarantine inspection and
debugging see everything, whatever the rule says. Only `CreateTodoParams.Payload` is projected. A
quarantined todo is held for a human to read whole, so a quarantine or drop action carries no
projection at all — refused at save time rather than ignored at delivery time.

Then the rule that matters: **bytes are never worth losing data over.** A projection that matches
nothing, meets a payload that is not an object, or encodes past the cap does not replace the stored
payload. The todo keeps the body it always carried, the routing trace carries
`projection_cause` explaining why, and the applied `payload_fields` are recorded on the trace only
when they were actually applied — so a todo explains what it stores instead of what somebody asked
for.

```jsonc
// A Gitea issue rule that stores about 120 bytes instead of 8 KB.
{ "id": "issues", "expr": ".headers[\"x-gitea-event\"] == \"issues\"",
  "action": { "queue": "forge",
              "payload_fields": ["action", "issue.number", "issue.labels[].name", "repository.full_name"] } }
```

### 5. A projection at claim is a view, never an edit

`claim`, `claim_next`, `get_todo` and `list_todos` project on the way out. Nothing a read verb takes
changes what is stored: a later claim without the argument returns the whole payload again, and an
attempt history keeps whatever the worker reported. An unusable selector list is refused with
`invalid_argument` **before** a claim is taken, so no lease is ever handed out on an argument the
server cannot honour.

## Consequences

* Three surfaces get leaner at once, and the cheapest one — intake — helps every consumer including
  ones that never learn the feature exists. Doorbells and notification payloads built from the todo
  shrink with it.
* Stored payloads stay valid JSON objects, so anything that reads a todo by path still works; it just
  finds fewer paths. A worker that needs a field its rule projected away reads it from the tracker,
  which is what SPEC's consumers already do because a todo snapshot goes stale.
* Two new fields on the routing trace are additive: an old client ignores them.
* The `projection_cause` log line is the failure signal worth alerting on: a rule whose projection
  never applies is a rule that should be edited, and the body staying whole means it degrades quietly
  until someone looks.
* Out of scope here, deliberately: renaming or reshaping keys (a jq projection would allow it),
  per-endpoint default projections (a rule already speaks for the delivery), and any change to what
  the event record stores.
