---
status: approved
date: 2026-09-22
implements: [ADR-0038]
requires: [SPEC-0003, SPEC-0006, SPEC-0007, SPEC-0008, SPEC-0011, SPEC-0020, SPEC-0021]
related: [SPEC-0024, SPEC-0026, SPEC-0028, SPEC-0029, SPEC-0030, SPEC-0031, SPEC-0032, SPEC-0034]
---

# SPEC-0033: Teams and Tenancy

## Overview

Switchboard is multi-tenant. This spec makes the owner of every resource explicit and adds
**teams** as the second kind of owner, per
[ADR-0038](../../../adrs/ADR-0038-teams-and-tenancy.md). It defines:

* two **profiles**: the **operator**, who runs the instance, and **users**, who log in and use it;
* the **owner model**: every tenant resource is owned by exactly one human or one team, directly or
  through the endpoint it is attached to;
* **reach**, the single predicate every tenant read and write is filtered by;
* **teams**: creation, roles (`owner`, `admin`, `member`), invitations, and optional OIDC group sync;
* **team queues**, which members' agents drain competitively;
* the fixes for every global or unscoped surface found on `main` @ `8474757`, including the event-history tools (F1).

It amends, without renaming any requirement code cites: [SPEC-0007](../vended-endpoints/spec.md)
(endpoints may be team-owned; effective reach narrows the grant), [SPEC-0003](../todo-queue/spec.md)
(a todo is owned by an endpoint or a team), [SPEC-0006](../agent-tools/spec.md) (queue arguments
accept a team queue), [SPEC-0011](../channels/spec.md) (team-queue doorbells) and
[SPEC-0021](../github-login/spec.md) (the issuer gate is reused for team consent actions).

Companion records that take their owner model from this spec, accepted together on 2026-09-22 and
linked as front-matter edges: SPEC-0024 (notify hooks), ADR-0031 (trusted actors and quarantine), ADR-0033 (reply-to-source
credentials), ADR-0034 (notification sinks), ADR-0035 (admission control), ADR-0036 (rule packs),
ADR-0037 (provider secrets), ADR-0039 (attempt history).

Terms:

* **Operator**: a human listed in `SWITCHBOARD_OPERATORS` or carrying `SWITCHBOARD_OPERATOR_GROUP`.
  An operator is also a user.
* **User**: any authenticated human (a `humans` row).
* **Owner scope**: one human, or one team.
* **Directly owned**: a resource with its own `owner_human_id` / `owner_team_id` columns.
* **Attached**: a resource that belongs to an endpoint (or to a webhook, which belongs to an
  endpoint) and inherits that endpoint's owner scope.
* **Reach**: the owner scopes, with role-derived permissions, a principal may act in.
* **Team queue**: a queue identified by `(team, name)` whose todos are owned by the team.

## Requirements

### Requirement: Operator and User Profiles

Switchboard MUST distinguish two profiles. A human is an **operator** if and only if their session's
recorded provenance, written as `<issuer>|<subject>` (for example `https://id.example.com|abc-123`,
or `github.com|42`), appears in the comma-separated `SWITCHBOARD_OPERATORS`, or
`SWITCHBOARD_OPERATOR_GROUP` is set and their OIDC session's groups claim contains it. Cairn uses the
same format for `CAIRN_OPERATORS`. Every other authenticated human is a **user**. An operator MUST also be a user:
their personal resources are owned by their human row exactly like anyone else's.

Operator surfaces MUST require a session whose issuer is the passkey issuer (the SPEC-0021 issuer
gate). With neither variable set, the instance MUST have no operator and MUST still serve users
normally; operator routes MUST then answer `404`.

#### Scenario: Operator by subject

- **GIVEN** `SWITCHBOARD_OPERATORS=https://id.example.com|abc-123`
- **WHEN** the human whose `sub` at that issuer is `abc-123` logs in through it (the passkey issuer)
- **THEN** the operator console is available to them, and their own endpoints are listed as theirs

#### Scenario: GitHub session is not an operator session

- **GIVEN** `SWITCHBOARD_OPERATORS=github.com|42`
- **WHEN** that human logs in with GitHub and opens the operator console
- **THEN** the request is refused with `403` and a prompt to re-authenticate through the passkey
  issuer

#### Scenario: No operator configured

- **WHEN** neither `SWITCHBOARD_OPERATORS` nor `SWITCHBOARD_OPERATOR_GROUP` is set
- **THEN** every operator route answers `404` and every user surface behaves normally

### Requirement: Operator Surfaces Bound Tenant Data and Never Read It

Operator surfaces MUST be limited to instance configuration and governance:

