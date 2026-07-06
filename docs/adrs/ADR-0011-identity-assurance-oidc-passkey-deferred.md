---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-0008, ADR-0010]
---

# ADR-0011: Identity & Assurance — Simple OIDC Now, Passkey Step-Up Deferred

## Context and Problem Statement

The friending model ([ADR-0010](ADR-0010-a2a-discovery-human-vended-friending.md)) turns on a human making a **consent decision** — approving (and vending) another agent's access. Consent decisions are high-consequence: an approval mints a capability. In a mature federated world you would want assurance that the approving human authenticated in a **phishing-resistant** way (a passkey), and you would want the *requesting* human's assurance level attested too. OIDC can express this via the **`amr`** (authentication methods references, [RFC 8176](https://www.rfc-editor.org/rfc/rfc8176)) and **`acr`** (authentication context class reference) claims.

The question is how much of that assurance machinery to build **now**, for a single-tenant, single-IdP switchboard, versus defer — and to make sure the deferral is a *recorded, deliberate gap* rather than a silent one.

## Decision Drivers

* **Right-size for today.** Switchboard's humans currently authenticate against **one** IdP — **Pocket ID** — which is **passkey-only**. Within that tenant, every human login is already passkey-backed *by construction*.
* **Don't build assurance plumbing with no consumer.** Enforcing `amr`/`acr` step-up when there is exactly one IdP and it is already passkey-only adds code and failure modes for zero marginal security today.
* **Trust the issuer, for now.** With a single trusted passkey-only issuer, "the token came from Pocket ID" is a sufficient assurance signal for consent actions in the current deployment.
* **The deferral must not be silent.** The moment switchboard federates to a *non-passkey* IdP, issuer-trust stops being sufficient — a consent action could be authorized by a phished password login at the other IdP. That future requirement must be written down now as a known gap with a hard trigger, not discovered later.
* **Provenance still required now.** Independent of step-up, friend requests must already carry OIDC-signed provenance of the requesting human ([ADR-0010](ADR-0010-a2a-discovery-human-vended-friending.md)); this ADR is about *assurance level*, not *whether* identity is attested.

## Considered Options

* **(A) Build full `amr`/`acr` step-up enforcement now** — require a phishing-resistant claim on consent actions from day one.
* **(B) Simple OIDC now (trust the passkey-only issuer), with an explicit, triggered deferred-hardening requirement for step-up before federating to any non-passkey IdP.** *(chosen)*
* **(C) Simple OIDC now, and say nothing about the future** — trust the issuer and leave assurance unaddressed.

## Decision Outcome

Chosen option: **"(B) simple OIDC now, passkey step-up explicitly deferred."**

### Now

* The human principal is a **Pocket ID OIDC identity** ([ADR-0008](ADR-0008-human-principal-vended-endpoints.md)). Switchboard **trusts the issuer**.
* Friend requests carry the **signed identity** of the requesting human; the target human approves ([ADR-0010](ADR-0010-a2a-discovery-human-vended-friending.md)).
* **No `amr`/`acr` enforcement.** Switchboard does not require or check a specific authentication-method/assurance claim on consent actions.
* **Rationale:** Pocket ID is **passkey-only**, so within this tenant authentication is passkey-backed by construction. Issuer-trust is therefore an adequate proxy for "this human authenticated phishing-resistantly" *today*.

### Deferred-hardening requirement (a recorded gap, not silent)

> **DEFERRED HARDENING — MUST DO before federating to any non-passkey IdP:**
> Require a **phishing-resistant `amr`/`acr` claim** on high-consequence consent actions (friend approvals, and any future capability-minting action) **before** switchboard federates to, or accepts provenance from, **any IdP that is not passkey-only.**
>
> **Why:** issuer-trust is sufficient *only* while every trusted issuer is passkey-only. The moment a non-passkey IdP is in the trust set, a consent action (or a friend request's provenance) could be backed by a phishable password login. At that point switchboard must **step up** — demand and verify a phishing-resistant assurance signal on the consent path — rather than infer it from issuer identity.

This requirement is tracked as a first-class open item in [docs/README.md](../README.md), not buried.

### The `amr` value problem (flagged for the future decision)

When step-up *is* implemented, note that **there is no standard `amr` value that means "passkey."** [RFC 8176](https://www.rfc-editor.org/rfc/rfc8176) defines `hwk` (hardware key), `swk` (software key), `pop` (proof-of-possession), `mfa`, etc., but no `passkey`. A passkey is best expressed as a **combination** (e.g. `hwk` + `pop`), a **`phr`** ("phishing-resistant") marker where the issuer supports it, or asserted via an **`acr`** level agreed with the issuer. **The exact value/level is TBD at that time** and depends on what the then-federated IdPs actually emit. This ADR does not pin it — it flags that the choice is non-trivial and must be made against real issuer behavior.

### Consequences

* Good, because switchboard ships the friending/consent model without assurance plumbing that would have no consumer today.
* Good, because the security posture is honest: within a passkey-only tenant, issuer-trust genuinely is passkey-backed.
* Good, because the future weakening (federating to a non-passkey IdP) has a **written, triggered** requirement — the gap cannot be forgotten or crossed silently.
* Neutral, because provenance signing is required now regardless; only the *assurance-level enforcement* is deferred.
* Bad, because a future maintainer must remember to honor the deferred requirement at the federation boundary — mitigated by recording it in the open-questions index and the friend-requests spec, and by the trigger being a concrete, detectable event (adding a non-passkey issuer).
* Bad, because the eventual `amr`/`acr` choice is genuinely ambiguous (no `passkey` value) — surfaced now so it is not a surprise then.

### Confirmation

* Consent actions today succeed with a valid Pocket ID token and **do not** check `amr`/`acr` (a test asserts the absence of step-up enforcement is intentional, referencing this ADR).
* The deferred-hardening requirement appears in [docs/README.md](../README.md) open-questions and in the [friend-requests spec](../openspec/specs/friending/spec.md) provenance section.
* A code-level guard/comment at the IdP-trust-set configuration point references this ADR, so adding a non-passkey issuer forces a reviewer to confront the step-up requirement.

## Pros and Cons of the Options

### (A) Full step-up now (rejected)

* Good, because maximally strict from day one.
* Bad, because it builds and maintains assurance-enforcement plumbing with **no consumer** — the only issuer is already passkey-only, so the check is always trivially satisfied.
* Bad, because premature: the exact `amr`/`acr` semantics can't be pinned without a real non-passkey issuer to test against.

### (B) Simple now + explicit deferred requirement (chosen)

* Good, because right-sized for a single passkey-only IdP while making the future requirement explicit and triggered.
* Bad, because it relies on honoring a written deferral — mitigated by recording it prominently and tying it to a concrete trigger.

### (C) Simple now, silent about the future (rejected)

* Good, because least effort today.
* Bad, because it leaves a security-relevant assumption (issuer-trust ⇒ phishing-resistance) undocumented, to be violated silently the first time a non-passkey IdP is added. This is the exact silent-gap failure this ADR exists to prevent.

## More Information

* Human principal & OIDC posture: [ADR-0008](ADR-0008-human-principal-vended-endpoints.md).
* Where provenance is consumed (friend-request approvals): [ADR-0010](ADR-0010-a2a-discovery-human-vended-friending.md) and the [friend-requests spec](../openspec/specs/friending/spec.md).
* Pocket ID: <https://pocket-id.org/> (passkey-only OIDC provider). `amr` values: [RFC 8176](https://www.rfc-editor.org/rfc/rfc8176). `acr`/`amr` in OIDC core: <https://openid.net/specs/openid-connect-core-1_0.html>.
