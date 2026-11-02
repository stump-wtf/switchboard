---
status: proposed
date: 2026-11-01
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0019, ADR-0023]
related: [ADR-0038]
---

# ADR-0057: The Device Grant Is the Operator Grant, Entered From Anywhere

**Status note:** this ADR is **proposed**. It ships with an implementation so the choices can be
argued against working code rather than prose; nothing here is settled until Joe accepts it. It
answers [issue #308](https://gitea.stump.rocks/stump.wtf/switchboard/issues/308), whose thread
carried the RFC pointer and the acceptance criteria but no decisions; the four open choices the
issue left to the implementer — the user code's shape and entropy, the expiry and polling interval,
where entry rate limiting is counted, and where single use is enforced — are decided here.

## Context and Problem Statement

The operator API takes a human's OAuth grant: dynamic client registration, then authorization code
plus PKCE over a **loopback redirect** (ADR-0019, ADR-0023). That works when the browser and the CLI
share a machine. On a headless host — the common place to run `harness stack up` or a daemon — the
callback arrives at the *server's* loopback, so the operator must open an SSH port forward before
the browser can finish a login. `--no-browser` prints the URL; it does not move the redirect.

RFC 8628 solves exactly this, and its shape is a poor fit for what Switchboard already has: the
spec assumes a client that can neither receive a redirect nor be trusted with a secret, presenting a
short code to a human on another device. The parts that need a decision are the ones the RFC leaves
to the authorization server: how long a request lives, how fast it may be polled, what the short
code looks like, how the server stops somebody grinding it, and — the one that matters most here —
what principal a completed device flow may act as.

## Decision

### 1. The device flow is an entrance, not a kind of credential

A device request is a pending consent with no principal on it. Approval stamps the APPROVING human,
and that is the only way a principal ever appears on the row. The table deliberately has **no
endpoint column**: the flow structurally cannot mint an endpoint credential, only the same
operator-bound access/refresh pair the code flow issues (`oauth_tokens` with `human_id` set and
`endpoint_id` null, ADR-0023's binding, unchanged). The choice is enforced by the schema rather than
by a check in a handler, because "the CLI must not be able to become an agent" is the kind of
property that has to survive the next refactor.

Per ADR-0038 the grant acts for the approving human; nothing in this flow lets a device name a team
it was never given, because the row can only ever hold a human who was authenticated at the consent
screen.

### 2. Two codes, both hashed; the short one is unambiguous and short

`device_code` is 32 random bytes (base64url) — the same entropy class as every other credential
here — and both codes are persisted only as SHA-256 hashes, like `oauth_codes` and `oauth_tokens`
(ADR-0019). The plaintexts exist in transit and die there.

`user_code` is 8 characters from a 20-letter set with the ambiguous glyphs removed (no `0/O/1/I/L`),
displayed `XXXX-XXXX`: ~2.6e10 values, typeable from a printout, ~34.6 bits. `NormalizeUserCode`
accepts whatever the human typed (case, spaces, dashes) and hashes the canonical form, so the wire
form is never a second source of truth.

### 3. Ten minutes to live, five seconds apart

Expiry is RFC 8628's recommended ten minutes: long enough to walk to a phone, short enough that an
abandoned request is inert before the next coffee. The advertised interval is five seconds, and a
poll arriving sooner than the previous one plus five seconds minus a one-second skew margin answers
`slow_down`. The margin is what keeps honest 5s pollers on either side of a clock discrepancy from
being punished for it.

The interval is enforced while the request is still **waiting on a human** — the phase where every
answer is "not yet" and the poll is pure load. Once the row carries a decision or has expired there
is nothing left to wait for, so the answer goes out immediately; the single-use CAS below means a
client that hammers after approval still gets exactly one grant. The polling lookup returns the
PREVIOUS arrival (the read and the stamp are two statements on purpose: `UPDATE ... RETURNING` hands
back the new value, and a handler comparing `now()` against `now()+interval` would answer
`slow_down` to every poller after the first).

### 4. Entry is rate-limited per actor, because per-request cannot work

Ten user-code lookups per principal per minute, past which the lookup is refused whatever the code
hashes to. Counting guesses against the *pending request* — the obvious reading of "rate-limit user
code entry" — does not work: a wrong code hashes to no row, so the misses, which are the entire
traffic worth counting, would never be counted. The entry surface is session-gated, so every lookup
has an authenticated principal to charge, and one row per principal is enough state. At ten lookups
a minute the keyspace takes centuries; an honest operator who fat-fingers the code into a lockout
waits a minute. A denied, expired, unknown and wrong code all answer identically until the window
closes, and then everybody in the window gets the same message, so the entry page leaks nothing
about which requests exist.

### 5. Single use is enforced where the tokens are minted

Approval leaves the row unspent; the token endpoint spends it with the same compare-and-set shape
the code flow's redemption uses, so exactly one concurrent poll mints and the losers read
`invalid_grant`. Spending at consent would be wrong twice over: a grant nobody polled for is not
"spent", and the poll is the only moment that knows a client is actually there to receive it.

### 6. The consent screen never redirects

The device flow's premise is that the client cannot receive anything. So `GET /oauth/device` takes
the code (or shows the entry form), the POST records approve or deny and re-renders the same page in
its finished state, and the CLI learns the outcome from its own polling. Nothing about the flow can
deliver a URL to anybody.

## Decision Drivers

* Headless install is the actual use (Harness SPEC-0018 REQ-7), and the fix must not require the
  operator to understand port forwarding.
* The device flow must not become a second, weaker path to a credential — one grant shape, one
  binding, one revocation story (ADR-0023, ADR-0038).
* Everything the RFC leaves open should be boring, checkable numbers with a stated reason.
* The short code is the only new attack surface; its defense must be tested, not assumed.

## Considered Options

* **Document the SSH tunnel better.** Cheapest, and it is what Harness does today. Rejected: the
  issue exists because that step is the least friendly one in a headless install, and it asks the
  operator to expose a loopback port to make a login work.
* **A long-poll "pairing" endpoint outside OAuth** (CLI polls a pairing id; the web UI approves it).
  Rejected: it is RFC 8628 with the standard names sanded off. It would advertise nothing in the
  RFC 8414 metadata, so any other OAuth client would keep needing the tunnel, and it would put a
  second bespoke grant into the same token endpoint that today has one.
* **RFC 8628 as implemented here**, including `device_authorization_endpoint` in the metadata and the
  grant type in `grant_types_supported` so discovery advertises it. Chosen: standard names, one
  grant shape, and every other client gains the same path.

## Consequences

* The authorization server grows one public endpoint, one consent screen, and one grant branch; the
  token endpoint now answers a fourth grant type.
* Two tables arrive with migration 0037: the pending requests, and the per-principal entry counter.
  Neither holds a plaintext code.
* `slow_down` is observable only while a request is pending, which is a deliberate reading of the
  RFC rather than a literal one; the tests name it.
* An anonymous device request is not possible here: the entry surface needs a session, so a device
  flow always ends with an authenticated human's name on it. That is the point of the flow, and it
  is also why the entry page is a session page rather than a public one.

## Verification

* `internal/store/oauth_device_test.go` — the previous-stamp contract the interval depends on, the
  spend CAS (a second spend is refused), denial and expiry being final, unique hashes, and the entry
  limiter closing on misses, spare for another actor, and reopening on its own.
* `internal/oauthsrv/device_test.go` — `authorization_pending`, `slow_down`, `expired_token`,
  `access_denied`, `invalid_grant` on a spent grant, the resource and client bindings, and metadata
  advertisement.
* `internal/server/oauth_device_test.go` — end to end through the real router and Postgres: an
  anonymous hit bounces to login, the consent page shows the client and the operator bullets,
  approval yields a pair usable on `/api/v1` that resolves to the approving human and to no endpoint,
  denial yields `access_denied`, and the entry page refuses after the window.
* `cmd/switchboard/login_device_test.go` — the CLI side: discovery, the printed code, pending,
  `slow_down` backing off by a second, denial, expiry, and the error when a server advertises no
  device endpoint.