* the user and team directory: display name (a team's name), email, created and last-login times,
  suspension state, and **counts** of endpoints, webhooks, queues and todos per owner. A team's name
  is visible to the operator; its contents never are;
* per-user and per-team ceilings (endpoints, webhooks, teams per human, notify hooks, sinks);
* suspending and unsuspending a user or a team;
* instance settings, login policy, and the metrics scrape credential (SPEC-0023).

No operator surface MAY return, replay, export or render a tenant's todo payload, todo title, event
body or headers, attempt detail, secret, credential, routing rule body, notify-hook URL or sink
target. Operator status MUST NOT widen any tenant read: an operator's reach is their own reach as a
user.

Instance configuration MAY bound tenant data (limits, ceilings, retention windows, login
allowlists). It MUST NOT route, copy or reveal tenant data to a destination the operator chose: no
environment variable or instance setting MAY name a URL, queue or endpoint that receives other
owners' todos, events or notifications.

#### Scenario: Operator cannot read a user's todos

- **GIVEN** an operator session and a user U with pending todos
- **WHEN** the operator requests U's todos through the board, `/api/v1`, or MCP with the operator's
  own endpoint
- **THEN** every request returns `not_found` or an empty list, and the directory shows U's todo count
  only

#### Scenario: Suspending a user

- **WHEN** the operator suspends user U with a reason
- **THEN** U's sessions are revoked, U's personal endpoints fail authentication with
  `endpoint_suspended`, U's team memberships stop conferring reach, team-owned resources keep working
  for the team's other members, and an `operator_audit` row records operator, target, action and
  reason

#### Scenario: A proposed instance-wide destination is refused by review

- **WHEN** a change proposes an environment variable naming a URL that receives every tenant's
  notifications
- **THEN** it violates this requirement and MUST instead be an owned resource (SPEC-0024 notify
  hooks, ADR-0034 sinks)

### Requirement: Operator Audit Trail

Every operator action that affects a tenant (suspend, unsuspend, change a per-owner ceiling, delete a
team) MUST write an `operator_audit` row with the operator's human id, the target owner scope, the
action, a required free-text reason, and a timestamp, in the same transaction as the action. The
affected user, or every owner of the affected team, MUST be able to read the rows that name them on
their settings page. Rows MUST NOT be editable or deletable through any product surface.

#### Scenario: Owner sees the suspension of their team

- **WHEN** the operator suspends team T with reason "abuse report 2026-09-30"
- **THEN** each owner of T sees that row, with the operator's display name, on T's settings page

#### Scenario: Missing reason

- **WHEN** an operator submits a suspension with an empty reason
- **THEN** the action is refused with `400` and nothing changes

### Requirement: Owner Model

Every tenant resource MUST have exactly one owner scope, determined one of two ways:

1. **Directly owned** resources MUST carry `owner_human_id` and `owner_team_id` columns with a
   database constraint that exactly one is non-null, and a `created_by_human_id` column. The creator
   column MUST be used for audit and display only and MUST NOT confer any permission.
2. **Attached** resources MUST inherit the owner scope of the endpoint they belong to, directly or
   through a webhook, and MUST NOT carry owner columns of their own.

A resource MUST NOT be owned by the operator, by the instance, or by nothing. Deleting an owner scope
MUST delete (or, for endpoints, revoke) everything it owns.

#### Scenario: Constraint rejects two owners

- **WHEN** a write sets both `owner_human_id` and `owner_team_id` on a notification sink
- **THEN** the database rejects the row

#### Scenario: A webhook follows its endpoint

- **GIVEN** endpoint E owned by team T and a webhook W created by E
- **THEN** W's owner scope is T, and every T admin may rotate or delete W on the board

#### Scenario: Creator leaves

- **GIVEN** a team sink created by human H
- **WHEN** H leaves the team
- **THEN** the sink stays owned by the team, keeps working, and still shows H as its creator

### Requirement: Reach and Effective Reach

A human's **reach** MUST be their own scope plus each team they are a current, unsuspended member of,
with the permissions of their role in that team.

An endpoint's **effective reach** MUST be the intersection of its vend grant (SPEC-0007) and its
owner's reach **at the time of the call**. For a personal endpoint the owner is its human; for a
team endpoint it is the team, bounded by the permissions granted at vend time. Effective reach MUST
be evaluated on every MCP call and every `/api/v1` request and MUST NOT be cached beyond one request.

Every store method that reads or writes a tenant resource MUST take a reach value and apply it in the
query. Methods that must operate across tenants (ingest resolving a webhook token, the lease reaper,
retention, metrics gauges) MUST be named with an `Unscoped` suffix, MUST appear on an allowlist
enforced by a test, and MUST NOT return rows to a human or agent caller.

A request for a resource outside the caller's reach MUST produce the same `not_found` response,
status and timing class as a request for an id that does not exist.

#### Scenario: Grant cannot exceed live reach

- **GIVEN** endpoint E of human H holds grant `team:t/reviews`
- **WHEN** H is removed from team `t` and E then calls `claim_next` on `team:t/reviews`
- **THEN** the call returns `not_found`, with no re-vend

#### Scenario: Probing a foreign id

- **WHEN** an endpoint calls `get_todo` with another owner's todo id, and then with a random id
- **THEN** both responses are `not_found` and indistinguishable

#### Scenario: Unscoped method without the suffix

- **WHEN** a store method is added that queries `todos` without a reach parameter and without the
  `Unscoped` suffix
- **THEN** the store tenancy test fails naming the method

### Requirement: Team Lifecycle

Any user MAY create a team, up to the operator's `SWITCHBOARD_MAX_TEAMS_PER_HUMAN` ceiling (default
10) of teams they own. The creator MUST become the team's first owner. A team MUST have a slug that is
unique on the instance, 3–40 characters of lowercase letters, digits and single hyphens, and a
display name. Slugs of deleted teams MUST NOT be reusable for 30 days.

