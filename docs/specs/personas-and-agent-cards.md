# switchboard — Personas & A2A Agent Cards

How a **persona** is defined and how it maps to an **[A2A](https://a2a-protocol.org/) Agent Card**
served at a well-known endpoint. Decisions behind this are in
[ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md); the vended-verb pool a persona draws from
is in [ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md); the discovery/friending use of
these cards is in [ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md).

## Persona record

A persona is a named, scoped face of one registered agent, composed of exactly three parts:

```json
{
  "type": "object",
  "required": ["id", "agent_id", "name", "system_prompt", "verb_subset"],
  "properties": {
    "id":            { "type": "string", "description": "Persona id." },
    "agent_id":      { "type": "string", "description": "The registered agent (base runtime) this persona is a face of (ADR-008)." },
    "name":          { "type": "string", "description": "Human-facing persona name, e.g. 'reviewer', 'deployer'." },
    "system_prompt": { "type": "string", "description": "Human-authored intent/behavior. Authored by the owning human — the system does NOT derive this." },
    "verb_subset":   { "type": "array", "items": { "type": "string" },
                       "description": "Subset of the agent's vended verbs this persona may use. THE unit of capability scoping (ADR-009)." },
    "queues":        { "type": "array", "items": { "type": "string" },
                       "description": "Queues this persona may act on (⊆ the agent's vended queue grant)." },
    "description":   { "type": ["string", "null"], "description": "Optional short description surfaced on the Agent Card." }
  },
  "additionalProperties": false
}
```

- **Human authors the prompt; the system enforces the verbs.** `system_prompt` is human-written;
  `verb_subset`/`queues` are the capability slice and must be ⊆ the agent's vended scope
  ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).
- Two personas of one `agent_id` carry **different** `verb_subset`/`queues` — e.g. a `reviewer` and a
  `deployer` face of the same runtime, with different access
  ([ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md)).

## Skill derivation (skills follow capability)

A persona's advertised **skills are derived from `verb_subset`**, never hand-declared
([ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md)). The build maps each verb (or verb group)
to the skill it enables; a skill whose required verb is absent from `verb_subset` **cannot appear** on
the card. This makes "advertised = actual" a structural invariant.

Illustrative verb→skill map (authoritative map maintained with the code):

| Verb(s) in subset | Derived skill (on the Agent Card) |
|-------------------|-----------------------------------|
| `list_todos` + `claim` + `complete` | `process-work` (drain a queue) |
| `create_for` | `delegate-work` (hand work to a granted peer/queue) |
| `create_webhook` + `list_webhooks` … | `manage-webhooks` (within ceiling) |
| `send_friend_request` | `initiate-friending` |
| `approve`/`deny` | `approve-friending` (human-consent persona) |

## Agent Card mapping

Each persona is published as an A2A **Agent Card** (schema-valid per the A2A spec). The mapping:

| Agent Card field | Source |
|------------------|--------|
| `name` | persona `name` (optionally `agent.name / persona.name`) |
| `description` | persona `description` / `system_prompt` summary |
| `url` | the persona's A2A endpoint (discovery only — **not** a work-intake channel; work comes as todos, [ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md)) |
| `provider` / owner | the **owning human's** identity chain ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)), attestable via OIDC ([ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md)) |
| `skills` | **derived** from `verb_subset` (above) — never independently authored |
| `capabilities` | A2A capability flags switchboard supports (discovery/announcement; **not** direct task delegation) |

> The card advertises "who I am / what I do" (outward). It does **not** grant work-intake — that is
> governed by human vending and lands as todos ([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md)).

## Well-known endpoint

Per A2A, an Agent Card is served at **`/.well-known/agent-card.json`**. Switchboard hosts personas, so
it must disambiguate multiple personas:

- **Per-persona base path:** each persona is served under its own base, e.g.
  `https://switchboard.…/a/{persona_id}/.well-known/agent-card.json`, so the well-known path resolves
  to exactly one persona's card (A2A-compliant relative to that base).
- The card is **read-only** and reflects the persona's current `verb_subset` (skills recompute when the
  subset changes, [ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md)).
- Only personas whose owner has made them **discoverable** appear in a bounded directory
  ([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md) anti-spam); the well-known URL
  itself is the canonical card location.

## Example

```json
// persona record
{ "id": "persona:reviewer@agent-7", "agent_id": "agent-7", "name": "reviewer",
  "system_prompt": "You review pull requests for correctness and style…",
  "verb_subset": ["list_todos", "claim", "complete"], "queues": ["reviews"] }

// derived Agent Card at /a/persona:reviewer@agent-7/.well-known/agent-card.json
{ "name": "reviewer", "description": "Reviews pull requests…",
  "url": "https://switchboard.stump.rocks/a/persona:reviewer@agent-7/",
  "provider": { "organization": "joestump" },
  "skills": [ { "id": "process-work", "name": "Process review work",
               "description": "Claims and completes todos on the reviews queue." } ],
  "capabilities": { "streaming": false } }
```

## Cross-references

- Persona model & derivation decision: [ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md).
- Vended-verb pool & scope: [ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md), [accounts-and-endpoints spec](accounts-and-endpoints.md).
- Discovery + friending that consumes these cards: [ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md), [friend-requests spec](friend-requests.md).
- Owner-identity attestation: [ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md).
- A2A Agent Card format: <https://a2a-protocol.org/>.
