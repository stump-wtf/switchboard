# switchboard — Accounts & Vended Endpoints

The identity and access model: the human OIDC account, agent registration, endpoint vending (URL +
credential), the scope shape, and the immutable-by-default posture. Decisions are in
[ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md) (vending),
[ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md) (identity/assurance),
[ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md) (webhook ceiling); secret storage extends
[ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md).

## Human account (principal / tenant)

```json
{
  "type": "object",
  "required": ["id", "oidc_subject", "created_at"],
  "properties": {
    "id":           { "type": "string" },
    "oidc_subject": { "type": "string", "description": "Pocket ID OIDC 'sub'. The IdP holds HUMANS ONLY (ADR-008)." },
    "display_name": { "type": ["string", "null"] },
    "created_at":   { "type": "string", "format": "date-time" }
  }
}
```

- The human authenticates via **OIDC against Pocket ID** ([ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md))
  and is the **accountable principal** and **policy authority** for all their agents.
- No agent/bot is ever a principal in Pocket ID
  ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).

## Agent registration

```json
{
  "type": "object",
  "required": ["id", "owner_human_id", "name", "created_at"],
  "properties": {
    "id":             { "type": "string" },
    "owner_human_id": { "type": "string", "description": "The accountable human (ADR-008). Every agent action traces here." },
    "name":           { "type": "string" },
    "description":    { "type": ["string", "null"] },
    "runtime_meta":   { "type": ["object", "null"], "description": "Optional base-runtime metadata; non-authoritative." },
    "created_at":     { "type": "string", "format": "date-time" }
  }
}
```

Registration **grants nothing** on its own — access comes only from a vended endpoint.

## Vended endpoint (the capability grant)

```json
{
  "type": "object",
  "required": ["id", "agent_id", "persona_id", "url", "scope", "credential_ref", "mutability", "state", "created_at"],
  "properties": {
    "id":             { "type": "string" },
    "agent_id":       { "type": "string" },
    "persona_id":     { "type": ["string", "null"], "description": "Persona this endpoint serves (ADR-009); null for an agent-level endpoint." },
    "url":            { "type": "string", "format": "uri", "description": "The vended MCP endpoint URL. URL + credential = the capability (ADR-008)." },
    "credential_ref": { "type": "string", "description": "OpenBao path for the minted credential — e.g. secret/switchboard/agents/{agent_id}/endpoints/{id}. The value is NEVER returned after the one-time issue." },
    "scope":          { "$ref": "#/$defs/Scope" },
    "webhook_ceiling":{ "$ref": "#/$defs/WebhookCeiling" },
    "mutability":     { "type": "string", "enum": ["immutable", "mutable"], "default": "immutable",
                        "description": "PROPOSED DEFAULT immutable (ADR-008): change scope by revoke+re-vend. See open question." },
    "state":          { "type": "string", "enum": ["active", "revoked"] },
    "created_at":     { "type": "string", "format": "date-time" }
  }
}
```

### `Scope`
```json
{
  "type": "object",
  "required": ["queues", "verbs"],
  "properties": {
    "queues": { "type": "array", "items": { "type": "string" }, "description": "Queues this endpoint may act on." },
    "verbs":  { "type": "array", "items": { "type": "string" },
                "description": "Verb allowlist — a subset of the agent-mcp-tools verbs. Enforced at the boundary (ADR-008)." }
  }
}
```

### `WebhookCeiling` (ADR-012)
```json
{
  "type": "object",
  "properties": {
    "max":                  { "type": "integer", "minimum": 0, "description": "Max webhooks this endpoint may create." },
    "allowed_source_types": { "type": "array", "items": { "type": "string" }, "description": "e.g. ['github','generic']." },
    "allowed_queues":       { "type": "array", "items": { "type": "string" }, "description": "Queues created webhooks may target." }
  }
}
```

## Vend / revoke lifecycle

```
register agent → (human sets scope + ceiling) → VEND ──▶ active
                                                   │
                                                   └─ revoke ─▶ revoked (credential invalidated, URL unrouted)
```

- **Vend** mints a credential, escrows it in OpenBao at `credential_ref`, returns **the URL and the
  credential once** (never retrievable again), and marks the endpoint `active`
  ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).
- **Approval-is-vend:** a friend approval mints a vended endpoint the same way
  ([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md), [friend-requests spec](friend-requests.md)).
- **Revoke** invalidates the OpenBao credential and unroutes the URL — instant, total, no residue
  (nothing in the IdP because the bot was never there).
- Credentials are **short-lived** and rotatable independently of the human
  ([ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md)).

## OpenBao layout (extends ADR-004)

| Path | Field(s) | Purpose |
|------|----------|---------|
| `secret/switchboard/agents/{agent_id}/endpoints/{endpoint_id}` | `credential` | Vended endpoint credential (short-lived, revocable). |
| `secret/switchboard/agents/{agent_id}/webhooks/{webhook_id}` | `signing_secret` | Secret for an agent-created webhook — minted & held by switchboard; never returned to the agent ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)). |

The AppRole policy grants the service `read`/`write` on `secret/switchboard/agents/*` in addition to
the provider paths of [ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md).

## Immutable-by-default (open question)

> **PROPOSED DEFAULT — `mutability: immutable`** ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)):
> a vended endpoint's `scope` cannot be edited in place. To change access, **revoke and re-vend** a new
> endpoint with the new scope. Rationale: an immutable capability means a given URL+credential always
> denotes one fixed power set — simpler to reason about and audit, no "when/who changed this scope"
> ambiguity.
>
> **To confirm with Joe:** immutable vs. mutable (edit a live endpoint's `queues`/`verbs`). Mutable is
> more convenient for incremental tweaks but reintroduces the audit ambiguity. Recorded in
> [docs/README.md](../README.md) open-questions.

## Enforcement summary

| Rule | Where |
|------|-------|
| Verb outside `scope.verbs` → `forbidden` | every vended MCP call ([agent-mcp-tools spec](agent-mcp-tools.md)) |
| Queue outside `scope.queues` → `forbidden` | todo & webhook verbs |
| Webhook beyond ceiling → `ceiling_exceeded`/`forbidden_source_type`/`forbidden` | `create_webhook` ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)) |
| Friend `granted_scope ⊄ requested_scope` → `invalid_argument` | `approve` ([friend-requests spec](friend-requests.md)) |
| Revoked endpoint → all calls fail | boundary |

## Cross-references

- Vending, immutable default, revocation: [ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md).
- Personas subdivide an agent's vended verbs: [ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md), [personas-and-agent-cards spec](personas-and-agent-cards.md).
- Approval-is-vend (cross-agent grants): [ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md), [friend-requests spec](friend-requests.md).
- Identity/assurance: [ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md).
- Webhook ceiling: [ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md).
- Secret storage: [ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md).