Owners MAY rename a team, change its slug, and delete it. Deletion MUST require the owner to retype
the slug, MUST require a passkey-issuer session, MUST revoke every team-owned endpoint, and MUST
delete every team-owned resource and team-queue todo in one transaction.

Team existence MUST NOT be observable by non-members: slug lookups, invite pages and API routes MUST
answer `not_found` to a user who is not a member and holds no pending invite.

#### Scenario: Creating a team

- **WHEN** user H creates team `stump`
- **THEN** H is its owner, and H's reach now includes `stump`

#### Scenario: Ceiling reached

- **WHEN** H already owns 10 teams and creates another
- **THEN** the request fails with `team_ceiling_reached` and no team is created

#### Scenario: Non-member probes a slug

- **WHEN** a user who is not a member requests `/teams/stump`
- **THEN** the response is `404`, identical to an unused slug

### Requirement: Team Roles

A team membership MUST carry exactly one role: `owner`, `admin` or `member`. Permissions MUST be:

| Action | member | admin | owner |
|---|---|---|---|
| See team queues, todos, events, attempts and deliveries on the board | yes | yes | yes |
| Claim, heartbeat, complete, fail team-queue todos through a granted endpoint | yes | yes | yes |
| Grant a team queue to one of their own personal endpoints at vend | yes | yes | yes |
| Create a human-authored todo on a team queue | yes | yes | yes |
| Vend, rotate and revoke team-owned endpoints | no | yes | yes |
| Create, edit, delete team webhooks, routes, rules, rule-pack installs, notify hooks, sinks, credentials, secrets, admission budgets, trusted-actor lists | no | yes | yes |
| Invite and remove members and admins; revoke invites | no | yes | yes |
| Grant or revoke `owner`; rename, transfer, delete the team | no | no | yes |

The webhook row above is the default. A team owner MAY turn on the team setting
`members_create_webhooks`, which MUST default to `false`. While it is on, a member MAY create, edit and
delete webhooks on team endpoints their own grant covers; nothing else in that row changes for members.
Changing the setting MUST be recorded in the team's audit and shown on the team settings page.

A team MUST always have at least one owner: the last owner MUST NOT leave, be removed or be demoted.
A secret or credential value MUST NOT be returned to anyone, owners included, after the response that
created or rotated it.

#### Scenario: Member tries to add a webhook

- **GIVEN** team T has `members_create_webhooks = false` (the default)
- **WHEN** a member of team T calls the board's create-webhook route for a T endpoint
- **THEN** the request is refused with `403 role_required` and nothing is created

#### Scenario: Owner lets members add webhooks

- **GIVEN** an owner of team T turned on `members_create_webhooks`
- **WHEN** a member creates a webhook on a T endpoint their grant covers
- **THEN** the webhook is created, owned through the team endpoint, and the setting change is in T's
  audit

#### Scenario: Last owner leaves

- **WHEN** the only owner of T tries to leave T
- **THEN** the request is refused with `last_owner`

#### Scenario: Admin reads a team credential

- **WHEN** an admin of T requests a T reply credential after creation
- **THEN** the response carries its name, kind, created-by and last-used time, and never its value

### Requirement: Team Invitations

An admin or owner MAY invite an email address to a team with a role no higher than their own (an
admin MAY invite `member` or `admin`; only an owner MAY invite `owner`). Switchboard MUST mint an
unguessable, single-use token of at least 128 bits, store only its hash, and expire it 7 days after
creation. Admins and owners MUST be able to list pending invites and revoke them.

Accepting MUST require an authenticated user whose provider-verified email equals the invite address
(case-insensitive). An accepted, revoked or expired token MUST answer `not_found`. The invite page
MUST NOT reveal the team's slug or members to anyone who is not the addressee. Whether an invited
email already has an account MUST NOT be revealed to the inviter.

Switchboard MAY deliver invites through an owned notification sink (ADR-0034) or show a copyable
link; it MUST NOT send invite mail through an instance-wide channel the operator configured unless
that channel only ever carries invites and login mail.

#### Scenario: Accepting an invite

- **GIVEN** an invite to `bro@example.com` as `member` of T
- **WHEN** a user whose verified email is `Bro@Example.com` opens the link and accepts
- **THEN** they become a member of T and the token is consumed

#### Scenario: Wrong account

- **WHEN** a user whose verified email is `someone@example.com` opens the same link
- **THEN** the page answers `not_found` and the invite stays pending

#### Scenario: Admin cannot mint an owner

- **WHEN** an admin of T invites an address as `owner`
- **THEN** the request is refused with `role_ceiling`

### Requirement: Consent-Grade Team Actions

Vending a team-owned endpoint, creating or rotating a team credential or secret, and granting
`admin` or `owner` mint capabilities and MUST require a passkey-issuer session, exactly as SPEC-0021
gates friend approvals. A GitHub-established session MUST receive `403` with a step-up prompt on
these actions and MUST be able to perform every other member and admin action.

