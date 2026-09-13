---
status: proposed
date: 2026-09-13
decision-makers: joestump
extends: [ADR-0011, ADR-0008]
related: [ADR-0010, ADR-0001]
governs: [SPEC-0021]
---

# ADR-0026: GitHub as a Second Human Login Provider (and the ADR-0011 Step-Up Trigger)

## Context and Problem Statement

Switchboard's human principal (ADR-0008) authenticates via Pocket ID OIDC —
a passkey-only IdP — under ADR-0011's "trust the issuer" policy, which is
explicitly conditioned: **before switchboard accepts identity from any
non-passkey IdP, consent actions must require a phishing-resistant
`amr`/`acr` claim.** "Log in with GitHub" is exactly the event that condition
was written for: GitHub login is an OAuth 2.0 password login (phishable, no
ID token, no OIDC discovery for user login), and it is wanted for real
reasons — collaborators and outside humans who have a GitHub account but no
Pocket ID identity, and the operator from devices not enrolled with Pocket ID.

So the question has two inseparable halves: (1) how does switchboard accept
GitHub identity, and (2) does adding it honor the deferred-hardening
requirement ADR-0011 recorded, or rewrite it?

## Decision Drivers

* **ADR-0011's trigger is contractual, not advisory.** It says the step-up
  requirement "MUST DO before federating to any non-passkey IdP." Adding
  GitHub without addressing it would silently void a recorded security
  decision — the exact failure mode the SDD process exists to prevent.
* **Consent actions mint capabilities.** Friend approvals (ADR-0010) are the
  highest-consequence actions in switchboard; they are the reason the
  assurance bar exists.
* **GitHub cannot do OIDC for login.** No ID token, no discovery document;
  identity is OAuth 2.0 code exchange plus `GET /user` and `GET /user/emails`.
* **Same session model.** Like cairn (its ADR-0019), one ambient web session
  serves the operator board; a second login system bolted on at the edge is
  explicitly not how this house runs services.
* **Single-operator deployment reality.** Switchboard is single-tenant. The
  set of humans who can log in is tiny and known; the threat is a phished
  login, not account enumeration.

## Considered Options

* **Option A — GitHub login for ordinary board use, consent actions gated to
  Pocket ID passkey sessions (step-up by issuer restriction).** *(chosen)*
* **Option B — GitHub login everywhere, no gating:** honor the letter of
  "simple OIDC" by treating GitHub as just another trusted issuer.
* **Option C — Full `amr`/`acr` machinery now:** implement generic assurance
  claims, require GitHub to attest them.
* **Option D — No GitHub login**; enroll outside humans in Pocket ID.

## Decision Outcome

Chosen option: **(A) GitHub login for the operator board, with
high-consequence consent actions (friend approvals, capability minting)
requireting a Pocket ID passkey session.**

This *satisfies* ADR-0011's deferred-hardening requirement rather than
waiving it: the requirement's intent is "a consent action must not be
authorizable by a phishable login." Instead of building generic `amr`/`acr`
claim machinery for an IdP that cannot express it, switchboard enforces the
stronger, simpler invariant available at session-issuer granularity: a
session established by GitHub **cannot perform consent actions, period**. The
consent path demands the session's issuer be the passkey-only IdP
(`RequirePasskeySession` middleware, issuer-allow-list, not a claim check).
That is a deliberately coarser instrument than `amr`/`acr`, chosen because
GitHub offers no assurance signal to check — an interface demanding claims
GitHub cannot supply would either block GitHub logins entirely or force us to
lie about them.

### Consequences

* Good, because ADR-0011's recorded gap is closed the day GitHub ships, not
  left as a silent hole: the non-passkey IdP enters the trust set *only* for
  low-consequence use.
* Good, because outside collaborators get real access to board features
  (todo triage, provider views, webhook inspection) with an identity they
  already have.
* Good, because the enforcement is one middleware and one allow-list — far
  less machinery than generic assurance claims, and testable without a
  real IdP.
* Bad, because a GitHub-authenticated human must re-authenticate through
  Pocket ID to approve a friend — a real UX speed bump at the exact moment
  it matters most.