#### Scenario: GitHub session vends a team endpoint

- **WHEN** a team admin logged in with GitHub vends a team endpoint
- **THEN** the request is refused with `403 passkey_session_required`

### Requirement: Team-Owned Endpoints

A team admin or owner MAY vend an endpoint owned by the team. The endpoint MUST record
`vended_by_human_id`, which MUST be shown wherever the endpoint is shown and MUST be the accountable
human for its actions (ADR-0008). A team endpoint's grant MAY name only that team's queues and verbs.
It MUST survive the vending human leaving the team, and any admin or owner of the team MUST be able
to rotate or revoke it.

#### Scenario: Vender leaves

- **GIVEN** team endpoint E vended by admin A
- **WHEN** A leaves the team
- **THEN** E keeps working, and E's card shows "vended by A (former member)"

#### Scenario: Team endpoint names a personal queue

- **WHEN** an admin vends a team endpoint whose grant includes a personal queue
- **THEN** the vend is refused with `grant_out_of_scope`

### Requirement: Team Queues

A queue MUST be identified by an owner scope and a name. Endpoint queues keep their SPEC-0003
behaviour unchanged. A **team queue** is addressed as `team:<slug>/<name>` in every MCP and API queue
argument and grant.

A todo MUST be owned by exactly one of an endpoint (`endpoint_id`) or a team (`team_id`), enforced by
a database constraint. A team-queue todo MUST be visible to every member of the team and claimable
by every endpoint whose effective reach includes the queue: team-owned endpoints granted the queue,
and members' personal endpoints holding the queue in their grant while their human is a member.

Claims on a team queue MUST use the same competing-consumer claim, lease, retry and dead-letter
semantics as endpoint queues, and MUST record `claimed_by_endpoint_id`. Heartbeat, complete and fail
MUST succeed only for the endpoint that holds the lease.

#### Scenario: Two members compete

- **GIVEN** a team queue `team:t/reviews` with one pending todo, and endpoints of members A and B both
  granted it
- **WHEN** both call `claim_next` at the same moment
- **THEN** exactly one receives the todo and the other receives `{"empty": true}`

#### Scenario: Non-member's endpoint names the queue

- **WHEN** an endpoint of a user who is not a member of `t` calls `list_todos` on `team:t/reviews`
- **THEN** the call returns `not_found`

#### Scenario: Complete by a different endpoint

- **WHEN** member B's endpoint calls `complete` on a team todo claimed by member A's endpoint
- **THEN** the call returns `not_found` and the todo stays claimed by A

### Requirement: Membership Changes Take Effect Immediately

Removing a member, demoting them, suspending them, or their leaving MUST take effect on the next call
by any of their endpoints. Leases held by a removed or suspended member's endpoints on the team's
todos MUST be released in the same transaction, returning those todos to `pending` without counting
an attempt. Group-sourced removals (REQ "OIDC Group Sync") MUST behave identically.

#### Scenario: Removal releases a lease

- **GIVEN** member B's endpoint holds a lease on team todo X
- **WHEN** an admin removes B
- **THEN** X is `pending` with its attempt count unchanged, and B's endpoint's next `heartbeat` on X
  returns `not_found`

### Requirement: Team-Queue Push

A team-queue todo that passes the SPEC-0011 sender gate MUST ring exactly one live session among the
eligible endpoints that are clocked in (SPEC-0022), chosen least-recently-rung first. Notify hooks
(SPEC-0024) MUST fire for a team-queue todo only if they are attached to a team-owned endpoint of the
same team whose grant includes the queue. A team-queue todo MUST NOT ring or call any personal
endpoint's hook.

#### Scenario: Burst spreads across the team

- **GIVEN** three eligible, clocked-in endpoints with live sessions
- **WHEN** three team todos arrive
- **THEN** each endpoint is rung once

#### Scenario: Personal hook stays quiet

- **GIVEN** member A's personal endpoint has a notify hook and a grant on the team queue
- **WHEN** a team todo arrives
- **THEN** A's personal hook is not called

### Requirement: Resource Ownership Map

Each resource MUST be owned as follows, and every record that introduces one of these resources MUST
conform:

| Resource | Owner |
|---|---|
| Agents, endpoints | direct: human or team |
| Personas | attached to the endpoint |
| Endpoint queues and their todos | attached to the endpoint |
| Team queues and their todos | direct: the team (`todos.team_id`) |
| Webhooks, webhook routes | attached to the endpoint |
| Routing rules, rule-pack installs (ADR-0036) | attached to the webhook |
| Notify hooks (SPEC-0024) | attached to the endpoint |
| Trusted-actor lists, quarantined deliveries (ADR-0031) | attached to the webhook |
| Notification sinks (ADR-0034) | direct: human or team |
| Reply and outbound credentials (ADR-0033), provider secrets (ADR-0037) | direct: human or team |
| Admission budgets (ADR-0035), digest schedules (ADR-0034) | the queue's owner scope |
| Events, deliveries, attempt history (ADR-0039) | the event's owner, written at ingest (`events.endpoint_id` or `events.team_id`) |
| Friend edges | the two humans |
| Human OAuth clients and tokens | the human |
| Presence overrides and shifts | attached to the endpoint |
| Sessions | the human |

Instance settings, ceilings, login policy, and the metrics scrape credential are operator
configuration and MUST NOT hold tenant data.

#### Scenario: A new resource without an owner

- **WHEN** a migration adds a table holding tenant data with neither owner columns nor a foreign key
  to an endpoint or webhook
- **THEN** the store tenancy test fails naming the table

### Requirement: Same-Scope References

A resource MUST reference credentials, secrets, sinks and rule packs only in its own owner scope, or
code-shipped catalog entries that hold no tenant data. The reference MUST be validated when written
and again when used; a reference that has become cross-scope at use time (for example, after an
endpoint was transferred) MUST fail closed and mark the referencing resource unhealthy.

#### Scenario: Personal rule binds a team credential

- **WHEN** human H, an admin of T, saves a rule on a personal webhook that references T's reply
  credential
- **THEN** the save is refused with `reference_out_of_scope`

### Requirement: Credentials Follow the Todo

When work on a todo uses a credential, secret or sink (reply-to-source, notifications, provider
calls), the one used MUST be selected from the todo's owner scope, never from the claiming endpoint's
owner scope.

#### Scenario: Member replies on a team todo

- **GIVEN** member B's personal endpoint claims team todo X, and both B and team T have a Gitea reply
  credential
- **WHEN** B's agent replies to X's source
- **THEN** the reply is authenticated with T's credential, and B's is never loaded

### Requirement: Data Flows Into a Team, Never Out

A webhook route (SPEC-0020, ADR-0022) MAY target a team queue from a personal webhook if the granting
human is a member of that team; the route MUST stop delivering, without error to the sender, once the
granting human is no longer a member. A route from a team-owned webhook MUST NOT target a personal
endpoint or another owner's queue.

#### Scenario: Member shares deliveries into the team

- **WHEN** member H routes their personal webhook to `team:t/triage`
- **THEN** each delivery creates a todo on the team queue, and after H leaves `t` new deliveries
  create only H's own todos

#### Scenario: Team data routed out

- **WHEN** a team admin adds a route from a team webhook to their personal endpoint
- **THEN** the route is refused with `route_out_of_scope`

### Requirement: Owner-Scoped History Reads

Every read of events, deliveries and attempt history — `list_webhook_events`, `get_webhook_event`,
`replay_webhook_event`, the `switchboard://events/recent` resource, their board and `/api/v1`
equivalents, and any surface added by ADR-0031 or ADR-0039 — MUST return only rows whose owner is in
the caller's effective reach. Each event MUST carry its owner, written at ingest from the resolved
webhook's endpoint (`events.endpoint_id`), or the team for a human-authored todo pushed to a team
queue (`events.team_id`), exactly one of the two, and kept when the webhook is later deleted.
Existing events MUST be backfilled through `webhook_id`; an event whose owner cannot be established
MUST be invisible to agents and to the board, and is left to retention. `replay_webhook_event` MUST
refuse an event outside reach with `not_found` before reading its payload. This closes F1.

#### Scenario: Human B lists events (the F1 reproduction)

- **GIVEN** human A's webhook has received deliveries
- **WHEN** human B's endpoint calls `list_webhook_events` with no filters
- **THEN** none of A's events is returned

#### Scenario: Replay of a foreign event

- **WHEN** B's endpoint calls `replay_webhook_event` with the id of A's event
- **THEN** the call returns `not_found` and no outbound request is made

#### Scenario: Team member reads team deliveries

- **WHEN** a member of T lists events for a T webhook
- **THEN** the deliveries are returned

### Requirement: Closing the Audited Surfaces

Each unscoped surface listed in the Audit Findings table of design.md MUST be brought under REQ
"Reach and Effective Reach" or REQ "Operator Surfaces Bound Tenant Data and Never Read It", and each
MUST gain a tenancy-suite case that fails against the unfixed code.

For F11, an owner MAY opt one of its own queues into the instance scrape under its real name. The
opt-in MUST default to off, MUST stay bounded by the shared label cap, and is specified in the SPEC-0023
amendment F11 requires.

#### Scenario: Friend grant cannot carry webhook verbs (F3)

- **WHEN** a friend request asks for `set_webhook_rules`, and the target human approves it
- **THEN** the minted endpoint's grant contains only `create_for` and the drain verbs, and a call to
  `set_webhook_rules` returns `scope_denied`

#### Scenario: Rule verbs stay on their own webhook (F3, F19)

- **GIVEN** human H owns endpoints E1 (rule verbs, webhook W1) and E2 (webhook W2)
- **WHEN** E1 calls `set_webhook_rules` or `test_webhook_rules` on W2
- **THEN** the call returns `not_found`

#### Scenario: Vend screen suggests only reachable queues (F4)

- **WHEN** human B opens the vend screen on an instance where human A has a queue `a-secret`
- **THEN** `a-secret` is not suggested