* Bad, because issuer-granularity gating is coarser than claim-level
  assurance: if Pocket ID ever offers non-passkey login methods, issuer
  trust silently weakens. That regression would be caught by the same
  conditionals that catch "Pocket ID is no longer passkey-only" — a check
  switchboard cannot automate today and must record as operational (see
  Confirmation).
* Bad, because two providers means two sets of client credentials and a
  provider-selection control on the login page.

### Confirmation

Compliance is confirmed when: (1) tests demonstrate a GitHub-established
session receives HTTP 403 on every consent/friending endpoint while passing
on ordinary board endpoints, and a Pocket ID session passes both; (2) the
login page renders the GitHub control only when configured; (3) an
operational check (runbook item, not code — no OIDC API exposes Pocket ID's
method mix) documents that Pocket ID remains passkey-only, revisited
quarterly.

## Pros and Cons of the Options

### Option A — Issuer-gated step-up *(chosen)*

* Good, because it honors the recorded requirement's intent with the minimum
  machinery that GitHub's protocol limitations permit.
* Good, because "consent requires the passkey IdP" is a trivially testable
  invariant; claims plumbing is not.
* Neutral, because it makes the passkey IdP structurally special — a second
  non-passkey IdP later forces the generic-claims conversation ADR-0011
  anticipated.
* Bad, because it couples the consent UX to Pocket ID availability; a Pocket
  ID outage blocks approvals even for a fully GitHub-authenticated operator.

### Option B — Trust GitHub as any other issuer

* Good, because it is the least code and the smoothest UX.
* Bad, because it voids ADR-0011's explicitly recorded condition: friend
  approvals would be authorizable by a phished GitHub password. The deferral
  was written as "MUST DO", not "nice to have"; ignoring it converts a
  documented security boundary into a lie on the books.

### Option C — Generic `amr`/`acr` machinery now

* Good, because it is the future-proof answer if a third IdP with real
  assurance signals arrives.
* Bad, because GitHub supplies no assurance claims to enforce against — the
  machinery would exist with nothing to feed it, and the only workable
  outcome collapses back to issuer allow-listing (Option A) for GitHub
  specifically. Build it when a second *assuring* IdP exists.

### Option D — No GitHub login

* Good, because zero new code and zero new attack surface; ADR-0011's world
  is unchanged.
* Bad, because it leaves the actual people (outside collaborators, un-enrolled
  devices) doing identity-setup toil in Pocket ID as the price of using a
  todo queue, and the operator keeps hitting the same friction this is meant
  to remove.

## Architecture Diagram

```mermaid
sequenceDiagram
    participant B as Browser
    participant SB as Switchboard web
    participant GH as GitHub OAuth
    participant PI as Pocket ID (passkey-only OIDC)

    B->>SB: GET /auth/login?provider=github
    SB->>GH: redirect + state cookie
    GH-->>B: consent → /auth/callback?code
    B->>SB: callback → token exchange → GET /user, /user/emails
    SB->>B: session (iss=github.com, sub, verified email)
    Note over SB: Board routes: allowed
    B->>SB: POST friending consent action
    SB-->>B: 403 — issuer not passkey-only; re-auth via Pocket ID required

    B->>SB: GET /auth/login (Pocket ID, unchanged)
    SB->>PI: OIDC auth code + PKCE (ADR-0011 flow)
    PI-->>SB: id_token verified
    B->>SB: POST friending consent action
    SB->>B: 200 — approved, capability minted
```

## More Information

* Governing spec: SPEC-0021 (`docs/openspec/specs/github-login/`).
* ADR-0011 — the issuer-trust policy and its deferred-hardening requirement;
  this ADR closes that gap by construction rather than by claims machinery.
* ADR-0008 — the human principal; a GitHub-authenticated human is the same
  principal type, with recorded provider provenance.
* ADR-0010 — friending; the consent endpoints that receive the new gate.
* Cairn's parallel decision: `cairn` ADR-0019 / SPEC-0013 (same provider
  interface shape, no consent gate — cairn has no capability-minting action).