#### Scenario: One tenant's slow rules do not disable another's (F5)

- **GIVEN** tenant A floods deliveries against a deliberately slow rule
- **WHEN** a delivery reaches tenant B's webhook whose trust rule drops unverified senders
- **THEN** B's rule is evaluated within B's share of the sandbox, and if it cannot be evaluated the
  delivery is dropped (fail closed), never admitted by default

#### Scenario: Lane frames reach only the owner (F10)

- **WHEN** a delivery to A's webhook is rejected for a bad signature
- **THEN** A's board shows the `lane_rejected` frame and B's board receives nothing

#### Scenario: Tenant queue names stay out of the operator's scrape (F11)

- **GIVEN** user U (not an operator) has queue `payroll-escalations`
- **WHEN** the operator scrapes `/metrics`
- **THEN** no series carries `queue="payroll-escalations"`; U's queues are counted under
  `queue="__tenant__"`, and the operator's own queues keep their names

#### Scenario: Owner opts a queue name into the scrape (F11)

- **GIVEN** user U turned on the metrics-name opt-in for queue `reviews`
- **WHEN** the operator scrapes `/metrics`
- **THEN** series for that queue carry `queue="reviews"`, U's other queues stay under
  `queue="__tenant__"`, and the opt-in counts against the shared label cap

#### Scenario: Consent screen shows where tokens go (F12)

- **WHEN** a dynamically registered client named "Switchboard CLI" with redirect
  `https://evil.example/cb` requests human OAuth consent
- **THEN** the consent screen shows `evil.example` and marks the client unverified

#### Scenario: Friend requests from different tenants do not collide (F13)

- **WHEN** two humans each send a request from a persona named `reviewer` to the same target persona
- **THEN** both requests exist independently and neither sender learns of the other

#### Scenario: Dedup cannot cross owners (F14)

- **WHEN** two owners' webhooks receive deliveries with the same source and external id
- **THEN** each creates its own event and todos, and neither delivery is answered with the other's

#### Scenario: Same subject at two issuers (F15)

- **WHEN** a second OIDC issuer presents a `sub` equal to an existing human's
- **THEN** a separate human is created; the existing human's resources are not reachable

#### Scenario: Dev todo route requires ownership (F16)

- **GIVEN** `SWITCHBOARD_DEV_LOGIN=1`
- **WHEN** an unauthenticated request posts to `/dev/todos` with another human's endpoint id
- **THEN** the request is refused with `401`

#### Scenario: No read by queue name alone (F18)

- **WHEN** a wakeup arrives in the old format that names only a queue
- **THEN** it is ignored and logged; no endpoint's pending todos are read by name

#### Scenario: Route-target trace is redacted (F20)

- **GIVEN** A's webhook routes to friend B's endpoint through a rule named `vip-customers`
- **WHEN** B's agent reads its todo
- **THEN** the routing trace carries stage, cause and B's queue, and no rule name, rule id, fault
  text or other target

### Requirement: Enrollment Policy

The enrollment mode MUST be operator configuration, set with `SWITCHBOARD_ENROLLMENT_MODE`:

* `open`: any identity the configured providers authenticate;
* `allowlist`: only identities matching `SWITCHBOARD_ENROLLMENT_ALLOW`, a comma-separated list of
  provider-qualified subjects (`<issuer>|<subject>`), verified email addresses, `@domain` entries
  (matched against verified emails) and `github-org:<login>` entries;
* `invite`: only identities whose verified email holds a pending team invitation, plus the operators.

The default MUST be `invite` when the GitHub provider is configured and `open` otherwise. Any other
value MUST fail startup with an error naming the variable. The mode MUST apply to every login provider
and only to creating a new `humans` row: an existing human MUST NOT be locked out by a mode change (the
operator suspends them instead). A refused enrollment MUST create no row and MUST show a page that
does not reveal whether any invite or team exists.

The variable names, values and semantics MUST match Cairn's (`CAIRN_ENROLLMENT_MODE`,
`CAIRN_ENROLLMENT_ALLOW`), so an operator running both products configures enrollment the same way.

#### Scenario: Stranger signs in with GitHub (F2)

- **GIVEN** the GitHub provider is configured and `SWITCHBOARD_ENROLLMENT_MODE` is unset
- **WHEN** a GitHub account with no pending invite completes login
- **THEN** no human is created and the user sees "this instance is invite-only"

#### Scenario: Invited friend signs in with GitHub

- **GIVEN** an invite to `friend@example.com`
- **WHEN** a GitHub account whose primary verified email is `friend@example.com` completes login
- **THEN** a human is created and the invite page is offered

#### Scenario: Operator opens enrollment

- **GIVEN** the GitHub provider is configured and `SWITCHBOARD_ENROLLMENT_MODE=open`
- **WHEN** a GitHub account with no pending invite completes login
- **THEN** a human is created with no team memberships

#### Scenario: Allowlist by organisation

- **GIVEN** `SWITCHBOARD_ENROLLMENT_MODE=allowlist` and `SWITCHBOARD_ENROLLMENT_ALLOW=github-org:stump-wtf`
- **WHEN** a GitHub account outside that organisation completes login
- **THEN** no human is created, and the page does not say which list it missed

#### Scenario: Unknown mode fails startup

- **WHEN** the server starts with `SWITCHBOARD_ENROLLMENT_MODE=closed`
- **THEN** it exits with an error naming `SWITCHBOARD_ENROLLMENT_MODE` and the accepted values

#### Scenario: Mode change never locks out an existing user

- **GIVEN** human H enrolled while the mode was `open`
- **WHEN** the operator restarts with `SWITCHBOARD_ENROLLMENT_MODE=invite`
- **THEN** H still logs in, and only a new identity without an invite is refused

### Requirement: Owned Replay Targets

Replay destinations MUST be owned by the endpoint that replays. `replay_webhook_event` MUST accept a
target only if it is in the calling endpoint's own replay-target list or passes the SSRF validator
(ADR-0029's `internal/push.Validator`) at call time. No tenant call MAY bypass the SSRF validator. The
instance settings `replay_default_target` and `replay_allowed_targets` MUST be removed outright: the
migration deletes their rows, nothing reads or warns about them afterwards, and the release's
CHANGELOG `### Breaking` entry and upgrade note (SPEC-0027) name both settings and the owned-target
replacement.

#### Scenario: Instance trusted target is gone (F9)

- **GIVEN** an instance that had `replay_allowed_targets` set to an internal host
- **WHEN** any endpoint replays an event with no target argument
- **THEN** the call fails with `replay_target_required`, and no request reaches the internal host

### Requirement: Per-Owner Retention

Retention row caps MUST be counted and enforced per owner scope. The operator MUST set the default
caps and MAY raise them for one owner (audited per REQ "Operator Audit Trail"). Eviction for one owner
MUST NOT delete another owner's events or todos. Age-based retention remains an instance bound and
applies uniformly.

#### Scenario: A noisy tenant evicts only its own history (F6)

- **GIVEN** a per-owner cap of 10,000 events
- **WHEN** tenant A receives its 10,001st event
- **THEN** A's oldest event is evicted and tenant B's history is untouched

### Requirement: Per-Webhook Ingest Limits

After an ingest token resolves to a webhook, deliveries MUST be rate-limited per webhook, so one
webhook's flood cannot delay another's deliveries. The pre-lookup limiter MUST key on the client
address resolved through the trusted-proxy configuration (F7).

#### Scenario: A flooded webhook does not starve a neighbour (F7)

- **WHEN** webhook A receives 1,000 deliveries a second
- **THEN** webhook B's deliveries in the same second are accepted within B's own limit

### Requirement: OIDC Group Sync

Group sync MUST be off unless the operator sets `SWITCHBOARD_OIDC_GROUPS_CLAIM` to the name of the
claim to read. When on, a team owner MAY link their team to one group name and a role (`member` or
`admin`; never `owner`), and MUST carry that group in their own current session to create the link.

At each login through the configured OIDC issuer, Switchboard MUST add a group-sourced membership for
every linked team whose group the claim carries, and MUST remove a group-sourced membership whose
group the claim no longer carries. Manually granted memberships MUST NOT be changed by sync; a manual
membership and a group link for the same team MUST resolve to the higher role. GitHub logins MUST NOT
add or remove group-sourced memberships. Sync MUST NOT be able to remove a team's last owner.

#### Scenario: Group grants membership at login

- **GIVEN** team T linked to group `family` as `member`
- **WHEN** a user whose claim carries `family` logs in
- **THEN** they are a member of T with source `oidc_group`

#### Scenario: Group removed at the IdP

- **WHEN** that user's next login carries no `family`
- **THEN** their group-sourced membership of T is removed and its leases are released

#### Scenario: Owner links a group they are not in

- **WHEN** an owner whose session lacks group `admins` links T to `admins`
- **THEN** the link is refused with `group_not_held`

### Requirement: Migration to Explicit Ownership

The migration MUST assign every existing directly owned resource to the human who owns it today
(`agents.owner_human_id` for endpoints and agents) and MUST NOT create any team. Every existing todo
MUST keep its `endpoint_id` and have a null `team_id`. The migration MUST be additive and reversible
until the first team exists. After the migration, every read performed by a human with no team
memberships MUST return exactly the rows it returned before, except rows removed by REQ
"Owner-Scoped History Reads" and REQ "Closing the Audited Surfaces".

#### Scenario: Existing endpoint after upgrade

- **GIVEN** endpoint E owned by human H before the upgrade
- **THEN** after the upgrade E has `owner_human_id = H`, `owner_team_id = NULL`, and H's board is
  unchanged

### Requirement: Team Surfaces on the Board

The board MUST offer a scope switcher listing the human's personal scope and each team they belong
to. In a team scope it MUST show that team's endpoints, queues, todos, webhooks and deliveries, and a
team settings area with members and roles, pending invites, group link, the team's owned credentials
and sinks by name, and the operator audit rows naming the team (owners only). Controls a role does
not permit MUST NOT be rendered, and the server MUST enforce the same permission regardless.

#### Scenario: Member's view of settings

- **WHEN** a member opens T's settings
- **THEN** they see members and roles, and no invite, credential or delete controls

### Requirement: Human API for Teams

Team management MUST be available to humans through `/api/v1` under human OAuth (the routes the CLI
uses today) and MUST NOT be exposed as MCP verbs: agents MUST NOT create teams, invite, change roles,
or accept invites. Agents MAY learn which team queues they can reach from the existing identity verb
(`whoami`).

#### Scenario: Agent attempts to invite

- **WHEN** an agent calls any team-management route with its endpoint credential
- **THEN** the request is refused with `401`

### Requirement: Database Operation Standards

Membership changes, lease release, team deletion and every operator action with its audit row MUST
each run in a single transaction. All queries MUST be parameterized. The reach predicate MUST be
expressed as a parameterized join, never as string-built SQL.

#### Scenario: Audit row and action are atomic

- **WHEN** writing the `operator_audit` row fails during a suspension
- **THEN** the suspension is rolled back

## Endpoint Inventory

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/teams` | Required (session) | List the human's teams |
| POST | `/api/v1/teams` | Required (session or human OAuth) | Create a team |
| GET | `/api/v1/teams/{slug}` | Required, member | Team detail |
| PATCH | `/api/v1/teams/{slug}` | Required, owner | Rename, change slug |
| DELETE | `/api/v1/teams/{slug}` | Required, owner, passkey session | Delete the team |
| GET | `/api/v1/teams/{slug}/members` | Required, member | List members and roles |
| PATCH | `/api/v1/teams/{slug}/members/{human}` | Required, admin (owner for owner role), passkey session to grant admin/owner | Change a role |
| DELETE | `/api/v1/teams/{slug}/members/{human}` | Required, admin, or self to leave | Remove or leave |
| POST | `/api/v1/teams/{slug}/invites` | Required, admin | Create an invite |
| GET | `/api/v1/teams/{slug}/invites` | Required, admin | List pending invites |
| DELETE | `/api/v1/teams/{slug}/invites/{id}` | Required, admin | Revoke an invite |
| GET | `/invites/{token}` | Required (session) | Invite page for the addressee |
| POST | `/invites/{token}/accept` | Required (session), verified email match | Accept |
| PUT | `/api/v1/teams/{slug}/group-link` | Required, owner | Link or unlink an OIDC group |
| GET | `/operator` | Required, operator, passkey session | Operator console |
| GET | `/api/v1/operator/directory` | Required, operator, passkey session | Users and teams with counts |
| POST | `/api/v1/operator/suspensions` | Required, operator, passkey session | Suspend or unsuspend |
| PUT | `/api/v1/operator/ceilings/{scope}` | Required, operator, passkey session | Set a per-owner ceiling |

No route in this spec is public.

## Security Requirements

This is a web-facing spec. Topics not restated follow SPEC-0008 and SPEC-0021.

- **Authentication**: every route requires an authenticated human session or human OAuth token;
  agent endpoint credentials MUST NOT authorize any route in the inventory. Operator routes
  additionally require operator status and a passkey-issuer session.
- **Authorization**: every handler MUST resolve reach through the store's reach predicate; role
  checks MUST happen server-side on every request, independent of what the board rendered.
- **Enumeration**: team slugs, invite tokens, member lists and resource ids MUST answer `not_found`
  uniformly outside reach (REQ "Reach and Effective Reach").
- **Rate limiting**: invite creation MUST be limited per team (default 20 per hour) and invite
  acceptance attempts per session (default 10 per minute), using the existing limiter.
- **Security headers**: unchanged shell middleware (CSP and friends); team pages add no inline
  script.
- **Request body size limits**: team and invite bodies MUST be capped at 16 KiB by the existing
  middleware.
- **CSRF protection**: all state-changing board routes MUST use the existing CSRF middleware; the
  invite-accept route MUST be a POST.
- **Redirect validation**: after accepting an invite or re-authenticating for a consent action, the
  redirect target MUST be a same-origin path from an allow-list, never a query parameter.
- **Secrets**: invite tokens MUST be stored hashed; team credentials follow the `internal/cred`
  envelope and are write-only (REQ "Team Roles").

## Accessibility Requirements

This spec adds board UI. The following are mandatory per WCAG 2.1 AA.

- **WCAG 2.1 AA compliance**: team pages, the scope switcher, invite pages and the operator console
  MUST meet Level AA.
- **ARIA landmarks**: the new pages MUST keep the shell's `banner`, `navigation`, `main` and
  `contentinfo` landmarks.
- **Icon-only controls**: role chips, remove-member and revoke-invite buttons that show only an icon
  MUST carry an `aria-label` naming the member or invite they act on.
- **Dynamic content regions**: membership and invite lists updated by htmx MUST sit in
  `aria-live="polite"` regions; a failed consent action's step-up prompt MUST use
  `aria-live="assertive"`.
- **Keyboard navigation**: the scope switcher MUST be operable with arrow keys and Enter, and Escape
  MUST close it.
- **Focus management**: the delete-team and remove-member confirmation dialogs MUST trap focus, move
  focus to the first field on open, and return it to the triggering control on close.
